package lockfile

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/brutasse/rig/internal/digest"
)

func ManifestPath(root, moduleDir string) string {
	if moduleDir == "." {
		return filepath.Join(root, "deps.edn")
	}
	return filepath.Join(root, moduleDir, "deps.edn")
}

func (d *Document) Stale(root string) ([]string, error) {
	var stale []string
	if want := d.Workspace.ManifestSHA256; want != "" {
		got, err := digest.File(ManifestPath(root, "."))
		if err != nil || got != want {
			stale = append(stale, ".")
		}
	}
	for dir, m := range d.Modules {
		if dir == "." {
			continue
		}
		got, err := digest.File(ManifestPath(root, dir))
		if err != nil || got != m.ManifestSHA256 {
			stale = append(stale, dir)
		}
	}
	sort.Strings(stale)
	return stale, nil
}

func (d *Document) Module(dir string) (Module, error) {
	m, ok := d.Modules[dir]
	if !ok {
		return Module{}, fmt.Errorf("lockfile: unknown module %q", dir)
	}
	return m, nil
}
