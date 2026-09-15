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
