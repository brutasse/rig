package cli

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/brutasse/rig/internal/digest"
	"github.com/brutasse/rig/internal/jvm"
	"github.com/brutasse/rig/internal/kernel"
)

// mavenTs renders ms as a UTC maven timestamp (yyyyMMddHHmmss).
func mavenTs(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("20060102150405")
}

func sha1Hex(b []byte) string {
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:])
}

func writeString(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fileMavenRepo builds a file:// maven repo serving group/name with per-version
// publication times ({version published-ms}); returns the repo base URL.
func fileMavenRepo(t *testing.T, group, name string, published map[string]int64) string {
	t.Helper()
	dir := t.TempDir()
	base := filepath.Join(dir, strings.ReplaceAll(group, ".", "/"), name)
	versions := make([]string, 0, len(published))
	for v := range published {
		versions = append(versions, v)
	}
	sort.Strings(versions)

	var md strings.Builder
	md.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<metadata>\n  <versioning>\n    <versions>\n")
	for _, v := range versions {
		md.WriteString("      <version>" + v + "</version>\n")
	}
	md.WriteString("    </versions>\n    <lastUpdated>" +
		mavenTs(time.Now().UnixMilli()) + "</lastUpdated>\n  </versioning>\n</metadata>\n")
	writeString(t, filepath.Join(base, "maven-metadata.xml"), md.String())

	var per strings.Builder
	per.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<metadata>\n  <versioning>\n    <versions>\n")
	for _, v := range versions {
		per.WriteString("      <version>\n        <value>" + v +
			"</value>\n        <updated>" + mavenTs(published[v]) + "</updated>\n      </version>\n")
	}
	per.WriteString("    </versions>\n  </versioning>\n</metadata>\n")
	writeString(t, filepath.Join(base, "maven-metadata-f.xml"), per.String())

	for _, v := range versions {
		vdir := filepath.Join(base, v)
		pom := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
			"<project xmlns=\"http://maven.apache.org/POM/4.0.0\">\n" +
			"  <modelVersion>4.0.0</modelVersion>\n" +
			"  <groupId>" + group + "</groupId>\n" +
			"  <artifactId>" + name + "</artifactId>\n" +
			"  <version>" + v + "</version>\n" +
			"  <packaging>jar</packaging>\n</project>\n"
		jar := "jar-bytes-" + v + "\n"
		writeString(t, filepath.Join(vdir, name+"-"+v+".pom"), pom)
		writeString(t, filepath.Join(vdir, name+"-"+v+".jar"), jar)
		writeString(t, filepath.Join(vdir, name+"-"+v+".pom.sha1"), sha1Hex([]byte(pom)))
		writeString(t, filepath.Join(vdir, name+"-"+v+".jar.sha1"), sha1Hex([]byte(jar)))
	}
	return "file://" + dir
}

// cooldownWS writes a two-manifest workspace (root + modules/app) with a
// floating requirement on com.rig.test/cooldown wired to the file repo url.
func cooldownWS(t *testing.T, url string) {
	t.Helper()
	writeString(t, "deps.edn",
		"{:rig/modules [\"modules/app\"] :mvn/repos {\"f\" {:url \""+url+"\"}}}\n")
	writeString(t, "modules/app/deps.edn",
		"{:rig/lib example/app :paths [\"src\"]\n"+
			" :deps {com.rig.test/cooldown {:mvn/version \"RELEASE\"}}\n"+
			" :mvn/repos {\"f\" {:url \""+url+"\"}}}\n")
}

// kernelSetup pins the kernel jar + JARSHA and skips when java or the jar
// are unavailable.
func kernelSetup(t *testing.T) {
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
}

func TestCooldownRefused(t *testing.T) {
	kernelSetup(t)
	now := time.Now().UnixMilli()
	url := fileMavenRepo(t, "com.rig.test", "cooldown", map[string]int64{
		"1.0.0": now - 1*time.Hour.Milliseconds(),
		"1.0.1": now - 2*time.Hour.Milliseconds(),
	})
	dir := t.TempDir()
	t.Chdir(dir)
	cooldownWS(t, url)
	code, out := runCLI(t, "update", "com.rig.test/cooldown", "--cache-dir", t.TempDir())
	if code != 5 {
		t.Fatalf("update exit = %d, want 5; out: %s", code, out)
	}
	if !strings.Contains(out, "refused com.rig.test/cooldown (cooldown 48h)") {
		t.Fatalf("out = %q, want refused line", out)
	}
	if statOK("deps.lock") {
		t.Fatal("lock must not be written on refusal")
	}
}

func TestCooldownSelectsOldSkipsFresh(t *testing.T) {
	kernelSetup(t)
	now := time.Now().UnixMilli()
	url := fileMavenRepo(t, "com.rig.test", "cooldown", map[string]int64{
		"1.0.0": now - 50*time.Hour.Milliseconds(), // older than the 48h cooldown
		"1.0.1": now - 1*time.Hour.Milliseconds(),
	})
	dir := t.TempDir()
	t.Chdir(dir)
	cooldownWS(t, url)
	code, out := runCLI(t, "update", "com.rig.test/cooldown", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("update exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, `set com.rig.test/cooldown "1.0.0"`) {
		t.Fatalf("out = %q, want 1.0.0 selected", out)
	}
	if !strings.Contains(out, "skipped com.rig.test/cooldown 1.0.1 (published") {
		t.Fatalf("out = %q, want fresh 1.0.1 skipped by cooldown", out)
	}
	b, err := os.ReadFile("modules/app/deps.edn")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"1.0.0"`) {
		t.Fatalf("manifest = %s, want 1.0.0 pinned", b)
	}
}

func TestCooldownForce(t *testing.T) {
	kernelSetup(t)
	now := time.Now().UnixMilli()
	url := fileMavenRepo(t, "com.rig.test", "cooldown", map[string]int64{
		"1.0.0": now - 50*time.Hour.Milliseconds(),
		"1.0.1": now - 1*time.Hour.Milliseconds(),
	})
	dir := t.TempDir()
	t.Chdir(dir)
	cooldownWS(t, url)
	code, out := runCLI(t, "update", "com.rig.test/cooldown", "--force", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("update --force exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, `set com.rig.test/cooldown "1.0.1"`) {
		t.Fatalf("out = %q, want 1.0.1 selected", out)
	}
	if !strings.Contains(out, "forced com.rig.test/cooldown 1.0.1 (cooldown") {
		t.Fatalf("out = %q, want forced pick recorded", out)
	}
}
