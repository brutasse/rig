package cli

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brutasse/rig/internal/jvm"
	"github.com/brutasse/rig/internal/lockfile"
	"github.com/brutasse/rig/internal/workspace"
)

func TestLaunchFlagsOrder(t *testing.T) {
	// No GC in :jvm-opts: all defaults first, module opts last (JVM
	// last-wins).
	flags := launchFlags([]string{"-Xmx2g"})
	if len(flags) != len(launchDefaults)+1 {
		t.Fatalf("len(flags) = %d, want %d", len(flags), len(launchDefaults)+1)
	}
	if flags[0] != "-XX:+UseG1GC" {
		t.Errorf("first flag = %q, want the rig G1 default first", flags[0])
	}
	if last := flags[len(flags)-1]; last != "-Xmx2g" {
		t.Errorf("last flag = %q, want the module :jvm-opts last (JVM last-wins)", last)
	}
	// A GC in :jvm-opts replaces the G1 default — the JVM refuses two
	// collectors.
	flags = launchFlags([]string{"-XX:+UseZGC"})
	if len(flags) != len(launchDefaults) {
		t.Fatalf("len(flags) = %d, want %d (G1 dropped, ZGC added)", len(flags), len(launchDefaults))
	}
	for _, f := range flags {
		if f == "-XX:+UseG1GC" {
			t.Error("G1 default must be dropped when :jvm-opts selects a collector")
		}
	}
	if last := flags[len(flags)-1]; last != "-XX:+UseZGC" {
		t.Errorf("last flag = %q, want the module :jvm-opts last", last)
	}
}

func writeZip(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchDescriptorRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := launchDescriptor{Version: 1, Rig: "dev", Main: "a.b", JVMOpts: []string{"-Xmx2g"}, Java: 21, Uber: true}
	writeZip(t, filepath.Join(dir, "with.jar"), map[string]string{launchDescriptorPath: `{"version":1,"rig":"dev","main":"a.b","jvm-opts":["-Xmx2g"],"java":21,"uber":true}`})
	writeZip(t, filepath.Join(dir, "without.jar"), map[string]string{"META-INF/MANIFEST.MF": "Manifest-Version: 1.0"})
	writeFile(t, filepath.Join(dir, "notjar.txt"), "text")

	got, err := readLaunchDescriptor(filepath.Join(dir, "with.jar"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Main != want.Main || got.Java != want.Java || !got.Uber || len(got.JVMOpts) != 1 {
		t.Errorf("descriptor = %+v, want %+v", got, want)
	}

	if got, err = readLaunchDescriptor(filepath.Join(dir, "without.jar")); err != nil || got != nil {
		t.Errorf("absent descriptor: got %+v, %v; want nil, nil", got, err)
	}

	_, err = readLaunchDescriptor(filepath.Join(dir, "notjar.txt"))
	if !errors.Is(err, errNotZip) {
		t.Errorf("err = %v, want errNotZip", err)
	}
}

func TestLaunchPlanFallbackToLock(t *testing.T) {
	// A jar without a descriptor (legacy artifact): the lock is the plan.
	dir := t.TempDir()
	lock := lockfile.ForTest(".")
	mod := lock.Modules["."]
	mod.Main = "a.b"
	mod.JVMOpts = []string{"-Xmx1g"}
	mod.Build = lockfile.Build{Jar: true, ClassDir: "target/classes", Uberjar: &lockfile.Uberjar{File: "target/x.jar", Main: "a.b"}}
	lock.Modules["."] = mod
	lock.JVM = &lockfile.JVM{Vendor: "temurin", Requested: "21", Version: "21.0.12.1+9"}

	jar := filepath.Join(dir, "target", "x.jar")
	writeZip(t, jar, map[string]string{"a.class": ""})

	root := &workspace.Root{Dir: dir}
	plan, err := launchPlanFor(&opts{}, lock, root, "", jar)
	if err != nil {
		t.Fatal(err)
	}
	if plan.main != "a.b" || !plan.uber || plan.java != 21 || len(plan.jvmOpts) != 1 {
		t.Errorf("plan = %+v, want main a.b, uber, java 21, jvm-opts from the lock", plan)
	}
}

func TestJavaVersionOK(t *testing.T) {
	// Exact match on the major version: equal passes, a different major
	// fails in both directions, nothing to check always passes.
	cases := []struct {
		got, want int
		ok        bool
	}{
		{21, 21, true},
		{21, 17, false}, // too new
		{17, 21, false}, // too old
		{0, 21, false},  // undetermined
		{21, 0, true},   // no build version to check
		{0, 0, true},
	}
	for _, c := range cases {
		if got := javaVersionOK(c.got, c.want); got != c.ok {
			t.Errorf("javaVersionOK(%d, %d) = %v, want %v", c.got, c.want, got, c.ok)
		}
	}
}

func TestHotLaunchMissingArtifact(t *testing.T) {
	hotSetup(t)
	code, out := runCLI(t, "launch", "--cache-dir", t.TempDir())
	if code != 2 {
		t.Fatalf("exit = %d, want 2; out: %s", code, out)
	}
	if !strings.Contains(out, "no build output") {
		t.Errorf("out = %q", out)
	}
}

func TestLaunchStandaloneMissingJar(t *testing.T) {
	// No workspace: the first positional was meant to be the jar.
	if _, err := jvm.Find(); err != nil {
		t.Skipf("no java available: %v", err)
	}
	dir := t.TempDir()
	t.Chdir(dir)
	code, out := runCLI(t, "launch", "--cache-dir", t.TempDir(), "nope.jar")
	if code != 2 {
		t.Fatalf("exit = %d, want 2; out: %s", code, out)
	}
	if !strings.Contains(out, "no such jar") {
		t.Errorf("out = %q", out)
	}
}

func TestHotLaunchUber(t *testing.T) {
	hotSetup(t)
	cacheDir := t.TempDir()
	if code, out := runCLI(t, "build", "-p", "modules/app", "--cache-dir", cacheDir, "--uber"); code != 0 {
		t.Fatalf("build exit = %d; out: %s", code, out)
	}
	// No jar argument: the lock's build output, main args after the first
	// positional.
	code, out := runCLI(t, "launch", "-p", "modules/app", "--cache-dir", cacheDir, "world")
	if code != 0 {
		t.Fatalf("launch exit = %d; out: %s", code, out)
	}
	if !strings.Contains(out, "hello world") {
		t.Errorf("launch out = %q", out)
	}
}

func TestHotLaunchJVMOverride(t *testing.T) {
	hotSetup(t)
	cacheDir := t.TempDir()
	if code, out := runCLI(t, "build", "-p", "modules/app", "--cache-dir", cacheDir, "--uber"); code != 0 {
		t.Fatalf("build exit = %d; out: %s", code, out)
	}
	// The fixture module's :jvm-opts switch the collector to ZGC and print
	// the effective flags: the override must win over rig's G1 default
	// (the JVM refuses two collectors, so a clean start is itself proof)
	// and the rest of the default set must still be applied.
	code, out := runCLI(t, "launch", "-p", "modules/app", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("launch exit = %d; out: %s", code, out)
	}
	for _, want := range []string{
		"-XX:+UseZGC",
		"-XX:+AlwaysPreTouch",
		"-XX:+ExitOnOutOfMemoryError",
		"-XX:+HeapDumpOnOutOfMemoryError",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("launch out missing %q:\n%s", want, out)
		}
	}
}

func TestHotLaunchJavaVersionMismatch(t *testing.T) {
	hotSetup(t)
	cacheDir := t.TempDir()
	if code, out := runCLI(t, "build", "-p", "modules/app", "--cache-dir", cacheDir, "--uber"); code != 0 {
		t.Fatalf("build exit = %d; out: %s", code, out)
	}
	// A java that reports a major version no build ever used: the exact
	// match is refused before the app JVM starts (99 cannot be a build
	// JVM). The wrapper fakes -version and execs the real java otherwise.
	wrap := filepath.Join(t.TempDir(), "fakejava")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"-version\" ]; then\n" +
		"  echo 'openjdk version \"99.0.1\" 2026-09-25'\n" +
		"  exit 0\n" +
		"fi\n" +
		"exec /usr/bin/java \"$@\"\n"
	if err := os.WriteFile(wrap, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIG_JAVA", wrap)
	code, out := runCLI(t, "launch", "-p", "modules/app", "--cache-dir", cacheDir)
	if code != 2 {
		t.Fatalf("launch exit = %d, want 2; out: %s", code, out)
	}
	if !strings.Contains(out, "exact match required") {
		t.Errorf("out = %q", out)
	}
}

func TestHotLaunchJar(t *testing.T) {
	hotSetup(t)
	cacheDir := t.TempDir()
	// The plain (non-uber) jar: launched with the locked classpath, its own
	// source paths replaced by the built jar.
	if code, out := runCLI(t, "build", "-p", "modules/app", "--cache-dir", cacheDir); code != 0 {
		t.Fatalf("build exit = %d; out: %s", code, out)
	}
	code, out := runCLI(t, "launch", "--cache-dir", cacheDir, "modules/app/target/app-0.1.0.jar", "hello")
	if code != 0 {
		t.Fatalf("launch exit = %d; out: %s", code, out)
	}
	if !strings.Contains(out, "hello hello") {
		t.Errorf("launch out = %q", out)
	}
}

func TestHotLaunchStandalone(t *testing.T) {
	hotSetup(t)
	cacheDir := t.TempDir()
	if code, out := runCLI(t, "build", "-p", "modules/app", "--cache-dir", cacheDir, "--uber"); code != 0 {
		t.Fatalf("build exit = %d; out: %s", code, out)
	}
	uber, err := filepath.Abs(filepath.Join("modules", "app", "target", "app-uber.jar"))
	if err != nil {
		t.Fatal(err)
	}
	// No workspace up the tree: the launch plan comes from the jar's
	// descriptor, the java from the environment.
	out := t.TempDir()
	t.Chdir(out)
	code, o := runCLI(t, "launch", "--cache-dir", cacheDir, uber, "there")
	if code != 0 {
		t.Fatalf("launch exit = %d; out: %s", code, o)
	}
	if !strings.Contains(o, "hello there") {
		t.Errorf("launch out = %q", o)
	}
}
