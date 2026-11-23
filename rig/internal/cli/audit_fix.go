package cli

// rig audit --fix: apply the fixable findings' fixes, re-lock once,
// verify by re-querying OSV. The scan is scanAudit (audit.go); the fix
// itself is one kernel call (the audit-fix op: plan + batch edits +
// single re-lock, the editDep plumbing reused), then an OSV batch
// re-query of every locked version of the original findings'
// coordinates in the saved lock.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/brutasse/rig/internal/ednlit"
	"github.com/brutasse/rig/internal/kernel"
	"github.com/brutasse/rig/internal/lockfile"
	"github.com/brutasse/rig/internal/osv"
	"github.com/brutasse/rig/internal/workspace"
)

// fixTarget is one kernel audit-fix target: a vulnerable (coordinate,
// locked version) pair, the union of the findings' fix versions.
type fixTarget struct {
	Coord  string   `json:"coord"`
	Locked string   `json:"locked"`
	Fixes  []string `json:"fixes"`
}

// fixTargets groups the findings per (coordinate, locked version) and
// returns one target per pair (sorted) with the union of its findings'
// fix versions, and the set of every finding's vuln id.
func fixTargets(findings []auditFinding) ([]fixTarget, map[string]bool) {
	type pair struct{ coord, version string }
	fixes := make(map[pair]map[string]bool)
	ids := make(map[string]bool)
	for _, f := range findings {
		p := pair{f.coord, f.version}
		m := fixes[p]
		if m == nil {
			m = make(map[string]bool)
			fixes[p] = m
		}
		ids[f.vuln.ID] = true
		for _, v := range osv.FixedVersions(f.vuln) {
			m[v] = true
		}
	}
	targets := make([]fixTarget, 0, len(fixes))
	for p, fs := range fixes {
		fv := make([]string, 0, len(fs))
		for v := range fs {
			fv = append(fv, v)
		}
		sort.Strings(fv)
		targets = append(targets, fixTarget{Coord: p.coord, Locked: p.version, Fixes: fv})
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Coord != targets[j].Coord {
			return targets[i].Coord < targets[j].Coord
		}
		return targets[i].Locked < targets[j].Locked
	})
	return targets, ids
}

// auditFixPlan is one plan row of the kernel audit-fix response (and of
// the --json fix section): a target entry's picked target, or the
// reason it is unfixable (the other is nil).
type auditFixPlan struct {
	Coord  string  `json:"coord"`
	Locked string  `json:"locked"`
	Target *string `json:"target"`
	Reason *string `json:"reason"`
}

// auditFixSite is one edit site of an applied fix: the manifest file,
// the module, the deps section (and alias, for alias sections), and the
// version the site carried (nil for a floating requirement).
type auditFixSite struct {
	File    string  `json:"file"`
	Module  string  `json:"module"`
	Section string  `json:"section"`
	Alias   *string `json:"alias"`
	From    *string `json:"from"`
}

// auditFixPin is the root force pin of an applied fix: the section it
// is written in (the :rig/deps pool, "pool", or the :rig/overrides
// force pins, "overrides"), add (the entry is created) or update (from
// the entry's own version), at value.
type auditFixPin struct {
	Section string  `json:"section"`
	Action  string  `json:"action"`
	From    *string `json:"from"`
	Value   string  `json:"value"`
}

// auditFixApplied is one applied fix: the coordinate, its target, its
// edit sites (empty when only a pin was written), and the pin (nil
// when none).
type auditFixApplied struct {
	Coord  string         `json:"coord"`
	Target string         `json:"target"`
	Sites  []auditFixSite `json:"sites"`
	Pin    *auditFixPin   `json:"pin"`
}

// auditFixRemain is one finding that still stands after the fix: the
// vuln id, the (coordinate, version) it hits, and the remediation note
// the re-locked state derives for the pair (nil when none).
type auditFixRemain struct {
	ID      string  `json:"id"`
	Coord   string  `json:"coord"`
	Version string  `json:"version"`
	Note    *string `json:"note,omitempty"`
}

// auditFixVerified is the verification of the fix: the original
// findings' vuln ids absent from the re-query (fixed) and the ids the
// re-query still returns (remaining, with the pair they hit).
type auditFixVerified struct {
	Fixed     []string         `json:"fixed"`
	Remaining []auditFixRemain `json:"remaining"`
}

// auditFix is the --json report's fix section: the kernel's plan and
// applied lists, the root's :rig/overrides net diff across the fix
// (absent when the section is unchanged), and the verification
// (absent when it could not run).
type auditFix struct {
	Plan      []auditFixPlan       `json:"plan"`
	Applied   []auditFixApplied    `json:"applied"`
	Overrides []auditOverridesDiff `json:"overrides,omitempty"`
	Verified  *auditFixVerified    `json:"verified,omitempty"`
}

// auditFixResponse is the kernel audit-fix op's response: the plan (one
// row per target), the applied fixes, the manifest edits (the edit-dep
// record shape), the new lock document (absent when nothing was applied
// and no re-lock was requested) or the sweep of the dry re-derivation
// (per coordinate, per module, per scope, the resolved kind and
// version — the pins' verdict basis, see sweepDrops), and the re-lock's
// refused requirements.
type auditFixResponse struct {
	Plan    []auditFixPlan                            `json:"plan"`
	Applied []auditFixApplied                         `json:"applied"`
	Edits   []manifestEdit                            `json:"edits"`
	Lock    *lockfile.Document                        `json:"lock"`
	Sweep   map[string]map[string]map[string][]string `json:"sweep"`
	Refused []map[string]any                          `json:"refused"`
}

// pinEdit is one caller-supplied edit of the root's :rig/overrides
// force pins: a nil requirement removes the entry (the stale-pin drop),
// a value sets it (the restore of a pin the verification shows was
// load-bearing).
type pinEdit struct {
	Coord       string  `json:"coord"`
	Requirement *string `json:"requirement"`
}

// auditFixCall runs the kernel audit-fix op: every fixable coordinate's
// edit sites and force pin (the root's :rig/overrides entry, or the
// :rig/deps pool entry it already carries) are applied, the
// caller-supplied :rig/overrides pin edits are applied, and the
// workspace re-locks once — or, with drySweep, re-derives without
// locking (the :rig/overrides force pins stripped from the basis) and
// returns the sweep. It completes and saves the lock, returning the
// response and the saved document. On any failure after the kernel may
// have edited a manifest, the manifests are restored byte-for-byte — a
// refusal arrives from the re-lock, after the edits, and is reported as
// an error for the same reason.
func (o *opts) auditFixCall(ctx context.Context, root *workspace.Root, targets []fixTarget, breaking, drySweep bool, pinEdits []pinEdit, reqOverrides map[string]string) (resp auditFixResponse, lock *lockfile.Document, err error) {
	store, err := o.store()
	if err != nil {
		return resp, nil, err
	}
	jar, err := kernel.Kernel(store)
	if err != nil {
		return resp, nil, err
	}
	java, _, err := o.pickJava(ctx, store, root)
	if err != nil {
		return resp, nil, err
	}
	env, err := o.kernelEnv(ctx, root)
	if err != nil {
		return resp, nil, err
	}
	penv, err := o.kernelProxy(ctx, root)
	if err != nil {
		return resp, nil, err
	}
	defer o.closeProxy()
	snap, err := snapshotManifests(root)
	if err != nil {
		return resp, nil, err
	}
	defer func() {
		if err != nil {
			revertManifests(snap)
		}
	}()
	out, err := kernel.Call(ctx, jar, java, kernel.Request{
		Op:        "audit-fix",
		Workspace: root.Dir,
		Lock:      "deps.lock",
		Args: map[string]any{
			"targets":               targets,
			"breaking":              breaking,
			"offline":               o.offline,
			"force":                 o.force,
			"dry-sweep":             drySweep,
			"pin-edits":             pinEdits,
			"requirement-overrides": reqOverrides,
		},
	}, append(env, penv...)...)
	if err != nil {
		var oe *kernel.OpError
		if errors.As(err, &oe) {
			return resp, nil, exitf(1, "audit --fix: %v", oe)
		}
		return resp, nil, err
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return resp, nil, fmt.Errorf("audit --fix: bad kernel response: %w", err)
	}
	if len(resp.Refused) > 0 {
		return resp, nil, refusedErr(resp.Refused)
	}
	if resp.Lock == nil {
		return resp, nil, nil
	}
	if err := completeLock(ctx, o.fetchClient(ctx, root), store, o.m2Root(), jar, resp.Lock); err != nil {
		return resp, nil, err
	}
	if err := resp.Lock.Save(root.LockPath()); err != nil {
		return resp, nil, err
	}
	return resp, resp.Lock, nil
}

// verifyFix re-queries OSV for every locked version of the original
// findings' coordinates in the saved lock, plus every locked version of
// the pins the fix dropped (their lock is the only proof the drop was
// safe): an original finding's vuln id is fixed when the re-query does
// not return it, remaining (with the pair it hits) when it does —
// whatever its originality, a new finding at a new version stands too.
func (o *opts) verifyFix(ctx context.Context, lock *lockfile.Document, findings []auditFinding, drops []string) (*auditFixVerified, error) {
	// Every (coordinate, version) pair the lock carries for an original
	// finding's coordinate (one coordinate can lock several versions,
	// per module) — and for a dropped pin's coordinate.
	coords := make(map[string]bool)
	for _, f := range findings {
		coords[f.coord] = true
	}
	for _, c := range drops {
		coords[c] = true
	}
	seen := make(map[auditCoord]bool)
	var pairs []auditCoord
	for _, a := range lock.Artifacts {
		if a.Kind != "mvn" || !coords[a.Group+"/"+a.Name] {
			continue
		}
		c := auditCoord{a.Group + "/" + a.Name, a.Version}
		if !seen[c] {
			seen[c] = true
			pairs = append(pairs, c)
		}
	}
	queries := make([]osv.Query, len(pairs))
	for i, c := range pairs {
		queries[i] = osv.Query{Package: osv.Pkg{Name: strings.ReplaceAll(c.name, "/", ":"), Ecosystem: "Maven"}, Version: c.version}
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	var perQuery [][]string
	if err := auditRetry(ctx, func() error {
		var e error
		perQuery, e = osv.QueryBatch(ctx, client, osvAPI(), queries)
		return e
	}); err != nil {
		return nil, err
	}
	hit := make(map[string]map[auditCoord]bool)
	for i, ids := range perQuery {
		for _, id := range ids {
			m := hit[id]
			if m == nil {
				m = make(map[auditCoord]bool)
				hit[id] = m
			}
			m[pairs[i]] = true
		}
	}
	orig := make(map[string]bool)
	for _, f := range findings {
		orig[f.vuln.ID] = true
	}
	out := &auditFixVerified{Fixed: []string{}, Remaining: []auditFixRemain{}}
	for id := range orig {
		if _, still := hit[id]; !still {
			out.Fixed = append(out.Fixed, id)
		}
	}
	for id, ps := range hit {
		for p := range ps {
			out.Remaining = append(out.Remaining, auditFixRemain{ID: id, Coord: p.name, Version: p.version})
		}
	}
	sort.Strings(out.Fixed)
	sort.Slice(out.Remaining, func(i, j int) bool {
		if out.Remaining[i].ID != out.Remaining[j].ID {
			return out.Remaining[i].ID < out.Remaining[j].ID
		}
		if out.Remaining[i].Coord != out.Remaining[j].Coord {
			return out.Remaining[i].Coord < out.Remaining[j].Coord
		}
		return out.Remaining[i].Version < out.Remaining[j].Version
	})
	return out, nil
}

// sweepDrops judges the workspace's :rig/overrides pins against the
// dry re-derivation's sweep: a pin is stale when no module (base or
// alias scope) resolves its coordinate at an OSV-vulnerable version —
// the coordinate absent from the sweep, or every resolved version
// clean. A resolved version OSV cannot query (git or local) keeps the
// pin conservatively. Returns the stale pins.
func (o *opts) sweepDrops(ctx context.Context, sweep map[string]map[string]map[string][]string, pins map[string]string) (map[string]bool, error) {
	drop := make(map[string]bool, len(pins))
	for c := range pins {
		drop[c] = true
	}
	type pair struct{ coord, version string }
	var pairs []pair
	seen := make(map[pair]bool)
	for c := range pins {
		for _, scopes := range sweep[c] {
			for _, kv := range scopes {
				if len(kv) != 2 {
					continue
				}
				if kv[0] != "mvn" {
					drop[c] = false
					continue
				}
				p := pair{c, kv[1]}
				if !seen[p] {
					seen[p] = true
					pairs = append(pairs, p)
				}
			}
		}
	}
	if len(pairs) == 0 {
		return drop, nil
	}
	queries := make([]osv.Query, len(pairs))
	for i, p := range pairs {
		queries[i] = osv.Query{Package: osv.Pkg{Name: strings.ReplaceAll(p.coord, "/", ":"), Ecosystem: "Maven"}, Version: p.version}
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	var perQuery [][]string
	if err := auditRetry(ctx, func() error {
		var e error
		perQuery, e = osv.QueryBatch(ctx, client, osvAPI(), queries)
		return e
	}); err != nil {
		return nil, err
	}
	for i, ids := range perQuery {
		if len(ids) > 0 {
			drop[pairs[i].coord] = false
		}
	}
	return drop, nil
}

// overridesOf is the root manifest's :rig/overrides force pins carrying
// an mvn version, as coordinate -> version (entries without a version
// are out of the pin lifecycle's scope and never reported or touched).
func overridesOf(root *workspace.Root) map[string]string {
	out := make(map[string]string)
	man, ok := manifestEDN(root, ".")
	if !ok {
		return out
	}
	v, ok := ednlitGet(man, ednlit.Keyword{NS: "rig", Name: "overrides"})
	if !ok {
		return out
	}
	if m, ok := v.(ednlit.Map); ok {
		for _, p := range m {
			if c, ok := coordOf(p.K); ok {
				if s := specVersion(p.V); s != "" {
					out[c] = s
				}
			}
		}
	}
	return out
}

// appliedTargets is an applied fix list's coord -> target map: the
// requirement-overrides that ride the re-lock after the pins' verdict
// (the fix versions stay locked whatever the pins do).
func appliedTargets(applied []auditFixApplied) map[string]string {
	out := make(map[string]string, len(applied))
	for _, a := range applied {
		out[a.Coord] = a.Target
	}
	return out
}

// auditOverridesDiff is one row of the :rig/overrides section's net
// diff across the fix: add (the fix wrote the pin and it stands),
// remove (the pin was stale and dropped), update (the fix moved the
// pin's version), keep (the fix dropped the pin, the verification
// showed it was load-bearing, and it was restored).
type auditOverridesDiff struct {
	Coord  string  `json:"coord"`
	Action string  `json:"action"`
	From   *string `json:"from,omitempty"`
	To     *string `json:"to,omitempty"`
}

// overridesDiff is the :rig/overrides section's net change across the
// fix: the pre-fix pins against the final ones (a pin added and dropped
// by the same run is absent, an updated-then-dropped pin is one remove),
// plus a keep row per restored pin.
func overridesDiff(before, after map[string]string, restored []string) []auditOverridesDiff {
	var out []auditOverridesDiff
	s := func(v string) *string { return &v }
	for c, v := range after {
		old, was := before[c]
		switch {
		case !was:
			out = append(out, auditOverridesDiff{Coord: c, Action: "add", To: s(v)})
		case old != v:
			out = append(out, auditOverridesDiff{Coord: c, Action: "update", From: s(old), To: s(v)})
		}
	}
	for c, v := range before {
		if _, is := after[c]; !is {
			out = append(out, auditOverridesDiff{Coord: c, Action: "remove", From: s(v)})
		}
	}
	for _, c := range restored {
		if v, is := after[c]; is {
			out = append(out, auditOverridesDiff{Coord: c, Action: "keep", To: s(v)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Coord != out[j].Coord {
			return out[i].Coord < out[j].Coord
		}
		return out[i].Action < out[j].Action
	})
	return out
}

// printOverridesDiff prints the :rig/overrides net-diff block after the
// fix and unfixable blocks, before the lock write line; no block when
// the section's net change is empty.
func printOverridesDiff(rows []auditOverridesDiff) {
	if len(rows) == 0 {
		return
	}
	fmt.Println("overrides:")
	for _, r := range rows {
		switch r.Action {
		case "add":
			fmt.Printf("  add %s %q\n", r.Coord, *r.To)
		case "remove":
			fmt.Printf("  remove %s %q (no module resolves vulnerable without the pin)\n", r.Coord, *r.From)
		case "update":
			fmt.Printf("  update %s %q → %q\n", r.Coord, *r.From, *r.To)
		case "keep":
			fmt.Printf("  keep %s %q (pin-less resolution still pulls vulnerable)\n", r.Coord, *r.To)
		}
	}
}

// verifiedOf is the verification when nothing was applied: no re-query
// (nothing moved), the original findings stand as-is, their notes
// derived from the unchanged pin state.
func verifiedOf(findings []auditFinding, state *auditPinState) *auditFixVerified {
	out := &auditFixVerified{Fixed: []string{}, Remaining: []auditFixRemain{}}
	seen := make(map[string]bool)
	for _, f := range findings {
		k := f.vuln.ID + " " + f.coord + " " + f.version
		if seen[k] {
			continue
		}
		seen[k] = true
		r := auditFixRemain{ID: f.vuln.ID, Coord: f.coord, Version: f.version}
		if note := state.auditNote(f.coord, f.version); note != "" {
			r.Note = &note
		}
		out.Remaining = append(out.Remaining, r)
	}
	sort.Slice(out.Remaining, func(i, j int) bool {
		if out.Remaining[i].ID != out.Remaining[j].ID {
			return out.Remaining[i].ID < out.Remaining[j].ID
		}
		if out.Remaining[i].Coord != out.Remaining[j].Coord {
			return out.Remaining[i].Coord < out.Remaining[j].Coord
		}
		return out.Remaining[i].Version < out.Remaining[j].Version
	})
	return out
}

// groupFindings fills report.Findings from the sorted findings, one
// entry per (coordinate, version) (the --json rendering), with the
// remediation note the pin state derives for the pair.
func groupFindings(report *auditReport, findings []auditFinding, chainsByScope map[string]map[string][][]string, state *auditPinState) {
	for _, f := range findings {
		// The findings are sorted per artifact: a new coordinate
		// starts a new group, the rest append to it.
		n := len(report.Findings)
		if n == 0 || report.Findings[n-1].Coordinate != f.coord || report.Findings[n-1].Version != f.version {
			a := auditArtifact{
				Coordinate: f.coord,
				Version:    f.version,
				Scopes:     f.scopes,
				Vulns:      []auditVulnRow{},
				Chains:     chainsFor(f.coord, f.version, f.scopes, chainsByScope),
			}
			if note := state.auditNote(f.coord, f.version); note != "" {
				a.Note = &note
			}
			report.Findings = append(report.Findings, a)
		}
		last := &report.Findings[len(report.Findings)-1]
		last.Vulns = append(last.Vulns, vulnRow(f.version, f.vuln))
	}
}

// printFindingsHuman prints the findings' human-readable report: one
// block per (coordinate, version), the remediation note, and its
// vulnerabilities under it.
func printFindingsHuman(findings []auditFinding, chainsByScope map[string]map[string][][]string, state *auditPinState) {
	prev := ""
	for _, f := range findings {
		// The artifact line and its scopes print once per artifact,
		// whatever vulnerabilities it carries.
		if f.coord+" "+f.version != prev {
			prev = f.coord + " " + f.version
			fmt.Printf("%s %s\n", f.coord, f.version)
			if len(f.scopes) > 0 {
				fmt.Printf("  used in %s\n", strings.Join(f.scopes, ", "))
			}
			if lines := pulledInLines(f.coord, f.version, f.scopes, chainsByScope); len(lines) > 0 {
				fmt.Println("  pulled in:")
				for _, l := range lines {
					fmt.Printf("    %s\n", l)
				}
			}
			if note := state.auditNote(f.coord, f.version); note != "" {
				fmt.Printf("  note: %s\n", note)
			}
		}
		fmt.Printf("  %s%s — %s\n", f.vuln.ID, aliasesOf(f.vuln), f.vuln.Summary)
		fmt.Printf("    %s\n", metaOf(f.version, f.vuln))
	}
}

// printFixHuman prints the fix report: per applied coordinate, the
// fix line, the edit and pin lines, and the vuln ids it fixes; then
// the unfixable lines with their reasons and the pin note the state
// derives for the pair.
func printFixHuman(findings []auditFinding, resp *auditFixResponse, chainsByScope map[string]map[string][][]string, state *auditPinState) {
	lockedOf := make(map[string][]string)
	scopesOf := make(map[string]map[string]bool)
	vulnsOf := make(map[string]map[string]bool)
	for _, p := range resp.Plan {
		lockedOf[p.Coord] = append(lockedOf[p.Coord], p.Locked)
	}
	for _, f := range findings {
		m := scopesOf[f.coord]
		if m == nil {
			m = make(map[string]bool)
			scopesOf[f.coord] = m
		}
		for _, s := range f.scopes {
			m[s] = true
		}
		v := vulnsOf[f.coord]
		if v == nil {
			v = make(map[string]bool)
			vulnsOf[f.coord] = v
		}
		v[f.vuln.ID+aliasesOf(f.vuln)] = true
	}
	sortedKeys := func(m map[string]bool) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	for _, a := range resp.Applied {
		fmt.Printf("fix: %s %s → %s\n", a.Coord, strings.Join(lockedOf[a.Coord], ", "), a.Target)
		if scopes := scopesOf[a.Coord]; len(scopes) > 0 {
			fmt.Printf("  used in %s\n", strings.Join(sortedKeys(scopes), ", "))
		}
		if lines := fixPulledLines(a.Coord, findings, chainsByScope); len(lines) > 0 {
			fmt.Println("  pulled in:")
			for _, l := range lines {
				fmt.Printf("    %s\n", l)
			}
		}
		for _, s := range a.Sites {
			alias := ""
			if s.Alias != nil {
				alias = ", alias " + *s.Alias
			}
			fmt.Printf("  %s: set %s %q (%s%s)\n", s.File, a.Coord, a.Target, s.Section, alias)
		}
		if a.Pin != nil && a.Pin.Section != "overrides" {
			// The :rig/overrides pin lines print in the section's net
			// diff block (printOverridesDiff), not per fix.
			if a.Pin.From == nil {
				fmt.Printf("  %s: add %s %q\n", a.Pin.Section, a.Coord, a.Pin.Value)
			} else {
				fmt.Printf("  %s: update %s %q → %q\n", a.Pin.Section, a.Coord, *a.Pin.From, a.Pin.Value)
			}
		}
		if ids := vulnsOf[a.Coord]; len(ids) > 0 {
			fmt.Printf("  fixes: %s\n", strings.Join(sortedKeys(ids), ", "))
		}
	}
	for _, p := range resp.Plan {
		if p.Reason != nil {
			fmt.Printf("unfixable: %s %s: %s\n", p.Coord, p.Locked, unfixableText(*p.Reason))
			if note := state.auditNote(p.Coord, p.Locked); note != "" {
				fmt.Printf("  note: %s\n", note)
			}
		}
	}
}

// fixPulledLines is a fixed coordinate's "pulled in" lines, one per
// chain per (coordinate, version) pair of its findings (the chains
// come from the pre-fix lock).
func fixPulledLines(coord string, findings []auditFinding, byScope map[string]map[string][][]string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, f := range findings {
		if f.coord != coord || seen[f.version] {
			continue
		}
		seen[f.version] = true
		out = append(out, pulledInLines(f.coord, f.version, f.scopes, byScope)...)
	}
	return out
}

// unfixableText renders a plan row's reason as a human line.
func unfixableText(reason string) string {
	switch reason {
	case "no-fix":
		return "no fix listed"
	case "breaking":
		return "fixes only in a higher major (use --breaking)"
	case "below-locked":
		return "fixes only below the locked version"
	case "fix-unavailable":
		return "fix not available in the workspace's repositories"
	case "stale-lock":
		return "the manifest already pins a clean version; the lock is stale (run rig lock)"
	default:
		return reason
	}
}

// auditModuleDecls is one module manifest's declarations of the
// coordinates the notes derive from: the versions each coordinate is
// declared at, and whether one of its declarations is a floating
// requirement (release, latest).
type auditModuleDecls struct {
	versions map[string]map[string]bool // coord -> the declared versions
	floating map[string]bool            // coord -> a floating declaration
}

// auditPinState is the pin situation of a workspace's manifests and one
// lock document: the root :rig/deps pool's and :rig/overrides' versions
// per coordinate, the lock modules whose classpaths (base or alias)
// carry each (coordinate, version), and every module manifest's
// declarations. From it auditNote derives a finding's remediation note.
type auditPinState struct {
	pool  map[string]string            // coord -> the pool entry's version ("" when absent)
	ovr   map[string]string            // coord -> the overrides entry's version ("" when absent)
	pulls map[auditCoord][]string      // (coord, version) -> the lock modules carrying it
	decls map[string]*auditModuleDecls // lock module dir -> its manifest's declarations
}

// auditPinStateOf reads the root manifest (its :rig/deps pool and
// :rig/overrides force pins) and every lock module's manifest (ednlit;
// a manifest that is unreadable or unparseable declares nothing) and
// the lock's classpaths, and returns the pin state the finding notes
// derive from.
func auditPinStateOf(root *workspace.Root, lock *lockfile.Document) *auditPinState {
	st := &auditPinState{
		pool:  make(map[string]string),
		ovr:   make(map[string]string),
		pulls: make(map[auditCoord][]string),
		decls: make(map[string]*auditModuleDecls),
	}
	byID := make(map[string]auditCoord, len(lock.Artifacts))
	for _, a := range lock.Artifacts {
		if a.Kind == "mvn" {
			byID[a.ID] = auditCoord{a.Group + "/" + a.Name, a.Version}
		}
	}
	seen := make(map[auditCoord]map[string]bool)
	for dir, m := range lock.Modules {
		entries := append([]lockfile.ClasspathEntry{}, m.Classpath...)
		for _, al := range m.Aliases {
			entries = append(entries, al.Classpath...)
		}
		for _, e := range entries {
			if e.Ref == nil {
				continue
			}
			c, ok := byID[*e.Ref]
			if !ok {
				continue
			}
			if seen[c] == nil {
				seen[c] = make(map[string]bool)
			}
			if seen[c][dir] {
				continue
			}
			seen[c][dir] = true
			st.pulls[c] = append(st.pulls[c], dir)
		}
	}
	for dir := range lock.Modules {
		man, ok := manifestEDN(root, dir)
		if !ok {
			continue
		}
		if dir == "." {
			for _, sec := range []ednlit.Keyword{{NS: "rig", Name: "deps"}, {NS: "rig", Name: "overrides"}} {
				var dst map[string]string
				if sec.Name == "deps" {
					dst = st.pool
				} else {
					dst = st.ovr
				}
				if v, ok := ednlitGet(man, sec); ok {
					if m, ok := v.(ednlit.Map); ok {
						for _, p := range m {
							if c, ok := coordOf(p.K); ok {
								dst[c] = specVersion(p.V)
							}
						}
					}
				}
			}
		}
		d := st.decls[dir]
		if d == nil {
			d = &auditModuleDecls{versions: make(map[string]map[string]bool), floating: make(map[string]bool)}
			st.decls[dir] = d
		}
		for _, section := range depSectionsOf(man) {
			addDecls(d, section)
		}
	}
	return st
}

// manifestEDN is the manifest at module dir ("." is the root), parsed
// with ednlit; a manifest that is unreadable or unparseable is !ok.
func manifestEDN(root *workspace.Root, dir string) (ednlit.Map, bool) {
	data, err := os.ReadFile(filepath.Join(root.Dir, dir, "deps.edn"))
	if err != nil {
		return nil, false
	}
	v, err := ednlit.ParseNamed("deps.edn", string(data))
	if err != nil {
		return nil, false
	}
	m, ok := v.(ednlit.Map)
	return m, ok
}

// depSectionsOf is a manifest's dep sections: its :deps and every
// alias's :deps, :extra-deps and :replace-deps.
func depSectionsOf(man ednlit.Map) []ednlit.Map {
	var out []ednlit.Map
	if deps, ok := ednlitGet(man, ednlit.Keyword{Name: "deps"}); ok {
		if m, ok := deps.(ednlit.Map); ok {
			out = append(out, m)
		}
	}
	if aliases, ok := ednlitGet(man, ednlit.Keyword{Name: "aliases"}); ok {
		if m, ok := aliases.(ednlit.Map); ok {
			for _, pair := range m {
				al, ok := pair.V.(ednlit.Map)
				if !ok {
					continue
				}
				for _, k := range []ednlit.Keyword{{Name: "deps"}, {Name: "extra-deps"}, {Name: "replace-deps"}} {
					if s, ok := ednlitGet(al, k); ok {
						if sm, ok := s.(ednlit.Map); ok {
							out = append(out, sm)
						}
					}
				}
			}
		}
	}
	return out
}

// coordOf is a deps entry's coordinate as "group/name": a symbol key,
// or a string key.
func coordOf(k any) (string, bool) {
	switch t := k.(type) {
	case ednlit.Symbol:
		return t.String(), true
	case string:
		return t, true
	}
	return "", false
}

// specVersion is a deps entry's version: the spec itself when it is a
// version string, its :mvn/version when it is a map ("" for a git or
// local spec, which pins no version).
func specVersion(spec any) string {
	if s, ok := spec.(string); ok {
		return s
	}
	if m, ok := spec.(ednlit.Map); ok {
		if v, ok := ednlitGet(m, ednlit.Keyword{NS: "mvn", Name: "version"}); ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

// addDecls records a dep section's entries in d.
func addDecls(d *auditModuleDecls, m ednlit.Map) {
	for _, p := range m {
		c, ok := coordOf(p.K)
		if !ok {
			continue
		}
		v := specVersion(p.V)
		if v == "" {
			continue
		}
		if d.versions[c] == nil {
			d.versions[c] = make(map[string]bool)
		}
		d.versions[c][v] = true
		if strings.EqualFold(v, "release") || strings.EqualFold(v, "latest") {
			d.floating[c] = true
		}
	}
}

// auditNote is the remediation note of a (coordinate, version) finding,
// derived from the pin state: the root's force pin the coordinate sits
// under — its :rig/overrides entry (the section a fix writes, beating
// the pool on the same coordinate) or its :rig/deps pool entry — at the
// vulnerable version (bump the entry) or at a clean version the lock has
// not picked up (a plain re-lock) — or a coordinate some locking module
// pins while others pull it undeclared, the pull only a force pin
// reaches. Empty when every locking module pins the coordinate itself,
// or no manifest does (a pure transitive pull, which --fix reaches by
// writing the :rig/overrides pin).
func (st *auditPinState) auditNote(coord, version string) string {
	for _, e := range []struct{ sec, p string }{{"overrides", st.ovr[coord]}, {"pool", st.pool[coord]}} {
		if e.p == "" {
			continue
		}
		if e.p == version {
			if e.sec == "pool" {
				return "bump the pool entry"
			}
			return "bump the :rig/overrides pin"
		}
		if e.sec == "pool" {
			return fmt.Sprintf("the :rig/deps pool pins %s; the lock still carries %s — rig lock picks up the pool pin", e.p, version)
		}
		return fmt.Sprintf("the :rig/overrides pin pins %s; the lock still carries %s — rig lock picks up the pin", e.p, version)
	}
	sited, undecl := false, 0
	for _, m := range st.pulls[auditCoord{coord, version}] {
		d := st.decls[m]
		if d != nil && (d.floating[coord] || d.versions[coord][version]) {
			sited = true
		} else {
			undecl++
		}
	}
	if sited && undecl > 0 {
		return "a :rig/overrides force pin would force-pin it workspace-wide (rig audit --fix writes one)"
	}
	return ""
}

func runAuditFix(ctx context.Context, o *opts, jsonOut, explain, breaking bool) error {
	root, err := workspace.Find(".")
	if err != nil {
		return err
	}
	if root.Lock == nil {
		return exitf(3, "no lock at %s (run 'rig lock')", root.LockPath())
	}
	if o.offline {
		return exitf(1, "offline: 'rig audit --fix' needs the network")
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
	// The fix needs every finding's fix set: a record that could not be
	// fetched may carry fixes, so an incomplete scan does not attempt
	// it — the report stands, as the scan's.
	if failedN > 0 {
		var chainsByScope map[string]map[string][][]string
		if explain {
			chainsByScope = o.explainFindings(ctx, root, findings)
		}
		if jsonOut {
			report.Failed = failedN
			groupFindings(&report, findings, chainsByScope, state)
			if err := printReport(&report); err != nil {
				return err
			}
		} else {
			fmt.Printf("audit: %d known vulnerabilities in %d of %d pinned artifacts\n\n",
				len(findings), len(affected), report.Scanned)
			printFindingsHuman(findings, chainsByScope, state)
		}
		return exitf(4, "audit: scan incomplete: %d artifact record(s) could not be fetched; --fix not attempted", failedN)
	}
	var chainsByScope map[string]map[string][][]string
	if explain {
		chainsByScope = o.explainFindings(ctx, root, findings)
	}

	targets, _ := fixTargets(findings)
	ovBefore := overridesOf(root)
	// The stale-pin sweep: the fixes apply, and a dry re-derivation
	// (the :rig/overrides force pins stripped from the basis, no lock)
	// judges which of the section's pins — the fix's included — still
	// protect anything. The stale ones are dropped in the single real
	// re-lock, which carries the applied fixes' targets. When the
	// section is empty after the fix the kernel short-circuits to the
	// plain re-lock — one call, today's behavior.
	resp, lock, err := o.auditFixCall(ctx, root, targets, breaking, true, nil, nil)
	if err != nil {
		return err
	}
	var (
		drops    []string
		ovAfter1 map[string]string
	)
	if lock == nil {
		ovAfter1 = overridesOf(root)
		dm, err := o.sweepDrops(ctx, resp.Sweep, ovAfter1)
		if err != nil {
			// The verdict failed (OSV unreachable after the retries):
			// keep every pin, complete the fixes' re-lock.
			fmt.Fprintf(os.Stderr, "audit: pin verdict failed (%v): pins kept\n", err)
			dm = make(map[string]bool)
		}
		drops = make([]string, 0, len(dm))
		for c, d := range dm {
			if d {
				drops = append(drops, c)
			}
		}
		sort.Strings(drops)
		reqOv := appliedTargets(resp.Applied)
		if len(drops) > 0 || len(reqOv) > 0 {
			edits := make([]pinEdit, 0, len(drops))
			for _, c := range drops {
				edits = append(edits, pinEdit{Coord: c})
			}
			_, l, err := o.auditFixCall(ctx, root, nil, breaking, false, edits, reqOv)
			if err != nil {
				return err
			}
			lock = l
		}
	}
	// The verification runs before the report prints: the overrides
	// net-diff below reports the final section, and a pin the verdict
	// got wrong comes back at the value the fix left it, with its keep
	// line.
	var (
		v        *auditFixVerified
		verErr   error
		restored []string
	)
	if lock != nil {
		v, verErr = o.verifyFix(ctx, lock, findings, drops)
		if verErr == nil {
			for _, c := range drops {
				for _, r := range v.Remaining {
					if r.Coord == c {
						restored = append(restored, c)
						break
					}
				}
			}
			if len(restored) > 0 {
				edits := make([]pinEdit, 0, len(restored))
				for _, c := range restored {
					val := ovAfter1[c]
					edits = append(edits, pinEdit{Coord: c, Requirement: &val})
				}
				_, l, err := o.auditFixCall(ctx, root, nil, breaking, false, edits, nil)
				if err != nil {
					return err
				}
				lock = l
				v, verErr = o.verifyFix(ctx, lock, findings, restored)
			}
		}
	}
	ovDiff := overridesDiff(ovBefore, overridesOf(root), restored)
	report.Fix = &auditFix{
		Plan:      resp.Plan,
		Applied:   resp.Applied,
		Overrides: ovDiff,
		Verified:  verifiedOf(findings, state),
	}
	if report.Fix.Plan == nil {
		report.Fix.Plan = []auditFixPlan{}
	}
	if report.Fix.Applied == nil {
		report.Fix.Applied = []auditFixApplied{}
	}
	if !jsonOut {
		printFixHuman(findings, &resp, chainsByScope, state)
		printOverridesDiff(ovDiff)
	}
	if lock != nil {
		if !jsonOut {
			fmt.Printf("wrote %s: %d artifacts, %d modules\n",
				root.LockPath(), len(lock.Artifacts), len(lock.Modules))
			printSkipped(lock)
		}
		if verErr != nil {
			if jsonOut {
				if err := printReport(&report); err != nil {
					return err
				}
			}
			fmt.Fprintf(os.Stderr, "audit: verification failed: %v\n", verErr)
			return exitf(4, "audit: verification failed: %v", verErr)
		}
		report.Fix.Verified = v
		// The remaining pairs' notes derive from the re-locked state:
		// a pair locked at an applied fix target is the fixed version
		// itself; the rest re-derive from the new manifests and lock.
		newState := auditPinStateOf(root, lock)
		targets := make(map[string]string, len(resp.Applied))
		for _, a := range resp.Applied {
			targets[a.Coord] = a.Target
		}
		for i := range v.Remaining {
			r := &v.Remaining[i]
			if t, ok := targets[r.Coord]; ok && t == r.Version {
				note := "the fixed version itself has a finding — re-run --fix"
				r.Note = &note
				continue
			}
			if note := newState.auditNote(r.Coord, r.Version); note != "" {
				r.Note = &note
			}
		}
		if !jsonOut {
			fmt.Printf("verified: %d fixed, %d remaining\n", len(v.Fixed), len(v.Remaining))
			for _, r := range v.Remaining {
				fmt.Printf("  %s — %s %s\n", r.ID, r.Coord, r.Version)
				if r.Note != nil {
					fmt.Printf("    note: %s\n", *r.Note)
				}
			}
		}
	} else if !jsonOut && len(resp.Applied) == 0 {
		fmt.Println("audit: nothing to fix")
	}
	if jsonOut {
		groupFindings(&report, findings, chainsByScope, state)
		if err := printReport(&report); err != nil {
			return err
		}
	}
	if lock != nil && len(report.Fix.Verified.Remaining) == 0 {
		return nil
	}
	if lock != nil {
		return exitf(1, "audit: %d finding(s) remain after the fix", len(report.Fix.Verified.Remaining))
	}
	n := len(report.Fix.Verified.Remaining)
	return exitf(1, "audit: %d finding(s) remain (none fixable)", n)
}
