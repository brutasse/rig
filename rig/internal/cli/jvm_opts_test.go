package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brutasse/rig/internal/jdk"
	"github.com/brutasse/rig/internal/jvm"
)

// loggingJava installs a wrapper for the system java that appends every
// launch's argv to logPath (one line per launch, args space-separated)
// before exec'ing the real java, and points RIG_JAVA at it. It returns
// the wrapper path. It lets a test assert which flags reached which JVM
// without rig-side instrumentation.
func loggingJava(t *testing.T, logPath string) string {
	t.Helper()
	real, err := jvm.Find()
	if err != nil {
		t.Skipf("no java available: %v", err)
	}
	absReal, err := filepath.Abs(real)
	if err != nil {
		t.Fatal(err)
	}
	absLog, err := filepath.Abs(logPath)
	if err != nil {
		t.Fatal(err)
	}
	wrap := filepath.Join(t.TempDir(), "java")
	script := "#!/bin/sh\n" +
		fmt.Sprintf("{ for a in \"$@\"; do printf '%%s ' \"$a\"; done; printf '\\n'; } >> %s\n", absLog) +
		fmt.Sprintf("exec %q \"$@\"\n", absReal)
	if err := os.WriteFile(wrap, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIG_JAVA", wrap)
	return wrap
}

func launchLogLines(t *testing.T, logPath string) []string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read the JVM launch log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func logLinesWith(lines []string, marker string) []string {
	var out []string
	for _, l := range lines {
		if strings.Contains(l, marker) {
			out = append(out, l)
		}
	}
	return out
}

// TestHotCompileJVMOpts checks :rig/compile-jvm-opts threading: the build's
// kernel JVM and the check ns-load JVM get the flags; the check stage-1
// kernel (metadata only) and the dev-execution run JVM do not. The build's
// AOT fork is not observed here — it runs on the kernel JVM's own java.home
// (see aot-fork-runs-on-the-kernels-jvm in the resolver suite, which asserts
// the flags on its command line).
func TestHotCompileJVMOpts(t *testing.T) {
	hotSetup(t)
	if err := os.WriteFile("deps.edn",
		[]byte("{:rig/modules [\"modules/app\"]\n :rig/compile-jvm-opts [\"-Drig.compile.opt=1\"]}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const flag = "-Drig.compile.opt=1"
	logPath := filepath.Join(t.TempDir(), "java.log")
	loggingJava(t, logPath)
	cacheDir := t.TempDir()

	if code, out := runCLI(t, "build", "-p", "modules/app", "--cache-dir", cacheDir); code != 0 {
		t.Fatalf("build exit = %d; out: %s", code, out)
	}
	if code, out := runCLI(t, "check", "--cache-dir", cacheDir); code != 0 {
		t.Fatalf("check exit = %d; out: %s", code, out)
	}
	if code, out := runCLI(t, "run", "-p", "modules/app", "--cache-dir", cacheDir); code != 0 {
		t.Fatalf("run exit = %d; out: %s", code, out)
	}

	lines := launchLogLines(t, logPath)
	// Kernel JVMs all launch as `java [flags] -jar <kernel> --request -`; the
	// flag rides only on the build's.
	kernels := logLinesWith(lines, "--request")
	with, without := 0, 0
	for _, l := range kernels {
		if strings.Contains(l, flag) {
			with++
		} else {
			without++
		}
	}
	if with != 1 {
		t.Errorf("kernel JVMs carrying the compile flag = %d, want 1 (the build's):\n%s", with, strings.Join(kernels, "\n"))
	}
	if without < 1 {
		t.Errorf("kernel JVMs without the compile flag = %d, want >= 1 (the re-lock and check stage 1):\n%s", without, strings.Join(kernels, "\n"))
	}
	// The check ns-load JVM (rig.runner) carries the flag.
	if got := logLinesWith(lines, "rig.runner"); len(got) != 1 || !strings.Contains(got[0], flag) {
		t.Errorf("ns-load JVMs = %v, want exactly one, carrying the compile flag", got)
	}
	// The build's AOT fork never reaches this wrapper: it runs on the kernel
	// JVM's own java.home, the workspace's JVM, not on the launcher rig was
	// invoked through.
	if got := logLinesWith(lines, "compile.clj"); len(got) != 0 {
		t.Errorf("AOT fork JVMs launched through the workspace java = %v, want none", got)
	}
	// The dev-execution run JVM is the only clojure.main launch here, and it
	// does not get the compile flag.
	runs := logLinesWith(lines, "clojure.main")
	if len(runs) != 1 {
		t.Fatalf("clojure.main JVMs = %v, want one (the dev run)", runs)
	}
	if strings.Contains(runs[0], flag) {
		t.Errorf("dev-execution run JVM must not get the compile flag:\n%s", runs[0])
	}
}

// TestHotBuildAOTForkUsesPickedJava checks that the build's AOT compile does
// not resolve its java from the environment: tools.build's default lookup
// ($JAVA_CMD, then java on PATH, then $JAVA_HOME) lands on the host JVM, and
// a build that compiles there is green about the wrong JVM. A decoy java
// first on PATH fails every JVM that resolves through it, so a green build
// proves the fork ran on the kernel JVM's own java instead.
func TestHotBuildAOTForkUsesPickedJava(t *testing.T) {
	hotSetup(t)
	picked, err := jvm.Find()
	if err != nil {
		t.Skipf("no java available: %v", err)
	}
	dir := t.TempDir()
	ran := filepath.Join(dir, "host-java-ran")
	script := "#!/bin/sh\n" +
		fmt.Sprintf("touch %q\n", ran) +
		"echo 'rig test: a JVM resolved java from PATH' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "java"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIG_JAVA", picked) // the workspace's JVM, picked before PATH changed
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if code, out := runCLI(t, "build", "-p", "modules/app", "--cache-dir", t.TempDir()); code != 0 {
		t.Fatalf("build exit = %d; out: %s", code, out)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("the build's AOT compile resolved its java from PATH, not the workspace's JVM")
	}
}

// TestHotLaunchOptsSeparation checks the per-context split: :rig/launch-opts
// reaches rig launch (and only it), :jvm-opts reaches rig run (and only it).
func TestHotLaunchOptsSeparation(t *testing.T) {
	hotSetup(t)
	logPath := filepath.Join(t.TempDir(), "java.log")
	loggingJava(t, logPath)
	cacheDir := t.TempDir()

	if code, out := runCLI(t, "build", "-p", "modules/app", "--cache-dir", cacheDir, "--uber"); code != 0 {
		t.Fatalf("build exit = %d; out: %s", code, out)
	}
	if code, out, _ := runCLISubprocess(t, "launch", "-p", "modules/app", "--cache-dir", cacheDir); code != 0 {
		t.Fatalf("launch exit = %d; out: %s", code, out)
	}
	if code, out := runCLI(t, "run", "-p", "modules/app", "--cache-dir", cacheDir); code != 0 {
		t.Fatalf("run exit = %d; out: %s", code, out)
	}

	lines := launchLogLines(t, logPath)
	launch := logLinesWith(lines, "app-uber.jar")
	if len(launch) != 1 {
		t.Fatalf("launch JVMs = %v, want exactly one", launch)
	}
	if !strings.Contains(launch[0], "-Drig.launch.opt=1") {
		t.Errorf("launch JVM missing :rig/launch-opts:\n%s", launch[0])
	}
	if strings.Contains(launch[0], "-Drig.dev.opt=1") {
		t.Errorf("launch JVM must not get the dev-execution :jvm-opts:\n%s", launch[0])
	}
	run := logLinesWith(lines, "clojure.main")
	if len(run) != 1 {
		t.Fatalf("run JVMs = %v, want exactly one", run)
	}
	if !strings.Contains(run[0], "-Drig.dev.opt=1") {
		t.Errorf("run JVM missing the dev-execution :jvm-opts:\n%s", run[0])
	}
	if strings.Contains(run[0], "-Drig.launch.opt=1") {
		t.Errorf("run JVM must not get :rig/launch-opts:\n%s", run[0])
	}
}

// TestHotBuildKernelGetsManagedJavaHome checks that the build's kernel JVM —
// the JVM the AOT compile fork inherits its environment from — runs with
// JAVA_HOME set to the managed JDK, as the prep, run, test and launch JVMs
// already do. A fake managed JDK whose java wrapper over the system java
// stands in for `rig jvm install`.
func TestHotBuildKernelGetsManagedJavaHome(t *testing.T) {
	hotSetup(t)
	real, err := jvm.Find()
	if err != nil {
		t.Skipf("no java available: %v", err)
	}
	absReal, err := filepath.Abs(real)
	if err != nil {
		t.Fatal(err)
	}
	v, err := jvm.Version(real)
	if err != nil {
		t.Fatal(err)
	}
	major := fmt.Sprintf("%d", jdk.FeatureVersion(v))
	cacheDir := t.TempDir()
	home := filepath.Join(fakeJDK(t, cacheDir, major+".0.10+7"), "jdk")
	absLog, err := filepath.Abs(filepath.Join(t.TempDir(), "java.log"))
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		fmt.Sprintf("echo \"JAVA_HOME=${JAVA_HOME:-} $*\" >> %q\n", absLog) +
		fmt.Sprintf("exec %q \"$@\"\n", absReal)
	if err := os.WriteFile(filepath.Join(home, "bin", "java"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("deps.edn",
		[]byte(fmt.Sprintf("{:rig/modules [\"modules/app\"]\n :rig/jvm %q}\n", major)), 0o644); err != nil {
		t.Fatal(err)
	}
	// The lock has to carry the pin before Rig can pick the managed JDK for
	// the workspace: the command that writes it cannot know it yet.
	if code, out := runCLI(t, "lock", "--cache-dir", cacheDir); code != 0 {
		t.Fatalf("lock exit = %d; out: %s", code, out)
	}
	// The lock write above needs no managed JDK, so the log can be absent.
	if err := os.Remove(absLog); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if code, out := runCLI(t, "build", "-p", "modules/app", "--cache-dir", cacheDir); code != 0 {
		t.Fatalf("build exit = %d; out: %s", code, out)
	}
	kernels := logLinesWith(launchLogLines(t, absLog), "--request")
	if len(kernels) == 0 {
		t.Fatal("the build launched no kernel JVM through the managed JDK")
	}
	for _, l := range kernels {
		if !strings.Contains(l, "JAVA_HOME="+home) {
			t.Errorf("kernel JVM launched without the managed JDK's JAVA_HOME:\n%s", l)
		}
	}
}
