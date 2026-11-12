package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ghaFakeIssuer stands in for the GitHub Actions OIDC issuer: a discovery
// document with no token endpoint, a JWKS, and the id-token request
// endpoint the runner's credentials are exchanged against.
type ghaFakeIssuer struct {
	ts       *httptest.Server
	key      *rsa.PrivateKey
	keyID    string
	reqTok   string                // the accepted request token
	ttl      time.Duration         // id-token lifetime at mint time
	mints    int32                 // id tokens issued
	mintMut  func(m jwt.MapClaims) // claim mutation at mint time
	audQuery string                // last audience seen on the request URL
	audBody  string                // last audience seen in the JSON body
}

func newGhaFakeIssuer(t *testing.T, reqTok string, ttl time.Duration) *ghaFakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fi := &ghaFakeIssuer{key: key, keyID: "k1", reqTok: reqTok, ttl: ttl}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		// The GitHub Actions document carries issuer and jwks_uri
		// only — no token endpoint.
		json.NewEncoder(w).Encode(map[string]string{
			"issuer":   fi.ts.URL,
			"jwks_uri": fi.ts.URL + "/.well-known/jwks",
		})
	})
	mux.HandleFunc("/.well-known/jwks", func(w http.ResponseWriter, r *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())
		json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{"kty": "RSA", "kid": fi.keyID, "n": n, "e": e, "alg": "RS256", "use": "sig"}},
		})
	})
	mux.HandleFunc("/jwt", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fi.reqTok {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"message": "invalid request token"})
			return
		}
		aud := r.URL.Query().Get("audience")
		var body struct {
			Audience string `json:"audience"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fi.audQuery, fi.audBody = aud, body.Audience
		if aud == "" {
			aud = body.Audience
		}
		if aud == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"message": "audience is required"})
			return
		}
		n := atomic.AddInt32(&fi.mints, 1)
		m := jwt.MapClaims{
			"iss": fi.ts.URL,
			"aud": aud,
			"sub": "repo:acme/widget:ref:refs/heads/main",
			"jti": strconv.Itoa(int(n)),
			"exp": time.Now().Add(fi.ttl).Unix(),
			"iat": time.Now().Unix(),
			"nbf": time.Now().Add(-time.Minute).Unix(),
		}
		if fi.mintMut != nil {
			fi.mintMut(m)
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, m)
		tok.Header["kid"] = fi.keyID
		s, err := tok.SignedString(fi.key)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"value": s})
	})
	fi.ts = httptest.NewServer(mux)
	t.Cleanup(fi.ts.Close)
	return fi
}

// ghaGate points the gate at the fake issuer as the GitHub Actions one:
// the GHA well-known marker is swapped to the fake's discovery URL for the
// duration of the test.
func ghaGate(t *testing.T, fi *ghaFakeIssuer) *Gate {
	t.Helper()
	old := GhaWellKnown
	GhaWellKnown = fi.ts.URL + "/.well-known/openid-configuration"
	t.Cleanup(func() { GhaWellKnown = old })
	return testGate(t, "pier", fi.ts.URL)
}

// ghaEnv sets the runner's id-token request credentials of fi, clearing
// the gate's env token so the negotiation chain reaches the exchange.
func ghaEnv(t *testing.T, g *Gate, fi *ghaFakeIssuer) {
	t.Helper()
	t.Setenv(g.EnvVar(), "")
	t.Setenv(ghaRequestURLEnv, fi.ts.URL+"/jwt")
	t.Setenv(ghaRequestTokenEnv, fi.reqTok)
}

func TestGHAExchangesAndCaches(t *testing.T) {
	fi := newGhaFakeIssuer(t, "req-tok", 5*time.Minute)
	g := ghaGate(t, fi)
	ghaEnv(t, g, fi)
	dir := t.TempDir()
	r := NewResolver(&Config{Gates: map[string]*Gate{"pier": g}}, dir)

	tok, err := r.Token(context.Background(), g, "")
	if err != nil {
		t.Fatal(err)
	}
	if tok == "" {
		t.Fatal("no token negotiated")
	}
	if got := atomic.LoadInt32(&fi.mints); got != 1 {
		t.Fatalf("id tokens minted = %d, want 1", got)
	}
	if fi.audQuery != g.Audience || fi.audBody != g.Audience {
		t.Fatalf("audience on the request = query %q body %q, want %q in both", fi.audQuery, fi.audBody, g.Audience)
	}
	if _, err := os.Stat(r.CacheFile(g)); err != nil {
		t.Fatalf("cache file missing: %v", err)
	}

	// The held token is reused: no second exchange.
	tok2, err := r.Token(context.Background(), g, "")
	if err != nil {
		t.Fatal(err)
	}
	if tok2 != tok {
		t.Fatal("the held token was re-negotiated")
	}
	if got := atomic.LoadInt32(&fi.mints); got != 1 {
		t.Fatalf("id tokens minted = %d, want 1 (held token reused)", got)
	}

	// A fresh resolver over the same state dir reuses the cache.
	r2 := NewResolver(&Config{Gates: map[string]*Gate{"pier": g}}, dir)
	tok3, err := r2.Token(context.Background(), g, "")
	if err != nil {
		t.Fatal(err)
	}
	if tok3 != tok {
		t.Fatal("the cached token was not reused")
	}
	if got := atomic.LoadInt32(&fi.mints); got != 1 {
		t.Fatalf("id tokens minted = %d, want 1 (cached token reused)", got)
	}
}

func TestGHAMissingEnv(t *testing.T) {
	fi := newGhaFakeIssuer(t, "req-tok", 5*time.Minute)
	g := ghaGate(t, fi)
	t.Setenv(g.EnvVar(), "")
	t.Setenv(ghaRequestURLEnv, "")
	t.Setenv(ghaRequestTokenEnv, "")
	r := NewResolver(&Config{Gates: map[string]*Gate{"pier": g}}, t.TempDir())

	_, err := r.Token(context.Background(), g, "")
	if err == nil || !strings.Contains(err.Error(), "id-token: write") {
		t.Fatalf("error = %v, want the id-token: write hint", err)
	}
	if got := atomic.LoadInt32(&fi.mints); got != 0 {
		t.Fatalf("id tokens minted = %d, want 0", got)
	}
	// The failure is stable for the command.
	_, err2 := r.Token(context.Background(), g, "")
	if err2.Error() != err.Error() {
		t.Fatalf("second error = %v, want the same %v", err2, err)
	}
}

func TestGHABadRequestToken(t *testing.T) {
	fi := newGhaFakeIssuer(t, "req-tok", 5*time.Minute)
	g := ghaGate(t, fi)
	ghaEnv(t, g, fi)
	t.Setenv(ghaRequestTokenEnv, "wrong")
	r := NewResolver(&Config{Gates: map[string]*Gate{"pier": g}}, t.TempDir())

	_, err := r.Token(context.Background(), g, "")
	if err == nil || !strings.Contains(err.Error(), "invalid request token") {
		t.Fatalf("error = %v, want the issuer's 401 message", err)
	}
	if got := atomic.LoadInt32(&fi.mints); got != 0 {
		t.Fatalf("id tokens minted = %d, want 0", got)
	}
}

func TestGHAWrongAudienceRejected(t *testing.T) {
	fi := newGhaFakeIssuer(t, "req-tok", 5*time.Minute)
	fi.mintMut = func(m jwt.MapClaims) { m["aud"] = "other" }
	g := ghaGate(t, fi)
	ghaEnv(t, g, fi)
	r := NewResolver(&Config{Gates: map[string]*Gate{"pier": g}}, t.TempDir())

	_, err := r.Token(context.Background(), g, "")
	if err == nil || !strings.Contains(err.Error(), "issued token rejected") {
		t.Fatalf("error = %v, want the audience rejection", err)
	}
}

func TestGHARenegotiatesAfterInvalidate(t *testing.T) {
	fi := newGhaFakeIssuer(t, "req-tok", 5*time.Minute)
	g := ghaGate(t, fi)
	ghaEnv(t, g, fi)
	old := jitThrottle
	jitThrottle = 0
	defer func() { jitThrottle = old }()
	r := NewResolver(&Config{Gates: map[string]*Gate{"pier": g}}, t.TempDir())

	tok1, err := r.Token(context.Background(), g, "")
	if err != nil {
		t.Fatal(err)
	}
	// The registry rejects the held token: the next call re-negotiates.
	r.Invalidate(g)
	tok2, err := r.Token(context.Background(), g, "")
	if err != nil {
		t.Fatal(err)
	}
	if tok2 == tok1 {
		t.Fatal("the rejected token was not re-negotiated")
	}
	if got := atomic.LoadInt32(&fi.mints); got != 2 {
		t.Fatalf("id tokens minted = %d, want 2", got)
	}
}

func TestGHAThrottlesRejectedToken(t *testing.T) {
	fi := newGhaFakeIssuer(t, "req-tok", 5*time.Minute)
	g := ghaGate(t, fi)
	ghaEnv(t, g, fi)
	// The default throttle (5s) blocks an immediate re-negotiation of a
	// rejected token.
	r := NewResolver(&Config{Gates: map[string]*Gate{"pier": g}}, t.TempDir())

	tok1, err := r.Token(context.Background(), g, "")
	if err != nil {
		t.Fatal(err)
	}
	r.Invalidate(g)
	tok2, err := r.Token(context.Background(), g, "")
	if err != nil {
		t.Fatal(err)
	}
	if tok2 != tok1 {
		t.Fatal("the rejected token was re-negotiated inside the throttle window")
	}
	if got := atomic.LoadInt32(&fi.mints); got != 1 {
		t.Fatalf("id tokens minted = %d, want 1 (throttled)", got)
	}
}

func TestGHARenegotiatesOnExpiry(t *testing.T) {
	fi := newGhaFakeIssuer(t, "req-tok", time.Millisecond) // expires inside the margin
	g := ghaGate(t, fi)
	ghaEnv(t, g, fi)
	r := NewResolver(&Config{Gates: map[string]*Gate{"pier": g}}, t.TempDir())

	if _, err := r.Token(context.Background(), g, ""); err != nil {
		t.Fatal(err)
	}
	tok2, err := r.Token(context.Background(), g, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&fi.mints); got != 2 {
		t.Fatalf("id tokens minted = %d, want 2 (expired token re-negotiated)", got)
	}
	if tok2 == "" {
		t.Fatal("no token after the re-negotiation")
	}
}

func TestGHAEscapesAudience(t *testing.T) {
	// The audience is URL-quoted on the request, verbatim in the body.
	fi := newGhaFakeIssuer(t, "req-tok", 5*time.Minute)
	g := ghaGate(t, fi)
	g.Audience = "https://pier.example/"
	ghaEnv(t, g, fi)
	r := NewResolver(&Config{Gates: map[string]*Gate{"pier": g}}, t.TempDir())

	if _, err := r.Token(context.Background(), g, ""); err != nil {
		t.Fatal(err)
	}
	if fi.audBody != "https://pier.example/" {
		t.Fatalf("body audience = %q, want the verbatim URL", fi.audBody)
	}
	if fi.audQuery != "https://pier.example/" {
		t.Fatalf("query audience = %q, want the decoded URL", fi.audQuery)
	}
}

func TestGHASkippedForFlow(t *testing.T) {
	fi := newGhaFakeIssuer(t, "req-tok", 5*time.Minute)
	g := ghaGate(t, fi)
	ghaEnv(t, g, fi)
	r := NewResolver(&Config{Gates: map[string]*Gate{"pier": g}}, t.TempDir())

	_, err := r.Token(context.Background(), g, "browser")
	if err == nil || !strings.Contains(err.Error(), "does not apply") {
		t.Fatalf("error = %v, want the flow refusal", err)
	}
	if got := atomic.LoadInt32(&fi.mints); got != 0 {
		t.Fatalf("id tokens minted = %d, want 0", got)
	}
}

func TestGHAStaticEnvTokenNotRefreshed(t *testing.T) {
	fi := newGhaFakeIssuer(t, "req-tok", time.Millisecond) // expired on purpose
	g := ghaGate(t, fi)
	ghaEnv(t, g, fi)
	t.Setenv(g.EnvVar(), "static")
	r := NewResolver(&Config{Gates: map[string]*Gate{"pier": g}}, t.TempDir())

	tok, err := r.Token(context.Background(), g, "")
	if err != nil {
		t.Fatal(err)
	}
	r.Invalidate(g)
	tok2, err := r.Token(context.Background(), g, "")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "static" || tok2 != "static" {
		t.Fatalf("tokens = %q, %q; the env token is used as-is", tok, tok2)
	}
	if got := atomic.LoadInt32(&fi.mints); got != 0 {
		t.Fatalf("id tokens minted = %d, want 0 (the env token wins)", got)
	}
}

// TestGhaWellKnownIsThePublicDiscoveryURL pins the real discovery URL.
// Every other test points GhaWellKnown at a fake issuer, so a wrong real
// host would only surface on a live runner.
func TestGhaWellKnownIsThePublicDiscoveryURL(t *testing.T) {
	const want = "https://token.actions.githubusercontent.com/.well-known/openid-configuration"
	if GhaWellKnown != want {
		t.Fatalf("GhaWellKnown = %q, want %q", GhaWellKnown, want)
	}
}
