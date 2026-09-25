package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/brutasse/rig/internal/digest"
	"github.com/brutasse/rig/internal/jvm"
	"github.com/brutasse/rig/internal/lockfile"
)

// prepStamp is the on-disk record that a module's :deps/prep-lib output is
// up to date. It lives under the module's target dir, so 'rig clean'
// removes it together with the output.
type prepStamp struct {
	ManifestSHA256  string `json:"manifest_sha256"`
	SourceDigest    string `json:"source_digest"`
	ClasspathDigest string `json:"classpath_digest"`
	Prep            string `json:"prep"`
}

const prepStampFile = ".rig-prep.json"

// prepStampPath is the stamp location of module m, relative to the
// workspace root: under its target dir (the parent of the locked
// class-dir).
func prepStampPath(m string, mod lockfile.Module) string {
	return filepath.Join(m, filepath.Dir(mod.Build.ClassDir), prepStampFile)
}

// ensurePreps runs the :deps/prep-lib prep function of the target module
// and of every local module on its classpath, in dependency order, when
// their :ensure output is missing or stale. rig does not produce the
// generated code itself; it launches each module's own prep function on
// the locked prep-alias classpath and verifies the declared output. A
// module that declares :ensure output rig cannot run for it (no :fn, or a
// lock predating prep support) stays a hard error.
func (e *hotEnv) ensurePreps(ctx context.Context, m string, mod lockfile.Module) error {
	set, err := prepSet(e, m, mod)
	if err != nil {
		return err
	}
	ordered, err := prepOrder(e, set)
	if err != nil {
		return err
	}
	ran := map[string]bool{}
	for _, dir := range ordered {
		pmod := e.lock.Modules[dir]
		st, err := e.prepState(dir, pmod, ran)
		if err != nil {
			return err
		}
		if !st.stale {
			continue
		}
		fmt.Printf("prep %s: %s (%s)\n", dir, pmod.PrepFn, st.reason)
		if err := e.runPrep(ctx, dir, pmod); err != nil {
			return fmt.Errorf("prep %s: %w", dir, err)
		}
		ran[dir] = true
		if missing := missingEnsures(e, dir, pmod); len(missing) > 0 {
			return exitf(1, "prep %s did not produce: %s", dir, strings.Join(missing, ", "))
		}
		if err := e.writeStamp(dir, pmod, st); err != nil {
			return fmt.Errorf("prep %s: %w", dir, err)
		}
	}
	return nil
}

// prepSet is the set of modules whose prep matters for target m: m itself
// plus every local module on its (transitively locked) classpath that
// declares :ensure output. It errors when a member declares output rig
// cannot run for it (a lock predating prep support).
func prepSet(e *hotEnv, m string, mod lockfile.Module) ([]string, error) {
	seen := map[string]bool{}
	var set []string
	add := func(dir string) error {
		if seen[dir] {
			return nil
		}
		seen[dir] = true
		dep, ok := e.lock.Modules[dir]
		if !ok || len(dep.PrepEnsure) == 0 {
			return nil
		}
		if dep.PrepAlias == "" || dep.PrepFn == "" {
			return exitf(2, "lock for %s predates prep support (prep-ensure without prep-fn); run 'rig lock'", dir)
		}
		set = append(set, dir)
		return nil
	}
	if err := add(m); err != nil {
		return nil, err
	}
	for _, en := range mod.Classpath {
		if en.Local == nil {
			continue
		}
		if err := add(*en.Local); err != nil {
			return nil, err
		}
	}
	return set, nil
}

// prepOrder orders prep dirs so each dir comes after the local modules on
// its classpath that are in dirs (a prep may read their output). Cycles
// are an error.
func prepOrder(e *hotEnv, dirs []string) ([]string, error) {
	sorted := append([]string{}, dirs...)
	sort.Strings(sorted)
	in := make(map[string]bool, len(dirs))
	for _, d := range dirs {
		in[d] = true
	}
	pending := make(map[string]map[string]bool, len(dirs))
	for _, d := range dirs {
		dep := map[string]bool{}
		m, ok := e.lock.Modules[d]
		if ok {
			for _, en := range m.Classpath {
				if en.Local != nil && in[*en.Local] {
					dep[*en.Local] = true
				}
			}
		}
		pending[d] = dep
	}
	var out []string
	done := make(map[string]bool, len(dirs))
	for len(out) < len(dirs) {
		var ready []string
		for _, d := range sorted {
			if !done[d] && len(pending[d]) == 0 {
				ready = append(ready, d)
			}
		}
		if len(ready) == 0 {
			return nil, errors.New("cycle in local module dependencies")
		}
		for _, d := range ready {
			done[d] = true
			out = append(out, d)
			for _, o := range sorted {
				delete(pending[o], d)
			}
		}
	}
	return out, nil
}

// prepState is the staleness verdict for one module: whether its prep must
// (re)run, why (for the log line), and the digests a fresh stamp records.
type prepState struct {
	stale  bool
	reason string
	source string
	class  string
}

// prepState decides whether m's prep must run: missing :ensure output, a
// missing or out-of-date stamp (manifest, sources or locked dependencies
// changed, prep function changed), or a local dependency that was
// re-prepped in this invocation.
func (e *hotEnv) prepState(m string, mod lockfile.Module, ran map[string]bool) (prepState, error) {
	src, err := e.sourceDigest(m, mod)
	if err != nil {
		return prepState{}, fmt.Errorf("digest sources of %s: %w", m, err)
	}
	cpd, err := classpathDigest(e, mod)
	if err != nil {
		return prepState{}, fmt.Errorf("digest classpath of %s: %w", m, err)
	}
	st := prepState{source: src, class: cpd}
	if missing := missingEnsures(e, m, mod); len(missing) > 0 {
		st.stale, st.reason = true, "missing "+strings.Join(missing, ", ")
		return st, nil
	}
	stamp, ok := e.readStamp(m, mod)
	if !ok {
		st.stale, st.reason = true, "no stamp"
		return st, nil
	}
	switch {
	case stamp.Prep != mod.PrepFn:
		st.stale, st.reason = true, "prep function changed"
	case stamp.ManifestSHA256 != mod.ManifestSHA256:
		st.stale, st.reason = true, "manifest changed"
	case src != stamp.SourceDigest:
		st.stale, st.reason = true, "sources changed"
	case cpd != stamp.ClasspathDigest:
		st.stale, st.reason = true, "dependencies changed"
	}
	if !st.stale {
		for _, en := range mod.Classpath {
			if en.Local != nil && ran[*en.Local] {
				st.stale, st.reason = true, "dependency "+*en.Local+" re-prepped"
				return st, nil
			}
		}
	}
	return st, nil
}

// sourceDigest content-addresses the module's source tree: its build src
// and java-src dirs plus the prep alias's extra-paths (so edits to the
// prep build file count as a change).
func (e *hotEnv) sourceDigest(m string, mod lockfile.Module) (string, error) {
	dir := e.modDir(m)
	dirs := absJoin(dir, mod.Build.SrcDirs)
	dirs = append(dirs, absJoin(dir, mod.Build.JavaSrcDirs)...)
	if al, ok := mod.Aliases[mod.PrepAlias]; ok {
		dirs = append(dirs, absJoin(dir, al.Paths)...)
	}
	return digest.Dirs(dirs...)
}

// classpathDigest covers the module's locked base classpath: the mvn and
// git artifact ids with their digests. Local entries are excluded — their
// content flows through the re-prepped-dependency rule instead.
func classpathDigest(e *hotEnv, mod lockfile.Module) (string, error) {
	arts := make(map[string]lockfile.Artifact, len(e.lock.Artifacts))
	for _, a := range e.lock.Artifacts {
		arts[a.ID] = a
	}
	h := sha256.New()
	for _, en := range mod.Classpath {
		if en.Local != nil || en.Ref == nil {
			continue
		}
		a := arts[*en.Ref]
		s := a.SHA256
		if a.Git != nil {
			s = a.Git.SHA
		}
		fmt.Fprintf(h, "%s:%s\n", a.ID, s)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// missingEnsures lists the module's :ensure paths absent on disk, rendered
// module-relative as the lock records them.
func missingEnsures(e *hotEnv, m string, mod lockfile.Module) []string {
	var missing []string
	for _, p := range mod.PrepEnsure {
		if _, err := os.Stat(filepath.Join(e.root.Dir, m, p)); err != nil {
			missing = append(missing, filepath.Join(m, p))
		}
	}
	return missing
}

// readStamp loads m's prep stamp; a missing or corrupt stamp reads as
// stale.
func (e *hotEnv) readStamp(m string, mod lockfile.Module) (prepStamp, bool) {
	var s prepStamp
	b, err := os.ReadFile(filepath.Join(e.root.Dir, prepStampPath(m, mod)))
	if err != nil {
		return s, false
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, false
	}
	return s, true
}

// writeStamp records that m's prep ran and produced its output.
func (e *hotEnv) writeStamp(m string, mod lockfile.Module, st prepState) error {
	s := prepStamp{
		ManifestSHA256:  mod.ManifestSHA256,
		SourceDigest:    st.source,
		ClasspathDigest: st.class,
		Prep:            mod.PrepFn,
	}
	b, err := json.MarshalIndent(&s, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(e.root.Dir, prepStampPath(m, mod))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, append(b, '\n'), 0o644)
}

// runPrep launches m's :deps/prep-lib :fn on its locked prep-alias
// classpath, from the module's directory; the child's exit code
// propagates verbatim.
func (e *hotEnv) runPrep(ctx context.Context, m string, mod lockfile.Module) error {
	al, env, err := e.alias(m, mod.PrepAlias)
	if err != nil {
		return err
	}
	cp, err := e.cpOf(ctx, m, mod.PrepAlias)
	if err != nil {
		return err
	}
	expr := fmt.Sprintf("(let [f (requiring-resolve '%s)] (if f (f) (throw (Exception. \"rig: prep function %s not found\"))))",
		mod.PrepFn, mod.PrepFn)
	args := append([]string{}, mod.JVMOpts...)
	args = append(args, al.JVMOpts...)
	args = append(args, "-cp", cp, "clojure.main", "-e", expr)
	return launch(jvm.Run{Java: e.java, Args: args, Dir: e.modDir(m), Env: append(env, e.javaEnv...)})
}
