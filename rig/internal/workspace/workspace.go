package workspace

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/brutasse/rig/internal/lockfile"
)

var ErrNotAProject = errors.New("workspace: not a rig project (no deps.edn, deps.lock or project.clj found)")

type Root struct {
	Dir       string
	Workspace bool
	Lock      *lockfile.Document
}

func Find(start string) (*Root, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return nil, err
	}
	var (
		workspaceFallback *Root
		singleFallback    *Root
		leinFallback      *Root
	)
	dir := abs
	for {
		lockPath := filepath.Join(dir, "deps.lock")
		if st, err := os.Stat(lockPath); err == nil && !st.IsDir() {
			doc, err := lockfile.Load(lockPath)
			if err != nil {
				return nil, err
			}
			return &Root{Dir: dir, Workspace: hasModules(doc.Workspace.Modules), Lock: doc}, nil
		}
		if manifest, err := os.ReadFile(filepath.Join(dir, "deps.edn")); err == nil {
			if bytes.Contains(manifest, []byte(":rig/modules")) {
				if workspaceFallback == nil {
					workspaceFallback = &Root{Dir: dir, Workspace: true}
				}
			} else if singleFallback == nil {
				singleFallback = &Root{Dir: dir}
			}
		} else if st, err := os.Stat(filepath.Join(dir, "project.clj")); err == nil && !st.IsDir() {
			// A Leiningen manifest is only a migration entry point (rig
			// migrate converts it to deps.edn); it is the last-resort root
			// marker, below any deps.edn found while walking up.
			if leinFallback == nil {
				leinFallback = &Root{Dir: dir}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	switch {
	case workspaceFallback != nil:
		return workspaceFallback, nil
	case singleFallback != nil:
		return singleFallback, nil
	case leinFallback != nil:
		return leinFallback, nil
	default:
		return nil, ErrNotAProject
	}
}

func hasModules(modules []string) bool {
	for _, m := range modules {
		if m != "." {
			return true
		}
	}
	return false
}

func (r *Root) Modules() []string {
	if r.Lock != nil {
		return r.Lock.Workspace.Modules
	}
	return []string{"."}
}

func (r *Root) LockPath() string {
	return filepath.Join(r.Dir, "deps.lock")
}

func (r *Root) Resolve(p string) (string, error) {
	if p == "" || p == "." {
		return ".", nil
	}
	p = filepath.ToSlash(filepath.Clean(p))
	for _, m := range r.Modules() {
		if m == p {
			return m, nil
		}
	}
	if abs, err := filepath.Abs(p); err == nil {
		if rel, err := filepath.Rel(r.Dir, abs); err == nil {
			rel = filepath.ToSlash(rel)
			if !strings.HasPrefix(rel, "..") {
				for _, m := range r.Modules() {
					if m == rel {
						return m, nil
					}
				}
				if _, err := os.Stat(filepath.Join(r.Dir, rel, "deps.edn")); err == nil {
					return rel, nil
				}
			}
		}
	}
	if _, err := os.Stat(filepath.Join(r.Dir, p, "deps.edn")); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("workspace: unknown module %q", p)
}

func (r *Root) TargetDir(module string) string {
	if r.Lock != nil {
		if m, err := r.Lock.Module(module); err == nil {
			return targetDirFrom(m)
		}
	}
	return "target"
}

func targetDirFrom(m lockfile.Module) string {
	cd := m.Build.ClassDir
	if i := strings.LastIndex(cd, "/"); i > 0 {
		return cd[:i]
	}
	return "target"
}
