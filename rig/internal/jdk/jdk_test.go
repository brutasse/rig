package jdk

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidRequested(t *testing.T) {
	good := []string{"21", "17", "21.0", "21.0.10", "21.0.10+7", "21.0.12.1+1", "21.0.12.1"}
	for _, s := range good {
		if !ValidRequested(s) {
			t.Errorf("ValidRequested(%q) = false, want true", s)
		}
	}
	bad := []string{"", "v21", "21.0.10.1.2", "+7", "21.", "2a", "21.0.10+", "999", " 21"}
	for _, s := range bad {
		if ValidRequested(s) {
			t.Errorf("ValidRequested(%q) = true, want false", s)
		}
	}
}

func TestFeatureVersion(t *testing.T) {
	for in, want := range map[string]int{
		"21.0.12":   21,
		"1.8.0_422": 8,
		"17.0.9":    17,
		"11.0.1+2":  11,
		"25":        25,
		"garbage":   0,
	} {
		if got := FeatureVersion(in); got != want {
			t.Errorf("FeatureVersion(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestSatisfies(t *testing.T) {
	cases := []struct {
		requested, candidate string
		want                 bool
	}{
		{"21", "21.0.10+7", true},
		{"21", "22.0.1+12", false},
		{"21.0", "21.0.10+7", true},
		{"21.0.10", "21.0.10+7", true},
		{"21.0.10", "21.0.11+9", false},
		{"21.0.10", "21.0.10+8", true},
		{"21.0.10+7", "21.0.10+7", true},
		{"21.0.10+7", "21.0.10+8", false},
		{"21.0.10+7", "21.0.10", false},
		{"21.0.10", "21.0.10.1+1", true},
		{"21.0.10.1", "21.0.10.1+1", true},
		{"21.0.10.1", "21.0.10.2+3", false},
		{"21.0.12.1+1", "21.0.12.1+1", true},
		{"21.0.12.1+1", "21.0.12.1+2", false},
		{"99", "21.0.10+7", false},
		{"bad", "21.0.10+7", false},
		{"21", "bad", false},
	}
	for _, c := range cases {
		if got := Satisfies(c.requested, c.candidate); got != c.want {
			t.Errorf("Satisfies(%q, %q) = %v, want %v", c.requested, c.candidate, got, c.want)
		}
	}
}

func TestCompare(t *testing.T) {
	older, newer := "21.0.10+7", "21.0.11+9"
	if Compare(older, newer) >= 0 {
		t.Errorf("Compare(%q, %q) should be negative", older, newer)
	}
	if Compare(newer, older) <= 0 {
		t.Errorf("Compare(%q, %q) should be positive", newer, older)
	}
	if Compare("21.0.10+7", "21.0.10+7") != 0 {
		t.Error("Compare equal versions should be 0")
	}
	if Compare("21.0.10", "21.0.10+7") > 0 {
		t.Error("build-less should not be newer than the same release with build")
	}
	if Compare("21.0.12.1+1", "21.0.11+10") <= 0 {
		t.Error("4-component version should order after the older 3-component one")
	}
}

func fakeInstall(t *testing.T, root, version string) *Store {
	t.Helper()
	st := NewStoreAt(root)
	dir := st.Dir(version)
	if err := os.MkdirAll(filepath.Join(dir, "jdk", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "jdk", "bin", javaName()), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := marker{Vendor: Vendor, Version: version, OS: "linux", Arch: "x64",
		Archive: "a.tar.gz", URL: "https://example.invalid/a.tar.gz",
		SHA256: strings.Repeat("0", 64), InstalledAt: "2026-01-01T00:00:00Z"}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rig-jdk.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestStoreLookup(t *testing.T) {
	st := fakeInstall(t, t.TempDir(), "21.0.10+7")
	inst, err := st.Lookup("21.0.10+7")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Home != filepath.Join(st.Root, Vendor+"-21.0.10+7", "jdk") {
		t.Errorf("home = %q", inst.Home)
	}
	if !strings.HasSuffix(inst.JavaPath, filepath.Join("bin", javaName())) {
		t.Errorf("java path = %q", inst.JavaPath)
	}
	if _, err := st.Lookup("17.0.13+11"); err != ErrNotInstalled {
		t.Errorf("missing lookup err = %v, want ErrNotInstalled", err)
	}
}

func TestStoreBest(t *testing.T) {
	root := t.TempDir()
	fakeInstall(t, root, "21.0.10+7")
	fakeInstall(t, root, "21.0.11+9")
	fakeInstall(t, root, "17.0.13+11")
	st := NewStoreAt(root)

	inst, err := st.Best("21")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Version != "21.0.11+9" {
		t.Errorf("best 21 = %q", inst.Version)
	}
	inst, err = st.Best("21.0.10")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Version != "21.0.10+7" {
		t.Errorf("best 21.0.10 = %q", inst.Version)
	}
	if _, err := st.Best("22"); err != ErrNotInstalled {
		t.Errorf("best 22 err = %v, want ErrNotInstalled", err)
	}
}

func TestStoreListUninstall(t *testing.T) {
	root := t.TempDir()
	fakeInstall(t, root, "21.0.10+7")
	fakeInstall(t, root, "17.0.13+11")
	st := NewStoreAt(root)

	insts, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(insts) != 2 || insts[0].Version != "21.0.10+7" || insts[1].Version != "17.0.13+11" {
		t.Errorf("list = %v", insts)
	}

	if _, err := st.Uninstall("17"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Uninstall("17"); err == nil {
		t.Error("second uninstall should fail")
	}
	fakeInstall(t, root, "21.0.11+9")
	if _, err := st.Uninstall("21.0.1"); err == nil {
		t.Error("ambiguous prefix should fail")
	}
	if v, err := st.Uninstall("21.0.10+7"); err != nil || v != "21.0.10+7" {
		t.Errorf("uninstall = %q, %v", v, err)
	}
}

// makeTargz builds a JDK-shaped tarball and returns its path and sha256.
func makeTargz(t *testing.T, version string) (string, string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "jdk.tar.gz")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	addDir := func(name string) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
	}
	addFile := func(name string, body []byte, mode int64) {
		hdr := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(body))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	addDir("jdk-" + version)
	addDir("jdk-" + version + "/bin")
	addFile("jdk-"+version+"/bin/"+javaName(), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	addFile("jdk-"+version+"/lib/modules", []byte("modules\n"), 0o644)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return src, hex.EncodeToString(sum[:])
}

// apiServer serves the Adoptium feature_releases endpoint and a download for
// the given version (code != 200 makes the API endpoint fail).
func apiServer(t *testing.T, version, archive, sum string, code int) *httptest.Server {
	t.Helper()
	osN, archN, ok := platform()
	if !ok {
		t.Skipf("no platform mapping for %s/%s", osN, archN)
	}
	rels := []apiRelease{{
		ReleaseName: "jdk-" + version,
		Binaries: []apiBinary{{
			OS:           osN,
			Architecture: archN,
			ImageType:    "jdk",
			HeapSize:     "normal",
		}},
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("/v3/assets/feature_releases/", func(w http.ResponseWriter, r *http.Request) {
		if code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rels)
	})
	mux.HandleFunc("/dl", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, archive)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	b := rels[0].Binaries[0]
	b.Package.Name = "OpenJDK-jdk.tar.gz"
	b.Package.Link = srv.URL + "/dl"
	b.Package.Checksum = sum
	b.Package.Size = 10
	rels[0].Binaries[0] = b
	return srv
}

func TestResolve(t *testing.T) {
	archive, sum := makeTargz(t, "21.0.10+7")
	srv := apiServer(t, "21.0.10+7", archive, sum, http.StatusOK)
	api := NewAPI(srv.URL)

	a, err := api.Resolve(context.Background(), "21")
	if err != nil {
		t.Fatal(err)
	}
	if a.Version != "21.0.10+7" || a.SHA256 != sum || !strings.HasSuffix(a.URL, "/dl") {
		t.Errorf("asset = %+v", a)
	}
	a, err = api.Resolve(context.Background(), "21.0.10")
	if err != nil || a.Version != "21.0.10+7" {
		t.Errorf("resolve 21.0.10 = %q, %v", a.Version, err)
	}
	a, err = api.Resolve(context.Background(), "21.0.10+7")
	if err != nil || a.Version != "21.0.10+7" {
		t.Errorf("resolve exact = %q, %v", a.Version, err)
	}
	if _, err = api.Resolve(context.Background(), "21.0.11+9"); err == nil {
		t.Error("resolve of an unknown release should fail")
	}
}

func TestResolveUnknownFeature(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	api := NewAPI(srv.URL)
	if _, err := api.Resolve(context.Background(), "99"); err == nil || !strings.Contains(err.Error(), "no GA releases") {
		t.Errorf("err = %v", err)
	}
}

func TestInstall(t *testing.T) {
	archive, sum := makeTargz(t, "21.0.10+7")
	srv := apiServer(t, "21.0.10+7", archive, sum, http.StatusOK)
	st := NewStoreAt(t.TempDir())

	a, err := NewAPI(srv.URL).Resolve(context.Background(), "21")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := st.Install(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Version != "21.0.10+7" {
		t.Errorf("version = %q", inst.Version)
	}
	if _, err := os.Stat(inst.JavaPath); err != nil {
		t.Errorf("java binary: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(inst.Home, "lib", "modules")); err != nil || string(got) != "modules\n" {
		t.Errorf("modules file: %q, %v", got, err)
	}
	if inst.SHA256 != sum {
		t.Errorf("sha256 = %q, want %q", inst.SHA256, sum)
	}
	// Re-install is a no-op.
	if _, err := st.Install(context.Background(), a); err != nil {
		t.Fatal(err)
	}
}

func TestInstallChecksumMismatch(t *testing.T) {
	archive, _ := makeTargz(t, "21.0.10+7")
	srv := apiServer(t, "21.0.10+7", archive, strings.Repeat("f", 64), http.StatusOK)
	st := NewStoreAt(t.TempDir())
	a, err := NewAPI(srv.URL).Resolve(context.Background(), "21")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Install(context.Background(), a); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("err = %v", err)
	}
}

func TestInstallUnsafePath(t *testing.T) {
	root := t.TempDir()
	st := NewStoreAt(root)
	// Archive with a path-traversal entry must be refused; "../../escape-target"
	// resolves to <root>/escape-target, outside the scratch dir.
	src := filepath.Join(t.TempDir(), "evil.tar.gz")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	body := []byte("evil")
	hdr := &tar.Header{Name: "../../escape-target", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	tw.Write(body)
	tw.Close()
	gz.Close()
	f.Close()

	if _, err := st.extract(src, "evil.tar.gz"); err == nil {
		t.Error("expected unsafe-path error")
	}
	if _, err := os.Stat(filepath.Join(root, "escape-target")); err == nil {
		t.Error("path traversal wrote outside the scratch dir")
	}
}

func TestEnsureOffline(t *testing.T) {
	st := NewStoreAt(t.TempDir())
	_, err := Ensure(context.Background(), st, "21", "", true)
	if err == nil || !strings.Contains(err.Error(), "offline") {
		t.Errorf("err = %v", err)
	}
	_, err = Ensure(context.Background(), st, "21", "21.0.10+7", true)
	if err == nil || !strings.Contains(err.Error(), "21.0.10+7") {
		t.Errorf("err = %v", err)
	}
}

func TestEnsureUsesInstalled(t *testing.T) {
	root := t.TempDir()
	st := NewStoreAt(root)
	fakeInstall(t, root, "21.0.11+9")
	inst, err := Ensure(context.Background(), st, "21", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Version != "21.0.11+9" {
		t.Errorf("version = %q (should use the installed JDK, not download)", inst.Version)
	}
}

func TestLatestLTS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/info/available_releases" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"most_recent_lts": 25, "most_recent_feature_release": 27}`))
	}))
	t.Cleanup(srv.Close)
	got, err := NewAPI(srv.URL).LatestLTS(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != 25 {
		t.Errorf("LatestLTS = %d, want 25", got)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(bad.Close)
	if _, err := NewAPI(bad.URL).LatestLTS(context.Background()); err == nil {
		t.Error("non-200 response should fail")
	}
}
