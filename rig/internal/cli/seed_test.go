package cli

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// pin is one pinned fixture artifact: its Maven coordinate, the pinned
// jar's sha256, and the upstream URL the bytes are fetched from on a
// cold machine. The pins are the integrity gate of the seeded repo: a
// download that does not hash to the pin fails the suite.
type pin struct {
	Coord    string `json:"coord"`
	Version  string `json:"version"`
	SHA256   string `json:"sha256"`
	Upstream string `json:"upstream"`
}

type pinFile struct {
	Entries []pin `json:"entries"`
}

// seedLastUpdated stamps the generated maven-metadata.xml files with a
// fixed publication time: old enough that no fixture release is inside
// any cooldown, deterministic so cooldown and outdated behavior is
// stable. It is generated, never fetched — live metadata is what made
// TestUpdateLatest flake.
const seedLastUpdated = "20250101000000"

var (
	seedOnce sync.Once
	seedErr  error

	// testRepoURL is the absolute file:// URL of the seeded fixture
	// maven repo, set once the seed is ready. Fixture copies and the
	// tests that write their own manifests point their :mvn/repos at
	// it, replacing the real Central and Clojars for the whole suite.
	testRepoURL string

	// seedDirs are suite-scoped temp dirs (the shared pre-locked
	// primal); TestMain removes them on exit.
	seedDirs []string
)

// seededRepos is the :mvn/repos EDN for a manifest a test writes whole:
// both standard ids pointed at the seeded fixture repo, so a resolution
// from that manifest never reaches the real Central or Clojars. Extra
// EDN entries (repo ids not in the standard set) join the same map.
func seededRepos(extra ...string) string {
	return ` :mvn/repos {"central" {:url "` + testRepoURL + `"} "clojars" {:url "` + testRepoURL + `"}` +
		strings.Join(extra, " ") + `}`
}

func testdataDir() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("testdataDir: no caller")
	}
	return filepath.Join(filepath.Dir(thisFile), "testdata")
}

// seedTestRepo builds (or reuses) the seeded fixture maven repo and
// exposes it as testRepoURL. It is the suite's single network point:
// artifact bytes, pinned and immutable, on a cold machine only.
func seedTestRepo(t *testing.T) {
	t.Helper()
	seedOnce.Do(func() {
		seedErr = seedRepo()
	})
	if seedErr != nil {
		t.Skipf("cannot seed the fixture repo: %v", seedErr)
	}
}

// seedRepo builds testdata/.seeded-repo from testdata/pins.json.
// Present, hash-verified artifacts are kept; missing ones are
// downloaded from their pinned upstream. maven-metadata.xml is
// generated from the pin set (versions, newest as latest, fixed old
// lastUpdated), never downloaded.
func seedRepo() error {
	dir := filepath.Join(testdataDir(), ".seeded-repo")
	raw, err := os.ReadFile(filepath.Join(testdataDir(), "pins.json"))
	if err != nil {
		return err
	}
	var pf pinFile
	if err := json.Unmarshal(raw, &pf); err != nil {
		return fmt.Errorf("bad pins.json: %w", err)
	}

	// Marker: the pin set's digest. A pins.json change re-seeds
	// (missing, changed or new artifacts are (re)downloaded; a stale
	// artifact that now hashes to a different pin is replaced).
	marker := sha256.Sum256(raw)
	if b, err := os.ReadFile(filepath.Join(dir, ".pins-sha")); err == nil {
		if string(b) != string(marker[:]) {
			// The pin set moved; drop the marker so the artifacts are
			// re-verified below, then rewrite it fresh at the end.
			os.Remove(filepath.Join(dir, ".pins-sha"))
		}
	}

	client := &http.Client{Timeout: 90 * time.Second}
	byCoord := map[string][]string{}
	for _, e := range pf.Entries {
		g, n := e.Coord[:strings.LastIndex(e.Coord, "/")], e.Coord[strings.LastIndex(e.Coord, "/")+1:]
		byCoord[e.Coord] = append(byCoord[e.Coord], e.Version)
		vdir := filepath.Join(dir, strings.ReplaceAll(g, ".", "/"), n, e.Version)
		jar, pom := filepath.Join(vdir, n+"-"+e.Version+".jar"), filepath.Join(vdir, n+"-"+e.Version+".pom")
		if ok, err := hashFileSHA256(jar); err == nil && ok == e.SHA256 && statOK(pom) {
			continue
		}
		if err := os.MkdirAll(vdir, 0o755); err != nil {
			return err
		}
		if err := fetchPinned(client, e.Upstream, jar, e.SHA256); err != nil {
			return err
		}
		pomURL := strings.TrimSuffix(e.Upstream, ".jar") + ".pom"
		if err := fetchUnpinned(client, pomURL, pom); err != nil {
			return err
		}
		if err := writeSHA1(jar); err != nil {
			return err
		}
		if err := writeSHA1(pom); err != nil {
			return err
		}
	}

	// Generated per-coord metadata.
	for coord, versions := range byCoord {
		sort.Strings(versions)
		g, n := coord[:strings.LastIndex(coord, "/")], coord[strings.LastIndex(coord, "/")+1:]
		var b strings.Builder
		b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<metadata>\n")
		b.WriteString("  <groupId>" + g + "</groupId>\n  <artifactId>" + n + "</artifactId>\n  <versioning>\n")
		b.WriteString("    <latest>" + newestMavenVersion(versions) + "</latest>\n")
		b.WriteString("    <versions>\n")
		for _, v := range versions {
			b.WriteString("      <version>" + v + "</version>\n")
		}
		b.WriteString("    </versions>\n    <lastUpdated>" + seedLastUpdated + "</lastUpdated>\n  </versioning>\n</metadata>\n")
		p := filepath.Join(dir, strings.ReplaceAll(g, ".", "/"), n, "maven-metadata.xml")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
			return err
		}
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, ".pins-sha"), marker[:], 0o644); err != nil {
		return err
	}
	testRepoURL = "file://" + dir
	return nil
}

// fetchPinned downloads url to path (atomically) and fails when the
// bytes do not hash to want.
func fetchPinned(client *http.Client, url, path, want string) error {
	var last error
	for i := 0; i < 3; i++ {
		resp, err := client.Get(url)
		if err != nil {
			last = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			last = fmt.Errorf("%s: status %d", url, resp.StatusCode)
			continue
		}
		tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
		if err != nil {
			resp.Body.Close()
			return err
		}
		h := sha256.New()
		_, err = io.Copy(io.MultiWriter(tmp, h), resp.Body)
		resp.Body.Close()
		tmp.Close()
		if err != nil {
			os.Remove(tmp.Name())
			last = err
			continue
		}
		if hex.EncodeToString(h.Sum(nil)) != want {
			os.Remove(tmp.Name())
			last = fmt.Errorf("pin mismatch for %s: got %s, want %s", url, hex.EncodeToString(h.Sum(nil)), want)
			return last
		}
		return os.Rename(tmp.Name(), path)
	}
	return last
}

// fetchUnpinned downloads url to path (atomically). Pom bytes carry no
// pin: the jar's hash is the gate, and a pom is only read for
// resolution, never trusted for behavior.
func fetchUnpinned(client *http.Client, url, path string) error {
	var last error
	for i := 0; i < 3; i++ {
		resp, err := client.Get(url)
		if err != nil {
			last = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			last = fmt.Errorf("%s: status %d", url, resp.StatusCode)
			continue
		}
		tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
		if err != nil {
			resp.Body.Close()
			return err
		}
		_, err = io.Copy(tmp, resp.Body)
		resp.Body.Close()
		tmp.Close()
		if err != nil {
			os.Remove(tmp.Name())
			last = err
			continue
		}
		return os.Rename(tmp.Name(), path)
	}
	return last
}

// hashFileSHA256 returns (hex, error) of the file's bytes.
func hashFileSHA256(path string) (string, error) {
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

// writeSHA1 writes the Maven <file>.sha1 record beside path.
func writeSHA1(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	return os.WriteFile(path+".sha1", []byte(hex.EncodeToString(h.Sum(nil))+"\n"), 0o644)
}

// newestMavenVersion returns the newest of vs by Maven ordering
// (numeric segments first, then non-numeric; longer wins on a common
// prefix). Good enough for the plain versions the pin set carries.
func newestMavenVersion(vs []string) string {
	best := vs[0]
	for _, v := range vs[1:] {
		if mavenCmp(v, best) > 0 {
			best = v
		}
	}
	return best
}

// mavenCmp compares two versions the Maven way for plain X.Y[.Z][-qual]
// shapes: segment by segment, numeric segments as numbers, non-numeric
// segments as strings; a version that is a prefix of the other is
// older.
func mavenCmp(a, b string) int {
	// A qualifier (a dash) makes a version younger than its base only
	// within the same numeric head; treat "-SNAPSHOT" as oldest among
	// the pin set's shapes by splitting on the first dash.
	headA, qualA := a, ""
	if i := strings.IndexByte(a, '-'); i >= 0 {
		headA, qualA = a[:i], a[i+1:]
	}
	headB, qualB := b, ""
	if i := strings.IndexByte(b, '-'); i >= 0 {
		headB, qualB = b[:i], b[i+1:]
	}
	if c := mavenHeadCmp(headA, headB); c != 0 {
		return c
	}
	switch {
	case qualA == "" && qualB != "":
		return 1 // a release outranks its snapshot/qualifier
	case qualA != "" && qualB == "":
		return -1
	}
	return strings.Compare(qualA, qualB)
}

func mavenHeadCmp(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y string
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		if x == y {
			continue
		}
		if x == "" {
			return -1
		}
		if y == "" {
			return 1
		}
		if xn, xerr := parseSegment(x); xerr == nil {
			if yn, yerr := parseSegment(y); yerr == nil {
				if xn != yn {
					if xn < yn {
						return -1
					}
					return 1
				}
				continue
			}
		}
		if x < y {
			return -1
		}
		return 1
	}
	return 0
}

func parseSegment(s string) (int, error) {
	var n int
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not numeric")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

// registerSeedDir keeps a suite-scoped temp dir for TestMain's cleanup.
func registerSeedDir(d string) {
	seedDirs = append(seedDirs, d)
}

// removeSeedDirs is called from TestMain after the suite runs.
func removeSeedDirs() {
	for _, d := range seedDirs {
		os.RemoveAll(d)
	}
}
