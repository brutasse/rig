// Package jdk manages rig-managed JDKs: Temurin builds resolved through the
// Adoptium API, sha256-verified, stored under the rig state dir.
package jdk

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
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
	"strconv"
	"strings"
	"time"
)

// Vendor is the only JDK vendor rig manages.
const Vendor = "temurin"

// DefaultBase is the Adoptium API used to resolve Temurin releases.
var DefaultBase = "https://api.adoptium.net"

// ErrNotInstalled means no JDK matching the request is in the store.
var ErrNotInstalled = errors.New("jdk: not installed")

var versionRe = regexp.MustCompile(`^\d{1,2}(\.\d{1,3}){0,3}(\+\d+)?$`)

type version struct {
	comps    []int
	build    int
	hasBuild bool
}

func parseVersion(s string) (version, error) {
	if !versionRe.MatchString(s) {
		return version{}, fmt.Errorf("jdk: bad version %q (want e.g. \"21\" or \"21.0.10+7\")", s)
	}
	v := version{}
	base := s
	if i := strings.IndexByte(s, '+'); i >= 0 {
		v.hasBuild = true
		v.build, _ = strconv.Atoi(s[i+1:])
		base = s[:i]
	}
	for _, c := range strings.Split(base, ".") {
		n, _ := strconv.Atoi(c)
		v.comps = append(v.comps, n)
	}
	return v, nil
}

// ValidRequested reports whether s is a usable JVM version request.
func ValidRequested(s string) bool {
	_, err := parseVersion(s)
	return err == nil
}

// FeatureVersion is the JVM feature version of a version string:
// "21.0.12" → 21, "1.8.0_422" → 8. 0 when unparseable.
func FeatureVersion(v string) int {
	parts := strings.SplitN(v, ".", 3)
	if parts[0] == "1" && len(parts) > 1 {
		parts = parts[1:]
	}
	n, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0
	}
	return n
}

// IsExact reports whether s fully specifies a release version (three or
// more components, no build number): such a request maps to exactly one
// release, so it can be served without looking the release list up.
func IsExact(s string) bool {
	v, err := parseVersion(s)
	return err == nil && !v.hasBuild && len(v.comps) >= 3
}

// Satisfies reports whether candidate fulfills requested: every component
// requested specifies must equal the candidate's; an unspecified requested
// component matches anything, including an absent one.
func Satisfies(requested, candidate string) bool {
	r, err := parseVersion(requested)
	if err != nil {
		return false
	}
	c, err := parseVersion(candidate)
	if err != nil {
		return false
	}
	for i, want := range r.comps {
		got := 0
		if i < len(c.comps) {
			got = c.comps[i]
		}
		if got != want {
			return false
		}
	}
	if r.hasBuild && (!c.hasBuild || c.build != r.build) {
		return false
	}
	return true
}

// Compare orders versions (negative: a is older). Components compare
// positionally (absent = 0), then the build number.
func Compare(a, b string) int {
	av, _ := parseVersion(a)
	bv, _ := parseVersion(b)
	n := len(av.comps)
	if len(bv.comps) > n {
		n = len(bv.comps)
	}
	for i := 0; i < n; i++ {
		x, y := 0, 0
		if i < len(av.comps) {
			x = av.comps[i]
		}
		if i < len(bv.comps) {
			y = bv.comps[i]
		}
		if x != y {
			return x - y
		}
	}
	return av.build - bv.build
}

func platform() (os, arch string, ok bool) {
	switch runtime.GOOS {
	case "linux":
		os = "linux"
	case "darwin":
		os = "mac"
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

type apiClient struct {
	base string
	http *http.Client
}

// NewAPI returns the Adoptium API client; base "" uses DefaultBase.
func NewAPI(base string) *apiClient {
	if base == "" {
		base = DefaultBase
	}
	return &apiClient{base: base, http: &http.Client{Timeout: 2 * time.Minute}}
}

// Asset is one downloadable JDK build.
type Asset struct {
	Vendor  string
	Version string // exact version, e.g. "21.0.10+7"
	OS      string
	Arch    string
	Archive string
	URL     string
	SHA256  string
	Size    int64
}

type apiBinary struct {
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

type apiRelease struct {
	ReleaseName string      `json:"release_name"`
	Binaries    []apiBinary `json:"binaries"`
}

// Resolve returns the newest GA Temurin release satisfying requested that
// ships a build for this platform.
func (a *apiClient) Resolve(ctx context.Context, requested string) (Asset, error) {
	os, arch, ok := platform()
	if !ok {
		return Asset{}, fmt.Errorf("jdk: no temurin build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	v, err := parseVersion(requested)
	if err != nil {
		return Asset{}, err
	}
	rels, err := a.releases(ctx, v.comps[0])
	if err != nil {
		return Asset{}, err
	}
	for _, rel := range rels { // newest first
		relVersion := strings.TrimPrefix(rel.ReleaseName, "jdk-")
		if !Satisfies(requested, relVersion) {
			continue
		}
		for _, b := range rel.Binaries {
			if b.OS != os || b.Architecture != arch || b.ImageType != "jdk" || b.HeapSize != "normal" {
				continue
			}
			return Asset{
				Vendor:  Vendor,
				Version: relVersion,
				OS:      os,
				Arch:    arch,
				Archive: b.Package.Name,
				URL:     b.Package.Link,
				SHA256:  b.Package.Checksum,
				Size:    b.Package.Size,
			}, nil
		}
	}
	return Asset{}, fmt.Errorf("jdk: no temurin %s/%s build satisfies %q", os, arch, requested)
}

// LatestLTS returns the newest LTS feature version Adoptium serves (e.g. 25),
// from the info/available_releases endpoint.
func (a *apiClient) LatestLTS(ctx context.Context) (int, error) {
	url := a.base + "/v3/info/available_releases"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("jdk: %s: status %d", url, resp.StatusCode)
	}
	var info struct {
		RecentLTS int `json:"most_recent_lts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return 0, fmt.Errorf("jdk: bad Adoptium API response: %w", err)
	}
	if info.RecentLTS <= 0 {
		return 0, fmt.Errorf("jdk: no LTS feature version reported by %s", url)
	}
	return info.RecentLTS, nil
}

func (a *apiClient) releases(ctx context.Context, major int) ([]apiRelease, error) {
	url := fmt.Sprintf("%s/v3/assets/feature_releases/%d/ga", a.base, major)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("jdk: no GA releases for feature %d", major)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jdk: %s: status %d", url, resp.StatusCode)
	}
	var rels []apiRelease
	if err := json.NewDecoder(resp.Body).Decode(&rels); err != nil {
		return nil, fmt.Errorf("jdk: bad Adoptium API response: %w", err)
	}
	return rels, nil
}

// marker is the on-disk record beside an installed JDK.
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

// Inst describes an installed JDK.
type Inst struct {
	Vendor      string
	Version     string
	OS          string
	Arch        string
	Archive     string
	URL         string
	SHA256      string
	InstalledAt string
	Dir         string
	Home        string // JAVA_HOME value
	JavaPath    string
}

// Store holds installed JDKs under <root>/jdks.
type Store struct{ Root string }

// NewStoreAt returns the store rooted at the rig state dir.
func NewStoreAt(root string) *Store { return &Store{Root: filepath.Join(root, "jdks")} }

func (s *Store) Dir(version string) string {
	return filepath.Join(s.Root, Vendor+"-"+version)
}

func javaName() string {
	if runtime.GOOS == "windows" {
		return "java.exe"
	}
	return "java"
}

// Lookup returns the install of the exact version, or ErrNotInstalled.
func (s *Store) Lookup(version string) (*Inst, error) {
	b, err := os.ReadFile(filepath.Join(s.Dir(version), "rig-jdk.json"))
	if err != nil {
		return nil, ErrNotInstalled
	}
	var m marker
	if err := json.Unmarshal(b, &m); err != nil || m.Version != version {
		return nil, ErrNotInstalled
	}
	home := filepath.Join(s.Dir(version), "jdk")
	java := filepath.Join(home, "bin", javaName())
	if st, err := os.Stat(java); err != nil || st.IsDir() {
		return nil, fmt.Errorf("jdk: %s: corrupt install (missing bin/java); run 'rig jvm install %s'", s.Dir(version), version)
	}
	return &Inst{
		Vendor: m.Vendor, Version: m.Version, OS: m.OS, Arch: m.Arch,
		Archive: m.Archive, URL: m.URL, SHA256: m.SHA256, InstalledAt: m.InstalledAt,
		Dir: s.Dir(version), Home: home, JavaPath: java,
	}, nil
}

// Best returns the newest installed JDK satisfying requested, or ErrNotInstalled.
func (s *Store) Best(requested string) (*Inst, error) {
	insts, err := s.List()
	if err != nil {
		return nil, err
	}
	var best *Inst
	for i := range insts {
		if !Satisfies(requested, insts[i].Version) {
			continue
		}
		if best == nil || Compare(insts[i].Version, best.Version) > 0 {
			best = &insts[i]
		}
	}
	if best == nil {
		return nil, ErrNotInstalled
	}
	return best, nil
}

// List returns all installed JDKs, newest first.
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
	sort.Slice(out, func(i, j int) bool { return Compare(out[i].Version, out[j].Version) > 0 })
	return out, nil
}

// Uninstall removes an installed JDK matching requested (exact version or
// unique prefix) and returns the removed version.
func (s *Store) Uninstall(requested string) (string, error) {
	insts, err := s.List()
	if err != nil {
		return "", err
	}
	var matches []string
	for _, i := range insts {
		if Satisfies(requested, i.Version) || strings.HasPrefix(i.Version, requested) {
			matches = append(matches, i.Version)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("jdk: nothing installed matching %q", requested)
	case 1:
	default:
		return "", fmt.Errorf("jdk: %q matches several installs: %s", requested, strings.Join(matches, ", "))
	}
	v := matches[0]
	if err := os.RemoveAll(s.Dir(v)); err != nil {
		return "", err
	}
	return v, nil
}

// Ensure returns an installed JDK for the pin: the exact version when set,
// else the newest installed satisfying requested. When nothing suitable is
// installed and offline is false, the needed release is downloaded,
// hash-verified and installed.
func Ensure(ctx context.Context, st *Store, requested, version string, offline bool) (*Inst, error) {
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
		return nil, fmt.Errorf("offline: temurin %s not installed (run 'rig jvm install %s' online)", want, want)
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
	top, err := Extract(s.Root, archive, a.Archive)
	if err != nil {
		return nil, err
	}
	dir := s.Dir(a.Version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	home := filepath.Join(dir, "jdk")
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
	if err := os.WriteFile(filepath.Join(dir, "rig-jdk.json"), b, 0o644); err != nil {
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
			return nil, fmt.Errorf("jdk: timed out waiting for a concurrent install of %s", version)
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
		return "", fmt.Errorf("jdk: %s: status %d", a.URL, resp.StatusCode)
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
		return "", fmt.Errorf("jdk: download %s: %w", a.Version, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != a.SHA256 {
		os.Remove(tmp)
		return "", fmt.Errorf("jdk: %s: sha256 mismatch (got %s)", a.Version, got)
	}
	return tmp, nil
}

// Extract unpacks archive into a scratch dir under root and returns the path
// of the single top-level directory it contains. Shared with the graal
// package (the archive shapes match).
func Extract(root, archive, name string) (string, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	work, err := os.MkdirTemp(root, ".extract-")
	if err != nil {
		return "", err
	}
	fail := func(err error) (string, error) {
		os.RemoveAll(work)
		return "", err
	}
	if strings.HasSuffix(name, ".zip") {
		err = extractZip(archive, work)
	} else {
		err = extractTargz(archive, work)
	}
	if err != nil {
		return fail(err)
	}
	entries, err := os.ReadDir(work)
	if err != nil {
		return fail(err)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		return fail(fmt.Errorf("jdk: unexpected archive layout in %s", name))
	}
	return filepath.Join(work, entries[0].Name()), nil
}

func safeJoin(dst, name string) (string, error) {
	clean := filepath.Clean(filepath.Join(dst, name))
	if clean == dst {
		return dst, nil
	}
	if !strings.HasPrefix(clean, dst+string(filepath.Separator)) {
		return "", fmt.Errorf("jdk: unsafe path %q in archive", name)
	}
	return clean, nil
}

func extractTargz(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(dst, h.Name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			os.Remove(target)
			if err := os.Symlink(h.Linkname, target); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, h.FileInfo().Mode().Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("jdk: unsupported archive entry %q", h.Name)
		}
	}
	return gz.Close()
}

func extractZip(src, dst string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, z := range r.File {
		name := strings.ReplaceAll(z.Name, "\\", "/")
		target, err := safeJoin(dst, name)
		if err != nil {
			return err
		}
		if strings.HasSuffix(name, "/") {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := z.Open()
		if err != nil {
			return err
		}
		mode := z.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
		if err != nil {
			rc.Close()
			return err
		}
		_, cerr := io.Copy(out, rc)
		rc.Close()
		if cerr != nil {
			out.Close()
			return cerr
		}
		if err := out.Close(); err != nil {
			return err
		}
	}
	return nil
}
