package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brutasse/rig/internal/jvm"
)

// loggingJava installs a wrapper for the system java that appends every
// launch's argv to logPath (one line per launch, args space-separated)
// before exec'ing the real java, and points RIG_JAVA at it. It lets a test
// assert which flags reached which JVM without rig-side instrumentation.
func loggingJava(t *testing.T, logPath string) {
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
// kernel (AOT) JVM and the check ns-load JVM get the flags; the check
// stage-1 kernel (metadata only) and the dev-execution run JVM do not.
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
	// The dev-execution run JVM does not.
	if got := logLinesWith(lines, "clojure.main"); len(got) != 1 || strings.Contains(got[0], flag) {
		t.Errorf("run JVMs = %v, want exactly one, without the compile flag", got)
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
	if code, out := runCLI(t, "launch", "-p", "modules/app", "--cache-dir", cacheDir); code != 0 {
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
