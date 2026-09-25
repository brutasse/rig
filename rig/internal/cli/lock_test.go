package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brutasse/rig/internal/digest"
	"github.com/brutasse/rig/internal/jdk"
	"github.com/brutasse/rig/internal/jvm"
	"github.com/brutasse/rig/internal/kernel"
	"github.com/brutasse/rig/internal/lockfile"
)

func kernelJarPath(t *testing.T) string {
	t.Helper()
	var p string
	if v := os.Getenv("RIG_TEST_KERNEL_JAR"); v != "" && statOK(v) {
		p = v
	}
	if p == "" {
		if cand := filepath.Join("..", "..", "..", "resolver", "target", "rig-resolver-v0.1.0.jar"); statOK(cand) {
			p = cand
		}
	}
	if p == "" {
		t.Skip("kernel jar not found; build it with: cd resolver && clojure -X:build (or set RIG_TEST_KERNEL_JAR)")
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func statOK(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func TestLockVerifyEndToEnd(t *testing.T) {
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

	dir := t.TempDir()
	t.Chdir(dir)
	cacheDir := t.TempDir()
	writeFile(t, "deps.edn", "{:rig/lib \"example/fixture\" :paths [\"src\"] :deps {org.clojure/clojure {:mvn/version \"1.12.5\"}}}\n")

	code, out := runCLI(t, "lock", "--cache-dir", cacheDir)
	if code != 0 && strings.Contains(out, "status 429") {
		time.Sleep(10 * time.Second)
		code, out = runCLI(t, "lock", "--cache-dir", cacheDir)
	}
	if code != 0 {
		t.Fatalf("lock exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "wrote") || !statOK("deps.lock") {
		t.Fatalf("lock did not write deps.lock; out: %s", out)
	}

	code, out = runCLI(t, "verify", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("verify exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "verified 3 artifacts") {
		t.Fatalf("verify out = %q", out)
	}

	entries, err := os.ReadDir(filepath.Join(cacheDir, "artifacts"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("cache empty after lock: %v", err)
	}
	corrupted := filepath.Join(cacheDir, "artifacts", entries[0].Name())
	b, err := os.ReadFile(corrupted)
	if err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0xFF
	if err := os.WriteFile(corrupted, b, 0o644); err != nil {
		t.Fatal(err)
	}
	code, out = runCLI(t, "verify", "--cache-dir", cacheDir)
	if code != 4 {
		t.Fatalf("corrupted verify exit = %d, want 4; out: %s", code, out)
	}
	if !strings.Contains(out, "hash mismatch") {
		t.Fatalf("corrupted verify out = %q", out)
	}

	if err := os.RemoveAll(cacheDir); err != nil {
		t.Fatal(err)
	}
	home := os.Getenv("HOME")
	os.Setenv("HOME", t.TempDir())
	code, out = runCLI(t, "verify", "--offline", "--cache-dir", cacheDir)
	os.Setenv("HOME", home)
	if code != 1 {
		t.Fatalf("offline verify exit = %d, want 1; out: %s", code, out)
	}
	if !strings.Contains(out, "offline") {
		t.Fatalf("offline verify out = %q", out)
	}
}

// TestLockRecordsJVM checks the full :rig/jvm flow end to end: the kernel
// copies the pin into the lock and Go resolves the exact version through the
// Adoptium API.
func TestLockRecordsJVM(t *testing.T) {
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

	dir := t.TempDir()
	t.Chdir(dir)
	cacheDir := t.TempDir()
	writeFile(t, "deps.edn", "{:rig/lib \"example/fixture\" :rig/jvm \"21\"}\n")

	code, out := runCLI(t, "lock", "--cache-dir", cacheDir)
	if code != 0 && strings.Contains(out, "status 429") {
		time.Sleep(10 * time.Second)
		code, out = runCLI(t, "lock", "--cache-dir", cacheDir)
	}
	if code != 0 {
		t.Fatalf("lock exit = %d, want 0; out: %s", code, out)
	}
	doc, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	if doc.JVM == nil {
		t.Fatal("lock has no jvm field")
	}
	if doc.JVM.Vendor != "temurin" || doc.JVM.Requested != "21" {
		t.Errorf("jvm = %+v", doc.JVM)
	}
	if doc.JVM.Version == "" || !jdk.Satisfies(doc.JVM.Requested, doc.JVM.Version) {
		t.Errorf("jvm version = %q (should be resolved from the Adoptium API)", doc.JVM.Version)
	}

	// Re-locking keeps the exact pinned version (no re-resolution drift).
	// RIG_JAVA sidesteps the managed-JDK ensure (no 200 MB download in tests).
	if real, err := jvm.Find(); err == nil {
		t.Setenv("RIG_JAVA", real)
	}
	code, out = runCLI(t, "lock", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("relock exit = %d, want 0; out: %s", code, out)
	}
	doc2, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	if doc2.JVM == nil || doc2.JVM.Version != doc.JVM.Version {
		t.Errorf("relock changed the pinned version: %q -> %q",
			doc.JVM.Version, func() string {
				if doc2.JVM == nil {
					return "<nil>"
				}
				return doc2.JVM.Version
			}())
	}
}
