package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brutasse/rig/internal/cache"
	"github.com/brutasse/rig/internal/jvm"
)

func TestEnsureFromEnvJar(t *testing.T) {
	dir := t.TempDir()
	jar := filepath.Join(dir, "kernel.jar")
	if err := os.WriteFile(jar, []byte("fake-kernel-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	pin := Pin{GitSHA: "abc"}
	t.Setenv("RIG_KERNEL_JAR", jar)

	store := cache.NewAt(t.TempDir())
	got, err := pin.Ensure(context.Background(), store, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != jar {
		t.Errorf("ensure = %s, want %s", got, jar)
	}
}

func TestEnsureEnvJarMissing(t *testing.T) {
	pin := Pin{GitSHA: "abc"}
	t.Setenv("RIG_KERNEL_JAR", filepath.Join(t.TempDir(), "nope.jar"))

	store := cache.NewAt(t.TempDir())
	if _, err := pin.Ensure(context.Background(), store, false); err == nil {
		t.Error("expected error for missing RIG_KERNEL_JAR")
	}
}

func TestRunnerFromEnvJar(t *testing.T) {
	dir := t.TempDir()
	runner := filepath.Join(dir, "runner.jar")
	if err := os.WriteFile(runner, []byte("fake-runner-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	pin := Pin{GitSHA: "abc"}
	t.Setenv("RIG_RUNNER_JAR", runner)

	store := cache.NewAt(t.TempDir())
	got, err := pin.Runner(context.Background(), store, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != runner {
		t.Errorf("runner = %s, want %s", got, runner)
	}
}

func TestRunnerEnvJarMissing(t *testing.T) {
	pin := Pin{GitSHA: "abc"}
	t.Setenv("RIG_RUNNER_JAR", filepath.Join(t.TempDir(), "nope.jar"))

	store := cache.NewAt(t.TempDir())
	if _, err := pin.Runner(context.Background(), store, false); err == nil {
		t.Error("expected error for missing RIG_RUNNER_JAR")
	}
}

func TestRunnerKernelOverrideRequiresRunnerOverride(t *testing.T) {
	dir := t.TempDir()
	jar := filepath.Join(dir, "kernel.jar")
	if err := os.WriteFile(jar, []byte("fake-kernel-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	pin := Pin{GitSHA: "abc"}
	t.Setenv("RIG_KERNEL_JAR", jar)

	store := cache.NewAt(t.TempDir())
	_, err := pin.Runner(context.Background(), store, false)
	if err == nil {
		t.Fatal("expected error: RIG_KERNEL_JAR without RIG_RUNNER_JAR")
	}
	if !strings.Contains(err.Error(), "RIG_RUNNER_JAR") {
		t.Errorf("err = %v, want the RIG_RUNNER_JAR hint", err)
	}
}

func TestRunnerUnpinned(t *testing.T) {
	// A pin without a runner sha (v0.1.0 predates the runner artifact).
	pin := Pin{Version: "v0.1.0", GitSHA: "abc"}

	store := cache.NewAt(t.TempDir())
	if _, err := pin.Runner(context.Background(), store, false); err == nil {
		t.Error("expected error for a pin without a runner")
	}
}

func TestRunnerFetchAndCache(t *testing.T) {
	const body = "runner-jar-bytes"
	sum := sha256.Sum256([]byte(body))
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Write([]byte(body))
	}))
	defer srv.Close()
	pin := Pin{GitSHA: "abc", RunnerURL: srv.URL + "/rig-runner.jar", RunnerSHA: hex.EncodeToString(sum[:])}
	ctx := context.Background()
	store := cache.NewAt(t.TempDir())

	if _, err := pin.Runner(ctx, store, true); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("offline err = %v, want the offline error", err)
	}
	got, err := pin.Runner(ctx, store, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != store.RunnerJar("abc") {
		t.Errorf("runner = %s, want %s", got, store.RunnerJar("abc"))
	}
	if b, err := os.ReadFile(got); err != nil || string(b) != body {
		t.Errorf("cached runner = %q (err %v), want %q", b, err, body)
	}
	if n != 1 {
		t.Fatalf("requests = %d, want 1", n)
	}
	if _, err := pin.Runner(ctx, store, false); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("requests after cache hit = %d, want 1", n)
	}
}

func TestRunnerCachedCorruption(t *testing.T) {
	sum := sha256.Sum256([]byte("expected-bytes"))
	pin := Pin{GitSHA: "abc", RunnerURL: "http://example.invalid/rig-runner.jar", RunnerSHA: hex.EncodeToString(sum[:])}
	store := cache.NewAt(t.TempDir())
	path := store.RunnerJar("abc")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupted"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := pin.Runner(context.Background(), store, false); err == nil ||
		!strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("err = %v, want the corruption error", err)
	}
}

func TestRunnerFetchSHAMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("other-bytes"))
	}))
	defer srv.Close()
	sum := sha256.Sum256([]byte("expected-bytes"))
	pin := Pin{GitSHA: "abc", RunnerURL: srv.URL + "/rig-runner.jar", RunnerSHA: hex.EncodeToString(sum[:])}
	store := cache.NewAt(t.TempDir())
	if _, err := pin.Runner(context.Background(), store, false); err == nil ||
		!strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("err = %v, want the sha mismatch error", err)
	}
	// A rejected download must not land in the store.
	if _, err := os.Stat(store.RunnerJar("abc")); !os.IsNotExist(err) {
		t.Errorf("store path stat = %v, want absent", err)
	}
}

const stubSrc = `
public class Stub {
	public static void main(String[] args) {
		String code = System.getenv("RIG_TEST_EXIT");
		if (code != null) {
			System.exit(Integer.parseInt(code));
		}
		System.out.println("{\"ok\":true}");
	}
}
`

func makeStubJar(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Stub.java"), []byte(stubSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runDir(t, dir, "javac", "Stub.java"); err != nil {
		t.Skipf("javac unavailable: %v", err)
	}
	jar := filepath.Join(dir, "stub.jar")
	if err := runDir(t, dir, "jar", "c", "e", jar, "Stub", "Stub.class"); err != nil {
		t.Skipf("jar unavailable: %v", err)
	}
	return jar
}

func runDir(t *testing.T, dir, name string, args ...string) error {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("%v: %s", err, out)
	}
	return err
}

func TestCallSuccess(t *testing.T) {
	java, err := jvm.Find()
	if err != nil {
		t.Skip(err)
	}
	jar := makeStubJar(t)
	out, err := Call(context.Background(), jar, java, Request{Op: "resolve", Workspace: ".", Args: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "{\"ok\":true}" {
		t.Errorf("stdout = %q", out)
	}
}

func TestCallExitMapping(t *testing.T) {
	java, err := jvm.Find()
	if err != nil {
		t.Skip(err)
	}
	jar := makeStubJar(t)
	cases := []struct {
		code int
		want error
	}{
		{1, ErrResolution},
		{2, ErrBadRequest},
		{3, ErrLegacyKeys},
	}
	for _, c := range cases {
		t.Run(fmt.Sprint(c.code), func(t *testing.T) {
			t.Setenv("RIG_TEST_EXIT", fmt.Sprint(c.code))
			_, err := Call(context.Background(), jar, java, Request{Op: "resolve", Workspace: ".", Args: map[string]any{}})
			var oe *OpError
			if !errors.As(err, &oe) {
				t.Fatalf("err = %v, want *OpError", err)
			}
			if oe.Op != "resolve" {
				t.Errorf("op = %q", oe.Op)
			}
			if !errors.Is(oe.Err, c.want) {
				t.Errorf("mapped = %v, want %v", oe.Err, c.want)
			}
		})
	}
}
