package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brutasse/rig/internal/jvm"
)

// stdinEOF points the process (and the forked JVM) at an empty stdin for
// the duration of the test: the nREPL console sees EOF, quits, and the
// server shuts down — which is how these tests end the session.
func stdinEOF(t *testing.T) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(f)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = in
	t.Cleanup(func() {
		os.Stdin = old
		in.Close()
	})
}

func TestNreplStartsAndStops(t *testing.T) {
	hotSetup(t)
	stdinEOF(t)
	cacheDir := t.TempDir()
	code, out := runCLI(t, "nrepl", "--cache-dir", cacheDir, "-p", "modules/app")
	if code != 0 {
		t.Fatalf("nrepl exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "nREPL server started on port") {
		t.Errorf("nrepl out = %q, want the started banner", out)
	}
}

func TestNreplPinnedPort(t *testing.T) {
	hotSetup(t)
	stdinEOF(t)
	cacheDir := t.TempDir()
	code, out := runCLI(t, "nrepl", "--cache-dir", cacheDir, "-p", "modules/app", "--port", "41873")
	if code != 0 {
		t.Fatalf("nrepl exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "nREPL server started on port 41873") {
		t.Errorf("nrepl out = %q, want the banner on the requested port", out)
	}
}

func TestNreplClojureFloor(t *testing.T) {
	jar := kernelJarPath(t)
	if _, err := jvm.Find(); err != nil {
		t.Skipf("no java available: %v", err)
	}
	os.Setenv("RIG_KERNEL_JAR", jar)
	t.Cleanup(func() { os.Unsetenv("RIG_KERNEL_JAR") })
	t.Chdir(t.TempDir())
	writeFile(t, "deps.edn", "{:rig/lib \"example/floor\" :paths [\"src\"] :deps {org.clojure/clojure {:mvn/version \"1.9.0\"}}}\n")
	code, out := runCLI(t, "lock", "--cache-dir", t.TempDir())
	if code != 0 && strings.Contains(out, "status 429") {
		code, out = runCLI(t, "lock", "--cache-dir", t.TempDir())
	}
	if code != 0 {
		t.Fatalf("lock exit = %d, want 0; out: %s", code, out)
	}

	code, out = runCLI(t, "nrepl", "--cache-dir", t.TempDir())
	if code != 2 {
		t.Fatalf("nrepl exit = %d, want 2; out: %s", code, out)
	}
	if !strings.Contains(out, "1.9.0") || !strings.Contains(out, "1.10.0") {
		t.Errorf("nrepl out = %q, want the locked version and the floor hint", out)
	}
}
