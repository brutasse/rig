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
	doc := lockfile.ForTest(".", "dep", "jdep", "plain")
	local := func(m string) *string { return &m }
	m := doc.Modules["."]
	m.PrepEnsure, m.PrepAlias, m.PrepFn = []string{"target/classes"}, "prep", "build/prep"
	m.Classpath = []lockfile.ClasspathEntry{{Local: local("dep")}, {Local: local("jdep")}, {Local: local("plain")}}
	doc.Modules["."] = m
	dep := doc.Modules["dep"]
	dep.PrepEnsure, dep.PrepAlias, dep.PrepFn = []string{"target/classes"}, "prep", "build/prep"
	doc.Modules["dep"] = dep
	jdep := doc.Modules["jdep"]
	jdep.Build.JavaSrcDirs = []string{"java"}
	doc.Modules["jdep"] = jdep
	e := prepEnv(t, t.TempDir(), doc)

	set, err := prepSet(e, ".", m, false)
	if err != nil {
		t.Fatal(err)
	}
	// the local dep with java sources is in the set even without a prep fn
	if want := []string{".", "dep", "jdep"}; !reflect.DeepEqual(set, want) {
		t.Errorf("set = %v, want %v", set, want)
	}

	// the target's own java is prep'd only when the caller needs it
	// compiled (test, run), not when the module build does it
	tm := m
	tm.PrepEnsure = nil
	tm.Build.JavaSrcDirs = []string{"java"}
	doc.Modules["."] = tm
	set, err = prepSet(e, ".", tm, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"dep", "jdep"}; !reflect.DeepEqual(set, want) {
		t.Errorf("set = %v, want %v", set, want)
	}
	set, err = prepSet(e, ".", tm, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{".", "dep", "jdep"}; !reflect.DeepEqual(set, want) {
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

	_, err := prepSet(e, ".", root, false)
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
	m.Build.ArtifactDirs = []string{"src"}
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
	if err := e.writeStamp(".", m, st, m.PrepFn, prepStampFile); err != nil {
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
	writeFile(t, filepath.Join(dir, "build", "build.clj"), "(ns build)\n(defn prep [_])\n")
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
	if err := e.writeStamp(".", m, stFresh, m.PrepFn, prepStampFile); err != nil {
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

// TestJavacState walks the staleness matrix on a module whose declared
// java sources have been javac'd and stamped.
func TestJavacState(t *testing.T) {
	dir := t.TempDir()
	java := "package a;\n\npublic class A {}\n"
	writeFile(t, filepath.Join(dir, "java", "a.java"), java)
	writeFile(t, filepath.Join(dir, "target", "classes", "a", "A.class"), "A")

	doc := lockfile.ForTest(".")
	m := doc.Modules["."]
	m.Build.JavaSrcDirs = []string{"java"}
	e := prepEnv(t, dir, doc)

	// no stamp -> stale
	st, err := e.javacState(".", m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !st.stale || st.reason != "no stamp" {
		t.Fatalf("state = %+v, want stale 'no stamp'", st)
	}

	// fresh stamp -> up to date
	if err := e.writeStamp(".", m, st, "javac", javacStampFile); err != nil {
		t.Fatal(err)
	}
	st, err = e.javacState(".", m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.stale {
		t.Fatalf("fresh module reported stale: %s", st.reason)
	}

	// java source changed -> stale
	writeFile(t, filepath.Join(dir, "java", "a.java"), java+"// changed\n")
	st, _ = e.javacState(".", m, nil)
	if !st.stale || st.reason != "sources changed" {
		t.Errorf("state = %+v, want stale 'sources changed'", st)
	}

	// class-dir missing -> stale (checked before the stamp; re-stamp first)
	writeFile(t, filepath.Join(dir, "java", "a.java"), java)
	stFresh, _ := e.javacState(".", m, nil)
	if err := e.writeStamp(".", m, stFresh, "javac", javacStampFile); err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(filepath.Join(dir, "target"))
	st, _ = e.javacState(".", m, nil)
	if !st.stale || !strings.HasPrefix(st.reason, "missing") {
		t.Errorf("state = %+v, want stale 'missing ...'", st)
	}

	// manifest changed -> stale (re-stamp: the missing-output scenario
	// wiped target/ and the stamp with it)
	_ = os.MkdirAll(filepath.Join(dir, "target", "classes", "a"), 0o755)
	writeFile(t, filepath.Join(dir, "target", "classes", "a", "A.class"), "A")
	stFresh, _ = e.javacState(".", m, nil)
	if err := e.writeStamp(".", m, stFresh, "javac", javacStampFile); err != nil {
		t.Fatal(err)
	}
	m2 := m
	m2.ManifestSHA256 = strings.Repeat("0", 64)
	st, _ = e.javacState(".", m2, nil)
	if !st.stale || st.reason != "manifest changed" {
		t.Errorf("state = %+v, want stale 'manifest changed'", st)
	}

	// a re-prepped local dependency invalidates the stamp
	dep := "dep"
	m3 := m
	m3.Classpath = append(m3.Classpath, lockfile.ClasspathEntry{Local: &dep})
	st, _ = e.javacState(".", m3, map[string]bool{"dep": true})
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

// TestBuildPrepEndToEnd exercises the full flow: lock, build (native
// javac and prep fns run in dependency order, javac before the fn of the
// same module), rebuild (no re-prep), source touch (transitive re-prep),
// run and test (up to date), failing prep fn.
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
	    :rig/modules ["a" "b" "c" "d"]
	    :deps {org.clojure/clojure {:mvn/version "1.12.5"}
	            example/a {:local/root "a"}
	            example/b {:local/root "b"}
	            example/c {:local/root "c"}
	            example/d {:local/root "d"}}
	    :aliases {:test {:extra-paths ["test"]
	                   :exec-fn test-exec/exec}}}`+"\n")
	writeFile(t, "src/root.clj", "(ns root)\n(defn hello [] (str (c.Thing/hi) \"/\" (d.Thing/hi)))\n(defn -main [] (println \"root-main-ran\"))\n")
	writeFile(t, "src/test_exec.clj", "(ns test-exec\n  (:require [clojure.test :as t]))\n\n(defn exec [opts]\n  (let [ns (symbol (or (:ns opts) \"root-test\"))]\n    (require ns)\n    (let [summary (t/run-tests ns)]\n      (and (zero? (:fail summary 0)) (zero? (:error summary 0))))))\n")
	writeFile(t, "test/root_test.clj", "(ns root-test\n  (:require [clojure.test :refer :all] [root :as r]))\n(deftest main-works (is (= (r/hello) \"c-java/d-java\")))\n")
	writeFile(t, "a/deps.edn",
		"{:rig/lib \"example/a\"\n :deps {org.clojure/clojure {:mvn/version \"1.12.5\"}}\n :deps/prep-lib {:ensure \"target/classes\" :alias :prep :fn prep}\n :aliases {:prep {:extra-paths [\"build\"] :ns-default build}}}\n")
	// strict single-arg prep fn: a 0-arg invocation fails with an
	// ArityException and the build errors out
	writeFile(t, "a/build/build.clj",
		"(ns build)\n\n(defn prep [_]\n  (clojure.java.io/make-parents (clojure.java.io/file \"target\" \"classes\" \"marker.txt\"))\n  (spit \"target/classes/marker.txt\" \"a\")\n  (spit \"../prep-log\" \"a\\n\" :append true))\n")
	writeFile(t, "a/src/a.clj", "(ns a)\n")
	writeFile(t, "b/deps.edn",
		"{:rig/lib \"example/b\"\n :deps {org.clojure/clojure {:mvn/version \"1.12.5\"}\n        example/a {:local/root \"../a\"}}\n :deps/prep-lib {:ensure \"target/classes\" :alias :prep :fn prep}\n :aliases {:prep {:extra-paths [\"build\"] :ns-default build}}}\n")
	writeFile(t, "b/build/build.clj",
		"(ns build)\n\n(defn prep [_]\n  (clojure.java.io/make-parents (clojure.java.io/file \"target\" \"classes\" \"marker.txt\"))\n  (spit \"target/classes/marker.txt\" \"b\")\n  (spit \"../prep-log\" \"b\\n\" :append true))\n")
	writeFile(t, "b/src/b.clj", "(ns b)\n")
	// c: declared java sources, no prep-lib — rig javacs it natively.
	// target/classes flows to the dependents' classpath via :paths.
	writeFile(t, "c/deps.edn",
		"{:rig/lib \"example/c\"\n :paths [\"target/classes\"]\n :deps {org.clojure/clojure {:mvn/version \"1.12.5\"}}\n :rig/java-src-dirs [\"java\"]}\n")
	writeFile(t, "c/java/c/Thing.java", "package c;\n\npublic class Thing {\n  public static String hi() {\n    return \"c-java\";\n  }\n}\n")
	// d: declared java sources AND a prep fn — the javac runs first, and
	// the fn asserts the javac'd class is already in place.
	writeFile(t, "d/deps.edn",
		"{:rig/lib \"example/d\"\n :paths [\"src\" \"target/classes\"]\n :deps {example/c {:local/root \"../c\"}}\n :rig/java-src-dirs [\"java\"]\n :deps/prep-lib {:ensure \"target/classes\" :alias :prep :fn prep}\n :aliases {:prep {:extra-paths [\"build\"] :ns-default build}}}\n")
	writeFile(t, "d/build/build.clj",
		"(ns build)\n\n(defn prep [_]\n  (when-not (.exists (clojure.java.io/file \"target\" \"classes\" \"d\" \"Thing.class\"))\n    (throw (Exception. \"d prep ran before the javac\")))\n  (clojure.java.io/make-parents (clojure.java.io/file \"target\" \"classes\" \"marker.txt\"))\n  (spit \"target/classes/marker.txt\" \"d\")\n  (spit \"../prep-log\" \"d\\n\" :append true))\n")
	writeFile(t, "d/java/d/Thing.java", "package d;\n\npublic class Thing {\n  public static String hi() {\n    return \"d-java\";\n  }\n}\n")

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

	// 1. build preps everything, in dependency order: c javac'd natively,
	// d javac'd before its own prep fn, then builds the jar (root's
	// namespaces compile against the javac'd c and d classes).
	code, out = runCLI(t, "build", "-p", ".", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("build exit = %d, want 0; out: %s", code, out)
	}
	for _, want := range []string{"prep a:", "prep b:", "prep c: javac", "prep d: javac", "prep d: build/prep"} {
		if !strings.Contains(out, want) {
			t.Fatalf("build out missing %q: %s", want, out)
		}
	}
	ai, bi := strings.Index(out, "prep a:"), strings.Index(out, "prep b:")
	ci, dji, dpi := strings.Index(out, "prep c: javac"), strings.Index(out, "prep d: javac"), strings.Index(out, "prep d: build/prep")
	if !(ai < bi && ci < dji && dji < dpi) {
		t.Errorf("prep order wrong (a=%d b=%d c=%d d-javac=%d d-prep=%d): %s", ai, bi, ci, dji, dpi, out)
	}
	log, err := os.ReadFile("prep-log")
	if err != nil || string(log) != "a\nb\nd\n" {
		t.Fatalf("prep-log = %q (err %v), want \"a\\nb\\nd\\n\"", log, err)
	}
	for _, p := range []string{
		"a/target/classes/marker.txt",
		"b/target/classes/marker.txt",
		"c/target/classes/c/Thing.class",
		"d/target/classes/d/Thing.class",
		"d/target/classes/marker.txt",
		"a/target/.rig-prep.json",
		"c/target/.rig-javac.json",
		"d/target/.rig-javac.json",
		"d/target/.rig-prep.json",
	} {
		if !statOK(p) {
			t.Fatalf("%s missing after build; out: %s", p, out)
		}
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
	if string(log) != "a\nb\nd\n" {
		t.Fatalf("prep re-ran on rebuild: %q", log)
	}

	// 3. touching a's source re-preps a and (transitively) b, in order —
	// not c or d, which do not depend on a.
	writeFile(t, "a/src/a.clj", "(ns a)\n;; touched\n")
	code, out = runCLI(t, "build", "-p", ".", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("touch rebuild exit = %d, want 0; out: %s", code, out)
	}
	log, _ = os.ReadFile("prep-log")
	if string(log) != "a\nb\nd\na\nb\n" {
		t.Fatalf("prep-log after touch = %q, want a b d a b", log)
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

	// 5. touching c's java re-javacs c and (transitively) re-preps d —
	// javac and fn — while a and b stay up to date.
	writeFile(t, "c/java/c/Thing.java", "package c;\n\npublic class Thing {\n  public static String hi() {\n    return \"c-java-2\";\n  }\n}\n")
	code, out = runCLI(t, "build", "-p", ".", "--cache-dir", cacheDir)
	if code != 0 {
		t.Fatalf("touch-c rebuild exit = %d, want 0; out: %s", code, out)
	}
	for _, want := range []string{"prep c: javac", "prep d: javac", "prep d: build/prep"} {
		if !strings.Contains(out, want) {
			t.Fatalf("touch-c out missing %q: %s", want, out)
		}
	}
	if strings.Contains(out, "prep a:") || strings.Contains(out, "prep b:") {
		t.Fatalf("touch-c re-prepped unrelated modules: %s", out)
	}
	log, _ = os.ReadFile("prep-log")
	if string(log) != "a\nb\nd\na\nb\nd\n" {
		t.Fatalf("prep-log after touch-c = %q, want a b d a b d", log)
	}

	// 6. a failing prep fn fails the build.
	writeFile(t, "b/build/build.clj", "(ns build)\n(defn prep [_] (throw (Exception. \"prep boom\")))\n")
	code, out = runCLI(t, "build", "-p", ".", "--cache-dir", cacheDir)
	if code == 0 {
		t.Fatalf("build succeeded with failing prep; out: %s", out)
	}
	if !strings.Contains(out, "prep boom") {
		t.Fatalf("build out = %q, want prep boom", out)
	}
}
