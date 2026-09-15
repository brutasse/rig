// Package updater finds rig releases on GitHub and installs them.
package updater

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// DefaultAPI is the GitHub API base for the rig repository.
const DefaultAPI = "https://api.github.com/repos/brutasse/rig"

var releaseTagRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// Release is a GitHub release of rig.
type Release struct {
	Tag    string
	Assets []Asset
}

// Asset is one downloadable file of a release.
type Asset struct {
	Name string
	URL  string
}

// ParseTag validates a vX.Y.Z tag and returns it without the v prefix.
func ParseTag(tag string) (string, error) {
	if !releaseTagRe.MatchString(tag) {
		return "", fmt.Errorf("updater: bad version tag %q (want vX.Y.Z)", tag)
	}
	return strings.TrimPrefix(tag, "v"), nil
}

// CompareVersions orders dotted versions: -1, 0, 1 for a<b, a==b, a>b.
func CompareVersions(a, b string) int {
	pa, pb := parseDotted(a), parseDotted(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parseDotted(v string) [3]int {
	var out [3]int
	for i, part := range strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3) {
		if i >= 3 {
			break
		}
		if n, err := strconv.Atoi(part); err == nil {
			out[i] = n
		}
	}
	return out
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

func toRelease(g ghRelease) Release {
	rel := Release{Tag: g.TagName}
	for _, a := range g.Assets {
		rel.Assets = append(rel.Assets, Asset{Name: a.Name, URL: a.BrowserDownloadURL})
	}
	return rel
}

func getRelease(ctx context.Context, client *http.Client, apiBase, path string) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("updater: %s: status %d", path, resp.StatusCode)
	}
	var g ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&g); err != nil {
		return nil, fmt.Errorf("updater: bad release response: %w", err)
	}
	rel := toRelease(g)
	return &rel, nil
}

// Latest returns the latest (non-prerelease) release.
func Latest(ctx context.Context, client *http.Client, apiBase string) (*Release, error) {
	return getRelease(ctx, client, apiBase, "/releases/latest")
}

// Get returns the release for tag (vX.Y.Z).
func Get(ctx context.Context, client *http.Client, apiBase, tag string) (*Release, error) {
	if _, err := ParseTag(tag); err != nil {
		return nil, err
	}
	return getRelease(ctx, client, apiBase, "/releases/tags/"+tag)
}

// AssetOf finds asset name in the release's assets.
func (r Release) AssetOf(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// BinaryName is the release asset name of the rig binary for os/arch.
func BinaryName(os, arch string) string {
	return "rig-" + os + "-" + arch
}

// InstallBinary downloads the release binary for osName/arch into dir,
// verifies it against the release's SHA256SUMS asset, and atomically installs
// it as dir/rig, returning the path.
func InstallBinary(ctx context.Context, client *http.Client, rel *Release, osName, arch, dir string) (string, error) {
	sums, ok := rel.AssetOf("SHA256SUMS")
	if !ok {
		return "", fmt.Errorf("updater: release %s has no SHA256SUMS asset", rel.Tag)
	}
	bin, ok := rel.AssetOf(BinaryName(osName, arch))
	if !ok {
		return "", fmt.Errorf("updater: release %s has no %s asset", rel.Tag, BinaryName(osName, arch))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(dir, ".rig-update-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	sumsPath, err := download(ctx, client, sums.URL, tmp)
	if err != nil {
		return "", err
	}
	want, err := checksumOf(sumsPath, BinaryName(osName, arch))
	if err != nil {
		return "", err
	}
	binPath, err := download(ctx, client, bin.URL, tmp)
	if err != nil {
		return "", err
	}
	if got, err := fileSHA256(binPath); err != nil {
		return "", err
	} else if got != want {
		return "", fmt.Errorf("updater: %s: sha256 mismatch (got %s)", BinaryName(osName, arch), got)
	}
	if err := os.Chmod(binPath, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, "rig")
	if err := os.Rename(binPath, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func download(ctx context.Context, client *http.Client, url, dir string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("updater: %s: status %d", url, resp.StatusCode)
	}
	tmp, err := os.CreateTemp(dir, ".dl-")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// checksumOf returns the expected sha256 for name from a SHA256SUMS file.
func checksumOf(path, name string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && fields[1] == name {
			return fields[0], nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("updater: %s not listed in SHA256SUMS", name)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
