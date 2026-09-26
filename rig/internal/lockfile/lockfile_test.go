package lockfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brutasse/rig/internal/digest"
)

const fixturePath = "../../../fixtures/lock/example.json"

func TestLoadFixture(t *testing.T) {
	d, err := Load(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if d.Version != SupportedVersion {
		t.Errorf("version = %d", d.Version)
	}
	if len(d.Artifacts) != 4 {
		t.Errorf("artifacts = %d, want 4", len(d.Artifacts))
	}
	if len(d.Modules) != 3 {
		t.Errorf("modules = %d, want 3", len(d.Modules))
	}
	orch, err := d.Module("modules/orchestrator")
	if err != nil {
		t.Fatal(err)
	}
	if orch.Main != "com.example.main" {
		t.Errorf("main = %q", orch.Main)
	}
	test := orch.Aliases["test"]
	if test.Exec == nil || test.Exec.Type != "exec-fn" || test.Exec.Fn != "kaocha.runner/exec-fn" {
		t.Errorf("test exec = %+v", test.Exec)
	}
	var local *string
	for _, e := range orch.Classpath {
		if e.Local != nil {
			local = e.Local
		}
	}
	if local == nil || *local != "modules/schemas" {
		t.Errorf("local classpath entry = %v", local)
	}
}

func TestSaveRoundTrip(t *testing.T) {
	d, err := Load(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "deps.lock")
	if err := d.Save(p); err != nil {
		t.Fatal(err)
	}
	d2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if d2.LockedAt != d.LockedAt {
		t.Errorf("locked_at round-trip: %v != %v", d2.LockedAt, d.LockedAt)
	}
	if len(d2.Artifacts) != len(d.Artifacts) {
		t.Errorf("artifacts round-trip: %d != %d", len(d2.Artifacts), len(d.Artifacts))
	}
	a := d2.Artifacts[0]
	if a.Classifier != nil || a.Git != nil {
		t.Errorf("null fields not preserved: %+v", a)
	}
	g := d2.Artifacts[3]
	if g.Git == nil || g.Git.SHA != "0123456789abcdef0123456789abcdef01234567" || g.DepsRoot != "modules/entities" {
		t.Errorf("git artifact round-trip: %+v", g)
	}
}

// TestClasspathEntryJSON pins the classpath-entry JSON contract: a bare string
// is an artifact ref, {"local": m} is a workspace-module ref (including an
// out-of-root "../proto" value), and any other shape (e.g. the kernel's old
// {"path": ...}) is rejected.
func TestClasspathEntryJSON(t *testing.T) {
	var e ClasspathEntry
	if err := json.Unmarshal([]byte(`"org.clojure/clojure:1.11.0:jar"`), &e); err != nil {
		t.Fatal(err)
	}
	if e.Ref == nil || *e.Ref != "org.clojure/clojure:1.11.0:jar" || e.Local != nil {
		t.Fatalf("ref entry = %+v", e)
	}

	var loc ClasspathEntry
	if err := json.Unmarshal([]byte(`{"local":"../proto"}`), &loc); err != nil {
		t.Fatal(err)
	}
	if loc.Local == nil || *loc.Local != "../proto" || loc.Ref != nil {
		t.Fatalf("local entry = %+v", loc)
	}

	var bad ClasspathEntry
	if err := json.Unmarshal([]byte(`{"path":"/abs/src"}`), &bad); err == nil {
		t.Fatal(`expected error for {"path"} classpath entry`)
	}

	b, err := json.Marshal(loc)
	if err != nil {
		t.Fatal(err)
	}
	var back ClasspathEntry
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Local == nil || *back.Local != "../proto" {
		t.Fatalf("round-trip = %+v", back)
	}
}

func TestValidateCorruption(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(d *Document)
	}{
		{"bad version", func(d *Document) { d.Version = 99 }},
		{"no modules", func(d *Document) { d.Modules = map[string]Module{} }},
		{"duplicate artifact", func(d *Document) {
			d.Artifacts = append(d.Artifacts, d.Artifacts[0])
		}},
		{"bad mvn sha", func(d *Document) { d.Artifacts[0].SHA256 = "xyz" }},
		{"git without sha", func(d *Document) { d.Artifacts[3].Git = &GitRef{URL: "u"} }},
		{"bad artifact kind", func(d *Document) { d.Artifacts[0].Kind = "svn" }},
		{"unknown artifact ref", func(d *Document) {
			m := d.Modules["."]
			m.Classpath = []ClasspathEntry{{Ref: str("nope/nope:1:jar")}}
			d.Modules["."] = m
		}},
		{"unknown local ref", func(d *Document) {
			m := d.Modules["."]
			m.Classpath = []ClasspathEntry{{Local: str("modules/ghost")}}
			d.Modules["."] = m
		}},
		{"empty classpath entry", func(d *Document) {
			m := d.Modules["."]
			m.Classpath = []ClasspathEntry{{}}
			d.Modules["."] = m
		}},
		{"bad manifest sha", func(d *Document) {
			m := d.Modules["."]
			m.ManifestSHA256 = "nothex"
			d.Modules["."] = m
		}},
		{"unknown alias artifact", func(d *Document) {
			m := d.Modules["modules/orchestrator"]
			a := m.Aliases["test"]
			a.Classpath = []ClasspathEntry{{Ref: str("ghost")}}
			m.Aliases["test"] = a
			d.Modules["modules/orchestrator"] = m
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := Load(fixturePath)
			if err != nil {
				t.Fatal(err)
			}
			c.mutate(d)
			if err := d.Validate(); err == nil {
				t.Error("expected validation error")
			}
		})
	}
}

func TestJVMFloor(t *testing.T) {
	for requested, wantErr := range map[string]bool{
		"7":   true,
		"8":   false,
		"1.8": false,
		"10":  false,
		"11":  false,
		"21":  false,
	} {
		t.Run(requested, func(t *testing.T) {
			d, err := Load(fixturePath)
			if err != nil {
				t.Fatal(err)
			}
			d.JVM = &JVM{Vendor: "temurin", Requested: requested}
			err = d.Validate()
			if wantErr && (err == nil || !strings.Contains(err.Error(), "kernel floor")) {
				t.Errorf("requested %q: err = %v, want a kernel floor error", requested, err)
			}
			if !wantErr && err != nil {
				t.Errorf("requested %q: err = %v, want none", requested, err)
			}
		})
	}
}

func TestLoadBadJSON(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "deps.lock")
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Error("expected error for bad json")
	}
}

func TestStale(t *testing.T) {
	dir := t.TempDir()
	write := func(p, content string) string {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		h, err := digest.File(full)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	rootSHA := write("deps.edn", "{:a 1}\n")
	orchSHA := write("modules/orchestrator/deps.edn", "{:b 2}\n")

	d := &Document{
		Version: SupportedVersion,
		Workspace: Workspace{
			Modules:        []string{".", "modules/orchestrator"},
			ManifestSHA256: rootSHA,
		},
		Modules: map[string]Module{
			".":                    {ManifestSHA256: rootSHA},
			"modules/orchestrator": {ManifestSHA256: orchSHA},
			"modules/ghost":        {ManifestSHA256: strings.Repeat("0", 64)},
		},
	}

	stale, err := d.Stale(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0] != "modules/ghost" {
		t.Errorf("stale = %v, want [modules/ghost]", stale)
	}

	if err := os.WriteFile(filepath.Join(dir, "deps.edn"), []byte("{:a 999}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale, err = d.Stale(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".", "modules/ghost"}
	if len(stale) != 2 || stale[0] != want[0] || stale[1] != want[1] {
		t.Errorf("stale = %v, want %v", stale, want)
	}
}

func str(s string) *string { return &s }
