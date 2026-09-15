package cli

import (
	"strings"
	"testing"
)

func TestTree(t *testing.T) {
	hotSetup(t)
	cacheDir := t.TempDir()
	code, out := runCLI(t, "tree", "-p", "modules/app", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("tree exit = %d, want 0; out: %s", code, out)
	}
	// Root is the module's lib coord; clojure is a direct dep, spec.alpha a transitive one.
	for _, want := range []string{
		"example/app:0.1.0",
		"org.clojure/clojure:1.11.0",
		"org.clojure/spec.alpha:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q; out: %s", want, out)
		}
	}
	// Depth 1 gets one indent step, depth 2 two.
	if !strings.Contains(out, "\n  org.clojure/clojure:1.11.0\n") {
		t.Errorf("clojure not at depth 1; out: %s", out)
	}
}

func TestTreeAlias(t *testing.T) {
	hotSetup(t)
	cacheDir := t.TempDir()
	// kaocha lives in the :test alias's extra-deps, not in :deps.
	code, out := runCLI(t, "tree", "-p", "modules/app", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("tree exit = %d, want 0; out: %s", code, out)
	}
	if strings.Contains(out, "lambdaisland/kaocha") {
		t.Errorf("alias dep shown without --alias; out: %s", out)
	}
	code, out = runCLI(t, "tree", "-p", "modules/app", "--alias", "test", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("tree --alias exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "lambdaisland/kaocha") {
		t.Errorf("alias dep missing with --alias test; out: %s", out)
	}
}

func TestTreeOffline(t *testing.T) {
	hotSetup(t)
	cacheDir := t.TempDir()
	// An online run warms the ~/.m2 POM cache; the same tree must then
	// resolve fully offline.
	code, out := runCLI(t, "tree", "-p", "modules/app", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("online tree exit = %d, want 0; out: %s", code, out)
	}
	code, out = runCLI(t, "tree", "-p", "modules/app", "--offline", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("offline tree exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "org.clojure/clojure:1.11.0") {
		t.Errorf("offline tree missing clojure; out: %s", out)
	}
}
