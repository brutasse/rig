package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brutasse/rig/internal/lockfile"
	"github.com/brutasse/rig/internal/workspace"
)

// osvFixture feeds a fake OSV API: batch maps "group:name@version" to the
// vuln ids the querybatch response reports, and detail maps a vuln id to
// its record JSON for the /vulns/{id} fetch.
type osvFixture struct {
	batch  map[string][]string
	detail map[string]string
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

// The vector record's severity is the CVSS vector string itself: the
// audit computes its base score in-process (9.8, Critical).
const testVulnRecordVector = `{
	"id": "GHSA-vec-cccc-dddd",
	"summary": "Remote denial of service in clojure.core",
	"aliases": ["CVE-2024-0002"],
	"severity": [{"type": "CVSS_V3", "score": "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"}],
	"published": "2024-03-02T03:04:05Z",
	"affected": [{"ranges": [{"type": "ECOSYSTEM", "events": [{"introduced": "0"}, {"fixed": "1.12.1"}]}]}]
}`

// The bad-vector record's severity is not a scorable vector: the audit
// keeps the type (and the vector) and leaves the score unset.
const testVulnRecordBadVector = `{
	"id": "GHSA-vec-eeee-ffff",
	"summary": "Mystery severity",
	"severity": [{"type": "CVSS_V4", "score": "not-a-vector"}],
	"published": "2024-04-02T03:04:05Z",
	"affected": [{"ranges": [{"type": "ECOSYSTEM", "events": [{"introduced": "0"}, {"fixed": "1.13.0"}]}]}]
}`

const testPrivVulnRecord = `{
	"id": "GHSA-priv-1111-2222",
	"summary": "Something in internal",
	"severity": [{"type": "CVSS_V3", "score": "7.5"}],
	"published": "2024-02-02T03:04:05Z",
	"affected": [{"ranges": [{"type": "ECOSYSTEM", "events": [{"introduced": "0"}, {"fixed": "2.1.0"}]}]}]
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
		"used in .",
		"GHSA-test-aaaa-bbbb (CVE-2024-0001) — Insecure deserialization in clojure.reader",
		"published 2024-01-02",
		"severity CVSS_V3 9.8 Critical",
		"fixed in 1.12.0",
		"audit: 1 known vulnerabilities in 1 artifact(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q; out: %s", want, out)
		}
	}
}

// A severity given as the CVSS vector string is scored in-process: the
// report line carries the computed base score and its bucket.
func TestAuditVulnVectorScored(t *testing.T) {
	auditWorkspace(t)
	fix := osvFixture{
		batch: map[string][]string{"org.clojure:clojure@1.11.0": {"GHSA-vec-cccc-dddd"}},
		detail: map[string]string{
			"GHSA-vec-cccc-dddd": testVulnRecordVector,
		},
	}
	t.Setenv("RIG_OSV_API", osvServer(t, fix).URL)
	code, out := runCLI(t, "audit", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
	if want := "severity CVSS_V3 9.8 Critical"; !strings.Contains(out, want) {
		t.Errorf("out missing %q; out: %s", want, out)
	}
	// An unscoreable vector keeps the type, without a fabricated score.
	fix = osvFixture{
		batch: map[string][]string{"org.clojure:clojure@1.11.0": {"GHSA-vec-eeee-ffff"}},
		detail: map[string]string{
			"GHSA-vec-eeee-ffff": testVulnRecordBadVector,
		},
	}
	t.Setenv("RIG_OSV_API", osvServer(t, fix).URL)
	code, out = runCLI(t, "audit", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
	if want := "severity CVSS_V4"; !strings.Contains(out, want) {
		t.Errorf("out missing %q; out: %s", want, out)
	}
}

// The lock records every alias's classpath: the finding's scopes name
// the modules and alias scopes the artifact is locked into.
func TestAuditScopes(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	doc := lockfile.ForTest(".", "modules/app")
	mod := doc.Modules["modules/app"]
	mod.Aliases = map[string]lockfile.Alias{
		"smoke": {Classpath: mod.Classpath},
	}
	doc.Modules["modules/app"] = mod
	writeFile(t, "deps.edn", "{:deps {}}\n")
	if err := doc.Save("deps.lock"); err != nil {
		t.Fatal(err)
	}
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
	if want := "used in ., modules/app, modules/app:smoke"; !strings.Contains(out, want) {
		t.Errorf("out missing %q; out: %s", want, out)
	}
}

// auditJSONDoc decodes the --json document out of the combined
// runCLI output (the exit message rides on stderr after it).
func auditJSONDoc(t *testing.T, out string) auditReport {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(out))
	var report auditReport
	if err := dec.Decode(&report); err != nil {
		t.Fatalf("bad JSON document: %v; out: %s", err, out)
	}
	return report
}

func TestAuditJSON(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	doc := lockfile.ForTest(".", "modules/app")
	mod := doc.Modules["modules/app"]
	mod.Aliases = map[string]lockfile.Alias{
		"smoke": {Classpath: mod.Classpath},
	}
	doc.Modules["modules/app"] = mod
	writeFile(t, "deps.edn", "{:deps {}}\n")
	if err := doc.Save("deps.lock"); err != nil {
		t.Fatal(err)
	}
	fix := osvFixture{
		batch: map[string][]string{"org.clojure:clojure@1.11.0": {
			"GHSA-vec-cccc-dddd",
			"GHSA-vec-eeee-ffff",
		}},
		detail: map[string]string{
			"GHSA-vec-cccc-dddd": testVulnRecordVector,
			"GHSA-vec-eeee-ffff": testVulnRecordBadVector,
		},
	}
	t.Setenv("RIG_OSV_API", osvServer(t, fix).URL)
	code, out := runCLI(t, "audit", "--json", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
	report := auditJSONDoc(t, out)
	if report.Scanned != 1 || report.Skipped.Private != 0 || report.Skipped.Git != 0 {
		t.Errorf("scanned/skipped = %+v, want 1/0/0", report)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("findings = %+v, want one artifact group", report.Findings)
	}
	a := report.Findings[0]
	if a.Coordinate != "org.clojure/clojure" || a.Version != "1.11.0" {
		t.Errorf("artifact = %+v", a)
	}
	if got := strings.Join(a.Scopes, ", "); got != "., modules/app, modules/app:smoke" {
		t.Errorf("scopes = %q", got)
	}
	if len(a.Vulns) != 2 {
		t.Fatalf("vulnerabilities = %+v, want 2", a.Vulns)
	}
	v := a.Vulns[0] // sorted by id: the vector record first
	if v.ID != "GHSA-vec-cccc-dddd" ||
		len(v.Aliases) != 1 || v.Aliases[0] != "CVE-2024-0002" ||
		v.Summary != "Remote denial of service in clojure.core" ||
		v.Published != "2024-03-02T03:04:05Z" ||
		len(v.Fixed) != 1 || v.Fixed[0] != "1.12.1" {
		t.Errorf("vuln row = %+v", v)
	}
	if v.Severity == nil || v.Severity.Type != "CVSS_V3" ||
		v.Severity.Score == nil || *v.Severity.Score != 9.8 ||
		v.Severity.Bucket == nil || *v.Severity.Bucket != "Critical" ||
		v.Severity.Vector == nil || *v.Severity.Vector != "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H" {
		t.Errorf("severity = %+v", v.Severity)
	}
	bad := a.Vulns[1]
	if bad.Severity == nil || bad.Severity.Score != nil || bad.Severity.Bucket != nil ||
		bad.Severity.Vector == nil || *bad.Severity.Vector != "not-a-vector" {
		t.Errorf("unscoreable severity = %+v", bad.Severity)
	}
}

// A clean scan with --json exits 0 with an empty findings list.
func TestAuditJSONClean(t *testing.T) {
	auditWorkspace(t)
	t.Setenv("RIG_OSV_API", osvServer(t, osvFixture{}).URL)
	code, out := runCLI(t, "audit", "--json", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("exit = %d, want 0; out: %s", code, out)
	}
	report := auditJSONDoc(t, out)
	if report.Scanned != 1 || len(report.Findings) != 0 {
		t.Errorf("report = %+v, want 1 scanned, no findings", report)
	}
}

// A vuln record the API does not know (a 404, permanent, not retried)
// is skipped: with no findings left, the scan is incomplete, exit 4,
// the reason on stderr.
func TestAuditVulnRecordFetchFails(t *testing.T) {
	auditWorkspace(t)
	fix := osvFixture{
		batch: map[string][]string{"org.clojure:clojure@1.11.0": {"GHSA-missing"}},
	}
	t.Setenv("RIG_OSV_API", osvServer(t, fix).URL)
	code, out := runCLI(t, "audit", "--cache-dir", t.TempDir())
	if code != 4 {
		t.Fatalf("exit = %d, want 4; out: %s", code, out)
	}
	for _, want := range []string{
		"audit: GHSA-missing: osv: HTTP 404",
		"audit: scan incomplete: 1 artifact record(s) could not be fetched",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q; out: %s", want, out)
		}
	}
}

// zeroAuditBackoffs makes auditRetry's waits instant (the attempts
// still happen), restoring the production backoffs afterwards.
func zeroAuditBackoffs(t *testing.T) {
	t.Helper()
	old := auditBackoffs
	auditBackoffs = []time.Duration{0}
	t.Cleanup(func() { auditBackoffs = old })
}

// osvHandler is the fake OSV API behind osvServer, exposed for wrapping.
func osvHandler(f osvFixture) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
	}
}

func osvServer(t *testing.T, f osvFixture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(osvHandler(f))
	t.Cleanup(srv.Close)
	return srv
}

// A transient OSV failure (5xx) is retried: the batch and the record
// fetch each fail once, the retries succeed, and the scan completes.
func TestAuditTransientRetried(t *testing.T) {
	auditWorkspace(t)
	fix := osvFixture{
		batch:  map[string][]string{"org.clojure:clojure@1.11.0": {"GHSA-test-aaaa-bbbb"}},
		detail: map[string]string{"GHSA-test-aaaa-bbbb": testVulnRecord},
	}
	inner := osvHandler(fix)
	var batchN, vulnN int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n *int32
		if r.URL.Path == "/querybatch" {
			n = &batchN
		} else if strings.HasPrefix(r.URL.Path, "/vulns/") {
			n = &vulnN
		}
		if n != nil && atomic.AddInt32(n, 1) == 1 {
			http.Error(w, "boom", http.StatusServiceUnavailable)
			return
		}
		inner(w, r)
	}))
	t.Cleanup(srv.Close)
	zeroAuditBackoffs(t)
	t.Setenv("RIG_OSV_API", srv.URL)
	code, out := runCLI(t, "audit", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
	if got := atomic.LoadInt32(&batchN); got != 2 {
		t.Errorf("batch attempts = %d, want 2 (failed once, retried)", got)
	}
	if got := atomic.LoadInt32(&vulnN); got != 2 {
		t.Errorf("record fetch attempts = %d, want 2 (failed once, retried)", got)
	}
	if !strings.Contains(out, "GHSA-test-aaaa-bbbb") {
		t.Errorf("out missing the finding; out: %s", out)
	}
}

// A batch that fails through all the attempts fails the scan: exit 4,
// the reason on stderr, distinct from "has findings".
func TestAuditScanFailed(t *testing.T) {
	auditWorkspace(t)
	var batchN int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&batchN, 1)
		http.Error(w, "boom", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	zeroAuditBackoffs(t)
	t.Setenv("RIG_OSV_API", srv.URL)
	code, out := runCLI(t, "audit", "--cache-dir", t.TempDir())
	if code != 4 {
		t.Fatalf("exit = %d, want 4; out: %s", code, out)
	}
	if got := atomic.LoadInt32(&batchN); got != 2 {
		t.Errorf("batch attempts = %d, want 2 (1 + len(backoffs))", got)
	}
	if want := "audit: scan failed: osv: HTTP 503"; !strings.Contains(out, want) {
		t.Errorf("out missing %q; out: %s", want, out)
	}
}

// A record fetch that fails permanently leaves the rest of the report
// intact: the other findings stand, the affected artifact is counted,
// and the exit stays 1.
func TestAuditRecordFailedWithFindings(t *testing.T) {
	auditWorkspace(t)
	fix := osvFixture{
		batch: map[string][]string{"org.clojure:clojure@1.11.0": {"GHSA-test-aaaa-bbbb", "GHSA-missing"}},
		detail: map[string]string{
			"GHSA-test-aaaa-bbbb": testVulnRecord,
		},
	}
	t.Setenv("RIG_OSV_API", osvServer(t, fix).URL)
	code, out := runCLI(t, "audit", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
	if want := "audit: 1 known vulnerabilities in 1 artifact(s); 1 artifact record(s) could not be fetched"; !strings.Contains(out, want) {
		t.Errorf("out missing %q; out: %s", want, out)
	}
	if !strings.Contains(out, "audit: GHSA-missing: osv: HTTP 404") {
		t.Errorf("out missing the failed record note; out: %s", out)
	}
	// JSON carries the same: one vuln row, the failed count.
	code, out = runCLI(t, "audit", "--json", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
	report := auditJSONDoc(t, out)
	if report.Failed != 1 {
		t.Errorf("failed = %d, want 1; report: %+v", report.Failed, report)
	}
	if len(report.Findings) != 1 || len(report.Findings[0].Vulns) != 1 {
		t.Fatalf("findings = %+v, want one artifact with one vuln", report.Findings)
	}
}

// A finding's fixes span series: the report shows the locked version's
// own series first, the other series marked. fixedSeries carries the
// filter to the JSON document.
const testVulnRecordTwoSeries = `{
	"id": "GHSA-fix-own-both",
	"summary": "Fix in the own and another series",
	"severity": [{"type": "CVSS_V3", "score": "5.5"}],
	"published": "2024-05-02T03:04:05Z",
	"affected": [{"ranges": [{"type": "ECOSYSTEM", "events": [{"introduced": "0"}, {"fixed": "1.11.1"}, {"fixed": "1.12.0"}]}]}]
}`

const testVulnRecordOtherSeries = `{
	"id": "GHSA-fix-other",
	"summary": "Fix only in another series",
	"severity": [{"type": "CVSS_V3", "score": "5.5"}],
	"published": "2024-05-03T03:04:05Z",
	"affected": [{"ranges": [{"type": "ECOSYSTEM", "events": [{"introduced": "0"}, {"fixed": "1.12.0"}]}]}]
}`

func TestAuditFixedSeries(t *testing.T) {
	auditWorkspace(t)
	fix := osvFixture{
		batch: map[string][]string{"org.clojure:clojure@1.11.0": {"GHSA-fix-own-both", "GHSA-fix-other"}},
		detail: map[string]string{
			"GHSA-fix-own-both": testVulnRecordTwoSeries,
			"GHSA-fix-other":    testVulnRecordOtherSeries,
		},
	}
	t.Setenv("RIG_OSV_API", osvServer(t, fix).URL)
	code, out := runCLI(t, "audit", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
	for _, want := range []string{
		"fixed in 1.11.1 · other series: 1.12.0",
		"fixed in 1.12.0 (other series)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q; out: %s", want, out)
		}
	}
	code, out = runCLI(t, "audit", "--json", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
	report := auditJSONDoc(t, out)
	if len(report.Findings) != 1 || len(report.Findings[0].Vulns) != 2 {
		t.Fatalf("findings = %+v, want one artifact with two vulns", report.Findings)
	}
	byID := make(map[string]auditVulnRow)
	for _, v := range report.Findings[0].Vulns {
		byID[v.ID] = v
	}
	both, other := byID["GHSA-fix-own-both"], byID["GHSA-fix-other"]
	if both.ID != "GHSA-fix-own-both" ||
		strings.Join(both.Fixed, ",") != "1.11.1,1.12.0" ||
		strings.Join(both.FixedSeries, ",") != "1.11.1" {
		t.Errorf("vuln row = %+v, want fixed 1.11.1+1.12.0, fixedSeries 1.11.1", both)
	}
	if other.ID != "GHSA-fix-other" ||
		strings.Join(other.Fixed, ",") != "1.12.0" ||
		len(other.FixedSeries) != 0 {
		t.Errorf("vuln row = %+v, want fixed 1.12.0, no fixedSeries", other)
	}
}

// --explain adds each finding's puller chains, one per scope: the chains
// the kernel walks out of the module's trace (the hot fixture: spec.alpha
// under clojure in the base classpath, under clojure AND kaocha in the
// :test alias; kaocha a direct extra-dep of the alias).
func TestAuditExplain(t *testing.T) {
	hotSetup(t)
	fix := osvFixture{
		batch: map[string][]string{
			"org.clojure:spec.alpha@0.3.218": {"GHSA-expl-1111-2222"},
			"lambdaisland:kaocha@1.66.1034":  {"GHSA-expl-3333-4444"},
		},
		detail: map[string]string{
			"GHSA-expl-1111-2222": testVulnRecord,
			"GHSA-expl-3333-4444": testPrivVulnRecord,
		},
	}
	t.Setenv("RIG_OSV_API", osvServer(t, fix).URL)
	specChain := "modules/app: org.clojure/clojure:1.11.0 → org.clojure/spec.alpha:0.3.218"
	code, out := runCLI(t, "audit", "--explain", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
	for _, want := range []string{
		"lambdaisland/kaocha 1.66.1034",
		"used in modules/app:test",
		"pulled in:",
		"modules/app:test: lambdaisland/kaocha:1.66.1034",
		"org.clojure/spec.alpha 0.3.218",
		"used in modules/app, modules/app:test",
		specChain,
		"modules/app:test: lambdaisland/kaocha:1.66.1034 → org.clojure/spec.alpha:0.3.218",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q; out: %s", want, out)
		}
	}
	if !strings.Contains(out, "audit: 2 known vulnerabilities in 2 artifact(s)") {
		t.Errorf("out missing the summary; out: %s", out)
	}

	// JSON: chains per scope per artifact, the kernel's chains verbatim.
	code, out = runCLI(t, "audit", "--explain", "--json", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
	report := auditJSONDoc(t, out)
	var spec, kaocha *auditArtifact
	for i := range report.Findings {
		switch report.Findings[i].Coordinate {
		case "org.clojure/spec.alpha":
			spec = &report.Findings[i]
		case "lambdaisland/kaocha":
			kaocha = &report.Findings[i]
		}
	}
	if spec == nil || kaocha == nil {
		t.Fatalf("findings = %+v, want both artifacts", report.Findings)
	}
	countChains := func(a *auditArtifact, scope string) int {
		n := 0
		for _, c := range a.Chains {
			if c.Scope == scope {
				n++
			}
		}
		return n
	}
	if got := len(spec.Chains); got != 3 {
		t.Fatalf("spec.alpha chains = %+v, want 3 (one per scope's chain)", spec.Chains)
	}
	if got := countChains(spec, "modules/app"); got != 1 {
		t.Errorf("spec.alpha chains in modules/app = %d, want 1", got)
	}
	if got := countChains(spec, "modules/app:test"); got != 2 {
		t.Errorf("spec.alpha chains in modules/app:test = %d, want 2 (clojure, kaocha)", got)
	}
	if got := countChains(kaocha, "modules/app:test"); got != 1 {
		t.Errorf("kaocha chains = %+v, want 1", kaocha.Chains)
	}
	if got := kaocha.Chains[0].Path; len(got) != 1 || got[0] != "lambdaisland/kaocha:1.66.1034" {
		t.Errorf("kaocha chain = %+v, want the one-step extra-dep chain", got)
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

// privArtifactTo appends a com.example/internal artifact pinned from the
// given repository to the doc, on the module's classpath.
func privArtifactTo(t *testing.T, doc *lockfile.Document, repo string) {
	t.Helper()
	internal := lockfile.Artifact{
		ID:         "com.example/internal:2.0.0:jar",
		Kind:       "mvn",
		Group:      "com.example",
		Name:       "internal",
		Version:    "2.0.0",
		Extension:  "jar",
		Repository: repo,
		URL:        "https://example.invalid/internal-2.0.0.jar",
		SHA256:     strings.Repeat("ab", 32),
	}
	doc.Artifacts = append(doc.Artifacts, internal)
	ref := internal.ID
	for m, mod := range doc.Modules {
		mod.Classpath = append(mod.Classpath, lockfile.ClasspathEntry{Ref: &ref})
		doc.Modules[m] = mod
	}
}

// Artifacts from a private repository are skipped before the OSV query:
// the fixture carries a finding for the coordinate, which a regression
// that queried it would fail on (exit 1). The scan sees nothing to audit.
func TestAuditPrivateRepoSkipped(t *testing.T) {
	doc := auditWorkspace(t)
	doc.Artifacts[0].Repository = "nexus"
	if err := doc.Save("deps.lock"); err != nil {
		t.Fatal(err)
	}
	fix := osvFixture{
		batch:  map[string][]string{"org.clojure:clojure@1.11.0": {"GHSA-test-aaaa-bbbb"}},
		detail: map[string]string{"GHSA-test-aaaa-bbbb": testVulnRecord},
	}
	t.Setenv("RIG_OSV_API", osvServer(t, fix).URL)
	code, out := runCLI(t, "audit", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("exit = %d, want 0; out: %s", code, out)
	}
	if want := "audit: no maven artifacts to scan (1 from private repositories, not audited)"; !strings.Contains(out, want) {
		t.Errorf("out missing %q; out: %s", want, out)
	}
}

// Overriding central or clojars keeps the id: the artifact is scanned as
// the real one, and its finding fails the audit.
func TestAuditRepoOverrideScanned(t *testing.T) {
	doc := auditWorkspace(t)
	doc.Artifacts[0].Repository = "clojars"
	doc.Artifacts[0].URL = "https://mirror.example.invalid/clojure-1.11.0.jar"
	if err := doc.Save("deps.lock"); err != nil {
		t.Fatal(err)
	}
	fix := osvFixture{
		batch:  map[string][]string{"org.clojure:clojure@1.11.0": {"GHSA-test-aaaa-bbbb"}},
		detail: map[string]string{"GHSA-test-aaaa-bbbb": testVulnRecord},
	}
	t.Setenv("RIG_OSV_API", osvServer(t, fix).URL)
	code, out := runCLI(t, "audit", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
	if want := "audit: 1 known vulnerabilities in 1 of 1 pinned artifacts"; !strings.Contains(out, want) {
		t.Errorf("out missing %q; out: %s", want, out)
	}
}

// A clean public artifact next to a private one: the private artifact is
// counted, not audited, and the scan is clean.
func TestAuditCleanWithPrivate(t *testing.T) {
	doc := auditWorkspace(t)
	privArtifactTo(t, doc, "nexus")
	if err := doc.Save("deps.lock"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIG_OSV_API", osvServer(t, osvFixture{}).URL)
	code, out := runCLI(t, "audit", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("exit = %d, want 0; out: %s", code, out)
	}
	if want := "audit: no known vulnerabilities (1 maven artifact, 1 from private repositories, not audited)"; !strings.Contains(out, want) {
		t.Errorf("out missing %q; out: %s", want, out)
	}
}

// Private coordinates never enter the OSV query: only the public one is
// counted and failed on, and the fixture's private finding stays unseen.
func TestAuditPrivateNotCounted(t *testing.T) {
	doc := auditWorkspace(t)
	privArtifactTo(t, doc, "nexus")
	if err := doc.Save("deps.lock"); err != nil {
		t.Fatal(err)
	}
	fix := osvFixture{
		batch: map[string][]string{
			"org.clojure:clojure@1.11.0": {"GHSA-test-aaaa-bbbb"},
			"com.example:internal@2.0.0": {"GHSA-priv-1111-2222"},
		},
		detail: map[string]string{
			"GHSA-test-aaaa-bbbb": testVulnRecord,
			"GHSA-priv-1111-2222": testPrivVulnRecord,
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
	} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q; out: %s", want, out)
		}
	}
	for _, absent := range []string{
		"com.example/internal",
		"GHSA-priv-1111-2222",
	} {
		if strings.Contains(out, absent) {
			t.Errorf("out should not contain %q; out: %s", absent, out)
		}
	}
}

// TestAuditPinNote derives the remediation notes of findings from the
// workspace's manifests and lock: the overrides pin or the pool entry
// pinning the vulnerable version, the entry pinning a clean version
// (the stale lock), the overrides entry beating the pool on the same
// coordinate, a coordinate some module pins while others pull it
// undeclared — and no note in the plain cases (every locking module
// declares it, or none does).
func TestAuditPinNote(t *testing.T) {
	cases := []struct {
		name string
		root string
		app  string
		want string
	}{
		{
			name: "pool pins the vulnerable version",
			root: `{ :rig/modules ["modules/app"] :rig/deps {org.clojure/clojure "1.11.0"} }`,
			app:  `{ :deps {} }`,
			want: "bump the pool entry",
		},
		{
			name: "pool pins a clean version",
			root: `{ :rig/modules ["modules/app"] :rig/deps {org.clojure/clojure "1.12.0"} }`,
			app:  `{ :deps {} }`,
			want: "the :rig/deps pool pins 1.12.0; the lock still carries 1.11.0 — rig lock picks up the pool pin",
		},
		{
			name: "overrides pin pins the vulnerable version",
			root: `{ :rig/modules ["modules/app"] :rig/overrides {org.clojure/clojure "1.11.0"} }`,
			app:  `{ :deps {} }`,
			want: "bump the :rig/overrides pin",
		},
		{
			name: "overrides pin pins a clean version",
			root: `{ :rig/modules ["modules/app"] :rig/overrides {org.clojure/clojure "1.12.0"} }`,
			app:  `{ :deps {} }`,
			want: "the :rig/overrides pin pins 1.12.0; the lock still carries 1.11.0 — rig lock picks up the pin",
		},
		{
			name: "overrides beats the pool on the same coordinate",
			root: `{ :rig/modules ["modules/app"] :rig/deps {org.clojure/clojure "1.11.0"} :rig/overrides {org.clojure/clojure "1.12.0"} }`,
			app:  `{ :deps {} }`,
			want: "the :rig/overrides pin pins 1.12.0; the lock still carries 1.11.0 — rig lock picks up the pin",
		},
		{
			name: "declared in one module, pulled undeclared in the other",
			root: `{ :rig/modules ["modules/app"] }`,
			app:  `{ :deps {org.clojure/clojure "1.11.0"} }`,
			want: "a :rig/overrides force pin would force-pin it workspace-wide (rig audit --fix writes one)",
		},
		{
			name: "declared in every locking module",
			root: `{ :rig/modules ["modules/app"] :deps {org.clojure/clojure "1.11.0"} }`,
			app:  `{ :deps {org.clojure/clojure "1.11.0"} }`,
			want: "",
		},
		{
			name: "declared nowhere",
			root: `{ :rig/modules ["modules/app"] }`,
			app:  `{ :deps {} }`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			doc := lockfile.ForTest(".", "modules/app")
			if err := doc.Save("deps.lock"); err != nil {
				t.Fatal(err)
			}
			writeFile(t, "deps.edn", tc.root+"\n")
			writeFile(t, filepath.Join("modules", "app", "deps.edn"), tc.app+"\n")
			root, err := workspace.Find(".")
			if err != nil {
				t.Fatal(err)
			}
			st := auditPinStateOf(root, root.Lock)
			if got := st.auditNote("org.clojure/clojure", "1.11.0"); got != tc.want {
				t.Errorf("note = %q, want %q", got, tc.want)
			}
		})
	}
}

// The hot fixture's root manifest gains a :rig/deps pool pin of the fix,
// the lock still carrying the vulnerable version: the finding's note
// names the re-lock as its remedy. Bare audit never re-locks — the note
// is guidance, and it rides on the JSON artifact group too.
func TestAuditStalePoolNote(t *testing.T) {
	hotSetup(t)
	mutateFile(t, "deps.edn", func(s string) string {
		return strings.Replace(s, ":rig/modules",
			`:rig/deps {org.clojure/core.rrb-vector "0.1.2"} :rig/modules`, 1)
	})
	fix := osvFixture{
		batch:  map[string][]string{"org.clojure:core.rrb-vector@0.0.14": {"GHSA-rrb-1234-5678"}},
		detail: map[string]string{"GHSA-rrb-1234-5678": rrbFixRecord},
	}
	t.Setenv("RIG_OSV_API", osvServer(t, fix).URL)
	code, out := runCLI(t, "audit", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
	for _, want := range []string{
		"org.clojure/core.rrb-vector 0.0.14",
		"note: the :rig/deps pool pins 0.1.2; the lock still carries 0.0.14 — rig lock picks up the pool pin",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q; out: %s", want, out)
		}
	}
	code, out = runCLI(t, "audit", "--json", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("--json exit = %d, want 1; out: %s", code, out)
	}
	report := auditJSONDoc(t, out)
	if len(report.Findings) != 1 {
		t.Fatalf("findings = %+v, want one artifact group", report.Findings)
	}
	a := report.Findings[0]
	if a.Coordinate != "org.clojure/core.rrb-vector" || a.Version != "0.0.14" {
		t.Errorf("artifact = %+v", a)
	}
	if a.Note == nil || *a.Note != "the :rig/deps pool pins 0.1.2; the lock still carries 0.0.14 — rig lock picks up the pool pin" {
		t.Errorf("note = %v, want the stale pool note", a.Note)
	}
}
