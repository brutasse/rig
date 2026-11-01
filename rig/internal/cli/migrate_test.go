package cli

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/brutasse/rig/internal/jvm"
)

// setKernel points RIG_KERNEL_JAR at the local test kernel jar.
func setKernel(t *testing.T) {
	t.Helper()
	jar := kernelJarPath(t)
	if _, err := jvm.Find(); err != nil {
		t.Skipf("no java available: %v", err)
	}
	t.Setenv("RIG_KERNEL_JAR", jar)
}

// legacyFixture is a minimal deps-modules-style manifest: a lib key, a
// version file, shared deps with a managed version and an inherit marker.
const legacyFixture = `{:exoscale.project/lib x/y
 :exoscale.project/version-file "VERSION"
 :exoscale.deps/managed-dependencies {a/b {:mvn/version "1.0.0"}}
 :deps {a/b {:exoscale.deps/inherit :all}}}
`

func TestMigrateDryRun(t *testing.T) {
	setKernel(t)
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", legacyFixture)

	code, out := runCLI(t, "migrate", "--dry-run", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("migrate --dry-run exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "deps.edn: would change") {
		t.Errorf("out = %q", out)
	}
	got, err := os.ReadFile("deps.edn")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != legacyFixture {
		t.Errorf("dry-run rewrote the file:\n%s", got)
	}
}

func TestMigrateWrites(t *testing.T) {
	setKernel(t)
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", legacyFixture)

	code, out := runCLI(t, "migrate", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("migrate exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "deps.edn: migrated") {
		t.Errorf("out = %q", out)
	}
	got, err := os.ReadFile("deps.edn")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "exoscale") {
		t.Errorf("legacy keys remain:\n%s", got)
	}
	if strings.Contains(string(got), ":rig/deps") {
		t.Errorf("managed-dependencies must not be carried as :rig/deps:\n%s", got)
	}
	for _, want := range []string{":rig/lib x/y", `:rig/version-file "VERSION"`, ":deps"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("migrated manifest missing %q:\n%s", want, got)
		}
	}
}

func TestMigrateClean(t *testing.T) {
	setKernel(t)
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", "{:rig/lib x/y}\n")

	code, out := runCLI(t, "migrate", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("migrate exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "nothing to migrate") {
		t.Errorf("out = %q", out)
	}
}

func TestMigrateProblemsBlockWrites(t *testing.T) {
	setKernel(t)
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", "{:exoscale.project/lib x/y\n :exoscale.project/version-fn \"unknown/tool\"}\n")

	code, out := runCLI(t, "migrate", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("migrate exit = %d, want 1; out: %s", code, out)
	}
	if !strings.Contains(out, "problem") {
		t.Errorf("out = %q", out)
	}
	got, err := os.ReadFile("deps.edn")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "unknown/tool") {
		t.Errorf("file was modified despite problems:\n%s", got)
	}
}

// leinFixture is a minimal Leiningen project manifest.
const leinFixture = `(defproject foo/bar "1.0"
 :dependencies [[org.clojure/clojure "1.12.1"]
                [aero "1.1.6"]]
 :repositories {"my-registry" {:url "https://registry.example.com"}})` + "\n"

func TestMigrateLeiningen(t *testing.T) {
	setKernel(t)
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "project.clj", leinFixture)

	code, out := runCLI(t, "migrate", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("migrate exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "deps.edn: migrated") {
		t.Errorf("out = %q", out)
	}
	got, err := os.ReadFile("deps.edn")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{":rig/lib foo/bar", `:rig/version "1.0"`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("migrated manifest missing %q:\n%s", want, got)
		}
	}

	// Re-running is a fixed point: the generated manifest has no legacy
	// keys, so the deps.edn path finds nothing to migrate.
	code, out = runCLI(t, "migrate", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("second migrate exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "nothing to migrate") {
		t.Errorf("out = %q", out)
	}
}

// TestMigrateLeiningenBundledRunner: the runner rig falls back to when a
// project declares none must be the one rig new falls back to. The value is
// two literals in two languages (see migrate/default-kaocha), and this is what
// keeps them equal.
func TestMigrateLeiningenBundledRunner(t *testing.T) {
	setKernel(t)
	t.Chdir(t.TempDir())
	writeFile(t, "project.clj", leinFixture)

	code, out := runCLI(t, "migrate", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("migrate exit = %d, want 0; out: %s", code, out)
	}
	want := fmt.Sprintf(`lambdaisland/kaocha {:mvn/version %q}`, defaultTestRunnerVersion)
	if got := readFile(t, "deps.edn"); !strings.Contains(got, want) {
		t.Errorf("migrated manifest =\n%s\nwant the bundled runner %s", got, want)
	}
}

func TestMigrateLeiningenDryRun(t *testing.T) {
	setKernel(t)
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "project.clj", leinFixture)

	code, out := runCLI(t, "migrate", "--dry-run", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("migrate --dry-run exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "deps.edn: would change") {
		t.Errorf("out = %q", out)
	}
	if _, err := os.ReadFile("deps.edn"); !os.IsNotExist(err) {
		t.Errorf("dry-run wrote deps.edn: %v", err)
	}
}
