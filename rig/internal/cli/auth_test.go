package cli

import (
	"strings"
	"testing"
)

// TestAuthGetSingleGate: a single-gate config, no argument — the gate's
// RIG_TOKEN_<GATE> is printed to stdout.
func TestAuthGetSingleGate(t *testing.T) {
	writeAuthYaml(t, "gates:\n  pier:\n    url: https://pier.example\n    well-known: https://idp.example/w\n")
	t.Setenv("RIG_TOKEN_PIER", "tok-env")
	code, out := runCLI(t, "auth", "get")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "tok-env") {
		t.Errorf("out = %q, want the env token on stdout", out)
	}
}

// TestAuthGetByNameAndUrl: the argument is a gate name or a repository URL
// the gate fronts; each resolves to its own gate's token.
func TestAuthGetByNameAndUrl(t *testing.T) {
	writeAuthYaml(t, "gates:\n"+
		"  pier:\n    url: https://pier.example\n    well-known: https://idp.example/w\n"+
		"  corp:\n    url: https://maven.corp.example\n    well-known: https://idp2.example/w\n")
	t.Setenv("RIG_TOKEN_PIER", "tok-pier")
	t.Setenv("RIG_TOKEN_CORP", "tok-corp")

	code, out := runCLI(t, "auth", "get", "pier")
	if code != 0 || !strings.Contains(out, "tok-pier") || strings.Contains(out, "tok-corp") {
		t.Errorf("by name: exit = %d out = %q", code, out)
	}
	code, out = runCLI(t, "auth", "get", "https://maven.corp.example/releases")
	if code != 0 || !strings.Contains(out, "tok-corp") || strings.Contains(out, "tok-pier") {
		t.Errorf("by url: exit = %d out = %q", code, out)
	}
}

// TestAuthGetNoConfig: no gate config, no argument — the error names the
// config path.
func TestAuthGetNoConfig(t *testing.T) {
	writeAuthYaml(t, "")
	code, out := runCLI(t, "auth", "get")
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
	for _, want := range []string{"no gate in", "auth.yaml"} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q: %q", want, out)
		}
	}
}

// TestAuthGetMultipleGatesNoArg: several gates, no argument — the config's
// gate names are named.
func TestAuthGetMultipleGatesNoArg(t *testing.T) {
	writeAuthYaml(t, "gates:\n"+
		"  pier:\n    url: https://pier.example\n    well-known: https://idp.example/w\n"+
		"  corp:\n    url: https://maven.corp.example\n    well-known: https://idp2.example/w\n")
	code, out := runCLI(t, "auth", "get")
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
	for _, want := range []string{"2 gates", "pier", "corp", "gate name or repository URL"} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q: %q", want, out)
		}
	}
}

// TestAuthGetUnknownGate: an argument that is neither a gate name nor a URL
// fronted by one.
func TestAuthGetUnknownGate(t *testing.T) {
	writeAuthYaml(t, "gates:\n  pier:\n    url: https://pier.example\n    well-known: https://idp.example/w\n")
	code, out := runCLI(t, "auth", "get", "nope")
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out: %s", code, out)
	}
	if !strings.Contains(out, `gate "nope" not in`) {
		t.Errorf("out = %q", out)
	}
}
