package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/brutasse/rig/internal/cache"
	"github.com/brutasse/rig/internal/classpath"
	"github.com/brutasse/rig/internal/fetch"
	"github.com/brutasse/rig/internal/jvm"
	"github.com/brutasse/rig/internal/kernel"
	"github.com/brutasse/rig/internal/lockfile"
	"github.com/brutasse/rig/internal/workspace"
)

// hotLock returns the workspace root with a fresh, non-stale lock, applying
// the stale-lock policy (design §"Lock staleness"):
//
//   - no lock at all           -> exit 3 with a hint to run 'rig lock'
//   - stale + --frozen          -> exit 3, lock untouched (CI)
//   - stale (default, dev)      -> re-resolve and refresh the lock, print one
//     "relocked" line, and continue with the fresh lock.
//
// Staleness is detected by recomputing each manifest's sha256 and comparing
// it to the value recorded in the lock (no EDN parsing).
func (o *opts) hotLock(ctx context.Context) (*workspace.Root, *lockfile.Document, error) {
	root, err := workspace.Find(".")
	if err != nil {
		return nil, nil, err
	}
	if root.Lock == nil {
		return nil, nil, exitf(3, "no lock at %s (run 'rig lock')", root.LockPath())
	}
	stale, err := root.Lock.Stale(root.Dir)
	if err != nil {
		return nil, nil, err
	}
	if len(stale) > 0 {
		if o.frozen {
			return nil, nil, exitf(3,
				"lock is stale (manifest changed in: %s); run 'rig lock' to refresh",
				strings.Join(stale, ", "))
		}
		lock, err := o.relock(ctx, root)
		if err != nil {
			return nil, nil, err
		}
		fmt.Printf("relocked (stale: %s)\n", strings.Join(stale, ", "))
		return root, lock, nil
	}
	return root, root.Lock, nil
}

// hotEnv bundles what the hot commands (test, run, repl, exec, build) need
// once the stale-lock policy has been applied.
type hotEnv struct {
	root    *workspace.Root
	lock    *lockfile.Document
	store   *cache.Store
	client  *fetch.Client
	m2root  string
	gitlibs string
	java    string
	javaEnv []string
	kernel  string // empty until needed (hot(ctx, false) callers)
	runner  string // rig.runner jar, appended last on the project's locked classpath
	offline bool
	oidc    func(repo string) (string, bool) // bearer for :auth :oidc repos
}

// runnerClojureFloor is the lowest org.clojure/clojure the rig.runner
// entry point loads on: the gen-class wrapper's static initializer calls
// clojure.lang.Util/loadWithClass(String, Class) and modern-compiled
// namespace bodies reference clojure.lang.Tuple, both absent from
// Clojure 1.7.x.
const runnerClojureFloor = "1.8.0"

// clojureBelowFloor returns module m's locked org.clojure/clojure version
// when it is below runnerClojureFloor, "" otherwise. Modules pinning no
// Clojure return "" too: the load stage only runs on modules with
// Clojure sources, and their classpath cannot host the runner anyway.
func (e *hotEnv) clojureBelowFloor(m string) string {
	mod, ok := e.lock.Modules[m]
	if !ok {
		return ""
	}
	byID := make(map[string]lockfile.Artifact, len(e.lock.Artifacts))
	for _, a := range e.lock.Artifacts {
		byID[a.ID] = a
	}
	for _, ent := range mod.Classpath {
		if ent.Ref == nil {
			continue
		}
		a, ok := byID[*ent.Ref]
		if !ok || a.Group != "org.clojure" || a.Name != "clojure" {
			continue
		}
		if versionBelow(a.Version, runnerClojureFloor) {
			return a.Version
		}
	}
	return ""
}

// versionBelow reports whether numeric dot-separated version v is strictly
// below floor ("1.7.0" < "1.8.0" < "1.10.0"). Trailing suffixes
// ("-alpha4") are ignored.
func versionBelow(v, floor string) bool {
	pick := func(s string) [2]int {
		var out [2]int
		for i, comp := range strings.Split(s, ".") {
			if i >= 2 {
				break
			}
			n := 0
			for _, c := range comp {
				if c < '0' || c > '9' {
					break
				}
				n = n*10 + int(c-'0')
			}
			out[i] = n
		}
		return out
	}
	vp, fp := pick(v), pick(floor)
	return vp[0] < fp[0] || (vp[0] == fp[0] && vp[1] < fp[1])
}

// hot applies the stale-lock policy and prepares the launch environment.
// needKernel fetches the kernel jar (the rig.fmt entry point lives in it;
// the rig.runner entry point lives in the pin's runner jar, resolved by
// Pin.Runner where needed).
func (o *opts) hot(ctx context.Context, needKernel bool) (*hotEnv, error) {
	root, lock, err := o.hotLock(ctx)
	if err != nil {
		return nil, err
	}
	if err := o.resolve(ctx, root); err != nil {
		return nil, err
	}
	e := &hotEnv{root: root, lock: lock, m2root: o.m2Root(), oidc: o.bearerFor(ctx, root), offline: o.offline}
	if e.store, err = o.store(); err != nil {
		return nil, err
	}
	e.client = o.fetchClient(ctx, root)
	if e.gitlibs, err = classpath.GitlibsRoot(); err != nil {
		return nil, err
	}
	if e.java, e.javaEnv, err = o.pickJava(ctx, e.store, root); err != nil {
		return nil, err
	}
	if needKernel {
		if e.kernel, err = kernel.Current.Ensure(ctx, e.store, o.offline); err != nil {
			return nil, err
		}
	}
	return e, nil
}

// resolveModule resolves the -p module against the workspace, defaulting to
// the root module. Works with or without a lock.
func resolveModule(root *workspace.Root, p string) (string, error) {
	if p == "" || p == "." {
		return ".", nil
	}
	return root.Resolve(p)
}

// moduleDir returns the on-disk directory of module m.
func moduleDir(root *workspace.Root, m string) string {
	if m == "." {
		return root.Dir
	}
	return filepath.Join(root.Dir, m)
}

// modDir returns the on-disk directory of module m.
func (e *hotEnv) modDir(m string) string { return moduleDir(e.root, m) }

// cpOf assembles the locked classpath of module m under alias a ("" = base)
// as a string joined with the OS path separator.
func (e *hotEnv) cpOf(ctx context.Context, m, a string) (string, error) {
	entries, err := e.entriesOf(ctx, m, a)
	if err != nil {
		return "", err
	}
	return strings.Join(classpath.Flatten(entries), string(filepath.ListSeparator)), nil
}

func (e *hotEnv) entriesOf(ctx context.Context, m, a string) ([]classpath.Entry, error) {
	return classpath.Build(ctx, e.client, e.store, e.m2root, e.gitlibs, e.root.Dir, e.lock, m, a)
}

// module returns module m of the lock, with a friendly error.
func (e *hotEnv) module(m string) (lockfile.Module, error) {
	mod, err := e.lock.Module(m)
	if err != nil {
		return lockfile.Module{}, exitf(2, "module %s is not in the lock (run 'rig lock')", m)
	}
	return mod, nil
}

// targetModule resolves the -p module, defaulting to the root module.
func (e *hotEnv) targetModule(p string) (string, error) {
	return resolveModule(e.root, p)
}

// alias returns the alias config of module m under name a (zero value when
// a == ""), the alias env as sorted KEY=VALUE entries, or an error.
func (e *hotEnv) alias(m, a string) (lockfile.Alias, []string, error) {
	if a == "" {
		return lockfile.Alias{}, nil, nil
	}
	mod, err := e.module(m)
	if err != nil {
		return lockfile.Alias{}, nil, err
	}
	al, ok := mod.Aliases[a]
	if !ok {
		return lockfile.Alias{}, nil, exitf(2, "module %s has no alias %q", m, a)
	}
	return al, envSlice(al.Env), nil
}

// launch runs r and propagates the child's exit code verbatim.
func launch(r jvm.Run) error {
	err := r.Run()
	if err == nil {
		return nil
	}
	if code := jvm.Code(err); code >= 0 {
		return exitCode(code)
	}
	return err
}

// envSlice returns m as sorted KEY=VALUE entries.
func envSlice(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k + "=" + m[k]
	}
	return out
}
