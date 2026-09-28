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
	"github.com/brutasse/rig/internal/kernel"
	"github.com/brutasse/rig/internal/lockfile"
)

// prepStamp is the on-disk record that a module's prep output is up to date.
// It lives under the module's target dir, so 'rig clean' removes it together
// with the output.
type prepStamp struct {
	ManifestSHA256  string `json:"manifest_sha256"`
	SourceDigest    string `json:"source_digest"`
	ClasspathDigest string `json:"classpath_digest"`
	Prep            string `json:"prep"`
}

const (
	// prepStampFile records that the module's :deps/prep-lib fn ran.
	prepStampFile = ".rig-prep.json"
	// javacStampFile records that rig's own javac of the module's declared
	// java sources ran.
	javacStampFile = ".rig-javac.json"
)

// hasPrep reports whether the module declares a :deps/prep-lib prep fn.
func hasPrep(mod lockfile.Module) bool { return len(mod.PrepEnsure) > 0 }

// hasJava reports whether the module declares java sources rig must
// compile for it.
func hasJava(mod lockfile.Module) bool { return len(mod.Build.JavaSrcDirs) > 0 }

// ensurePreps prepares the target module and every local module on its
// classpath for the operation about to run. Two kinds of preparation:
//
//   - rig javacs modules that declare :rig/java-src-dirs, on the module's
//     locked base classpath, into its class-dir (the same b/javac a full
//     module build runs);
//   - rig runs each module's :deps/prep-lib :fn on its locked prep-alias
//     classpath and verifies the declared :ensure output.
//
// A module declaring both is prepared with the javac first: the kernel
// build wipes the class-dir, so the javac must not run after the fn, and
// the fn's classpath leads with the javac'd classes. Each kind keeps its
// own stamp and staleness state. Stale preps run in dependency order; a
// re-prepped dependency re-preps its dependents in the same invocation.
//
// targetJava reports whether the caller needs the target's own java
// compiled here: test and run do (they never compile the target); build,
// install and publish compile it as part of the module build.
func (e *hotEnv) ensurePreps(ctx context.Context, m string, mod lockfile.Module, targetJava bool) error {
	set, err := prepSet(e, m, mod, targetJava)
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
		var jst, pst *prepState
		if hasJava(pmod) {
			st, err := e.javacState(dir, pmod, ran)
			if err != nil {
				return err
			}
			if st.stale {
				jst = &st
			}
		}
		if hasPrep(pmod) {
			st, err := e.prepState(dir, pmod, ran)
			if err != nil {
				return err
			}
			if st.stale {
				pst = &st
			}
		}
		if jst == nil && pst == nil {
			continue
		}
		if jst != nil {
			fmt.Printf("prep %s: javac (%s)\n", dir, jst.reason)
			if err := e.runJavaPrep(ctx, dir, pmod); err != nil {
				return fmt.Errorf("prep %s: %w", dir, err)
			}
		}
		if pst != nil {
			fmt.Printf("prep %s: %s (%s)\n", dir, pmod.PrepFn, pst.reason)
			if err := e.runPrep(ctx, dir, pmod); err != nil {
				return fmt.Errorf("prep %s: %w", dir, err)
			}
		}
		if missing := missingEnsures(e, dir, pmod); len(missing) > 0 {
			return exitf(1, "prep %s did not produce: %s", dir, strings.Join(missing, ", "))
		}
		ran[dir] = true
		// Stamps last: the prep fn may have wiped the module's target dir
		// (and the javac stamp with it); both are re-recorded from this run.
		if jst != nil {
			if err := e.writeStamp(dir, pmod, *jst, "javac", javacStampFile); err != nil {
				return fmt.Errorf("prep %s: %w", dir, err)
			}
		}
		if pst != nil {
			if err := e.writeStamp(dir, pmod, *pst, pmod.PrepFn, prepStampFile); err != nil {
				return fmt.Errorf("prep %s: %w", dir, err)
			}
		}
	}
	return nil
}

// prepSet is the set of modules whose prep matters for target m: m itself
// plus every local module on its (transitively locked) classpath that
// declares :ensure output or java sources rig must compile. It errors when
// a member declares :ensure output rig cannot run for it (a lock predating
// prep support).
func prepSet(e *hotEnv, m string, mod lockfile.Module, targetJava bool) ([]string, error) {
	seen := map[string]bool{}
	var set []string
	add := func(dir string, isTarget bool) error {
		if seen[dir] {
			return nil
		}
		seen[dir] = true
		dep, ok := e.lock.Modules[dir]
		if !ok {
			return nil
		}
		needsJava := hasJava(dep) && (!isTarget || targetJava)
		if !hasPrep(dep) && !needsJava {
			return nil
		}
		if hasPrep(dep) && (dep.PrepAlias == "" || dep.PrepFn == "") {
			return exitf(2, "lock for %s predates prep support (prep-ensure without prep-fn); run 'rig lock'", dir)
		}
		set = append(set, dir)
		return nil
	}
	if err := add(m, true); err != nil {
		return nil, err
	}
	for _, en := range mod.Classpath {
		if en.Local == nil {
			continue
		}
		if err := add(*en.Local, false); err != nil {
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

// javacState is the staleness verdict for rig's own javac of m's declared
// java sources: missing class-dir, a missing or out-of-date stamp
// (manifest, sources or locked dependencies changed), or a local
// dependency that was re-prepped in this invocation.
func (e *hotEnv) javacState(m string, mod lockfile.Module, ran map[string]bool) (prepState, error) {
	src, err := e.sourceDigest(m, mod)
	if err != nil {
		return prepState{}, fmt.Errorf("digest sources of %s: %w", m, err)
	}
	cpd, err := classpathDigest(e, mod)
	if err != nil {
		return prepState{}, fmt.Errorf("digest classpath of %s: %w", m, err)
	}
	st := prepState{source: src, class: cpd}
	if _, err := os.Stat(filepath.Join(e.root.Dir, m, mod.Build.ClassDir)); err != nil {
		st.stale, st.reason = true, "missing " + filepath.Join(m, mod.Build.ClassDir)
		return st, nil
	}
	stamp, ok := e.readStamp(m, mod, javacStampFile)
	if !ok {
		st.stale, st.reason = true, "no stamp"
		return st, nil
	}
	switch {
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
				st.stale, st.reason = true, "dependency " + *en.Local + " re-prepped"
				return st, nil
			}
		}
	}
	return st, nil
}

// prepState decides whether m's prep fn must run: missing :ensure output,
// a missing or out-of-date stamp (manifest, sources or locked dependencies
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
	if missing := missingPrepEnsures(e, m, mod); len(missing) > 0 {
		st.stale, st.reason = true, "missing " + strings.Join(missing, ", ")
		return st, nil
	}
	stamp, ok := e.readStamp(m, mod, prepStampFile)
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
				st.stale, st.reason = true, "dependency " + *en.Local + " re-prepped"
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

// missingPrepEnsures lists the module's declared :ensure paths absent on
// disk, rendered module-relative as the lock records them.
func missingPrepEnsures(e *hotEnv, m string, mod lockfile.Module) []string {
	var missing []string
	for _, p := range mod.PrepEnsure {
		if _, err := os.Stat(filepath.Join(e.root.Dir, m, p)); err != nil {
			missing = append(missing, p)
		}
	}
	return missing
}

// missingEnsures lists the prep output absent on disk after a prep ran:
// the declared :ensure paths plus, for a module with declared java
// sources, its class-dir. Duplicates collapse; paths render
// module-relative.
func missingEnsures(e *hotEnv, m string, mod lockfile.Module) []string {
	paths := append([]string{}, mod.PrepEnsure...)
	if hasJava(mod) {
		paths = append(paths, mod.Build.ClassDir)
	}
	seen := map[string]bool{}
	var missing []string
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		if _, err := os.Stat(filepath.Join(e.root.Dir, m, p)); err != nil {
			missing = append(missing, filepath.Join(m, p))
		}
	}
	return missing
}

// readStamp loads m's prep stamp under file; a missing or corrupt stamp
// reads as stale.
func (e *hotEnv) readStamp(m string, mod lockfile.Module, file string) (prepStamp, bool) {
	var s prepStamp
	b, err := os.ReadFile(filepath.Join(e.root.Dir, m, filepath.Dir(mod.Build.ClassDir), file))
	if err != nil {
		return s, false
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, false
	}
	return s, true
}

// writeStamp records that m's prep ran and produced its output. prep is
// the record's Prep value: the prep fn name, or "javac".
func (e *hotEnv) writeStamp(m string, mod lockfile.Module, st prepState, prep, file string) error {
	s := prepStamp{
		ManifestSHA256:  mod.ManifestSHA256,
		SourceDigest:    st.source,
		ClasspathDigest: st.class,
		Prep:            prep,
	}
	b, err := json.MarshalIndent(&s, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(e.root.Dir, m, filepath.Dir(mod.Build.ClassDir), file)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, append(b, '\n'), 0o644)
}

// runJavaPrep compiles m's declared :rig/java-src-dirs into its class-dir
// through the kernel build path — the same b/javac a full module build
// runs — on the module's locked base classpath. No re-resolution: the
// classpath is rig-fetched and locked, so no in-JVM maven resolution runs.
func (e *hotEnv) runJavaPrep(ctx context.Context, m string, mod lockfile.Module) error {
	// run and repl open the environment without a kernel (hot ctx, false);
	// fetch it lazily here, only when a javac actually has to run.
	k := e.kernel
	if k == "" {
		var err error
		if k, err = kernel.Current.Ensure(ctx, e.store, e.offline); err != nil {
			return err
		}
	}
	entries, err := e.entriesOf(ctx, m, "")
	if err != nil {
		return err
	}
	cps := make([]map[string]any, 0, len(entries))
	for _, en := range entries {
		cps = append(cps, map[string]any{"id": en.ID, "paths": en.Paths})
	}
	dir := e.modDir(m)
	// src-dirs is an empty vector, not absent: tools.build's compile-clj
	// falls back to scanning the whole basis for namespaces when it is nil.
	cfg := map[string]any{
		"classpath":     cps,
		"src-dirs":      []string{},
		"java-src-dirs": absJoin(dir, mod.Build.JavaSrcDirs),
		"class-dir":     filepath.Join(dir, mod.Build.ClassDir),
	}
	if opts := javacOptsOf(e.lock.JVM, mod.Build.JavacOpts); len(opts) > 0 {
		cfg["javac-opts"] = opts
	}
	_, err = kernel.Call(ctx, k, e.java, kernel.Request{
		Op:        "build",
		Workspace: e.root.Dir,
		Modules:   []string{m},
		Args:      map[string]any{"builds": map[string]any{m: cfg}},
	})
	if err != nil {
		var oe *kernel.OpError
		if errors.As(err, &oe) {
			return exitf(1, "javac: %v", oe)
		}
		return err
	}
	return nil
}

// runPrep launches m's :deps/prep-lib :fn on its locked prep-alias
// classpath, from the module's directory; the child's exit code
// propagates verbatim. The fn is called with a single nil argument, the
// way tools.deps' exec-prep! invokes it when the prep alias declares no
// :exec-args.
func (e *hotEnv) runPrep(ctx context.Context, m string, mod lockfile.Module) error {
	al, env, err := e.alias(m, mod.PrepAlias)
	if err != nil {
		return err
	}
	cp, err := e.cpOf(ctx, m, mod.PrepAlias)
	if err != nil {
		return err
	}
	expr := fmt.Sprintf("(let [f (requiring-resolve '%s)] (if f (f nil) (throw (Exception. \"rig: prep function %s not found\"))))",
		mod.PrepFn, mod.PrepFn)
	args := append([]string{}, mod.JVMOpts...)
	args = append(args, al.JVMOpts...)
	args = append(args, "-cp", cp, "clojure.main", "-e", expr)
	return launch(jvm.Run{Java: e.java, Args: args, Dir: e.modDir(m), Env: append(env, e.javaEnv...)})
}
