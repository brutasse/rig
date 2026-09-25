package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/brutasse/rig/internal/lockfile"
)

func writeLock(t *testing.T, dir string, modules ...string) *lockfile.Document {
	t.Helper()
	doc := lockfile.ForTest(modules...)
	if err := doc.Save(filepath.Join(dir, "deps.lock")); err != nil {
		t.Fatal(err)
	}
	return doc
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindWorkspaceFromNestedModule(t *testing.T) {
	root := t.TempDir()
	writeLock(t, root, ".", "modules/app")
	writeFile(t, filepath.Join(root, "deps.edn"), "{:rig/modules [\"modules/app\"]}\n")
	writeFile(t, filepath.Join(root, "modules", "app", "deps.edn"), "{:rig/lib x/app}\n")

	r, err := Find(filepath.Join(root, "modules", "app"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Dir != root {
		t.Errorf("dir = %s, want %s", r.Dir, root)
	}
	if !r.Workspace {
		t.Error("want workspace")
	}
	if r.Lock == nil {
		t.Fatal("want lock")
	}
	if got := r.Modules(); len(got) != 2 || got[1] != "modules/app" {
		t.Errorf("modules = %v", got)
	}
}

func TestFindSingleModule(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "deps.edn"), "{:rig/lib x/standalone}\n")

	r, err := Find(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Workspace {
		t.Error("don't want workspace")
	}
	if r.Lock != nil {
		t.Error("don't want lock")
	}
	if got := r.Modules(); len(got) != 1 || got[0] != "." {
		t.Errorf("modules = %v", got)
	}
}

func TestFindLeiningenFallback(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "project.clj"), "(defproject x/standalone \"1.0.0\")\n")

	r, err := Find(filepath.Join(root, "src"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Workspace {
		t.Error("don't want workspace")
	}
	if r.Lock != nil {
		t.Error("don't want lock")
	}
	if r.Dir != root {
		t.Errorf("dir = %s, want %s", r.Dir, root)
	}
	if got := r.Modules(); len(got) != 1 || got[0] != "." {
		t.Errorf("modules = %v", got)
	}
}

func TestFindDepsEdnBeatsProjectClj(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "project.clj"), "(defproject x/old \"1.0.0\")\n")
	writeFile(t, filepath.Join(dir, "deps.edn"), "{:rig/lib x/new}\n")

	r, err := Find(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Modules(); len(got) != 1 || got[0] != "." {
		t.Errorf("modules = %v", got)
	}
}

func TestFindWorkspaceBootstrapNoLock(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "deps.edn"), "{:rig/modules [\"modules/app\"]}\n")
	writeFile(t, filepath.Join(root, "modules", "app", "deps.edn"), "{:rig/lib x/app}\n")

	r, err := Find(filepath.Join(root, "modules", "app"))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Workspace {
		t.Error("want workspace (bootstrap)")
	}
	if r.Lock != nil {
		t.Error("don't want lock")
	}
}

func TestFindNotAProject(t *testing.T) {
	if _, err := Find(t.TempDir()); !errors.Is(err, ErrNotAProject) {
		t.Errorf("err = %v, want ErrNotAProject", err)
	}
}

func TestFindBadLockFails(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "deps.lock"), "{bad json")
	if _, err := Find(dir); err == nil {
		t.Error("want error for corrupt lock")
	}
}

func TestResolve(t *testing.T) {
	root := t.TempDir()
	writeLock(t, root, ".", "modules/app", "modules/lib")

	r, err := Find(root)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		in   string
		want string
	}{
		{"", "."},
		{".", "."},
		{"modules/app", "modules/app"},
	}
	for _, c := range cases {
		got, err := r.Resolve(c.in)
		if err != nil {
			t.Errorf("Resolve(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Resolve(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if _, err := r.Resolve("modules/ghost"); err == nil {
		t.Error("want error for unknown module")
	}
}

func TestResolveAbsolutePath(t *testing.T) {
	root := t.TempDir()
	writeLock(t, root, ".", "modules/app")
	writeFile(t, filepath.Join(root, "modules", "app", "deps.edn"), "{}\n")

	r, err := Find(root)
	if err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(root, "modules", "app")
	got, err := r.Resolve(abs)
	if err != nil {
		t.Fatal(err)
	}
	if got != "modules/app" {
		t.Errorf("resolve abs = %q", got)
	}
}

func TestTargetDir(t *testing.T) {
	root := t.TempDir()
	writeLock(t, root, ".", "modules/app")
	r, _ := Find(root)

	if got := r.TargetDir("modules/app"); got != "target" {
		t.Errorf("target dir = %q, want target (default class-dir)", got)
	}
	if got := r.TargetDir("modules/ghost"); got != "target" {
		t.Errorf("target dir = %q, want target fallback", got)
	}
}
