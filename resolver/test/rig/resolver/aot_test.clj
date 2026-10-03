(ns rig.resolver.aot-test
  (:require [clojure.java.io :as io]
            [clojure.test :refer :all]
            [rig.resolver.aot :as aot]))

(defn- temp-dir
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
