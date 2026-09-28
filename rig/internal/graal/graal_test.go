package graal

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func fakeInstall(t *testing.T, root, version string) *Store {
	t.Helper()
	st := NewStoreAt(root)
	dir := st.Dir(version)
	if err := os.MkdirAll(filepath.Join(dir, "graal", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"java", "native-image"} {
		if err := os.WriteFile(filepath.Join(dir, "graal", "bin", name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m := marker{Vendor: Vendor, Version: version, OS: "linux", Arch: "x64",
		Archive: "a.tar.gz", URL: "https://example.invalid/a.tar.gz",
		SHA256: strings.Repeat("0", 64), InstalledAt: "2026-01-01T00:00:00Z"}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rig-graal.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestStoreLookup(t *testing.T) {
	st := fakeInstall(t, t.TempDir(), "25.0.2")
	inst, err := st.Lookup("25.0.2")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Home != filepath.Join(st.Root, Vendor+"-25.0.2", "graal") {
		t.Errorf("home = %q", inst.Home)
	}
	if !strings.HasSuffix(inst.NativeImagePath, filepath.Join("bin", "native-image")) {
		t.Errorf("native-image path = %q", inst.NativeImagePath)
	}
	if _, err := st.Lookup("25.0.1"); err != ErrNotInstalled {
		t.Errorf("missing lookup err = %v, want ErrNotInstalled", err)
	}
}

func TestStoreBestList(t *testing.T) {
	root := t.TempDir()
	fakeInstall(t, root, "25.0.2")
	fakeInstall(t, root, "25.0.1")
	fakeInstall(t, root, "24.0.2")
	st := NewStoreAt(root)

	inst, err := st.Best("25")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Version != "25.0.2" {
		t.Errorf("best 25 = %q", inst.Version)
	}
	inst, err = st.Best("25.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Version != "25.0.1" {
		t.Errorf("best 25.0.1 = %q", inst.Version)
	}
	if _, err := st.Best("21"); err != ErrNotInstalled {
		t.Errorf("best 21 err = %v, want ErrNotInstalled", err)
	}

	insts, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(insts) != 3 || insts[0].Version != "25.0.2" || insts[2].Version != "24.0.2" {
		t.Errorf("list = %v", insts)
	}
}

func TestStoreUninstall(t *testing.T) {
	root := t.TempDir()
	fakeInstall(t, root, "25.0.2")
	fakeInstall(t, root, "24.0.2")
	st := NewStoreAt(root)

	if _, err := st.Uninstall("24"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(st.Dir("24.0.2")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dir 24.0.2 still present: %v", err)
	}
	if _, err := st.Uninstall("24"); err == nil {
		t.Error("second uninstall should fail")
	}
	fakeInstall(t, root, "25.0.19")
	if _, err := st.Uninstall("25.0"); err == nil {
		t.Error("ambiguous prefix should fail")
	}
	if v, err := st.Uninstall("25.0.2"); err != nil || v != "25.0.2" {
		t.Errorf("uninstall = %q, %v", v, err)
	}
}

// makeTargz builds a GraalVM-shaped tarball and returns its path and sha256.
func makeTargz(t *testing.T, version string) (string, string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "graalvm.tar.gz")
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
	top := "graalvm-jdk-" + version
	addDir(top)
	addDir(top + "/bin")
	addFile(top+"/bin/java", []byte("#!/bin/sh\nexit 0\n"), 0o755)
	addFile(top+"/bin/native-image", []byte("#!/bin/sh\nexit 0\n"), 0o755)
	addFile(top+"/lib/modules", []byte("modules\n"), 0o644)
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

// testServer wraps an httptest server with a counter for requests on the
// releases API (the exact-version path must never use it).
type testServer struct {
	*httptest.Server
	apiCalls atomic.Int32
}

// apiServer serves the GitHub releases list (newest first, as the API does),
// the archive/sha256 downloads for the given version, and the direct
// release-asset download URLs for it (the exact-version path).
func apiServer(t *testing.T, version, archive, sum string, code int) *testServer {
	t.Helper()
	osN, archN, ok := platform()
	if !ok {
		t.Skipf("no platform mapping for %s/%s", osN, archN)
	}
	want := "graalvm-community-jdk-" + version + "_" + osN + "-" + archN + "_bin"
	rels := []apiRelease{
		// An Innovation release ahead of the jdk-* one: its assets are named
		// by a different version and must be skipped.
		{TagName: "graal-99.9.9", Assets: []apiAsset{{
			Name:               "graalvm-community-jdk-99i9-99.0.9_" + osN + "-" + archN + "_bin.tar.gz",
			BrowserDownloadURL: "https://example.invalid/innovation.tar.gz",
		}}},
		{TagName: "jdk-" + version, Assets: []apiAsset{
			{Name: want + ".tar.gz", BrowserDownloadURL: "", Size: 10},
			{Name: want + ".tar.gz.sha256", BrowserDownloadURL: ""},
		}},
	}
	srv := &testServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+repo+"/releases", func(w http.ResponseWriter, r *http.Request) {
		srv.apiCalls.Add(1)
		if code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rels)
	})
	mux.HandleFunc("/"+repo+"/releases/download/jdk-"+version+"/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/"+repo+"/releases/download/jdk-"+version+"/")
		switch name {
		case want + ".tar.gz", want + ".zip":
			http.ServeFile(w, r, archive)
		case want + ".tar.gz.sha256", want + ".zip.sha256":
			w.Write([]byte(sum + "\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	mux.HandleFunc("/dl", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, archive)
	})
	mux.HandleFunc("/sha", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sum + "\n"))
	})
	srv.Server = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	rels[1].Assets[0].BrowserDownloadURL = srv.URL + "/dl"
	rels[1].Assets[1].BrowserDownloadURL = srv.URL + "/sha"
	return srv
}

// withDownloadBase points the release asset download host at base for the
// test.
func withDownloadBase(t *testing.T, base string) {
	t.Helper()
	old := DownloadBase
	DownloadBase = base
	t.Cleanup(func() { DownloadBase = old })
}

func TestResolve(t *testing.T) {
	archive, sum := makeTargz(t, "25.0.2")
	srv := apiServer(t, "25.0.2", archive, sum, http.StatusOK)
	api := NewAPI(srv.URL)

	a, err := api.Resolve(context.Background(), "25")
	if err != nil {
		t.Fatal(err)
	}
	if a.Version != "25.0.2" || a.SHA256 != sum || a.Vendor != Vendor || !strings.HasSuffix(a.URL, "/dl") {
		t.Errorf("asset = %+v", a)
	}
	if a.Archive != "graalvm-community-jdk-25.0.2_"+a.OS+"-"+a.Arch+"_bin.tar.gz" {
		t.Errorf("archive name = %q", a.Archive)
	}
	if _, err := api.Resolve(context.Background(), "24"); err == nil {
		t.Error("resolve of an unserved feature version should fail")
	}
}

func TestResolveExact(t *testing.T) {
	archive, sum := makeTargz(t, "21.0.2")
	srv := apiServer(t, "21.0.2", archive, sum, http.StatusOK)
	withDownloadBase(t, srv.URL)
	a, err := NewAPI(srv.URL).Resolve(context.Background(), "21.0.2")
	if err != nil {
		t.Fatal(err)
	}
	if a.Version != "21.0.2" || a.SHA256 != sum {
		t.Errorf("asset = %+v", a)
	}
	if a.Archive != "graalvm-community-jdk-21.0.2_"+a.OS+"-"+a.Arch+"_bin.tar.gz" {
		t.Errorf("archive name = %q", a.Archive)
	}
	if !strings.HasSuffix(a.URL, "/"+repo+"/releases/download/jdk-21.0.2/"+a.Archive) {
		t.Errorf("url = %q", a.URL)
	}
	if n := srv.apiCalls.Load(); n != 0 {
		t.Errorf("releases API called %d times; an exact version must not use it", n)
	}
}

func TestResolveExactMissing(t *testing.T) {
	// A fully-specified version with no such release: the sidecar 404s and
	// the error names the version.
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	withDownloadBase(t, srv.URL)
	_, err := NewAPI(srv.URL).Resolve(context.Background(), "21.0.99")
	if err == nil || !strings.Contains(err.Error(), "no graalvm") {
		t.Errorf("err = %v", err)
	}
}

func TestResolveRateLimitBody(t *testing.T) {
	// A rate-limited API response must surface the server's message.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"API rate limit exceeded for 1.2.3.4"}`))
	}))
	t.Cleanup(srv.Close)
	_, err := NewAPI(srv.URL).Resolve(context.Background(), "25")
	if err == nil || !strings.Contains(err.Error(), "API rate limit exceeded") {
		t.Errorf("err = %v", err)
	}
}

func TestResolveBadSidecar(t *testing.T) {
	archive, _ := makeTargz(t, "25.0.2")
	srv := apiServer(t, "25.0.2", archive, "not-a-sha", http.StatusOK)
	if _, err := NewAPI(srv.URL).Resolve(context.Background(), "25"); err == nil || !strings.Contains(err.Error(), "bad sha256 sidecar") {
		t.Errorf("err = %v", err)
	}
}

func TestResolveAPIError(t *testing.T) {
	archive, sum := makeTargz(t, "25.0.2")
	srv := apiServer(t, "25.0.2", archive, sum, http.StatusBadGateway)
	if _, err := NewAPI(srv.URL).Resolve(context.Background(), "25"); err == nil {
		t.Error("expected an API error")
	}
}

func TestInstall(t *testing.T) {
	archive, sum := makeTargz(t, "25.0.2")
	srv := apiServer(t, "25.0.2", archive, sum, http.StatusOK)
	st := NewStoreAt(t.TempDir())

	a, err := NewAPI(srv.URL).Resolve(context.Background(), "25")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := st.Install(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Version != "25.0.2" {
		t.Errorf("version = %q", inst.Version)
	}
	if _, err := os.Stat(inst.NativeImagePath); err != nil {
		t.Errorf("native-image binary: %v", err)
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
	archive, _ := makeTargz(t, "25.0.2")
	srv := apiServer(t, "25.0.2", archive, strings.Repeat("f", 64), http.StatusOK)
	st := NewStoreAt(t.TempDir())
	a, err := NewAPI(srv.URL).Resolve(context.Background(), "25")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Install(context.Background(), a); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("err = %v", err)
	}
}

func TestEnsureMissing(t *testing.T) {
	st := NewStoreAt(t.TempDir())
	_, err := Ensure(st, "25", "")
	if err == nil || !strings.Contains(err.Error(), "graalvm 25 not installed (run 'rig graalvm install 25')") {
		t.Errorf("err = %v", err)
	}
	_, err = Ensure(st, "25", "25.0.2")
	if err == nil || !strings.Contains(err.Error(), "graalvm 25.0.2 not installed (run 'rig graalvm install 25.0.2')") {
		t.Errorf("err = %v", err)
	}
}

func TestEnsureNoFallback(t *testing.T) {
	root := t.TempDir()
	fakeInstall(t, root, "25.0.1")
	// An exact pin is never satisfied by another installed version.
	if _, err := Ensure(NewStoreAt(root), "25", "25.0.2"); err == nil {
		t.Error("expected an error: 25.0.2 is not installed")
	}
}

func TestEnsureUsesInstalled(t *testing.T) {
	root := t.TempDir()
	fakeInstall(t, root, "25.0.2")
	inst, err := Ensure(NewStoreAt(root), "25", "")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Version != "25.0.2" {
		t.Errorf("version = %q (should use the installed GraalVM, not download)", inst.Version)
	}
}

func TestEnsureOverride(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "bin", "native-image"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIG_GRAALVM_HOME", home)

	inst, err := Ensure(NewStoreAt(t.TempDir()), "25", "25.0.2")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Home != home {
		t.Errorf("home = %q, want the override", inst.Home)
	}
	if inst.NativeImagePath != filepath.Join(home, "bin", "native-image") {
		t.Errorf("native-image = %q", inst.NativeImagePath)
	}

	// A home without native-image is refused.
	empty := t.TempDir()
	t.Setenv("RIG_GRAALVM_HOME", empty)
	if _, err := Ensure(NewStoreAt(t.TempDir()), "25", "25.0.2"); err == nil {
		t.Error("expected an error for an override home without native-image")
	}
}
