(ns rig.resolver.aot-test
  (:require [clojure.java.io :as io]
            [clojure.test :refer :all]
            [rig.resolver.aot :as aot]
            [rig.resolver.test-util :refer [temp-dir]])
  (:import (java.util.zip ZipEntry ZipOutputStream)))

(defn- write-jar
  "A jar in dir holding [entry-name content] pairs."
  [dir & entries]
  (let [jar (io/file dir "dep.jar")]
    (io/make-parents jar)
    (with-open [zos (ZipOutputStream. (io/output-stream jar))]
      (doseq [[name content] entries]
        (.putNextEntry zos (ZipEntry. name))
        (when content (.write zos (.getBytes content)))
        (.closeEntry zos)))
    (.getPath jar)))

(deftest plan-skips-data-sources-and-reports-them
  "A .clj under :paths without a readable ns form is data (a config map
  file, a deps-new template), not a namespace: the plan leaves it out
  and says why, instead of dying."
  (let [src (temp-dir)]
    (io/make-parents (io/file src "a" "core.clj"))
    (spit (io/file src "a" "core.clj") "(ns a.core)\n")
    (spit (io/file src "migratus.clj") "{:store :in-memory\n :migration-dir \"migrations\"}\n")
    (io/make-parents (io/file src "template" "core.clj"))
    (spit (io/file src "template" "core.clj") "(ns {{final-name}}.core\n  (:require [clojure.string]))\n")
    (let [plan (aot/plan [(.getPath src)] nil nil)]
      (is (= ["a.core"] (:compile plan)))
      (is (= #{"migratus.clj" "template/core.clj"}
             (set (map #(.substring (:file %) (inc (.length (.getPath src))))
                       (:skipped plan)))))
      (is (= #{"no ns declaration" "unreadable ns declaration"}
             (set (map :reason (:skipped plan))))))))

(defn- spit-clj
  [root rel text]
  (let [f (io/file root rel)]
    (io/make-parents f)
    (spit f text)))

(deftest plan-skips-sources-the-classpath-cannot-resolve
  "A .clj nested below a path root whose declared namespace does not map
  back to its path (exported clj-kondo hooks, templates) is not loadable:
  compiling it fails with 'Could not locate ... on classpath', so it
  must stay out of the plan."
  (let [root (temp-dir)
        src (io/file root "src")
        res (io/file root "resources")]
    (spit-clj src "a/core.clj" "(ns a.core\n  (:require [a.util :as u]))\n")
    (spit-clj src "a/util.clj" "(ns a.util)\n")
    (spit-clj res "clj-kondo.exports/acme/a_hooks.clj" "(ns a_hooks)\n")
    (let [plan (aot/plan [(.getPath src) (.getPath res)] nil nil)]
      (is (= ["a.util" "a.core"] (:compile plan)))
      (is (empty? (:preload plan))))))

(deftest plan-tolerates-self-requirement
  "A namespace :requiring itself is legal: the JVM load/compile ignores
  the edge, and call sites may qualify through the self-alias.
  The dependency graph must not turn it into 'Circular
  dependency between X and X'. Sources that all parse are planned in full:
  nothing is reported skipped."
  (let [src (temp-dir)]
    (spit-clj src "a/util.clj" "(ns a.util)\n")
    (spit-clj src "a/core.clj"
              "(ns a.core\n  (:require [a.core :as self] [a.util :as u]))\n(defn f [x] (self/f x))\n")
    (let [plan (aot/plan [(.getPath src)] nil nil)]
      (is (= ["a.util" "a.core"] (:compile plan)))
      (is (empty? (:preload plan)))
      (is (empty? (:skipped plan))))))

(deftest plan-adds-the-entry-namespace-that-is-not-a-module-namespace
  "The build's :rig/main joins the entry points when it is not one of the
  module's own namespaces and a source for it exists on the classpath: its
  classes must reach the artifact even though nothing compiles it by
  construction, so the plan compiles it."
  (let [src (temp-dir)
        dep (temp-dir)]
    (spit-clj src "a/core.clj" "(ns a.core)\n")
    (spit-clj dep "entry/main.clj" "(ns entry.main)\n")
    (let [plan (aot/plan [(.getPath src)] "entry.main" [(.getPath dep)])]
      (is (= ["a.core" "entry.main"] (:compile plan)))
      (is (= ["entry.main"] (:extra plan))))))

(deftest plan-keeps-a-module-entry-namespace-out-of-the-entry-points
  "A :rig/main that IS one of the module's namespaces stays out of the
  entry points: it already compiles in the module's own list, and making
  it an entry point would run its top level twice."
  (let [src (temp-dir)]
    (spit-clj src "a/core.clj" "(ns a.core)\n")
    (spit-clj src "entry/main.clj" "(ns entry.main)\n")
    (let [plan (aot/plan [(.getPath src)] "entry.main" [(.getPath src)])]
      (is (= #{"entry.main" "a.core"} (set (:compile plan))))
      (is (empty? (:extra plan))))))

(deftest plan-adds-in-tree-gen-class-namespaces-to-the-entry-points
  "An in-tree namespace whose ns form carries :gen-class joins the entry
  points by construction: its generated class reaches the artifact only
  as AOT output, so the plan compiles it unconditionally."
  (let [src (temp-dir)]
    (spit-clj src "a/util.clj" "(ns a.util\n  (:gen-class))\n")
    (spit-clj src "a/core.clj" "(ns a.core\n  (:require [a.util :as u]))\n")
    (let [plan (aot/plan [(.getPath src)] nil nil)]
      (is (= ["a.util"] (:extra plan)))
      (is (= ["a.util" "a.core"] (:compile plan))))))

(deftest plan-detects-a-gen-class-with-options
  "(:gen-class :name Foo) is the clause with options; the entry-point
  detection must not require the bare form."
  (let [src (temp-dir)]
    (spit-clj src "a/util.clj" "(ns a.util\n  (:gen-class :name a.util.Helper))\n")
    (let [plan (aot/plan [(.getPath src)] nil nil)]
      (is (= ["a.util"] (:extra plan))))))

(deftest plan-compiles-a-gen-class-main-exactly-once
  "A :rig/main that IS one of the module's namespaces and carries
  :gen-class joins the entry points like any other :gen-class namespace
  (its generated class needs the unconditional compile), but the
  compile list carries it once: its top level never runs twice."
  (let [src (temp-dir)]
    (spit-clj src "entry/main.clj" "(ns entry.main\n  (:gen-class))\n")
    (let [plan (aot/plan [(.getPath src)] "entry.main" [(.getPath src)])]
      (is (= ["entry.main"] (:compile plan)))
      (is (= ["entry.main"] (:extra plan))))))

(deftest plan-ignores-a-main-with-no-source-on-the-classpath
  "A :rig/main that is not a loadable Clojure source stays out of the
  entry points: nothing to compile (a Java main class, e.g. the
  resolver's own Main-Class), so the plan must not try to compile it."
  (let [src (temp-dir)]
    (spit-clj src "a/core.clj" "(ns a.core)\n")
    (let [plan (aot/plan [(.getPath src)] "Main" [(.getPath src)])]
      (is (= ["a.core"] (:compile plan)))
      (is (empty? (:extra plan))))))

(deftest plan-ignores-a-main-the-classpath-carries-as-classes-only
  "A :rig/main whose namespace the classpath ships only as compiled
  classes (a .class, no .clj/.cljc) has nothing to compile here: it
  stays out of the entry points, even when a source for a different
  namespace is present."
  (let [src (temp-dir)
        dep (temp-dir)]
    (spit-clj src "a/core.clj" "(ns a.core)\n")
    (spit-clj dep "entry/main.clj" "(ns entry.main)\n")
    (io/make-parents (io/file dep "dep" "only"))
    (spit (io/file dep "dep" "only.class") "")
    (let [plan (aot/plan [(.getPath src)] "dep.only" [(.getPath dep)])]
      (is (= ["a.core"] (:compile plan)))
      (is (empty? (:extra plan))))))

(deftest plan-gates-a-main-found-in-a-dependency-jar
  "main-source? looks into jar roots: a .clj or .cljc entry in a jar is
  enough for main to join the entry points; a jar that ships only
  compiled classes is not."
  (let [src (temp-dir)
        dep (temp-dir)
        src-jar (write-jar dep ["entry/main.clj" "(ns entry.main)\n"])
        cljc-jar (write-jar (io/file dep "c") ["other/app.cljc" "(ns other.app)\n"])
        class-jar (write-jar (io/file dep "cls") ["dep/only.class" ""])]
    (spit-clj src "a/core.clj" "(ns a.core)\n")
    (is (= ["entry.main"]
           (:extra (aot/plan [(.getPath src)] "entry.main" [src-jar]))))
    (is (= ["other.app"]
           (:extra (aot/plan [(.getPath src)] "other.app" [cljc-jar]))))
    (is (empty?
         (:extra (aot/plan [(.getPath src)] "dep.only" [class-jar]))))))

(deftest plan-accepts-the-munged-file-name
  "load resolves my-ns.foo to my_ns/foo.clj: the underscored file IS
  loadable and must stay in the plan; the dashed file of the same
  namespace is not."
  (let [root (temp-dir)
        src (io/file root "src")]
    (spit-clj src "my_ns/foo.clj" "(ns my-ns.foo)\n")
    (is (= ["my-ns.foo"] (:compile (aot/plan [(.getPath src)] nil nil))))
    (let [root2 (temp-dir)
          src2 (io/file root2 "src")]
      (spit-clj src2 "my-ns/foo.clj" "(ns my-ns.foo)\n")
      (is (empty? (:compile (aot/plan [(.getPath src2)] nil nil)))))))
