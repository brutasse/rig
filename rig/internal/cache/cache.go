package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/brutasse/rig/internal/digest"
)

var (
	ErrMissing  = errors.New("cache: artifact missing")
	ErrMismatch = errors.New("cache: hash mismatch")
)

var shaRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

var extRe = regexp.MustCompile(`^[0-9a-z]+$`)

type Store struct{ Root string }

func New() (*Store, error) {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		base = filepath.Join(home, ".local", "share")
	}
	return &Store{Root: filepath.Join(base, "rig")}, nil
}

func NewAt(root string) *Store { return &Store{Root: root} }

func (s *Store) Artifacts() string { return filepath.Join(s.Root, "artifacts") }

// ArtifactPath returns an artifact's cache path: "<sha256>.<extension>".
// The extension is part of the name on purpose: the JDK derives jar: URLs
// from the file suffix, and an extension-less classpath entry hands CLJS
// consumers file: URLs, breaking resource copying out of jars. An empty or
// malformed extension addresses the legacy extension-less layout.
func (s *Store) ArtifactPath(sha, ext string) string {
	if !extRe.MatchString(ext) {
		ext = ""
	}
	if ext == "" {
		return filepath.Join(s.Artifacts(), sha)
	}
	return filepath.Join(s.Artifacts(), sha+"."+ext)
}

func (s *Store) KernelDir(sha string) string {
	return filepath.Join(s.Root, "kernel", sha)
}

func (s *Store) KernelJar(sha string) string {
	return filepath.Join(s.KernelDir(sha), "rig-resolver.jar")
}

// RunnerJar is the rig.runner jar of the kernel identified by sha,
// placed there at install time (image build, or first-use fetch).
func (s *Store) RunnerJar(sha string) string {
	return filepath.Join(s.Root, "runner", sha, "rig-runner.jar")
}

func checkSHA(sha string) error {
	if !shaRe.MatchString(sha) {
		return fmt.Errorf("cache: bad sha %q", sha)
	}
	return nil
}

func (s *Store) Get(sha, ext string) (string, error) {
	if err := checkSHA(sha); err != nil {
		return "", err
	}
	p := s.ArtifactPath(sha, ext)
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	if ext == "" {
		return "", ErrMissing
	}
	// A legacy extension-less entry (pre-extension layout): adopt it into
	// the extension layout so a classpath never carries an extension-less
	// name again. A losing rename race means a concurrent Get already
	// adopted it; the entry itself is the same verified content either way.
	old := s.ArtifactPath(sha, "")
	if _, err := os.Stat(old); err != nil {
		return "", ErrMissing
	}
	if err := os.Rename(old, p); err != nil {
		if _, err := os.Stat(p); err != nil {
			return "", ErrMissing
		}
	}
	return p, nil
}

func (s *Store) Add(src, sha, ext string) error {
	if err := checkSHA(sha); err != nil {
		return err
	}
	if err := os.MkdirAll(s.Artifacts(), 0o755); err != nil {
		return err
	}
	dst := s.ArtifactPath(sha, ext)
	// A unique temp per call: concurrent Adds of the same sha (identical
	// content served from different URLs) must not collide on one shared
	// "<sha>.tmp" — the second rename would fail with ENOENT.
	tmp, err := os.CreateTemp(s.Artifacts(), "."+sha+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := copyFile(src, name); err != nil {
		return err
	}
	return os.Rename(name, dst)
}

// RecordedSHA returns the sha256 previously recorded for url, if any. Callers
// must still verify the cached artifact before use.
func (s *Store) RecordedSHA(url string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(s.Root, "url-index", urlKey(url)))
	if err != nil {
		return "", false
	}
	sha := strings.TrimSpace(string(b))
	if !shaRe.MatchString(sha) {
		return "", false
	}
	return sha, true
}

// RecordURL records the sha256 computed for url (best effort).
func (s *Store) RecordURL(url, sha string) {
	if err := checkSHA(sha); err != nil {
		return
	}
	dir := filepath.Join(s.Root, "url-index")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	key := urlKey(url)
	tmp := filepath.Join(dir, key+".tmp")
	if err := os.WriteFile(tmp, []byte(sha), 0o644); err != nil {
		return
	}
	os.Rename(tmp, filepath.Join(dir, key))
}

func urlKey(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:])
}

func (s *Store) Verify(sha, ext string) error {
	p, err := s.Get(sha, ext)
	if err != nil {
		return err
	}
	got, err := digest.File(p)
	if err != nil {
		return err
	}
	if got != sha {
		return ErrMismatch
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}
