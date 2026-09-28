package cli

import (
	"archive/zip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/brutasse/rig/internal/digest"
	"github.com/brutasse/rig/internal/jvm"
	"github.com/brutasse/rig/internal/kernel"
	"github.com/brutasse/rig/internal/lockfile"
)

// fixtureSetup pins the kernel jar and its JARSHA, copies the named
// testdata fixture workspace into a fresh dir, and chdirs into it. It
// skips when java or the kernel jar is unavailable.
func fixtureSetup(t *testing.T, fixture string) {
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
	if err := copyTree(fixture, dst); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dst)
}

func hotSetup(t *testing.T) {
	fixtureSetup(t, "testdata/hot")
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

// TestBuildPathsOnly locks and builds a module whose :paths point at a
// non-default source dir (src2) with no :rig/artifact-dirs declaration:
// the lock must derive artifact-dirs from :paths, and the jar must carry
// the compiled src2 code while the default src/ decoy stays out.
func TestBuildPathsOnly(t *testing.T) {
	fixtureSetup(t, "testdata/paths")
	cacheDir := t.TempDir()
	code, out := runCLI(t, "lock", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("lock exit = %d, want 0; out: %s", code, out)
	}
	lock, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	mod := lock.Modules["modules/app"]
	if got := mod.Build.ArtifactDirs; !reflect.DeepEqual(got, []string{"src2"}) {
		t.Fatalf("lock artifact-dirs = %v, want [src2]", got)
	}
	code, out = runCLI(t, "build", "--cache-dir", cacheDir, "-p", "modules/app")
	if code != 0 {
		t.Fatalf("build exit = %d, want 0; out: %s", code, out)
	}
	jars, _ := filepath.Glob(filepath.Join("modules", "app", "target", "*.jar"))
	if len(jars) != 1 {
		t.Fatalf("jars in modules/app/target = %v, want exactly one", jars)
	}
	zr, err := zip.OpenReader(jars[0])
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["app/paths__init.class"] {
		t.Errorf("jar %s missing the compiled src2 class app/paths__init.class", jars[0])
	}
	if names["app/decoy__init.class"] {
		t.Errorf("jar %s contains the src/ decoy class; :paths must scope the build", jars[0])
	}
}

func TestBuildNativeFlags(t *testing.T) {
	hotSetup(t)
	// --native and --uber are mutually exclusive.
	code, out := runCLI(t, "build", "--cache-dir", t.TempDir(), "--uber", "--native")
	if code != 2 || !strings.Contains(out, "mutually exclusive") {
		t.Errorf("uber+native exit = %d, out = %q", code, out)
	}
	// A module that declares no native-image build is a usage error.
	code, out = runCLI(t, "build", "--cache-dir", t.TempDir(), "-p", "modules/app", "--native")
	if code != 2 {
		t.Errorf("native build exit = %d, want 2; out: %s", code, out)
	}
	if !strings.Contains(out, "declares no native-image build") {
		t.Errorf("out = %q", out)
	}
}

// TestHotBuildNative builds the testdata/native fixture's module as a
// GraalVM native-image binary and runs it. Skips unless RIG_TEST_GRAALVM
// points at a GraalVM home with bin/native-image (the JVM pins in the lock
// are overridden by RIG_JAVA/RIG_GRAALVM_HOME, so no managed download).
func TestHotBuildNative(t *testing.T) {
	graalHome := os.Getenv("RIG_TEST_GRAALVM")
	if graalHome == "" {
		t.Skip("RIG_TEST_GRAALVM not set (point it at a GraalVM home with bin/native-image)")
	}
	java, err := jvm.Find()
	if err != nil {
		t.Skipf("no java available: %v", err)
	}
	jar := kernelJarPath(t)
	sha, err := digest.File(jar)
	if err != nil {
		t.Fatal(err)
	}
	oldSHA := kernel.Current.JARSHA
	kernel.Current.JARSHA = sha
	os.Setenv("RIG_KERNEL_JAR", jar)
	os.Setenv("RIG_JAVA", java)
	os.Setenv("RIG_GRAALVM_HOME", graalHome)
	t.Cleanup(func() {
		kernel.Current.JARSHA = oldSHA
		os.Unsetenv("RIG_KERNEL_JAR")
		os.Unsetenv("RIG_JAVA")
		os.Unsetenv("RIG_GRAALVM_HOME")
	})
	dst := t.TempDir()
	if err := copyTree("testdata/native", dst); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dst)
	cacheDir := t.TempDir()
	code, out := runCLI(t, "build", "--cache-dir", cacheDir, "-p", "modules/app", "--native")
	if code != 0 {
		t.Fatalf("build --native exit = %d, want 0; out: %s", code, out)
	}
	bin := filepath.Join(dst, "modules", "app", "target", "native-app")
	if !statOK(bin) {
		t.Fatalf("native binary missing at %s; out: %s", bin, out)
	}
	// The binary is a standalone native executable: it takes plain args.
	code, out = runCLI(t, "exec", "--cache-dir", cacheDir, "-p", "modules/app", bin, "world")
	if code != 0 {
		t.Fatalf("run native binary exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "native hello world") {
		t.Errorf("native binary out = %q", out)
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
