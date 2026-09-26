package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// repoServer is a throwaway Maven repository: it records the PUT path and
// Authorization header of every request and accepts everything.
func repoServer(t *testing.T) (*httptest.Server, func() map[string]string) {
	t.Helper()
	var mu sync.Mutex
	seen := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		mu.Lock()
		seen[r.URL.Path] = r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	return srv, func() map[string]string {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]string, len(seen))
		for k, v := range seen {
			out[k] = v
		}
		return out
	}
}

func TestInstall(t *testing.T) {
	hotSetup(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	code, out := runCLI(t, "install", "--cache-dir", t.TempDir(), "-p", "modules/app")
	if code != 0 {
		t.Fatalf("install exit = %d, want 0; out: %s", code, out)
	}
	base := filepath.Join(home, ".m2", "repository", "example", "app", "0.1.0")
	if !statOK(filepath.Join(base, "app-0.1.0.jar")) {
		t.Errorf("jar missing in %s; out: %s", base, out)
	}
	if !statOK(filepath.Join(base, "app-0.1.0.pom")) {
		t.Errorf("pom missing in %s; out: %s", base, out)
	}
	if !strings.Contains(out, "installed example/app 0.1.0") {
		t.Errorf("out = %q", out)
	}
}

func TestPublish(t *testing.T) {
	hotSetup(t)
	t.Setenv("CLOJURE_CLI_ALLOW_HTTP_REPO", "1")
	srv, seen := repoServer(t)
	writeFile(t, "modules/app/deps.edn", `{:rig/lib example/app
 :rig/version "0.1.0"
 :rig/main app.core
 :rig/uberjar? true
 :rig/uberjar-file "target/app-uber.jar"
 :rig/publish? true
 :rig/publish {:repo "test"}
 :mvn/repos {"test" {:url "`+srv.URL+`"}}
 :paths ["src"]
 :deps {org.clojure/clojure {:mvn/version "1.11.0"}}
 :aliases
 {:test {:extra-deps {lambdaisland/kaocha {:mvn/version "1.66.1034"}}
         :extra-paths ["test"]
         :exec-fn kaocha.runner/exec-fn}}}`)
	code, out := runCLI(t, "publish", "--cache-dir", t.TempDir(), "-p", "modules/app")
	if code != 0 {
		t.Fatalf("publish exit = %d, want 0; out: %s", code, out)
	}
	for _, p := range []string{"/example/app/0.1.0/app-0.1.0.jar", "/example/app/0.1.0/app-0.1.0.pom"} {
		if _, ok := seen()[p]; !ok {
			t.Errorf("repo did not receive PUT %s; got %v", p, seen())
		}
	}
	if !strings.Contains(out, "published example/app 0.1.0") {
		t.Errorf("out = %q", out)
	}
}

// TestPublishUnsupportedRepoURL: a repo URL outside http/https (the removed
// s3p:// direct-write transport) fails with a clear error, not an HTTP
// client failure.
func TestPublishUnsupportedRepoURL(t *testing.T) {
	hotSetup(t)
	t.Setenv("CLOJURE_CLI_ALLOW_HTTP_REPO", "1")
	writeFile(t, "modules/app/deps.edn", `{:rig/lib example/app
 :rig/version "0.1.0"
 :rig/main app.core
 :rig/uberjar? true
 :rig/uberjar-file "target/app-uber.jar"
 :rig/publish? true
 :rig/publish {:repo "test"}
 :mvn/repos {"test" {:url "s3p://my-bucket/releases"}}
 :paths ["src"]
 :deps {org.clojure/clojure {:mvn/version "1.11.0"}}
 :aliases
 {:test {:extra-deps {lambdaisland/kaocha {:mvn/version "1.66.1034"}}
         :extra-paths ["test"]
         :exec-fn kaocha.runner/exec-fn}}}`)
	code, out := runCLI(t, "publish", "--cache-dir", t.TempDir(), "-p", "modules/app")
	if code != 2 {
		t.Fatalf("publish exit = %d, want 2; out: %s", code, out)
	}
	if !strings.Contains(out, "unsupported repository URL s3p://my-bucket/releases") {
		t.Errorf("out = %q", out)
	}
}

// TestPublishBearer: publishing to an :auth :oidc repo sends the bearer
// token (the target gate's RIG_TOKEN_<GATE>) on the PUT, not basic auth.
func TestPublishBearer(t *testing.T) {
	hotSetup(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLOJURE_CLI_ALLOW_HTTP_REPO", "1")
	srv, seen := repoServer(t)
	// Gate config fronting the repo, and the gate's token env var.
	if err := os.MkdirAll(filepath.Join(home, ".config", "rig"), 0o755); err != nil {
		t.Fatal(err)
	}
	authYaml := "gates:\n  gate:\n    url: " + srv.URL + "\n    well-known: https://idp.example/w\n"
	if err := os.WriteFile(filepath.Join(home, ".config", "rig", "auth.yaml"), []byte(authYaml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIG_TOKEN_GATE", "pub-token-456")
	writeFile(t, "modules/app/deps.edn", `{:rig/lib example/app
 :rig/version "0.1.0"
 :rig/main app.core
 :rig/uberjar? true
 :rig/uberjar-file "target/app-uber.jar"
 :rig/publish? true
 :rig/publish {:repo "oidc-test"}
 :mvn/repos {"oidc-test" {:url "`+srv.URL+`" :auth :oidc}}
 :paths ["src"]
 :deps {org.clojure/clojure {:mvn/version "1.11.0"}}
 :aliases
 {:test {:extra-deps {lambdaisland/kaocha {:mvn/version "1.66.1034"}}
         :extra-paths ["test"]
         :exec-fn kaocha.runner/exec-fn}}}`)
	code, out := runCLI(t, "publish", "--cache-dir", t.TempDir(), "-p", "modules/app")
	if code != 0 {
		t.Fatalf("publish exit = %d, want 0; out: %s", code, out)
	}
	for _, p := range []string{"/example/app/0.1.0/app-0.1.0.jar", "/example/app/0.1.0/app-0.1.0.pom"} {
		a, ok := seen()[p]
		if !ok {
			t.Errorf("repo did not receive PUT %s; got %v", p, seen())
			continue
		}
		if a != "Bearer pub-token-456" {
			t.Errorf("PUT %s sent Authorization = %q, want the bearer token", p, a)
		}
	}
}

func TestPublishOffline(t *testing.T) {
	hotSetup(t)
	code, out := runCLI(t, "publish", "--offline", "--cache-dir", t.TempDir())
	if code != 2 {
		t.Errorf("exit = %d, want 2; out: %s", code, out)
	}
	if !strings.Contains(out, "requires the network") {
		t.Errorf("out = %q", out)
	}
}

func TestPublishNotPublishable(t *testing.T) {
	hotSetup(t)
	code, out := runCLI(t, "publish", "--cache-dir", t.TempDir(), "-p", "modules/app")
	if code != 2 {
		t.Errorf("exit = %d, want 2; out: %s", code, out)
	}
	if !strings.Contains(out, "not publishable") {
		t.Errorf("out = %q", out)
	}
}
