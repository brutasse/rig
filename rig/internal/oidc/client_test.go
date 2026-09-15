package oidc

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testISS      = "https://fake-idp.example"
	testAudience = "pier"
)

// fakeIDP is an OpenID provider for tests: discovery, JWKS, an
// authorization endpoint that immediately redirects with a code
// (browser flow), a device authorization endpoint, and a token
// endpoint that validates both grants.
type fakeIDP struct {
	ts    *httptest.Server
	key   *rsa.PrivateKey
	aud   string
	noDev bool // omit device_authorization_endpoint from discovery

	// state captured from the authorize request, checked at /token
	challenge   string
	redirectURI string

	authorizeCalls atomic.Int64
	tokenCalls     atomic.Int64
	// pending device polls that must answer authorization_pending
	pending int
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIDP{key: key, aud: testAudience}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]string{
			"issuer":                 testISS,
			"authorization_endpoint": idp.ts.URL + "/authorize",
			"token_endpoint":         idp.ts.URL + "/token",
			"jwks_uri":               idp.ts.URL + "/jwks",
		}
		if !idp.noDev {
			doc["device_authorization_endpoint"] = idp.ts.URL + "/device"
		}
		json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{
			{"kty": "RSA", "kid": "k1", "n": n, "e": e, "alg": "RS256"},
		}})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		idp.authorizeCalls.Add(1)
		q := r.URL.Query()
		if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" ||
			q.Get("code_challenge") == "" || q.Get("state") == "" || q.Get("client_id") == "" {
			http.Error(w, "bad authorize request", http.StatusBadRequest)
			return
		}
		idp.challenge = q.Get("code_challenge")
		idp.redirectURI = q.Get("redirect_uri")
		// "The user" approves immediately.
		u, _ := url.Parse(q.Get("redirect_uri"))
		rq := u.Query()
		rq.Set("code", "auth-code-1")
		rq.Set("state", q.Get("state"))
		u.RawQuery = rq.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	})
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"device_code":               "dev-code-1",
			"user_code":                 "ABCD-EFGH",
			"verification_uri":          idp.ts.URL + "/verify",
			"verification_uri_complete": idp.ts.URL + "/verify?user_code=ABCD-EFGH",
			"interval":                  1,
			"expires_in":                300,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		idp.tokenCalls.Add(1)
		r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			verifier := r.Form.Get("code_verifier")
			sum := sha256.Sum256([]byte(verifier))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != idp.challenge ||
				r.Form.Get("code") != "auth-code-1" ||
				r.Form.Get("redirect_uri") != idp.redirectURI {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
			idp.tokenResponse(w)
		case "urn:ietf:params:oauth:grant-type:device_code":
			if r.Form.Get("device_code") != "dev-code-1" {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
			if idp.pending > 0 {
				idp.pending--
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
				return
			}
			idp.tokenResponse(w)
		default:
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "unsupported_grant_type"})
		}
	})
	idp.ts = httptest.NewServer(mux)
	t.Cleanup(idp.ts.Close)
	return idp
}

func (idp *fakeIDP) tokenResponse(w http.ResponseWriter) {
	json.NewEncoder(w).Encode(map[string]any{
		"access_token": idp.token(nil),
		"token_type":   "Bearer",
		"expires_in":   3600,
	})
}

// token signs an ID token with the IdP key. aud defaults to idp.aud.
func (idp *fakeIDP) token(extra map[string]any) string {
	m := jwt.MapClaims{
		"iss": testISS,
		"aud": idp.aud,
		"sub": "sub-42",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	for k, v := range extra {
		m[k] = v
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, m)
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(idp.key)
	if err != nil {
		panic(err)
	}
	return s
}

// testConfig points at the fake IdP with a throwaway cache path.
func testConfig(t *testing.T, idp *fakeIDP) *ClientConfig {
	t.Helper()
	return &ClientConfig{
		WellKnown: idp.ts.URL + "/.well-known/openid-configuration",
		Audience:  testAudience,
		ClientID:  "rig",
		CachePath: filepath.Join(t.TempDir(), "token.json"),
	}
}

func newTestClient(t *testing.T, idp *fakeIDP) *Client {
	t.Helper()
	c, err := New(context.Background(), testConfig(t, idp))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// syncBuf is a concurrency-safe io.Writer for capturing flow output.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func lineWith(s, needle string) (string, bool) {
	for _, ln := range strings.Split(s, "\n") {
		if strings.Contains(ln, needle) {
			return strings.TrimSpace(ln), true
		}
	}
	return "", false
}

// fakeBrowser waits for the browser flow to print the authorization URL,
// then fetches it, following the IdP's redirect to the local callback.
func fakeBrowser(t *testing.T, out *syncBuf) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var authURL string
	for time.Now().Before(deadline) {
		if u, ok := lineWith(out.String(), "code_challenge="); ok {
			authURL = u
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if authURL == "" {
		t.Fatal("authorization URL never printed")
	}
	resp, err := http.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestBrowserFlow(t *testing.T) {
	openBrowser = func(string) {}
	idp := newFakeIDP(t)
	c := newTestClient(t, idp)

	var out syncBuf
	type res struct {
		tok string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		tok, err := c.Token(context.Background(), "browser", &out)
		ch <- res{tok, err}
	}()
	fakeBrowser(t, &out)
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		tok := r.tok
		// The issued token must verify against the IdP's JWKS.
		if err := verifyToken(t, idp, tok); err != nil {
			t.Fatal(err)
		}
		// It must be cached, with 0600 permissions.
		b, err := os.ReadFile(c.cfg.CachePath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), tok) {
			t.Fatal("token not cached")
		}
		fi, err := os.Stat(c.cfg.CachePath)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("cache permissions %v, want 0600", fi.Mode().Perm())
		}
		// A second call must come from the cache, not the IdP.
		before := idp.tokenCalls.Load()
		tok2, err := c.Token(context.Background(), "browser", io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if tok2 != tok {
			t.Fatal("cache returned a different token")
		}
		if got := idp.tokenCalls.Load(); got != before {
			t.Fatalf("token endpoint called again: %d calls", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("browser flow did not complete")
	}
}

// TestBrowserFlowConfiguredRedirect runs the browser flow with a fixed
// loopback redirect URI, as required by IdPs with pre-registered
// redirect URIs.
func TestBrowserFlowConfiguredRedirect(t *testing.T) {
	openBrowser = func(string) {}
	idp := newFakeIDP(t)
	c := newTestClient(t, idp)

	// Reserve a loopback port, as an IdP registration would.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	c.cfg.RedirectURI = fmt.Sprintf("http://127.0.0.1:%d/rig/callback", port)

	var out syncBuf
	type res struct {
		tok string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		tok, err := c.Token(context.Background(), "browser", &out)
		ch <- res{tok, err}
	}()
	fakeBrowser(t, &out)
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		// The IdP saw the configured URI on the authorize request and
		// only accepts the exchange with the same one.
		if idp.redirectURI != c.cfg.RedirectURI {
			t.Fatalf("redirect_uri %q, want %q", idp.redirectURI, c.cfg.RedirectURI)
		}
		if err := verifyToken(t, idp, r.tok); err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("browser flow did not complete")
	}
}

func TestDeviceFlow(t *testing.T) {
	openBrowser = func(string) {}
	idp := newFakeIDP(t)
	idp.pending = 2 // exercise the authorization_pending polling
	c := newTestClient(t, idp)

	tok, err := c.Token(context.Background(), "device", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyToken(t, idp, tok); err != nil {
		t.Fatal(err)
	}
}

// verifyToken checks tok against the IdP's JWKS with the expected
// audience, using rig's own verifier.
func verifyToken(t *testing.T, idp *fakeIDP, tok string) error {
	t.Helper()
	claims, err := NewVerifier(idp.ts.URL+"/.well-known/openid-configuration", testISS, testAudience).Verify(context.Background(), tok)
	if err != nil {
		return err
	}
	if claims["sub"] != "sub-42" {
		t.Fatalf("sub claim %v, want sub-42", claims["sub"])
	}
	return nil
}

func TestAutoFlowHeadless(t *testing.T) {
	openBrowser = func(string) {}
	canOpenBrowser = func() bool { return false }
	idp := newFakeIDP(t)
	c := newTestClient(t, idp)

	tok, err := c.Token(context.Background(), "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if idp.authorizeCalls.Load() != 0 {
		t.Fatalf("browser flow used in headless env: %d authorize calls", idp.authorizeCalls.Load())
	}
	if idp.tokenCalls.Load() < 1 {
		t.Fatal("token endpoint never called")
	}
	if tok == "" {
		t.Fatal("empty token")
	}
}

func TestAutoFlowBrowser(t *testing.T) {
	openBrowser = func(string) {}
	canOpenBrowser = func() bool { return true }
	idp := newFakeIDP(t)
	c := newTestClient(t, idp)

	var out syncBuf
	type res struct {
		tok string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		tok, err := c.Token(context.Background(), "", &out)
		ch <- res{tok, err}
	}()
	fakeBrowser(t, &out)
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if idp.authorizeCalls.Load() != 1 {
			t.Fatalf("authorize calls: %d, want 1", idp.authorizeCalls.Load())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("auto flow did not complete")
	}
}

func TestAutoFlowUnavailable(t *testing.T) {
	openBrowser = func(string) {}
	canOpenBrowser = func() bool { return false }
	idp := newFakeIDP(t)
	idp.noDev = true // issuer supports neither flow
	c := newTestClient(t, idp)

	if _, err := c.Token(context.Background(), "", io.Discard); err == nil {
		t.Fatal("expected error when no flow is available")
	}
}

func TestUnknownFlow(t *testing.T) {
	idp := newFakeIDP(t)
	c := newTestClient(t, idp)
	if _, err := c.Token(context.Background(), "carrier-pigeon", io.Discard); err == nil {
		t.Fatal("expected error for unknown flow")
	}
}

func TestDeviceFlowUnavailable(t *testing.T) {
	idp := newFakeIDP(t)
	idp.noDev = true
	c := newTestClient(t, idp)
	if _, err := c.Token(context.Background(), "device", io.Discard); err == nil {
		t.Fatal("expected error when device flow is unavailable")
	}
}

func TestIssuedTokenAudienceRejected(t *testing.T) {
	openBrowser = func(string) {}
	idp := newFakeIDP(t)
	idp.aud = "someone-else" // IdP issues a token rig would not accept
	c := newTestClient(t, idp)

	if _, err := c.Token(context.Background(), "device", io.Discard); err == nil {
		t.Fatal("expected audience mismatch to be rejected")
	}
	// Nothing must be cached.
	if _, err := os.Stat(c.cfg.CachePath); !os.IsNotExist(err) {
		t.Fatalf("rejected token was cached: %v", err)
	}
}

func TestCacheExpiry(t *testing.T) {
	openBrowser = func(string) {}
	idp := newFakeIDP(t)
	c := newTestClient(t, idp)

	// An expired cache entry must trigger renegotiation.
	if err := writeCache(c.cfg.CachePath, &cachedToken{
		Value:     idp.token(nil),
		ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	var out syncBuf
	type res struct {
		tok string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		tok, err := c.Token(context.Background(), "browser", &out)
		ch <- res{tok, err}
	}()
	fakeBrowser(t, &out)
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		b, err := os.ReadFile(c.cfg.CachePath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), r.tok) {
			t.Fatal("cache not updated after renegotiation")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cache expiry flow did not complete")
	}
}
