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

// GitlibsRoot returns the git dependencies root (~/.gitlibs/libs).
func GitlibsRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gitlibs", "libs"), nil
}

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

// Build resolves the ordered classpath of module m (alias a, "" = base) of
// lock. mvn artifacts are secured in the store (cache, m2, repository); git
// deps must be present under the gitlibs root; local refs expand to the
// referenced module's paths. All returned paths are absolute, and the order
// is exactly the one recorded in the lock (alias extra-paths, module paths,
// then classpath entries).
func Build(ctx context.Context, client *fetch.Client, store *cache.Store, m2root, gitlibs, ws string, lock *lockfile.Document, m, alias string) ([]Entry, error) {
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
		entries = al.Classpath
	} else {
		paths = mod.Paths
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
				p, err := gitPaths(gitlibs, a)
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
			p := make([]string, len(lm.Paths))
			for i, d := range lm.Paths {
				p[i] = filepath.Join(ws, *e.Local, d)
			}
			out = append(out, Entry{ID: "local:" + *e.Local, Paths: p})
		}
	}
	return out, nil
}

func secureMvn(ctx context.Context, client *fetch.Client, store *cache.Store, m2root string, a lockfile.Artifact) (string, error) {
	if a.URL == "" {
		if p, err := store.Get(a.SHA256); err == nil && store.Verify(a.SHA256) == nil {
			return p, nil
		}
		return "", fmt.Errorf("classpath: artifact %s is not in the cache and has no download URL", a.ID)
	}
	p, _, err := client.Get(ctx, store, fetch.Item{URL: a.URL, Repo: a.Repository, Local: M2Path(m2root, a), SHA: a.SHA256})
	return p, err
}

func gitPaths(gitlibs string, a lockfile.Artifact) ([]string, error) {
	base := filepath.Join(gitlibs, a.Group, a.Name, a.Git.SHA)
	if st, err := os.Stat(base); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("%w: %s not found at %s (run 'rig lock')", ErrGitMissing, a.ID, base)
	}
	p := make([]string, len(a.Paths))
	for i, d := range a.Paths {
		p[i] = filepath.Join(base, d)
	}
	return p, nil
}
