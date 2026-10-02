package cli

import (
	"archive/zip"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/brutasse/rig/internal/lockfile"
)

func TestJavacOptsOf(t *testing.T) {
	pin := func(requested string) *lockfile.JVM {
		return &lockfile.JVM{Vendor: "temurin", Requested: requested}
	}
	tests := []struct {
		name string
		host string
		jvm  *lockfile.JVM
		opts []string
		want []string
	}{
		{"no pin, opts pass through", "", nil, []string{"-encoding", "UTF-8"}, []string{"-encoding", "UTF-8"}},
		{"no pin, no opts", "", nil, nil, nil},
		{"pin, no opts", "21.0.5", pin("17"), nil, []string{"--release", "17"}},
		{"pin, module opts kept after injection", "21.0.5", pin("11"), []string{"-Dfoo=bar", "-encoding", "UTF-8"}, []string{"--release", "11", "-Dfoo=bar", "-encoding", "UTF-8"}},
		{"pin, user --release wins", "21.0.5", pin("11"), []string{"--release", "17"}, []string{"--release", "17"}},
		{"pin, user --release= wins", "21.0.5", pin("11"), []string{"--release=17"}, []string{"--release=17"}},
		{"pin, user -source wins", "21.0.5", pin("11"), []string{"-source", "11", "-target", "11"}, []string{"-source", "11", "-target", "11"}},
		{"pin, user -source= wins", "21.0.5", pin("11"), []string{"-source=11"}, []string{"-source=11"}},
		{"pin, user -target wins", "21.0.5", pin("11"), []string{"-target", "11"}, []string{"-target", "11"}},
		{"java 8 host, no --release flag", "1.8.0_422", pin("8"), nil, nil},
		{"java 8 host, pin above host, no --release flag", "1.8.0_422", pin("11"), nil, nil},
		{"unknown host, pin, --release kept", "", pin("11"), nil, []string{"--release", "11"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := javacOptsOf(tt.host, tt.jvm, tt.opts); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("javacOptsOf = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFloorOf(t *testing.T) {
	pin := func(requested string) *lockfile.JVM {
		return &lockfile.JVM{Vendor: "temurin", Requested: requested}
	}
	tests := []struct {
		name string
		jvm  *lockfile.JVM
		want int
	}{
		{"no pin", nil, 0},
		{"plain feature version", pin("21"), 21},
		{"floor major", pin("8"), 8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := floorOf(tt.jvm); got != tt.want {
				t.Errorf("floorOf = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestControlsSourceLevel(t *testing.T) {
	for _, o := range [][]string{
		{"--release", "17"},
		{"--release=17"},
		{"-g", "-source", "17"},
		{"-source=17"},
		{"-target", "17"},
		{"-target=17"},
	} {
		if !controlsSourceLevel(o) {
			t.Errorf("controlsSourceLevel(%v) = false, want true", o)
		}
	}
	for _, o := range [][]string{
		nil,
		{"-encoding", "UTF-8"},
		{"-Dfoo=bar"},
		{"-Xlint:all"},
	} {
		if controlsSourceLevel(o) {
			t.Errorf("controlsSourceLevel(%v) = true, want false", o)
		}
	}
}

func TestNativeInitPackages(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Namespace init classes at various depths; non-init and root-level
	// entries must be ignored (a root __init has no package to mark).
	write("app/core__init.class")
	write("app/util$fn__1.class")
	write("clojure/core__init.class")
	write("clojure/core/server__init.class")
	write("core__init.class")

	// A classpath jar contributes its namespace packages too.
	jar := filepath.Join(dir, "lib.jar")
	f, err := os.Create(jar)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, name := range []string{"hello_world/main__init.class", "java/util/Other.class"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := nativeInitPackages([]string{dir, jar})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"app", "clojure", "clojure.core", "hello_world"}
	if len(got) != len(want) {
		t.Fatalf("packages = %v, want %v", got, want)
	}
	sort.Strings(got)
	for i, p := range want {
		if got[i] != p {
			t.Errorf("packages[%d] = %q, want %q (full: %v)", i, got[i], p, got)
		}
	}
}

func TestNativeShim(t *testing.T) {
	src := nativeShim("app.core")
	for _, want := range []string{
		"clojure.lang.RT.init();",
		`clojure.lang.RT.load("app/core");`,
		`all[1] = "app.core";`,
		"clojure.main.main(all);",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("shim missing %q in:\n%s", want, src)
		}
	}
	// A dashed namespace loads by its munged class path but runs by name.
	src = nativeShim("my-app.core")
	for _, want := range []string{
		`clojure.lang.RT.load("my_app/core");`,
		`all[1] = "my-app.core";`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("shim missing %q in:\n%s", want, src)
		}
	}
}
