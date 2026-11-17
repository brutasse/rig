package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

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

// auditFinding is one known vulnerability of one pinned artifact.
type auditFinding struct {
	coord   string
	version string
	vuln    *osv.Vulnerability
}

func newAuditCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "audit",
		Short: "Scan the lock for known vulnerabilities (OSV)",
		Long: `Scans every maven artifact pinned in the workspace lock against the OSV
vulnerability database (osv.dev) and lists each known vulnerability of a
pinned version. Git deps are pinned by commit sha and OSV cannot look
them up; they are skipped and counted.

audit reads the lock only: it never re-locks, never downloads artifacts,
and needs no JVM. It exits 1 when any pinned artifact has a known
vulnerability, 0 when the lock is clean. --offline refuses (the scan
needs the network).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAudit(cmd.Context(), o)
		},
	}
}

func runAudit(ctx context.Context, o *opts) error {
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
	// version either way.
	type coord struct{ name, version string }
	seen := make(map[coord]bool)
	var coords []coord
	git := 0
	for _, a := range root.Lock.Artifacts {
		switch a.Kind {
		case "mvn":
			c := coord{a.Group + "/" + a.Name, a.Version}
			if !seen[c] {
				seen[c] = true
				coords = append(coords, c)
			}
		case "git":
			git++
		}
	}
	if len(coords) == 0 {
		msg := "audit: no maven artifacts to scan"
		if git > 0 {
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
			findings = append(findings, auditFinding{coord: coords[i].name, version: coords[i].version, vuln: v})
		}
	}
	if len(findings) == 0 {
		msg := fmt.Sprintf("audit: no known vulnerabilities (%d maven artifact%s", len(coords), plural(len(coords)))
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
	fmt.Printf("audit: %d known vulnerabilities in %d of %d pinned artifacts\n\n",
		len(findings), len(affected), len(coords))
	for _, f := range findings {
		fmt.Printf("%s %s\n", f.coord, f.version)
		fmt.Printf("  %s%s — %s\n", f.vuln.ID, aliasesOf(f.vuln), f.vuln.Summary)
		fmt.Printf("    %s\n", metaOf(f.vuln))
	}
	return exitf(1, "audit: %d known vulnerabilities in %d artifact(s)", len(findings), len(affected))
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

// severityOf renders the vuln's severity label. OSV sometimes gives a
// numeric score ("9.8") and sometimes the CVSS vector string itself
// ("CVSS:3.1/AV:N/…"); only the number fits the report line, so the
// vector reduces to its type.
func severityOf(v *osv.Vulnerability) string {
	if len(v.Severity) == 0 {
		return ""
	}
	s := v.Severity[0]
	if _, err := strconv.ParseFloat(s.Score, 64); err != nil {
		return "severity " + s.Type
	}
	return "severity " + s.Type + " " + s.Score
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
