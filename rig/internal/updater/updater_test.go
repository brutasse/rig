package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func shaOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestParseTag(t *testing.T) {
	for tag, want := range map[string]string{
		"v0.1.0":  "0.1.0",
		"v10.2.3": "10.2.3",
	} {
		got, err := ParseTag(tag)
		if err != nil || got != want {
			t.Errorf("ParseTag(%q) = %q, %v; want %q", tag, got, err, want)
		}
	}
	for _, bad := range []string{"0.1.0", "v1", "v1.2", "v1.2.3-rc1", "latest"} {
		if _, err := ParseTag(bad); err == nil {
			t.Errorf("ParseTag(%q) succeeded, want error", bad)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	for _, c := range [][3]string{
		{"0.1.0", "0.1.0", "0"},
		{"0.1.0", "0.2.0", "-1"},
		{"0.2.0", "0.1.9", "1"},
		{"1.0.0", "0.9.9", "1"},
		{"1.10.0", "1.9.9", "1"},
		{"v1.2.3", "1.2.3", "0"},
	} {
		want, _ := strconv.Atoi(c[2])
		if got := CompareVersions(c[0], c[1]); got != want {
			t.Errorf("CompareVersions(%s, %s) = %d, want %d", c[0], c[1], got, want)
		}
	}
}

// fakeAPI serves /releases/latest and /releases/tags/<tag> plus the release
// assets from one server.
func fakeAPI(t *testing.T, tag string, binaryContent string) (*httptest.Server, *Release) {
	t.Helper()
	binName := BinaryName(runtime.GOOS, runtime.GOARCH)
	sums := shaOf(binaryContent) + "  " + binName + "\n"
	rel := Release{Tag: tag, Assets: []Asset{
		{Name: "SHA256SUMS", URL: ""},
		{Name: binName, URL: ""},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest", "/releases/tags/" + tag:
			fmt.Fprintf(w, `{"tag_name":%q,"assets":[
				{"name":"SHA256SUMS","browser_download_url":"%s/sums"},
				{"name":%q,"browser_download_url":"%s/bin"}]}`, tag, srvURL(r), binName, srvURL(r))
		case "/sums":
			fmt.Fprint(w, sums)
		case "/bin":
			fmt.Fprint(w, binaryContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	for i := range rel.Assets {
		rel.Assets[i].URL = srv.URL + []string{"", "/sums", "/bin"}[i]
	}
	return srv, &rel
}

// srvURL derives the request's origin (the test server URL) from the request.
func srvURL(r *http.Request) string {
	return "http://" + r.Host
}

func TestLatestAndInstall(t *testing.T) {
	const content = "new-rig-binary-bytes"
	srv, want := fakeAPI(t, "v0.2.0", content)
	got, err := Latest(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tag != want.Tag || len(got.Assets) != len(want.Assets) {
		t.Fatalf("release = %+v, want %+v", got, want)
	}
	if _, ok := got.AssetOf("SHA256SUMS"); !ok {
		t.Fatal("SHA256SUMS asset missing")
	}
	dir := t.TempDir()
	path, err := InstallBinary(context.Background(), srv.Client(), got, runtime.GOOS, runtime.GOARCH, dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != content {
		t.Errorf("binary content = %q", b)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&0o111 == 0 {
		t.Errorf("binary not executable: %v", st.Mode())
	}
}

func TestInstallMismatch(t *testing.T) {
	// Serve a SHA256SUMS that does not match the binary bytes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			fmt.Fprintf(w, `{"tag_name":"v0.2.0","assets":[
				{"name":"SHA256SUMS","browser_download_url":"%s/sums"},
				{"name":%q,"browser_download_url":"%s/bin"}]}`,
				srvURL(r), BinaryName(runtime.GOOS, runtime.GOARCH), srvURL(r))
		case "/sums":
			fmt.Fprint(w, strings.Repeat("0", 64)+"  "+BinaryName(runtime.GOOS, runtime.GOARCH)+"\n")
		case "/bin":
			fmt.Fprint(w, "bytes")
		}
	}))
	t.Cleanup(srv.Close)
	rel, err := Latest(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = InstallBinary(context.Background(), srv.Client(), rel, runtime.GOOS, runtime.GOARCH, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("err = %v, want sha256 mismatch", err)
	}
}

func TestInstallMissingSums(t *testing.T) {
	rel := &Release{Tag: "v0.2.0"}
	_, err := InstallBinary(context.Background(), nil, rel, runtime.GOOS, runtime.GOARCH, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "no SHA256SUMS") {
		t.Fatalf("err = %v, want missing SHA256SUMS", err)
	}
}

func TestGetUnknownTag(t *testing.T) {
	srv, _ := fakeAPI(t, "v0.2.0", "x")
	if _, err := Get(context.Background(), srv.Client(), srv.URL, "not-a-tag"); err == nil {
		t.Error("Get(bad tag) succeeded, want error")
	}
}
