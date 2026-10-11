package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brutasse/rig/internal/digest"
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
		if cand := filepath.Join("..", "..", "..", "resolver", "target", "rig-resolver-"+resolverVersion()+".jar"); statOK(cand) {
			p = cand
		}
	}
	if p == "" {
		t.Skip("kernel jar not found; build it with: make kernel (or set RIG_TEST_KERNEL_JAR)")
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

func TestLockFrozenRejected(t *testing.T) {
	// --frozen means "never write the lock"; rig lock writes it. The
	// combination used to silently ignore the flag and rewrite
	// deps.lock anyway — a CI trap. It is a usage error now, refused
	// before anything resolves or reads the workspace.
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", "{}\n")
	code, out := runCLI(t, "lock", "--frozen", "--cache-dir", t.TempDir())
	if code != 2 || !strings.Contains(out, "--frozen") {
		t.Fatalf("lock --frozen exit = %d, want 2 with the flag named; out: %s", code, out)
	}
	if statOK("deps.lock") {
		t.Errorf("lock --frozen wrote deps.lock anyway")
	}
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

	// The resolver block is Go's stamp: the pin, plus the sha256 of the
	// kernel jar that actually ran (here the RIG_KERNEL_JAR override).
	doc, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Resolver.Lib != kernel.Current.Lib ||
		doc.Resolver.Version != kernel.Current.Version ||
		doc.Resolver.GitSHA != kernel.Current.GitSHA {
		t.Errorf("resolver identity = %+v, want pin %+v", doc.Resolver, kernel.Current)
	}
	if doc.Resolver.SHA256 != sha {
		t.Errorf("resolver sha256 = %s, want %s (the launched jar)", doc.Resolver.SHA256, sha)
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

// TestLockNamespacedMapDeps checks that a manifest using the compact
// `#:mvn{…}` reader-macro form in :deps locks cleanly. The ednlit reader
// understands the `#:qualifier {…}` dispatch (mirroring the Clojure reader);
// previously it failed with "unsupported dispatch" on such a manifest.
func TestLockNamespacedMapDeps(t *testing.T) {
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
	writeFile(t, "deps.edn", "{:rig/lib \"example/fixture\" :paths [\"src\"] :deps {org.clojure/clojure #:mvn{:version \"1.12.5\"}}}\n")

	code, out := runCLI(t, "lock", "--cache-dir", cacheDir)
	if code != 0 && strings.Contains(out, "status 429") {
		time.Sleep(10 * time.Second)
		code, out = runCLI(t, "lock", "--cache-dir", cacheDir)
	}
	if code != 0 {
		t.Fatalf("lock exit = %d, want 0; out: %s", code, out)
	}
	if !statOK("deps.lock") {
		t.Fatalf("lock did not write deps.lock; out: %s", out)
	}
}

// TestLockRecordsJVM checks the :rig/jvm flow end to end: the kernel copies
// the major-version pin into the lock.
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

	// Re-locking keeps the pin.
	// RIG_JAVA sidesteps the managed-JDK pick (no 200 MB download in tests).
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
	if doc2.JVM == nil || doc2.JVM.Requested != "21" {
		t.Errorf("relock jvm = %+v, want pin 21", doc2.JVM)
	}
}
