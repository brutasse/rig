(ns rig.resolver.manifest-test
  (:require [clojure.java.io :as io]
            [clojure.test :refer :all]
            [clojure.tools.build.api :as b]
            [rig.resolver.manifest :as manifest]))

(defn- temp-dir
  []
  (doto (io/file (str (java.nio.file.Files/createTempDirectory
                       "rig-manifest-test"
                       (into-array java.nio.file.attribute.FileAttribute []))))
    (.deleteOnExit)))

(deftest version-fn-template-trailing-newline-is-not-the-version
  "A newline-terminated template file is the normal case; the LF must not
  end up in the version (and from there in the artifact filename)."
  (with-redefs [b/git-count-revs (constantly "42")]
    (let [dir (temp-dir)]
      (spit (io/file dir "VERSION_TEMPLATE") "1.0.GENERATED_VERSION\n")
      (is (= "1.0.42"
             (manifest/read-version {:rig/version-fn :git-count-revs}
                                    (.getPath dir) (.getPath dir)))))))

(deftest version-file-first-non-blank-line-wins
  (let [dir (temp-dir)]
    (spit (io/file dir "VERSION") "\n1.2.3\nnext line ignored\n")
    (is (= "1.2.3" (manifest/read-version {} (.getPath dir) (.getPath dir))))))
