(ns rig.resolver.manifest-test
  (:require [clojure.java.io :as io]
            [clojure.test :refer :all]
            [clojure.tools.build.api :as b]
            [rig.resolver.manifest :as manifest]
            [rig.resolver.test-util :refer [temp-dir]]))

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

(deftest read-manifest-refuses-a-removed-key
  "A removed :rig/* key is refused like a legacy one: the manifest names
  the file and the fix, and the refusal marks the removed-key exit code."
  (let [dir (temp-dir)]
    (spit (io/file dir "deps.edn")
          "{:rig/lib x/y\n :rig/ns-compile [a.b c.d]}\n")
    (let [e (try
              (manifest/read-manifest (.getPath dir))
              (catch Exception ex
                ex))]
      (is (instance? Exception e))
      (is (true? (get (ex-data e) :rig/removed?)))
      (is (re-find #"uses removed key :rig/ns-compile" (.getMessage e))))))

(deftest read-manifest-refuses-legacy-keys-before-removed-ones
  "With both a legacy key and a removed key present the legacy refusal
  wins: migrate is the single command that fixes both."
  (let [dir (temp-dir)]
    (spit (io/file dir "deps.edn")
          "{:exoscale.project/lib x/y\n :rig/ns-compile [a.b]}\n")
    (let [e (try
              (manifest/read-manifest (.getPath dir))
              (catch Exception ex
                ex))]
      (is (true? (get (ex-data e) :rig/legacy?)))
      (is (nil? (get (ex-data e) :rig/removed?))))))
