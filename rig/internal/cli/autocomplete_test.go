package cli

import (
	"strings"
	"testing"

	"github.com/brutasse/rig/internal/lockfile"
)

func TestAutocompleteScripts(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "deps.edn", "{}\n")
	markers := map[string]string{
		"bash":       "# bash completion V2 for rig",
		"zsh":        "#compdef rig",
		"fish":       "complete -c rig",
		"powershell": "Register-ArgumentCompleter -CommandName 'rig'",
	}
	for shell, marker := range markers {
		// Explicit shell, both "--autocomplete=shell" and "--autocomplete shell".
		for _, args := range [][]string{{"--autocomplete=" + shell}, {"--autocomplete", shell}} {
			code, out := runCLI(t, args...)
			if code != 0 {
				t.Errorf("%v: exit = %d, want 0; out: %s", args, code, out)
			}
			if !strings.Contains(out, marker) {
				t.Errorf("%v: marker %q not in:\n%s", args, marker, out)
			}
		}
	}
}

func TestAutocompleteDetect(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"BASH_VERSION": "5.2.1"}, "bash"},
		{map[string]string{"ZSH_VERSION": "5.9"}, "zsh"},
		{map[string]string{"FISH_VERSION": "3.7.1"}, "fish"},
		{map[string]string{"PSModulePath": "/modules"}, "powershell"},
		{map[string]string{"SHELL": "/usr/bin/bash"}, "bash"},
		{map[string]string{"SHELL": "/usr/local/bin/zsh"}, "zsh"},
		{map[string]string{"SHELL": "/opt/bin/fish"}, "fish"},
		{map[string]string{"SHELL": "/usr/bin/pwsh"}, "powershell"},
	}
	for _, tc := range cases {
		for _, v := range []string{"BASH_VERSION", "ZSH_VERSION", "FISH_VERSION", "PSModulePath", "SHELL"} {
			t.Setenv(v, "")
		}
		for k, v := range tc.env {
			t.Setenv(k, v)
		}
		dir := t.TempDir()
		t.Chdir(dir)
		writeFile(t, "deps.edn", "{}\n")
		code, out := runCLI(t, "--autocomplete")
		if code != 0 {
			t.Fatalf("%v: exit = %d, want 0; out: %s", tc.env, code, out)
		}
		marker := map[string]string{
			"bash":       "# bash completion V2 for rig",
			"zsh":        "#compdef rig",
			"fish":       "complete -c rig",
			"powershell": "Register-ArgumentCompleter -CommandName 'rig'",
		}[tc.want]
		if !strings.Contains(out, marker) {
			t.Errorf("%v: expected %s script (marker %q) in:\n%s", tc.env, tc.want, marker, out)
		}
	}
}

func TestAutocompleteUndetectable(t *testing.T) {
	for _, v := range []string{"BASH_VERSION", "ZSH_VERSION", "FISH_VERSION", "PSModulePath", "SHELL"} {
		t.Setenv(v, "")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	code, out := runCLI(t, "--autocomplete")
	if code != 2 {
		t.Errorf("exit = %d, want 2; out: %s", code, out)
	}
	for _, want := range []string{"could not detect", "bash", "zsh", "fish", "powershell"} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q: %q", want, out)
		}
	}
}

func TestAutocompleteUnknownShell(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	for _, args := range [][]string{{"--autocomplete=tcsh"}, {"--autocomplete", "tcsh"}} {
		code, out := runCLI(t, args...)
		if code != 2 {
			t.Errorf("%v: exit = %d, want 2; out: %s", args, code, out)
		}
		if !strings.Contains(out, `unknown shell "tcsh"`) {
			t.Errorf("%v: out = %q", args, out)
		}
	}
}

func TestAutocompleteTrailingArg(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	code, out := runCLI(t, "--autocomplete", "bash", "extra")
	if code != 2 {
		t.Errorf("exit = %d, want 2; out: %s", code, out)
	}
	if !strings.Contains(out, `unknown command "extra"`) {
		t.Errorf("out = %q", out)
	}
}

// completionWorkspace writes a two-module workspace with per-module aliases.
func completionWorkspace(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	doc := lockfile.ForTest(".", "modules/app")
	m := doc.Modules["."]
	m.Aliases = map[string]lockfile.Alias{"dev": {}, "test": {}}
	doc.Modules["."] = m
	m = doc.Modules["modules/app"]
	m.Aliases = map[string]lockfile.Alias{"integration": {}}
	doc.Modules["modules/app"] = m
	writeFile(t, "deps.edn", "{}\n")
	writeFile(t, "modules/deps.edn", "{}\n")
	if err := doc.Save("deps.lock"); err != nil {
		t.Fatal(err)
	}
}

func TestAutocompletePathCompletion(t *testing.T) {
	completionWorkspace(t)
	code, out := runCLI(t, "__complete", "test", "--path", "")
	if code != 0 {
		t.Fatalf("exit = %d; out: %s", code, out)
	}
	for _, want := range []string{".", "modules/app", ":4"} {
		if !strings.Contains(out, want) {
			t.Errorf("completion missing %q in:\n%s", want, out)
		}
	}
}

func TestAutocompleteAliasCompletion(t *testing.T) {
	completionWorkspace(t)
	// Default module (.): its locked aliases.
	code, out := runCLI(t, "__complete", "run", "--alias", "")
	if code != 0 {
		t.Fatalf("exit = %d; out: %s", code, out)
	}
	for _, want := range []string{"dev", "test", ":4"} {
		if !strings.Contains(out, want) {
			t.Errorf("completion missing %q in:\n%s", want, out)
		}
	}
	// Targeted module: its own aliases, not the root's.
	code, out = runCLI(t, "__complete", "run", "-p", "modules/app", "--alias", "")
	if code != 0 {
		t.Fatalf("exit = %d; out: %s", code, out)
	}
	if !strings.Contains(out, "integration") {
		t.Errorf("completion missing integration in:\n%s", out)
	}
	if strings.Contains(out, "dev\t") || strings.Contains(out, "\ndev\n") {
		t.Errorf("root aliases leaked into modules/app completion:\n%s", out)
	}
}
