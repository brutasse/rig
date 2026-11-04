package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brutasse/rig/internal/jvm"
	"github.com/brutasse/rig/internal/lockfile"
)

// TestGitDepColdRunnerProvisions (Arnaud's field failure): a fresh machine
// (no ~/.gitlibs) must be able to run a hot command on a workspace whose
// committed lock pins a git dep — the command provisions the checkout
// itself, and a missing checkout under --offline fails pointing at the
// gitlibs cache.
func TestGitDepColdRunnerProvisions(t *testing.T) {
	jar := kernelJarPath(t)
	if _, err := jvm.Find(); err != nil {
		t.Skipf("no java available: %v", err)
	}
	os.Setenv("RIG_KERNEL_JAR", jar)
	os.Setenv("RIG_RUNNER_JAR", runnerJarPath(t))
	t.Cleanup(func() {
		os.Unsetenv("RIG_KERNEL_JAR")
		os.Unsetenv("RIG_RUNNER_JAR")
	})

	// A git repo holding one commit — the locked git dep (file:// url).
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo,
			"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	writeFile(t, filepath.Join(repo, "deps.edn"), "{:paths [\"src\"]}\n")
	writeFile(t, filepath.Join(repo, "src", "x.clj"), "(ns gitlib.x)\n")
	git("init")
	git("add", "-A")
	git("commit", "-m", "x")
	shaOut, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(shaOut))

	// The workspace: one module whose only dep is the git coord.
	ws := t.TempDir()
	t.Chdir(ws)
	writeFile(t, "deps.edn",
		"{:rig/lib example/gittest\n"+
			" :rig/version \"0.1.0\"\n"+
			" :paths [\"src\"]\n"+
			" :deps {example/gitlib {:git/url \"file://"+repo+"\"\n"+
			"                         :git/sha \""+sha+"\"}}}\n")
	writeFile(t, filepath.Join("src", "x.clj"), "(ns example.x)\n")

	// A fresh gitlibs root: no checkouts at all. GITLIBS is the override
	// both the kernel (tools.gitlibs) and rig honor; on Linux user.home
	// cannot be redirected via $HOME, so a cold-runner test must use it.
	// CRaC is parked on an empty store: a restored kernel would keep the
	// bootstrap run's environment and provision into the wrong place.
	gitlibs := t.TempDir()
	t.Setenv("GITLIBS", gitlibs)
	t.Setenv("RIG_CRAC_DIR", t.TempDir())

	cacheDir := t.TempDir()

	// lock provisions the checkout under the fresh home.
	code, out := runCLI(t, "lock", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("lock exit = %d, want 0; out: %s", code, out)
	}
	doc, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	var gitArtifact *lockfile.Artifact
	for i := range doc.Artifacts {
		if doc.Artifacts[i].Kind == "git" {
			gitArtifact = &doc.Artifacts[i]
			break
		}
	}
	if gitArtifact == nil {
		t.Fatalf("no git artifact in the lock: %+v", doc.Artifacts)
	}
	if gitArtifact.Git.URL != "file://"+repo {
		t.Errorf("git url = %s, want %s", gitArtifact.Git.URL, "file://"+repo)
	}
	if gitArtifact.Git.SHA != sha {
		t.Errorf("git sha = %s, want %s", gitArtifact.Git.SHA, sha)
	}
	checkout := filepath.Join(gitlibs, "libs", "example", "gitlib", sha)
	if !statOK(checkout) {
		t.Fatalf("lock did not provision the checkout at %s", checkout)
	}

	// The cold runner: the cache is gone, `rig build` must re-provision it.
	if err := os.RemoveAll(filepath.Join(gitlibs, "libs")); err != nil {
		t.Fatal(err)
	}
	code, out = runCLI(t, "build", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("build exit = %d, want 0 (it must provision the checkout); out: %s", code, out)
	}
	if !statOK(checkout) {
		t.Fatalf("build did not provision the checkout at %s", checkout)
	}

	// Offline: a missing checkout cannot be provisioned; the failure
	// points at the gitlibs cache.
	if err := os.RemoveAll(filepath.Join(gitlibs, "libs")); err != nil {
		t.Fatal(err)
	}
	code, out = runCLI(t, "build", "--offline", "--cache-dir", cacheDir)
	if code == 0 {
		t.Fatalf("offline build exit = 0, want failure; out: %s", out)
	}
	if !strings.Contains(out, "gitlibs") {
		t.Fatalf("offline build out = %q, want the gitlibs cache hint", out)
	}
}
