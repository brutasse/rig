package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/cvss"
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
	var jsonOut bool
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
bucket (Critical / High / Medium / Low).

audit reads the lock only: it never re-locks, never downloads artifacts,
and needs no JVM. It exits 1 when any scanned artifact has a known
vulnerability, 0 when the lock is clean. --offline refuses (the scan
needs the network). --json prints the report as a JSON document on
stdout instead of the human-readable report.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAudit(cmd.Context(), o, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false,
		"print the report as a JSON document (stdout), the human-readable report is suppressed")
	return cmd
}

// auditReport is the --json document: the scanned coordinates, the
// skipped ones, and the findings grouped per artifact.
type auditReport struct {
	Workspace string          `json:"workspace"`
	Scanned   int             `json:"scanned"`
	Skipped   auditSkipped    `json:"skipped"`
	Findings  []auditArtifact `json:"findings"`
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
}

type auditVulnRow struct {
	ID        string         `json:"id"`
	Aliases   []string       `json:"aliases"`
	Summary   string         `json:"summary"`
	Published string         `json:"published"`
	Severity  *auditSeverity `json:"severity"`
	Fixed     []string       `json:"fixed"`
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

func runAudit(ctx context.Context, o *opts, jsonOut bool) error {
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

	// One query per pinned maven coordinate: the same coordinate can be
	// locked under several extensions or classifiers, and it is one
	// version either way. Artifacts from private repositories are
	// skipped before the query (counted), like git deps.
	type coord struct{ name, version string }
	seen := make(map[coord]bool)
	var coords []coord
	git, priv := 0, 0
	idsByCoord := make(map[coord][]string)
	for _, a := range root.Lock.Artifacts {
		c := coord{a.Group + "/" + a.Name, a.Version}
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
	scopesOfArtifact := scopeIndex(root.Lock)
	report := auditReport{
		Workspace: root.Dir,
		Scanned:   len(coords),
		Skipped:   auditSkipped{Private: priv, Git: git},
		Findings:  []auditArtifact{},
	}
	if len(coords) == 0 {
		if jsonOut {
			return printReport(&report)
		}
		msg := "audit: no maven artifacts to scan"
		if priv > 0 {
			msg += fmt.Sprintf(" (%d from private repositories, not audited", priv)
			if git > 0 {
				msg += fmt.Sprintf(", %d git dep%s skipped, pinned by commit sha", git, plural(git))
			}
			msg += ")"
		} else if git > 0 {
			msg += fmt.Sprintf(" (%d git dep%s skipped, pinned by commit sha)", git, plural(git))
		}
		fmt.Println(msg)
		return nil
	}

	queries := make([]osv.Query, len(coords))
	for i, c := range coords {
		queries[i] = osv.Query{Package: osv.Pkg{Name: strings.ReplaceAll(c.name, "/", ":"), Ecosystem: "Maven"}, Version: c.version}
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	perQuery, err := osv.QueryBatch(ctx, client, osvAPI(), queries)
	if err != nil {
		return exitf(1, "audit: %v", err)
	}

	// The batch carries ids only: fetch each distinct id's full record
	// once, whatever artifacts it affects.
	records := make(map[string]*osv.Vulnerability)
	var findings []auditFinding
	for i, ids := range perQuery {
		for _, id := range ids {
			v, ok := records[id]
			if !ok {
				v, err = osv.Get(ctx, client, osvAPI(), id)
				if err != nil {
					return exitf(1, "audit: %v", err)
				}
				records[id] = v
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
	if len(findings) == 0 {
		if jsonOut {
			return printReport(&report)
		}
		msg := fmt.Sprintf("audit: no known vulnerabilities (%d maven artifact%s", len(coords), plural(len(coords)))
		if priv > 0 {
			msg += fmt.Sprintf(", %d from private repositories, not audited", priv)
		}
		if git > 0 {
			msg += fmt.Sprintf(", %d git dep%s skipped", git, plural(git))
		}
		fmt.Println(msg + ")")
		return nil
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
	if !jsonOut {
		fmt.Printf("audit: %d known vulnerabilities in %d of %d pinned artifacts\n\n",
			len(findings), len(affected), len(coords))
	}
	prev := ""
	for _, f := range findings {
		if jsonOut {
			// The findings are sorted per artifact: a new coordinate
			// starts a new group, the rest append to it.
			n := len(report.Findings)
			if n == 0 || report.Findings[n-1].Coordinate != f.coord || report.Findings[n-1].Version != f.version {
				report.Findings = append(report.Findings, auditArtifact{
					Coordinate: f.coord,
					Version:    f.version,
					Scopes:     f.scopes,
					Vulns:      []auditVulnRow{},
				})
			}
			last := &report.Findings[len(report.Findings)-1]
			last.Vulns = append(last.Vulns, vulnRow(f.vuln))
			continue
		}
		// The artifact line and its scopes print once per artifact,
		// whatever vulnerabilities it carries.
		if f.coord+" "+f.version != prev {
			prev = f.coord + " " + f.version
			fmt.Printf("%s %s\n", f.coord, f.version)
			if len(f.scopes) > 0 {
				fmt.Printf("  used in %s\n", strings.Join(f.scopes, ", "))
			}
		}
		fmt.Printf("  %s%s — %s\n", f.vuln.ID, aliasesOf(f.vuln), f.vuln.Summary)
		fmt.Printf("    %s\n", metaOf(f.vuln))
	}
	if jsonOut {
		if err := printReport(&report); err != nil {
			return err
		}
	}
	return exitf(1, "audit: %d known vulnerabilities in %d artifact(s)", len(findings), len(affected))
}

// vulnRow is one finding's --json vulnerability entry.
func vulnRow(v *osv.Vulnerability) auditVulnRow {
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
func metaOf(v *osv.Vulnerability) string {
	var parts []string
	if d := publishedDate(v.Published); d != "" {
		parts = append(parts, "published "+d)
	}
	if s := severityOf(v); s != "" {
		parts = append(parts, s)
	}
	switch fixed := osv.FixedVersions(v); len(fixed) {
	case 0:
		parts = append(parts, "no fix listed")
	case 1:
		parts = append(parts, "fixed in "+fixed[0])
	default:
		parts = append(parts, "fixed in "+strings.Join(fixed, ", "))
	}
	return strings.Join(parts, " · ")
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
