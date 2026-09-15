package cli

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/brutasse/rig/internal/jdk"
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

func TestNew(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	withLTSStub(t, 25, http.StatusOK)
	code, out := runCLI(t, "new", "example/foo")
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

func TestNewOffline(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	code, out := runCLI(t, "new", "example/foo", "--offline")
	if code != 0 {
		t.Fatalf("new exit = %d, want 0; out: %s", code, out)
	}
	root, _ := os.ReadFile("foo/deps.edn")
	if strings.Contains(string(root), ":rig/jvm") {
		t.Errorf("offline scaffold must not pin a JVM: %s", root)
	}
}

func TestNewLTSUnavailable(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	withLTSStub(t, 25, http.StatusInternalServerError)
	code, out := runCLI(t, "new", "example/foo")
	if code != 0 {
		t.Fatalf("new exit = %d, want 0 (scaffold proceeds without a pin); out: %s", code, out)
	}
	root, _ := os.ReadFile("foo/deps.edn")
	if strings.Contains(string(root), ":rig/jvm") {
		t.Errorf("scaffold must not pin a JVM when the lookup fails: %s", root)
	}
	if !strings.Contains(out, "could not determine the current LTS") {
		t.Errorf("out = %q", out)
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
