(ns rig.resolver.aot-test
  (:require [clojure.java.io :as io]
            [clojure.test :refer :all]
            [rig.resolver.aot :as aot]))

(defn- temp-src
  []
  (doto (io/file (str (java.nio.file.Files/createTempDirectory
                       "rig-aot-test"
                       (into-array java.nio.file.attribute.FileAttribute []))))
    (.deleteOnExit)))

<<<<<<< HEAD
(deftest plan-skips-data-sources-and-reports-them
  "A .clj under :paths without a readable ns form is data (a config map
  file, a deps-new template), not a namespace: the plan leaves it out
  and says why, instead of dying."
  (let [src (temp-src)]
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
  (let [src (temp-src)]
    (io/make-parents (io/file src "a" "core.clj"))
    (spit (io/file src "a" "core.clj") "(ns a.core)\n")
    (is (empty? (:skipped (aot/plan [(.getPath src)] []))))))
=======
(defn- spit-clj
  [root rel text]
  (let [f (io/file root rel)]
    (io/make-parents f)
    (spit f text)))

(deftest plan-tolerates-self-requirement
  "A namespace :requiring itself is legal: the JVM load/compile ignores
  the edge, and call sites may qualify through the self-alias.
  The dependency graph must not turn it into 'Circular
  dependency between X and X'."
  (let [src (temp-src)]
    (spit-clj src "a/util.clj" "(ns a.util)\n")
    (spit-clj src "a/core.clj"
              "(ns a.core\n  (:require [a.core :as self] [a.util :as u]))\n(defn f [x] (self/f x))\n")
    (let [plan (aot/plan [(.getPath src)] [])]
      (is (= ["a.util" "a.core"] (:compile plan)))
      (is (empty? (:preload plan))))))
>>>>>>> origin/main
