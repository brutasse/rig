package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/brutasse/rig/internal/jdk"
)

func TestCRACInstallAndStatus(t *testing.T) {
	t.Setenv("RIG_CRAC_JDK", "") // TestMain sentinel would print an override line
	archive, sum := makeFakeJDKTarball(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, archive)
	}))
	t.Cleanup(srv.Close)
	oldPins := jdk.CRaCPins
	t.Cleanup(func() { jdk.CRaCPins = oldPins })
	jdk.CRaCPins = map[string]jdk.Asset{
		runtime.GOOS + "/" + runtime.GOARCH: {
			Vendor: jdk.CRaCVendor, Version: "99.0.0+1",
			OS: runtime.GOOS, Arch: "x64",
			Archive: "crac.tar.gz", URL: srv.URL, SHA256: sum, Size: 10,
		},
	}

	cacheDir := t.TempDir()
	code, out := runCLI(t, "crac", "status", "--cache-dir", cacheDir)
	if code != 0 || !strings.Contains(out, "not installed (rig crac install)") {
		t.Fatalf("status before install: code %d; out: %s", code, out)
	}

	code, out = runCLI(t, "crac", "install", "--cache-dir", cacheDir)
	if code != 0 || !strings.Contains(out, "installed zulu-crac 99.0.0+1") {
		t.Fatalf("install: code %d; out: %s", code, out)
	}

	// Idempotent install, and status now reports the install dir.
	code, out = runCLI(t, "crac", "install", "--cache-dir", cacheDir)
	if code != 0 || !strings.Contains(out, "already installed") {
		t.Fatalf("second install: code %d; out: %s", code, out)
	}
	code, out = runCLI(t, "crac", "status", "--cache-dir", cacheDir)
	if code != 0 || !strings.Contains(out, "zulu-crac 99.0.0+1") ||
		!strings.Contains(out, "installed") || strings.Contains(out, "not installed") {
		t.Fatalf("status after install: code %d; out: %s", code, out)
	}

	// The CRaC install stays out of the workspace JDK listing.
	code, out = runCLI(t, "jvm", "list", "--cache-dir", cacheDir)
	if code != 0 || strings.Contains(out, "zulu-crac") {
		t.Fatalf("jvm list leaked the CRaC install: code %d; out: %s", code, out)
	}
}

// Pruning semantics are covered by kernelrun.TestClean; through the CLI the
// resolution gates are what matters: clean never downloads the kernel jar,
// and never classifies chains while a dev jar is in play.
func TestCRACCleanResolutionGates(t *testing.T) {
	t.Setenv("RIG_KERNEL_JAR", "") // dev shell override must not leak into the gate
	code, out := runCLI(t, "crac", "clean", "--cache-dir", t.TempDir())
	if code != 1 || !strings.Contains(out, "cannot resolve the kernel jar") {
		t.Fatalf("clean with uncached jar: code %d; out: %s", code, out)
	}

	t.Setenv("RIG_KERNEL_JAR", filepath.Join(t.TempDir(), "dev.jar"))
	if err := os.WriteFile(os.Getenv("RIG_KERNEL_JAR"), []byte("dev"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out = runCLI(t, "crac", "clean", "--cache-dir", t.TempDir())
	if code != 1 || !strings.Contains(out, "refusing to prune") {
		t.Fatalf("clean with dev jar: code %d; out: %s", code, out)
	}
}
