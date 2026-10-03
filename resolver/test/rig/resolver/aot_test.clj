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
