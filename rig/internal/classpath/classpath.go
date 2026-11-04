// Package classpath assembles the JVM classpath of a locked module.
package classpath

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/brutasse/rig/internal/cache"
	"github.com/brutasse/rig/internal/fetch"
	"github.com/brutasse/rig/internal/lockfile"
)

// ErrGitMissing is wrapped when a git dep is not checked out under the
// gitlibs root.
var ErrGitMissing = errors.New("classpath: git dep missing")

// M2Path returns the local Maven repository path of a under m2root, or ""
// when the artifact has no mvn coordinates.
func M2Path(m2root string, a lockfile.Artifact) string {
	if m2root == "" || a.Group == "" || a.Name == "" || a.Version == "" || a.Extension == "" {
		return ""
	}
	name := a.Name + "-" + a.Version
	if a.Classifier != nil && *a.Classifier != "" {
		name += "-" + *a.Classifier
	}
	return filepath.Join(m2root, strings.ReplaceAll(a.Group, ".", "/"), a.Name, a.Version, name+"."+a.Extension)
}

// GitlibsRoot returns the git dependencies root: $GITLIBS/libs when GITLIBS
// is set, else ~/.gitlibs/libs — the same root tools.gitlibs (the kernel,
// the clojure CLI) checks out under.
func GitlibsRoot() (string, error) {
	if g := strings.TrimSpace(os.Getenv("GITLIBS")); g != "" {
		return filepath.Join(g, "libs"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gitlibs", "libs"), nil
}

// GitProcure provisions one missing git dep checkout under the gitlibs
// root (the kernel op git-procure: the same tools.gitlibs procure the lock
// runs). It returns nil once the checkout is in place.
type GitProcure func(ctx context.Context, a lockfile.Artifact) error

// Entry is one resolved classpath component, in lock order.
type Entry struct {
	ID    string
	Paths []string
}

// Flatten returns the classpath list of es.
func Flatten(es []Entry) []string {
	var cp []string
	for _, e := range es {
		cp = append(cp, e.Paths...)
	}
	return cp
}

// appendClassDir appends the module's compiled class dir to its own paths
// when the module declares :rig/java-src-dirs and the dir is not already
// among them: rig javacs those sources into the class dir, so those classes
// belong on the module's classpath even when the manifest does not name the
// dir (the way lein puts target/classes on the classpath). It goes last, so
// a module's sources shadow its stale compiled classes.
func appendClassDir(paths []string, mod lockfile.Module) []string {
	if len(mod.Build.JavaSrcDirs) == 0 || mod.Build.ClassDir == "" {
		return paths
	}
	for _, p := range paths {
		if p == mod.Build.ClassDir {
			return paths
		}
	}
	return append(append([]string{}, paths...), mod.Build.ClassDir)
}

// Build resolves the ordered classpath of module m (alias a, "" = base) of
// lock. mvn artifacts are secured in the store (cache, m2, repository); a
// missing git dep is provisioned by procure (nil or offline: the checkout
// must already be under the gitlibs root); local refs expand to the
// referenced module's paths. All returned paths are absolute, and the order
// is exactly the one recorded in the lock (alias extra-paths, module paths,
// then classpath entries).
func Build(ctx context.Context, client *fetch.Client, store *cache.Store, m2root, gitlibs, ws string, lock *lockfile.Document, m, alias string, procure GitProcure) ([]Entry, error) {
	mod, err := lock.Module(m)
	if err != nil {
		return nil, err
	}
	var (
		entries []lockfile.ClasspathEntry
		paths   []string
	)
	if alias != "" {
		al, ok := mod.Aliases[alias]
		if !ok {
			return nil, fmt.Errorf("classpath: module %q has no alias %q", m, alias)
		}
		// Full alias classpath (tools.deps order, verified against
		// `clojure -Spath` in T3): alias extra-paths, module base paths,
		// then the alias's merged classpath.
		paths = append(paths, al.Paths...)
		paths = append(paths, mod.Paths...)
		paths = appendClassDir(paths, mod)
		entries = al.Classpath
	} else {
		paths = appendClassDir(mod.Paths, mod)
		entries = mod.Classpath
	}

	arts := make(map[string]lockfile.Artifact, len(lock.Artifacts))
	for _, a := range lock.Artifacts {
		arts[a.ID] = a
	}

	modDir := ws
	if m != "." {
		modDir = filepath.Join(ws, m)
	}

	out := make([]Entry, 0, len(entries)+1)
	if len(paths) > 0 {
		p := make([]string, len(paths))
		for i, d := range paths {
			p[i] = filepath.Join(modDir, d)
		}
		out = append(out, Entry{ID: "paths:" + m, Paths: p})
	}

	for _, e := range entries {
		switch {
		case e.Ref != nil:
			a, ok := arts[*e.Ref]
			if !ok {
				return nil, fmt.Errorf("classpath: module %q references unknown artifact %q", m, *e.Ref)
			}
			switch a.Kind {
			case "mvn":
				p, err := secureMvn(ctx, client, store, m2root, a)
				if err != nil {
					return nil, err
				}
				out = append(out, Entry{ID: a.ID, Paths: []string{p}})
			case "git":
				p, err := gitPaths(ctx, client, gitlibs, a, procure)
				if err != nil {
					return nil, err
				}
				out = append(out, Entry{ID: a.ID, Paths: p})
			}
		case e.Local != nil:
			lm, ok := lock.Modules[*e.Local]
			if !ok {
				return nil, fmt.Errorf("classpath: module %q references unknown module %q", m, *e.Local)
			}
			lp := appendClassDir(lm.Paths, lm)
			p := make([]string, len(lp))
			for i, d := range lp {
				p[i] = filepath.Join(ws, *e.Local, d)
			}
			out = append(out, Entry{ID: "local:" + *e.Local, Paths: p})
		}
	}
	return out, nil
}

func secureMvn(ctx context.Context, client *fetch.Client, store *cache.Store, m2root string, a lockfile.Artifact) (string, error) {
	if a.URL == "" {
		if p, err := store.Get(a.SHA256, a.Extension); err == nil && store.Verify(a.SHA256, a.Extension) == nil {
			return p, nil
		}
		return "", fmt.Errorf("classpath: artifact %s is not in the cache and has no download URL", a.ID)
	}
	p, _, err := client.Get(ctx, store, fetch.Item{URL: a.URL, Repo: a.Repository, Ext: a.Extension, Local: M2Path(m2root, a), SHA: a.SHA256})
	return p, err
}

func gitPaths(ctx context.Context, client *fetch.Client, gitlibs string, a lockfile.Artifact, procure GitProcure) ([]string, error) {
	base := filepath.Join(gitlibs, a.Group, a.Name, a.Git.SHA)
	if st, err := os.Stat(base); err != nil || !st.IsDir() {
		if a.Git.URL == "" {
			return nil, fmt.Errorf("%w: %s has no git url in the lock (run 'rig lock' online)", ErrGitMissing, a.ID)
		}
		if client.Offline || procure == nil {
			return nil, fmt.Errorf("%w: %s not found at %s (restore the gitlibs cache or run online)", ErrGitMissing, a.ID, base)
		}
		if err := procure(ctx, a); err != nil {
			return nil, err
		}
		if st, err := os.Stat(base); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("%w: %s not found at %s after provisioning", ErrGitMissing, a.ID, base)
		}
	}
	p := make([]string, len(a.Paths))
	for i, d := range a.Paths {
		p[i] = filepath.Join(base, d)
	}
	return p, nil
}
