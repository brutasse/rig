(ns rig.resolver.migrate-test
  (:require [clojure.edn :as edn]
            [clojure.java.io :as io]
            [clojure.test :refer :all]
            [rig.resolver.migrate :as migrate]))

(defn- temp-ws
  "A temp workspace holding the given {relative-path text} files."
  [files]
  (let [ws (doto (java.io.File.
                    (str (java.nio.file.Files/createTempDirectory "rig-migrate-test"
                                                                  (into-array java.nio.file.attribute.FileAttribute []))))
               (.deleteOnExit))]
    (doseq [[rel text] files]
      (io/make-parents (io/file ws rel))
      (spit (io/file ws rel) text))
    (str ws)))

(defn- run
  "Run the :migrate op on ws; with :dry-run? true no file is written."
  [ws & {:keys [dry-run?]}]
  (migrate/migrate (if (some? dry-run?)
                     {:workspace ws :args {:dry-run? dry-run?}}
                     {:workspace ws})))

(defn- file-text [ws rel] (slurp (io/file ws rel)))

(defn- file-edn [ws rel] (edn/read-string (file-text ws rel)))

(defn- problems [r] (get r "problems"))

(defn- warnings [r] (get r "warnings"))

(def root-with-managed
  "{:exoscale.project/lib x/y\n :exoscale.project/modules [\"modules/m\" \"modules/n\"]\n :exoscale.deps/managed-dependencies {a/b {:mvn/version \"1.0.0\" :exclusions [x/y]}\n                                                              c/d {:local/root \"modules/m\"}\n                                                              e/f {:deps/root \"modules/m\"}\n                                                              t/c {:mvn/version \"1.1.0\"}}}\n")

(deftest legacy-keys-are-renamed
  (let [ws (temp-ws {"deps.edn"
                     "{:exoscale.project/lib x/y\n :exoscale.project/version-file \"VERSION\"\n :exoscale.project/deploy? true}\n"})]
    (let [r (run ws)]
      (is (empty? (problems r)))
      (let [d (file-edn ws "deps.edn")]
        (is (= 'x/y (get d :rig/lib)))
        (is (= "VERSION" (get d :rig/version-file)))
        (is (= true (get d :rig/publish?)))
        (is (nil? (get d :exoscale.project/lib)))
        (is (nil? (get d :exoscale.project/version-file)))
        (is (nil? (get d :exoscale.project/deploy?)))))))

(deftest bypass-test-is-inverted
  (doseq [[legacy expected] [[true false] [false true]]]
    (let [ws (temp-ws {"deps.edn"
                       (str "{:exoscale.project/lib x/y :exoscale.project/bypass-test? " legacy "}\n")})]
      (let [r (run ws)]
        (is (empty? (problems r)))
        (is (= expected (get (file-edn ws "deps.edn") :rig/test?)))))))

(deftest known-version-fn-becomes-a-keyword
  (doseq [value [":exoscale.tools.project.api.version/git-count-revs"
                 "\"exoscale.tools.project.api.version/git-count-revs\""]]
    (let [ws (temp-ws {"deps.edn"
                       (str "{:exoscale.project/lib x/y\n :exoscale.project/version-fn " value "}\n")})]
      (is (empty? (problems (run ws))))
      (is (= :git-count-revs (get (file-edn ws "deps.edn") :rig/version-fn))))))

(deftest unknown-version-fn-is-a-problem
  (doseq [value [":my.proj/revs" "\"my.proj/revs\""]]
    (let [text (str "{:exoscale.project/lib x/y :exoscale.project/version-fn " value "}\n")
          ws (temp-ws {"deps.edn" text})
          r (run ws)]
      (is (seq (problems r)))
      (is (= text (file-text ws "deps.edn"))))))

(deftest version-fn-in-module-files
  (let [ws (temp-ws {"deps.edn"
                     "{:exoscale.project/lib x/y :exoscale.project/modules [\"modules/m\"]}\n"
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib m/lib :exoscale.project/version-fn :exoscale.tools.project.api.version/git-count-revs}\n"})]
    (is (empty? (problems (run ws))))
    (is (= :git-count-revs (get (file-edn ws "modules/m/deps.edn") :rig/version-fn)))))

(deftest root-managed-dependencies-become-rig-deps
  (let [ws (temp-ws {"deps.edn"
                     "{:exoscale.project/lib x/y\n :exoscale.deps/managed-dependencies {a/b {:mvn/version \"1.0.0\"}}}\n"})]
    (let [r (run ws)]
      (is (empty? (problems r)))
      (let [d (file-edn ws "deps.edn")]
        (is (= {:mvn/version "1.0.0"} (get (get d :rig/deps) 'a/b)))
        (is (nil? (get d :exoscale.deps/managed-dependencies)))))))

(deftest managed-dependencies-at-module-level-are-a-problem
  (let [ws (temp-ws {"deps.edn"
                     "{:exoscale.project/lib x/y :exoscale.project/modules [\"modules/m\"]}\n"
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib m/lib\n :exoscale.deps/managed-dependencies {a/b {:mvn/version \"1\"}}}\n"})
        r (run ws)]
    (is (some #(re-find #"managed-dependencies" %) (problems r)))
    (is (re-find #"exoscale" (file-text ws "modules/m/deps.edn")))))

(deftest inherit-all-materializes-managed-values
  (let [ws (temp-ws {"deps.edn" root-with-managed
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib m/lib\n :deps {a/b {:mvn/version \"0.9.0\" :exoscale.deps/inherit :all}\n                   plain/p {:mvn/version \"2.0.0\"}}}\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (let [d (file-edn ws "modules/m/deps.edn")]
      (is (= {:mvn/version "1.0.0" :exclusions '[x/y]}
             (get (get d :deps) 'a/b)))
      (is (= {:mvn/version "2.0.0"} (get (get d :deps) 'plain/p))))))

(deftest materialized-dep-with-extra-keyword-pair-stays-valid
  "Regression: the value node of a materialized dep map is built pair by
  pair; without a separator between pairs a keyword-valued pair glues the
  next key (\":else\" + \":mvn/version\" -> \":else:mvn/version\"), leaving
  an odd-form map the EDN reader rejects."
  (let [ws (temp-ws {"deps.edn"
                     "{:exoscale.project/lib x/y\n :exoscale.project/modules [\"modules/m\"]\n :exoscale.deps/managed-dependencies {a/b {:mvn/version \"1.0.0\"}}}\n"
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib x/m\n :deps {a/b {:exoscale.deps/inherit :all\n                      :mvn/version \"0.9.0\"\n                      :something :else}}}\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (= {:mvn/version "1.0.0" :something :else}
           (get (get (file-edn ws "modules/m/deps.edn") :deps) 'a/b)))))

(deftest inherit-vector-selects-managed-keys
  (let [ws (temp-ws {"deps.edn" root-with-managed
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib m/lib :deps {a/b {:exoscale.deps/inherit [:mvn/version]}}}\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (= {:mvn/version "1.0.0"}
           (get (get (file-edn ws "modules/m/deps.edn") :deps) 'a/b)))))

(deftest inherit-only-dep-missing-from-managed-is-a-problem
  (let [text "{:exoscale.project/lib m/lib :deps {zz/z {:exoscale.deps/inherit :all}}}\n"
        ws (temp-ws {"deps.edn" root-with-managed
                     "modules/m/deps.edn" text})
        r (run ws)]
    (is (some #(re-find #"zz/z.*declares no version" %) (problems r)))
    (is (= text (file-text ws "modules/m/deps.edn")))
    (is (re-find #"exoscale" (file-text ws "deps.edn")))))

(deftest unmanaged-inherit-deps-keep-declared-keys
  (let [ws (temp-ws {"deps.edn" root-with-managed
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib m/lib :deps {zz/z {:exoscale.deps/inherit :all :mvn/version \"2.0.0\"}\n                                                          mm/mm {:exoscale.deps/inherit :all :local/root \"../n\"}}}\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (= 2 (count (filter #(re-find #":exoscale.deps/inherit dropped" %) (warnings r)))))
    (is (= {:mvn/version "2.0.0"}
           (get-in (file-edn ws "modules/m/deps.edn") [:deps 'zz/z])))
    (is (= {:local/root "../n"}
           (get-in (file-edn ws "modules/m/deps.edn") [:deps 'mm/mm])))))

(deftest local-root-is-canonicalized-module-relative
  (let [ws (temp-ws {"deps.edn" root-with-managed
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib m/lib :deps {c/d {:exoscale.deps/inherit :all}}}\n"
                     "modules/n/deps.edn"
                     "{:exoscale.project/lib n/lib :deps {c/d {:exoscale.deps/inherit :all}}}\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (= "." (get-in (file-edn ws "modules/m/deps.edn") [:deps 'c/d :local/root])))
    (is (= "../m" (get-in (file-edn ws "modules/n/deps.edn") [:deps 'c/d :local/root])))))

(deftest deps-root-is-not-canonicalized
  (let [ws (temp-ws {"deps.edn" root-with-managed
                     "modules/n/deps.edn"
                     "{:exoscale.project/lib n/lib :deps {e/f {:exoscale.deps/inherit :all}}}\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (= "modules/m"
           (get-in (file-edn ws "modules/n/deps.edn") [:deps 'e/f :deps/root])))))

(deftest managed-aliases-only-project-are-dropped-with-a-warning
  (let [ws (temp-ws {"deps.edn"
                     "{:exoscale.project/lib x/y\n :exoscale.deps/managed-aliases {:project {:extra-deps {a/b {:mvn/version \"1\"}}}}}\n"})]
    (let [r (run ws)]
      (is (empty? (problems r)))
      (is (some #(re-find #"managed-aliases" %) (warnings r)))
      (is (nil? (get (file-edn ws "deps.edn") :exoscale.deps/managed-aliases))))))

(deftest managed-aliases-other-entries-are-dropped-with-a-warning
  (let [ws (temp-ws {"deps.edn"
                     "{:exoscale.project/lib x/y\n :exoscale.deps/managed-aliases {:project {} :dev {}}}\n"})]
    (let [r (run ws)]
      (is (empty? (problems r)))
      (is (some #(re-find #":dev" %) (warnings r)))
      (is (nil? (get (file-edn ws "deps.edn") :exoscale.deps/managed-aliases))))))

(deftest project-alias-is-dropped
  (let [ws (temp-ws {"deps.edn"
                     "{:exoscale.project/lib x/y\n :aliases {:project {:exoscale.deps/inherit :all}}}\n"})]
    (is (empty? (problems (run ws))))
    (is (nil? (get (file-edn ws "deps.edn") :aliases)))))

(deftest non-project-aliases-are-kept
  (let [ws (temp-ws {"deps.edn"
                     "{:exoscale.project/lib x/y\n :aliases {:project {:exoscale.deps/inherit :all}\n                      :test {:extra-paths [\"test\"]}}}\n"})]
    (is (empty? (problems (run ws))))
    (is (= {:extra-paths ["test"]}
           (get (get (file-edn ws "deps.edn") :aliases) :test)))))

(deftest alias-extra-deps-inherit-is-materialized
  (let [ws (temp-ws {"deps.edn" root-with-managed
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib m/lib\n :aliases {:project {:exoscale.deps/inherit :all}\n                       :test {:extra-deps {t/c {:exoscale.deps/inherit :all}}}}}\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (= {:mvn/version "1.1.0"}
           (get-in (file-edn ws "modules/m/deps.edn")
                   [:aliases :test :extra-deps 't/c])))))

(deftest alias-top-level-inherit-is-dropped-with-a-warning
  (let [ws (temp-ws {"deps.edn" root-with-managed
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib m/lib :aliases {:test {:exoscale.deps/inherit :all :extra-paths [\"test\"]}}}\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (some #(re-find #":test" %) (warnings r)))
    (is (= {:extra-paths ["test"]}
           (get (get (file-edn ws "modules/m/deps.edn") :aliases) :test)))))

(deftest s3p-exec-args-become-id-based-publish
  (let [ws (temp-ws {"deps.edn" root-with-managed
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib m/lib\n :mvn/repos {\"exoscale\" {:url \"https://artifacts.example\"}}\n :slipset.deps-deploy/exec-args {:repository {\"releases\" {:url \"s3p://bucket/prefix\"}}\n                                 :installer :remote\n                                 :sign-releases? false}}\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (let [d (file-edn ws "modules/m/deps.edn")]
      (is (= {:repo "bucket"} (get d :rig/publish)))
      (is (= {:url "s3p://bucket/prefix"} (get (get d :mvn/repos) "bucket")))
      (is (= {:url "https://artifacts.example"} (get (get d :mvn/repos) "exoscale")))
      (is (nil? (get d :slipset.deps-deploy/exec-args))))))

(deftest s3p-url-trailing-slash-is-normalized
  (let [ws (temp-ws {"deps.edn" root-with-managed
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib m/lib\n :slipset.deps-deploy/exec-args {:repository {\"releases\" {:url \"s3p://bucket/prefix/\"}}\n                                           :sign-releases? false}}\n"})]
    (is (empty? (problems (run ws))))
    (is (= "s3p://bucket/prefix"
           (get-in (file-edn ws "modules/m/deps.edn")
                   [:mvn/repos "bucket" :url])))))

(deftest sign-releases-true-is-a-problem
  (let [text "{:exoscale.project/lib m/lib :slipset.deps-deploy/exec-args {:repository \"any\" :sign-releases? true}}\n"
        ws (temp-ws {"deps.edn" root-with-managed
                     "modules/m/deps.edn" text})
        r (run ws)]
    (is (some #(re-find #"sign-releases" %) (problems r)))
    (is (= text (file-text ws "modules/m/deps.edn")))))

(deftest string-repository-uses-the-id-directly
  (let [ws (temp-ws {"deps.edn" root-with-managed
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib m/lib :slipset.deps-deploy/exec-args {:repository \"my-repo\"}}\n"})]
    (is (empty? (problems (run ws))))
    (let [d (file-edn ws "modules/m/deps.edn")]
      (is (= {:repo "my-repo"} (get d :rig/publish)))
      (is (nil? (get d :mvn/repos))))))

(deftest url-repository-matches-an-existing-repo
  (let [ws (temp-ws {"deps.edn" root-with-managed
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib m/lib\n :mvn/repos {\"exoscale\" {:url \"https://artifacts.example\"}}\n :slipset.deps-deploy/exec-args {:repository {\"releases\" {:url \"https://artifacts.example\"}}}}\n"})]
    (is (empty? (problems (run ws))))
    (let [d (file-edn ws "modules/m/deps.edn")]
      (is (= {:repo "exoscale"} (get d :rig/publish)))
      (is (= {"exoscale" {:url "https://artifacts.example"}} (get d :mvn/repos))))))

(deftest url-repository-derives-a-slugified-id
  (let [ws (temp-ws {"deps.edn" root-with-managed
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib m/lib\n :slipset.deps-deploy/exec-args {:repository {\"releases\" {:url \"https://artifacts.example.com/m2\"}}}}\n"})]
    (is (empty? (problems (run ws))))
    (let [d (file-edn ws "modules/m/deps.edn")]
      (is (= {:repo "artifacts-example-com"} (get d :rig/publish)))
      (is (= {:url "https://artifacts.example.com/m2"}
             (get (get d :mvn/repos) "artifacts-example-com"))))))

(deftest installer-local-produces-a-warning
  (let [ws (temp-ws {"deps.edn" root-with-managed
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib m/lib :slipset.deps-deploy/exec-args {:repository \"my-repo\" :installer :local}}\n"})]
    (let [r (run ws)]
      (is (some #(re-find #"(?i)rig install" %) (warnings r)))
      (is (= {:repo "my-repo"} (get (file-edn ws "modules/m/deps.edn") :rig/publish))))))

(deftest dry-run-reports-edits-without-writing
  (let [text "{:exoscale.project/lib x/y}\n"
        ws (temp-ws {"deps.edn" text})
        r (run ws :dry-run? true)]
    (is (= "deps.edn" (get-in r ["edits" 0 "file"])))
    (is (= true (get-in r ["edits" 0 "changed"])))
    (is (= text (file-text ws "deps.edn")))))

(deftest problems-block-all-writes
  (let [ws (temp-ws {"deps.edn" root-with-managed
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib m/lib :exoscale.project/version-fn \"zz/bad\"}\n"
                     "modules/n/deps.edn"
                     "{:exoscale.project/lib n/lib}\n"})
        n-text "{:exoscale.project/lib n/lib}\n"
        r (run ws)]
    (is (seq (problems r)))
    (is (= n-text (file-text ws "modules/n/deps.edn")))))

(deftest clean-manifest-is-reported-unchanged
  (let [text "{:rig/lib x/y :paths [\"src\"] :deps {a/b {:mvn/version \"1\"}}}\n"
        ws (temp-ws {"deps.edn" text})
        r (run ws)]
    (is (empty? (problems r)))
    (is (empty? (warnings r)))
    (is (= false (get-in r ["edits" 0 "changed"])))
    (is (= text (file-text ws "deps.edn")))))

(deftest unmapped-legacy-keys-are-dropped-with-a-warning
  (let [ws (temp-ws {"deps.edn" "{:exoscale.project/lib x/y :antq.core/something 1}\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (some #(re-find #"dropped :antq.core/something" %) (warnings r)))
    (is (nil? (get (file-edn ws "deps.edn") :antq.core/something)))))

(deftest comments-are-preserved
  (let [ws (temp-ws {"deps.edn"
                     "{:exoscale.project/lib x/y ;; the lib\n :deps\n {keep/dep {:mvn/version \"1.0.0\"} ;; keep me\n   a/b {:exoscale.deps/inherit :all}}\n :exoscale.deps/managed-dependencies {a/b {:mvn/version \"2.0.0\"}}}\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (let [text (file-text ws "deps.edn")]
      (is (re-find #"the lib" text))
      (is (re-find #"keep me" text)))
    (is (= {:mvn/version "2.0.0"}
           (get (get (file-edn ws "deps.edn") :deps) 'a/b)))))
