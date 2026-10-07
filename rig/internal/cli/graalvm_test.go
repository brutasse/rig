package cli

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/brutasse/rig/internal/graal"
	"github.com/brutasse/rig/internal/lockfile"
)

// graalPlatform mirrors the graal package's platform mapping (unexported
// there) so the fake release can name its assets for this machine.
func graalPlatform() (os, arch string, ok bool) {
	switch runtime.GOOS {
	case "linux":
		os = "linux"
	case "darwin":
		os = "macos"
	case "windows":
		os = "windows"
	default:
		return "", "", false
	}
	switch runtime.GOARCH {
	case "amd64":
		arch = "x64"
	case "arm64":
		arch = "aarch64"
	default:
		return "", "", false
	}
	return os, arch, true
}

// makeFakeGraalTarball builds a GraalVM-shaped tarball and returns its path
// and sha256.
func makeFakeGraalTarball(t *testing.T, version string) (string, string) {
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

// graalAPIServer serves the GitHub releases list for one jdk-* release
// (platform of this machine) and the tarball/sha256 downloads.
func graalAPIServer(t *testing.T, version, archive, sum string) *httptest.Server {
	t.Helper()
	osN, archN, ok := graalPlatform()
	if !ok {
		t.Skipf("no platform mapping for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	st, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	var relJSON []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/graalvm/graalvm-ce-builds/releases", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(relJSON)
	})
	mux.HandleFunc("/graalvm/graalvm-ce-builds/releases/download/jdk-"+version+"/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/graalvm/graalvm-ce-builds/releases/download/jdk-"+version+"/")
		want := "graalvm-community-jdk-" + version + "_" + osN + "-" + archN + "_bin"
		switch name {
		case want + ".tar.gz", want + ".zip":
			http.ServeFile(w, r, archive)
		case want + ".tar.gz.sha256", want + ".zip.sha256":
			w.Write([]byte(sum))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	mux.HandleFunc("/dl", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, archive)
	})
	mux.HandleFunc("/dl.sha256", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sum))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	want := "graalvm-community-jdk-" + version + "_" + osN + "-" + archN + "_bin"
	rels := []map[string]any{{
		"tag_name": "jdk-" + version,
		"assets": []map[string]any{
			{"name": want + ".tar.gz", "browser_download_url": srv.URL + "/dl", "size": st.Size()},
			{"name": want + ".tar.gz.sha256", "browser_download_url": srv.URL + "/dl.sha256"},
		},
	}}
	relJSON, err = json.Marshal(rels)
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestGraalVMInstallListUninstall(t *testing.T) {
	archive, sum := makeFakeGraalTarball(t, "25.0.2")
	srv := graalAPIServer(t, "25.0.2", archive, sum)
	oldBase := graal.DefaultBase
	graal.DefaultBase = srv.URL
	t.Cleanup(func() { graal.DefaultBase = oldBase })

	cacheDir := t.TempDir()
	code, out := runCLI(t, "graalvm", "install", "25", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("install exit = %d; out: %s", code, out)
	}
	if !strings.Contains(out, "installed graalvm 25.0.2") {
		t.Errorf("install out = %q", out)
	}

	// One GraalVM per major: reinstalling the same major does nothing.
	code, out = runCLI(t, "graalvm", "install", "25", "--cache-dir", cacheDir)
	if code != 0 || !strings.Contains(out, "already installed") {
		t.Errorf("second install exit = %d; out: %s", code, out)
	}

	code, out = runCLI(t, "graalvm", "list", "--cache-dir", cacheDir)
	if code != 0 || !strings.Contains(out, "graalvm 25.0.2") {
		t.Errorf("list exit = %d; out: %s", code, out)
	}

	code, out = runCLI(t, "graalvm", "uninstall", "25", "--cache-dir", cacheDir)
	if code != 0 || !strings.Contains(out, "uninstalled graalvm 25.0.2") {
		t.Errorf("uninstall exit = %d; out: %s", code, out)
	}
	code, out = runCLI(t, "graalvm", "list", "--cache-dir", cacheDir)
	if code != 0 || !strings.Contains(out, "(none)") {
		t.Errorf("list after uninstall exit = %d; out: %s", code, out)
	}
}

func TestGraalVMInstallOffline(t *testing.T) {
	code, out := runCLI(t, "graalvm", "install", "21", "--offline", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Errorf("exit = %d, want 1; out: %s", code, out)
	}
	if !strings.Contains(out, "offline") {
		t.Errorf("out = %q", out)
	}
}

// TestGraalVMPath covers the script-facing lookup: the bare GRAALVM_HOME of
// the newest install satisfying the major on stdout; exit 2 with the
// install hint when nothing matches.
func TestGraalVMPath(t *testing.T) {
	cacheDir := t.TempDir()
	fakeGraalVM(t, cacheDir, "21.0.2")
	code, out := runCLI(t, "graalvm", "path", "21", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("path exit = %d; out: %s", code, out)
	}
	if want := filepath.Join(cacheDir, "graal", "graalvm-21.0.2", "graal"); strings.TrimSpace(out) != want {
		t.Errorf("path out = %q, want %q", out, want)
	}

	// Nothing installed for the major: exit 2 with the install hint.
	code, out = runCLI(t, "graalvm", "path", "17", "--cache-dir", cacheDir)
	if code != 2 {
		t.Errorf("path exit = %d, want 2; out: %s", code, out)
	}
	if !strings.Contains(out, "rig graalvm install 17") {
		t.Errorf("out = %q", out)
	}

	// Patch versions are not requests.
	code, out = runCLI(t, "graalvm", "path", "21.0.2", "--cache-dir", cacheDir)
	if code != 2 || !strings.Contains(out, "bad version") {
		t.Errorf("path exit = %d, want 2; out: %s", code, out)
	}
}

func TestGraalVMUpdate(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", "{:rig/lib x/y}\n")
	doc := lockfile.ForTest(".")
	doc.GraalVM = &lockfile.GraalVM{Vendor: "graalvm", Requested: "21"}
	doc.FreshFor(map[string]string{".": "{:rig/lib x/y}\n"})
	if err := doc.Save("deps.lock"); err != nil {
		t.Fatal(err)
	}

	archive, sum := makeFakeGraalTarball(t, "21.0.3")
	srv := graalAPIServer(t, "21.0.3", archive, sum)
	oldBase := graal.DefaultBase
	graal.DefaultBase = srv.URL
	t.Cleanup(func() { graal.DefaultBase = oldBase })

	// No GraalVM for the major installed: update installs the newest build.
	cacheDir := t.TempDir()
	code, out := runCLI(t, "graalvm", "update", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("update exit = %d; out: %s", code, out)
	}
	if !strings.Contains(out, "installed graalvm 21.0.3") {
		t.Errorf("update out = %q", out)
	}

	// Second update: the installed build is already the newest.
	code, out = runCLI(t, "graalvm", "update", "--cache-dir", cacheDir)
	if code != 0 || !strings.Contains(out, "up to date") {
		t.Errorf("second update exit = %d; out: %s", code, out)
	}

	// An older build installed: update replaces it (one GraalVM per major).
	oldCache := t.TempDir()
	fakeGraalVM(t, oldCache, "21.0.2")
	code, out = runCLI(t, "graalvm", "update", "--cache-dir", oldCache)
	if code != 0 {
		t.Fatalf("update exit = %d; out: %s", code, out)
	}
	if !strings.Contains(out, "graalvm updated: 21.0.2 → 21.0.3") {
		t.Errorf("update out = %q", out)
	}
	if _, err := os.Stat(filepath.Join(oldCache, "graal", "graalvm-21.0.2")); !os.IsNotExist(err) {
		t.Errorf("old install should be removed: %v", err)
	}

	// Update never writes the lock.
	d2, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	if d2.GraalVM == nil || d2.GraalVM.Requested != "21" {
		t.Errorf("lock graalvm = %+v", d2.GraalVM)
	}
}

// fakeGraalVM plants a GraalVM install in the store at cacheDir.
func fakeGraalVM(t *testing.T, cacheDir, version string) string {
	t.Helper()
	st := graal.NewStoreAt(cacheDir)
	dir := st.Dir(version)
	if err := os.MkdirAll(filepath.Join(dir, "graal", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"java", "native-image"} {
		if err := os.WriteFile(filepath.Join(dir, "graal", "bin", b), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m := map[string]string{
		"vendor": "graalvm", "version": version, "os": "linux", "arch": "x64",
		"archive": "a.tar.gz", "url": "u", "sha256": strings.Repeat("0", 64),
		"installed_at": "2026-01-01T00:00:00Z",
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rig-graal.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestGraalVMUpdateNoPin(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", "{:rig/lib x/y}\n")
	doc := lockfile.ForTest(".")
	doc.FreshFor(map[string]string{".": "{:rig/lib x/y}\n"})
	if err := doc.Save("deps.lock"); err != nil {
		t.Fatal(err)
	}
	code, out := runCLI(t, "graalvm", "update", "--cache-dir", t.TempDir())
	if code != 2 {
		t.Errorf("exit = %d, want 2; out: %s", code, out)
	}
	if !strings.Contains(out, "no GraalVM in the lock") {
		t.Errorf("out = %q", out)
	}
}
