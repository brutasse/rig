package fetch

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/brutasse/rig/internal/cache"
)

func shaOf(t *testing.T, content string) string {
	t.Helper()
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

// m2File writes an m2-style artifact plus its Maven .sha1 record under dir.
func m2File(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	s := sha1.Sum([]byte(content))
	if err := os.WriteFile(p+".sha1", []byte(hex.EncodeToString(s[:])+" "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func testServer(t *testing.T, files map[string]string, lastModified map[string]time.Time) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for path, content := range files {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if lm, ok := lastModified[path]; ok {
				w.Header().Set("Last-Modified", lm.UTC().Format(http.TimeFormat))
			}
			if r.Method == http.MethodHead {
				return
			}
			w.Write([]byte(content))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestGetDownloadsAndCaches(t *testing.T) {
	srv := testServer(t, map[string]string{"/a.jar": "artifact-bytes"}, nil)
	store := cache.NewAt(t.TempDir())
	c := New(false)
	sha := shaOf(t, "artifact-bytes")
	it := Item{URL: srv.URL + "/a.jar", SHA: sha}

	p, fromCache, err := c.Get(context.Background(), store, it)
	if err != nil {
		t.Fatal(err)
	}
	if fromCache {
		t.Error("first get should be a fetch")
	}
	if filepath.Base(p) != sha {
		t.Errorf("cached path = %s", p)
	}

	_, fromCache, err = c.Get(context.Background(), store, it)
	if err != nil {
		t.Fatal(err)
	}
	if !fromCache {
		t.Error("second get should be cached")
	}
}

// TestGetSendsRigUserAgent: wire requests must identify as rig; Maven
// Central answers the Go default User-Agent with 429.
func TestGetSendsRigUserAgent(t *testing.T) {
	seen := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("User-Agent")
		w.Write([]byte("artifact-bytes"))
	}))
	t.Cleanup(srv.Close)
	store := cache.NewAt(t.TempDir())
	c := New(false)
	if _, _, err := c.Get(context.Background(), store, Item{URL: srv.URL + "/a.jar", SHA: shaOf(t, "artifact-bytes")}); err != nil {
		t.Fatal(err)
	}
	if seen != "rig" {
		t.Errorf("User-Agent = %q, want %q", seen, "rig")
	}
}

func TestGetHashMismatch(t *testing.T) {
	srv := testServer(t, map[string]string{"/a.jar": "artifact-bytes"}, nil)
	store := cache.NewAt(t.TempDir())
	c := New(false)
	if _, _, err := c.Get(context.Background(), store, Item{URL: srv.URL + "/a.jar", SHA: shaOf(t, "other-bytes")}); !errors.Is(err, cache.ErrMismatch) {
		t.Errorf("err = %v, want ErrMismatch", err)
	}
}

func TestGetOfflineMissing(t *testing.T) {
	srv := testServer(t, map[string]string{"/a.jar": "artifact-bytes"}, nil)
	store := cache.NewAt(t.TempDir())
	c := New(true)
	if _, _, err := c.Get(context.Background(), store, Item{URL: srv.URL + "/a.jar", SHA: shaOf(t, "artifact-bytes")}); !errors.Is(err, ErrOffline) {
		t.Errorf("err = %v, want ErrOffline", err)
	}
}

func TestGetOfflineCached(t *testing.T) {
	srv := testServer(t, map[string]string{"/a.jar": "artifact-bytes"}, nil)
	store := cache.NewAt(t.TempDir())
	c := New(false)
	sha := shaOf(t, "artifact-bytes")
	it := Item{URL: srv.URL + "/a.jar", SHA: sha}
	if _, _, err := c.Get(context.Background(), store, it); err != nil {
		t.Fatal(err)
	}
	co := New(true)
	if _, fromCache, err := co.Get(context.Background(), store, it); err != nil || !fromCache {
		t.Errorf("offline cached get: %v, %v", fromCache, err)
	}
}

func TestGetRefetchAfterCorruption(t *testing.T) {
	srv := testServer(t, map[string]string{"/a.jar": "artifact-bytes"}, nil)
	store := cache.NewAt(t.TempDir())
	c := New(false)
	sha := shaOf(t, "artifact-bytes")
	it := Item{URL: srv.URL + "/a.jar", SHA: sha}
	p, _, err := c.Get(context.Background(), store, it)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, fromCache, err := c.Get(context.Background(), store, it); err != nil || fromCache {
		t.Errorf("refetch after corruption: %v, %v", fromCache, err)
	}
}

func TestGetFromLocalM2(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n++ }))
	t.Cleanup(srv.Close)
	local := m2File(t, t.TempDir(), "a.jar", "artifact-bytes")
	store := cache.NewAt(t.TempDir())
	p, fromCache, err := New(false).Get(context.Background(), store, Item{URL: srv.URL + "/a.jar", Local: local, SHA: shaOf(t, "artifact-bytes")})
	if err != nil {
		t.Fatal(err)
	}
	if fromCache {
		t.Error("should be a fetch (m2 import), not a cache hit")
	}
	if filepath.Base(p) != shaOf(t, "artifact-bytes") {
		t.Errorf("path = %s", p)
	}
	if n != 0 {
		t.Errorf("requests = %d, want 0 (m2 hit)", n)
	}
}

func TestGetLocalMismatchFallsBack(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Write([]byte("artifact-bytes"))
	}))
	t.Cleanup(srv.Close)
	local := m2File(t, t.TempDir(), "a.jar", "wrong-bytes")
	if _, _, err := New(false).Get(context.Background(), cache.NewAt(t.TempDir()), Item{URL: srv.URL + "/a.jar", Local: local, SHA: shaOf(t, "artifact-bytes")}); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("requests = %d, want 1 (m2 mismatch falls back to repo)", n)
	}
}

func TestGetOfflineFromLocalM2(t *testing.T) {
	local := m2File(t, t.TempDir(), "a.jar", "artifact-bytes")
	if _, _, err := New(true).Get(context.Background(), cache.NewAt(t.TempDir()), Item{URL: "https://x/a.jar", Local: local, SHA: shaOf(t, "artifact-bytes")}); err != nil {
		t.Errorf("offline m2 get: %v", err)
	}
}

func TestGetNewComputesAndCaches(t *testing.T) {
	srv := testServer(t, map[string]string{"/a.jar": "artifact-bytes"}, nil)
	store := cache.NewAt(t.TempDir())
	c := New(false)
	sha, p, err := c.GetNew(context.Background(), store, Item{URL: srv.URL + "/a.jar"})
	if err != nil {
		t.Fatal(err)
	}
	if sha != shaOf(t, "artifact-bytes") {
		t.Errorf("sha = %s", sha)
	}
	if filepath.Base(p) != sha {
		t.Errorf("cached path = %s", p)
	}
	if err := store.Verify(sha); err != nil {
		t.Errorf("cached artifact failed to verify: %v", err)
	}
}

func TestGetNewOffline(t *testing.T) {
	store := cache.NewAt(t.TempDir())
	c := New(true)
	if _, _, err := c.GetNew(context.Background(), store, Item{URL: "https://x/a.jar"}); !errors.Is(err, ErrOffline) {
		t.Errorf("err = %v, want ErrOffline", err)
	}
}

func TestGetNewFromLocalM2(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n++ }))
	t.Cleanup(srv.Close)
	local := m2File(t, t.TempDir(), "a.jar", "m2-new-bytes")
	sha, _, err := New(true).GetNew(context.Background(), cache.NewAt(t.TempDir()), Item{URL: srv.URL + "/a.jar", Local: local})
	if err != nil {
		t.Fatal(err)
	}
	if sha != shaOf(t, "m2-new-bytes") {
		t.Errorf("sha = %s", sha)
	}
	if n != 0 {
		t.Errorf("requests = %d, want 0 (m2 hit)", n)
	}
}

func TestGetNewLocalBadSha1FallsBack(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Write([]byte("repo-bytes"))
	}))
	t.Cleanup(srv.Close)
	local := filepath.Join(t.TempDir(), "a.jar")
	if err := os.WriteFile(local, []byte("poisoned"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local+".sha1", []byte("0000000000000000000000000000000000000000 a.jar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sha, _, err := New(false).GetNew(context.Background(), cache.NewAt(t.TempDir()), Item{URL: srv.URL + "/a.jar", Local: local})
	if err != nil {
		t.Fatal(err)
	}
	if sha != shaOf(t, "repo-bytes") {
		t.Errorf("sha = %s, want repo sha (poisoned m2 must fall back to repo)", sha)
	}
	if n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

func TestVerifyGetStrictOnCorruption(t *testing.T) {
	srv := testServer(t, map[string]string{"/a.jar": "artifact-bytes"}, nil)
	store := cache.NewAt(t.TempDir())
	c := New(false)
	sha := shaOf(t, "artifact-bytes")
	it := Item{URL: srv.URL + "/a.jar", SHA: sha}
	p, _, err := c.Get(context.Background(), store, it)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.VerifyGet(context.Background(), store, it); !errors.Is(err, cache.ErrMismatch) {
		t.Errorf("err = %v, want ErrMismatch (strict, no self-heal)", err)
	}
}

func TestVerifyGetOKAndOffline(t *testing.T) {
	srv := testServer(t, map[string]string{"/a.jar": "artifact-bytes"}, nil)
	store := cache.NewAt(t.TempDir())
	c := New(false)
	sha := shaOf(t, "artifact-bytes")
	it := Item{URL: srv.URL + "/a.jar", SHA: sha}
	if _, _, err := c.Get(context.Background(), store, it); err != nil {
		t.Fatal(err)
	}
	co := New(true)
	if _, fromCache, err := co.VerifyGet(context.Background(), store, it); err != nil || !fromCache {
		t.Errorf("offline cached verifyget: %v, %v", fromCache, err)
	}
	if _, _, err := co.VerifyGet(context.Background(), store, Item{URL: srv.URL + "/a.jar", SHA: shaOf(t, "absent")}); !errors.Is(err, ErrOffline) {
		t.Errorf("offline missing verifyget: %v, want ErrOffline", err)
	}
}

func TestFetchNewAll(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 10; i++ {
		files["/"+string(rune('a'+i))+".jar"] = "content-" + string(rune('a'+i))
	}
	srv := testServer(t, files, nil)
	items := make([]Item, 0, len(files))
	want := map[string]string{}
	for name, content := range files {
		items = append(items, Item{URL: srv.URL + name})
		want[srv.URL+name] = shaOf(t, content)
	}
	store := cache.NewAt(t.TempDir())
	got, err := FetchNewAll(context.Background(), New(false), store, items, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d shas, want %d", len(got), len(want))
	}
	for u, s := range want {
		if got[u] != s {
			t.Errorf("sha[%s] = %s, want %s", u, got[u], s)
		}
	}
}

func TestVerifyAllStopsOnCorruption(t *testing.T) {
	srv := testServer(t, map[string]string{"/ok.jar": "ok"}, nil)
	store := cache.NewAt(t.TempDir())
	c := New(false)
	sha := shaOf(t, "ok")
	it := Item{URL: srv.URL + "/ok.jar", SHA: sha}
	if _, _, err := c.Get(context.Background(), store, it); err != nil {
		t.Fatal(err)
	}
	p, _ := store.Get(sha)
	if err := os.WriteFile(p, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	items := []Item{it, it}
	_, _, err := VerifyAll(context.Background(), c, store, items, 2)
	if !errors.Is(err, cache.ErrMismatch) {
		t.Errorf("err = %v, want ErrMismatch", err)
	}
}

func TestGetNewCapsHugeRetryAfter(t *testing.T) {
	oldBudget, oldWait := fetchBudget, maxWait
	fetchBudget, maxWait = 4*time.Second, 400*time.Millisecond
	defer func() { fetchBudget, maxWait = oldBudget, oldWait }()

	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Retry-After", "300")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := time.Now()
	_, _, err := New(false).GetNew(ctx, cache.NewAt(t.TempDir()), Item{URL: srv.URL + "/a.jar"})
	if err == nil {
		t.Fatal("expected failure after budget")
	}
	// Capped: ~10 attempts in the 4s budget. Uncapped (bug): one 300s sleep,
	// cut short by the ctx timeout, so n stays at 1.
	if n < 4 {
		t.Errorf("attempts = %d, want >= 4 (Retry-After 300 must be capped at maxWait); took %v", n, time.Since(start))
	}
}

func TestGetNewReusesURLIndex(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Write([]byte("indexed-bytes"))
	}))
	t.Cleanup(srv.Close)
	store := cache.NewAt(t.TempDir())
	c := New(false)
	it := Item{URL: srv.URL + "/a.jar"}
	sha, _, err := c.GetNew(context.Background(), store, it)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("requests = %d, want 1", n)
	}
	// second call served from url-index + cache, no network
	if got, _, err := c.GetNew(context.Background(), store, it); err != nil {
		t.Fatal(err)
	} else if got != sha {
		t.Errorf("sha = %s, want %s", got, sha)
	}
	if n != 1 {
		t.Errorf("requests = %d, want 1 (reused url-index)", n)
	}
	// offline also served from url-index
	if _, _, err := New(true).GetNew(context.Background(), store, it); err != nil {
		t.Errorf("offline url-index reuse: %v", err)
	}
	if n != 1 {
		t.Errorf("requests = %d, want 1 (offline reuse)", n)
	}
}

func TestGetNewRetriesOn429(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte("retry-bytes"))
	}))
	t.Cleanup(srv.Close)
	sha, _, err := New(false).GetNew(context.Background(), cache.NewAt(t.TempDir()), Item{URL: srv.URL + "/a.jar"})
	if err != nil {
		t.Fatal(err)
	}
	if sha != shaOf(t, "retry-bytes") {
		t.Errorf("sha = %s", sha)
	}
	if n != 3 {
		t.Errorf("attempts = %d, want 3", n)
	}
}

func TestGetNewWithCredentials(t *testing.T) {
	const user, pass = "alice", "s3cret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != user || p != pass {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte("private-artifact-bytes"))
	}))
	t.Cleanup(srv.Close)
	c := New(false)
	c.CredFor = func(repo string) (string, string, bool) {
		if repo == "corp" {
			return user, pass, true
		}
		return "", "", false
	}
	// With credentials: success.
	sha, _, err := c.GetNew(context.Background(), cache.NewAt(t.TempDir()), Item{URL: srv.URL + "/a.jar", Repo: "corp"})
	if err != nil {
		t.Fatal(err)
	}
	if sha != shaOf(t, "private-artifact-bytes") {
		t.Errorf("sha = %s", sha)
	}
	// Without credentials (fresh store, so the url-index can't mask it): auth failure.
	if _, _, err := c.GetNew(context.Background(), cache.NewAt(t.TempDir()), Item{URL: srv.URL + "/a.jar", Repo: "central"}); err == nil {
		t.Error("expected auth failure for repo without credentials")
	}
	// No CredFor at all (fresh store): auth failure.
	if _, _, err := New(false).GetNew(context.Background(), cache.NewAt(t.TempDir()), Item{URL: srv.URL + "/a.jar", Repo: "corp"}); err == nil {
		t.Error("expected auth failure without CredFor")
	}
}

func TestGetNewWithBearer(t *testing.T) {
	const token = "tok-abc"
	var authMu sync.Mutex
	auths := map[string]string{} // repo-independent: recorded per request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authMu.Lock()
		auths[r.URL.Path] = r.Header.Get("Authorization")
		authMu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte("bearer-protected-bytes"))
	}))
	t.Cleanup(srv.Close)
	c := New(false)
	c.BearerFor = func(repo string) (string, bool) {
		if repo == "oidc" {
			return token, true
		}
		return "", false
	}
	// With the bearer: success, and the server saw the header.
	sha, _, err := c.GetNew(context.Background(), cache.NewAt(t.TempDir()), Item{URL: srv.URL + "/a.jar", Repo: "oidc"})
	if err != nil {
		t.Fatal(err)
	}
	if sha != shaOf(t, "bearer-protected-bytes") {
		t.Errorf("sha = %s", sha)
	}
	authMu.Lock()
	got := auths["/a.jar"]
	authMu.Unlock()
	if got != "Bearer "+token {
		t.Errorf("server saw Authorization = %q, want %q", got, "Bearer "+token)
	}
	// Unmarked repo: no bearer, auth failure (fresh store, so the
	// url-index can't mask it).
	if _, _, err := c.GetNew(context.Background(), cache.NewAt(t.TempDir()), Item{URL: srv.URL + "/b.jar", Repo: "central"}); err == nil {
		t.Error("expected auth failure for repo without a bearer")
	}
	// BearerFor takes precedence over CredFor: the server sees the bearer,
	// not basic auth.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte("ok"))
	}))
	t.Cleanup(srv2.Close)
	c.CredFor = func(repo string) (string, string, bool) { return "alice", "s3cret", true }
	if _, _, err := c.GetNew(context.Background(), cache.NewAt(t.TempDir()), Item{URL: srv2.URL + "/a.jar", Repo: "oidc"}); err != nil {
		t.Errorf("bearer should have won over basic auth: %v", err)
	}
}

func TestFetchAll(t *testing.T) {
	files := map[string]string{}
	items := []Item{}
	for i := 0; i < 10; i++ {
		path := string(rune('a'+i)) + ".jar"
		files["/"+path] = "content-" + string(rune('a'+i))
		items = append(items, Item{SHA: shaOf(t, "content-"+string(rune('a'+i)))})
	}
	srv := testServer(t, files, nil)
	for i := range items {
		items[i].URL = srv.URL + "/" + string(rune('a'+i)) + ".jar"
	}
	store := cache.NewAt(t.TempDir())
	c := New(false)
	cached, fetched, err := FetchAll(context.Background(), c, store, items, 3)
	if err != nil {
		t.Fatal(err)
	}
	if cached != 0 || fetched != 10 {
		t.Errorf("cached=%d fetched=%d, want 0/10", cached, fetched)
	}
	cached, fetched, err = FetchAll(context.Background(), c, store, items, 3)
	if err != nil {
		t.Fatal(err)
	}
	if cached != 10 || fetched != 0 {
		t.Errorf("cached=%d fetched=%d, want 10/0", cached, fetched)
	}
}

func TestFetchAllStopsOnFirstError(t *testing.T) {
	srv := testServer(t, map[string]string{"/ok.jar": "ok"}, nil)
	store := cache.NewAt(t.TempDir())
	c := New(false)
	items := []Item{
		{URL: srv.URL + "/ok.jar", SHA: shaOf(t, "ok")},
		{URL: srv.URL + "/ok.jar", SHA: shaOf(t, "WRONG")},
		{URL: srv.URL + "/ok.jar", SHA: shaOf(t, "ok")},
	}
	_, _, err := FetchAll(context.Background(), c, store, items, 2)
	if !errors.Is(err, cache.ErrMismatch) {
		t.Errorf("err = %v, want ErrMismatch", err)
	}
}

func TestLastModified(t *testing.T) {
	when := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	srv := testServer(t, map[string]string{"/a.jar": "x"}, map[string]time.Time{"/a.jar": when})
	c := New(false)
	got, ok := c.LastModified(context.Background(), srv.URL+"/a.jar")
	if !ok {
		t.Fatal("no last-modified")
	}
	if !got.Equal(when) {
		t.Errorf("last-modified = %v, want %v", got, when)
	}
	_, ok = c.LastModified(context.Background(), srv.URL+"/missing")
	if ok {
		t.Error("expected no last-modified for 404")
	}
}
