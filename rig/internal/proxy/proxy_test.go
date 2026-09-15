package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type req struct {
	method, path, auth, host string
}

// target is a recording "upstream" server: it enforces the expected bearer
// and echoes nothing interesting.
func target(t *testing.T, wantAuth string) (*httptest.Server, *[]req) {
	t.Helper()
	var mu sync.Mutex
	var seen []req
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		seen = append(seen, req{r.Method, r.URL.RequestURI(), auth, r.Host})
		mu.Unlock()
		if wantAuth != "" && auth != wantAuth {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		io.WriteString(w, "artifact-bytes")
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func get(t *testing.T, url string, wantStatus int) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("status = %d, want %d (%s)", resp.StatusCode, wantStatus, string(b))
	}
	return resp.StatusCode, string(b)
}

func TestProxyInjectsBearer(t *testing.T) {
	up, seen := target(t, "Bearer tok-1")
	p, err := Start(map[string]string{"pier": up.URL}, map[string]string{"pier": "tok-1"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	status, body := get(t, p.Base("pier")+"/org/x/1.0.0/x-1.0.0.jar", 200)
	if status != 200 || body != "artifact-bytes" {
		t.Fatalf("got status %d body %q", status, body)
	}
	if len(*seen) != 1 || (*seen)[0].method != "GET" ||
		(*seen)[0].path != "/org/x/1.0.0/x-1.0.0.jar" || (*seen)[0].auth != "Bearer tok-1" {
		t.Fatalf("upstream saw: %#v", *seen)
	}
}

// TestProxyPerRepoTokens: each repository id gets the bearer of its own
// gate — two repos, two tokens, no cross-talk.
func TestProxyPerRepoTokens(t *testing.T) {
	a, seenA := target(t, "Bearer tok-a")
	b, seenB := target(t, "Bearer tok-b")
	p, err := Start(
		map[string]string{"a": a.URL, "b": b.URL},
		map[string]string{"a": "tok-a", "b": "tok-b"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	get(t, p.Base("a")+"/org/x/1.0.0/x-1.0.0.jar", 200)
	get(t, p.Base("b")+"/org/y/2.0.0/y-2.0.0.jar", 200)
	get(t, p.Base("a")+"/org/x/1.0.0/x-1.0.0.jar", 200)

	if len(*seenA) != 2 || (*seenA)[0].auth != "Bearer tok-a" || (*seenA)[1].auth != "Bearer tok-a" {
		t.Fatalf("upstream a saw: %#v", *seenA)
	}
	if len(*seenB) != 1 || (*seenB)[0].auth != "Bearer tok-b" {
		t.Fatalf("upstream b saw: %#v", *seenB)
	}
}

func TestProxyUnknownIDIs404(t *testing.T) {
	up, _ := target(t, "")
	p, err := Start(map[string]string{"pier": up.URL}, map[string]string{"pier": "tok-1"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	resp, err := http.Get(p.Base("other") + "/x")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestProxyNoBearerWithoutToken(t *testing.T) {
	up, seen := target(t, "")
	p, err := Start(map[string]string{"pier": up.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	get(t, p.Base("pier")+"/a/b", 200)
	if got := (*seen)[0].auth; got != "" {
		t.Fatalf("Authorization = %q, want empty", got)
	}
}

func TestProxyForwardsStatusAndQuery(t *testing.T) {
	var mu sync.Mutex
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		query = r.URL.RawQuery
		mu.Unlock()
		http.Error(w, "nope", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	p, err := Start(map[string]string{"pier": srv.URL}, map[string]string{"pier": "tok-1"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	resp, err := http.Get(p.Base("pier") + "/x?foo=bar&baz=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 from upstream", resp.StatusCode)
	}
	if query != "foo=bar&baz=1" {
		t.Fatalf("upstream query = %q", query)
	}
}

func TestProxyStreamsBody(t *testing.T) {
	want := strings.Repeat("x", 1<<20) // 1 MiB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, want)
	}))
	t.Cleanup(srv.Close)
	p, err := Start(map[string]string{"pier": srv.URL}, map[string]string{"pier": "tok-1"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	resp, err := http.Get(p.Base("pier") + "/big")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("body length = %d, want %d", len(got), len(want))
	}
}

func TestProxyHead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method = %s, want HEAD", r.Method)
		}
		w.Header().Set("Content-Length", "1234")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	p, err := Start(map[string]string{"pier": srv.URL}, map[string]string{"pier": "tok-1"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	req, err := http.NewRequest(http.MethodHead, p.Base("pier")+"/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength != 1234 {
		t.Fatalf("status %d content-length %d", resp.StatusCode, resp.ContentLength)
	}
}
