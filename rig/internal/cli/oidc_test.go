package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/brutasse/rig/internal/lockfile"
	"github.com/brutasse/rig/internal/workspace"
)

func TestOidcRepos(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", "{:rig/modules [\"m\"]\n :mvn/repos {\"root-oidc\" {:url \"https://r.example\" :auth :oidc}}}\n")
	writeFile(t, "m/deps.edn", "{:mvn/repos {\"oidc\" {:url \"https://o.example\" :auth :oidc}\n \"plain\" {:url \"https://p.example\"}\n \"basic\" {:url \"https://b.example\" :auth :basic}}}\n")
	root, err := workspace.Find(".")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := oidcRepos(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"root-oidc": "https://r.example", "oidc": "https://o.example"}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("oidcRepos = %v, want %v", ids, want)
	}
}

func TestOidcReposNoModules(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", "{}\n")
	root, err := workspace.Find(".")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := oidcRepos(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Errorf("oidcRepos = %v, want empty", ids)
	}
}

// writeAuthYaml writes the gate config at the default location under home
// (an isolated HOME, so the real user config never leaks in).
func writeAuthYaml(t *testing.T, content string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "rig", "auth.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLockOidcFailFast: a workspace with an :auth :oidc repo fails fast when
// no gate fronts its URL (no gate config at all) — before any kernel work.
func TestLockOidcFailFast(t *testing.T) {
	hotSetup(t)
	writeAuthYaml(t, "") // isolated HOME: no gate config
	writeFile(t, "deps.edn", "{:rig/modules [\"modules/app\"]\n :mvn/repos {\"oidc\" {:url \"https://oidc.example\" :auth :oidc}}}\n")
	code, out := runCLI(t, "lock", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
	for _, want := range []string{"repo \"oidc\" (https://oidc.example)", "is marked :auth :oidc", "no gate in", "auth.yaml"} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q: %q", want, out)
		}
	}
}

// TestLockOidcResolvesViaProxy: with a gate configured and its token in
// RIG_TOKEN_<GATE>, 'rig lock' starts a loopback auth proxy and points the
// kernel's marked repo at it. The resolver's traffic (tools.deps cannot
// carry a bearer of its own) is forwarded to the real repo URL with the
// token attached, and the lock still attributes the artifact to the marked
// repo with its real URL.
func TestLockOidcResolvesViaProxy(t *testing.T) {
	hotSetup(t)
	// The kernel JVM reads the real user.home m2 (this JDK ignores HOME), so
	// resolution lands in ~/.m2/repository — clean that coord before/after.
	// Capture the real home before isolating HOME for the gate config.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no user home: %v", err)
	}
	coord := filepath.Join(home, ".m2", "repository", "example", "oidc", "fake")
	if err := os.RemoveAll(coord); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(coord) })
	const tok = "Bearer ci-token-123"
	pom := `<project xmlns="http://maven.apache.org/POM/4.0.0"><modelVersion>4.0.0</modelVersion><groupId>example.oidc</groupId><artifactId>fake</artifactId><version>1.0.0</version></project>`
	jar := []byte("fake-artifact-bytes")
	var mu sync.Mutex
	var reqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") != tok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/example/oidc/fake/1.0.0/fake-1.0.0.pom":
			_, _ = w.Write([]byte(pom))
		case "/example/oidc/fake/1.0.0/fake-1.0.0.pom.sha1":
			_, _ = w.Write([]byte(sha1Hex([]byte(pom))))
		case "/example/oidc/fake/1.0.0/fake-1.0.0.jar":
			_, _ = w.Write(jar)
		case "/example/oidc/fake/1.0.0/fake-1.0.0.jar.sha1":
			_, _ = w.Write([]byte(sha1Hex(jar)))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	writeAuthYaml(t, "gates:\n  pier:\n    url: "+srv.URL+"\n    well-known: https://idp.example/w\n")
	t.Setenv("RIG_TOKEN_PIER", "ci-token-123")
	// example.oidc/fake exists only in the OIDC repo.
	writeFile(t, "modules/app/deps.edn", "{:rig/lib example/app\n :rig/version \"0.1.0\"\n :rig/main app.core\n :paths [\"src\"]\n :deps {org.clojure/clojure {:mvn/version \"1.11.0\"}\n        example.oidc/fake {:mvn/version \"1.0.0\"}}\n :mvn/repos {\"oidc\" {:url \""+srv.URL+"\" :auth :oidc}}}\n")
	code, out := runCLI(t, "lock", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("lock exit = %d, want 0; out: %s", code, out)
	}
	doc, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	var art *lockfile.Artifact
	for i := range doc.Artifacts {
		if doc.Artifacts[i].Group == "example.oidc" && doc.Artifacts[i].Name == "fake" {
			art = &doc.Artifacts[i]
		}
	}
	if art == nil {
		t.Fatal("example.oidc/fake missing from the lock")
	}
	if art.Repository != "oidc" {
		t.Errorf("artifact repository = %q, want %q", art.Repository, "oidc")
	}
	if !strings.HasPrefix(art.URL, srv.URL) {
		t.Errorf("artifact URL = %q, want the real repo URL %q prefix", art.URL, srv.URL)
	}
	if !m2Has(coord, "1.0.0", "fake-1.0.0.jar") {
		t.Error("the jar is not in the local m2")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reqs) == 0 {
		t.Fatal("the oidc repo was never requested; out: " + out)
	}
	for _, r := range reqs {
		if !strings.HasSuffix(r, " "+tok) {
			t.Errorf("request without the bearer: %s", r)
		}
	}
}

// TestLockOidcMultipleGates: two repos behind two different OIDC gates each
// get their own gate's bearer on every request — no cross-talk.
func TestLockOidcMultipleGates(t *testing.T) {
	hotSetup(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no user home: %v", err)
	}
	coordA := filepath.Join(home, ".m2", "repository", "example", "a", "fake")
	coordB := filepath.Join(home, ".m2", "repository", "example", "b", "fake")
	os.RemoveAll(coordA)
	os.RemoveAll(coordB)
	t.Cleanup(func() { os.RemoveAll(coordA); os.RemoveAll(coordB) })

	// newServer serves the given artifact paths, enforcing its token.
	newServer := func(tok string, paths map[string]string) (*httptest.Server, *[]string) {
		var mu sync.Mutex
		var reqs []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			reqs = append(reqs, r.Header.Get("Authorization"))
			mu.Unlock()
			if r.Header.Get("Authorization") != tok {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if b, ok := paths[r.URL.Path]; ok {
				_, _ = w.Write([]byte(b))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(srv.Close)
		return srv, &reqs
	}
	// artifact builds the pom/jar/sha1 paths of group/name at version v.
	artifact := func(group, name, v string) map[string]string {
		base := "/" + strings.ReplaceAll(group, ".", "/") + "/" + name + "/" + v + "/" + name + "-" + v
		pom := `<project xmlns="http://maven.apache.org/POM/4.0.0"><modelVersion>4.0.0</modelVersion><groupId>` + group + `</groupId><artifactId>` + name + `</artifactId><version>` + v + `</version></project>`
		jar := []byte("fake-artifact-bytes-" + name)
		return map[string]string{
			base + ".pom":      pom,
			base + ".pom.sha1": sha1Hex([]byte(pom)),
			base + ".jar":      string(jar),
			base + ".jar.sha1": sha1Hex(jar),
		}
	}
	sa, seenA := newServer("Bearer tok-a", artifact("example.a", "fake", "1.0.0"))
	sb, seenB := newServer("Bearer tok-b", artifact("example.b", "fake", "1.0.0"))

	writeAuthYaml(t, "gates:\n"+
		"  gate-a:\n    url: "+sa.URL+"\n    well-known: https://idp-a.example/w\n"+
		"  gate-b:\n    url: "+sb.URL+"\n    well-known: https://idp-b.example/w\n")
	t.Setenv("RIG_TOKEN_GATE_A", "tok-a")
	t.Setenv("RIG_TOKEN_GATE_B", "tok-b")
	writeFile(t, "modules/app/deps.edn", "{:rig/lib example/app\n :rig/version \"0.1.0\"\n :rig/main app.core\n :paths [\"src\"]\n"+
		" :deps {org.clojure/clojure {:mvn/version \"1.11.0\"}\n"+
		"        example.a/fake {:mvn/version \"1.0.0\"}\n"+
		"        example.b/fake {:mvn/version \"1.0.0\"}}\n"+
		" :mvn/repos {\"repoA\" {:url \""+sa.URL+"\" :auth :oidc}\n"+
		"               \"repoB\" {:url \""+sb.URL+"\" :auth :oidc}}}\n")
	code, out := runCLI(t, "lock", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("lock exit = %d, want 0; out: %s", code, out)
	}
	doc, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []string{"example.a", "example.b"} {
		var art *lockfile.Artifact
		for i := range doc.Artifacts {
			if doc.Artifacts[i].Group == g && doc.Artifacts[i].Name == "fake" {
				art = &doc.Artifacts[i]
			}
		}
		if art == nil {
			t.Fatalf("%s/fake missing from the lock", g)
		}
	}
	if len(*seenA) == 0 || len(*seenB) == 0 {
		t.Fatalf("both repos must be requested: A=%d B=%d; out: %s", len(*seenA), len(*seenB), out)
	}
	for _, r := range *seenA {
		if r != "Bearer tok-a" {
			t.Errorf("repo A saw %q, want Bearer tok-a", r)
		}
	}
	for _, r := range *seenB {
		if r != "Bearer tok-b" {
			t.Errorf("repo B saw %q, want Bearer tok-b", r)
		}
	}
}

// m2Has reports whether m2/coord/version/name exists.
func m2Has(coord, version, name string) bool {
	_, err := os.Stat(filepath.Join(coord, version, name))
	return err == nil
}
