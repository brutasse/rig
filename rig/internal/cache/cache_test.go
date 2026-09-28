package cache

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/brutasse/rig/internal/digest"
)

func makeArtifact(t *testing.T, content string) (path, sha string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "artifact.jar")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	sha, err := digest.File(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, sha
}

func TestAddGetVerify(t *testing.T) {
	store := NewAt(t.TempDir())
	path, sha := makeArtifact(t, "hello artifact")
	if err := store.Add(path, sha); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(sha)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := os.ReadFile(got); err != nil || string(c) != "hello artifact" {
		t.Errorf("cached content = %q, %v", c, err)
	}
	if err := store.Verify(sha); err != nil {
		t.Errorf("verify: %v", err)
	}
}

func TestGetMissing(t *testing.T) {
	store := NewAt(t.TempDir())
	_, sha := makeArtifact(t, "x")
	if _, err := store.Get(sha); !errors.Is(err, ErrMissing) {
		t.Errorf("get = %v, want ErrMissing", err)
	}
}

func TestVerifyMismatch(t *testing.T) {
	store := NewAt(t.TempDir())
	path, sha := makeArtifact(t, "hello artifact")
	if err := store.Add(path, sha); err != nil {
		t.Fatal(err)
	}
	cached, _ := store.Get(sha)
	if err := os.WriteFile(cached, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := store.Verify(sha)
	if !errors.Is(err, ErrMismatch) {
		t.Errorf("verify = %v, want ErrMismatch", err)
	}
	if errors.Is(err, ErrMissing) {
		t.Error("mismatch must not be ErrMissing")
	}
}

func TestBadSHA(t *testing.T) {
	store := NewAt(t.TempDir())
	if _, err := store.Get("xyz"); err == nil {
		t.Error("expected error for bad sha")
	}
	p, _ := makeArtifact(t, "x")
	if err := store.Add(p, "xyz"); err == nil {
		t.Error("expected error for bad sha")
	}
}

// TestAddConcurrentSameSHA is the CI flake: two repositories serve byte-identical
// artifacts at different URLs, so the lock fetches them in parallel and both
// Adds target the same sha. The staging temp must be unique per call; a shared
// "<sha>.tmp" lets the second rename hit a file the first already moved (ENOENT).
func TestAddConcurrentSameSHA(t *testing.T) {
	store := NewAt(t.TempDir())
	path, sha := makeArtifact(t, "identical bytes")
	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = store.Add(path, sha)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if err := store.Verify(sha); err != nil {
		t.Errorf("verify: %v", err)
	}
}

func TestAddSameHashTwice(t *testing.T) {
	store := NewAt(t.TempDir())
	path, sha := makeArtifact(t, "v1")
	if err := store.Add(path, sha); err != nil {
		t.Fatal(err)
	}
	path2, sha2 := makeArtifact(t, "v1")
	if sha != sha2 {
		t.Fatal("test bug: different hash")
	}
	if err := store.Add(path2, sha2); err != nil {
		t.Fatal(err)
	}
	if err := store.Verify(sha); err != nil {
		t.Errorf("verify: %v", err)
	}
}
