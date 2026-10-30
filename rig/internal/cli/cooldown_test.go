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

	"github.com/brutasse/rig/internal/jvm"
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

// kernelSetup points RIG_KERNEL_JAR at the local test kernel jar and skips
// when java or the jar are unavailable.
func kernelSetup(t *testing.T) {
	t.Helper()
	jar := kernelJarPath(t)
	if _, err := jvm.Find(); err != nil {
		t.Skipf("no java available: %v", err)
	}
	os.Setenv("RIG_KERNEL_JAR", jar)
	t.Cleanup(func() {
		os.Unsetenv("RIG_KERNEL_JAR")
	})
}

// TestCooldown is the cooldown gate end to end on a file:// maven repo: a
// version published inside the window is skipped in favour of the older one,
// --force takes the fresh one and records it, and a repo whose every version
// is inside the window is refused without writing anything.
func TestCooldown(t *testing.T) {
	kernelSetup(t)
	now := time.Now().UnixMilli()
	hour := time.Hour.Milliseconds()
	const coord = "com.rig.test/cooldown"

	cases := []struct {
		name       string
		published  map[string]int64
		args       []string
		wantCode   int
		wantOut    []string
		wantPin    string // version expected in the module manifest, "" when nothing is written
		wantNoLock bool
	}{
		{
			name:       "refused while every version is inside the window",
			published:  map[string]int64{"1.0.0": now - hour, "1.0.1": now - 2*hour},
			wantCode:   5,
			wantOut:    []string{"refused com.rig.test/cooldown (cooldown 48h)"},
			wantNoLock: true,
		},
		{
			name:      "selects the version older than the window",
			published: map[string]int64{"1.0.0": now - 50*hour, "1.0.1": now - hour},
			wantCode:  0,
			wantOut: []string{`set com.rig.test/cooldown "1.0.0"`,
				"skipped com.rig.test/cooldown 1.0.1 (published"},
			wantPin: `"1.0.0"`,
		},
		{
			name:      "force selects the fresh version",
			published: map[string]int64{"1.0.0": now - 50*hour, "1.0.1": now - hour},
			args:      []string{"--force"},
			wantCode:  0,
			wantOut: []string{`set com.rig.test/cooldown "1.0.1"`,
				"forced com.rig.test/cooldown 1.0.1 (cooldown"},
			wantPin: `"1.0.1"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url := fileMavenRepo(t, "com.rig.test", "cooldown", tc.published)
			dir := t.TempDir()
			t.Chdir(dir)
			cooldownWS(t, url)
			args := append([]string{"update", coord}, tc.args...)
			args = append(args, "--cache-dir", t.TempDir())
			code, out := runCLI(t, args...)
			if code != tc.wantCode {
				t.Fatalf("update exit = %d, want %d; out: %s", code, tc.wantCode, out)
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(out, want) {
					t.Errorf("out missing %q; out: %s", want, out)
				}
			}
			if tc.wantNoLock && statOK("deps.lock") {
				t.Error("lock must not be written on refusal")
			}
			if tc.wantPin == "" {
				return
			}
			b, err := os.ReadFile("modules/app/deps.edn")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(b), tc.wantPin) {
				t.Errorf("manifest = %s, want %s pinned", b, tc.wantPin)
			}
		})
	}
}
