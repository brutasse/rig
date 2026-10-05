(ns rig.resolver.aot-test
  (:require [clojure.java.io :as io]
            [clojure.test :refer :all]
            [rig.resolver.aot :as aot]
            [rig.resolver.test-util :refer [temp-dir]]))

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
    (let [plan (aot/plan [(.getPath src)] [])]
      (is (= ["a.core"] (:compile plan)))
      (is (= #{"migratus.clj" "template/core.clj"}
             (set (map #(.substring (:file %) (inc (.length (.getPath src))))
                       (:skipped plan)))))
      (is (= #{"no ns declaration" "unreadable ns declaration"}
             (set (map :reason (:skipped plan))))))))

(deftest plan-clean-when-all-sources-parse
  (let [src (temp-dir)]
    (io/make-parents (io/file src "a" "core.clj"))
    (spit (io/file src "a" "core.clj") "(ns a.core)\n")
    (is (empty? (:skipped (aot/plan [(.getPath src)] []))))))

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
    (let [plan (aot/plan [(.getPath src) (.getPath res)] [])]
      (is (= ["a.util" "a.core"] (:compile plan)))
      (is (empty? (:preload plan))))))

(deftest plan-tolerates-self-requirement
  "A namespace :requiring itself is legal: the JVM load/compile ignores
  the edge, and call sites may qualify through the self-alias.
  The dependency graph must not turn it into 'Circular
  dependency between X and X'."
  (let [src (temp-dir)]
    (spit-clj src "a/util.clj" "(ns a.util)\n")
    (spit-clj src "a/core.clj"
              "(ns a.core\n  (:require [a.core :as self] [a.util :as u]))\n(defn f [x] (self/f x))\n")
    (let [plan (aot/plan [(.getPath src)] [])]
      (is (= ["a.util" "a.core"] (:compile plan)))
      (is (empty? (:preload plan))))))

(deftest plan-accepts-the-munged-file-name
  "load resolves my-ns.foo to my_ns/foo.clj: the underscored file IS
  loadable and must stay in the plan; the dashed file of the same
  namespace is not."
  (let [root (temp-dir)
        src (io/file root "src")]
    (spit-clj src "my_ns/foo.clj" "(ns my-ns.foo)\n")
    (is (= ["my-ns.foo"] (:compile (aot/plan [(.getPath src)] []))))
    (let [root2 (temp-dir)
          src2 (io/file root2 "src")]
      (spit-clj src2 "my-ns/foo.clj" "(ns my-ns.foo)\n")
      (is (empty? (:compile (aot/plan [(.getPath src2)] [])))))))
