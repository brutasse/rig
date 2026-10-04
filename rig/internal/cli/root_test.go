package cli

import (
	"strings"
	"testing"
)

func TestRepeatPathFlagFails(t *testing.T) {
	// -p/--path is a single module. A repeat used to silently keep the
	// last value, so `rig build -p a -p b` built only b — a CI trap.
	// It is a usage error (exit 2) now, refused before the command runs.
	code, out := runCLI(t, "build", "-p", "app", "-p", "orphan")
	if code != 2 {
		t.Fatalf("repeat -p exit = %d, want 2; out: %s", code, out)
	}
	if !strings.Contains(out, "only once") {
		t.Errorf("out = %q, want the only-once message", out)
	}
	// The long form repeated must fail the same way, and the two forms
	// mix.
	if code, _ := runCLI(t, "build", "--path", "app", "--path", "orphan"); code != 2 {
		t.Errorf("repeat --path exit = %d, want 2", code)
	}
	if code, _ := runCLI(t, "build", "-p", "app", "--path", "orphan"); code != 2 {
		t.Errorf("mixed -p/--path exit = %d, want 2", code)
	}
}
