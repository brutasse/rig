package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/brutasse/rig/internal/digest"
	"github.com/brutasse/rig/internal/jvm"
	"github.com/brutasse/rig/internal/kernel"
	"github.com/brutasse/rig/internal/lockfile"
	"github.com/brutasse/rig/internal/workspace"
)

func prepEnv(t *testing.T, dir string, doc *lockfile.Document) *hotEnv {
	t.Helper()
	return &hotEnv{root: &workspace.Root{Dir: dir}, lock: doc}
}

func TestPrepOrder(t *testing.T) {
	// app depends on lib1 and lib2; lib2 depends on lib1.
	doc := lockfile.ForTest(".", "lib1", "lib2", "app")
	local := func(m string) *string { return &m }
	app := doc.Modules["app"]
	app.Classpath = []lockfile.ClasspathEntry{{Local: local("lib2")}, {Local: local("lib1")}}
	doc.Modules["app"] = app
	lib2 := doc.Modules["lib2"]
	lib2.Classpath = []lockfile.ClasspathEntry{{Local: local("lib1")}}
	doc.Modules["lib2"] = lib2
	e := prepEnv(t, t.TempDir(), doc)

	got, err := prepOrder(e, []string{"app", "lib2", "lib1"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"lib1", "lib2", "app"}; !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestPrepOrderCycle(t *testing.T) {
	doc := lockfile.ForTest("a", "b")
	a, b := "a", "b"
	ma := doc.Modules["a"]
	ma.Classpath = []lockfile.ClasspathEntry{{Local: &b}}
	doc.Modules["a"] = ma
	mb := doc.Modules["b"]
	mb.Classpath = []lockfile.ClasspathEntry{{Local: &a}}
	doc.Modules["b"] = mb
	e := prepEnv(t, t.TempDir(), doc)

	if _, err := prepOrder(e, []string{"a", "b"}); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("err = %v, want cycle", err)
	}
}

func TestPrepSet(t *testing.T) {
	doc := lockfile.ForTest(".", "dep", "plain")
	local := func(m string) *string { return &m }
	m := doc.Modules["."]
	m.PrepEnsure, m.PrepAlias, m.PrepFn = []string{"target/classes"}, "prep", "build/prep"
	m.Classpath = []lockfile.ClasspathEntry{{Local: local("dep")}, {Local: local("plain")}}
	doc.Modules["."] = m
	dep := doc.Modules["dep"]
	dep.PrepEnsure, dep.PrepAlias, dep.PrepFn = []string{"target/classes"}, "prep", "build/prep"
	doc.Modules["dep"] = dep
	e := prepEnv(t, t.TempDir(), doc)

	set, err := prepSet(e, ".", m)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{".", "dep"}; !reflect.DeepEqual(set, want) {
		t.Errorf("set = %v, want %v", set, want)
	}
}

func TestPrepSetLegacyLock(t *testing.T) {
	doc := lockfile.ForTest(".", "dep")
	local := func(m string) *string { return &m }
	dep := doc.Modules["dep"]
	dep.PrepEnsure = []string{"target/classes"} // no prep-alias / prep-fn
	doc.Modules["dep"] = dep
	root := doc.Modules["."]
	root.Classpath = []lockfile.ClasspathEntry{{Local: local("dep")}}
	doc.Modules["."] = root
	e := prepEnv(t, t.TempDir(), doc)

	_, err := prepSet(e, ".", root)
	if err == nil || !strings.Contains(err.Error(), "predates prep support") {
		t.Errorf("err = %v, want predates prep support", err)
	}
}

// TestPrepState walks the staleness matrix on a module whose prep output
// exists and is stamped.
func TestPrepState(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "src", "a.clj"), "(ns a)\n")
	writeFile(t, filepath.Join(dir, "target", "classes", "x.class"), "x")
	writeFile(t, filepath.Join(dir, "build", "build.clj"), "(ns build)\n")

	doc := lockfile.ForTest(".")
	m := doc.Modules["."]
	m.PrepEnsure, m.PrepAlias, m.PrepFn = []string{"target/classes"}, "prep", "build/prep"
	m.Build.SrcDirs = []string{"src"}
	m.Aliases = map[string]lockfile.Alias{"prep": {Paths: []string{"build"}}}
	e := prepEnv(t, dir, doc)

	// no stamp -> stale
	st, err := e.prepState(".", m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !st.stale || st.reason != "no stamp" {
		t.Fatalf("state = %+v, want stale 'no stamp'", st)
	}

	// fresh stamp -> up to date
	if err := e.writeStamp(".", m, st); err != nil {
		t.Fatal(err)
	}
	st, err = e.prepState(".", m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.stale {
		t.Fatalf("fresh module reported stale: %s", st.reason)
	}

	// source changed -> stale
	writeFile(t, filepath.Join(dir, "src", "a.clj"), "(ns a)\n;; changed\n")
	st, _ = e.prepState(".", m, nil)
	if !st.stale || st.reason != "sources changed" {
		t.Errorf("state = %+v, want stale 'sources changed'", st)
	}

	// prep build file changed -> stale (alias extra-paths count)
	writeFile(t, filepath.Join(dir, "src", "a.clj"), "(ns a)\n")
	writeFile(t, filepath.Join(dir, "build", "build.clj"), "(ns build)\n(defn prep [])\n")
	st, _ = e.prepState(".", m, nil)
	if !st.stale || st.reason != "sources changed" {
		t.Errorf("state = %+v, want stale 'sources changed'", st)
	}

	// manifest changed -> stale
	writeFile(t, filepath.Join(dir, "build", "build.clj"), "(ns build)\n")
	m2 := m
	m2.ManifestSHA256 = strings.Repeat("0", 64)
	st, _ = e.prepState(".", m2, nil)
	if !st.stale || st.reason != "manifest changed" {
		t.Errorf("state = %+v, want stale 'manifest changed'", st)
	}

	// ensure output missing -> stale (checked before the stamp)
	m3 := m
	_ = os.RemoveAll(filepath.Join(dir, "target"))
	st, _ = e.prepState(".", m3, nil)
	if !st.stale || !strings.HasPrefix(st.reason, "missing") {
		t.Errorf("state = %+v, want stale 'missing ...'", st)
	}

	// a re-prepped local dependency invalidates the stamp (re-stamp first:
	// the missing-output scenario wiped target/ and the stamp with it)
	_ = os.MkdirAll(filepath.Join(dir, "target", "classes"), 0o755)
	writeFile(t, filepath.Join(dir, "target", "classes", "x.class"), "x")
	stFresh, _ := e.prepState(".", m, nil)
	if err := e.writeStamp(".", m, stFresh); err != nil {
		t.Fatal(err)
	}
	ref := doc.Artifacts[0].ID
	dep := "dep"
	m4 := m
	m4.Classpath = []lockfile.ClasspathEntry{{Ref: &ref}, {Local: &dep}}
	st, _ = e.prepState(".", m4, map[string]bool{"dep": true})
	if !st.stale || st.reason != "dependency dep re-prepped" {
		t.Errorf("state = %+v, want stale 'dependency dep re-prepped'", st)
	}
}

func TestDigestDirs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "src", "a.clj"), "one\n")
	d1, err := digest.Dirs(filepath.Join(dir, "src"), filepath.Join(dir, "missing"))
	if err != nil {
		t.Fatal(err)
	}
	// same content -> same digest (missing roots skipped)
	d2, _ := digest.Dirs(filepath.Join(dir, "src"), filepath.Join(dir, "missing"))
	if d1 != d2 {
		t.Errorf("digest not stable: %s != %s", d1, d2)
	}
	// content change -> different
	writeFile(t, filepath.Join(dir, "src", "a.clj"), "two\n")
	d3, _ := digest.Dirs(filepath.Join(dir, "src"))
	if d3 == d1 {
		t.Error("digest did not change with content")
	}
	// new file -> different
	writeFile(t, filepath.Join(dir, "src", "b.clj"), "x\n")
	d4, _ := digest.Dirs(filepath.Join(dir, "src"))
	if d4 == d3 {
		t.Error("digest did not change with a new file")
	}
}

// TestBuildPrepEndToEnd exercises the full flow: lock, build (prep runs in
// dependency order), rebuild (no re-prep), source touch (transitive
// re-prep), run and test (up to date), failing prep fn.
func TestBuildPrepEndToEnd(t *testing.T) {
	jar := kernelJarPath(t)
	if _, err := jvm.Find(); err != nil {
		t.Skipf("no java available: %v", err)
	}
	sha, err := digest.File(jar)
	if err != nil {
		t.Fatal(err)
	}
	oldSHA := kernel.Current.JARSHA
	kernel.Current.JARSHA = sha
	os.Setenv("RIG_KERNEL_JAR", jar)
	t.Cleanup(func() {
		kernel.Current.JARSHA = oldSHA
		os.Unsetenv("RIG_KERNEL_JAR")
	})

	dir := t.TempDir()
	t.Chdir(dir)
	cacheDir := t.TempDir()
	writeFile(t, "VERSION", "0.0.1\n")
	writeFile(t, "deps.edn",
		`{:rig/lib "example/root"
	    :rig/main "root"
	    :rig/modules ["a" "b"]
	    :deps {org.clojure/clojure {:mvn/version "1.12.5"}
	            example/a {:local/root "a"}
	            example/b {:local/root "b"}}
	    :aliases {:test {:extra-paths ["test"]
	                   :exec-fn test-exec/exec}}}`+"\n")
	writeFile(t, "src/root.clj", "(ns root)\n(defn hello [] \"hello\")\n(defn -main [] (println \"root-main-ran\"))\n")
	writeFile(t, "src/test_exec.clj", "(ns test-exec\n  (:require [clojure.test :as t]))\n\n(defn exec [opts]\n  (let [ns (symbol (or (:ns opts) \"root-test\"))]\n    (require ns)\n    (let [summary (t/run-tests ns)]\n      (and (zero? (:fail summary 0)) (zero? (:error summary 0))))))\n")
	writeFile(t, "test/root_test.clj", "(ns root-test\n  (:require [clojure.test :refer :all] [root :as r]))\n(deftest main-works (is (= (r/hello) \"hello\")))\n")
	writeFile(t, "a/deps.edn",
		"{:rig/lib \"example/a\"\n :deps {org.clojure/clojure {:mvn/version \"1.12.5\"}}\n :deps/prep-lib {:ensure \"target/classes\" :alias :prep :fn prep}\n :aliases {:prep {:extra-paths [\"build\"] :ns-default build}}}\n")
	writeFile(t, "a/build/build.clj",
		"(ns build)\n\n(defn prep [& _]\n  (clojure.java.io/make-parents (clojure.java.io/file \"target\" \"classes\" \"marker.txt\"))\n  (spit \"target/classes/marker.txt\" \"a\")\n  (spit \"../prep-log\" \"a\\n\" :append true))\n")
	writeFile(t, "a/src/a.clj", "(ns a)\n")
	writeFile(t, "b/deps.edn",
		"{:rig/lib \"example/b\"\n :deps {org.clojure/clojure {:mvn/version \"1.12.5\"}\n        example/a {:local/root \"../a\"}}\n :deps/prep-lib {:ensure \"target/classes\" :alias :prep :fn prep}\n :aliases {:prep {:extra-paths [\"build\"] :ns-default build}}}\n")
	writeFile(t, "b/build/build.clj",
		"(ns build)\n\n(defn prep [& _]\n  (clojure.java.io/make-parents (clojure.java.io/file \"target\" \"classes\" \"marker.txt\"))\n  (spit \"target/classes/marker.txt\" \"b\")\n  (spit \"../prep-log\" \"b\\n\" :append true))\n")
	writeFile(t, "b/src/b.clj", "(ns b)\n")

	code, out := runCLI(t, "lock", "--cache-dir", cacheDir)
	if code != 0 && strings.Contains(out, "status 429") {
		time.Sleep(10 * time.Second)
		code, out = runCLI(t, "lock", "--cache-dir", cacheDir)
	}
	if code != 0 {
		t.Fatalf("lock exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "wrote") || !statOK("deps.lock") {
		t.Fatalf("lock did not write deps.lock; out: %s", out)
	}

	// 1. build preps a and b, in dependency order, then builds the jar.
	code, out = runCLI(t, "build", "-p", ".", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("build exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "prep a:") || !strings.Contains(out, "prep b:") {
		t.Fatalf("build out missing prep lines: %s", out)
	}
	ai := strings.Index(out, "prep a:")
	bi := strings.Index(out, "prep b:")
	if ai > bi {
		t.Errorf("prep order: a after b in %q", out)
	}
	log, err := os.ReadFile("prep-log")
	if err != nil || string(log) != "a\nb\n" {
		t.Fatalf("prep-log = %q (err %v), want \"a\\nb\\n\"", log, err)
	}
	if _, err := os.Stat("a/target/classes/marker.txt"); err != nil {
		t.Fatalf("a prep output missing: %v", err)
	}
	if _, err := os.Stat("b/target/classes/marker.txt"); err != nil {
		t.Fatalf("b prep output missing: %v", err)
	}
	if !statOK("target/root-0.0.1.jar") {
		t.Fatalf("root jar not built; out: %s", out)
	}

	// 2. rebuild: nothing re-preps.
	code, out = runCLI(t, "build", "-p", ".", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("rebuild exit = %d, want 0; out: %s", code, out)
	}
	if strings.Contains(out, "prep ") {
		t.Fatalf("rebuild re-ran prep: %s", out)
	}
	log, _ = os.ReadFile("prep-log")
	if string(log) != "a\nb\n" {
		t.Fatalf("prep re-ran on rebuild: %q", log)
	}

	// 3. touching a's source re-preps a and (transitively) b, in order.
	writeFile(t, "a/src/a.clj", "(ns a)\n;; touched\n")
	code, out = runCLI(t, "build", "-p", ".", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("touch rebuild exit = %d, want 0; out: %s", code, out)
	}
	log, _ = os.ReadFile("prep-log")
	if string(log) != "a\nb\na\nb\n" {
		t.Fatalf("prep-log after touch = %q, want a b a b", log)
	}

	// 4. run and test see the preps as up to date.
	code, out = runCLI(t, "run", "-p", ".", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("run exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "root-main-ran") {
		t.Fatalf("run out = %q", out)
	}
	if strings.Contains(out, "prep ") {
		t.Fatalf("run re-ran prep: %s", out)
	}
	code, out = runCLI(t, "test", "-p", ".", "--cache-dir", cacheDir, ":ns", "root-test")
	if code != 0 {
		t.Fatalf("test exit = %d, want 0; out: %s", code, out)
	}
	if !strings.Contains(out, "Ran 1 tests") {
		t.Fatalf("test out = %q, want the fixture test to run", out)
	}
	if strings.Contains(out, "prep ") {
		t.Fatalf("test re-ran prep: %s", out)
	}

	// 5. a failing prep fn fails the build.
	writeFile(t, "b/build/build.clj", "(ns build)\n(defn prep [& _] (throw (Exception. \"prep boom\")))\n")
	code, out = runCLI(t, "build", "-p", ".", "--cache-dir", cacheDir)
	if code == 0 {
		t.Fatalf("build succeeded with failing prep; out: %s", out)
	}
	if !strings.Contains(out, "prep boom") {
		t.Fatalf("build out = %q, want prep boom", out)
	}
}
