package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brutasse/rig/internal/cache"
	"github.com/brutasse/rig/internal/jvm"
)

// embedRunner swaps the embedded runner bytes for the duration of a test:
// materialize reads the package's embedded vars, so tests can exercise it
// without a real jar.
func embedRunner(t *testing.T, b []byte) {
	t.Helper()
	old := runnerJar
	runnerJar = b
	t.Cleanup(func() { runnerJar = old })
}

func TestKernelFromEnvJar(t *testing.T) {
	dir := t.TempDir()
	jar := filepath.Join(dir, "kernel.jar")
	if err := os.WriteFile(jar, []byte("fake-kernel-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIG_KERNEL_JAR", jar)

	store := cache.NewAt(t.TempDir())
	got, err := Kernel(store)
	if err != nil {
		t.Fatal(err)
	}
	if got != jar {
		t.Errorf("ensure = %s, want %s", got, jar)
	}
}

func TestKernelEnvJarMissing(t *testing.T) {
	t.Setenv("RIG_KERNEL_JAR", filepath.Join(t.TempDir(), "nope.jar"))

	store := cache.NewAt(t.TempDir())
	if _, err := Kernel(store); err == nil {
		t.Error("expected error for missing RIG_KERNEL_JAR")
	}
}

func TestRunnerFromEnvJar(t *testing.T) {
	dir := t.TempDir()
	runner := filepath.Join(dir, "runner.jar")
	if err := os.WriteFile(runner, []byte("fake-runner-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIG_RUNNER_JAR", runner)

	store := cache.NewAt(t.TempDir())
	got, err := Runner(store)
	if err != nil {
		t.Fatal(err)
	}
	if got != runner {
		t.Errorf("runner = %s, want %s", got, runner)
	}
}

func TestRunnerEnvJarMissing(t *testing.T) {
	t.Setenv("RIG_RUNNER_JAR", filepath.Join(t.TempDir(), "nope.jar"))

	store := cache.NewAt(t.TempDir())
	if _, err := Runner(store); err == nil {
		t.Error("expected error for missing RIG_RUNNER_JAR")
	}
}

func TestRunnerKernelOverrideRequiresRunnerOverride(t *testing.T) {
	dir := t.TempDir()
	jar := filepath.Join(dir, "kernel.jar")
	if err := os.WriteFile(jar, []byte("fake-kernel-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIG_KERNEL_JAR", jar)

	store := cache.NewAt(t.TempDir())
	_, err := Runner(store)
	if err == nil {
		t.Fatal("expected error: RIG_KERNEL_JAR without RIG_RUNNER_JAR")
	}
	if !strings.Contains(err.Error(), "RIG_RUNNER_JAR") {
		t.Errorf("err = %v, want the RIG_RUNNER_JAR hint", err)
	}
}

func TestMaterializePlaceholder(t *testing.T) {
	embedRunner(t, nil)
	store := cache.NewAt(t.TempDir())
	_, err := Runner(store)
	if err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Errorf("err = %v, want the placeholder hint", err)
	}
}

func TestMaterializeWritesAndAdopts(t *testing.T) {
	const body = "embedded-runner-bytes"
	embedRunner(t, []byte(body))
	sum := sha256.Sum256([]byte(body))
	store := cache.NewAt(t.TempDir())
	want := store.RunnerJar(hex.EncodeToString(sum[:]))

	got, err := Runner(store)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("runner = %s, want the content-sha path %s", got, want)
	}
	if b, err := os.ReadFile(got); err != nil || string(b) != body {
		t.Errorf("materialized = %q (err %v), want %q", b, err, body)
	}
	// Second call adopts the cached copy (same path, still byte-identical).
	got2, err := Runner(store)
	if err != nil || got2 != got {
		t.Errorf("second Runner = (%s, %v), want (%s, nil)", got2, err, got)
	}
	// A corrupted copy is rewritten, not adopted.
	if err := os.WriteFile(got, []byte("corrupted"), 0o644); err != nil {
		t.Fatal(err)
	}
	got3, err := Runner(store)
	if err != nil || got3 != got {
		t.Fatalf("Runner after corruption = (%s, %v)", got3, err)
	}
	if b, err := os.ReadFile(got); err != nil || string(b) != body {
		t.Errorf("after corruption = %q (err %v), want %q", b, err, body)
	}
}

func TestEmbeddedNreplMaterializes(t *testing.T) {
	// The vendored nREPL jar is committed, so this runs on every tree (it
	// is the only embedded jar that is not a placeholder before 'make
	// kernel').
	store := cache.NewAt(t.TempDir())
	got, err := Nrepl(store)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(got)
	if err != nil || st.Size() == 0 {
		t.Fatalf("nrepl jar = %s (err %v), want a materialized file", got, err)
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

func TestOpErrorError(t *testing.T) {
	// The verb comes from the caller; Error() must not repeat the kernel
	// op name ("build: build: kernel: op failed" used to be the trailer).
	oe := &OpError{Op: "build", Err: ErrOpFailed}
	if got := oe.Error(); got != "kernel: op failed" {
		t.Errorf("Error() = %q, want %q", got, "kernel: op failed")
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
		{1, ErrOpFailed},
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
