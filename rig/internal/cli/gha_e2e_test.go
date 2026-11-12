package cli

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/brutasse/rig/internal/lockfile"
	"github.com/brutasse/rig/internal/oidc"
)

// GitHub Actions E2E, fully self-contained: an in-process fake GHA issuer
// (discovery + JWKS + id-token endpoint) and an in-process OIDC-gated Maven
// repo. The workspace's gate fronts the repo on the GHA well-known; the
// runner's id-token request credentials are set in the environment. The
// first minted token expires before the kernel reaches the repo — the
// mid-run expiry — so the run only succeeds if rig re-negotiates: the
// kernel's floating probe goes through the auth proxy (refresh on the
// registry's 401), and the lock attributes the artifact to the repo's real
// URL.

// ghaIssuer stands in for the GitHub Actions OIDC issuer: the discovery
// document (issuer + jwks_uri only, like the real one — no token endpoint),
// a JWKS, and the id-token request endpoint the runner's credentials are
// exchanged against. Every minted token carries a unique jti.
type ghaIssuer struct {
	ts     *httptest.Server
	key    *rsa.PrivateKey
	reqTok string
	mints  int32
}

func startGhaIssuer(t *testing.T) *ghaIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fi := &ghaIssuer{key: key, reqTok: "gha-req-tok"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"issuer":   fi.ts.URL,
			"jwks_uri": fi.ts.URL + "/.well-known/jwks",
		})
	})
	mux.HandleFunc("/.well-known/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "gha-e2e",
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			"alg": "RS256", "use": "sig",
		}}})
	})
	mux.HandleFunc("/jwt", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fi.reqTok {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"message": "invalid request token"})
			return
		}
		aud := r.URL.Query().Get("audience")
		if aud == "" {
			var body struct {
				Audience string `json:"audience"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
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
			"sub": "repo:example/gha-e2e:ref:refs/heads/main",
			"jti": fmt.Sprintf("jti-%d", n),
			"exp": time.Now().Add(5 * time.Minute).Unix(),
			"iat": time.Now().Unix(),
			"nbf": time.Now().Add(-time.Minute).Unix(),
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, m)
		tok.Header["kid"] = "gha-e2e"
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

// ghaRepo is a gated Maven repository serving org.example/gha-e2e: every
// request must carry a valid id token signed by fi (signature, iss, aud,
// exp). Once dead1 is set, the first minted token (jti-1) is rejected with
// 401 for life — the server-side death of a short-lived token mid-run, the
// case the re-negotiation exists for.
type ghaRepo struct {
	ts      *httptest.Server
	fi      *ghaIssuer
	aud     string // set to the server's own URL after start (the GHA convention)
	dead1   atomic.Bool
	rejHits int32
}

func startGhaRepo(t *testing.T, fi *ghaIssuer, ver string) *ghaRepo {
	t.Helper()
	repo := &ghaRepo{fi: fi}
	pom := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<project><groupId>org.example</groupId><artifactId>gha-e2e</artifactId><version>` + ver + `</version><packaging>jar</packaging></project>` + "\n"
	metadata := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<metadata><groupId>org.example</groupId><artifactId>gha-e2e</artifactId><versioning>` + "\n" +
		`  <latest>` + ver + `</latest><release>` + ver + `</release>` + "\n" +
		`  <versions><version>` + ver + `</version></versions>` + "\n" +
		`  <lastUpdated>` + time.Now().Add(-72*time.Hour).UTC().Format("20060102150405") + `</lastUpdated>` + "\n" +
		`</versioning></metadata>` + "\n"
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		claims, err := repo.verify(tok)
		if err != nil || (repo.dead1.Load() && claims["jti"] == "jti-1") {
			atomic.AddInt32(&repo.rejHits, 1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/maven-metadata.xml"):
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, metadata)
		case strings.HasSuffix(r.URL.Path, ".pom"):
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, pom)
		case strings.HasSuffix(r.URL.Path, ".jar"):
			io.WriteString(w, "gha-e2e-jar-bytes")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	repo.ts = httptest.NewServer(mux)
	t.Cleanup(repo.ts.Close)
	return repo
}

// verify checks tok against fi's key: signature, iss, aud, exp.
func (r *ghaRepo) verify(tok string) (jwt.MapClaims, error) {
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(r.fi.ts.URL),
		jwt.WithAudience(r.aud),
	)
	parsed, err := parser.ParseWithClaims(tok, jwt.MapClaims{}, func(t *jwt.Token) (any, error) {
		return r.fi.key.Public(), nil
	})
	if err != nil || !parsed.Valid {
		return nil, fmt.Errorf("token rejected: %v", err)
	}
	return parsed.Claims.(jwt.MapClaims), nil
}

// ghaGateConfig isolates HOME and writes the gate config: a single gate
// "pier" fronting the gated repo, negotiating on the GHA well-known (fi's
// discovery URL — the test points oidc.GhaWellKnown at it). The audience
// is the repo's own URL, the GHA convention.
func ghaGateConfig(t *testing.T, repoURL, wellKnown string) {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	path := filepath.Join(tmpHome, ".config", "rig", "auth.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "gates:\n" +
		"  pier:\n" +
		"    url: " + repoURL + "\n" +
		"    well-known: " + wellKnown + "\n" +
		"    audience: " + repoURL + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ghaM2Group returns the real user-home m2 group dir the kernel's
// resolution writes into (the JDK ignores HOME), and registers its removal.
func ghaM2Group(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no user home: %v", err)
	}
	group := filepath.Join(home, ".m2", "repository", "org", "example", "gha-e2e")
	if err := os.RemoveAll(group); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(group) })
	return group
}

// TestGhaRenegotiatesExpiredToken is the CI path without a pre-set token:
// rig negotiates the GHA id token itself from the runner's credentials,
// the registry invalidates the minted token mid-run, and the next
// commands (lock, update, check) still succeed — the repo is served with
// the re-negotiated token.
func TestGhaRenegotiatesExpiredToken(t *testing.T) {
	pierSetup(t)
	fi := startGhaIssuer(t)
	old := oidc.GhaWellKnown
	oidc.GhaWellKnown = fi.ts.URL + "/.well-known/openid-configuration"
	t.Cleanup(func() { oidc.GhaWellKnown = old })
	ver := "0.0.1-gha-e2e"
	repo := startGhaRepo(t, fi, ver)
	repo.aud = repo.ts.URL
	group := ghaM2Group(t) // real home, captured before the HOME override below
	ghaGateConfig(t, repo.ts.URL, fi.ts.URL+"/.well-known/openid-configuration")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", fi.ts.URL+"/jwt")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", fi.reqTok)
	t.Setenv("RIG_TOKEN_PIER", "")

	writeFile(t, "deps.edn",
		"{:rig/lib \"example/gha-consumer\"\n"+
			" :deps {org.clojure/clojure {:mvn/version \"1.11.0\"}\n"+
			"        org.example/gha-e2e {:mvn/version \"RELEASE\"}}\n"+
			fmt.Sprintf(" :mvn/repos {\"pier\" {:url %q :auth :oidc}}}", repo.ts.URL))
	cacheDir := t.TempDir()

	// First lock: rig negotiates the GHA id token from the runner's
	// credentials; the floating probe goes through the auth proxy, and the
	// repo serves the metadata and the jar with the minted token.
	code, out := runCLI(t, "lock", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("lock exit = %d, want 0; out: %s", code, out)
	}
	// The registry invalidates the minted token — the mid-run death of a
	// short-lived token (the GHA id tokens live 300s; the job keeps
	// running). The next command re-probes the repo and must re-negotiate:
	// rig update runs the latest op (a fresh metadata probe through the
	// auth proxy) and pins the coord.
	repo.dead1.Store(true)
	code, out = runCLI(t, "update", "org.example/gha-e2e", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("update after expiry exit = %d, want 0; out: %s", code, out)
	}
	if got := atomic.LoadInt32(&fi.mints); got < 2 {
		t.Fatalf("id tokens minted = %d, want >= 2 (the dead token was re-negotiated)", got)
	}
	if got := atomic.LoadInt32(&repo.rejHits); got < 1 {
		t.Error("the repo never rejected the dead token: the refresh path was not exercised")
	}
	doc, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	var found *lockfile.Artifact
	for i := range doc.Artifacts {
		if doc.Artifacts[i].Group == "org.example" && doc.Artifacts[i].Name == "gha-e2e" {
			found = &doc.Artifacts[i]
		}
	}
	if found == nil {
		t.Fatal("lock does not contain org.example/gha-e2e")
	}
	if found.Version != ver {
		t.Errorf("lock version = %q, want %q", found.Version, ver)
	}
	if found.Repository != "pier" {
		t.Errorf("artifact repository = %q, want %q", found.Repository, "pier")
	}
	if want := repo.ts.URL + "/org/example/gha-e2e/" + ver + "/gha-e2e-" + ver + ".jar"; found.URL != want {
		t.Errorf("artifact url = %q, want the repo's real url %q", found.URL, want)
	}
	if found.SHA256 == "" {
		t.Error("artifact sha256 empty")
	}
	if !statOK(filepath.Join(group, ver, "gha-e2e-"+ver+".jar")) {
		t.Error("the jar is not in the local m2")
	}
	if got := atomic.LoadInt32(&fi.mints); got < 2 {
		t.Fatalf("id tokens minted = %d, want >= 2 (the dead token was re-negotiated)", got)
	}
	if got := atomic.LoadInt32(&repo.rejHits); got < 1 {
		t.Error("the repo never rejected the dead token: the refresh path was not exercised")
	}

	// check: the floating requirement is now pinned (rig update re-locked
	// with the exact version), and the locked artifacts verify against the
	// repo with the re-negotiated token.
	code, out = runCLI(t, "check", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("check exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "verified") {
		t.Errorf("check out = %q", out)
	}
}
