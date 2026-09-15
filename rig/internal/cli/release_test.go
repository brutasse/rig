package cli

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitOut(t.Context(), dir, args...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return out
}

// releaseFixture enables publishing against srv (the module's version moves
// to a VERSION file at 1.0.0-snapshot), re-locks, and puts the workspace in
// a git repo with a bare origin. Returns the branch name.
func releaseFixture(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	// The throwaway repo is plain http; tools.deps refuses http repos unless
	// this is set (inherited by the kernel JVM).
	t.Setenv("CLOJURE_CLI_ALLOW_HTTP_REPO", "1")
	writeFile(t, "modules/app/deps.edn", `{:rig/lib example/app
 :rig/main app.core
 :rig/publish? true
 :rig/publish {:repo "test"}
 :mvn/repos {"test" {:url "`+srv.URL+`"}}
 :paths ["src"]
 :deps {org.clojure/clojure {:mvn/version "1.11.0"}}
 :aliases
 {:test {:extra-deps {lambdaisland/kaocha {:mvn/version "1.66.1034"}}
         :extra-paths ["test"]
         :exec-fn kaocha.runner/exec-fn}}}`)
	writeFile(t, "VERSION", "1.0.0-snapshot\n")

	origin := t.TempDir()
	gitRun(t, origin, "init", "--bare")
	gitRun(t, ".", "init")
	gitRun(t, ".", "config", "user.email", "rig@test")
	gitRun(t, ".", "config", "user.name", "rig")
	gitRun(t, ".", "config", "commit.gpgsign", "false")
	gitRun(t, ".", "config", "tag.gpgsign", "false")
	gitRun(t, ".", "add", ".")
	gitRun(t, ".", "commit", "-m", "init")
	gitRun(t, ".", "remote", "add", "origin", origin)
	gitRun(t, ".", "push", "origin", "HEAD")
	branch := gitRun(t, ".", "rev-parse", "--abbrev-ref", "HEAD")

	code, out := runCLI(t, "lock", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("lock exit = %d, want 0; out: %s", code, out)
	}
	return branch
}

func TestReleaseDryRun(t *testing.T) {
	hotSetup(t)
	srv, _ := repoServer(t)
	releaseFixture(t, srv)
	before := gitRun(t, ".", "rev-list", "--count", "HEAD")

	code, out := runCLI(t, "release", "--dry-run")
	if code != 0 {
		t.Fatalf("dry-run exit = %d, want 0; out: %s", code, out)
	}
	for _, want := range []string{
		"1. remove-snapshot", "2. publish", "3. commit", "4. tag",
		"5. bump-and-snapshot", "6. commit", "7. push",
		"1.0.0-snapshot -> 1.0.0", "example/app 1.0.0 -> test",
		"1.0.0 -> 1.0.1-snapshot", "v1.0.0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run out missing %q; out: %s", want, out)
		}
	}

	// No side effects.
	if got := readFile(t, "VERSION"); got != "1.0.0-snapshot\n" {
		t.Errorf("VERSION changed by dry-run: %q", got)
	}
	if got := gitRun(t, ".", "rev-list", "--count", "HEAD"); got != before {
		t.Errorf("dry-run changed the commit count: %s -> %s", before, got)
	}
	if _, err := gitOut(t.Context(), ".", "rev-parse", "--verify", "--quiet", "refs/tags/v1.0.0"); err == nil {
		t.Error("dry-run created a tag")
	}
}

func TestRelease(t *testing.T) {
	hotSetup(t)
	srv, seen := repoServer(t)
	branch := releaseFixture(t, srv)

	code, out := runCLI(t, "release")
	if code != 0 {
		t.Fatalf("release exit = %d, want 0; out: %s", code, out)
	}
	for _, p := range []string{"/example/app/1.0.0/app-1.0.0.jar", "/example/app/1.0.0/app-1.0.0.pom"} {
		if _, ok := seen()[p]; !ok {
			t.Errorf("repo did not receive PUT %s; got %v", p, seen())
		}
	}
	if got := readFile(t, "VERSION"); got != "1.0.1-snapshot\n" {
		t.Errorf("VERSION = %q, want 1.0.1-snapshot", got)
	}
	log := gitRun(t, ".", "log", "--format=%s")
	if !strings.Contains(log, "Release v1.0.0") {
		t.Errorf("git log missing release commit: %q", log)
	}
	if !strings.Contains(log, "Bump version to 1.0.1-snapshot") {
		t.Errorf("git log missing bump commit: %q", log)
	}
	if got := gitRun(t, ".", "tag", "-l"); got != "v1.0.0" {
		t.Errorf("tags = %q, want v1.0.0", got)
	}
	if got := gitRun(t, ".", "ls-remote", "--tags", "origin"); !strings.Contains(got, "refs/tags/v1.0.0") {
		t.Errorf("origin missing the v1.0.0 tag: %q", got)
	}
	if got := gitRun(t, ".", "ls-remote", "--heads", "origin", branch); got == "" {
		t.Errorf("origin missing the %s branch", branch)
	}
}
