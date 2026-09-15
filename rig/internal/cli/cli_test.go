package cli

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brutasse/rig/internal/cache"
	"github.com/brutasse/rig/internal/fetch"
	"github.com/brutasse/rig/internal/lockfile"
)

func runCLI(t *testing.T, args ...string) (int, string) {
	t.Helper()
	oldArgs := os.Args
	os.Args = append([]string{"rig"}, args...)
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdout, oldStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	code := Execute()
	outW.Close()
	errW.Close()
	os.Stdout, os.Stderr = oldStdout, oldStderr
	os.Args = oldArgs
	var b strings.Builder
	_, _ = io.Copy(&b, outR)
	_, _ = io.Copy(&b, errR)
	return code, b.String()
}

func shaOf(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func artifactServer(t *testing.T, body string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestVerifyNoLock(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", "{:rig/lib x/y}\n")
	code, out := runCLI(t, "verify")
	if code != 3 {
		t.Errorf("exit = %d, want 3; out: %s", code, out)
	}
	if !strings.Contains(out, "no lock") {
		t.Errorf("out = %q", out)
	}
}

func TestVerifyFrozenStale(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	// Lock whose recorded manifest sha does NOT match the deps.edn on disk:
	// the lock is stale. --frozen must refuse to relock and exit 3.
	doc := lockfile.ForTest(".")
	writeFile(t, "deps.edn", "{:rig/lib x/y}\n")
	if err := doc.Save("deps.lock"); err != nil {
		t.Fatal(err)
	}
	code, out := runCLI(t, "verify", "--frozen", "--cache-dir", t.TempDir())
	if code != 3 {
		t.Errorf("exit = %d, want 3; out: %s", code, out)
	}
	if !strings.Contains(out, "stale") {
		t.Errorf("out = %q", out)
	}
}

func TestVerifyOK(t *testing.T) {
	cacheDir := t.TempDir()
	body := "test-artifact-bytes"
	srv := artifactServer(t, body)

	dir := t.TempDir()
	t.Chdir(dir)
	doc := lockfile.ForTest(".")
	doc.Artifacts[0].SHA256 = shaOf(body)
	doc.Artifacts[0].URL = srv.URL + "/a.jar"
	writeFile(t, "deps.edn", "{:rig/lib x/y}\n")
	doc.FreshFor(map[string]string{".": "{:rig/lib x/y}\n"})
	if err := doc.Save("deps.lock"); err != nil {
		t.Fatal(err)
	}

	code, out := runCLI(t, "verify", "--cache-dir", cacheDir)
	if code != 0 {
		t.Errorf("exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "verified 1 artifacts") {
		t.Errorf("out = %q", out)
	}
	cached := filepath.Join(cacheDir, "artifacts", shaOf(body))
	if _, err := os.Stat(cached); err != nil {
		t.Errorf("artifact not cached: %v", err)
	}

	code, out = runCLI(t, "verify", "--cache-dir", cacheDir)
	if code != 0 {
		t.Errorf("second verify exit = %d; out: %s", code, out)
	}
	if !strings.Contains(out, "1 cached, 0 fetched") {
		t.Errorf("out = %q", out)
	}
}

func TestVerifyHashMismatch(t *testing.T) {
	srv := artifactServer(t, "server-served-other-bytes")
	dir := t.TempDir()
	t.Chdir(dir)
	doc := lockfile.ForTest(".")
	doc.Artifacts[0].SHA256 = shaOf("what-the-lock-expects")
	doc.Artifacts[0].URL = srv.URL + "/a.jar"
	writeFile(t, "deps.edn", "{:rig/lib x/y}\n")
	doc.FreshFor(map[string]string{".": "{:rig/lib x/y}\n"})
	if err := doc.Save("deps.lock"); err != nil {
		t.Fatal(err)
	}
	code, out := runCLI(t, "verify", "--cache-dir", t.TempDir())
	if code != 4 {
		t.Errorf("exit = %d, want 4; out: %s", code, out)
	}
	if !strings.Contains(out, "hash mismatch") {
		t.Errorf("out = %q", out)
	}
}

func TestVerifyOfflineMissing(t *testing.T) {
	srv := artifactServer(t, "x")
	dir := t.TempDir()
	t.Chdir(dir)
	doc := lockfile.ForTest(".")
	doc.Artifacts[0].SHA256 = shaOf("x")
	doc.Artifacts[0].URL = srv.URL + "/a.jar"
	writeFile(t, "deps.edn", "{}\n")
	doc.FreshFor(map[string]string{".": "{}\n"})
	if err := doc.Save("deps.lock"); err != nil {
		t.Fatal(err)
	}
	code, out := runCLI(t, "verify", "--offline", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Errorf("exit = %d, want 1; out: %s", code, out)
	}
	if !strings.Contains(out, "offline") {
		t.Errorf("out = %q", out)
	}
}

func TestLockFromResponse(t *testing.T) {
	body := "lock-artifact-bytes"
	srv := artifactServer(t, body)
	store := cache.NewAt(t.TempDir())
	doc := lockfile.ForTest(".")
	doc.Artifacts[0].URL = srv.URL + "/a.jar"
	resp, err := json.Marshal(map[string]any{"lock": doc})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := lockFromResponse(context.Background(), fetch.New(false), store, t.TempDir(), resp)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Artifacts[0].SHA256 != shaOf(body) {
		t.Errorf("sha = %s, want %s", lock.Artifacts[0].SHA256, shaOf(body))
	}
	if lock.Tool.Name != "rig" || lock.Tool.Version != Version {
		t.Errorf("tool = %+v", lock.Tool)
	}
	if lock.LockedAt.IsZero() {
		t.Error("locked_at not set")
	}
	if _, err := store.Get(shaOf(body)); err != nil {
		t.Errorf("artifact not cached: %v", err)
	}
}

func TestLockFromResponseUsesM2(t *testing.T) {
	const body = "m2-bytes"
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Write([]byte("should-not-be-used"))
	}))
	t.Cleanup(srv.Close)

	m2 := t.TempDir()
	dir := filepath.Join(m2, "org", "clojure", "clojure", "1.11.0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "clojure-1.11.0.jar"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum([]byte(body))
	if err := os.WriteFile(filepath.Join(dir, "clojure-1.11.0.jar.sha1"), []byte(hex.EncodeToString(sum[:])+" clojure-1.11.0.jar\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := cache.NewAt(t.TempDir())
	doc := lockfile.ForTest(".")
	doc.Artifacts[0].URL = srv.URL + "/a.jar"
	resp, err := json.Marshal(map[string]any{"lock": doc})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := lockFromResponse(context.Background(), fetch.New(true), store, m2, resp)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Artifacts[0].SHA256 != shaOf(body) {
		t.Errorf("sha = %s, want %s", lock.Artifacts[0].SHA256, shaOf(body))
	}
	if n != 0 {
		t.Errorf("requests = %d, want 0 (m2 hit must not touch the network)", n)
	}
	if _, err := store.Get(shaOf(body)); err != nil {
		t.Errorf("artifact not cached: %v", err)
	}
}

func TestLockFromResponseOffline(t *testing.T) {
	srv := artifactServer(t, "x")
	doc := lockfile.ForTest(".")
	doc.Artifacts[0].URL = srv.URL + "/a.jar"
	resp, err := json.Marshal(map[string]any{"lock": doc})
	if err != nil {
		t.Fatal(err)
	}
	_, err = lockFromResponse(context.Background(), fetch.New(true), cache.NewAt(t.TempDir()), t.TempDir(), resp)
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Errorf("err = %v, want exit 1 (offline)", err)
	}
}

func TestLockFromResponseRefused(t *testing.T) {
	resp, _ := json.Marshal(map[string]any{
		"lock":    lockfile.ForTest("."),
		"refused": []map[string]any{{"coord": "a/b", "reason": "cooldown"}},
	})
	_, err := lockFromResponse(context.Background(), fetch.New(false), cache.NewAt(t.TempDir()), t.TempDir(), resp)
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 5 {
		t.Errorf("err = %v, want exit 5 (refused)", err)
	}
	if !strings.Contains(err.Error(), "cooldown") {
		t.Errorf("err = %q", err)
	}
}

func TestClean(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", "{:rig/lib x/y}\n")
	writeFile(t, "target/classes/a.class", "x")
	code, out := runCLI(t, "clean")
	if code != 0 {
		t.Errorf("exit = %d; out: %s", code, out)
	}
	if _, err := os.Stat("target"); !os.IsNotExist(err) {
		t.Error("target dir still present")
	}
}

func TestCleanWithLockTargetDir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	doc := lockfile.ForTest(".")
	m := doc.Modules["."]
	m.Build.ClassDir = "build/classes"
	doc.Modules["."] = m
	writeFile(t, "deps.edn", "{}\n")
	writeFile(t, "build/classes/a.class", "x")
	if err := doc.Save("deps.lock"); err != nil {
		t.Fatal(err)
	}
	code, _ := runCLI(t, "clean")
	if code != 0 {
		t.Errorf("exit = %d", code)
	}
	if _, err := os.Stat("build"); !os.IsNotExist(err) {
		t.Error("build dir still present")
	}
}

func TestVersion(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", "{}\n")
	writeFile(t, "VERSION", "1.2.3\n")
	code, out := runCLI(t, "version")
	if code != 0 || strings.TrimSpace(out) != "1.2.3" {
		t.Errorf("exit=%d out=%q", code, out)
	}
}

func TestVersionFromLockFallback(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	doc := lockfile.ForTest(".")
	writeFile(t, "deps.edn", "{}\n")
	if err := doc.Save("deps.lock"); err != nil {
		t.Fatal(err)
	}
	code, out := runCLI(t, "version")
	if code != 0 || strings.TrimSpace(out) != "0.0.1" {
		t.Errorf("exit=%d out=%q", code, out)
	}
}

func TestInfo(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	doc := lockfile.ForTest(".", "modules/app")
	writeFile(t, "deps.edn", "{:rig/modules [\"modules/app\"]}\n")
	if err := doc.Save("deps.lock"); err != nil {
		t.Fatal(err)
	}
	code, out := runCLI(t, "info", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Errorf("exit = %d; out: %s", code, out)
	}
	for _, want := range []string{"type:\tworkspace", "modules:\t., modules/app", "lock:\t2026-01-01T00:00:00Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("info missing %q in:\n%s", want, out)
		}
	}
}

func TestUnknownFlag(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	code, _ := runCLI(t, "version", "--bogus")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

func TestNotAProject(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	code, out := runCLI(t, "info")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(out, "not a rig project") {
		t.Errorf("out = %q", out)
	}
}
