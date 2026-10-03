package cli

import (
	"os"
	"strings"
	"testing"
)

// mutateFile rewrites path applying fn to its contents.
func mutateFile(t *testing.T, path string, fn func(string) string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fn(string(b))), 0o644); err != nil {
		t.Fatal(err)
	}
}

// cleanFixture makes the copied hot fixture consistent: the module's clojure
// requirement is raised to the version the lock pins, so stage 1 reports no
// stale-lock. (The lock's recorded manifest sha is irrelevant to check.)
func cleanFixture(t *testing.T) {
	t.Helper()
	mutateFile(t, "modules/app/deps.edn",
		func(s string) string { return strings.Replace(s, "1.11.0", "1.12.5", 1) })
}

func TestCheckClean(t *testing.T) {
	hotSetup(t)
	cleanFixture(t)
	code, out := runCLI(t, "check", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("check exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "check: ok") {
		t.Errorf("check out = %q", out)
	}
}

func TestCheckStaleFrozen(t *testing.T) {
	hotSetup(t)
	// The fixture is stale (module requires 1.11.0, lock pins 1.12.5).
	// --frozen must NOT trigger the stale policy (no exit 3); check reports.
	code, out := runCLI(t, "check", "--frozen", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("check --frozen exit = %d, want 1; out: %s", code, out)
	}
	if !strings.Contains(out, "stale-lock") {
		t.Errorf("check out missing stale-lock: %q", out)
	}
	if !strings.Contains(out, `manifest requires "1.11.0"; lock pins "1.12.5"`) {
		t.Errorf("check out missing the stale-lock message: %q", out)
	}
}

func TestCheckDrift(t *testing.T) {
	hotSetup(t)
	// The workspace manages clojure at the locked version; the module requires
	// a different one -> a drift warning (plus the stale-lock error).
	mutateFile(t, "deps.edn", func(s string) string {
		return strings.Replace(s, `["modules/app"]}`,
			`["modules/app"] :rig/deps {org.clojure/clojure {:mvn/version "1.12.5"}}}`, 1)
	})
	code, out := runCLI(t, "check", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("check exit = %d, want 1; out: %s", code, out)
	}
	if !strings.Contains(out, "drift") {
		t.Errorf("check out missing drift: %q", out)
	}
	if !strings.Contains(out, `module requires "1.11.0", workspace requires "1.12.5"`) {
		t.Errorf("check out missing the drift message: %q", out)
	}
}

func TestCheckNoLock(t *testing.T) {
	hotSetup(t)
	if err := os.Remove("deps.lock"); err != nil {
		t.Fatal(err)
	}
	code, out := runCLI(t, "check", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("check exit = %d, want 1; out: %s", code, out)
	}
	if !strings.Contains(out, "run rig lock") {
		t.Errorf("check out missing the no-lock hint: %q", out)
	}
}

func TestCheckLoadFail(t *testing.T) {
	hotSetup(t)
	cleanFixture(t)
	// A namespace that cannot load: stage 2 must flag it.
	writeFile(t, "modules/app/src/app/broken.clj",
		"(ns app.broken\n  (:require [nonexistent.thing :as t]))\n")
	code, out := runCLI(t, "check", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("check exit = %d, want 1; out: %s", code, out)
	}
	if !strings.Contains(out, "load-fail") {
		t.Errorf("check out missing load-fail: %q", out)
	}
	if !strings.Contains(out, "failed to load") {
		t.Errorf("check out missing the load error: %q", out)
	}
}

func TestCheckDataSourcesSkipped(t *testing.T) {
	hotSetup(t)
	cleanFixture(t)
	// tools.deps puts .clj data files on the classpath as resources: a
	// config map with no ns form, and a deps-new template whose ns does
	// not parse. The plan must skip both with a warning, not die: check
	// used to hard-fail on both.
	writeFile(t, "modules/app/src/migratus.clj", "{:store :in-memory}\n")
	writeFile(t, "modules/app/src/tmpl/core.clj",
		"(ns {{final-name}}.core\n  (:require [clojure.string]))\n")
	code, out := runCLI(t, "check", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("check exit = %d, want 0 (a skip is a warning); out: %s", code, out)
	}
	if !strings.Contains(out, "plan-skip") || !strings.Contains(out, "warning") {
		t.Errorf("check out = %q, want a plan-skip warning", out)
	}
	if !strings.Contains(out, "no ns declaration") || !strings.Contains(out, "unreadable") {
		t.Errorf("check out = %q, want both skip reasons", out)
	}
}

func TestCheckKernelLeak(t *testing.T) {
	hotSetup(t)
	cleanFixture(t)
	// A namespace that exists only inside the kernel jar (tools.deps is the
	// kernel's own dependency, not the module's): the kernel jar must not be
	// on the load classpath, so the preload of the leaking namespace must
	// fail.
	writeFile(t, "modules/app/src/app/leak.clj",
		"(ns app.leak\n  (:require [clojure.tools.deps.util.dir :as d]))\n")
	code, out := runCLI(t, "check", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("check exit = %d, want 1; out: %s", code, out)
	}
	if !strings.Contains(out, "load-fail") {
		t.Errorf("check out missing load-fail: %q", out)
	}
	if !strings.Contains(out, "failed to load clojure.tools.deps.util.dir") {
		t.Errorf("check out missing the load error: %q", out)
	}
}

func TestCheckDataReadersSkipped(t *testing.T) {
	hotSetup(t)
	cleanFixture(t)
	// A data-readables file (a top-level map, not an ns form) at the source
	// root is a clojure.main classpath convention, not a namespace: stage 2
	// must not load it (requiring it fails to compile). The module's real
	// namespace still loads, so the check stays green.
	writeFile(t, "modules/app/src/data_readers.clj",
		"{foo/bar clojure.core/identity}\n")
	code, out := runCLI(t, "check", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("check exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "check: ok") {
		t.Errorf("check out = %q", out)
	}
}

func TestCheckCljcLoadFail(t *testing.T) {
	hotSetup(t)
	cleanFixture(t)
	// A .cljc source must be loaded by stage 2 too: a .cljc namespace that
	// cannot load must be flagged (a .clj-only walk would report vacuous ok).
	writeFile(t, "modules/app/src/app/cljconly.cljc",
		"(ns app.cljconly\n  (:require [nonexistent.cljc.thing :as t]))\n")
	code, out := runCLI(t, "check", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("check exit = %d, want 1; out: %s", code, out)
	}
	if !strings.Contains(out, "load-fail") {
		t.Errorf("check out missing load-fail: %q", out)
	}
	if !strings.Contains(out, "failed to load") {
		t.Errorf("check out missing the load error: %q", out)
	}
}

func TestCheckCljcLoads(t *testing.T) {
	hotSetup(t)
	cleanFixture(t)
	// A loadable .cljc namespace must not break the check; its namespace
	// name must be derived without the .cljc extension.
	writeFile(t, "modules/app/src/app/shared.cljc",
		"(ns app.shared\n  (:require [app.core :as c]))\n")
	code, out := runCLI(t, "check", "--cache-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("check exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "check: ok") {
		t.Errorf("check out = %q", out)
	}
}
