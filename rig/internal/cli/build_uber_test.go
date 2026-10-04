package cli

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestIgnoredUberOptsKeys(t *testing.T) {
	got := ignoredUberOptsKeys(map[string]any{"exclude": []any{"x.*"}, "main-opts": []any{}, "bogus": 1})
	want := []string{"bogus", "main-opts"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildWarnsUnknownUberOptsKey(t *testing.T) {
	hotSetup(t)
	p := "modules/app/deps.edn"
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(b), ":rig/uberjar? true",
		":rig/uberjar? true\n :rig/uber-opts {:exclude [\"app/.*\"] :bogus-key 1}", 1)
	if text == string(b) {
		t.Fatal("fixture manifest no longer matches")
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := runCLI(t, "build", "-p", "modules/app", "--cache-dir", t.TempDir(), "--uber")
	if code != 0 {
		t.Fatalf("build exit = %d; out: %s", code, out)
	}
	if !strings.Contains(out, ":bogus-key ignored") {
		t.Errorf("no unknown-key warning; out: %s", out)
	}
	if strings.Contains(out, "matched no entry") {
		t.Errorf("matching :exclude pattern warned; out: %s", out)
	}
}
