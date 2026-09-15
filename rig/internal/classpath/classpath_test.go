package classpath

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/brutasse/rig/internal/cache"
	"github.com/brutasse/rig/internal/fetch"
	"github.com/brutasse/rig/internal/lockfile"
)

const artifactBody = "artifact-bytes"

func shaOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// storeWithArtifact returns a store whose cache holds a file hashing to the
// sha recorded in a ForTest lock's single mvn artifact.
func storeWithArtifact(t *testing.T, doc *lockfile.Document) *cache.Store {
	t.Helper()
	store := cache.NewAt(t.TempDir())
	src := filepath.Join(t.TempDir(), "a.jar")
	if err := os.WriteFile(src, []byte(artifactBody), 0o644); err != nil {
		t.Fatal(err)
	}
	sha := shaOf(artifactBody)
	doc.Artifacts[0].SHA256 = sha
	if err := store.Add(src, sha); err != nil {
		t.Fatal(err)
	}
	return store
}

func setPaths(t *testing.T, doc *lockfile.Document, mod string, paths ...string) {
	t.Helper()
	m := doc.Modules[mod]
	m.Paths = paths
	doc.Modules[mod] = m
}

func TestBuildBase(t *testing.T) {
	ws := t.TempDir()
	doc := lockfile.ForTest(".")
	setPaths(t, doc, ".", "src", "resources")
	store := storeWithArtifact(t, doc)

	es, err := Build(context.Background(), fetch.New(false), store, "", "", ws, doc, ".", "")
	if err != nil {
		t.Fatal(err)
	}
	sha := doc.Artifacts[0].SHA256
	want := []string{
		filepath.Join(ws, "src"),
		filepath.Join(ws, "resources"),
		filepath.Join(store.Artifacts(), sha),
	}
	got := Flatten(es)
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestBuildAlias(t *testing.T) {
	ws := t.TempDir()
	doc := lockfile.ForTest(".")
	setPaths(t, doc, ".", "src")
	ref := doc.Artifacts[0].ID
	mod := doc.Modules["."]
	mod.Aliases = map[string]lockfile.Alias{
		"test": {
			Paths:     []string{"test"},
			Classpath: []lockfile.ClasspathEntry{{Ref: &ref}},
		},
	}
	doc.Modules["."] = mod
	store := storeWithArtifact(t, doc)

	es, err := Build(context.Background(), fetch.New(false), store, "", "", ws, doc, ".", "test")
	if err != nil {
		t.Fatal(err)
	}
	// alias extra-paths, module base paths, then classpath (tools.deps order)
	want := []string{
		filepath.Join(ws, "test"),
		filepath.Join(ws, "src"),
		filepath.Join(store.Artifacts(), doc.Artifacts[0].SHA256),
	}
	got := Flatten(es)
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestBuildLocalModule(t *testing.T) {
	ws := t.TempDir()
	doc := lockfile.ForTest(".", "modules/app")
	setPaths(t, doc, ".", "src")
	setPaths(t, doc, "modules/app", "src", "resources")
	store := storeWithArtifact(t, doc)

	app := "modules/app"
	root := doc.Modules["."]
	root.Classpath = append(root.Classpath, lockfile.ClasspathEntry{Local: &app})
	doc.Modules["."] = root

	es, err := Build(context.Background(), fetch.New(false), store, "", "", ws, doc, ".", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(ws, "src"),
		filepath.Join(store.Artifacts(), doc.Artifacts[0].SHA256),
		filepath.Join(ws, "modules", "app", "src"),
		filepath.Join(ws, "modules", "app", "resources"),
	}
	got := Flatten(es)
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestBuildGitDep(t *testing.T) {
	ws := t.TempDir()
	gitlibs := t.TempDir()
	doc := lockfile.ForTest(".")
	setPaths(t, doc, ".", "src")
	gsha := "b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1"
	gitID := "com.example/git-dep:0.1.0:jar"
	doc.Artifacts = append(doc.Artifacts, lockfile.Artifact{
		ID:        gitID,
		Kind:      "git",
		Group:     "com.example",
		Name:      "git-dep",
		Version:   "0.1.0",
		Extension: "jar",
		Git:       &lockfile.GitRef{URL: "https://example.invalid/git-dep.git", SHA: gsha},
		Paths:     []string{"src", "resources"},
	})
	store := storeWithArtifact(t, doc)
	ref := gitID
	m := doc.Modules["."]
	m.Classpath = append(m.Classpath, lockfile.ClasspathEntry{Ref: &ref})
	doc.Modules["."] = m
	if err := os.MkdirAll(filepath.Join(gitlibs, "com.example", "git-dep", gsha, "src"), 0o755); err != nil {
		t.Fatal(err)
	}

	es, err := Build(context.Background(), fetch.New(false), store, "", gitlibs, ws, doc, ".", "")
	if err != nil {
		t.Fatal(err)
	}
	got := Flatten(es)
	wantGit := []string{
		filepath.Join(gitlibs, "com.example", "git-dep", gsha, "src"),
		filepath.Join(gitlibs, "com.example", "git-dep", gsha, "resources"),
	}
	if len(got) != 1+1+2 { // src + mvn + 2 git paths
		t.Fatalf("got %d entries, want 4: %v", len(got), got)
	}
	for i, w := range wantGit {
		if got[2+i] != w {
			t.Errorf("git entry %d = %s, want %s", i, got[2+i], w)
		}
	}
}

func TestBuildGitDepMissing(t *testing.T) {
	ws := t.TempDir()
	doc := lockfile.ForTest(".")
	gsha := "b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1"
	gitID := "com.example/git-dep:0.1.0:jar"
	doc.Artifacts = append(doc.Artifacts, lockfile.Artifact{
		ID:        gitID,
		Kind:      "git",
		Group:     "com.example",
		Name:      "git-dep",
		Version:   "0.1.0",
		Extension: "jar",
		Git:       &lockfile.GitRef{URL: "u", SHA: gsha},
		Paths:     []string{"src"},
	})
	store := storeWithArtifact(t, doc)
	ref := gitID
	m := doc.Modules["."]
	m.Classpath = append(m.Classpath, lockfile.ClasspathEntry{Ref: &ref})
	doc.Modules["."] = m

	_, err := Build(context.Background(), fetch.New(false), store, "", t.TempDir(), ws, doc, ".", "")
	if err == nil || !errors.Is(err, ErrGitMissing) {
		t.Fatalf("err = %v, want git-missing", err)
	}
}

func TestBuildMvnOfflineMissing(t *testing.T) {
	ws := t.TempDir()
	doc := lockfile.ForTest(".")
	store := cache.NewAt(t.TempDir()) // empty cache

	_, err := Build(context.Background(), fetch.New(true), store, "", "", ws, doc, ".", "")
	if !errors.Is(err, fetch.ErrOffline) {
		t.Fatalf("err = %v, want fetch.ErrOffline", err)
	}
}

func TestBuildUnknownAlias(t *testing.T) {
	ws := t.TempDir()
	doc := lockfile.ForTest(".")
	store := storeWithArtifact(t, doc)
	_, err := Build(context.Background(), fetch.New(false), store, "", "", ws, doc, ".", "nope")
	if err == nil {
		t.Fatal("want error for unknown alias")
	}
}
