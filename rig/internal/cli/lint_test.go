package cli

import (
	"os/exec"
	"strings"
	"testing"
)

func TestLint(t *testing.T) {
	if _, err := exec.LookPath("clj-kondo"); err != nil {
		t.Skip("clj-kondo not on PATH")
	}
	hotSetup(t)
	cleanFixture(t)
	// A call to an undefined symbol: clj-kondo must lint it, not print
	// its usage. (Pre-2025 clj-kondo took the path positionally; ≥ 2025.x
	// lints only via --lint, and rig lint was a green no-op.)
	writeFile(t, "modules/app/src/app/broken.clj",
		"(ns app.broken)\n(defn f [] (no.such/ns fn-x))\n")
	code, out := runCLI(t, "lint", "-p", "modules/app")
	if code == 0 {
		t.Fatalf("lint exit = 0, want an error; out: %s", out)
	}
	if !strings.Contains(out, "app/broken.clj") || !strings.Contains(out, "error") {
		t.Errorf("lint out = %q, want the error on app/broken.clj", out)
	}
}
