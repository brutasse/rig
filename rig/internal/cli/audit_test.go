package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/brutasse/rig/internal/lockfile"
)

// osvFixture feeds a fake OSV API: batch maps "group:name@version" to the
// vuln ids the querybatch response reports, and detail maps a vuln id to
// its record JSON for the /vulns/{id} fetch.
type osvFixture struct {
	batch  map[string][]string
	detail map[string]string
}

func osvServer(t *testing.T, f osvFixture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/querybatch" {
			var req struct {
				Queries []struct {
					Package struct {
						Name string `json:"name"`
					} `json:"package"`
					Version string `json:"version"`
				} `json:"queries"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			results := make([]map[string]any, len(req.Queries))
			for i, q := range req.Queries {
				ids := f.batch[q.Package.Name+"@"+q.Version]
				vulns := make([]map[string]string, len(ids))
				for j, id := range ids {
					vulns[j] = map[string]string{"id": id, "modified": "2024-01-01T00:00:00Z"}
				}
				results[i] = map[string]any{"vulns": vulns}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/vulns/") {
			if body, ok := f.detail[strings.TrimPrefix(r.URL.Path, "/vulns/")]; ok {
				_, _ = w.Write([]byte(body))
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// auditWorkspace writes a single-module workspace pinning
// org.clojure/clojure 1.11.0 into a fresh directory the test moves to.
func auditWorkspace(t *testing.T) *lockfile.Document {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	doc := lockfile.ForTest(".")
	writeFile(t, "deps.edn", "{:deps {}}\n")
	if err := doc.Save("deps.lock"); err != nil {
		t.Fatal(err)
	}
	return doc
}

const testVulnRecord = `{
	"id": "GHSA-test-aaaa-bbbb",
	"summary": "Insecure deserialization in clojure.reader",
	"aliases": ["CVE-2024-0001"],
	"severity": [{"type": "CVSS_V3", "score": "9.8"}],
	"published": "2024-01-02T03:04:05Z",
	"affected": [{"ranges": [{"type": "ECOSYSTEM", "events": [{"introduced": "0"}, {"fixed": "1.12.0"}]}]}]
}`

func TestAuditClean(t *testing.T) {
	auditWorkspace(t)
	t.Setenv("RIG_OSV_API", osvServer(t, osvFixture{}).URL)
	code, out := runCLI(t, "audit", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("exit = %d, want 0; out: %s", code, out)
	}
	if want := "audit: no known vulnerabilities (1 maven artifact)"; !strings.Contains(out, want) {
		t.Errorf("out missing %q; out: %s", want, out)
	}
}

func TestAuditVulnFound(t *testing.T) {
	auditWorkspace(t)
	fix := osvFixture{
		batch: map[string][]string{"org.clojure:clojure@1.11.0": {"GHSA-test-aaaa-bbbb"}},
		detail: map[string]string{
			"GHSA-test-aaaa-bbbb": testVulnRecord,
		},
	}
	t.Setenv("RIG_OSV_API", osvServer(t, fix).URL)
	code, out := runCLI(t, "audit", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
	for _, want := range []string{
		"audit: 1 known vulnerabilities in 1 of 1 pinned artifacts",
		"org.clojure/clojure 1.11.0",
		"GHSA-test-aaaa-bbbb (CVE-2024-0001) — Insecure deserialization in clojure.reader",
		"published 2024-01-02",
		"severity CVSS_V3 9.8",
		"fixed in 1.12.0",
		"audit: 1 known vulnerabilities in 1 artifact(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q; out: %s", want, out)
		}
	}
}

// The batch names the vuln id, the full record comes from the /vulns
// fetch: a record the fake API does not know must fail the audit, not
// drop the finding.
func TestAuditVulnRecordFetchFails(t *testing.T) {
	auditWorkspace(t)
	fix := osvFixture{
		batch: map[string][]string{"org.clojure:clojure@1.11.0": {"GHSA-missing"}},
	}
	t.Setenv("RIG_OSV_API", osvServer(t, fix).URL)
	code, out := runCLI(t, "audit", "--cache-dir", t.TempDir())
	if code != 1 || !strings.Contains(out, "HTTP 404") {
		t.Fatalf("exit = %d, want a fetch failure; out: %s", code, out)
	}
}

func TestAuditOffline(t *testing.T) {
	auditWorkspace(t)
	code, out := runCLI(t, "audit", "--offline", "--cache-dir", t.TempDir())
	if code != 1 || !strings.Contains(out, "offline: 'rig audit' needs the network") {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
}

func TestAuditNoLock(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", "{:deps {}}\n")
	code, out := runCLI(t, "audit", "--cache-dir", t.TempDir())
	if code != 3 || !strings.Contains(out, "no lock at") {
		t.Fatalf("exit = %d, want 3; out: %s", code, out)
	}
}

// A lock holding only git deps has nothing for OSV to look up: the scan
// reports the skip and exits clean.
func TestAuditGitOnly(t *testing.T) {
	doc := auditWorkspace(t)
	git := lockfile.Artifact{
		ID:      "x/y:1:git",
		Kind:    "git",
		Group:   "x",
		Name:    "y",
		Version: "1",
		Git:     &lockfile.GitRef{URL: "https://example.invalid/x.git", SHA: strings.Repeat("a", 40)},
	}
	doc.Artifacts = []lockfile.Artifact{git}
	ref := git.ID
	for m, mod := range doc.Modules {
		mod.Classpath = []lockfile.ClasspathEntry{{Ref: &ref}}
		doc.Modules[m] = mod
	}
	if err := doc.Save("deps.lock"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIG_OSV_API", osvServer(t, osvFixture{}).URL)
	code, out := runCLI(t, "audit", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("exit = %d, want 0; out: %s", code, out)
	}
	if want := "audit: no maven artifacts to scan (1 git dep skipped, pinned by commit sha)"; !strings.Contains(out, want) {
		t.Errorf("out missing %q; out: %s", want, out)
	}
}
