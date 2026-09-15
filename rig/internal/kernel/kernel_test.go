package kernel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brutasse/rig/internal/cache"
	"github.com/brutasse/rig/internal/digest"
	"github.com/brutasse/rig/internal/jvm"
)

func TestEnsureFromEnvJar(t *testing.T) {
	dir := t.TempDir()
	jar := filepath.Join(dir, "kernel.jar")
	if err := os.WriteFile(jar, []byte("fake-kernel-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	sha, err := digest.File(jar)
	if err != nil {
		t.Fatal(err)
	}
	pin := Pin{GitSHA: "abc", JARSHA: sha}
	t.Setenv("RIG_KERNEL_JAR", jar)

	store := cache.NewAt(t.TempDir())
	got, err := pin.Ensure(context.Background(), store, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != jar {
		t.Errorf("ensure = %s, want %s", got, jar)
	}
}

func TestEnsureEnvJarHashMismatch(t *testing.T) {
	dir := t.TempDir()
	jar := filepath.Join(dir, "kernel.jar")
	if err := os.WriteFile(jar, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	pin := Pin{GitSHA: "abc", JARSHA: "0000000000000000000000000000000000000000000000000000000000000000"}
	t.Setenv("RIG_KERNEL_JAR", jar)

	store := cache.NewAt(t.TempDir())
	if _, err := pin.Ensure(context.Background(), store, false); err == nil {
		t.Error("expected hash mismatch error")
	}
}

const stubSrc = `
public class Stub {
	public static void main(String[] args) {
		String code = System.getenv("RIG_TEST_EXIT");
		if (code != null) {
			System.exit(Integer.parseInt(code));
		}
		System.out.println("{\"ok\":true}");
	}
}
`

func makeStubJar(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Stub.java"), []byte(stubSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runDir(t, dir, "javac", "Stub.java"); err != nil {
		t.Skipf("javac unavailable: %v", err)
	}
	jar := filepath.Join(dir, "stub.jar")
	if err := runDir(t, dir, "jar", "c", "e", jar, "Stub", "Stub.class"); err != nil {
		t.Skipf("jar unavailable: %v", err)
	}
	return jar
}

func runDir(t *testing.T, dir, name string, args ...string) error {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("%v: %s", err, out)
	}
	return err
}

func TestCallSuccess(t *testing.T) {
	java, err := jvm.Find()
	if err != nil {
		t.Skip(err)
	}
	jar := makeStubJar(t)
	out, err := Call(context.Background(), jar, java, Request{Op: "resolve", Workspace: ".", Args: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "{\"ok\":true}" {
		t.Errorf("stdout = %q", out)
	}
}

func TestCallExitMapping(t *testing.T) {
	java, err := jvm.Find()
	if err != nil {
		t.Skip(err)
	}
	jar := makeStubJar(t)
	cases := []struct {
		code int
		want error
	}{
		{1, ErrResolution},
		{2, ErrBadRequest},
		{3, ErrLegacyKeys},
	}
	for _, c := range cases {
		t.Run(fmt.Sprint(c.code), func(t *testing.T) {
			t.Setenv("RIG_TEST_EXIT", fmt.Sprint(c.code))
			_, err := Call(context.Background(), jar, java, Request{Op: "resolve", Workspace: ".", Args: map[string]any{}})
			var oe *OpError
			if !errors.As(err, &oe) {
				t.Fatalf("err = %v, want *OpError", err)
			}
			if oe.Op != "resolve" {
				t.Errorf("op = %q", oe.Op)
			}
			if !errors.Is(oe.Err, c.want) {
				t.Errorf("mapped = %v, want %v", oe.Err, c.want)
			}
		})
	}
}
