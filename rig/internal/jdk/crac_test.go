package jdk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// withCRaCPin points the pin table at a fixture asset for the running
// platform and returns it.
func withCRaCPin(t *testing.T, url, sha, version string) Asset {
	t.Helper()
	a := Asset{
		Vendor: CRaCVendor, Version: version,
		OS: runtime.GOOS, Arch: "x64",
		Archive: "crac.tar.gz", URL: url, SHA256: sha, Size: 10,
	}
	old := CRaCPins
	t.Cleanup(func() { CRaCPins = old })
	CRaCPins = map[string]Asset{runtime.GOOS + "/" + runtime.GOARCH: a}
	return a
}

func TestCRaCInstall(t *testing.T) {
	archive, sum := makeTargz(t, "25.0.4+1")
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.ServeFile(w, r, archive)
	}))
	defer srv.Close()
	a := withCRaCPin(t, srv.URL, sum, "25.0.4+1")

	st := NewStoreAt(t.TempDir())
	inst, err := EnsureCRaC(context.Background(), st)
	if err != nil {
		t.Fatalf("EnsureCRaC: %v", err)
	}
	if inst.Vendor != CRaCVendor || inst.Version != a.Version {
		t.Errorf("install = %s %s, want %s %s", inst.Vendor, inst.Version, CRaCVendor, a.Version)
	}
	if base := filepath.Base(inst.Dir); base != CRaCVendor+"-25.0.4+1" {
		t.Errorf("install dir = %s, want %s-25.0.4+1", base, CRaCVendor)
	}
	if st, err := os.Stat(inst.JavaPath); err != nil || st.IsDir() {
		t.Errorf("JavaPath %s not executable-looking: %v", inst.JavaPath, err)
	}

	// Namespaced away from workspace selection: invisible to List/Best and
	// the temurin-vendor Lookup, so a workspace pin can never pick it.
	if insts, err := st.List(); err != nil || len(insts) != 0 {
		t.Errorf("List() = %v, %v; want empty", insts, err)
	}
	if _, err := st.Best("25"); err != ErrNotInstalled {
		t.Errorf("Best(25) err = %v, want ErrNotInstalled", err)
	}
	if _, err := st.Lookup("25.0.4+1"); err != ErrNotInstalled {
		t.Errorf("temurin Lookup err = %v, want ErrNotInstalled", err)
	}
	if _, err := st.Uninstall("25.0.4+1"); err == nil {
		t.Error("Uninstall reached the CRaC install; want nothing installed matching")
	}

	// Idempotent: same install returned without a second download.
	if _, err := CRaCInstalled(st); err != nil {
		t.Fatalf("CRaCInstalled: %v", err)
	}
	if again, err := EnsureCRaC(context.Background(), st); err != nil || again.Dir != inst.Dir {
		t.Errorf("second EnsureCRaC = %v, %v; want same dir", again, err)
	}
	if hits != 1 {
		t.Errorf("downloads = %d, want 1", hits)
	}
	// The marker carries the CRaC vendor (a temurin-vendor marker would be
	// picked up by LookupVendor's vendor gate only via this field).
	b, err := os.ReadFile(filepath.Join(inst.Dir, "rig-jdk.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["vendor"] != CRaCVendor {
		t.Errorf("marker vendor = %v, want %s", m["vendor"], CRaCVendor)
	}
}

func TestCRaCInstallChecksumMismatch(t *testing.T) {
	archive, sum := makeTargz(t, "25.0.4+1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, archive)
	}))
	defer srv.Close()
	bad := strings.Repeat("0", 63) + "1"
	if bad == sum {
		t.Fatal("fixture collision")
	}
	withCRaCPin(t, srv.URL, bad, "25.0.4+1")

	st := NewStoreAt(t.TempDir())
	if _, err := EnsureCRaC(context.Background(), st); err == nil ||
		!strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("EnsureCRaC err = %v, want sha256 mismatch", err)
	}
	if _, err := CRaCInstalled(st); err != ErrNotInstalled {
		t.Errorf("CRaCInstalled after mismatch = %v, want ErrNotInstalled", err)
	}
	entries, err := os.ReadDir(st.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), CRaCVendor+"-") {
			t.Errorf("leftover install dir after mismatch: %s", e.Name())
		}
	}
}

func TestPinnedCRaCSane(t *testing.T) {
	for plat, a := range CRaCPins {
		if !strings.HasPrefix(plat, "linux/") {
			t.Errorf("pin for %s: checkpointing is Linux-only", plat)
		}
		if len(a.SHA256) != 64 || a.URL == "" || a.Version == "" || a.Vendor != CRaCVendor {
			t.Errorf("pin %s malformed: %+v", plat, a)
		}
		if !strings.HasSuffix(a.URL, a.Archive) {
			t.Errorf("pin %s: URL %s does not end in archive %s", plat, a.URL, a.Archive)
		}
	}
}
