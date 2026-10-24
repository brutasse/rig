package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/brutasse/rig/internal/jdk"
	"github.com/brutasse/rig/internal/jvm"
)

// withLTSStub points the Adoptium API at an info-endpoint stub and restores
// the real base afterwards.
func withLTSStub(t *testing.T, lts, code int) {
	t.Helper()
	srv := ltsAPIServer(t, lts, code)
	old := jdk.DefaultBase
	jdk.DefaultBase = srv.URL
	t.Cleanup(func() { jdk.DefaultBase = old })
}

// scaffoldKernel is setKernel plus a JVM: a scaffolded project has no lock, so
// pin the java the test-runner lookup runs on rather than let rig pick one.
func scaffoldKernel(t *testing.T) {
	t.Helper()
	setKernel(t)
	java, err := jvm.Find()
	if err != nil {
		t.Skipf("no java available: %v", err)
	}
	t.Setenv("RIG_JAVA", java)
}

// mavenStamp is maven-metadata's yyyyMMddHHmmss (UTC) for now minus ago.
func mavenStamp(ago time.Duration) string {
	return time.Now().UTC().Add(-ago).Format("20060102150405")
}

// metadataServer serves maven-metadata.xml the way a maven repository does,
// so the scaffold's test-runner lookup resolves versions offline. Only
// <lastUpdated> carries a publication time; the per-repo metadata file the
// repo id would name is absent, exactly as on repositories that skip it.
func metadataServer(t *testing.T, lastUpdated string, versions ...string) *httptest.Server {
	t.Helper()
	var sb strings.Builder
	for _, v := range versions {
		fmt.Fprintf(&sb, "<version>%s</version>", v)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/maven-metadata.xml") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"+
			"<metadata><versioning><versions>%s</versions>"+
			"<lastUpdated>%s</lastUpdated></versioning></metadata>",
			sb.String(), lastUpdated)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// scaffoldRepoAt points the scaffold's test-runner lookup at a repository URL
// for the rest of the test.
func scaffoldRepoAt(t *testing.T, url string) {
	t.Helper()
	old := scaffoldRepos
	scaffoldRepos = map[string]any{"central": map[string]any{"url": url}}
	t.Cleanup(func() { scaffoldRepos = old })
}

func TestNew(t *testing.T) {
	scaffoldKernel(t)
	dir := t.TempDir()
	t.Chdir(dir)
	withLTSStub(t, 25, http.StatusOK)
	scaffoldRepoAt(t, metadataServer(t, mavenStamp(100*time.Hour), "1.2.3").URL)
	code, out := runCLI(t, "new", "example/foo", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("new exit = %d, want 0; out: %s", code, out)
	}
	for _, p := range []string{
		"foo/deps.edn",
		"foo/modules/foo/deps.edn",
		"foo/modules/foo/src/example/foo/core.clj",
		"foo/modules/foo/test/example/foo/core_test.clj",
	} {
		if !statOK(p) {
			t.Errorf("missing %s", p)
		}
	}
	root, _ := os.ReadFile("foo/deps.edn")
	if !strings.Contains(string(root), `:rig/modules ["modules/foo"]`) {
		t.Errorf("root = %s", root)
	}
	if !strings.Contains(string(root), `:rig/jvm "25"`) {
		t.Errorf("root = %s (want the current-LTS :rig/jvm pin)", root)
	}
	mod, _ := os.ReadFile("foo/modules/foo/deps.edn")
	if !strings.Contains(string(mod), ":rig/lib example/foo") {
		t.Errorf("module lib = %s", mod)
	}
	if !strings.Contains(string(mod), ":rig/main example.foo.core") {
		t.Errorf("module main = %s", mod)
	}
	if statOK("foo/deps.lock") {
		t.Error("scaffold must not write a lock")
	}
}

// TestNewNoJVMPin: the scaffold pins :rig/jvm only when the current LTS is
// known — offline (no lookup at all) and a failing lookup both leave the
// manifest unpinned, the second with a note naming the failed lookup.
func TestNewNoJVMPin(t *testing.T) {
	cases := []struct {
		name    string
		offline bool
		ltsCode int // 0: no LTS stub (offline does no lookup)
		wantOut string
	}{
		{"offline", true, 0, ""},
		{"lts lookup fails", false, http.StatusInternalServerError, "could not determine the current LTS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			args := []string{"new", "example/foo", "--cache-dir", t.TempDir()}
			if tc.offline {
				args = append(args, "--offline")
			} else {
				scaffoldKernel(t)
				scaffoldRepoAt(t, metadataServer(t, mavenStamp(100*time.Hour), "1.2.3").URL)
				withLTSStub(t, 25, tc.ltsCode)
			}
			code, out := runCLI(t, args...)
			if code != 0 {
				t.Fatalf("new exit = %d, want 0 (no pin, not a failure); out: %s", code, out)
			}
			root, _ := os.ReadFile("foo/deps.edn")
			if strings.Contains(string(root), ":rig/jvm") {
				t.Errorf("scaffold must not pin a JVM: %s", root)
			}
			if tc.wantOut != "" && !strings.Contains(out, tc.wantOut) {
				t.Errorf("out missing %q: %q", tc.wantOut, out)
			}
		})
	}
}

func TestNewInvalid(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if code, out := runCLI(t, "new"); code != 2 {
		t.Errorf("no-arg new exit = %d, want 2; out: %s", code, out)
	}
	if code, out := runCLI(t, "new", "bad coord"); code != 2 {
		t.Errorf("bad-coord new exit = %d, want 2; out: %s", code, out)
	}
	writeString(t, "foo/deps.edn", "{}\n")
	if code, out := runCLI(t, "new", "example/foo"); code != 1 {
		t.Errorf("existing-dir new exit = %d, want 1; out: %s", code, out)
	}
}

// TestNewTestRunnerVersionResolved: the scaffolded module pins what the
// repository serves, not a version compiled into rig.
func TestNewTestRunnerVersionResolved(t *testing.T) {
	scaffoldKernel(t)
	t.Chdir(t.TempDir())
	withLTSStub(t, 25, http.StatusOK)
	scaffoldRepoAt(t, metadataServer(t, mavenStamp(100*time.Hour), "1.2.3", "1.2.4").URL)
	code, out := runCLI(t, "new", "example/foo", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("new exit = %d, want 0; out: %s", code, out)
	}
	mod := readFile(t, "foo/modules/foo/deps.edn")
	if !strings.Contains(mod, `lambdaisland/kaocha {:mvn/version "1.2.4"}`) {
		t.Errorf("module manifest = %s, want the newest version the stub serves", mod)
	}
	if strings.Contains(out, "note:") {
		t.Errorf("a resolved version needs no note: %s", out)
	}
}

// TestNewTestRunnerVersionCooldown: a repository whose every release is inside
// the cooldown answers nothing, and the scaffold falls back to the bundled
// version — saying so, since the project starts on a version it did not ask for.
func TestNewTestRunnerVersionCooldown(t *testing.T) {
	scaffoldKernel(t)
	t.Chdir(t.TempDir())
	withLTSStub(t, 25, http.StatusOK)
	scaffoldRepoAt(t, metadataServer(t, mavenStamp(1*time.Hour), "9.9.9").URL)
	code, out := runCLI(t, "new", "example/foo", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("new exit = %d, want 0 (a cooldown is not a failure); out: %s", code, out)
	}
	want := fmt.Sprintf(`lambdaisland/kaocha {:mvn/version %q}`, defaultTestRunnerVersion)
	if mod := readFile(t, "foo/modules/foo/deps.edn"); !strings.Contains(mod, want) {
		t.Errorf("module manifest = %s, want the bundled fallback", mod)
	}
	if !strings.Contains(out, "inside its cooldown") {
		t.Errorf("out missing the cooldown note: %s", out)
	}
}

// TestNewTestRunnerVersionUnreachable: a repository that serves nothing fails
// the lookup, not the scaffold.
func TestNewTestRunnerVersionUnreachable(t *testing.T) {
	scaffoldKernel(t)
	t.Chdir(t.TempDir())
	withLTSStub(t, 25, http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	scaffoldRepoAt(t, srv.URL)
	code, out := runCLI(t, "new", "example/foo", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("new exit = %d, want 0; out: %s", code, out)
	}
	want := fmt.Sprintf(`lambdaisland/kaocha {:mvn/version %q}`, defaultTestRunnerVersion)
	if mod := readFile(t, "foo/modules/foo/deps.edn"); !strings.Contains(mod, want) {
		t.Errorf("module manifest = %s, want the bundled fallback", mod)
	}
	if !strings.Contains(out, "no repository of the workspace serves") {
		t.Errorf("out missing the failed-lookup note: %s", out)
	}
}

// TestNewOfflinePinsBundledRunner: offline does not ask, so it writes the
// bundled version without a note.
func TestNewOfflinePinsBundledRunner(t *testing.T) {
	t.Chdir(t.TempDir())
	code, out := runCLI(t, "new", "example/foo", "--offline", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("new --offline exit = %d, want 0; out: %s", code, out)
	}
	want := fmt.Sprintf(`lambdaisland/kaocha {:mvn/version %q}`, defaultTestRunnerVersion)
	if mod := readFile(t, "foo/modules/foo/deps.edn"); !strings.Contains(mod, want) {
		t.Errorf("module manifest = %s, want the bundled fallback", mod)
	}
	if strings.Contains(out, "kaocha") {
		t.Errorf("offline scaffolding should not comment on the runner: %s", out)
	}
}

func TestNewModule(t *testing.T) {
	kernelSetup(t)
	dir := t.TempDir()
	t.Chdir(dir)
	// A pre-existing single-module workspace.
	writeString(t, "deps.edn", "{:rig/modules [\"modules/app\"]}\n")
	writeString(t, "modules/app/deps.edn",
		"{:rig/lib example/app :paths [\"src\"] :deps {org.clojure/clojure {:mvn/version \"1.12.5\"}}}\n")
	writeString(t, "modules/app/src/app/core.clj", "(ns app.core)\n")

	code, out := runCLI(t, "new-module", "other", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("new-module exit = %d, want 0; out: %s", code, out)
	}
	if !statOK("modules/other/deps.edn") || !statOK("modules/other/src/other/core.clj") {
		t.Fatal("module template not created")
	}
	root, _ := os.ReadFile("deps.edn")
	if !strings.Contains(string(root), "modules/other") {
		t.Errorf("new module not in root: %s", root)
	}
	if !strings.Contains(string(root), "modules/app") {
		t.Errorf("existing module lost: %s", root)
	}
	if !statOK("deps.lock") {
		t.Fatal("lock not written")
	}
	if !strings.Contains(out, "added module modules/other") {
		t.Errorf("out = %q", out)
	}
}
