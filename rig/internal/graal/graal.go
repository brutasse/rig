// Package graal manages rig-managed GraalVM JDKs: community builds resolved
// through the graalvm/graalvm-ce-builds GitHub releases, sha256-verified,
// stored under the rig state dir. Version semantics are Java's, shared with
// the jdk package.
package graal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/brutasse/rig/internal/jdk"
)

// Vendor is the GraalVM vendor rig manages.
const Vendor = "graalvm"

// DefaultBase is the GitHub API used to resolve GraalVM community builds.
var DefaultBase = "https://api.github.com"

// repo is the GraalVM community build releases rig installs from. Only
// `jdk-*` tagged releases are considered: the "Innovation" releases
// (graal-* tags) version their assets independently of the tag and are out
// of scope.
const repo = "graalvm/graalvm-ce-builds"

// ErrNotInstalled means no GraalVM matching the request is in the store.
var ErrNotInstalled = errors.New("graal: not installed")

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

type apiClient struct {
	base string
	http *http.Client
}

// NewAPI returns the GitHub releases API client; base "" uses DefaultBase.
func NewAPI(base string) *apiClient {
	if base == "" {
		base = DefaultBase
	}
	return &apiClient{base: base, http: &http.Client{Timeout: 2 * time.Minute}}
}

// Asset is one downloadable GraalVM build.
type Asset struct {
	Vendor  string
	Version string // exact version, e.g. "25.0.2"
	OS      string
	Arch    string
	Archive string
	URL     string
	SHA256  string
	Size    int64
}

type apiRelease struct {
	TagName string     `json:"tag_name"`
	Assets  []apiAsset `json:"assets"`
}

type apiAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

func platform() (os, arch string, ok bool) {
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

// Resolve returns the newest GraalVM community release (a `jdk-*` tagged
// release, newest first) satisfying requested that ships a build for this
// platform.
func (a *apiClient) Resolve(ctx context.Context, requested string) (Asset, error) {
	os, arch, ok := platform()
	if !ok {
		return Asset{}, fmt.Errorf("graal: no graalvm build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	rels, err := a.releases(ctx)
	if err != nil {
		return Asset{}, err
	}
	for _, rel := range rels { // newest first
		if !strings.HasPrefix(rel.TagName, "jdk-") {
			continue
		}
		v := strings.TrimPrefix(rel.TagName, "jdk-")
		if !jdk.Satisfies(requested, v) {
			continue
		}
		want := fmt.Sprintf("graalvm-community-jdk-%s_%s-%s_bin", v, os, arch)
		var archiveName, archiveURL, shaURL string
		var size int64
		for _, as := range rel.Assets {
			switch as.Name {
			case want + ".tar.gz", want + ".zip":
				archiveName, archiveURL, size = as.Name, as.BrowserDownloadURL, as.Size
			case want + ".tar.gz.sha256", want + ".zip.sha256":
				shaURL = as.BrowserDownloadURL
			}
		}
		if archiveURL == "" || shaURL == "" {
			continue
		}
		sum, err := a.sha256(ctx, shaURL)
		if err != nil {
			return Asset{}, err
		}
		return Asset{
			Vendor:  Vendor,
			Version: v,
			OS:      os,
			Arch:    arch,
			Archive: archiveName,
			URL:     archiveURL,
			SHA256:  sum,
			Size:    size,
		}, nil
	}
	return Asset{}, fmt.Errorf("graal: no graalvm %s/%s build satisfies %q", os, arch, requested)
}

func (a *apiClient) releases(ctx context.Context) ([]apiRelease, error) {
	url := a.base + "/repos/" + repo + "/releases?per_page=100"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("graal: %s: status %d", url, resp.StatusCode)
	}
	var rels []apiRelease
	if err := json.NewDecoder(resp.Body).Decode(&rels); err != nil {
		return nil, fmt.Errorf("graal: bad GitHub API response: %w", err)
	}
	return rels, nil
}

// sha256 fetches a .sha256 sidecar asset and returns its hex digest.
func (a *apiClient) sha256(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("graal: %s: status %d", url, resp.StatusCode)
	}
	var body strings.Builder
	if _, err := io.Copy(&body, io.LimitReader(resp.Body, 256)); err != nil {
		return "", err
	}
	sum := strings.TrimSpace(body.String())
	if !sha256Re.MatchString(sum) {
		return "", fmt.Errorf("graal: bad sha256 sidecar at %s: %q", url, sum)
	}
	return sum, nil
}

// marker is the on-disk record beside an installed GraalVM.
type marker struct {
	Vendor      string `json:"vendor"`
	Version     string `json:"version"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	Archive     string `json:"archive"`
	URL         string `json:"url"`
	SHA256      string `json:"sha256"`
	InstalledAt string `json:"installed_at"`
}

// Inst describes an installed GraalVM.
type Inst struct {
	Vendor          string
	Version         string
	OS              string
	Arch            string
	Archive         string
	URL             string
	SHA256          string
	InstalledAt     string
	Dir             string
	Home            string // GRAALVM_HOME value
	JavaPath        string
	NativeImagePath string
}

// Store holds installed GraalVMs under <root>/graal.
type Store struct{ Root string }

// NewStoreAt returns the store rooted at the rig state dir.
func NewStoreAt(root string) *Store { return &Store{Root: filepath.Join(root, "graal")} }

func (s *Store) Dir(version string) string {
	return filepath.Join(s.Root, Vendor+"-"+version)
}

func binName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// Lookup returns the install of the exact version, or ErrNotInstalled.
func (s *Store) Lookup(version string) (*Inst, error) {
	b, err := os.ReadFile(filepath.Join(s.Dir(version), "rig-graal.json"))
	if err != nil {
		return nil, ErrNotInstalled
	}
	var m marker
	if err := json.Unmarshal(b, &m); err != nil || m.Version != version {
		return nil, ErrNotInstalled
	}
	home := filepath.Join(s.Dir(version), "graal")
	native := filepath.Join(home, "bin", binName("native-image"))
	if st, err := os.Stat(native); err != nil || st.IsDir() {
		return nil, fmt.Errorf("graal: %s: corrupt install (missing bin/native-image)", s.Dir(version))
	}
	return &Inst{
		Vendor: m.Vendor, Version: m.Version, OS: m.OS, Arch: m.Arch,
		Archive: m.Archive, URL: m.URL, SHA256: m.SHA256, InstalledAt: m.InstalledAt,
		Dir: s.Dir(version), Home: home,
		JavaPath:        filepath.Join(home, "bin", binName("java")),
		NativeImagePath: native,
	}, nil
}

// Best returns the newest installed GraalVM satisfying requested, or
// ErrNotInstalled.
func (s *Store) Best(requested string) (*Inst, error) {
	insts, err := s.List()
	if err != nil {
		return nil, err
	}
	var best *Inst
	for i := range insts {
		if !jdk.Satisfies(requested, insts[i].Version) {
			continue
		}
		if best == nil || jdk.Compare(insts[i].Version, best.Version) > 0 {
			best = &insts[i]
		}
	}
	if best == nil {
		return nil, ErrNotInstalled
	}
	return best, nil
}

// List returns all installed GraalVMs, newest first.
func (s *Store) List() ([]Inst, error) {
	entries, err := os.ReadDir(s.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Inst
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), Vendor+"-") {
			continue
		}
		if i, err := s.Lookup(strings.TrimPrefix(e.Name(), Vendor+"-")); err == nil {
			out = append(out, *i)
		}
	}
	sort.Slice(out, func(i, j int) bool { return jdk.Compare(out[i].Version, out[j].Version) > 0 })
	return out, nil
}

// Ensure returns an installed GraalVM for the pin: the exact version when
// set, else the newest installed satisfying requested. When nothing
// suitable is installed and offline is false, the needed release is
// downloaded, hash-verified and installed. A RIG_GRAALVM_HOME override is
// used as-is (trusted, like RIG_JAVA) and never touches the store.
func Ensure(ctx context.Context, st *Store, requested, version string, offline bool) (*Inst, error) {
	if home := os.Getenv("RIG_GRAALVM_HOME"); home != "" {
		native := filepath.Join(home, "bin", binName("native-image"))
		if st, err := os.Stat(native); err != nil || st.IsDir() {
			return nil, fmt.Errorf("graal: RIG_GRAALVM_HOME %s: no bin/native-image", home)
		}
		return &Inst{
			Vendor:          Vendor,
			Version:         firstNonEmpty(version, requested),
			Dir:             home,
			Home:            home,
			JavaPath:        filepath.Join(home, "bin", binName("java")),
			NativeImagePath: native,
		}, nil
	}
	switch {
	case version != "":
		if inst, err := st.Lookup(version); err == nil {
			return inst, nil
		}
	case !offline:
		if inst, err := st.Best(requested); err == nil {
			return inst, nil
		}
	}
	if offline {
		want := requested
		if version != "" {
			want = version
		}
		return nil, fmt.Errorf("offline: graalvm %s not installed (run 'rig build --native' online once to install it)", want)
	}
	a, err := NewAPI("").Resolve(ctx, firstNonEmpty(version, requested))
	if err != nil {
		return nil, err
	}
	fmt.Printf("installing %s %s (%d MB)…\n", a.Vendor, a.Version, a.Size/1024/1024)
	inst, err := st.Install(ctx, a)
	if err != nil {
		return nil, err
	}
	fmt.Printf("installed %s %s\n", inst.Vendor, inst.Version)
	return inst, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Install downloads a, verifies its sha256, and extracts it under the store.
// Concurrent installs of the same version are serialized with a marker file.
func (s *Store) Install(ctx context.Context, a Asset) (*Inst, error) {
	if inst, err := s.Lookup(a.Version); err == nil {
		return inst, nil
	}
	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(s.Root, ".install-"+a.Version+".lock")
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err == nil {
		lf.Close()
		defer os.Remove(lockPath)
	} else if errors.Is(err, os.ErrExist) {
		return s.waitForInstall(ctx, a.Version)
	} else {
		return nil, err
	}
	archive, err := s.download(ctx, a)
	if err != nil {
		return nil, err
	}
	defer os.Remove(archive)
	top, err := jdk.Extract(s.Root, archive, a.Archive)
	if err != nil {
		return nil, err
	}
	dir := s.Dir(a.Version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	home := filepath.Join(dir, "graal")
	if err := os.RemoveAll(home); err != nil {
		return nil, err
	}
	if err := os.Rename(top, home); err != nil {
		return nil, err
	}
	m := marker{
		Vendor: Vendor, Version: a.Version, OS: a.OS, Arch: a.Arch,
		Archive: a.Archive, URL: a.URL, SHA256: a.SHA256,
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "rig-graal.json"), b, 0o644); err != nil {
		return nil, err
	}
	return s.Lookup(a.Version)
}

func (s *Store) waitForInstall(ctx context.Context, version string) (*Inst, error) {
	deadline := time.Now().Add(10 * time.Minute)
	for {
		if inst, err := s.Lookup(version); err == nil {
			return inst, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("graal: timed out waiting for a concurrent install of %s", version)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *Store) download(ctx context.Context, a Asset) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{} // no timeout: the context governs the big download
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("graal: %s: status %d", a.URL, resp.StatusCode)
	}
	tmp := filepath.Join(s.Root, ".dl-"+a.Version)
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", fmt.Errorf("graal: download %s: %w", a.Version, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != a.SHA256 {
		os.Remove(tmp)
		return "", fmt.Errorf("graal: %s: sha256 mismatch (got %s)", a.Version, got)
	}
	return tmp, nil
}
