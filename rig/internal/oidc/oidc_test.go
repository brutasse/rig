package oidc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMissingIsEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Gates) != 0 {
		t.Fatalf("gates = %v, want empty", cfg.Gates)
	}
	if !strings.HasSuffix(cfg.Path, "auth.yaml (not found)") {
		t.Fatalf("path %q should name the missing file", cfg.Path)
	}
}

// writeGateFile writes the gate config directly (no HOME dance) and loads
// it through the default location.
func writeGateFile(t *testing.T, home, content string) {
	t.Helper()
	path := filepath.Join(home, ".config", "rig", "auth.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeGateFile(t, home, "gates:\n  pier:\n    url: https://pier.example\n    well-known: https://idp.example/w\n")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.Gates["pier"]
	if g == nil {
		t.Fatal("gate pier missing")
	}
	if g.Audience != "pier" {
		t.Errorf("audience default = %q, want pier", g.Audience)
	}
	if g.ClientID != "rig" {
		t.Errorf("client-id default = %q, want rig", g.ClientID)
	}
	if len(g.URLs) != 1 || g.URLs[0] != "https://pier.example" {
		t.Errorf("urls = %v", g.URLs)
	}
	if g.EnvVar() != "RIG_TOKEN_PIER" {
		t.Errorf("env var = %q", g.EnvVar())
	}
}

func TestLoadExplicitValues(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeGateFile(t, home, "gates:\n  corp-maven:\n    url: [https://maven.corp.example, https://mvn2.corp.example]\n"+
		"    well-known: https://idp.corp.example/w\n    audience: maven.corp.example\n    client-id: rig-ci\n"+
		"    redirect-uri: http://127.0.0.1:8080\n")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.Gates["corp-maven"]
	if g.Audience != "maven.corp.example" || g.ClientID != "rig-ci" {
		t.Errorf("gate = %+v", g)
	}
	if len(g.URLs) != 2 || g.URLs[1] != "https://mvn2.corp.example" {
		t.Errorf("urls = %v", g.URLs)
	}
	// A loopback redirect with a missing path defaults to /callback.
	if g.RedirectURI != "http://127.0.0.1:8080/callback" {
		t.Errorf("redirect = %q", g.RedirectURI)
	}
	if g.EnvVar() != "RIG_TOKEN_CORP_MAVEN" {
		t.Errorf("env var = %q, want RIG_TOKEN_CORP_MAVEN", g.EnvVar())
	}
}

func TestLoadRejects(t *testing.T) {
	bad := []struct {
		name    string
		content string
		wantErr string
	}{
		{"missing well-known", "gates:\n  pier: {}\n", "well-known is required"},
		{"bad well-known", "gates:\n  pier:\n    well-known: not-a-url\n", "well-known must be an http(s) URL"},
		{"bad name", "gates:\n  \"Bad Name\":\n    well-known: https://idp.example/w\n", `name "Bad Name" must match`},
		{"uppercase name", "gates:\n  PIER:\n    well-known: https://idp.example/w\n", "name \"PIER\" must match"},
		{"bad url", "gates:\n  pier:\n    url: not-a-url\n    well-known: https://idp.example/w\n", "url must be an http(s) URL prefix"},
		{"bad redirect", "gates:\n  pier:\n    well-known: https://idp.example/w\n    redirect-uri: https://idp.example/cb\n", "redirect-uri must be an http loopback URI"},
		{"portless redirect", "gates:\n  pier:\n    well-known: https://idp.example/w\n    redirect-uri: http://127.0.0.1/cb\n", "redirect-uri must be an http loopback URI"},
		{"non-loopback redirect", "gates:\n  pier:\n    well-known: https://idp.example/w\n    redirect-uri: http://example.com/cb\n", "redirect-uri must be an http loopback URI"},
		{"two url-less gates", "gates:\n  a:\n    well-known: https://idp.example/w\n  b:\n    well-known: https://idp.example/w\n", "at most one gate may omit url"},
		{"unparseable", "gates: [unclosed", "parse "},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			writeGateFile(t, home, c.content)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want %q", err, c.wantErr)
			}
		})
	}
}

func TestGateMatching(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeGateFile(t, home, "gates:\n"+
		"  pier:\n    url: https://pier.example\n    well-known: https://idp.example/w\n"+
		"  corp:\n    url: [https://maven.corp.example, https://mvn2.corp.example]\n    well-known: https://idp2.example/w\n"+
		"  default-gate:\n    well-known: https://idp3.example/w\n")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}

	g, err := cfg.Gate("https://pier.example/org/x")
	if err != nil || g.Name != "pier" {
		t.Fatalf("pier repo: gate %v err %v", g, err)
	}
	g, err = cfg.Gate("https://maven.corp.example/releases/x")
	if err != nil || g.Name != "corp" {
		t.Fatalf("corp repo: gate %v err %v", g, err)
	}
	// A repo no url gate fronts falls to the single url-less gate.
	g, err = cfg.Gate("https://internal.example/x")
	if err != nil || g.Name != "default-gate" {
		t.Fatalf("internal repo: gate %v err %v", g, err)
	}
}

func TestGateMatchingNoConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	_, err = cfg.Gate("https://pier.example/x")
	if err == nil || !strings.Contains(err.Error(), "no gate in") || !strings.Contains(err.Error(), "auth.yaml (not found)") {
		t.Fatalf("err = %v, want the no-gate error naming the config", err)
	}
}

func TestGateMatchingMultipleUrlLess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeGateFile(t, home, "gates:\n  a:\n    well-known: https://idp.example/w\n  b:\n    well-known: https://idp.example/w\n")
	_, err := Load()
	if err == nil {
		t.Fatal("expected load error for two url-less gates")
	}
}

// testGate is a gate pointing at a fake IdP: device flow only, so no
// browser is needed to negotiate in tests.
func testGate(t *testing.T, name, idpURL string) *Gate {
	t.Helper()
	return &Gate{
		Name:        name,
		WellKnown:   idpURL + "/.well-known/openid-configuration",
		Audience:    testAudience,
		ClientID:    "rig",
		RedirectURI: "",
	}
}

func TestResolverEnvVarWins(t *testing.T) {
	idp := newFakeIDP(t)
	g := testGate(t, "pier", idp.ts.URL)
	r := NewResolver(&Config{Gates: map[string]*Gate{"pier": g}}, t.TempDir())
	t.Setenv(g.EnvVar(), "  env-token  ")

	tok, err := r.Token(context.Background(), g, "")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "env-token" {
		t.Fatalf("token = %q, want the trimmed env var value", tok)
	}
	if idp.tokenCalls.Load() != 0 {
		t.Fatalf("token endpoint called: %d", idp.tokenCalls.Load())
	}
}

func TestResolverCacheWins(t *testing.T) {
	idp := newFakeIDP(t)
	g := testGate(t, "pier", idp.ts.URL)
	dir := t.TempDir()
	r := NewResolver(&Config{Gates: map[string]*Gate{"pier": g}}, dir)
	t.Setenv(g.EnvVar(), "")

	// Negotiate once: the token endpoint is called and the cache is
	// written.
	tok, err := r.Token(context.Background(), g, "device")
	if err != nil {
		t.Fatal(err)
	}
	if idp.tokenCalls.Load() < 1 {
		t.Fatal("token endpoint never called")
	}
	fi, err := os.Stat(r.CacheFile(g))
	if err != nil || fi.IsDir() {
		t.Fatalf("cache file missing: %v", err)
	}

	// A fresh resolver over the same state dir must reuse the cache.
	r2 := NewResolver(&Config{Gates: map[string]*Gate{"pier": g}}, dir)
	before := idp.tokenCalls.Load()
	tok2, err := r2.Token(context.Background(), g, "")
	if err != nil {
		t.Fatal(err)
	}
	if tok2 != tok {
		t.Fatalf("second resolver token = %q, want the cached one", tok2)
	}
	if got := idp.tokenCalls.Load(); got != before {
		t.Fatalf("token endpoint called again: %d calls", got)
	}
}

func TestResolverPerGateCaches(t *testing.T) {
	a := newFakeIDP(t)
	b := newFakeIDP(t)
	// Same well-known URL would collide; distinct IdPs give distinct
	// identities.
	ga := testGate(t, "a", a.ts.URL)
	gb := testGate(t, "b", b.ts.URL)
	dir := t.TempDir()
	r := NewResolver(&Config{Gates: map[string]*Gate{"a": ga, "b": gb}}, dir)

	if _, err := r.Token(context.Background(), ga, "device"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Token(context.Background(), gb, "device"); err != nil {
		t.Fatal(err)
	}
	fa, fb := r.CacheFile(ga), r.CacheFile(gb)
	if fa == fb {
		t.Fatalf("two gates share one cache file %s", fa)
	}
	if _, err := os.Stat(fa); err != nil {
		t.Fatalf("cache a missing: %v", err)
	}
	if _, err := os.Stat(fb); err != nil {
		t.Fatalf("cache b missing: %v", err)
	}
}

func TestResolverNegotiationFailure(t *testing.T) {
	// No issuer at all: negotiation fails, and the error is stable
	// across calls (resolved once).
	g := testGate(t, "pier", "http://127.0.0.1:1") // nothing listens
	r := NewResolver(&Config{Gates: map[string]*Gate{"pier": g}}, t.TempDir())

	_, err := r.Token(context.Background(), g, "device")
	if err == nil {
		t.Fatal("expected negotiation to fail")
	}
	_, err2 := r.Token(context.Background(), g, "device")
	if err2 != err {
		t.Fatalf("second error = %v, want the same %v", err2, err)
	}
}

func TestResolverBareTokenIgnored(t *testing.T) {
	// Regression guard: no bare RIG_TOKEN anywhere in the chain.
	idp := newFakeIDP(t)
	g := testGate(t, "pier", idp.ts.URL)
	r := NewResolver(&Config{Gates: map[string]*Gate{"pier": g}}, t.TempDir())
	t.Setenv("RIG_TOKEN", "stray-token")
	t.Setenv(g.EnvVar(), "")

	tok, err := r.Token(context.Background(), g, "device")
	if err != nil {
		t.Fatal(err)
	}
	if tok == "stray-token" {
		t.Fatal("bare RIG_TOKEN was used; it must be ignored")
	}
	if idp.tokenCalls.Load() < 1 {
		t.Fatal("expected a negotiation (no env var, empty cache)")
	}
}
