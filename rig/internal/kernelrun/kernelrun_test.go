package kernelrun

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/brutasse/rig/internal/digest"
	"github.com/brutasse/rig/internal/jdk"
)

// fakeJar writes a minimal jar whose entries make jarCheckpointable pass
// (content is irrelevant: the restore JVM is the fake java).
func fakeJar(t *testing.T, withEntries bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rig-resolver-fake.jar")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z := zip.NewWriter(f)
	w, _ := z.Create("marker")
	w.Write([]byte(path)) // unique per call: distinct jars must hash distinctly
	for _, name := range []string{"rig/resolver/main.class", "meta.inf"} {
		w, _ := z.Create(name)
		w.Write([]byte{0xCA, 0xFE})
	}
	if withEntries {
		for _, name := range []string{"rig/kernel/Bootstrap.class", "rig/kernel/Runner.class"} {
			w, _ := z.Create(name)
			w.Write([]byte{0xCA, 0xFE})
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// env wires RIG_CRAC_DIR / RIG_CRAC_JDK to per-test fixtures and clears the
// process-level probe caches (paths are unique per test, but reset anyway
// for determinism).
func env(t *testing.T, jar string) (cracDir, ws, fake string) {
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
	probeMu.Lock()
	probeResult = map[string][2]string{}
	probeMu.Unlock()
	jarMu.Lock()
	jarProbed = map[string]bool{}
	jarMu.Unlock()
	return cracDir, ws, fake
}

func req(jar, ws, fake string) Req {
	return Req{
		Op:        "check",
		Workspace: ws,
		Body:      []byte(`{"op":"check","workspace":"x"}`),
		Jar:       jar,
		Java:      fake,
	}
}

// seedImage creates the key dir for this jar/JDK combo holding n images
// (img-000..img-(n-1)), each with a core.img.
func seedImage(t *testing.T, cracDir, jar, fake string, n int) string {
	t.Helper()
	buildID, ok := probeJDK(fake)
	if !ok {
		t.Fatal("fake java must probe as CRaC-capable")
	}
	key, err := keyFor(jar, buildID)
	if err != nil {
		t.Fatal(err)
	}
	kd := filepath.Join(cracDir, key)
	for i := range n {
		img := filepath.Join(kd, imgName(i))
		if err := os.MkdirAll(img, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(img, "core.img"), []byte("pages"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return kd
}

func imgName(i int) string { return fmt.Sprintf("img-%03d", i) }

func countImgs(t *testing.T, kd string) int {
	t.Helper()
	return len(images(kd))
}

func TestRestoreHappyPath(t *testing.T) {
	jar := fakeJar(t, true)
	cracDir, ws, fake := env(t, jar)
	kd := seedImage(t, cracDir, jar, fake, 1)

	t.Setenv("FAKE_PWD_OUT", filepath.Join(t.TempDir(), "pwd"))
	out, code, err := Restore(context.Background(), req(jar, ws, fake))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if code != 0 || string(out) != `{"ok":true,"via":"restore"}` {
		t.Fatalf("got code=%d out=%s", code, out)
	}
	if got := countImgs(t, kd); got != 2 {
		t.Fatalf("image not promoted: %d images", got)
	}
	ents, _ := os.ReadDir(kd)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "next-") || strings.HasPrefix(e.Name(), "req-") {
			t.Fatalf("leftover %s after restore", e.Name())
		}
	}
	pwdRec, err := os.ReadFile(os.Getenv("FAKE_PWD_OUT"))
	if err != nil || string(pwdRec) != ws {
		t.Fatalf("restore cwd not anchored to workspace: %q %v", pwdRec, err)
	}
}

func TestRestoreInfraFailuresFallBack(t *testing.T) {
	jar := fakeJar(t, true)
	for _, mode := range []string{"nodump", "crash"} {
		t.Run(mode, func(t *testing.T) {
			cracDir, ws, fake := env(t, jar)
			kd := seedImage(t, cracDir, jar, fake, 1)
			t.Setenv("FAKE_RESTORE", mode)
			if _, _, err := Restore(context.Background(), req(jar, ws, fake)); err == nil {
				t.Fatal("want infrastructure error, got success")
			}
			if got := countImgs(t, kd); got != 1 {
				t.Fatalf("image must not promote on %s: %d images", mode, got)
			}
		})
	}
}

func TestRestoreOpFailureKeepsImage(t *testing.T) {
	jar := fakeJar(t, true)
	cracDir, ws, fake := env(t, jar)
	kd := seedImage(t, cracDir, jar, fake, 1)
	t.Setenv("FAKE_RESTORE", "opfail")

	out, code, err := Restore(context.Background(), req(jar, ws, fake))
	if err != nil {
		t.Fatalf("op failure is not an infra error: %v", err)
	}
	if code != 1 || !bytes.Contains(out, []byte(`"ok":false`)) {
		t.Fatalf("got code=%d out=%s", code, out)
	}
	if got := countImgs(t, kd); got != 1 {
		t.Fatalf("failed op must not refresh: %d images", got)
	}
}

func TestRestoreNoDumpExitZeroSucceeds(t *testing.T) {
	jar := fakeJar(t, true)
	cracDir, ws, fake := env(t, jar)
	kd := seedImage(t, cracDir, jar, fake, 1)
	t.Setenv("FAKE_RESTORE", "plainzero")

	out, code, err := Restore(context.Background(), req(jar, ws, fake))
	if err != nil || code != 0 || string(out) != `{"ok":true,"via":"restore"}` {
		t.Fatalf("got code=%d out=%s err=%v", code, out, err)
	}
	if got := countImgs(t, kd); got != 1 {
		t.Fatalf("no dump means no promote: %d images", got)
	}
}

func TestRestoreUnavailableCases(t *testing.T) {
	good := fakeJar(t, true)
	noEntries := fakeJar(t, false)

	t.Run("no-image", func(t *testing.T) {
		_, ws, fake := env(t, good)
		if _, _, err := Restore(context.Background(), req(good, ws, fake)); err == nil {
			t.Fatal("want unavailable without a seeded image")
		}
	})
	t.Run("chain-capped", func(t *testing.T) {
		cracDir, ws, fake := env(t, good)
		seedImage(t, cracDir, good, fake, chainCap)
		if _, _, err := Restore(context.Background(), req(good, ws, fake)); err == nil {
			t.Fatal("want unavailable at the chain cap")
		}
	})
	t.Run("plain-jdk", func(t *testing.T) {
		cracDir, ws, fake := env(t, good)
		seedImage(t, cracDir, good, fake, 1)
		t.Setenv("FAKE_PLAIN", "1")
		probeMu.Lock()
		probeResult = map[string][2]string{} // the cached capable-probe predates FAKE_PLAIN
		probeMu.Unlock()
		if _, _, err := Restore(context.Background(), req(good, ws, fake)); err == nil {
			t.Fatal("want unavailable for a JVM without CRaC flags")
		}
	})
	t.Run("jar-without-entries", func(t *testing.T) {
		cracDir, ws, fake := env(t, good)
		seedImage(t, cracDir, good, fake, 1)
		if _, _, err := Restore(context.Background(), req(noEntries, ws, fake)); err == nil {
			t.Fatal("want unavailable for a jar without checkpoint entries")
		}
	})
	t.Run("different-jar-rekeys", func(t *testing.T) {
		cracDir, ws, fake := env(t, good)
		seedImage(t, cracDir, good, fake, 1)
		other := fakeJar(t, true)
		if _, _, err := Restore(context.Background(), req(other, ws, fake)); err == nil {
			t.Fatal("a different kernel jar must not reuse this image")
		}
	})
}

func TestEligibilityGates(t *testing.T) {
	jar := fakeJar(t, true)
	_, ws, fake := env(t, jar)
	base := req(jar, ws, fake)

	off := []struct {
		name string
		mut  func(*Req)
	}{
		{"non-whitelisted-op", func(r *Req) { r.Op = "build" }},
		{"jvm-flags", func(r *Req) { r.JVMFlags = []string{"--enable-preview"} }},
		{"no-workspace", func(r *Req) { r.Workspace = "" }},
		{"empty-body", func(r *Req) { r.Body = nil }},
	}
	for _, c := range off {
		t.Run(c.name, func(t *testing.T) {
			r := base
			c.mut(&r)
			if _, _, err := Restore(context.Background(), r); err == nil {
				t.Fatal("expected ineligible")
			}
		})
	}

	t.Run("token-env-eligible", func(t *testing.T) {
		// P2: request config rides the request file (:env map, populated by
		// kernel.Call), so a token-carrying call is checkpoint-safe again —
		// the JVM env no longer matters to the kernel.
		r := base
		r.Env = []string{"RIG_REPO_TOKENS=secret", "JAVA_HOME=/opt/jdk"}
		if !eligible(r) {
			t.Fatal("token env must not block the checkpoint path")
		}
	})
}

func TestWarmSeedsImageChain(t *testing.T) {
	jar := fakeJar(t, true)
	cracDir, ws, fake := env(t, jar)
	kd := filepath.Join(cracDir, mustKey(t, jar, fake))

	if err := Warm(context.Background(), req(jar, ws, fake)); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if got := countImgs(t, kd); got != 1 {
		t.Fatalf("warm did not seed: %d images at %s", got, kd)
	}
	// A second warm extends the chain (each bootstrap is a fresh full image).
	if err := Warm(context.Background(), req(jar, ws, fake)); err != nil {
		t.Fatalf("Warm 2: %v", err)
	}
	if got := countImgs(t, kd); got != 2 {
		t.Fatalf("second warm: %d images", got)
	}
	// The chain records its kernel jar for the fork-free boot sweep.
	if m := readChainMeta(kd); m.Jar != mustDigest(t, jar) || m.JDK == "" {
		t.Errorf("chainMeta = %+v, want the bootstrapped jar", m)
	}
}

func TestWarmFailuresAreSilentNoOps(t *testing.T) {
	jar := fakeJar(t, true)
	cracDir, ws, fake := env(t, jar)
	kd := filepath.Join(cracDir, mustKey(t, jar, fake))

	t.Setenv("FAKE_BOOT", "fail")
	if err := Warm(context.Background(), req(jar, ws, fake)); err == nil {
		t.Fatal("want error")
	}
	if entries, _ := os.ReadDir(kd); len(entries) != 0 {
		t.Fatalf("failed warm left: %v", entries)
	}
}

func TestWarmAtChainCap(t *testing.T) {
	jar := fakeJar(t, true)
	cracDir, ws, fake := env(t, jar)
	seedImage(t, cracDir, jar, fake, chainCap)
	if err := Warm(context.Background(), req(jar, ws, fake)); err == nil {
		t.Fatal("want unavailable at cap")
	}
}

func TestKeyIsolatesByJarAndJDK(t *testing.T) {
	jarA := fakeJar(t, true)
	jarB := fakeJar(t, true)
	sumA, _ := digest.File(jarA)
	sumB, _ := digest.File(jarB)
	if sumA == sumB {
		t.Fatal("fake jars must differ")
	}
	rA, rB := keyForTest(t, jarA), keyForTest(t, jarB)
	if rA == rB {
		t.Fatal("jar bytes must change the key")
	}
}

func keyForTest(t *testing.T, jar string) string {
	t.Helper()
	sum, err := keyFor(jar, "build-x")
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

func TestNoiseFilter(t *testing.T) {
	var sink bytes.Buffer
	nf := newNoiseFilter(&sink)
	nf.Write([]byte("real line 1\n[12.3s][error][crac] Checkpoint ...\n7243.697745: warp: Checkpoint 237051 to img/1\nOp"))
	nf.Write([]byte(" error\nINFO: Starting checkpoint\nOct 07 jdk.internal.crac.LoggerContainer info\n7243.749486: Checkpointing thread wakeup after checkpoint/restore\n"))
	got := sink.String()
	want := "real line 1\nOp error\n"
	if got != want {
		t.Fatalf("filter leaked:\n%q", got)
	}
}

func TestResultLineFiltersBannerAfterJSON(t *testing.T) {
	raw := []byte("{\"ok\":true}\n[1234.5s][info][crac] Checkpoint ...\n")
	if got := string(resultLine(raw)); got != `{"ok":true}` {
		t.Fatalf("got %q", got)
	}
}

func mustKey(t *testing.T, jar, fake string) string {
	t.Helper()
	buildID, ok := probeJDK(fake)
	if !ok {
		t.Fatal("fake must probe capable")
	}
	key, err := keyFor(jar, buildID)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestJdkForSelection(t *testing.T) {
	oldPins := jdk.CRaCPins
	t.Cleanup(func() { jdk.CRaCPins = oldPins })
	jdk.CRaCPins = map[string]jdk.Asset{
		runtime.GOOS + "/" + runtime.GOARCH: {Vendor: jdk.CRaCVendor, Version: "99.0.0+1"},
	}

	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	t.Setenv("RIG_CRAC_JDK", "")
	r := Req{Java: "/cold/java"}

	// No managed install: the cold-path JVM is used.
	if got := jdkFor(r); got != "/cold/java" {
		t.Fatalf("jdkFor without managed install = %s, want /cold/java", got)
	}

	// Managed install present (marker + bin/java under <xdg>/rig/jdks): used.
	home := filepath.Join(xdg, "rig", "jdks", jdk.CRaCVendor+"-99.0.0+1")
	if err := os.MkdirAll(filepath.Join(home, "jdk", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	managed := filepath.Join(home, "jdk", "bin", "java")
	if err := os.WriteFile(managed, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	marker, err := json.Marshal(map[string]string{"vendor": jdk.CRaCVendor, "version": "99.0.0+1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "rig-jdk.json"), marker, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := jdkFor(r); got != managed {
		t.Fatalf("jdkFor with managed install = %s, want %s", got, managed)
	}

	// RIG_CRAC_JDK beats the managed install.
	t.Setenv("RIG_CRAC_JDK", "/explicit/java")
	if got := jdkFor(r); got != "/explicit/java" {
		t.Fatalf("jdkFor with RIG_CRAC_JDK = %s, want /explicit/java", got)
	}
}

func TestClean(t *testing.T) {
	jar := fakeJar(t, true)
	cracDir, _, fake := env(t, jar)
	probeID, ok := probeJDK(fake)
	if !ok {
		t.Fatal("probe of fake java failed")
	}
	key, err := keyFor(jar, probeID)
	if err != nil {
		t.Fatal(err)
	}
	seed := func(dir, name string, content []byte) {
		if err := os.MkdirAll(filepath.Join(cracDir, dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cracDir, dir, name, "core.img"), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	seed(key, "img-000", []byte(strings.Repeat("a", 1<<12)))
	seed("deadbeefdeadbeefdeadbeefdeadbeef", "img-000", []byte("stale chain"))
	seed("deadbeefdeadbeefdeadbeefdeadbeef", "img-001", []byte("stale delta"))
	// transient leftovers inside the KEPT chain: interrupted refresh + request
	if err := os.MkdirAll(filepath.Join(cracDir, key, "next-42"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cracDir, key, "req-99.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(cracDir, "stray.txt")
	if err := os.WriteFile(stray, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	n, freed, err := Clean(jar)
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if n != 3 || freed <= 0 { // stale chain (1 dir) + next-42 + req-99.json
		t.Errorf("Clean = (%d, %d), want 3 items, >0 bytes", n, freed)
	}
	if _, err := os.Stat(filepath.Join(cracDir, key, "img-000", "core.img")); err != nil {
		t.Errorf("live chain pruned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cracDir, key, "next-42")); !os.IsNotExist(err) {
		t.Errorf("next-42 survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cracDir, "deadbeefdeadbeefdeadbeefdeadbeef")); !os.IsNotExist(err) {
		t.Errorf("stale chain survived: %v", err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("stray file touched: %v", err)
	}

	// Kernel rotation re-keys: the once-live chain is now stale.
	if _, _, err := Clean(fakeJar(t, true)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cracDir, key)); !os.IsNotExist(err) {
		t.Errorf("chain for old jar survived: %v", err)
	}

	// No candidate probes (override wins, cannot run): everything goes.
	seed(key, "img-000", []byte("re-seed"))
	t.Setenv("RIG_CRAC_JDK", "/nonexistent/rig-test-java")
	if _, _, err := Clean(jar); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cracDir, key)); !os.IsNotExist(err) {
		t.Errorf("chain survived with no usable JVM: %v", err)
	}
}

func mustDigest(t *testing.T, path string) string {
	t.Helper()
	s, err := digest.File(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSweepDaily(t *testing.T) {
	jar := fakeJar(t, true)
	cracDir, _, _ := env(t, jar) // wires RIG_CRAC_DIR + probe

	seed := func(dir string, meta *chainMeta) string {
		full := filepath.Join(cracDir, dir)
		if err := os.MkdirAll(filepath.Join(full, "img-000"), 0o700); err != nil {
			t.Fatal(err)
		}
		if meta != nil {
			b, _ := json.Marshal(meta)
			if err := os.WriteFile(filepath.Join(full, chainMetaFile), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return full
	}
	old := seed("aaaa", &chainMeta{Jar: strings.Repeat("f", 64), JDK: "old"})
	same := seed("bbbb", &chainMeta{Jar: mustDigest(t, jar), JDK: "other-jvm"})
	nometa := seed("cccc", nil)

	n, freed, err := SweepDaily(jar)
	if err != nil {
		t.Fatalf("SweepDaily: %v", err)
	}
	if n != 1 || freed <= 0 {
		t.Errorf("SweepDaily = (%d, %d), want 1 chain pruned", n, freed)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old-jar chain survived: %v", err)
	}
	// Same jar, different JVM key: boot sweep cannot judge it — kept.
	if _, err := os.Stat(same); err != nil {
		t.Errorf("same-jar other-JVM chain pruned: %v", err)
	}
	if _, err := os.Stat(nometa); err != nil {
		t.Errorf("meta-less chain pruned by boot sweep: %v", err)
	}
	// Marker stamped: not due again.
	if SweepDue() {
		t.Error("SweepDue after SweepDaily = true")
	}

	// Fresh store (no marker): due.
	stale := filepath.Join(cracDir, ".swept")
	if err := os.Chtimes(stale, time.Now().Add(-25*time.Hour), time.Now().Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !SweepDue() {
		t.Error("SweepDue with day-old marker = false")
	}
}

func TestSweepDueNoStore(t *testing.T) {
	t.Setenv("RIG_CRAC_DIR", filepath.Join(t.TempDir(), "absent"))
	if SweepDue() {
		t.Error("SweepDue without a store = true")
	}
}
