package fetch

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brutasse/rig/internal/cache"
)

var ErrOffline = errors.New("fetch: offline")

var (
	maxWait     = 60 * time.Second
	fetchBudget = 3 * time.Minute
)

var sha1Re = regexp.MustCompile(`^[0-9a-f]{40}$`)

type Client struct {
	HTTP    *http.Client
	Offline bool
	// CredFor returns basic-auth credentials for a Maven repository id, if any.
	CredFor func(repo string) (username, password string, ok bool)
	// BearerFor returns the bearer token for a Maven repository id, if any.
	// Takes precedence over CredFor when it reports a token.
	BearerFor func(repo string) (token string, ok bool)
}

func New(offline bool) *Client {
	return &Client{HTTP: &http.Client{Timeout: 5 * time.Minute}, Offline: offline}
}

func (c *Client) authorize(req *http.Request, repo string) {
	if c.BearerFor != nil {
		if t, ok := c.BearerFor(repo); ok {
			req.Header.Set("Authorization", "Bearer "+t)
			return
		}
	}
	if c.CredFor == nil {
		return
	}
	if u, p, ok := c.CredFor(repo); ok {
		req.SetBasicAuth(u, p)
	}
}

// Item identifies an artifact to fetch: its repository URL and id, the local
// Maven repository copy when known (Local, "" when absent), and, for known
// artifacts, the expected sha256 (SHA).
type Item struct {
	URL   string
	Repo  string
	Local string
	SHA   string
}

// Get returns the artifact path for it.SHA: from the cache when verified,
// else imported from the local m2 copy when it hashes to it.SHA, else
// downloaded (and verified) from the repository.
func (c *Client) Get(ctx context.Context, store *cache.Store, it Item) (string, bool, error) {
	if p, err := store.Get(it.SHA); err == nil {
		if err := store.Verify(it.SHA); err == nil {
			return p, true, nil
		}
	}
	if p, ok := fromLocal(store, it, it.SHA); ok {
		return p, false, nil
	}
	if c.Offline {
		return "", false, fmt.Errorf("%w: %s not in cache", ErrOffline, it.SHA)
	}
	p, err := c.download(ctx, store, it)
	return p, false, err
}

// VerifyGet is Get for the security gate: a cache entry that fails to verify
// is reported as a mismatch (never re-fetched), so tampering is surfaced, not
// masked. Missing entries are taken from m2 or the repository and checked.
func (c *Client) VerifyGet(ctx context.Context, store *cache.Store, it Item) (string, bool, error) {
	if _, err := store.Get(it.SHA); err == nil {
		if err := store.Verify(it.SHA); err == nil {
			return store.ArtifactPath(it.SHA), true, nil
		}
		return "", false, fmt.Errorf("%w: %s", cache.ErrMismatch, it.SHA)
	}
	if p, ok := fromLocal(store, it, it.SHA); ok {
		return p, false, nil
	}
	if c.Offline {
		return "", false, fmt.Errorf("%w: %s not in cache", ErrOffline, it.SHA)
	}
	p, err := c.download(ctx, store, it)
	return p, false, err
}

// GetNew obtains the artifact it (whose sha is unknown) and returns
// (sha, path, error). Sources in order: url-index hit that still verifies in
// the cache; the local m2 copy, when self-consistent with the Maven .sha1
// recorded beside it; a download from the repository.
func (c *Client) GetNew(ctx context.Context, store *cache.Store, it Item) (string, string, error) {
	if sha, ok := store.RecordedSHA(it.URL); ok {
		if _, err := store.Get(sha); err == nil && store.Verify(sha) == nil {
			return sha, store.ArtifactPath(sha), nil
		}
	}
	if it.Local != "" {
		if want, ok := mavenSHA1(it.Local); ok {
			if s1, sha, err := fileHashes(it.Local); err == nil && s1 == want {
				if err := store.Add(it.Local, sha); err == nil {
					store.RecordURL(it.URL, sha)
					return sha, store.ArtifactPath(sha), nil
				}
			}
		}
	}
	if c.Offline {
		return "", "", fmt.Errorf("%w: %s", ErrOffline, it.URL)
	}
	resp, err := c.doGet(ctx, it.URL, it.Repo)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("fetch: %s: status %d", it.URL, resp.StatusCode)
	}
	if err := os.MkdirAll(store.Artifacts(), 0o755); err != nil {
		return "", "", err
	}
	tmp, err := os.CreateTemp(store.Artifacts(), ".new-*.tmp")
	if err != nil {
		return "", "", err
	}
	name := tmp.Name()
	defer os.Remove(name)
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return "", "", err
	}
	if err := tmp.Close(); err != nil {
		return "", "", err
	}
	sha := hex.EncodeToString(h.Sum(nil))
	if err := store.Add(name, sha); err != nil {
		return "", "", err
	}
	store.RecordURL(it.URL, sha)
	return sha, store.ArtifactPath(sha), nil
}

func (c *Client) download(ctx context.Context, store *cache.Store, it Item) (string, error) {
	resp, err := c.doGet(ctx, it.URL, it.Repo)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch: %s: status %d", it.URL, resp.StatusCode)
	}
	if err := os.MkdirAll(store.Artifacts(), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(store.Artifacts(), "."+it.SHA+".tmp")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	defer os.Remove(name)
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != it.SHA {
		return "", fmt.Errorf("fetch: %s: %w (got %s, want %s)", it.URL, cache.ErrMismatch, got, it.SHA)
	}
	if err := store.Add(name, it.SHA); err != nil {
		return "", err
	}
	return store.ArtifactPath(it.SHA), nil
}

// fromLocal imports it.Local into the cache when its content hashes to want.
func fromLocal(store *cache.Store, it Item, want string) (string, bool) {
	if it.Local == "" {
		return "", false
	}
	got, err := hashFile(it.Local)
	if err != nil || got != want {
		return "", false
	}
	if err := store.Add(it.Local, want); err != nil {
		return "", false
	}
	return store.ArtifactPath(want), true
}

// mavenSHA1 reads the Maven <file>.sha1 record written beside downloaded
// artifacts. It is ("", false) when absent or malformed.
func mavenSHA1(path string) (string, bool) {
	b, err := os.ReadFile(path + ".sha1")
	if err != nil {
		return "", false
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 || !sha1Re.MatchString(fields[0]) {
		return "", false
	}
	return strings.ToLower(fields[0]), true
}

func hashFile(path string) (string, error) {
	_, sha, err := fileHashes(path)
	return sha, err
}

// fileHashes returns (sha1hex, sha256hex, error) of a file, read once.
func fileHashes(path string) (string, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	h1 := sha1.New()
	h2 := sha256.New()
	if _, err := io.Copy(io.MultiWriter(h1, h2), f); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(h1.Sum(nil)), hex.EncodeToString(h2.Sum(nil)), nil
}

func retryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

func retryAfter(v string) (time.Duration, bool) {
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	return 0, false
}

// fileResponse serves a file:// URL: a 200 with the file as body, or a 404
// when the file is absent (mirrors a missing repository artifact).
func fileResponse(path string) *http.Response {
	f, err := os.Open(path)
	if err != nil {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Body:       io.NopCloser(bytes.NewReader(nil)),
		}
	}
	return &http.Response{StatusCode: http.StatusOK, Body: f}
}

// doGet performs a GET, retrying transient network errors and retryable
// statuses (429/5xx) until fetchBudget elapses. Sleeps honor Retry-After
// (capped at maxWait) and otherwise back off exponentially. It returns the
// final response (even if retryable) so the caller can report it. file://
// URLs are served from the local filesystem.
func (c *Client) doGet(ctx context.Context, url, repo string) (*http.Response, error) {
	if u, err := neturl.Parse(url); err == nil && u.Scheme == "file" {
		return fileResponse(u.Path), nil
	}
	deadline := time.Now().Add(fetchBudget)
	delay := time.Second
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		// Identify as rig: Maven Central answers the Go default
		// User-Agent with 429.
		req.Header.Set("User-Agent", "rig")
		c.authorize(req, repo)
		resp, err := c.HTTP.Do(req)
		if err == nil && !retryable(resp.StatusCode) {
			return resp, nil
		}
		if resp != nil {
			if ra, ok := retryAfter(resp.Header.Get("Retry-After")); ok {
				delay = ra
			}
		}
		if delay > maxWait {
			delay = maxWait
		}
		if !time.Now().Add(delay).Before(deadline) {
			return resp, err
		}
		if resp != nil {
			resp.Body.Close()
		}
		if !sleepCtx(ctx, delay) {
			return nil, ctx.Err()
		}
		delay *= 2
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func (c *Client) LastModified(ctx context.Context, url string) (time.Time, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return time.Time{}, false
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return time.Time{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return time.Time{}, false
	}
	t, err := http.ParseTime(resp.Header.Get("Last-Modified"))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// runPool runs work(i) for i in [0,total) across workers, stopping at the first
// error. It returns that error (or nil).
func runPool(workers, total int, work func(i int) error) error {
	if workers < 1 {
		workers = 1
	}
	if total > 0 && workers > total {
		workers = total
	}
	jobs := make(chan int)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		stop     atomic.Bool
		firstErr error
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
		stop.Store(true)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if stop.Load() {
					continue
				}
				if err := work(i); err != nil {
					fail(err)
				}
			}
		}()
	}
	for i := 0; i < total; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return firstErr
}

func FetchAll(ctx context.Context, c *Client, store *cache.Store, items []Item, workers int) (cached int, fetched int, err error) {
	counts := [2]int{}
	var mu sync.Mutex
	err = runPool(workers, len(items), func(i int) error {
		_, fromCache, err := c.Get(ctx, store, items[i])
		if err != nil {
			return err
		}
		mu.Lock()
		if fromCache {
			counts[0]++
		} else {
			counts[1]++
		}
		mu.Unlock()
		return nil
	})
	return counts[0], counts[1], err
}

// VerifyAll is FetchAll for the security gate (uses VerifyGet).
func VerifyAll(ctx context.Context, c *Client, store *cache.Store, items []Item, workers int) (cached int, fetched int, err error) {
	counts := [2]int{}
	var mu sync.Mutex
	err = runPool(workers, len(items), func(i int) error {
		_, fromCache, err := c.VerifyGet(ctx, store, items[i])
		if err != nil {
			return err
		}
		mu.Lock()
		if fromCache {
			counts[0]++
		} else {
			counts[1]++
		}
		mu.Unlock()
		return nil
	})
	return counts[0], counts[1], err
}

// FetchNewAll obtains each item in parallel (url-index, m2, or download),
// computing + caching its sha256. Returns url -> sha256.
func FetchNewAll(ctx context.Context, c *Client, store *cache.Store, items []Item, workers int) (map[string]string, error) {
	shas := make(map[string]string, len(items))
	var mu sync.Mutex
	err := runPool(workers, len(items), func(i int) error {
		sha, _, err := c.GetNew(ctx, store, items[i])
		if err != nil {
			return err
		}
		mu.Lock()
		shas[items[i].URL] = sha
		mu.Unlock()
		return nil
	})
	return shas, err
}
