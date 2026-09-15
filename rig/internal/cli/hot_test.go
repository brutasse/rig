package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brutasse/rig/internal/digest"
	"github.com/brutasse/rig/internal/jvm"
	"github.com/brutasse/rig/internal/kernel"
)

// hotSetup pins the kernel jar and its JARSHA, copies the testdata/hot
// fixture workspace into a fresh dir, and chdirs into it. It skips when java
// or the kernel jar is unavailable.
func hotSetup(t *testing.T) {
	t.Helper()
	jar := kernelJarPath(t)
	if _, err := jvm.Find(); err != nil {
		t.Skipf("no java available: %v", err)
	}
	sha, err := digest.File(jar)
	if err != nil {
		t.Fatal(err)
	}
	oldSHA := kernel.Current.JARSHA
	kernel.Current.JARSHA = sha
	os.Setenv("RIG_KERNEL_JAR", jar)
	t.Cleanup(func() {
		kernel.Current.JARSHA = oldSHA
		os.Unsetenv("RIG_KERNEL_JAR")
	})
	dst := t.TempDir()
	if err := copyTree("testdata/hot", dst); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dst)
}

func copyTree(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyTree(s, d); err != nil {
				return err
			}
			continue
		}
		b, err := os.ReadFile(s)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(d), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(d, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func TestHotTestFocus(t *testing.T) {
	hotSetup(t)
	cacheDir := t.TempDir()
	// Focus the :unit suite: only the passing unit tests run.
	code, out := runCLI(t, "test", "--cache-dir", cacheDir, ":kaocha.filter/focus", "[:unit]")
	if code != 0 {
		t.Fatalf("focus test exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "2 tests, 2 assertions, 0 failures") {
		t.Errorf("focus test out = %q", out)
	}
	if strings.Contains(out, "this-fails") {
		t.Errorf("focus test ran the integration test; out: %s", out)
	}
}

func TestHotTestAll(t *testing.T) {
	hotSetup(t)
	cacheDir := t.TempDir()
	// No focus: every suite runs, including the failing integration test.
	code, out := runCLI(t, "test", "--cache-dir", cacheDir)
	if code != 1 {
		t.Fatalf("all test exit = %d, want 1; out: %s", code, out)
	}
	if !strings.Contains(out, "this-fails") {
		t.Errorf("all test out missing the failing test: %q", out)
	}
	if !strings.Contains(out, "tests failed in: modules/app") {
		t.Errorf("all test out = %q", out)
	}
}

func TestHotTestBadOpts(t *testing.T) {
	hotSetup(t)
	// An odd number of opts is a usage error.
	code, out := runCLI(t, "test", "--cache-dir", t.TempDir(), ":kaocha.filter/focus")
	if code != 2 {
		t.Errorf("exit = %d, want 2; out: %s", code, out)
	}
}

func TestHotRun(t *testing.T) {
	hotSetup(t)
	cacheDir := t.TempDir()
	code, out := runCLI(t, "run", "--cache-dir", cacheDir, "-p", "modules/app", "world")
	if code != 0 {
		t.Fatalf("run exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "hello world") {
		t.Errorf("run out = %q", out)
	}
}

func TestHotBuildUber(t *testing.T) {
	hotSetup(t)
	cacheDir := t.TempDir()
	code, out := runCLI(t, "build", "--cache-dir", cacheDir, "-p", "modules/app", "--uber")
	if code != 0 {
		t.Fatalf("build exit = %d, want 0; out: %s", code, out)
	}
	uber := filepath.Join("modules", "app", "target", "app-uber.jar")
	if !statOK(uber) {
		t.Fatalf("uber jar missing at %s; out: %s", uber, out)
	}
	// The uber is self-contained: run it straight off the file system.
	code, out = runCLI(t, "exec", "--cache-dir", cacheDir, "-p", "modules/app",
		"java", "-jar", "target/app-uber.jar", "there")
	if code != 0 {
		t.Fatalf("uber run exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "hello there") {
		t.Errorf("uber run out = %q", out)
	}
}

func TestHotTestOffline(t *testing.T) {
	hotSetup(t)
	cacheDir := t.TempDir()
	// Warm the cache, then the same run must succeed fully offline.
	code, out := runCLI(t, "test", "--cache-dir", cacheDir, ":kaocha.filter/focus", "[:unit]")
	if code != 0 {
		t.Fatalf("online warm exit = %d, want 0; out: %s", code, out)
	}
	code, out = runCLI(t, "test", "--offline", "--cache-dir", cacheDir, ":kaocha.filter/focus", "[:unit]")
	if code != 0 {
		t.Fatalf("offline test exit = %d, want 0; out: %s", code, out)
	}
}
