package cli

import (
	"strings"
	"testing"
)

func TestOutdated(t *testing.T) {
	hotSetup(t)
	cacheDir := t.TempDir()
	// Pin an old version; the newest tools.logging is newer, so it must be reported.
	code, out := runCLI(t, "add", "org.clojure/tools.logging", "1.2.4",
		"-p", "modules/app", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("add exit = %d, want 0; out: %s", code, out)
	}
	code, out = runCLI(t, "outdated", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("outdated exit = %d, want 0; out: %s", code, out)
	}
	if strings.Contains(out, "up to date") {
		t.Errorf("outdated reported up to date; out: %s", out)
	}
	if !strings.Contains(out, "org.clojure/tools.logging 1.2.4 ->") {
		t.Errorf("out missing the tools.logging update; out: %s", out)
	}
	// The --breaking filter must still run (it filters the same report).
	code, out = runCLI(t, "outdated", "--breaking", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("outdated --breaking exit = %d, want 0; out: %s", code, out)
	}
}

func TestOutdatedOffline(t *testing.T) {
	hotSetup(t)
	// Pins exist and outdated always needs fresh metadata: refuse.
	code, out := runCLI(t, "outdated", "--offline", "--cache-dir", t.TempDir())
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
	if !strings.Contains(out, "requires the network") {
		t.Errorf("out = %q", out)
	}
}
