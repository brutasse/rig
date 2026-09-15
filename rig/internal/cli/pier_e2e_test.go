package cli

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brutasse/rig/internal/digest"
	"github.com/brutasse/rig/internal/jvm"
	"github.com/brutasse/rig/internal/kernel"
	"github.com/brutasse/rig/internal/lockfile"
)

// Pier E2E against a live environment. Gated by RIG_TEST_PIER: the path to a
// Pier checkout whose test env (see .scratch) is running — a fake OIDC issuer
// (discovery + JWKS, auto-approved grants) on :8090 and a Pier on :8089
// serving an OIDC-gated Maven repo (group org.example reserved for uploads)
// backed by S3.
//
// Every test here skips when RIG_TEST_PIER is unset or the env is down.

// pierEnv returns the Pier checkout path, the pier URL, the fake issuer URL,
// and a valid bearer token for Pier. It skips when the gate env is unset or
// the environment is not running.
func pierEnv(t *testing.T) (pierCheckout, pier, issuer, token string) {
	t.Helper()
	pierCheckout = os.Getenv("RIG_TEST_PIER")
	if pierCheckout == "" {
		t.Skip("set RIG_TEST_PIER=<pier checkout> to run the Pier E2E (live env must be running)")
	}
	pier = envOr("RIG_TEST_PIER_URL", "http://127.0.0.1:8089")
	issuer = envOr("RIG_TEST_PIER_ISSUER", "http://127.0.0.1:8090")
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(issuer + "/.well-known/openid-configuration")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Skipf("OIDC issuer not reachable at %s: %v (is the Pier test env running?)", issuer, err)
	}
	resp.Body.Close()
	if resp, err := client.Get(pier + "/"); err != nil {
		t.Skipf("pier not reachable at %s: %v (is the Pier test env running?)", pier, err)
	} else {
		resp.Body.Close()
	}
	token = os.Getenv("RIG_TEST_PIER_TOKEN")
	if token == "" {
		dir := filepath.Join(pierCheckout, ".scratch", "fakeissuer")
		out, err := exec.Command(filepath.Join(dir, "fakeissuer"), "token",
			"-file", filepath.Join(dir, "key.json")).Output()
		if err != nil {
			t.Skipf("cannot mint a token with %s: %v (set RIG_TEST_PIER_TOKEN)", dir, err)
		}
		token = strings.TrimSpace(string(out))
	}
	return
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// pierSetup pins the kernel jar and chdirs into a fresh empty workspace —
// the tests write their own manifests.
func pierSetup(t *testing.T) {
	t.Helper()
	jar := kernelJarPath(t)
	if _, err := jvm.Find(); err != nil {
		t.Skipf("no java available: %v", err)
	}
	sha, err := digest.File(jar)
	if err != nil {
		t.Fatal(err)
	}
	oldSHA := kernel.Current.JARSHA
	kernel.Current.JARSHA = sha
	os.Setenv("RIG_KERNEL_JAR", jar)
	t.Cleanup(func() {
		kernel.Current.JARSHA = oldSHA
		os.Unsetenv("RIG_KERNEL_JAR")
	})
	t.Chdir(t.TempDir())
}

// pierRepo is the :mvn/repos entry for the live pier (a key/value pair for a
// manifest map).
func pierRepo(pier string) string {
	return fmt.Sprintf(`:mvn/repos {"pier" {:url %q :auth :oidc}}`, pier)
}

// pierGateConfig isolates HOME and writes the gate config: a single gate
// "pier" fronting the live pier, negotiating with the fake issuer. The
// audience/client-id match the live fake issuer's grant configuration.
func pierGateConfig(t *testing.T, pier, issuer string) {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	path := filepath.Join(tmpHome, ".config", "rig", "auth.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "gates:\n" +
		"  pier:\n" +
		"    url: " + pier + "\n" +
		"    well-known: " + issuer + "/.well-known/openid-configuration\n" +
		"    audience: rig-s3\n" +
		"    client-id: rig-auth\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// pierNoGateConfig isolates HOME with no gate config at all.
func pierNoGateConfig(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

// oidcCacheFiles lists the per-gate token cache files under dir/oidc.
func oidcCacheFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "oidc"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, e.Name())
		}
	}
	return out
}

// runLock runs `rig lock`, retrying once on repo rate limits (429) from the
// standard-repo probes, like TestLockVerifyEndToEnd.
func runLock(t *testing.T, cacheDir string) (int, string) {
	t.Helper()
	code, out := runCLI(t, "lock", "--cache-dir", cacheDir)
	if code != 0 && strings.Contains(out, "status 429") {
		time.Sleep(10 * time.Second)
		code, out = runCLI(t, "lock", "--cache-dir", cacheDir)
	}
	return code, out
}

// httpStatus GETs url with (bearer != "") or without the Authorization
// header and returns the status code.
func httpStatus(t *testing.T, url, bearer string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// pierConsumerWorkspace writes the consumer manifest: the e2e artifact lives
// only in pier, behind the OIDC gate.
func pierConsumerWorkspace(t *testing.T, pier, ver string) {
	t.Helper()
	writeFile(t, "deps.edn",
		"{:rig/lib \"example/pier-consumer\"\n"+
			" :deps {org.clojure/clojure {:mvn/version \"1.11.0\"}\n"+
			"        org.example/pier-e2e {:mvn/version \""+ver+"\"}}\n"+
			pierRepo(pier)+"}")
}

// pierPublisherWorkspace writes the publishing manifest: a single-module
// project deployed to pier.
func pierPublisherWorkspace(t *testing.T, pier, ver string) {
	t.Helper()
	writeFile(t, "deps.edn",
		"{:rig/lib \"org.example/pier-e2e\"\n"+
			" :rig/version \""+ver+"\"\n"+
			" :paths [\"src\"]\n"+
			" :rig/publish? true\n"+
			" :rig/publish {:repo \"pier\"}\n"+
			" :deps {org.clojure/clojure {:mvn/version \"1.11.0\"}}\n"+
			pierRepo(pier)+"}")
	writeFile(t, "src/org/example/pier_e2e.clj", "(ns org.example.pier-e2e)\n")
}

// pierM2Group returns the real user-home m2 group dir the kernel's resolution
// writes into (this JDK ignores HOME), and registers its removal.
func pierM2Group(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no user home: %v", err)
	}
	group := filepath.Join(home, ".m2", "repository", "org", "example", "pier-e2e")
	if err := os.RemoveAll(group); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(group) })
	return group
}

// pierDeletePublished removes the objects published by ver from pier.
func pierDeletePublished(t *testing.T, pier, ver, token string) {
	t.Helper()
	base := pier + "/org/example/pier-e2e/" + ver
	names := []string{"pier-e2e-" + ver + ".jar.md5", "pier-e2e-" + ver + ".jar.sha1",
		"pier-e2e-" + ver + ".pom.md5", "pier-e2e-" + ver + ".pom.sha1",
		"pier-e2e-" + ver + ".jar", "pier-e2e-" + ver + ".pom"}
	metadata := pier + "/org/example/pier-e2e/maven-metadata.xml"
	t.Cleanup(func() {
		urls := make([]string, 0, len(names)+1)
		for _, f := range names {
			urls = append(urls, base+"/"+f)
		}
		urls = append(urls, metadata)
		for _, u := range urls {
			req, _ := http.NewRequest(http.MethodDelete, u, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				resp.Body.Close()
			}
		}
	})
}

// pierPutMetadata publishes the artifact's maven-metadata.xml (a real Maven
// repository maintains it; the S3-backed Pier does not generate it), listing
// only ver as the release.
func pierPutMetadata(t *testing.T, pier, ver, token string) {
	t.Helper()
	body := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<metadata>` + "\n" +
		`  <groupId>org.example</groupId>` + "\n" +
		`  <artifactId>pier-e2e</artifactId>` + "\n" +
		`  <versioning>` + "\n" +
		`    <latest>` + ver + `</latest>` + "\n" +
		`    <release>` + ver + `</release>` + "\n" +
		`    <versions><version>` + ver + `</version></versions>` + "\n" +
		`    <lastUpdated>` + time.Now().UTC().Format("20060102150405") + `</lastUpdated>` + "\n" +
		`  </versioning>` + "\n" +
		`</metadata>` + "\n"
	req, err := http.NewRequest(http.MethodPut,
		pier+"/org/example/pier-e2e/maven-metadata.xml",
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT maven-metadata.xml: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("PUT maven-metadata.xml = %d, want 2xx", resp.StatusCode)
	}
}

// TestPierFailFastNoGate: a workspace with a pier-marked repo but no gate
// config fails fast with the actionable error, before any resolution work.
func TestPierFailFastNoGate(t *testing.T) {
	pierSetup(t)
	_, pier, _, _ := pierEnv(t)
	pierNoGateConfig(t)
	pierConsumerWorkspace(t, pier, "0.0.1")
	code, out := runCLI(t, "lock", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
	for _, want := range []string{"repo \"pier\" (", "is marked :auth :oidc", "no gate in", "auth.yaml"} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q: %q", want, out)
		}
	}
}

// TestPierPublishLockVerify is the CI path: RIG_TOKEN_PIER is injected, rig
// publishes to pier with the bearer, a consumer workspace locks the artifact
// from pier (resolved through rig's local auth proxy) and verifies it.
func TestPierPublishLockVerify(t *testing.T) {
	pierSetup(t)
	_, pier, issuer, tok := pierEnv(t)
	group := pierM2Group(t) // real home, captured before the HOME override below
	pierGateConfig(t, pier, issuer)
	t.Setenv("RIG_TOKEN_PIER", tok)

	ver := fmt.Sprintf("0.0.1-e2e-%d", time.Now().UnixNano())
	pierPublisherWorkspace(t, pier, ver)
	pierDeletePublished(t, pier, ver, tok)
	cacheDir := t.TempDir()

	if code, out := runLock(t, cacheDir); code != 0 {
		t.Fatalf("publish lock exit = %d, want 0; out: %s", code, out)
	}
	code, out := runCLI(t, "publish", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("publish exit = %d, want 0; out: %s", code, out)
	}
	wantLine := "published org.example/pier-e2e " + ver + " -> " +
		pier + "/org/example/pier-e2e/" + ver + "/pier-e2e-" + ver + ".jar"
	if !strings.Contains(out, wantLine) {
		t.Fatalf("publish out missing %q: %q", wantLine, out)
	}

	// The artifact is behind the gate: 200 with the bearer, 401 without.
	base := pier + "/org/example/pier-e2e/" + ver
	if got := httpStatus(t, base+"/pier-e2e-"+ver+".jar", tok); got != http.StatusOK {
		t.Errorf("GET jar with bearer = %d, want 200", got)
	}
	if got := httpStatus(t, base+"/pier-e2e-"+ver+".jar", ""); got != http.StatusUnauthorized {
		t.Errorf("GET jar without bearer = %d, want 401", got)
	}

	// The consuming workspace: the artifact lives only in pier.
	t.Chdir(t.TempDir())
	pierConsumerWorkspace(t, pier, ver)
	if code, out := runLock(t, cacheDir); code != 0 {
		t.Fatalf("consume lock exit = %d, want 0; out: %s", code, out)
	}
	doc, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range doc.Artifacts {
		if a.Group == "org.example" && a.Name == "pier-e2e" {
			found = true
			if a.Repository != "pier" {
				t.Errorf("artifact repository = %q, want %q", a.Repository, "pier")
			}
			if a.SHA256 == "" {
				t.Error("artifact sha256 empty")
			}
		}
	}
	if !found {
		t.Fatal("lock does not contain org.example/pier-e2e")
	}
	// The resolver pulled the jar from the gated repo through rig's local
	// auth proxy into the real m2 (that is how tools.deps resolved it).
	verDir := filepath.Join(group, ver)
	for _, f := range []string{"pier-e2e-" + ver + ".jar", "_remote.repositories"} {
		if !statOK(filepath.Join(verDir, f)) {
			t.Errorf("m2 missing %s", f)
		}
	}

	code, out = runCLI(t, "verify", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("verify exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "verified") {
		t.Errorf("verify out = %q", out)
	}
}

// TestPierFloatingVersion: a consumer requiring the e2e artifact at RELEASE
// pins the just-published version. The kernel probes the repo's
// maven-metadata with the bearer (--force overrides the default cooldown,
// a fresh publish), and the artifact resolves through the local auth proxy.
// The test publishes the maven-metadata.xml object itself: a real Maven
// repository maintains it, the S3-backed Pier does not.
func TestPierFloatingVersion(t *testing.T) {
	pierSetup(t)
	_, pier, issuer, tok := pierEnv(t)
	group := pierM2Group(t) // real home, captured before the HOME override below
	pierGateConfig(t, pier, issuer)
	t.Setenv("RIG_TOKEN_PIER", tok)

	ver := fmt.Sprintf("0.0.2-e2e-%d", time.Now().UnixNano())
	pierPublisherWorkspace(t, pier, ver)
	pierDeletePublished(t, pier, ver, tok)
	cacheDir := t.TempDir()

	if code, out := runLock(t, cacheDir); code != 0 {
		t.Fatalf("publish lock exit = %d, want 0; out: %s", code, out)
	}
	code, out := runCLI(t, "publish", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("publish exit = %d, want 0; out: %s", code, out)
	}
	pierPutMetadata(t, pier, ver, tok)

	// The consuming workspace: floating RELEASE, the artifact only in pier.
	t.Chdir(t.TempDir())
	writeFile(t, "deps.edn",
		"{:rig/lib \"example/pier-consumer\"\n"+
			" :deps {org.clojure/clojure {:mvn/version \"1.11.0\"}\n"+
			"        org.example/pier-e2e {:mvn/version \"RELEASE\"}}\n"+
			pierRepo(pier)+"}")
	if code, out := runCLI(t, "lock", "--force", "--cache-dir", cacheDir); code != 0 {
		t.Fatalf("floating lock exit = %d, want 0; out: %s", code, out)
	}
	doc, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range doc.Artifacts {
		if a.Group == "org.example" && a.Name == "pier-e2e" {
			found = true
			if a.Version != ver {
				t.Errorf("lock version = %q, want the published %q", a.Version, ver)
			}
			if a.Repository != "pier" {
				t.Errorf("artifact repository = %q, want %q", a.Repository, "pier")
			}
		}
	}
	if !found {
		t.Fatal("lock does not contain org.example/pier-e2e")
	}
	if !statOK(filepath.Join(group, ver, "pier-e2e-"+ver+".jar")) {
		t.Error("the jar is not in the local m2")
	}

	code, out = runCLI(t, "verify", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("verify exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "verified") {
		t.Errorf("verify out = %q", out)
	}
}

// TestPierLockViaDeviceFlow is the non-CI path: no RIG_TOKEN_PIER — rig
// negotiates the bearer itself with its embedded device-code flow (the auto
// flow takes device: xdg-open is hidden from PATH, a java-only PATH, so
// machines with a browser do not end up in the 5-minute browser flow). The
// negotiated token is verified against the issuer's JWKS, cached in the
// state dir, and reused by later commands. Every pier request carries the
// negotiated bearer.
func TestPierLockViaDeviceFlow(t *testing.T) {
	pierSetup(t)
	// The minted token is only for the test-side cleanup; rig itself never
	// sees it (RIG_TOKEN_PIER stays unset, the bearer comes from
	// rig's own negotiation).
	_, pier, issuer, tok := pierEnv(t)
	group := pierM2Group(t) // real home, captured before the HOME override below
	pierGateConfig(t, pier, issuer)

	// A java-only PATH hides xdg-open, steering the auto flow to device.
	java, err := jvm.Find()
	if err != nil {
		t.Skipf("no java available: %v", err)
	}
	binDir := t.TempDir()
	if err := os.Symlink(java, filepath.Join(binDir, "java")); err != nil {
		t.Skipf("cannot symlink java for the PATH-restricted flow: %v", err)
	}
	t.Setenv("PATH", binDir)

	ver := fmt.Sprintf("0.0.3-e2e-%d", time.Now().UnixNano())
	pierPublisherWorkspace(t, pier, ver)
	pierDeletePublished(t, pier, ver, tok)
	cacheDir := t.TempDir()

	// First rig command: rig negotiates the device flow token itself.
	if code, out := runLock(t, cacheDir); code != 0 {
		t.Fatalf("publish lock exit = %d, want 0; out: %s", code, out)
	}
	if len(oidcCacheFiles(t, cacheDir)) == 0 {
		t.Error("no per-gate token cache under", filepath.Join(cacheDir, "oidc"))
	}
	code, out := runCLI(t, "publish", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("publish exit = %d, want 0; out: %s", code, out)
	}

	// The consuming workspace locks from pier with the (cached) token —
	// no second negotiation.
	negBefore := len(oidcCacheFiles(t, cacheDir))
	t.Chdir(t.TempDir())
	pierConsumerWorkspace(t, pier, ver)
	if code, out := runLock(t, cacheDir); code != 0 {
		t.Fatalf("consume lock exit = %d, want 0; out: %s", code, out)
	}
	doc, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range doc.Artifacts {
		if a.Group == "org.example" && a.Name == "pier-e2e" {
			found = true
			if a.Repository != "pier" {
				t.Errorf("artifact repository = %q, want %q", a.Repository, "pier")
			}
		}
	}
	if !found {
		t.Fatal("lock does not contain org.example/pier-e2e")
	}
	if got := len(oidcCacheFiles(t, cacheDir)); got != negBefore {
		t.Errorf("token cache files = %d, want %d (cached token reused)", got, negBefore)
	}
	// The kernel JVM ignores HOME: resolution wrote to the real user-home m2,
	// served by pier with the negotiated bearer (nothing else could).
	verDir := filepath.Join(group, ver)
	for _, f := range []string{"pier-e2e-" + ver + ".jar", "_remote.repositories"} {
		if !statOK(filepath.Join(verDir, f)) {
			t.Errorf("m2 missing %s", f)
		}
	}
}

// TestPierSingleRepo is the single-repository pattern: the workspace
// redeclares the standard repo ids (central, clojars) against pier, so every
// probe, fetch, and lock attribution goes through it — the artifact is
// served by pier's pull-through of the test env's fake-central upstream,
// never by a real repository.
func TestPierSingleRepo(t *testing.T) {
	pierSetup(t)
	_, pier, issuer, tok := pierEnv(t)
	home, err := os.UserHomeDir() // real home, captured before the HOME override below
	if err != nil {
		t.Skipf("no user home: %v", err)
	}
	group := filepath.Join(home, ".m2", "repository", "org", "fake", "upstream-lib")
	if err := os.RemoveAll(group); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(group) })
	pierGateConfig(t, pier, issuer)
	t.Setenv("RIG_TOKEN_PIER", tok)

	// The dep exists only behind the gate.
	writeFile(t, "deps.edn",
		"{:rig/lib \"example/single-repo\"\n"+
			" :deps {org.fake/upstream-lib {:mvn/version \"1.0\"}}\n"+
			fmt.Sprintf(" :mvn/repos {\"central\" {:url %q :auth :oidc}\n", pier)+
			fmt.Sprintf("            \"clojars\" {:url %q :auth :oidc}}}", pier))
	cacheDir := t.TempDir()

	if code, out := runLock(t, cacheDir); code != 0 {
		t.Fatalf("lock exit = %d, want 0; out: %s", code, out)
	}
	doc, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range doc.Artifacts {
		if a.Group == "org.fake" && a.Name == "upstream-lib" {
			found = true
			if a.Version != "1.0" {
				t.Errorf("lock version = %q, want %q", a.Version, "1.0")
			}
			if a.Repository != "central" {
				t.Errorf("artifact repository = %q, want %q", a.Repository, "central")
			}
			if want := pier + "/org/fake/upstream-lib/1.0/upstream-lib-1.0.jar"; a.URL != want {
				t.Errorf("artifact url = %q, want %q", a.URL, want)
			}
			if a.SHA256 == "" {
				t.Error("artifact sha256 empty")
			}
		}
	}
	if !found {
		t.Fatal("lock does not contain org.fake/upstream-lib")
	}
	if !statOK(filepath.Join(group, "1.0", "upstream-lib-1.0.jar")) {
		t.Error("the jar is not in the local m2")
	}
	// The pull-through path is behind the gate like every other object.
	if got := httpStatus(t, pier+"/org/fake/upstream-lib/1.0/upstream-lib-1.0.jar", ""); got != http.StatusUnauthorized {
		t.Errorf("GET pulled jar without bearer = %d, want 401", got)
	}
	code, out := runCLI(t, "verify", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("verify exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "verified") {
		t.Errorf("verify out = %q", out)
	}
}
