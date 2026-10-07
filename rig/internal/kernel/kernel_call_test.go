package kernel

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeJar(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rig-resolver-fake.jar")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z := zip.NewWriter(f)
	w, _ := z.Create("marker")
	w.Write([]byte(path))
	for _, name := range []string{"rig/kernel/Bootstrap.class", "rig/kernel/Runner.class"} {
		w, _ := z.Create(name)
		w.Write([]byte{0xCA, 0xFE})
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func cracSetup(t *testing.T) (cracDir, ws, fake string) {
	t.Helper()
	cracDir = filepath.Join(t.TempDir(), "crac")
	ws = t.TempDir()
	fake = filepath.Join(t.TempDir(), "fakejava.sh")
	src, err := os.ReadFile(filepath.Join("testdata", "fakejava.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fake, src, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIG_CRAC_DIR", cracDir)
	t.Setenv("RIG_CRAC_JDK", fake)
	return cracDir, ws, fake
}

// imgTotal counts checkpoint images anywhere under the crac root.
func imgTotal(t *testing.T, cracDir string) int {
	t.Helper()
	levels, _ := filepath.Glob(filepath.Join(cracDir, "*", "img-*"))
	return len(levels)
}

func TestCallColdSeedsThenRestores(t *testing.T) {
	jar := fakeJar(t)
	cracDir, ws, fake := cracSetup(t)
	req := Request{Op: "check", Workspace: ws}

	// Cold path: the fake prints via=cold, then the best-effort bootstrap
	// seeds one image.
	out, err := Call(context.Background(), jar, fake, req)
	if err != nil {
		t.Fatalf("cold Call: %v", err)
	}
	if string(out) != `{"ok":true,"via":"cold"}` {
		t.Fatalf("cold out: %s", out)
	}
	if got := imgTotal(t, cracDir); got != 1 {
		t.Fatalf("cold success must seed a checkpoint: %d images", got)
	}

	// Warm path: restore serves the result and refreshes the chain.
	out, err = Call(context.Background(), jar, fake, req)
	if err != nil {
		t.Fatalf("warm Call: %v", err)
	}
	if string(out) != `{"ok":true,"via":"restore"}` {
		t.Fatalf("warm out: %s", out)
	}
	if got := imgTotal(t, cracDir); got != 2 {
		t.Fatalf("restore must refresh: %d images", got)
	}

	// Non-whitelisted ops and flag-carrying calls stay cold with an image ready.
	for _, r := range []Request{
		{Op: "build", Workspace: ws},
		{Op: "check", Workspace: ws, JVMFlags: []string{"--enable-preview"}},
	} {
		out, err := Call(context.Background(), jar, fake, r)
		if err != nil || string(out) != `{"ok":true,"via":"cold"}` {
			t.Fatalf("op %q flags=%v: code out=%s err=%v", r.Op, r.JVMFlags, out, err)
		}
	}
}

func TestCallRestoreOpFailureMaps(t *testing.T) {
	jar := fakeJar(t)
	_, ws, fake := cracSetup(t)
	req := Request{Op: "check", Workspace: ws}
	if _, err := Call(context.Background(), jar, fake, req); err != nil { // seed
		t.Fatal(err)
	}
	t.Setenv("FAKE_RESTORE", "opfail")

	out, err := Call(context.Background(), jar, fake, req)
	if !errors.Is(err, ErrOpFailed) {
		t.Fatalf("want ErrOpFailed, got %v (out=%s)", err, out)
	}
	var oe *OpError
	if !errors.As(err, &oe) || oe.Op != "check" {
		t.Fatalf("want *OpError for check, got %#v", err)
	}
	if !strings.Contains(string(out), `"ok":false`) {
		t.Fatalf("restore op-failure output lost: %s", out)
	}
}

// TestCallRequestEnvChannel proves kernel.Call mirrors the kernel's
// getenv-read config into the request JSON (the channel restored kernels
// read authoritatively) while leaving unrelated vars out.
func TestCallRequestEnvChannel(t *testing.T) {
	jar := fakeJar(t)
	_, ws, fake := cracSetup(t)
	stdinPath := filepath.Join(t.TempDir(), "body.json")
	t.Setenv("FAKE_STDIN_OUT", stdinPath)

	if _, err := Call(context.Background(), jar, fake,
		Request{Op: "check", Workspace: ws},
		`RIG_REPO_TOKENS={:a "tok"}`, "RIG_PROXY_REPOS=", "JAVA_HOME=/opt/jdk"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Env map[string]string }
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("request body: %v (%s)", err, body)
	}
	if got.Env["RIG_REPO_TOKENS"] != `{:a "tok"}` {
		t.Fatalf("token not mirrored: %v", got.Env)
	}
	if _, ok := got.Env["RIG_PROXY_REPOS"]; ok {
		t.Fatal("empty value must not be copied")
	}
	if _, ok := got.Env["JAVA_HOME"]; ok {
		t.Fatal("only kernel-read keys belong in the request env")
	}
}
