package cli

import (
	"archive/zip"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brutasse/rig/internal/cache"
	"github.com/brutasse/rig/internal/jdk"
	"github.com/brutasse/rig/internal/jvm"
	"github.com/brutasse/rig/internal/lockfile"
	"github.com/brutasse/rig/internal/workspace"
)

// resolverVersion is the version the local jars are named with: the
// contents of resolver/VERSION (rig's version-file, what make kernel
// builds), falling back to the dev default.
func resolverVersion() string {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "resolver", "VERSION"))
	if err != nil {
		return "v0.1.0"
	}
	if line := strings.SplitN(strings.TrimSpace(string(b)), "\n", 2)[0]; line != "" {
		return line
	}
	return "v0.1.0"
}

// runnerJarPath returns the locally built runner jar: RIG_TEST_RUNNER_JAR,
// else the jar make kernel builds in the runner module's target dir. It
// skips when unavailable.
func runnerJarPath(t *testing.T) string {
	t.Helper()
	var p string
	if v := os.Getenv("RIG_TEST_RUNNER_JAR"); v != "" && statOK(v) {
		p = v
	}
	if p == "" {
		if cand := filepath.Join("..", "..", "..", "resolver", "runner", "target", "rig-runner-"+resolverVersion()+".jar"); statOK(cand) {
			p = cand
		}
	}
	if p == "" {
		t.Skip("runner jar not found; build it with: make kernel (or set RIG_TEST_RUNNER_JAR)")
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// fixtureSetup overrides the kernel and runner with the locally built
// jars (the two local overrides always travel together), copies the named
// testdata fixture workspace into a fresh dir, and chdirs into it. The
// copied workspace is pointed at the seeded fixture maven repo (the real
// Central and Clojars stay out of the suite). It skips when java, the
// jars, or the seed are unavailable.
func fixtureSetup(t *testing.T, fixture string) {
	t.Helper()
	jar := kernelJarPath(t)
	if _, err := jvm.Find(); err != nil {
		t.Skipf("no java available: %v", err)
	}
	os.Setenv("RIG_KERNEL_JAR", jar)
	os.Setenv("RIG_RUNNER_JAR", runnerJarPath(t))
	t.Cleanup(func() {
		os.Unsetenv("RIG_KERNEL_JAR")
		os.Unsetenv("RIG_RUNNER_JAR")
	})
	seedTestRepo(t)
	src := fixture
	if fixture == "testdata/hot" {
		p, err := hotPrimal(t)
		if err != nil {
			t.Skipf("hot primal: %v", err)
		}
		src = p
	}
	dst := t.TempDir()
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if fixture != "testdata/hot" {
		// The hot fixture's manifests are injected (and locked) once in
		// buildHotPrimal; every other fixture injects into its own copy.
		injectFixtureRepos(t, dst)
	}
	t.Chdir(dst)
}

// injectFixtureRepos points every manifest in a copied fixture (the root
// and each module's) at the seeded fixture repo.
func injectFixtureRepos(t *testing.T, dir string) {
	t.Helper()
	manifests := []string{filepath.Join(dir, "deps.edn")}
	mods, err := filepath.Glob(filepath.Join(dir, "modules", "*", "deps.edn"))
	if err != nil {
		t.Fatal(err)
	}
	manifests = append(manifests, mods...)
	for _, p := range manifests {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(injectRepos(string(b), testRepoURL)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

var (
	primalOnce sync.Once
	primalDir  string
	primalErr  error
)

// hotPrimal returns the shared pre-locked hot fixture, built once per
// suite: the fixture copied with the seeded repo injected and locked
// with the locally built kernel. Every hot test copies this instead of
// re-locking: the injected manifests are byte-identical in every copy,
// so the lock is current in all of them.
func hotPrimal(t *testing.T) (string, error) {
	t.Helper()
	primalOnce.Do(func() {
		primalDir, primalErr = buildHotPrimal()
	})
	if primalErr != nil {
		return "", primalErr
	}
	return primalDir, nil
}

func buildHotPrimal() (string, error) {
	dir, err := os.MkdirTemp("", "rig-hot-primal-")
	if err != nil {
		return "", err
	}
	registerSeedDir(dir)
	if err := copyTree(filepath.Join("testdata", "hot"), dir); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	for _, rel := range []string{"deps.edn", filepath.Join("modules", "app", "deps.edn")} {
		p := filepath.Join(dir, rel)
		b, err := os.ReadFile(p)
		if err != nil {
			os.RemoveAll(dir)
			return "", err
		}
		if err := os.WriteFile(p, []byte(injectRepos(string(b), testRepoURL)), 0o644); err != nil {
			os.RemoveAll(dir)
			return "", err
		}
	}
	cache, err := os.MkdirTemp("", "rig-hot-primal-cache-")
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	registerSeedDir(cache)
	cmd := exec.Command(rigTestBin, "lock", "--cache-dir", cache)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("%v; out: %s", err, out)
	}
	return dir, nil
}

// injectRepos returns the manifest content with a :mvn/repos entry as
// its first entry, both standard repo ids pointed at the seeded fixture
// repo — for a copied fixture, resolution, metadata and lock never
// reach the real Central or Clojars.
func injectRepos(content, repoURL string) string {
	i := strings.Index(content, "{")
	if i < 0 {
		panic("injectRepos: no map in manifest: " + content)
	}
	entry := `:mvn/repos {"central" {:url "` + repoURL + `"} "clojars" {:url "` + repoURL + `"}} `
	return content[:i+1] + entry + content[i+1:]
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

func TestHotTestTimeout(t *testing.T) {
	hotSetup(t)
	// An exec-fn that never returns must be killed by the watchdog and
	// fail the run, not hang forever (a kaocha-cljs suite
	// wedged on a dead node client hung rig test indefinitely).
	writeFile(t, "modules/app/test/app/sleepy_test.clj",
		"(ns app.sleepy-test\n  (:require [clojure.test :refer [deftest]]))\n(deftest forever (Thread/sleep 3600000))\n")
	start := time.Now()
	code, out := runCLI(t, "test", "--cache-dir", t.TempDir(), "--timeout", "3s")
	elapsed := time.Since(start)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
	if !strings.Contains(out, "did not finish in 3s, killed") {
		t.Errorf("out = %q, want the watchdog failure", out)
	}
	if elapsed > time.Minute {
		t.Errorf("watchdog fired after %v, want promptly after 3s", elapsed)
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
// points at a GraalVM home with bin/native-image. The build JVM is the
// one pickJava resolves for the fixture's :rig/jvm pin (managed or
// matching system, installed on demand): never the machine's ambient
// java, whose major can outgrow the native-image JVM's and make the
// entry shim's bytecode unreadable.
func TestHotBuildNative(t *testing.T) {
	graalHome := os.Getenv("RIG_TEST_GRAALVM")
	if graalHome == "" {
		t.Skip("RIG_TEST_GRAALVM not set (point it at a GraalVM home with bin/native-image)")
	}
	if _, err := jvm.Find(); err != nil {
		t.Skipf("no java available: %v", err)
	}
	jar := kernelJarPath(t)
	os.Setenv("RIG_KERNEL_JAR", jar)
	os.Setenv("RIG_GRAALVM_HOME", graalHome)
	t.Cleanup(func() {
		os.Unsetenv("RIG_KERNEL_JAR")
		os.Unsetenv("RIG_GRAALVM_HOME")
	})
	dst := t.TempDir()
	if err := copyTree("testdata/native", dst); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dst)
	cacheDir := t.TempDir()

	// The entry shim is compiled by the build JVM and loaded by the
	// native-image JVM: resolve the build JVM now, from the same store
	// the build uses, and fail fast when either side is not the major
	// the fixture pins — a mismatch is a cryptic native-image error deep
	// into the build, not a test error.
	root, err := workspace.Find(".")
	if err != nil {
		t.Fatal(err)
	}
	if root.Lock.JVM == nil || root.Lock.GraalVM == nil {
		t.Fatal("fixture lock pins no jvm/graalvm; the guard needs both")
	}
	store := cache.NewAt(cacheDir)
	buildJava, _, err := (&opts{}).pickJava(context.Background(), store, root)
	if err != nil {
		t.Fatalf("build JVM for the fixture's :rig/jvm pin %q: %v", root.Lock.JVM.Requested, err)
	}
	assertMajor(t, "build JVM", buildJava, root.Lock.JVM.Requested)
	graalJava := filepath.Join(graalHome, "bin", "java")
	if runtime.GOOS == "windows" {
		graalJava += ".exe"
	}
	assertMajor(t, "native-image JVM", graalJava, root.Lock.GraalVM.Requested)

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

// assertMajor fails the test when the java binary at path does not report
// the major version the fixture pins.
func assertMajor(t *testing.T, what, path, wanted string) {
	t.Helper()
	ver, err := jvm.Version(path)
	if err != nil {
		t.Fatalf("%s %s: %v", what, path, err)
	}
	if maj := jdk.FeatureVersion(ver); maj != jdk.FeatureVersion(wanted) {
		t.Fatalf("%s %s is major %d, the fixture pins %q — its bytecode would not match the other side of the native build",
			what, path, maj, wanted)
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

// TestHotReadOnlyStore models the post-install story: the first run
// materializes the embedded kernel and runner jars into the store, and
// after that warm-up, check and test must do their work from the read-only
// store — the embedded jars are adopted from the cache, nothing is fetched
// or extracted again.
func TestHotReadOnlyStore(t *testing.T) {
	if _, err := jvm.Find(); err != nil {
		t.Skipf("no java available: %v", err)
	}
	for _, j := range []string{"rig-resolver.jar", "rig-runner.jar"} {
		st, err := os.Stat(filepath.Join("..", "kernel", "embed", j))
		if err != nil || st.Size() == 0 {
			t.Skip("embedded jars are placeholders; build them with: make kernel")
		}
	}

	dst := t.TempDir()
	if err := copyTree("testdata/hot", dst); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dst)

	store := t.TempDir()
	// Warm-up run (writable store): materializes the embedded jars and
	// fetches the locked artifacts into the store; the fixture's failing
	// integration test gives exit 1.
	if code, out := runCLI(t, "test", "--cache-dir", store); code != 1 {
		t.Fatalf("warm-up test exit = %d, want 1; out: %s", code, out)
	}
	// Read-only from here on; restore before the temp dir is removed.
	if err := chmodTree(store, 0o555, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = chmodTree(store, 0o755, 0o644) })

	code, out := runCLI(t, "check", "--cache-dir", store)
	if code != 0 {
		t.Fatalf("check exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "check: ok") {
		t.Errorf("check out = %q", out)
	}
	code, out = runCLI(t, "test", "--cache-dir", store)
	if code != 1 {
		t.Fatalf("test exit = %d, want 1; out: %s", code, out)
	}
	if !strings.Contains(out, "tests failed in: modules/app") {
		t.Errorf("test out missing the failing module: %q", out)
	}
}

func TestHotStaleFails(t *testing.T) {
	// The lock gate: a manifest changed after the lock was written, and the
	// hot commands never write the lock. The run fails (exit 3) with a hint
	// to run 'rig lock', and the lock on disk is untouched.
	hotSetup(t)
	lock, err := os.ReadFile("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	mutateFile(t, "deps.edn", func(s string) string { return s + "\n; touched\n" })
	code, out := runCLI(t, "test", "--cache-dir", t.TempDir())
	if code != 3 {
		t.Fatalf("stale test exit = %d, want 3; out: %s", code, out)
	}
	if !strings.Contains(out, "stale") || !strings.Contains(out, "run 'rig lock'") {
		t.Fatalf("stale test out missing the hint: %s", out)
	}
	if after, _ := os.ReadFile("deps.lock"); string(after) != string(lock) {
		t.Errorf("stale test rewrote deps.lock")
	}
}

// chmodTree sets dir and everything under it to dirMode/fileMode.
func chmodTree(dir string, dirMode, fileMode os.FileMode) error {
	return filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		m := fileMode
		if d.IsDir() {
			m = dirMode
		}
		return os.Chmod(p, m)
	})
}
