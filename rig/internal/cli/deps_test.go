package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/brutasse/rig/internal/lockfile"
)

// classpathVersion returns the version of coord in module's classpath.
func classpathVersion(t *testing.T, lock *lockfile.Document, module, coord string) string {
	t.Helper()
	m, ok := lock.Modules[module]
	if !ok {
		t.Fatalf("module %q not in lock", module)
	}
	for _, e := range m.Classpath {
		s := e.String()
		if strings.HasPrefix(s, coord+":") {
			parts := strings.Split(s, ":")
			if len(parts) >= 3 {
				return parts[1]
			}
		}
	}
	t.Fatalf("%s not in classpath of %s", coord, module)
	return ""
}

func TestAddExplicit(t *testing.T) {
	hotSetup(t)
	cacheDir := t.TempDir()
	code, out := runCLI(t, "add", "org.clojure/tools.logging", "1.3.1",
		"-p", "modules/app", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("add exit = %d, want 0; out: %s", code, out)
	}
	for _, want := range []string{
		`modules/app/deps.edn: set org.clojure/tools.logging "1.3.1"`,
		`deps.edn: set org.clojure/tools.logging "1.3.1"`,
		"pinned org.clojure/tools.logging 1.3.1 (explicit)",
		"artifacts, 2 modules",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q; out: %s", want, out)
		}
	}
	app, err := os.ReadFile("modules/app/deps.edn")
	if err != nil || !strings.Contains(string(app), "org.clojure/tools.logging") {
		t.Errorf("module manifest not updated: %s", app)
	}
	root, err := os.ReadFile("deps.edn")
	if err != nil || !strings.Contains(string(root), "org.clojure/tools.logging") {
		t.Errorf("root manifest not updated: %s", root)
	}
	lock, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	if v := classpathVersion(t, lock, "modules/app", "org.clojure/tools.logging"); v != "1.3.1" {
		t.Errorf("module classpath version = %s, want 1.3.1", v)
	}
}

func TestAddRemoveRoundTrip(t *testing.T) {
	hotSetup(t)
	cacheDir := t.TempDir()
	before, err := os.ReadFile("modules/app/deps.edn")
	if err != nil {
		t.Fatal(err)
	}
	code, out := runCLI(t, "add", "org.clojure/tools.logging", "1.3.1",
		"-p", "modules/app", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("add exit = %d, want 0; out: %s", code, out)
	}
	code, out = runCLI(t, "remove", "org.clojure/tools.logging",
		"-p", "modules/app", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("remove exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "modules/app/deps.edn: remove org.clojure/tools.logging") {
		t.Errorf("remove out = %q", out)
	}
	after, err := os.ReadFile("modules/app/deps.edn")
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("module manifest not restored byte-for-byte:\n--- before\n%s--- after\n%s", before, after)
	}
	root, _ := os.ReadFile("deps.edn")
	if strings.Contains(string(root), "tools.logging") {
		t.Errorf("coord left in root manifest: %s", root)
	}
}

func TestUpdateExplicit(t *testing.T) {
	hotSetup(t)
	cacheDir := t.TempDir()
	// Shared update from the root: records in :deps + :rig/deps, bumps
	// modules/app (1.11.0 -> 1.12.5), relocks.
	code, out := runCLI(t, "update", "org.clojure/clojure", "1.12.5", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("update exit = %d, want 0; out: %s", code, out)
	}
	for _, want := range []string{
		`deps.edn: set org.clojure/clojure "1.12.5"`,
		`modules/app/deps.edn: set org.clojure/clojure "1.12.5"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q; out: %s", want, out)
		}
	}
	lock, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	if v := classpathVersion(t, lock, ".", "org.clojure/clojure"); v != "1.12.5" {
		t.Errorf("root classpath clojure = %s, want 1.12.5", v)
	}
	if v := classpathVersion(t, lock, "modules/app", "org.clojure/clojure"); v != "1.12.5" {
		t.Errorf("module classpath clojure = %s, want 1.12.5 (bumped)", v)
	}
}

func TestUpdateLatest(t *testing.T) {
	hotSetup(t)
	cacheDir := t.TempDir()
	code, out := runCLI(t, "add", "org.clojure/tools.logging", "1.2.4",
		"-p", "modules/app", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("add exit = %d, want 0; out: %s", code, out)
	}
	// No version: bump to the newest eligible one (all old enough for the 48h cooldown).
	code, out = runCLI(t, "update", "org.clojure/tools.logging", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("update exit = %d, want 0; out: %s", code, out)
	}
	lock, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	v := classpathVersion(t, lock, "modules/app", "org.clojure/tools.logging")
	if v == "1.2.4" {
		t.Errorf("update to latest kept the old version; out: %s", out)
	}
	if v == "latest" || v == "RELEASE" {
		t.Errorf("a floating requirement was written, want concrete; got %s", v)
	}
}

func TestUpdateAlias(t *testing.T) {
	hotSetup(t)
	cacheDir := t.TempDir()
	code, out := runCLI(t, "update", "lambdaisland/kaocha", "1.67.1055",
		"-p", "modules/app", "--alias", "test", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("update exit = %d, want 0; out: %s", code, out)
	}
	for _, want := range []string{
		`modules/app/deps.edn: set lambdaisland/kaocha "1.67.1055"`,
		`deps.edn: set lambdaisland/kaocha "1.67.1055"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q; out: %s", want, out)
		}
	}
	app, err := os.ReadFile("modules/app/deps.edn")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(app), `"1.67.1055"`) {
		t.Errorf("alias entry not updated: %s", app)
	}
}

func TestUpdateSharedOnly(t *testing.T) {
	hotSetup(t)
	cacheDir := t.TempDir()
	before, err := os.ReadFile("modules/app/deps.edn")
	if err != nil {
		t.Fatal(err)
	}
	code, out := runCLI(t, "update", "org.clojure/clojure", "1.12.4",
		"--shared-only", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("update exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, `deps.edn: set org.clojure/clojure "1.12.4"`) {
		t.Errorf("out missing shared set; out: %s", out)
	}
	if strings.Contains(out, "modules/app/deps.edn:") {
		t.Errorf("module manifest edited by shared-only update; out: %s", out)
	}
	after, err := os.ReadFile("modules/app/deps.edn")
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("module manifest changed:\n--- before\n%s--- after\n%s", before, after)
	}
	lock, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	if v := classpathVersion(t, lock, "modules/app", "org.clojure/clojure"); v != "1.11.0" {
		t.Errorf("module classpath clojure = %s, want 1.11.0 (shared-only must not override resolution)", v)
	}
}

func TestUpdateNoArgs(t *testing.T) {
	hotSetup(t)
	cacheDir := t.TempDir()
	// Full re-resolve: no floating coords in the fixture, so pins stay stable.
	code, out := runCLI(t, "update", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("update exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "artifacts, 2 modules") {
		t.Errorf("out = %q", out)
	}
	lock, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	if v := classpathVersion(t, lock, "modules/app", "org.clojure/clojure"); v != "1.11.0" {
		t.Errorf("module classpath clojure = %s, want 1.11.0 (stable)", v)
	}
}

func TestAddLatestRefusedCooldown(t *testing.T) {
	hotSetup(t)
	cacheDir := t.TempDir()
	// A huge workspace cooldown makes every candidate ineligible.
	writeFile(t, "deps.edn", "{:rig/modules [\"modules/app\"]\n :rig/cooldown \"99999d\"}\n")
	code, out := runCLI(t, "add", "org.clojure/tools.logging", "--cache-dir", cacheDir)
	if code != 5 {
		t.Fatalf("exit = %d, want 5 (refused); out: %s", code, out)
	}
	if !strings.Contains(out, "refused org.clojure/tools.logging (cooldown 99999d)") {
		t.Errorf("out = %q", out)
	}
	if !strings.Contains(out, "retry with --force") {
		t.Errorf("out = %q", out)
	}
	root, _ := os.ReadFile("deps.edn")
	if strings.Contains(string(root), "tools.logging") {
		t.Errorf("manifest modified on refusal: %s", root)
	}
	// --force selects anyway and records the decision.
	code, out = runCLI(t, "add", "org.clojure/tools.logging", "--force", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("force exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "forced org.clojure/tools.logging") {
		t.Errorf("out missing forced note; out: %s", out)
	}
}

func TestLockKeepsFloatingPin(t *testing.T) {
	hotSetup(t)
	cacheDir := t.TempDir()
	code, out := runCLI(t, "add", "org.clojure/tools.logging", "1.2.4",
		"-p", "modules/app", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("add exit = %d, want 0; out: %s", code, out)
	}
	// Make the requirement floating; the lock already pins 1.2.4.
	p := "modules/app/deps.edn"
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"1.2.4"`) {
		t.Fatalf("expected a 1.2.4 pin in %s: %s", p, b)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), `"1.2.4"`, `"RELEASE"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	// rig lock keeps the existing pin (respect-existing-pins is true).
	code, out = runCLI(t, "lock", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("lock exit = %d, want 0; out: %s", code, out)
	}
	lock, err := lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	if v := classpathVersion(t, lock, "modules/app", "org.clojure/tools.logging"); v != "1.2.4" {
		t.Errorf("lock re-resolved the floating version: %s, want 1.2.4 (kept)", v)
	}
	// rig update (no args) re-resolves dynamic versions.
	code, out = runCLI(t, "update", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("update exit = %d, want 0; out: %s", code, out)
	}
	lock, err = lockfile.Load("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	if v := classpathVersion(t, lock, "modules/app", "org.clojure/tools.logging"); v == "1.2.4" {
		t.Errorf("update kept the old pin %s, want the re-resolved version", v)
	}
}

func TestAddLatestNotFound(t *testing.T) {
	hotSetup(t)
	cacheDir := t.TempDir()
	code, out := runCLI(t, "add", "org.rig.test/nonexistent-thing", "--cache-dir", cacheDir)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
	root, _ := os.ReadFile("deps.edn")
	if strings.Contains(string(root), "nonexistent") {
		t.Errorf("manifest modified on not-found: %s", root)
	}
}

func TestAddRevertsOnResolutionFailure(t *testing.T) {
	hotSetup(t)
	cacheDir := t.TempDir()
	rootBefore, err := os.ReadFile("deps.edn")
	if err != nil {
		t.Fatal(err)
	}
	appBefore, err := os.ReadFile("modules/app/deps.edn")
	if err != nil {
		t.Fatal(err)
	}
	lockBefore, err := os.ReadFile("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	// A version no repository serves: the kernel edits the manifests, then
	// the re-resolve fails — a failed add must leave no trace.
	code, out := runCLI(t, "add", "org.clojure/tools.logging", "99.99.99",
		"-p", "modules/app", "--cache-dir", cacheDir)
	if code != 1 {
		t.Fatalf("add exit = %d, want 1; out: %s", code, out)
	}
	rootAfter, err := os.ReadFile("deps.edn")
	if err != nil {
		t.Fatal(err)
	}
	appAfter, err := os.ReadFile("modules/app/deps.edn")
	if err != nil {
		t.Fatal(err)
	}
	if string(rootBefore) != string(rootAfter) {
		t.Errorf("root manifest not restored:\n--- before\n%s--- after\n%s", rootBefore, rootAfter)
	}
	if string(appBefore) != string(appAfter) {
		t.Errorf("module manifest not restored:\n--- before\n%s--- after\n%s", appBefore, appAfter)
	}
	lockAfter, err := os.ReadFile("deps.lock")
	if err != nil {
		t.Fatal(err)
	}
	if string(lockBefore) != string(lockAfter) {
		t.Errorf("deps.lock changed by a failed add")
	}
	if !strings.Contains(out, "reverted 2 manifest file(s)") {
		t.Errorf("out missing revert note: %s", out)
	}
}
