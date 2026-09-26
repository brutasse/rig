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
	code, out = runCLI(t, "graalvm", "install", "25.0.2", "--cache-dir", cacheDir)
	if code != 0 || !strings.Contains(out, "already installed") {
		t.Errorf("second install exit = %d; out: %s", code, out)
	}

	code, out = runCLI(t, "graalvm", "list", "--cache-dir", cacheDir)
	if code != 0 || !strings.Contains(out, "graalvm 25.0.2") {
		t.Errorf("list exit = %d; out: %s", code, out)
	}

	code, out = runCLI(t, "graalvm", "uninstall", "25.0.2", "--cache-dir", cacheDir)
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

func TestGraalVMUpdate(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", "{:rig/lib x/y}\n")
	doc := lockfile.ForTest(".")
	doc.GraalVM = &lockfile.GraalVM{Vendor: "graalvm", Requested: "21", Version: "21.0.2"}
	doc.FreshFor(map[string]string{".": "{:rig/lib x/y}\n"})
	if err := doc.Save("deps.lock"); err != nil {
		t.Fatal(err)
	}

	archive, sum := makeFakeGraalTarball(t, "21.0.3")
	srv := graalAPIServer(t, "21.0.3", archive, sum)
	oldBase := graal.DefaultBase
	graal.DefaultBase = srv.URL
	t.Cleanup(func() { graal.DefaultBase = oldBase })

	code, out := runCLI(t, "graalvm", "update", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("update exit = %d; out: %s", code, out)
	}
	if !strings.Contains(out, "21.0.3") {
		t.Errorf("update out = %q", out)
	}
	d2, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	if d2.GraalVM == nil || d2.GraalVM.Version != "21.0.3" {
		t.Errorf("lock graalvm = %+v", d2.GraalVM)
	}

	code, out = runCLI(t, "graalvm", "update", "--cache-dir", t.TempDir())
	if code != 0 || !strings.Contains(out, "up to date") {
		t.Errorf("second update exit = %d; out: %s", code, out)
	}
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
