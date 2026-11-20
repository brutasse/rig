package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/cvss"
	"github.com/brutasse/rig/internal/kernel"
	"github.com/brutasse/rig/internal/lockfile"
	"github.com/brutasse/rig/internal/osv"
	"github.com/brutasse/rig/internal/workspace"
)

// osvAPI is the OSV.dev API base for vulnerability lookups
// (overridable in tests, the same convention as the updates API).
func osvAPI() string {
	if v := os.Getenv("RIG_OSV_API"); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return osv.DefaultAPI
}

// publicRepo reports whether a lock's repository id names a public
// repository: maven central or clojars. An override keeps the id and
// changes only the url, so it counts as the real one. Every other id is
// a :mvn/repos entry, a private repository: its artifacts are not
// audited.
func publicRepo(id string) bool {
	return id == "central" || id == "clojars"
}

// auditFinding is one known vulnerability of one pinned artifact, with
// the lock's scopes (module or module:alias classpaths) the artifact
// sits in.
type auditFinding struct {
	coord   string
	version string
	scopes  []string
	vuln    *osv.Vulnerability
}

// scopeIndex maps each artifact id to the scopes its classpath
// references appear in: a module's base classpath is the scope "module",
// each alias's classpath the scope "module:alias".
func scopeIndex(doc *lockfile.Document) map[string][]string {
	sets := make(map[string]map[string]bool)
	add := func(id, scope string) {
		s, ok := sets[id]
		if !ok {
			s = make(map[string]bool)
			sets[id] = s
		}
		s[scope] = true
	}
	for mod, m := range doc.Modules {
		for _, e := range m.Classpath {
			if e.Ref != nil {
				add(*e.Ref, mod)
			}
		}
		for name, a := range m.Aliases {
			for _, e := range a.Classpath {
				if e.Ref != nil {
					add(*e.Ref, mod+":"+name)
				}
			}
		}
	}
	out := make(map[string][]string, len(sets))
	for id, s := range sets {
		scopes := make([]string, 0, len(s))
		for x := range s {
			scopes = append(scopes, x)
		}
		sort.Strings(scopes)
		out[id] = scopes
	}
	return out
}

// scopesOf is the union of the scopes of every artifact of one
// coordinate (the coordinate groups the artifact's extensions and
// classifiers).
func scopesOf(ids []string, idx map[string][]string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, id := range ids {
		for _, s := range idx[id] {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}

func newAuditCmd(o *opts) *cobra.Command {
	var jsonOut, explain, fix, breaking bool
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Scan the lock for known vulnerabilities (OSV)",
		Long: `Scans every maven artifact pinned in the workspace lock against the OSV
vulnerability database (osv.dev) and lists each known vulnerability of a
pinned version, in the scopes (modules and alias classpaths) the artifact
is locked into. Git deps are pinned by commit sha and OSV cannot look
them up; they are skipped and counted. Artifacts from private
repositories (the :mvn/repos entries other than central and clojars)
are skipped and counted the same way; an overridden central or clojars
keeps its name and is scanned as the real one.

Severity is the OSV score when the record carries one, the CVSS base
score computed from the record's vector string otherwise, with its
bucket (Critical / High / Medium / Low). Fix versions are shown per
series: the fixes in the locked version's own series first, the other
series' fixes marked as such.

A finding whose pin the report can see carries a note line: the root's
:rig/overrides force pin or :rig/deps pool entry that pins it — at the
vulnerable version (bump the entry) or at a clean version the lock has
not picked up (a plain re-lock) — or a coordinate some module pins
while other modules pull it undeclared, the pull only a force pin
reaches.

audit reads the lock only: it never re-locks, never downloads artifacts,
and needs no JVM. Transient OSV failures (a network error, a 5xx or
429) are retried a few times; a scan that still fails exits 4, and a
vulnerability record that cannot be fetched is noted, its artifact
counted, and the rest of the report stands. --explain adds each
finding's puller chains (which direct dep pulled the artifact in, per
scope); the chains come from the kernel, which needs a JVM and the
locked artifacts in the local maven repository, and its failures never
change the scan's exit code.

--fix applies the fixable findings' fixes: each vulnerable coordinate's
fix is the newest listed one at or above the locked version in the
locked version's own major (with --breaking, fixes in a higher major
join). A pin a module manifest already carries is updated in place, and
a root :rig/deps pool or :rig/overrides entry that pins the vulnerable
version is updated the same way. A coordinate no module declares — the
pure transitive pull — gets a force pin written to the root's
:rig/overrides (the security-pin section, fed to resolution as
override-deps, beating :rig/deps on the same coordinate). The workspace
re-locks once, and the fix is verified by re-querying OSV for every
locked version of the original findings' coordinates. When a fix cannot
be applied (no fix listed, or only a higher major without --breaking, or
only a version below the locked one, or a fix version no repository
serves) the finding is reported as unfixable and stands; a coordinate a
manifest already pins at a clean version stands stale — its remedy is a
plain re-lock, not an edit. --fix needs the kernel and re-locks: on any
failure after the manifests are edited, they are restored byte-for-byte
and the lock is left as it was.

Exit codes: 0 the lock is clean (or every finding is fixed and
verified); 1 known vulnerabilities (or findings remain after --fix);
2 usage; 3 no lock; 4 the scan or the verification failed or could not
complete; 5 a requirement was refused by cooldowns (--fix's explicit
pins are cooldown-exempt, a refusal means another floating requirement
of the workspace). --offline refuses (the scan needs the network).
--json prints the report as a JSON document on stdout instead of the
human-readable report (with --fix, a "fix" section).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if fix {
				return runAuditFix(cmd.Context(), o, jsonOut, explain, breaking)
			}
			return runAudit(cmd.Context(), o, jsonOut, explain)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false,
		"print the report as a JSON document (stdout), the human-readable report is suppressed")
	cmd.Flags().BoolVar(&explain, "explain", false,
		"for each finding, list the chains of direct deps that pull the artifact in (needs the kernel)")
	cmd.Flags().BoolVar(&fix, "fix", false,
		"apply the fixable findings' fixes, re-lock once, and verify by re-querying OSV (needs the kernel)")
	cmd.Flags().BoolVar(&breaking, "breaking", false,
		"with --fix, accept fixes in a higher major than the locked version")
	return cmd
}

// auditReport is the --json document: the scanned coordinates, the
// skipped ones, the findings grouped per artifact, the count of
// artifacts a vulnerability record could not be fetched for (absent
// when the scan completed), and, with --fix, the fix section.
type auditReport struct {
	Workspace string          `json:"workspace"`
	Scanned   int             `json:"scanned"`
	Skipped   auditSkipped    `json:"skipped"`
	Findings  []auditArtifact `json:"findings"`
	Failed    int             `json:"failed,omitempty"`
	Fix       *auditFix       `json:"fix,omitempty"`
}

type auditSkipped struct {
	Private int `json:"private"`
	Git     int `json:"git"`
}

type auditArtifact struct {
	Coordinate string         `json:"coordinate"`
	Version    string         `json:"version"`
	Scopes     []string       `json:"scopes"`
	Vulns      []auditVulnRow `json:"vulnerabilities"`
	Chains     []auditChain   `json:"chains,omitempty"`
	Note       *string        `json:"note,omitempty"`
}

// auditChain is one puller chain of a finding (--explain): the scope
// (module or module:alias) and the ordered display ids from a direct
// dep down to the artifact.
type auditChain struct {
	Scope string   `json:"scope"`
	Path  []string `json:"path"`
}

type auditVulnRow struct {
	ID          string         `json:"id"`
	Aliases     []string       `json:"aliases"`
	Summary     string         `json:"summary"`
	Published   string         `json:"published"`
	Severity    *auditSeverity `json:"severity"`
	Fixed       []string       `json:"fixed"`
	FixedSeries []string       `json:"fixedSeries,omitempty"`
}

// auditSeverity is the vuln's severity: the OSV score when the record
// carries a numeric one, the CVSS base score computed from its vector
// string otherwise; score and bucket are null when neither exists.
type auditSeverity struct {
	Type   string   `json:"type"`
	Score  *float64 `json:"score"`
	Bucket *string  `json:"bucket"`
	Vector *string  `json:"vector"`
}

func severityRow(s osv.Severity) *auditSeverity {
	out := &auditSeverity{Type: s.Type}
	if f, err := strconv.ParseFloat(s.Score, 64); err == nil {
		bucket := cvss.Bucket(f)
		out.Score = &f
		out.Bucket = &bucket
		return out
	}
	if s.Score != "" {
		out.Vector = &s.Score
		if sc, err := cvss.BaseScore(s.Score); err == nil {
			bucket := cvss.Bucket(sc.Base)
			out.Score = &sc.Base
			out.Bucket = &bucket
		}
	}
	return out
}

// auditBackoffs is the wait between audit attempts: a transient OSV
// failure (see auditTransient) is retried once per entry, so a scan
// makes 1+len(auditBackoffs) attempts. Tests zero it.
var auditBackoffs = []time.Duration{time.Second, 2 * time.Second}

// auditTransient reports whether an OSV error is worth a retry: a
// network error (no status came back) or a server-side one (5xx, 429).
// The other statuses are permanent: a 404 is an id the API does not
// know, and no retry changes it.
func auditTransient(err error) bool {
	var se *osv.StatusError
	if errors.As(err, &se) {
		return se.Status == http.StatusTooManyRequests || se.Status >= 500
	}
	return true
}

// auditRetry runs fn until it succeeds, a permanent error comes back,
// or the attempts are spent; transient errors are followed by a wait
// from auditBackoffs. The last error is returned when all attempts
// fail.
func auditRetry(ctx context.Context, fn func() error) error {
	var err error
	for i := 0; ; i++ {
		if i > 0 {
			select {
			case <-time.After(auditBackoffs[i-1]):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err = fn(); err == nil {
			return nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if !auditTransient(err) || i == len(auditBackoffs) {
			return err
		}
	}
}

// auditCoord is one (coordinate, locked version) pair pinned in the
// lock.
type auditCoord struct{ name, version string }

// scanAudit is the OSV scan shared by runAudit and runAuditFix: the
// pinned public maven coordinates are queried in one batch, each
// distinct vulnerability id's record is fetched once, and the findings
// are collected. It fills report (scanned, skipped) and returns the
// findings and the count of artifacts a vulnerability record could not
// be fetched for. A batch that fails after the retries is a hard error
// (exit 4, distinct from "has findings").
func scanAudit(ctx context.Context, o *opts, root *workspace.Root, report *auditReport) ([]auditFinding, int, error) {
	// One query per pinned maven coordinate: the same coordinate can be
	// locked under several extensions or classifiers, and it is one
	// version either way. Artifacts from private repositories are
	// skipped before the query (counted), like git deps.
	seen := make(map[auditCoord]bool)
	var coords []auditCoord
	git, priv := 0, 0
	idsByCoord := make(map[auditCoord][]string)
	for _, a := range root.Lock.Artifacts {
		c := auditCoord{a.Group + "/" + a.Name, a.Version}
		switch a.Kind {
		case "mvn":
			if !publicRepo(a.Repository) {
				priv++
				continue
			}
			idsByCoord[c] = append(idsByCoord[c], a.ID)
			if !seen[c] {
				seen[c] = true
				coords = append(coords, c)
			}
		case "git":
			git++
		}
	}
	report.Scanned = len(coords)
	report.Skipped = auditSkipped{Private: priv, Git: git}
	scopesOfArtifact := scopeIndex(root.Lock)
	queries := make([]osv.Query, len(coords))
	for i, c := range coords {
		queries[i] = osv.Query{Package: osv.Pkg{Name: strings.ReplaceAll(c.name, "/", ":"), Ecosystem: "Maven"}, Version: c.version}
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	// The batch is the scan: when it fails after the retries, the scan
	// failed, full stop (exit 4, distinct from "has findings").
	var perQuery [][]string
	if err := auditRetry(ctx, func() error {
		var e error
		perQuery, e = osv.QueryBatch(ctx, client, osvAPI(), queries)
		return e
	}); err != nil {
		return nil, 0, exitf(4, "audit: scan failed: %v", err)
	}

	// The batch carries ids only: fetch each distinct id's full record
	// once, whatever artifacts it affects. A record that cannot be
	// fetched (permanent error, or a transient one that outlived the
	// retries) is skipped: the affected artifact is counted, the reason
	// noted on stderr, and the rest of the report stands.
	records := make(map[string]*osv.Vulnerability)
	failedIDs := make(map[string]bool)
	failedArtifacts := make(map[auditCoord]bool)
	var failedNotes []string
	var findings []auditFinding
	for i, ids := range perQuery {
		for _, id := range ids {
			if failedIDs[id] {
				failedArtifacts[coords[i]] = true
				continue
			}
			v, ok := records[id]
			if !ok {
				var rec *osv.Vulnerability
				err := auditRetry(ctx, func() error {
					var e error
					rec, e = osv.Get(ctx, client, osvAPI(), id)
					return e
				})
				if err != nil {
					failedIDs[id] = true
					failedArtifacts[coords[i]] = true
					failedNotes = append(failedNotes, fmt.Sprintf("audit: %s: %v", id, err))
					continue
				}
				records[id] = rec
				v = rec
			}
			c := coords[i]
			findings = append(findings, auditFinding{
				coord:   c.name,
				version: c.version,
				scopes:  scopesOf(idsByCoord[c], scopesOfArtifact),
				vuln:    v,
			})
		}
	}
	if len(failedNotes) > 0 {
		for _, n := range failedNotes {
			fmt.Fprintln(os.Stderr, n)
		}
	}
	return findings, len(failedArtifacts), nil
}

// auditNothingPrinted is the scan's "nothing to scan" print (exit 0).
func auditNothingPrinted(report *auditReport, jsonOut bool) error {
	if jsonOut {
		return printReport(report)
	}
	msg := "audit: no maven artifacts to scan"
	if report.Skipped.Private > 0 {
		msg += fmt.Sprintf(" (%d from private repositories, not audited", report.Skipped.Private)
		if report.Skipped.Git > 0 {
			msg += fmt.Sprintf(", %d git dep%s skipped, pinned by commit sha", report.Skipped.Git, plural(report.Skipped.Git))
		}
		msg += ")"
	} else if report.Skipped.Git > 0 {
		msg += fmt.Sprintf(" (%d git dep%s skipped, pinned by commit sha)", report.Skipped.Git, plural(report.Skipped.Git))
	}
	fmt.Println(msg)
	return nil
}

// auditCleanPrinted is the scan's "no findings" print: exit 4 when the
// scan could not fetch every record, exit 0 when the lock is clean.
func auditCleanPrinted(report *auditReport, jsonOut bool, failedN int) error {
	if failedN > 0 {
		report.Failed = failedN
		if jsonOut {
			if err := printReport(report); err != nil {
				return err
			}
		}
		return exitf(4, "audit: scan incomplete: %d artifact record(s) could not be fetched", failedN)
	}
	if jsonOut {
		return printReport(report)
	}
	msg := fmt.Sprintf("audit: no known vulnerabilities (%d maven artifact%s", report.Scanned, plural(report.Scanned))
	if report.Skipped.Private > 0 {
		msg += fmt.Sprintf(", %d from private repositories, not audited", report.Skipped.Private)
	}
	if report.Skipped.Git > 0 {
		msg += fmt.Sprintf(", %d git dep%s skipped", report.Skipped.Git, plural(report.Skipped.Git))
	}
	fmt.Println(msg + ")")
	return nil
}

func runAudit(ctx context.Context, o *opts, jsonOut, explain bool) error {
	root, err := workspace.Find(".")
	if err != nil {
		return err
	}
	if root.Lock == nil {
		return exitf(3, "no lock at %s (run 'rig lock')", root.LockPath())
	}
	if o.offline {
		return exitf(1, "offline: 'rig audit' needs the network")
	}

	report := auditReport{
		Workspace: root.Dir,
		Findings:  []auditArtifact{},
	}
	findings, failedN, err := scanAudit(ctx, o, root, &report)
	if err != nil {
		return err
	}
	if report.Scanned == 0 {
		return auditNothingPrinted(&report, jsonOut)
	}
	if len(findings) == 0 {
		return auditCleanPrinted(&report, jsonOut, failedN)
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].coord != findings[j].coord {
			return findings[i].coord < findings[j].coord
		}
		if findings[i].version != findings[j].version {
			return findings[i].version < findings[j].version
		}
		return findings[i].vuln.ID < findings[j].vuln.ID
	})
	affected := make(map[string]bool)
	for _, f := range findings {
		affected[f.coord+"/"+f.version] = true
	}
	// The pin state the finding notes derive from: the manifests and
	// the lock, as scanned.
	state := auditPinStateOf(root, root.Lock)
	// The puller chains, one kernel call per scope the findings sit in;
	// best effort, its failures only cost the "pulled in" lines.
	var chainsByScope map[string]map[string][][]string
	if explain {
		chainsByScope = o.explainFindings(ctx, root, findings)
	}
	if jsonOut {
		groupFindings(&report, findings, chainsByScope, state)
		report.Failed = failedN
		if err := printReport(&report); err != nil {
			return err
		}
	} else {
		fmt.Printf("audit: %d known vulnerabilities in %d of %d pinned artifacts\n\n",
			len(findings), len(affected), report.Scanned)
		printFindingsHuman(findings, chainsByScope, state)
	}
	note := ""
	if failedN > 0 {
		note = fmt.Sprintf("; %d artifact record(s) could not be fetched", failedN)
	}
	return exitf(1, "audit: %d known vulnerabilities in %d artifact(s)%s", len(findings), len(affected), note)
}

// vulnRow is one finding's --json vulnerability entry.
func vulnRow(version string, v *osv.Vulnerability) auditVulnRow {
	row := auditVulnRow{
		ID:        v.ID,
		Aliases:   []string{},
		Summary:   v.Summary,
		Published: v.Published,
		Severity:  nil,
		Fixed:     []string{},
	}
	row.Aliases = append(row.Aliases, v.Aliases...)
	if len(v.Severity) > 0 {
		row.Severity = severityRow(v.Severity[0])
	}
	row.Fixed = append(row.Fixed, osv.FixedVersions(v)...)
	row.FixedSeries = fixedInSeries(version, row.Fixed)
	return row
}

// printReport writes the --json document to stdout.
func printReport(r *auditReport) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// aliasesOf renders the vuln's aliases (the CVE ids, usually) as " (…)".
func aliasesOf(v *osv.Vulnerability) string {
	if len(v.Aliases) == 0 {
		return ""
	}
	return " (" + strings.Join(v.Aliases, ", ") + ")"
}

// metaOf renders the one-line fact row of a finding: publication date,
// severity, and the fix (or its absence).
func metaOf(version string, v *osv.Vulnerability) string {
	var parts []string
	if d := publishedDate(v.Published); d != "" {
		parts = append(parts, "published "+d)
	}
	if s := severityOf(v); s != "" {
		parts = append(parts, s)
	}
	parts = append(parts, fixText(version, v))
	return strings.Join(parts, " · ")
}

// seriesOf is a version's series: its first two dot components
// ("2.18.6" -> "2.18", "0.2.62" -> "0.2"); a one-component version is
// its own series.
func seriesOf(v string) string {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) == 1 {
		return v
	}
	return parts[0] + "." + parts[1]
}

// fixedInSeries keeps the fixed versions in the locked version's
// series: the upgrades an in-series bump can reach.
func fixedInSeries(version string, fixed []string) []string {
	s := seriesOf(version)
	var out []string
	for _, f := range fixed {
		if seriesOf(f) == s {
			out = append(out, f)
		}
	}
	return out
}

// fixText renders the fix fact: the fixes in the locked version's own
// series first, the other series' fixes marked as such.
func fixText(version string, v *osv.Vulnerability) string {
	fixed := osv.FixedVersions(v)
	if len(fixed) == 0 {
		return "no fix listed"
	}
	own := fixedInSeries(version, fixed)
	var others []string
	for _, f := range fixed {
		if !inList(own, f) {
			others = append(others, f)
		}
	}
	switch {
	case len(others) == 0:
		return "fixed in " + strings.Join(own, ", ")
	case len(own) == 0:
		return "fixed in " + strings.Join(others, ", ") + " (other series)"
	default:
		return "fixed in " + strings.Join(own, ", ") + " · other series: " + strings.Join(others, ", ")
	}
}

func inList(list []string, x string) bool {
	for _, v := range list {
		if v == x {
			return true
		}
	}
	return false
}

// explainFindings asks the kernel for the puller chains of the
// findings' artifacts, one call per scope the findings sit in (the
// targets sorted for determinism). Best effort: a scope the kernel
// cannot walk prints a note on stderr and its findings render without
// chains.
func (o *opts) explainFindings(ctx context.Context, root *workspace.Root, findings []auditFinding) map[string]map[string][][]string {
	// scope -> the artifact display ids (group/name:version) to explain.
	byScope := make(map[string]map[string]bool)
	for _, f := range findings {
		id := f.coord + ":" + f.version
		for _, s := range f.scopes {
			m := byScope[s]
			if m == nil {
				m = make(map[string]bool)
				byScope[s] = m
			}
			m[id] = true
		}
	}
	if len(byScope) == 0 {
		return nil
	}
	store, err := o.store()
	if err != nil {
		return o.noteExplain(err)
	}
	jar, err := kernel.Kernel(store)
	if err != nil {
		return o.noteExplain(err)
	}
	java, _, err := o.pickJava(ctx, store, root)
	if err != nil {
		return o.noteExplain(err)
	}
	env, err := o.kernelEnv(ctx, root)
	if err != nil {
		return o.noteExplain(err)
	}
	penv, err := o.kernelProxy(ctx, root)
	if err != nil {
		return o.noteExplain(err)
	}
	defer o.closeProxy()
	out := make(map[string]map[string][][]string)
	scopes := make([]string, 0, len(byScope))
	for s := range byScope {
		scopes = append(scopes, s)
	}
	sort.Strings(scopes)
	for _, scope := range scopes {
		targets := make([]string, 0, len(byScope[scope]))
		for id := range byScope[scope] {
			targets = append(targets, id)
		}
		sort.Strings(targets)
		module, alias := splitScope(scope)
		chains, err := o.kernelExplain(ctx, root, jar, java, module, alias, targets, env, penv)
		if err != nil {
			fmt.Fprintf(os.Stderr, "audit: explain: %v (scope %s)\n", err, scope)
			continue
		}
		if len(chains) > 0 {
			out[scope] = chains
		}
	}
	return out
}

// noteExplain reports a plumbing failure of --explain (best effort, the
// scan's exit code is untouched).
func (o *opts) noteExplain(err error) map[string]map[string][][]string {
	fmt.Fprintf(os.Stderr, "audit: explain: %v\n", err)
	return nil
}

// kernelExplain is one kernel explain call: the puller chains of
// `targets` (display ids, group/name:version) in one scope, module or
// module:alias. The plumbing (store, jar, java, env) is done by the
// caller, which serves every scope with one kernel setup and one proxy.
func (o *opts) kernelExplain(ctx context.Context, root *workspace.Root, jar, java, module, alias string, targets []string, env, penv []string) (map[string][][]string, error) {
	out, err := kernel.Call(ctx, jar, java, kernel.Request{
		Op:        "explain",
		Workspace: root.Dir,
		Lock:      "deps.lock",
		Args:      map[string]any{"module": module, "alias": alias, "targets": targets, "offline": o.offline},
	}, append(env, penv...)...)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Chains map[string][][]string `json:"chains"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("bad kernel response: %w", err)
	}
	return parsed.Chains, nil
}

// splitScope splits a scope into its module and alias parts (a bare
// module has an empty alias).
func splitScope(scope string) (string, string) {
	m, a, _ := strings.Cut(scope, ":")
	return m, a
}

// pulledInLines is an artifact's "pulled in" lines, one per chain,
// "<scope>: a → b → artifact", the scopes in the finding's order.
func pulledInLines(coord, version string, scopes []string, byScope map[string]map[string][][]string) []string {
	id := coord + ":" + version
	var out []string
	for _, s := range scopes {
		for _, c := range byScope[s][id] {
			out = append(out, s+": "+strings.Join(c, " → "))
		}
	}
	return out
}

// chainsFor is an artifact's auditChains, one per chain per scope it
// sits in (only the scopes the kernel answered).
func chainsFor(coord, version string, scopes []string, byScope map[string]map[string][][]string) []auditChain {
	id := coord + ":" + version
	var out []auditChain
	for _, s := range scopes {
		for _, c := range byScope[s][id] {
			out = append(out, auditChain{Scope: s, Path: c})
		}
	}
	return out
}

// severityOf renders the vuln's severity as "severity <type> <score>
// <bucket>". OSV sometimes gives a numeric score ("9.8") and sometimes
// the CVSS vector string itself ("CVSS:3.1/AV:N/…"): the vector's base
// score is computed in-process. A vector that cannot be scored leaves
// the type alone.
func severityOf(v *osv.Vulnerability) string {
	if len(v.Severity) == 0 {
		return ""
	}
	s := v.Severity[0]
	if f, err := strconv.ParseFloat(s.Score, 64); err == nil {
		return "severity " + s.Type + " " + s.Score + " " + cvss.Bucket(f)
	}
	if s.Score != "" {
		if sc, err := cvss.BaseScore(s.Score); err == nil {
			return "severity " + s.Type + " " + fmt.Sprintf("%.1f", sc.Base) + " " + cvss.Bucket(sc.Base)
		}
	}
	return "severity " + s.Type
}

// publishedDate reduces an OSV timestamp to its date.
func publishedDate(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return ""
}

// plural returns "s" when the count is not 1, for message wording.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
