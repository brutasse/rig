package cli

// rig audit --fix end to end: the hot fixture (pre-locked against the
// seeded fixture repo) plus a fake OSV API. The re-lock resolves
// entirely from the seeded repo: the fix targets (org.clojure/clojure
// 1.12.5, mvxcvi/arrangement 2.0.0) are seeded, and their closures stay
// inside the fixture's pins (the new dependencies are test-scope in the
// poms, never fetched).

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/brutasse/rig/internal/lockfile"
)

// fixRecord: a finding whose fix sits in the locked version's own
// major (org.clojure/clojure 1.11.0 → 1.12.5, seeded).
const fixRecord = `{
	"id": "GHSA-fix-aaaa-bbbb",
	"summary": "Insecure deserialization in clojure.reader",
	"aliases": ["CVE-2026-0001"],
	"severity": [{"type": "CVSS_V3", "score": "9.8"}],
	"published": "2026-01-02T03:04:05Z",
	"affected": [{"ranges": [{"type": "ECOSYSTEM", "events": [{"introduced": "0"}, {"fixed": "1.12.5"}]}]}]
}`

// breakingRecord: a finding whose only fix is in a higher major
// (mvxcvi/arrangement 1.2.0 → 2.0.0, seeded).
const breakingRecord = `{
	"id": "GHSA-brk-cccc-dddd",
	"summary": "Command injection in arrangement.cli",
	"aliases": ["CVE-2026-0002"],
	"severity": [{"type": "CVSS_V3", "score": "9.8"}],
	"published": "2026-02-02T03:04:05Z",
	"affected": [{"ranges": [{"type": "ECOSYSTEM", "events": [{"introduced": "0"}, {"fixed": "2.0.0"}]}]}]
}`

// noFixRecord: a finding with no fix listed.
const noFixRecord = `{
	"id": "GHSA-nof-eeee-ffff",
	"summary": "Backport-only advisory",
	"severity": [{"type": "CVSS_V3", "score": "5.5"}],
	"published": "2026-03-02T03:04:05Z",
	"affected": [{"ranges": [{"type": "ECOSYSTEM", "events": [{"introduced": "0"}]}]}]
}`

// unservedFixRecord: a finding whose fix (org.clojure/clojure 1.11.0 →
// 1.12.99) is in the locked version's own major but never published —
// the seeded repo's clojure stops at 1.12.5.
const unservedFixRecord = `{
	"id": "GHSA-unv-1111-2222",
	"summary": "Parsing bypass in clojure.pprint",
	"aliases": ["CVE-2026-0003"],
	"severity": [{"type": "CVSS_V3", "score": "7.5"}],
	"published": "2026-04-02T03:04:05Z",
	"affected": [{"ranges": [{"type": "ECOSYSTEM", "events": [{"introduced": "0"}, {"fixed": "1.12.99"}]}]}]
}`

// lockVersion returns the version doc locks for group/name; it fails the
// test when the coordinate is absent or locks more than one version.
func lockVersion(t *testing.T, doc *lockfile.Document, group, name string) string {
	t.Helper()
	var out []string
	for _, a := range doc.Artifacts {
		if a.Kind == "mvn" && a.Group == group && a.Name == name {
			out = append(out, a.Version)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s/%s not in the lock (%d artifacts)", group, name, len(doc.Artifacts))
	}
	for _, v := range out[1:] {
		if v != out[0] {
			t.Fatalf("%s/%s locks %v, want one version", group, name, out)
		}
	}
	return out[0]
}

// A fixable direct dependency: the module pin moves, the workspace
// re-locks once, and the verification re-queries the new version and
// finds the vuln gone. Exit 0.
func TestAuditFixDirectDep(t *testing.T) {
	hotSetup(t)
	fix := osvFixture{
		batch:  map[string][]string{"org.clojure:clojure@1.11.0": {"GHSA-fix-aaaa-bbbb"}},
		detail: map[string]string{"GHSA-fix-aaaa-bbbb": fixRecord},
	}
	t.Setenv("RIG_OSV_API", osvServer(t, fix).URL)
	code, out := runCLI(t, "audit", "--fix", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("exit = %d, want 0; out: %s", code, out)
	}
	for _, want := range []string{
		"fix: org.clojure/clojure 1.11.0 → 1.12.5",
		"modules/app/deps.edn: set org.clojure/clojure \"1.12.5\" (deps)",
		"fixes: GHSA-fix-aaaa-bbbb (CVE-2026-0001)",
		"deps.lock: 26 artifacts, 2 modules",
		"verified: 1 fixed, 0 remaining",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q; out: %s", want, out)
		}
	}
	app, err := os.ReadFile("modules/app/deps.edn")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(app), "1.12.5") || strings.Contains(string(app), "1.11.0") {
		t.Errorf("module pin not updated; manifest: %s", app)
	}
	lock, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	if v := lockVersion(t, lock, "org.clojure", "clojure"); v != "1.12.5" {
		t.Errorf("lock clojure = %s, want 1.12.5", v)
	}
	// The re-lock re-resolves the fixed dep's closure: clojure 1.12.5
	// pulls spec.alpha 0.5.238, not the 0.3.218 the old pin did.
	if v := lockVersion(t, lock, "org.clojure", "spec.alpha"); v != "0.5.238" {
		t.Errorf("lock spec.alpha = %s, want 0.5.238", v)
	}
}

// A finding whose only fix is in a higher major stands without
// --breaking: the plan names the reason, nothing is applied, the lock
// and the manifests are untouched. Exit 1.
func TestAuditFixBreakingGate(t *testing.T) {
	hotSetup(t)
	orig := map[string][]byte{}
	for _, f := range []string{"deps.lock", "deps.edn", "modules/app/deps.edn"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		orig[f] = b
	}
	fix := osvFixture{
		batch:  map[string][]string{"mvxcvi:arrangement@1.2.0": {"GHSA-brk-cccc-dddd"}},
		detail: map[string]string{"GHSA-brk-cccc-dddd": breakingRecord},
	}
	t.Setenv("RIG_OSV_API", osvServer(t, fix).URL)
	code, out := runCLI(t, "audit", "--fix", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
	for _, want := range []string{
		"unfixable: mvxcvi/arrangement 1.2.0: fixes only in a higher major (use --breaking)",
		"audit: nothing to fix",
		"audit: 1 finding(s) remain (none fixable)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q; out: %s", want, out)
		}
	}
	for f, b := range orig {
		got, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, b) {
			t.Errorf("%s changed; before: %s; after: %s", f, b, got)
		}
	}
}

// With --breaking the higher-major fix is a candidate: arrangement is
// a pure transitive pull (kaocha's), so the fix is written as a
// :rig/overrides force pin at the root (a new section, seeded as a
// bare version string), and the re-lock force-pins 2.0.0. Exit 0.
func TestAuditFixBreakingApplied(t *testing.T) {
	hotSetup(t)
	fix := osvFixture{
		batch:  map[string][]string{"mvxcvi:arrangement@1.2.0": {"GHSA-brk-cccc-dddd"}},
		detail: map[string]string{"GHSA-brk-cccc-dddd": breakingRecord},
	}
	t.Setenv("RIG_OSV_API", osvServer(t, fix).URL)
	code, out := runCLI(t, "audit", "--fix", "--breaking", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("exit = %d, want 0; out: %s", code, out)
	}
	for _, want := range []string{
		"fix: mvxcvi/arrangement 1.2.0 → 2.0.0",
		"overrides: add mvxcvi/arrangement \"2.0.0\"",
		"fixes: GHSA-brk-cccc-dddd (CVE-2026-0002)",
		"deps.lock: 26 artifacts, 2 modules",
		"verified: 1 fixed, 0 remaining",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q; out: %s", want, out)
		}
	}
	root, err := os.ReadFile("deps.edn")
	if err != nil {
		t.Fatal(err)
	}
	bare := regexp.MustCompile(`mvxcvi/arrangement\s+"2\.0\.0"`)
	if !strings.Contains(string(root), ":rig/overrides") || !bare.MatchString(string(root)) {
		t.Errorf("root :rig/overrides pin missing or not a bare version string; manifest: %s", root)
	}
	if !strings.Contains(string(root), "\n :rig/overrides") {
		t.Errorf("section key not on its own line; manifest: %s", root)
	}
	if strings.Contains(string(root), ":rig/deps") || strings.Contains(string(root), ":mvn/version") {
		t.Errorf("pool created or entry seeded as a map; manifest: %s", root)
	}
	lock, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	if v := lockVersion(t, lock, "mvxcvi", "arrangement"); v != "2.0.0" {
		t.Errorf("lock arrangement = %s, want 2.0.0", v)
	}
	// The declaring module's own pin stands: kaocha is untouched.
	if v := lockVersion(t, lock, "lambdaisland", "kaocha"); v != "1.66.1034" {
		t.Errorf("lock kaocha = %s, want 1.66.1034", v)
	}
}

// rrbFixRecord: a finding on a pure transitive of the fixture (the
// :test alias pulls org.clojure/core.rrb-vector 0.0.14 undeclared),
// fixed in the locked version's own major (0.1.2, seeded).
const rrbFixRecord = `{
	"id": "GHSA-rrb-1234-5678",
	"summary": "Excessive memory retention in core.rrb-vector",
	"aliases": ["CVE-2026-0004"],
	"severity": [{"type": "CVSS_V3", "score": "5.3"}],
	"published": "2026-05-02T03:04:05Z",
	"affected": [{"ranges": [{"type": "ECOSYSTEM", "events": [{"introduced": "0"}, {"fixed": "0.1.2"}]}]}]
}`

// A fix on a pure transitive pull (the :test alias pulls
// org.clojure/core.rrb-vector 0.0.14 undeclared) is written as a
// :rig/overrides force pin: the section is created at the root (seeded
// as a bare version string), the re-lock force-pins 0.1.2, and the
// verification re-queries it clean. Exit 0.
func TestAuditFixOverridePinWritten(t *testing.T) {
	hotSetup(t)
	fix := osvFixture{
		batch:  map[string][]string{"org.clojure:core.rrb-vector@0.0.14": {"GHSA-rrb-1234-5678"}},
		detail: map[string]string{"GHSA-rrb-1234-5678": rrbFixRecord},
	}
	t.Setenv("RIG_OSV_API", osvServer(t, fix).URL)
	code, out := runCLI(t, "audit", "--fix", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("exit = %d, want 0; out: %s", code, out)
	}
	for _, want := range []string{
		"fix: org.clojure/core.rrb-vector 0.0.14 → 0.1.2",
		"overrides: add org.clojure/core.rrb-vector \"0.1.2\"",
		"fixes: GHSA-rrb-1234-5678 (CVE-2026-0004)",
		"verified: 1 fixed, 0 remaining",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q; out: %s", want, out)
		}
	}
	root, err := os.ReadFile("deps.edn")
	if err != nil {
		t.Fatal(err)
	}
	bare := regexp.MustCompile(`core\.rrb-vector\s+"0\.1\.2"`)
	if !strings.Contains(string(root), ":rig/overrides") || !bare.MatchString(string(root)) {
		t.Errorf("root :rig/overrides pin missing or not a bare version string; manifest: %s", root)
	}
	if !strings.Contains(string(root), "\n :rig/overrides") {
		t.Errorf("section key not on its own line; manifest: %s", root)
	}
	if strings.Contains(string(root), ":rig/deps") || strings.Contains(string(root), ":mvn/version") {
		t.Errorf("pool created or entry seeded as a map; manifest: %s", root)
	}
	lock, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	if v := lockVersion(t, lock, "org.clojure", "core.rrb-vector"); v != "0.1.2" {
		t.Errorf("lock core.rrb-vector = %s, want 0.1.2", v)
	}
}

// --fix composes with --explain and --json: the report gains the fix
// section — the plan row, the applied fix with its edit site, and the
// verification.
func TestAuditFixJSON(t *testing.T) {
	hotSetup(t)
	fix := osvFixture{
		batch:  map[string][]string{"org.clojure:clojure@1.11.0": {"GHSA-fix-aaaa-bbbb"}},
		detail: map[string]string{"GHSA-fix-aaaa-bbbb": fixRecord},
	}
	t.Setenv("RIG_OSV_API", osvServer(t, fix).URL)
	code, out := runCLI(t, "audit", "--fix", "--explain", "--json", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("exit = %d, want 0; out: %s", code, out)
	}
	report := auditJSONDoc(t, out)
	if len(report.Findings) != 1 {
		t.Fatalf("findings = %+v, want one artifact group", report.Findings)
	}
	if report.Fix == nil {
		t.Fatalf("report missing the fix section; report: %+v", report)
	}
	if len(report.Fix.Plan) != 1 {
		t.Fatalf("plan = %+v, want one row", report.Fix.Plan)
	}
	p := report.Fix.Plan[0]
	if p.Coord != "org.clojure/clojure" || p.Locked != "1.11.0" ||
		p.Target == nil || *p.Target != "1.12.5" || p.Reason != nil {
		t.Errorf("plan row = %+v", p)
	}
	if len(report.Fix.Applied) != 1 {
		t.Fatalf("applied = %+v, want one fix", report.Fix.Applied)
	}
	a := report.Fix.Applied[0]
	if a.Coord != "org.clojure/clojure" || a.Target != "1.12.5" || a.Pin != nil {
		t.Errorf("applied = %+v", a)
	}
	if len(a.Sites) != 1 {
		t.Fatalf("sites = %+v, want one", a.Sites)
	}
	s := a.Sites[0]
	if s.File != "modules/app/deps.edn" || s.Module != "modules/app" || s.Section != "deps" ||
		s.Alias != nil || s.From == nil || *s.From != "1.11.0" {
		t.Errorf("site = %+v", s)
	}
	if report.Fix.Verified == nil {
		t.Fatalf("verified = nil, want the re-query result")
	}
	if len(report.Fix.Verified.Fixed) != 1 || report.Fix.Verified.Fixed[0] != "GHSA-fix-aaaa-bbbb" {
		t.Errorf("fixed = %v, want the original finding's vuln id", report.Fix.Verified.Fixed)
	}
	if len(report.Fix.Verified.Remaining) != 0 {
		t.Errorf("remaining = %+v, want none", report.Fix.Verified.Remaining)
	}
}

// Without a lock the fix cannot plan: exit 3, before any OSV call.
func TestAuditFixNoLock(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", "{:deps {}}\n")
	code, out := runCLI(t, "audit", "--fix", "--cache-dir", t.TempDir())
	if code != 3 || !strings.Contains(out, "no lock at") {
		t.Fatalf("exit = %d, want 3; out: %s", code, out)
	}
}

// A record that cannot be fetched may carry fixes: the scan does not
// attempt the fix — exit 4, the lock untouched.
func TestAuditFixScanIncomplete(t *testing.T) {
	auditWorkspace(t)
	orig, err := os.ReadFile("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	fix := osvFixture{
		batch: map[string][]string{"org.clojure:clojure@1.11.0": {"GHSA-missing"}},
	}
	t.Setenv("RIG_OSV_API", osvServer(t, fix).URL)
	code, out := runCLI(t, "audit", "--fix", "--cache-dir", t.TempDir())
	if code != 4 {
		t.Fatalf("exit = %d, want 4; out: %s", code, out)
	}
	if want := "audit: scan incomplete: 1 artifact record(s) could not be fetched"; !strings.Contains(out, want) {
		t.Errorf("out missing %q; out: %s", want, out)
	}
	got, err := os.ReadFile("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, orig) {
		t.Error("deps.lock changed; the fix must not run on an incomplete scan")
	}
}

// A finding with no fix listed stands: the plan names the reason,
// nothing is applied. Exit 1.
func TestAuditFixNoFixStands(t *testing.T) {
	hotSetup(t)
	orig, err := os.ReadFile("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	fix := osvFixture{
		batch:  map[string][]string{"org.clojure:clojure@1.11.0": {"GHSA-nof-eeee-ffff"}},
		detail: map[string]string{"GHSA-nof-eeee-ffff": noFixRecord},
	}
	t.Setenv("RIG_OSV_API", osvServer(t, fix).URL)
	code, out := runCLI(t, "audit", "--fix", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
	for _, want := range []string{
		"unfixable: org.clojure/clojure 1.11.0: no fix listed",
		"audit: 1 finding(s) remain (none fixable)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q; out: %s", want, out)
		}
	}
	got, err := os.ReadFile("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, orig) {
		t.Error("deps.lock changed; nothing fixable, nothing to re-lock")
	}
}

// A fix version the workspace's repositories do not serve stands
// unfixable with a named reason: nothing is applied, the lock stays
// byte-identical, exit 1.
func TestAuditFixUnavailableStands(t *testing.T) {
	hotSetup(t)
	orig, err := os.ReadFile("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	fix := osvFixture{
		batch:  map[string][]string{"org.clojure:clojure@1.11.0": {"GHSA-unv-1111-2222"}},
		detail: map[string]string{"GHSA-unv-1111-2222": unservedFixRecord},
	}
	t.Setenv("RIG_OSV_API", osvServer(t, fix).URL)
	code, out := runCLI(t, "audit", "--fix", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
	for _, want := range []string{
		"unfixable: org.clojure/clojure 1.11.0: fix not available in the workspace's repositories",
		"audit: 1 finding(s) remain (none fixable)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q; out: %s", want, out)
		}
	}
	got, err := os.ReadFile("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, orig) {
		t.Error("deps.lock changed; nothing fixable, nothing to re-lock")
	}
}

// Two findings, one fixable: the fixable one is applied and verified,
// the other stands — exit 1 with the remaining finding named.
func TestAuditFixFindingsRemain(t *testing.T) {
	hotSetup(t)
	fix := osvFixture{
		batch: map[string][]string{
			"org.clojure:clojure@1.11.0": {"GHSA-fix-aaaa-bbbb"},
			"mvxcvi:arrangement@1.2.0":   {"GHSA-brk-cccc-dddd"},
		},
		detail: map[string]string{
			"GHSA-fix-aaaa-bbbb": fixRecord,
			"GHSA-brk-cccc-dddd": breakingRecord,
		},
	}
	t.Setenv("RIG_OSV_API", osvServer(t, fix).URL)
	code, out := runCLI(t, "audit", "--fix", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
	for _, want := range []string{
		"fix: org.clojure/clojure 1.11.0 → 1.12.5",
		"unfixable: mvxcvi/arrangement 1.2.0: fixes only in a higher major (use --breaking)",
		"verified: 1 fixed, 1 remaining",
		"GHSA-brk-cccc-dddd — mvxcvi/arrangement 1.2.0",
		"audit: 1 finding(s) remain after the fix",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q; out: %s", want, out)
		}
	}
	lock, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	if v := lockVersion(t, lock, "org.clojure", "clojure"); v != "1.12.5" {
		t.Errorf("lock clojure = %s, want 1.12.5", v)
	}
	if v := lockVersion(t, lock, "mvxcvi", "arrangement"); v != "1.2.0" {
		t.Errorf("lock arrangement = %s, want 1.2.0 (unfixable stays)", v)
	}
}
