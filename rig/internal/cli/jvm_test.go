package cli

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brutasse/rig/internal/digest"
	"github.com/brutasse/rig/internal/jdk"
	"github.com/brutasse/rig/internal/jvm"
	"github.com/brutasse/rig/internal/kernel"
	"github.com/brutasse/rig/internal/lockfile"
)

// makeFakeJDKTarball builds a JDK-shaped tarball and returns its path and sha256.
func makeFakeJDKTarball(t *testing.T) (string, string) {
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
	addDir("jdk-21.0.10+7")
	addDir("jdk-21.0.10+7/bin")
	addFile("jdk-21.0.10+7/bin/java", []byte("#!/bin/sh\nexit 0\n"), 0o755)
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

// jdkAPIServer serves the Adoptium feature_releases endpoint for one release
// (every platform) and the tarball download.
func jdkAPIServer(t *testing.T, version, archive, sum string) *httptest.Server {
	t.Helper()
	type fixtureBinary struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		ImageType    string `json:"image_type"`
		HeapSize     string `json:"heap_size"`
		Package      struct {
			Name     string `json:"name"`
			Link     string `json:"link"`
			Checksum string `json:"checksum"`
			Size     int64  `json:"size"`
		} `json:"package"`
	}
	var relJSON []byte
	var link string
	mux := http.NewServeMux()
	mux.HandleFunc("/v3/assets/feature_releases/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(relJSON)
	})
	mux.HandleFunc("/dl", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, archive)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	link = srv.URL + "/dl"

	var bins []fixtureBinary
	for _, p := range []struct{ os, arch string }{
		{"linux", "x64"}, {"linux", "aarch64"},
		{"mac", "x64"}, {"mac", "aarch64"},
		{"windows", "x64"}, {"windows", "aarch64"},
	} {
		var b fixtureBinary
		b.OS, b.Architecture, b.ImageType, b.HeapSize = p.os, p.arch, "jdk", "normal"
		b.Package.Name = "OpenJDK.tar.gz"
		b.Package.Link = link
		b.Package.Checksum = sum
		b.Package.Size = 10
		bins = append(bins, b)
	}
	rels := []map[string]any{{
		"release_name": "jdk-" + version,
		"binaries":     bins,
	}}
	relJSON, err := json.Marshal(rels)
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func fakeJDK(t *testing.T, cacheDir, version string) string {
	t.Helper()
	dir := filepath.Join(cacheDir, "jdks", "temurin-"+version)
	if err := os.MkdirAll(filepath.Join(dir, "jdk", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "jdk", "bin", "java"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := map[string]string{
		"vendor": "temurin", "version": version, "os": "linux", "arch": "x64",
		"archive": "a.tar.gz", "url": "u", "sha256": strings.Repeat("0", 64),
		"installed_at": "2026-01-01T00:00:00Z",
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rig-jdk.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestJVMInstallListUninstall(t *testing.T) {
	archive, sum := makeFakeJDKTarball(t)
	srv := jdkAPIServer(t, "21.0.10+7", archive, sum)
	oldBase := jdk.DefaultBase
	jdk.DefaultBase = srv.URL
	t.Cleanup(func() { jdk.DefaultBase = oldBase })

	cacheDir := t.TempDir()
	code, out := runCLI(t, "jvm", "install", "21", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("install exit = %d; out: %s", code, out)
	}
	if !strings.Contains(out, "installed temurin 21.0.10+7") {
		t.Errorf("install out = %q", out)
	}

	code, out = runCLI(t, "jvm", "list", "--cache-dir", cacheDir)
	if code != 0 || !strings.Contains(out, "temurin 21.0.10+7") {
		t.Errorf("list exit = %d; out: %s", code, out)
	}

	code, out = runCLI(t, "jvm", "uninstall", "21.0.10+7", "--cache-dir", cacheDir)
	if code != 0 || !strings.Contains(out, "uninstalled temurin 21.0.10+7") {
		t.Errorf("uninstall exit = %d; out: %s", code, out)
	}
	code, out = runCLI(t, "jvm", "list", "--cache-dir", cacheDir)
	if code != 0 || !strings.Contains(out, "(none)") {
		t.Errorf("list after uninstall exit = %d; out: %s", code, out)
	}
}

func TestJVMInstallOffline(t *testing.T) {
	code, out := runCLI(t, "jvm", "install", "21", "--offline", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Errorf("exit = %d, want 1; out: %s", code, out)
	}
	if !strings.Contains(out, "offline") {
		t.Errorf("out = %q", out)
	}
}

func TestJVMUpdate(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", "{:rig/lib x/y}\n")
	doc := lockfile.ForTest(".")
	doc.JVM = &lockfile.JVM{Vendor: "temurin", Requested: "21", Version: "21.0.10+7"}
	doc.FreshFor(map[string]string{".": "{:rig/lib x/y}\n"})
	if err := doc.Save("deps.lock"); err != nil {
		t.Fatal(err)
	}

	archive, sum := makeFakeJDKTarball(t)
	srv := jdkAPIServer(t, "21.0.11+9", archive, sum)
	oldBase := jdk.DefaultBase
	jdk.DefaultBase = srv.URL
	t.Cleanup(func() { jdk.DefaultBase = oldBase })

	code, out := runCLI(t, "jvm", "update", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("update exit = %d; out: %s", code, out)
	}
	if !strings.Contains(out, "21.0.11+9") {
		t.Errorf("update out = %q", out)
	}
	d2, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	if d2.JVM == nil || d2.JVM.Version != "21.0.11+9" {
		t.Errorf("lock jvm = %+v", d2.JVM)
	}

	code, out = runCLI(t, "jvm", "update", "--cache-dir", t.TempDir())
	if code != 0 || !strings.Contains(out, "up to date") {
		t.Errorf("second update exit = %d; out: %s", code, out)
	}
}

func TestJVMUpdateNoPin(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", "{:rig/lib x/y}\n")
	doc := lockfile.ForTest(".")
	doc.FreshFor(map[string]string{".": "{:rig/lib x/y}\n"})
	if err := doc.Save("deps.lock"); err != nil {
		t.Fatal(err)
	}
	code, out := runCLI(t, "jvm", "update", "--cache-dir", t.TempDir())
	if code != 2 {
		t.Errorf("exit = %d, want 2; out: %s", code, out)
	}
	if !strings.Contains(out, "no :rig/jvm pin") {
		t.Errorf("out = %q", out)
	}
}

func TestInfoPinnedJVM(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", "{:rig/lib x/y}\n")
	doc := lockfile.ForTest(".")
	doc.JVM = &lockfile.JVM{Vendor: "temurin", Requested: "21", Version: "21.0.10+7"}
	doc.FreshFor(map[string]string{".": "{:rig/lib x/y}\n"})
	if err := doc.Save("deps.lock"); err != nil {
		t.Fatal(err)
	}
	cacheDir := t.TempDir()

	code, out := runCLI(t, "info", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("info exit = %d; out: %s", code, out)
	}
	if !strings.Contains(out, "not installed") {
		t.Errorf("info out = %q", out)
	}

	fakeJDK(t, cacheDir, "21.0.10+7")
	code, out = runCLI(t, "info", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("info exit = %d; out: %s", code, out)
	}
	if !strings.Contains(out, `pinned "21"`) {
		t.Errorf("info out = %q", out)
	}
}

func TestInfoJAVAOverride(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", "{:rig/lib x/y}\n")
	doc := lockfile.ForTest(".")
	doc.FreshFor(map[string]string{".": "{:rig/lib x/y}\n"})
	if err := doc.Save("deps.lock"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIG_JAVA", "/usr/bin/true")
	code, out := runCLI(t, "info", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("info exit = %d; out: %s", code, out)
	}
	if !strings.Contains(out, "/usr/bin/true (RIG_JAVA)") {
		t.Errorf("info out = %q", out)
	}
}

// TestHotRunPinnedJVM pins the fixture workspace to a managed JDK (installed
// as a wrapper around the real java) and checks the hot path selects it and
// exports JAVA_HOME.
func TestHotRunPinnedJVM(t *testing.T) {
	hotSetup(t)
	realJava, err := jvm.Find()
	if err != nil {
		t.Skipf("no java available: %v", err)
	}
	doc, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	doc.JVM = &lockfile.JVM{Vendor: "temurin", Requested: "21", Version: "21.0.10+7"}
	if err := doc.Save("deps.lock"); err != nil {
		t.Fatal(err)
	}
	cacheDir := t.TempDir()
	jdkDir := fakeJDK(t, cacheDir, "21.0.10+7")
	marker := filepath.Join(t.TempDir(), "java-home")
	wrapper := filepath.Join(jdkDir, "jdk", "bin", "java")
	script := "#!/bin/sh\necho \"$JAVA_HOME\" > " + marker + "\nexec " + realJava + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	code, out := runCLI(t, "run", "--cache-dir", cacheDir, "-p", "modules/app", "world")
	if code != 0 {
		t.Fatalf("run exit = %d; out: %s", code, out)
	}
	if !strings.Contains(out, "hello world") {
		t.Errorf("run out = %q", out)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("managed JDK was not used (JAVA_HOME marker missing); out: %s", out)
	}
	if want := filepath.Join(jdkDir, "jdk"); strings.TrimSpace(string(got)) != want {
		t.Errorf("JAVA_HOME = %q, want %q", got, want)
	}
}

// ltsAPIServer serves the Adoptium info/available_releases endpoint
// (code != 200 makes it fail).
func ltsAPIServer(t *testing.T, lts, code int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"most_recent_lts": %d}`, lts)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestLockNoJavaHint: a machine with no java on PATH and no JAVA_HOME must
// fail (rig never installs on the launch path), but the error must name the
// install command with the current LTS.
func TestLockNoJavaHint(t *testing.T) {
	jar := kernelJarPath(t) // skips when the kernel jar is absent
	sha, err := digest.File(jar)
	if err != nil {
		t.Fatal(err)
	}
	oldSHA := kernel.Current.JARSHA
	kernel.Current.JARSHA = sha
	t.Cleanup(func() { kernel.Current.JARSHA = oldSHA })
	t.Setenv("RIG_KERNEL_JAR", jar)
	t.Setenv("RIG_JAVA", "")
	t.Setenv("JAVA_HOME", "")
	t.Setenv("PATH", t.TempDir()) // empty PATH: no java on it

	lts := ltsAPIServer(t, 25, http.StatusOK)
	oldBase := jdk.DefaultBase
	jdk.DefaultBase = lts.URL
	t.Cleanup(func() { jdk.DefaultBase = oldBase })

	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", "{:rig/lib x/y}\n")
	code, out := runCLI(t, "lock", "--cache-dir", t.TempDir())
	if code == 0 {
		t.Fatalf("lock without a JVM should fail; out: %s", out)
	}
	if !strings.Contains(out, "no java found") {
		t.Errorf("out = %q", out)
	}
	if !strings.Contains(out, "rig jvm install 25") {
		t.Errorf("out = %q (want the current-LTS install hint)", out)
	}
}
