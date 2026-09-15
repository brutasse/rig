package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// selfUpdateAPI serves the GitHub release API shape plus assets for tag.
func selfUpdateAPI(t *testing.T, tag, binaryContent string) *httptest.Server {
	t.Helper()
	bin := "rig-" + runtime.GOOS + "-" + runtime.GOARCH
	sums := shaOf(binaryContent) + "  " + bin + "\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest", "/releases/tags/" + tag:
			fmt.Fprintf(w, `{"tag_name":%q,"assets":[
				{"name":"SHA256SUMS","browser_download_url":"%s/sums"},
				{"name":%q,"browser_download_url":"%s/bin"}]}`, tag, "http://"+r.Host, bin, "http://"+r.Host)
		case "/sums":
			fmt.Fprint(w, sums)
		case "/bin":
			fmt.Fprint(w, binaryContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSelfUpdateDevBuild(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", "{}\n")
	code, out := runCLI(t, "self-update", "--cache-dir", t.TempDir())
	if code != 2 {
		t.Errorf("exit = %d, want 2; out: %s", code, out)
	}
	if !strings.Contains(out, "dev build") {
		t.Errorf("out = %q", out)
	}
}

func TestSelfUpdateOffline(t *testing.T) {
	oldVersion := Version
	Version = "0.1.0"
	t.Cleanup(func() { Version = oldVersion })
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", "{}\n")
	code, out := runCLI(t, "self-update", "--offline", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Errorf("exit = %d, want 1; out: %s", code, out)
	}
	if !strings.Contains(out, "offline") {
		t.Errorf("out = %q", out)
	}
}

func TestSelfUpdateFull(t *testing.T) {
	oldVersion := Version
	Version = "0.1.0"
	t.Cleanup(func() { Version = oldVersion })

	binDir := t.TempDir()
	oldExe := filepath.Join(binDir, "rig")
	writeFile(t, oldExe, "old-rig-bytes")
	oldExeFn := rigExecutable
	rigExecutable = func() (string, error) { return oldExe, nil }
	t.Cleanup(func() { rigExecutable = oldExeFn })

	srv := selfUpdateAPI(t, "v0.2.0", "new-rig-bytes")
	t.Setenv("RIG_UPDATES_API", srv.URL)

	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", "{}\n")

	code, out := runCLI(t, "self-update", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "updated rig 0.1.0 -> v0.2.0") {
		t.Errorf("out = %q", out)
	}
	if strings.Contains(out, "available") {
		t.Errorf("passive notice must be suppressed after self-update: %q", out)
	}
	b, err := os.ReadFile(oldExe)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "new-rig-bytes" {
		t.Errorf("binary not replaced: %q", b)
	}
	st, _ := os.Stat(oldExe)
	if st.Mode()&0o111 == 0 {
		t.Errorf("binary not executable: %v", st.Mode())
	}
}

func TestSelfUpdateCheck(t *testing.T) {
	oldVersion := Version
	Version = "0.1.0"
	t.Cleanup(func() { Version = oldVersion })

	srv := selfUpdateAPI(t, "v0.2.0", "new-rig-bytes")
	t.Setenv("RIG_UPDATES_API", srv.URL)

	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", "{}\n")

	code, out := runCLI(t, "self-update", "--check", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "v0.2.0 available (running 0.1.0)") {
		t.Errorf("out = %q", out)
	}

	// Same version: up to date.
	srv2 := selfUpdateAPI(t, "v0.1.0", "same-bytes")
	t.Setenv("RIG_UPDATES_API", srv2.URL)
	code, out = runCLI(t, "self-update", "--check", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "up to date") {
		t.Errorf("out = %q", out)
	}
}

func TestSelfUpdateExactVersion(t *testing.T) {
	oldVersion := Version
	Version = "0.1.0"
	t.Cleanup(func() { Version = oldVersion })

	binDir := t.TempDir()
	oldExe := filepath.Join(binDir, "rig")
	writeFile(t, oldExe, "old-rig-bytes")
	oldExeFn := rigExecutable
	rigExecutable = func() (string, error) { return oldExe, nil }
	t.Cleanup(func() { rigExecutable = oldExeFn })

	srv := selfUpdateAPI(t, "v0.3.0", "v030-rig-bytes")
	t.Setenv("RIG_UPDATES_API", srv.URL)

	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", "{}\n")

	code, out := runCLI(t, "self-update", "--version", "v0.3.0", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "updated rig 0.1.0 -> v0.3.0") {
		t.Errorf("out = %q", out)
	}

	// Bad tag is a usage error.
	code, out = runCLI(t, "self-update", "--version", "not-a-version", "--cache-dir", t.TempDir())
	if code != 2 && code != 1 {
		t.Errorf("exit = %d, want 1 or 2; out: %s", code, out)
	}
}

// TestUpdateNotice exercises the passive check: fresh TTL cache (no network),
// expired TTL (network), offline (no network), and dev builds (no network).
func TestUpdateNotice(t *testing.T) {
	selfUpdated = false // set by earlier tests in this process
	srv := selfUpdateAPI(t, "v0.2.0", "x")
	t.Setenv("RIG_UPDATES_API", srv.URL)

	cacheDir := t.TempDir()
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", "{}\n")

	// Dev build: no check, no notice.
	code, out := runCLI(t, "clean", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("clean exit = %d", code)
	}
	if strings.Contains(out, "available") {
		t.Errorf("dev build must not check for updates: %q", out)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "update-check")); !os.IsNotExist(err) {
		t.Error("dev build must not write the TTL file")
	}

	// Release build, stale/missing TTL: real check, notice + TTL file.
	oldVersion := Version
	Version = "0.1.0"
	t.Cleanup(func() { Version = oldVersion })
	_, out = runCLI(t, "clean", "--cache-dir", cacheDir)
	if !strings.Contains(out, "rig v0.2.0 available") {
		t.Errorf("notice missing: %q", out)
	}
	if !strings.Contains(readFile(t, filepath.Join(cacheDir, "update-check")), "v0.2.0") {
		t.Fatalf("TTL file not written: %q", readFile(t, filepath.Join(cacheDir, "update-check")))
	}

	// Fresh TTL cache: served from cache, no network (new server never hit).
	srv.Close()
	_, out = runCLI(t, "clean", "--cache-dir", cacheDir)
	if !strings.Contains(out, "rig v0.2.0 available") {
		t.Errorf("cached notice missing: %q", out)
	}

	// Cached tag equals running version: no notice.
	writeFile(t, filepath.Join(cacheDir, "update-check"),
		fmt.Sprintf("%d v0.1.0\n", time.Now().Unix()))
	_, out = runCLI(t, "clean", "--cache-dir", cacheDir)
	if strings.Contains(out, "available") {
		t.Errorf("unexpected notice: %q", out)
	}

	// Offline: no notice even with a stale TTL.
	writeFile(t, filepath.Join(cacheDir, "update-check"), "1 v0.9.9\n")
	_, out = runCLI(t, "clean", "--offline", "--cache-dir", cacheDir)
	if strings.Contains(out, "available") {
		t.Errorf("offline notice must be skipped: %q", out)
	}
}
