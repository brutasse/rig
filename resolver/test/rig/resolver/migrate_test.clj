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

(deftest rig-deps-preserves-managed-formatting
  "Regression: the renamed managed-dependencies node must survive the
  new-key pass; it used to be re-serialized compact on a single line."
  (let [ws (temp-ws {"deps.edn"
                     "{:exoscale.project/lib x/y\n :exoscale.deps/managed-dependencies\n {a/b {:mvn/version \"1.0.0\"}\n  c/d {:local/root \"modules/m\"}}}\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (let [text (file-text ws "deps.edn")
          d (file-edn ws "deps.edn")]
      (is (= {'a/b {:mvn/version "1.0.0"} 'c/d {:local/root "modules/m"}}
             (get d :rig/deps)))
      (is (re-find #":rig/deps\n \{" text))
      (is (not (re-find #":rig/deps \{.*\}\}" text))))))

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

(deftest managed-inherit-marker-does-not-leak-into-migrated-manifests
  "Regression: a managed entry's own :exoscale.deps/inherit marker used to
  leak into the materialized module deps and the renamed :rig/deps; a
  migrated manifest must carry no legacy-ns key anywhere."
  (let [ws (temp-ws {"deps.edn"
                     "{:exoscale.project/lib x/y :exoscale.project/modules [\"modules/m\"]\n :exoscale.deps/managed-dependencies {a/b {:exoscale.deps/inherit :all :mvn/version \"1.0.0\"}}}\n"
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib m/lib :deps {a/b {:exoscale.deps/inherit :all}}}\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (= {:mvn/version "1.0.0"}
           (get-in (file-edn ws "modules/m/deps.edn") [:deps 'a/b])))
    (is (= {:mvn/version "1.0.0"}
           (get-in (file-edn ws "deps.edn") [:rig/deps 'a/b])))
    (is (not (re-find #"exoscale.deps/inherit" (file-text ws "deps.edn"))))
    (is (not (re-find #"exoscale.deps/inherit" (file-text ws "modules/m/deps.edn"))))))

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
                     "{:exoscale.project/lib m/lib\n :mvn/repos {\"my-registry\" {:url \"https://artifacts.example\"}}\n :slipset.deps-deploy/exec-args {:repository {\"releases\" {:url \"s3p://bucket/prefix\"}}\n                                 :installer :remote\n                                 :sign-releases? false}}\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (let [d (file-edn ws "modules/m/deps.edn")]
      (is (= {:repo "bucket"} (get d :rig/publish)))
      (is (= {:url "s3p://bucket/prefix"} (get (get d :mvn/repos) "bucket")))
      (is (= {:url "https://artifacts.example"} (get (get d :mvn/repos) "my-registry")))
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
                     "{:exoscale.project/lib m/lib\n :mvn/repos {\"my-registry\" {:url \"https://artifacts.example\"}}\n :slipset.deps-deploy/exec-args {:repository {\"releases\" {:url \"https://artifacts.example\"}}}}\n"})]
    (is (empty? (problems (run ws))))
    (let [d (file-edn ws "modules/m/deps.edn")]
      (is (= {:repo "my-registry"} (get d :rig/publish)))
      (is (= {"my-registry" {:url "https://artifacts.example"}} (get d :mvn/repos))))))

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

(deftest second-run-is-a-no-op
  "Regression: a migrated tree is a fixed point; re-running used to strip
  re-attached markers and rewrite files with bogus warnings."
  (let [ws (temp-ws {"deps.edn"
                     "{:exoscale.project/lib x/y :exoscale.project/modules [\"modules/m\"]\n :exoscale.deps/managed-dependencies {a/b {:exoscale.deps/inherit :all :mvn/version \"1.0.0\"}}}\n"
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib m/lib :deps {a/b {:exoscale.deps/inherit :all}}}\n"})
        r1 (run ws)
        before (into {} (for [f ["deps.edn" "modules/m/deps.edn"]] [f (file-text ws f)]))
        r2 (run ws)]
    (is (empty? (problems r1)))
    (is (empty? (problems r2)))
    (is (empty? (warnings r2)))
    (is (every? false? (map #(get % "changed") (get r2 "edits"))))
    (doseq [[f t] before]
      (is (= t (file-text ws f))))))

;; --- leiningen (project.clj) ---

(def sample-project
  "(def version (.trim (try (slurp \"VERSION\") (catch Exception _ \"1.0.0-SNAPSHOT\"))))
(defproject com.example/example version
 :deploy-repositories [[\"releases\" {:url \"s3p://my-bucket/releases\" :no-auth true :sign-releases false}]]
 :repositories {\"my-registry\" {:url \"https://registry.example.com\"}}
 :profiles {:test {:plugins [[lein-test-report-junit-xml \"0.2.0\"]]}
            :dev {:jvm-opts [\"-Dexample.debug=true\"]
                  :resource-paths [\"test/resources\"]
                  :dependencies [[lambdaisland/kaocha \"1.0.669\"]
                                 [lambdaisland/deep-diff2 \"2.14.235\"]]
                  :aliases {\"kaocha\" [\"with-profile\" \"+dev\" \"run\" \"-m\" \"kaocha.runner\"]}}
            :docgen {:dependencies [[codox \"0.10.8\"]]}
            :uberjar {:aot :all}
            :graalvm {:native-image {:name \"example\"}}}
 :main ^:skip-aot example.main
 :dependencies [[org.clojure/clojure \"1.12.1\"]
                [aero \"1.1.6\"]
                [x/y \"0.1.0\" :exclusions [z/w]]]
 :plugins [[some/deploy-wagon \"1.0.2\"]]
 :resource-paths [\"resources\"])")

(deftest leiningen-project-clj-migrates
  (let [ws (temp-ws {"project.clj" sample-project})
        r (run ws)]
    (is (empty? (problems r)))
    (is (= "deps.edn" (get-in r ["edits" 0 "file"])))
    (is (= true (get-in r ["edits" 0 "changed"])))
    (let [d (file-edn ws "deps.edn")]
      (is (= 'com.example/example (get d :rig/lib)))
      (is (= 'example.main (get d :rig/main)))
      (is (nil? (get d :rig/version)))
      (is (nil? (get d :rig/version-file)))
      (is (= true (get d :rig/uberjar?)))
      (is (= {:repo "my-bucket"} (get d :rig/publish)))
      (is (= "s3p://my-bucket/releases" (get-in d [:mvn/repos "my-bucket" :url])))
      (is (= "https://registry.example.com" (get-in d [:mvn/repos "my-registry" :url])))
      (is (= ["src" "resources"] (get d :paths)))
      (is (nil? (get d :rig/src-dirs)))
      (is (= {:mvn/version "1.12.1"} (get-in d [:deps 'org.clojure/clojure])))
      (is (= {:mvn/version "1.1.6"} (get-in d [:deps 'aero/aero])))
      (is (= {:mvn/version "0.1.0" :exclusions '[z/w]} (get-in d [:deps 'x/y])))
      (is (= 'kaocha.runner/exec-fn (get-in d [:aliases :test :exec-fn])))
      ;; 1.0.669 predates kaocha.runner/exec-fn, so :test gets the rig pin;
      ;; :dev keeps the project's own version (no exec-fn there).
      (is (= "1.66.1034" (get-in d [:aliases :test :extra-deps 'lambdaisland/kaocha :mvn/version])))
      ;; lein runs tests with :dev active, so :test layers over :dev
      (is (= "2.14.235" (get-in d [:aliases :test :extra-deps 'lambdaisland/deep-diff2 :mvn/version])))
      (is (= ["test/resources" "test"] (get-in d [:aliases :test :extra-paths])))
      (is (= ["-Dexample.debug=true"] (get-in d [:aliases :test :jvm-opts])))
      (is (= "1.0.669" (get-in d [:aliases :dev :extra-deps 'lambdaisland/kaocha :mvn/version])))
      (is (= "2.14.235" (get-in d [:aliases :dev :extra-deps 'lambdaisland/deep-diff2 :mvn/version])))
      (is (= ["test/resources"] (get-in d [:aliases :dev :extra-paths])))
      (is (= ["-Dexample.debug=true"] (get-in d [:aliases :dev :jvm-opts])))
      (is (nil? (get-in d [:aliases :docgen])))
      (is (nil? (get-in d [:aliases :graalvm])))
      (is (nil? (get d :plugins)))
      (is (nil? (get d :profiles))))
    (is (some #(re-find #":docgen" %) (warnings r)))
    (is (some #(re-find #":graalvm" %) (warnings r)))
    (is (some #(re-find #":plugins" %) (warnings r)))
    (is (some #(re-find #":uberjar" %) (warnings r)))
    (is (some #(re-find #"predates kaocha.runner/exec-fn" %) (warnings r)))))

(deftest leiningen-version-literal
  (let [ws (temp-ws {"project.clj" "(defproject foo/bar \"1.2.3\" :main foo.main)\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (= "1.2.3" (get (file-edn ws "deps.edn") :rig/version)))))

(deftest leiningen-version-var-slurps-a-file
  (let [ws (temp-ws {"project.clj" "(def version (slurp \"VER\"))\n(defproject foo/bar version)\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (= "VER" (get (file-edn ws "deps.edn") :rig/version-file)))))

(deftest leiningen-version-var-uninterpretable
  (let [ws (temp-ws {"project.clj" "(def version (fetch-from-ci))\n(defproject foo/bar version)\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (some #(re-find #"fetch-from-ci" %) (warnings r)))
    (let [d (file-edn ws "deps.edn")]
      (is (nil? (get d :rig/version)))
      (is (nil? (get d :rig/version-file))))))

(deftest leiningen-sign-releases-is-a-problem
  (let [text "(defproject foo/bar \"1.0\"
 :deploy-repositories [[\"releases\" {:url \"s3p://bkt\" :sign-releases true}]])\n"
        ws (temp-ws {"project.clj" text})
        r (run ws)]
    (is (some #(re-find #"sign-releases" %) (problems r)))
    (is (empty? (get r "edits")))
    (is (false? (.exists (io/file ws "deps.edn"))))))

(deftest leiningen-test-alias-uses-the-rig-kaocha-pin
  (let [ws (temp-ws {"project.clj" "(defproject foo/bar \"1.0\")\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (= "1.66.1034" (get-in (file-edn ws "deps.edn")
                               [:aliases :test :extra-deps 'lambdaisland/kaocha :mvn/version])))))

(deftest leiningen-kaocha-pin-new-enough-is-kept
  (let [ws (temp-ws {"project.clj" "(defproject foo/bar \"1.0\"
 :profiles {:dev {:dependencies [[lambdaisland/kaocha \"1.0.937\"]]}})\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (every? #(not (re-find #"predates kaocha" %)) (warnings r)))
    (is (= "1.0.937" (get-in (file-edn ws "deps.edn")
                             [:aliases :test :extra-deps 'lambdaisland/kaocha :mvn/version])))))

(deftest leiningen-non-numeric-kaocha-version-is-kept
  (let [ws (temp-ws {"project.clj" "(defproject foo/bar \"1.0\"
 :profiles {:dev {:dependencies [[lambdaisland/kaocha \"RELEASE\"]]}})\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (every? #(not (re-find #"predates kaocha" %)) (warnings r)))
    (is (= "RELEASE" (get-in (file-edn ws "deps.edn")
                             [:aliases :test :extra-deps 'lambdaisland/kaocha :mvn/version])))))

(deftest leiningen-test-alias-layers-over-dev
  (let [ws (temp-ws {"project.clj" "(defproject foo/bar \"1.0\"
 :profiles {:dev {:jvm-opts [\"-Da=1\"]
                  :resource-paths [\"extra-res\"]
                  :dependencies [[org.foo/helper \"1.2.3\"]]}
            :test {:jvm-opts [\"-Db=2\"]}})\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (let [d (file-edn ws "deps.edn")]
      (is (= "1.2.3" (get-in d [:aliases :test :extra-deps 'org.foo/helper :mvn/version])))
      (is (= ["extra-res" "test"] (get-in d [:aliases :test :extra-paths])))
      (is (= ["-Da=1" "-Db=2"] (get-in d [:aliases :test :jvm-opts])))
      (is (= ["-Da=1"] (get-in d [:aliases :dev :jvm-opts]))))))

(deftest leiningen-local-root-and-git-deps
  (let [ws (temp-ws {"project.clj"
                     "(defproject foo/bar \"1.0\"
 :dependencies [[sub/proj :local/root \"modules/sub\"]
                [git/lib \"0.0.1\" :git/url \"https://git.example/git/lib\" :git/sha \"abc123\"]])\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (let [d (file-edn ws "deps.edn")]
      (is (= {:local/root "modules/sub"} (get-in d [:deps 'sub/proj])))
      (is (= {:git/sha "abc123" :git/url "https://git.example/git/lib"}
             (get-in d [:deps 'git/lib]))))))

(deftest leiningen-vector-repositories
  (let [ws (temp-ws {"project.clj"
                     "(defproject foo/bar \"1.0\"
 :repositories [[\"my-registry\" \"https://artifacts.example\"]])\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (= {:url "https://artifacts.example"}
           (get-in (file-edn ws "deps.edn") [:mvn/repos "my-registry"])))))

(deftest leiningen-extra-deploy-repos-are-dropped-with-a-warning
  (let [ws (temp-ws {"project.clj"
                     "(defproject foo/bar \"1.0\"
 :deploy-repositories [[\"snapshots\" {:url \"s3p://bkt/snap\"}]
                       [\"releases\" {:url \"s3p://bkt/rel\"}]])\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (some #(re-find #"(?i)first :deploy-repositories" %) (warnings r)))))

(deftest leiningen-dry-run-writes-nothing
  (let [ws (temp-ws {"project.clj" "(defproject foo/bar \"1.0\")\n"})
        r (run ws :dry-run? true)]
    (is (empty? (problems r)))
    (is (= true (get-in r ["edits" 0 "changed"])))
    (is (false? (.exists (io/file ws "deps.edn"))))))

(deftest leiningen-no-defproject-is-a-problem
  (let [ws (temp-ws {"project.clj" "(ns foo)\n"})
        r (run ws)]
    (is (some #(re-find #"defproject" %) (problems r)))
    (is (false? (.exists (io/file ws "deps.edn"))))))

(deftest leiningen-second-run-is-a-no-op
  "The second run takes the deps.edn path (the generated manifest is
  clean) and must be a fixed point."
  (let [ws (temp-ws {"project.clj" "(defproject foo/bar \"1.0\" :main foo.main)\n"})
        r1 (run ws)
        r2 (run ws)]
    (is (empty? (problems r1)))
    (is (empty? (problems r2)))
    (is (empty? (warnings r2)))
    (is (every? false? (map #(get % "changed") (get r2 "edits"))))))
