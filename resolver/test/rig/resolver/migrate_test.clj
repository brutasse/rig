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

(deftest version-and-javac-opts-are-renamed
  (let [ws (temp-ws {"deps.edn"
                     "{:exoscale.project/lib x/y\n :exoscale.project/version \"1.2.3\"\n :exoscale.project/javac-opts [\"-source\" \"11\" \"-target\" \"11\"]}\n"})]
    (let [r (run ws)]
      (is (empty? (problems r)))
      (let [d (file-edn ws "deps.edn")]
        (is (= "1.2.3" (get d :rig/version)))
        (is (= ["-source" "11" "-target" "11"] (get d :rig/javac-opts)))
        (is (nil? (get d :exoscale.project/version)))
        (is (nil? (get d :exoscale.project/javac-opts)))))))

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

(deftest root-managed-dependencies-are-dropped-not-carried-as-rig-deps
  "The pool is materialized into the modules' :deps, not carried as a
  :rig/deps shared requirement: an inert pin (no module declares it) stays
  inert and cannot become a stale-lock."
  (let [ws (temp-ws {"deps.edn"
                     "{:exoscale.project/lib x/y\n :exoscale.deps/managed-dependencies {a/b {:mvn/version \"1.0.0\"}}}"})]
    (let [r (run ws)]
      (is (empty? (problems r)))
      (is (some #(re-find #"managed-dependencies dropped" %) (warnings r)))
      (let [d (file-edn ws "deps.edn")]
        (is (nil? (get d :rig/deps)))
        (is (nil? (get d :exoscale.deps/managed-dependencies)))))))
(deftest managed-namespaced-maps-are-expanded-to-plain-form
  "The compact `#:mvn{...}` reader-macro form in the pool materializes into
  module deps as the standard `{:mvn/version ...}` notation, so the
  migrated manifest uses plain requirement maps rather than the legacy
  compact form."
  (let [ws (temp-ws {"deps.edn"
                     "{:exoscale.project/lib x/y\n :exoscale.project/modules [\"modules/m\"]\n :exoscale.deps/managed-dependencies {a/b #:mvn{:version \"1.0.0\"}\n                                                 c/d #:mvn{:version \"2.0.0\" :classifier \"sources\"}}}"
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib m/lib :deps {a/b {:exoscale.deps/inherit :all}\n                                              c/d {:exoscale.deps/inherit :all}}}"})
         r (run ws)]
    (is (empty? (problems r)))
    (let [text (file-text ws "modules/m/deps.edn")
          d (file-edn ws "modules/m/deps.edn")]
      (is (= {:mvn/version "1.0.0"} (get (get d :deps) 'a/b)))
      (is (= {:mvn/version "2.0.0" :mvn/classifier "sources"} (get (get d :deps) 'c/d)))
      (is (nil? (re-find (re-pattern "#:") text))))))
(deftest managed-dependencies-at-module-level-are-a-problem
  (let [ws (temp-ws {"deps.edn"
                     "{:exoscale.project/lib x/y :exoscale.project/modules [\"modules/m\"]}\n"
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib m/lib\n :exoscale.deps/managed-dependencies {a/b {:mvn/version \"1\"}}}\n"})
        r (run ws)]
    (is (some #(re-find #"managed-dependencies" %) (problems r)))
    (is (re-find #"exoscale" (file-text ws "modules/m/deps.edn")))))

(deftest inherit-all-materializes-managed-values
  "The managed entry fills in the keys the module did not declare
  (exclusions); a declared version wins over the managed one (a repo whose
  CI never ran merge-deps resolved the declared version), reported as a
  drift warning."
  (let [ws (temp-ws {"deps.edn" root-with-managed
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib m/lib\n :deps {a/b {:mvn/version \"0.9.0\" :exoscale.deps/inherit :all}\n                   plain/p {:mvn/version \"2.0.0\"}}}"})
         r (run ws)]
    (is (empty? (problems r)))
    (is (some #(re-find #"a/b: :mvn/version \"0\.9\.0\" \(managed \"1\.0\.0\"\)" %) (warnings r)))
    (let [d (file-edn ws "modules/m/deps.edn")]
      (is (= {:mvn/version "0.9.0" :exclusions '[x/y]}
             (get (get d :deps) 'a/b)))
      (is (= {:mvn/version "2.0.0"} (get (get d :deps) 'plain/p))))))

(deftest inherit-dep-with-matching-version-is-not-drift
  (let [ws (temp-ws {"deps.edn"
                     "{:exoscale.project/lib x/y\n :exoscale.project/modules [\"modules/m\"]\n :exoscale.deps/managed-dependencies {a/b {:mvn/version \"1.0.0\"}}}"
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib m/lib :deps {a/b {:mvn/version \"1.0.0\" :exoscale.deps/inherit :all}}}"})
         r (run ws)]
    (is (empty? (problems r)))
    (is (= {:mvn/version "1.0.0"}
           (get-in (file-edn ws "modules/m/deps.edn") [:deps 'a/b])))
    (is (nil? (some #(re-find #"declared kept" %) (warnings r))))))
(deftest managed-inherit-marker-does-not-leak-into-migrated-manifests
  "Regression: a managed entry's own :exoscale.deps/inherit marker used to
  leak into the materialized module deps; a migrated manifest must carry
  no legacy-ns key anywhere."
  (let [ws (temp-ws {"deps.edn"
                     "{:exoscale.project/lib x/y :exoscale.project/modules [\"modules/m\"]\n :exoscale.deps/managed-dependencies {a/b {:exoscale.deps/inherit :all :mvn/version \"1.0.0\"}}}"
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib m/lib :deps {a/b {:exoscale.deps/inherit :all}}}"})
         r (run ws)]
    (is (empty? (problems r)))
    (is (= {:mvn/version "1.0.0"}
           (get-in (file-edn ws "modules/m/deps.edn") [:deps 'a/b])))
    (is (not (re-find #"exoscale.deps/inherit" (file-text ws "deps.edn"))))
    (is (not (re-find #"exoscale.deps/inherit" (file-text ws "modules/m/deps.edn"))))))
(deftest materialized-dep-with-extra-keyword-pair-stays-valid
  "Regression: the value node of a materialized dep map is built pair by
  pair; without a separator between pairs a keyword-valued pair glues the
  next key (":else" + ":mvn/version" -> ":else:mvn/version"), leaving an
  odd-form map the EDN reader rejects. The declared version is kept
  (declared wins over the managed one)."
  (let [ws (temp-ws {"deps.edn"
                     "{:exoscale.project/lib x/y\n :exoscale.project/modules [\"modules/m\"]\n :exoscale.deps/managed-dependencies {a/b {:mvn/version \"1.0.0\"}}}"
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib x/m\n :deps {a/b {:exoscale.deps/inherit :all\n                      :mvn/version \"0.9.0\"\n                      :something :else}}}"})
         r (run ws)]
    (is (empty? (problems r)))
    (is (= {:mvn/version "0.9.0" :something :else}
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

(deftest declared-local-root-is-kept-module-relative
  (let [ws (temp-ws {"deps.edn"
                     "{:exoscale.project/lib x/y\n :exoscale.project/modules [\"modules/m\" \"modules/n\" \"modules/p\"]\n :exoscale.deps/managed-dependencies {c/d {:local/root \"modules/m\"}}}\n"
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib m/lib}\n"
                     "modules/n/deps.edn"
                     "{:exoscale.project/lib n/lib :deps {c/d {:local/root \"../m\" :exoscale.deps/inherit :all}}}\n"
                     "modules/p/deps.edn"
                     "{:exoscale.project/lib p/lib :deps {c/d {:local/root \"elsewhere\" :exoscale.deps/inherit :all}}}\n"})
        r (run ws)]
    (is (empty? (problems r)))
    ;; a declared module-relative :local/root pointing at the pool target is
    ;; kept as-is (not re-canonicalized) and is not drift
    (is (= "../m" (get-in (file-edn ws "modules/n/deps.edn") [:deps 'c/d :local/root])))
    (is (not (some #(re-find #"modules/n.*dep c/d" %) (warnings r))))
    ;; a declared :local/root pointing elsewhere wins over the pool, with drift
    (is (= "elsewhere"
           (get-in (file-edn ws "modules/p/deps.edn") [:deps 'c/d :local/root])))
    (is (some #(re-find #"modules/p.*dep c/d: :local/root \"elsewhere\" \(managed \"\.\./m\"\)" %)
              (warnings r)))))

(deftest root-listed-in-modules-is-migrated-once
  (let [ws (temp-ws {"deps.edn"
                     "{:exoscale.project/lib x/y\n :exoscale.project/modules [\".\" \"modules/m\"]\n :exoscale.deps/managed-dependencies {a/b {:mvn/version \"1.0.0\"}}\n :deps {a/b {:exoscale.deps/inherit :all}}}\n"
                     "modules/m/deps.edn"
                     "{:exoscale.project/lib m/lib :deps {a/b {:exoscale.deps/inherit :all}}}\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (= 1 (count (filter #(re-find #"managed-dependencies dropped" %) (warnings r)))))))

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

(deftest s3p-repo-is-a-problem
  "An s3p:// deploy url has no rig equivalent (rig publishes to http/https
  only): the migration is blocked and nothing is written."
  (let [text "{:exoscale.project/lib m/lib\n :slipset.deps-deploy/exec-args {:repository {\"releases\" {:url \"s3p://bucket/prefix\"}}\n                                 :installer :remote\n                                 :sign-releases? false}}\n"
        ws (temp-ws {"deps.edn" root-with-managed
                     "modules/m/deps.edn" text})
        r (run ws)]
    (is (some #(re-find #"s3p://bucket/prefix" %) (problems r)))
    (is (some #(re-find #"http/https" %) (problems r)))
    (is (= text (file-text ws "modules/m/deps.edn")))))

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
 :deploy-repositories [[\"releases\" {:url \"https://repo.example.com/m2\" :no-auth true :sign-releases false}]]
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
      (is (= {:repo "repo-example-com"} (get d :rig/publish)))
      (is (= "https://repo.example.com/m2" (get-in d [:mvn/repos "repo-example-com" :url])))
      (is (= "https://registry.example.com" (get-in d [:mvn/repos "my-registry" :url])))
      (is (= ["src" "resources"] (get d :paths)))
      (is (nil? (get d :rig/artifact-dirs)))
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

(deftest lein-plain-source-paths-migrate
  ;; A plain :source-paths vector replaces the leiningen default (top-level
  ;; ^:top-displace semantics), so the migration must keep src/clj on the
  ;; classpath and pin the build dirs to match, instead of silently
  ;; falling back to src/.
  (let [text "(defproject com.example/plain-paths \"0.1.0\"
              :source-paths [\"src/clj\"]
              :dependencies [[org.clojure/clojure \"1.12.1\"]])"
        ws (temp-ws {"project.clj" text})
        r (run ws)]
    (is (empty? (problems r)))
    (is (empty? (warnings r)))
    (let [d (file-edn ws "deps.edn")]
      (is (= ["src/clj" "resources"] (get d :paths)))
      (is (= ["src/clj" "resources"] (get d :rig/artifact-dirs))))))

(deftest lein-dev-only-warnings-do-not-blame-the-test-profile
  ;; The effective :test profile is :dev merged under :test (lein applies
  ;; :dev to the test task), but a workspace with no :test profile must not
  ;; get "profile :test dropped ..." warnings for keys it never wrote.
  (let [text "(defproject com.example/only-dev \"0.1.0\"\n              :dependencies [[org.clojure/clojure \"1.12.1\"]]\n              :profiles {:dev {:global-vars {*assert* true}\n                               :plugins [[lein-cljfmt \"0.6.4\"]]}})\n"
        ws (temp-ws {"project.clj" text})
        r (run ws)]
    (is (empty? (problems r)))
    (is (not (some #(re-find #"profile :test" %) (warnings r)))
        (pr-str (warnings r)))
    (is (some #(re-find #"profile :dev dropped :global-vars" %) (warnings r))
        (pr-str (warnings r)))))

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
 :deploy-repositories [[\"releases\" {:url \"https://bkt.example\" :sign-releases true}]])\n"
        ws (temp-ws {"project.clj" text})
        r (run ws)]
    (is (some #(re-find #"sign-releases" %) (problems r)))
    (is (empty? (get r "edits")))
    (is (false? (.exists (io/file ws "deps.edn"))))))

(deftest leiningen-s3p-deploy-repo-is-a-problem
  (let [text "(defproject foo/bar \"1.0\"
 :deploy-repositories [[\"releases\" {:url \"s3p://bkt/rel\"}]])\n"
        ws (temp-ws {"project.clj" text})
        r (run ws)]
    (is (some #(re-find #":deploy-repositories s3p://bkt/rel" %) (problems r)))
    (is (some #(re-find #"http/https" %) (problems r)))
    (is (empty? (get r "edits")))
    (is (false? (.exists (io/file ws "deps.edn"))))))

(deftest leiningen-clojars-shorthand-emits-the-rig-clojars-repo
  (let [ws (temp-ws {"project.clj" "(defproject foo/bar \"1.0\"\n  :deploy-repositories [[\"releases\" :clojars]])\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (let [d (file-edn ws "deps.edn")]
      (is (= {:repo "clojars"} (get d :rig/publish)))
      (is (nil? (get d :mvn/repos))))))

(deftest leiningen-clojars-shorthand-map-form
  (let [ws (temp-ws {"project.clj" "(defproject foo/bar \"1.0\"\n  :deploy-repositories {\"releases\" :clojars})\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (= {:repo "clojars"} (get (file-edn ws "deps.edn") :rig/publish)))))

(deftest leiningen-clojars-shorthand-more-entries-are-a-warning
  (let [ws (temp-ws {"project.clj" "(defproject foo/bar \"1.0\"\n  :deploy-repositories [[\"releases\" :clojars] [\"snapshots\" :clojars]])\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (= {:repo "clojars"} (get (file-edn ws "deps.edn") :rig/publish)))
    (is (some #(re-find #"(?i)first :deploy-repositories" %) (warnings r)))))

(deftest leiningen-clojars-shorthand-reuses-the-fetch-repo-id
  (let [ws (temp-ws {"project.clj" "(defproject foo/bar \"1.0\"\n  :repositories {\"clojars\" {:url \"https://repo.clojars.org/\"}}\n  :deploy-repositories [[\"releases\" :clojars]])\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (let [d (file-edn ws "deps.edn")]
      (is (= {:repo "clojars"} (get d :rig/publish)))
      (is (= "https://repo.clojars.org/"
             (get-in d [:mvn/repos "clojars" :url]))))))

(deftest leiningen-unknown-deploy-shorthand-is-a-problem
  (let [text "(defproject foo/bar \"1.0\"
  :deploy-repositories [[\"releases\" :nope]])\n"
         ws (temp-ws {"project.clj" text})
         r (run ws)]
    (is (some #(re-find #"not a spec map" %) (problems r)))
    (is (some #(re-find #":clojars" %) (problems r)))
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

(deftest leiningen-keyword-repository-ids-are-stringified
  (let [ws (temp-ws {"project.clj"
                    "(defproject foo/bar \"1.0\"
 :repositories {:exoscale {:url \"https://artifacts.example\"}})\n"})
         r (run ws)]
    (is (empty? (problems r)))
    (is (= {:url "https://artifacts.example"}
           (get-in (file-edn ws "deps.edn") [:mvn/repos "exoscale"])))
    (is (nil? (get-in (file-edn ws "deps.edn") [:mvn/repos :exoscale])))))

(deftest leiningen-extra-deploy-repos-are-dropped-with-a-warning
  (let [ws (temp-ws {"project.clj"
                     "(defproject foo/bar \"1.0\"
 :deploy-repositories [[\"snapshots\" {:url \"https://bkt.example/snap\"}]
                       [\"releases\" {:url \"https://bkt.example/rel\"}]])\n"})
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

;; --- lein :managed-dependencies / :parent-project (issue 04) ---

(deftest lein-managed-dependencies-materialize-bare-deps
  (let [ws (temp-ws {"project.clj"
                     "(defproject foo/bar \"1.0.0\"
 :managed-dependencies [[\"a/b\" \"1.0.0\"]
                        [\"c/d\" \"2.0.0\"]]
 :dependencies [[a/b]
                [c/d]
                [e/f \"9.9.9\"]])\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (some #(re-find #"(?i)transitive version constraints" %) (warnings r)))
    (let [d (file-edn ws "deps.edn")]
      (is (= {:mvn/version "1.0.0"} (get-in d [:deps 'a/b])))
      (is (= {:mvn/version "2.0.0"} (get-in d [:deps 'c/d])))
      (is (= {:mvn/version "9.9.9"} (get-in d [:deps 'e/f]))))))

(deftest lein-managed-version-token-materializes-the-project-version
  (let [ws (temp-ws {"project.clj"
                     "(defproject foo/bar \"1.2.3\"
 :managed-dependencies [[foo/sub :version]]
 :dependencies [[foo/sub]])\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (= {:mvn/version "1.2.3"}
           (get-in (file-edn ws "deps.edn") [:deps 'foo/sub])))))

(deftest lein-version-token-in-the-dep-vector
  (let [ws (temp-ws {"project.clj"
                     "(defproject foo/bar \"4.5.6\"
 :dependencies [[foo/bar :version]
                [a/b \"1.0\"]])\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (let [d (file-edn ws "deps.edn")]
      (is (= {:mvn/version "4.5.6"} (get-in d [:deps 'foo/bar])))
      (is (= {:mvn/version "1.0"} (get-in d [:deps 'a/b]))))))

(deftest lein-managed-pool-force-pins-a-declared-version
  "lein's :managed-dependencies force-pin even a dep that declares its own
  version — the managed version is the one the repo runs, so migrate
  materializes it (with a warning): keeping the declared one would leave
  the module's pin and the workspace requirement permanently apart."
  (let [ws (temp-ws {"project.clj"
                     "(defproject foo/bar \"1.0.0\"
 :managed-dependencies [[\"a/b\" \"1.0.0\"]]
 :dependencies [[a/b \"0.9.0\"]])\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (some #(re-find #"managed version wins" %) (warnings r)))
    (is (= {:mvn/version "1.0.0"}
           (get-in (file-edn ws "deps.edn") [:deps 'a/b])))))

(deftest lein-versionless-dep-keeps-its-own-exclusions
  (let [ws (temp-ws {"project.clj"
                     "(defproject foo/bar \"1.0.0\"
 :managed-dependencies [[\"a/b\" \"1.0.0\" :exclusions [x/y]]]
 :dependencies [[a/b :exclusions [z/w]]])\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (= {:mvn/version "1.0.0" :exclusions '[z/w]}
           (get-in (file-edn ws "deps.edn") [:deps 'a/b])))))

(deftest lein-bare-dep-inherits-the-pool-entry-exclusions
  (let [ws (temp-ws {"project.clj"
                     "(defproject foo/bar \"1.0.0\"
 :managed-dependencies [[\"a/b\" \"1.0.0\" :exclusions [x/y]]]
 :dependencies [[a/b]])\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (= {:mvn/version "1.0.0" :exclusions '[x/y]}
           (get-in (file-edn ws "deps.edn") [:deps 'a/b])))))

(deftest lein-bare-exclusion-symbols-get-qualified
  "lein's bare exclusion X means X/X; tools.deps matches exclusions by
  qualified lib, so copying the bare symbol verbatim made every such
  exclusion a silent no-op: lock prints 'change foo => foo/foo' per
  entry and excludes nothing."
  (let [ws (temp-ws {"project.clj"
                     "(defproject foo/bar \"1.0.0\"
 :managed-dependencies [[\"a/b\" \"1.0.0\" :exclusions [manifold]]]
 :dependencies [[a/b]
                [c/d \"2.0\" :exclusions [exemplar io.netty/netty]]])\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (let [d (file-edn ws "deps.edn")]
      (is (= {:mvn/version "1.0.0" :exclusions '[manifold/manifold]}
             (get-in d [:deps 'a/b]))
          "pool entry exclusion qualified")
      (is (= {:mvn/version "2.0" :exclusions '[exemplar/exemplar io.netty/netty]}
             (get-in d [:deps 'c/d]))
          "declared exclusions qualified, already-qualified kept"))))

(deftest lein-versionless-dep-without-pool-entry-is-a-problem
  (let [ws (temp-ws {"project.clj"
                     "(defproject foo/bar \"1.0.0\"
 :managed-dependencies [[\"c/d\" \"2.0.0\"]]
 :dependencies [[a/b]
                [c/d]])\n"})
        r (run ws)]
    (is (= 1 (count (problems r))))
    (is (some #(re-find #"declares no version" %) (problems r)))
    (is (false? (.exists (io/file ws "deps.edn"))))))

(deftest lein-managed-version-var-resolves-a-static-def
  (let [ws (temp-ws {"project.clj"
                    "(def sub-v \"1.0.0\")
 (defproject foo/bar \"1.0.0\"
 :managed-dependencies [[\"a/b\" ~sub-v]]
 :dependencies [[a/b]])\n"})
         r (run ws)]
    (is (empty? (problems r)))
    (is (= {:mvn/version "1.0.0"}
           (get-in (file-edn ws "deps.edn") [:deps 'a/b])))))

(deftest lein-managed-version-var-computed-is-a-problem
  (let [ws (temp-ws {"project.clj"
                    "(def sub-v (inc 0))
 (defproject foo/bar \"1.0.0\"
 :managed-dependencies [[\"a/b\" ~sub-v]]
 :dependencies [[a/b]])\n"})
         r (run ws)]
    (is (= 1 (count (problems r))))
    (is (some #(re-find #"lein-replace var that cannot be resolved statically" %) (problems r)))
    (is (some #(re-find #"sub-v" %) (problems r)))
    (is (false? (.exists (io/file ws "deps.edn"))))))

(deftest lein-version-var-resolves-a-static-def
  (let [ws (temp-ws {"project.clj"
                    "(def v \"1.0.0\")
 (defproject foo/bar \"1.0.0\"
 :dependencies [[\"a/b\" ~v]])\n"})
         r (run ws)]
    (is (empty? (problems r)))
    (is (= {:mvn/version "1.0.0"}
           (get-in (file-edn ws "deps.edn") [:deps 'a/b])))))

(deftest lein-version-var-with-exclusions
  (let [ws (temp-ws {"project.clj"
                    "(def v \"1.0.0\")
 (defproject foo/bar \"1.0.0\"
 :dependencies [[\"a/b\" ~v :exclusions [x/y]]])\n"})
         r (run ws)]
    (is (empty? (problems r)))
    (is (= {:mvn/version "1.0.0" :exclusions '[x/y]}
           (get-in (file-edn ws "deps.edn") [:deps 'a/b])))))

(deftest lein-profile-version-var-materializes-in-the-alias
  (let [ws (temp-ws {"project.clj"
                    "(def v \"1.0.0\")
 (defproject foo/bar \"1.0.0\"
 :dependencies []
 :profiles {:test {:dependencies [[\"a/b\" ~v]]}})\n"})
         r (run ws)]
    (is (empty? (problems r)))
    (is (= {:mvn/version "1.0.0"}
           (get-in (file-edn ws "deps.edn") [:aliases :test :extra-deps 'a/b])))))

(deftest lein-version-var-computed-is-a-problem
  (let [ws (temp-ws {"project.clj"
                    "(def v (inc 0))
 (defproject foo/bar \"1.0.0\"
 :dependencies [[\"a/b\" ~v]])\n"})
         r (run ws)]
    (is (= 1 (count (problems r))))
    (is (some #(re-find #"version var ~v cannot be resolved statically" %) (problems r)))
    (is (false? (.exists (io/file ws "deps.edn"))))))

(deftest lein-version-var-undefined-is-a-problem
  (let [ws (temp-ws {"project.clj"
                    "(defproject foo/bar \"1.0.0\"
 :dependencies [[\"a/b\" ~undefined-v]])\n"})
         r (run ws)]
    (is (= 1 (count (problems r))))
    (is (some #(re-find #"version var ~undefined-v cannot be resolved statically" %) (problems r)))
    (is (false? (.exists (io/file ws "deps.edn"))))))

(deftest lein-version-var-slurps-a-file
  (let [ws (temp-ws {"project.clj"
                    "(def v (slurp \"VERSION\"))
 (defproject foo/bar \"1.0.0\"
 :dependencies [[\"a/b\" ~v]])"
                    "VERSION" "2.5.0\n"})
         r (run ws)]
    (is (empty? (problems r)))
    (is (= {:mvn/version "2.5.0"}
           (get-in (file-edn ws "deps.edn") [:deps 'a/b])))))

(deftest lein-profile-deps-materialize-from-the-root-pool
  (let [ws (temp-ws {"project.clj"
                     "(defproject foo/bar \"1.0.0\"
 :managed-dependencies [[\"a/b\" \"1.0.0\"]]
 :dependencies []
 :profiles {:dev  {:dependencies [[a/b]]}
            :test {:dependencies [[a/b]]}})\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (let [d (file-edn ws "deps.edn")]
      (is (= {:mvn/version "1.0.0"} (get-in d [:aliases :dev :extra-deps 'a/b])))
      (is (= {:mvn/version "1.0.0"} (get-in d [:aliases :test :extra-deps 'a/b]))))))

(deftest lein-parent-managed-dependencies-inherit
  (let [ws (temp-ws {"parent.clj"
                     "(defproject foo/parent \"0.0.1\"
 :managed-dependencies [[\"a/b\" \"1.0.0\"]
                        [\"c/d\" \"2.0.0\"]])\n"
                     "project.clj"
                     "(defproject foo/bar \"1.0.0\"
 :parent-project {:path \"parent.clj\" :inherit [:managed-dependencies]}
 :managed-dependencies [[\"a/b\" \"3.0.0\"]]
 :dependencies [[a/b]
                [c/d]])\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (some #(re-find #"other inherited keys are dropped" %) (warnings r)))
    (let [d (file-edn ws "deps.edn")]
      ;; the manifest's own pin wins over the parent's
      (is (= {:mvn/version "3.0.0"} (get-in d [:deps 'a/b])))
      ;; the parent's pin is inherited
      (is (= {:mvn/version "2.0.0"} (get-in d [:deps 'c/d]))))))

(deftest lein-missing-parent-project-is-a-single-clear-problem
  (let [ws (temp-ws {"project.clj"
                     "(defproject foo/bar \"1.0.0\"
 :parent-project {:path \"../../project.clj\" :inherit [:managed-dependencies]}
 :dependencies [[a/b]
                [c/d]])\n"})
        r (run ws)]
    (is (= 1 (count (problems r))))
    (is (some #(re-find #":parent-project path" %) (problems r)))
    (is (false? (.exists (io/file ws "deps.edn"))))))

(deftest lein-managed-and-parent-keys-are-not-dropped
  (let [ws (temp-ws {"parent.clj"
                     "(defproject foo/parent \"0.0.1\")\n"
                     "project.clj"
                     "(defproject foo/bar \"1.0.0\"
 :managed-dependencies [[\"a/b\" \"1.0.0\"]]
 :parent-project {:path \"parent.clj\" :inherit [:managed-dependencies]}
 :dependencies [[a/b]])\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (= {:mvn/version "1.0.0"}
           (get-in (file-edn ws "deps.edn") [:deps 'a/b])))
    (is (not (some #(re-find #"no rig equivalent" %) (warnings r))))))

(deftest lein-java-source-paths-and-javac-options-migrate
  ;; lein :java-source-paths / :javac-options map to the existing
  ;; :rig/java-src-dirs / :rig/javac-opts (no new keys); they are no longer
  ;; reported as dropped.
  (let [text "(defproject com.example/java \"0.1.0\"
 :java-source-paths [\"src/java\"]
 :javac-options [\"-target\" \"1.8\" \"-source\" \"1.8\"]
 :dependencies [[org.clojure/clojure \"1.12.1\"]])"
        ws (temp-ws {"project.clj" text})
        r (run ws)]
    (is (empty? (problems r)))
    (let [d (file-edn ws "deps.edn")]
      (is (= ["src/java"] (get d :rig/java-src-dirs)))
      (is (= ["-target" "1.8" "-source" "1.8"] (get d :rig/javac-opts))))
    (is (not (some #(re-find #":java-source-paths|:javac-options" %)
                   (warnings r))))))

(deftest lein-uberjar-name-migrates-to-rig-uberjar-file
  ;; lein :uberjar-name maps to the existing :rig/uberjar-file, rooted at
  ;; target/; a name without a .jar suffix gets one (rig implies
  ;; :rig/uberjar? from a declared :rig/uberjar-file).
  (let [ws (temp-ws {"project.clj"
                     "(defproject com.example/uber \"0.1.0\"
 :uberjar-name \"metadump.jar\"
 :dependencies [[org.clojure/clojure \"1.12.1\"]])\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (= "target/metadump.jar"
           (get (file-edn ws "deps.edn") :rig/uberjar-file)))
    (is (not (some #(re-find #":uberjar-name" %) (warnings r)))))
  (let [ws (temp-ws {"project.clj"
                     "(defproject com.example/uber \"0.1.0\"
 :uberjar-name \"metadump\"
 :dependencies [[org.clojure/clojure \"1.12.1\"]])\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (= "target/metadump.jar"
           (get (file-edn ws "deps.edn") :rig/uberjar-file)))))

;; --- lein :sub monorepo (issue 05) ---

(def lein-sub-fixture
  {"project.clj"
   "(defproject foo/parent \"1.0.0\"
  :managed-dependencies [[\"a/b\" \"1.0.0\"]
                         [foo/m :version]
                         [foo/n :version]
                         [org.clojure/clojure \"1.12.1\"]]
  :deploy-repositories [[\"releases\" :clojars] [\"snapshots\" :clojars]]
  :sub [\"modules/m\" \"modules/n\"])"
   "modules/m/project.clj"
   "(defproject foo/m \"1.0.0\"
  :parent-project {:path \"../../project.clj\" :inherit [:managed-dependencies :deploy-repositories]}
  :source-paths [\"src/clj\"]
  :dependencies [[a/b]
                 [foo/n]
                 [org.clojure/clojure]])"
   "modules/n/project.clj"
   "(defproject foo/n \"1.0.0\"
  :parent-project {:path \"../../project.clj\" :inherit [:managed-dependencies]}
  :dependencies [[a/b]])"})

(deftest lein-sub-monorepo-migrates-root-and-modules
  (let [ws (temp-ws lein-sub-fixture)
        r (run ws)]
    (is (empty? (problems r)))
    (is (= ["deps.edn" "modules/m/deps.edn" "modules/n/deps.edn"]
           (map #(get % "file") (get r "edits"))))
    (is (every? true? (map #(get % "changed") (get r "edits"))))
    (is (some #(re-find #"first :deploy-repositories" %) (warnings r)))
    (is (= 2 (count (filter #(re-find #"inherited :managed-dependencies" %)
                            (warnings r)))))
    (is (every? #(not (re-find #"transitive version constraints" %)) (warnings r)))
    (is (every? #(not (re-find #"no rig equivalent" %)) (warnings r)))
    (let [root (file-edn ws "deps.edn")]
      (is (= 'foo/parent (get root :rig/lib)))
      (is (= "1.0.0" (get root :rig/version)))
      (is (= ["modules/m" "modules/n"] (get root :rig/modules)))
      (is (= {:local/root "modules/m"} (get-in root [:rig/deps 'foo/m])))
      (is (= {:local/root "modules/n"} (get-in root [:rig/deps 'foo/n])))
      (is (= {:mvn/version "1.0.0"} (get-in root [:rig/deps 'a/b])))
      (is (= {:mvn/version "1.12.1"}
             (get-in root [:rig/deps 'org.clojure/clojure])))
      (is (= {:repo "clojars"} (get root :rig/publish))))
    (let [m (file-edn ws "modules/m/deps.edn")]
      (is (= 'foo/m (get m :rig/lib)))
      (is (= "1.0.0" (get m :rig/version)))
      (is (= {:mvn/version "1.0.0"} (get-in m [:deps 'a/b])))
      (is (= {:local/root "../n"} (get-in m [:deps 'foo/n])))
      (is (= {:mvn/version "1.12.1"}
             (get-in m [:deps 'org.clojure/clojure])))
      (is (= ["src/clj" "resources"] (get m :paths)))
      (is (= ["src/clj" "resources"] (get m :rig/artifact-dirs)))
      (is (= "1.66.1034"
             (get-in m [:aliases :test :extra-deps
                        'lambdaisland/kaocha :mvn/version])))
      (is (nil? (get m :rig/publish)))
      (is (nil? (get m :rig/modules)))
      (is (nil? (get m :rig/deps))))
    (let [n (file-edn ws "modules/n/deps.edn")]
      (is (= 'foo/n (get n :rig/lib)))
      (is (= {:mvn/version "1.0.0"} (get-in n [:deps 'a/b]))))))

(deftest lein-sub-second-run-is-a-no-op
  (let [ws (temp-ws lein-sub-fixture)
        r1 (run ws)
        r2 (run ws)]
    (is (empty? (problems r1)))
    (is (empty? (problems r2)))
    (is (empty? (warnings r2)))
    (is (= 3 (count (get r2 "edits"))))
    (is (every? false? (map #(get % "changed") (get r2 "edits"))))))

(deftest lein-sub-missing-module-manifest-is-a-problem
  (let [ws (temp-ws {"project.clj"
                     "(defproject foo/parent \"1.0.0\"
  :sub [\"modules/m\" \"modules/n\"])"
        "modules/m/project.clj"
        "(defproject foo/m \"1.0.0\")"})
        r (run ws)]
    (is (= ["modules/n/project.clj: not found"] (problems r)))
    (is (empty? (get r "edits")))
    (is (false? (.exists (io/file ws "deps.edn"))))))

(deftest lein-sub-module-without-defproject-is-a-problem
  (let [ws (temp-ws {"project.clj"
                     "(defproject foo/parent \"1.0.0\" :sub [\"modules/m\"])"
        "modules/m/project.clj"
        "(ns foo.m)\n"})
        r (run ws)]
    (is (= ["modules/m/project.clj: no defproject form found"] (problems r)))
    (is (false? (.exists (io/file ws "deps.edn"))))))

(deftest lein-sub-versionless-dep-not-in-pool-blocks-all-writes
  (let [ws (temp-ws {"project.clj"
                     "(defproject foo/parent \"1.0.0\"
  :managed-dependencies [[\"a/b\" \"1.0.0\"]]
  :sub [\"modules/m\"])"
        "modules/m/project.clj"
        "(defproject foo/m \"1.0.0\"
  :parent-project {:path \"../../project.clj\" :inherit [:managed-dependencies]}
  :dependencies [[a/b]
                 [zz/z]])"})
        r (run ws)]
    (is (= ["modules/m/project.clj: dep zz/z declares no version, :local/root or :git/url (no :managed-dependencies entry)"]
           (problems r)))
    (is (false? (.exists (io/file ws "deps.edn"))))
    (is (false? (.exists (io/file ws "modules/m/deps.edn"))))))

(deftest lein-sub-pool-versions-materialize-in-rig-deps
  ;; the :version token and ~var pool entries materialize at the root
  ;; (:rig/deps), and an inherited pool entry materializes in a module
  ;; against the module's own version and the parent's static ~var defs.
  (let [ws (temp-ws {"project.clj"
                     "(def otel \"1.54.1\")
(defproject foo/parent \"1.2.3\"
  :managed-dependencies [[\"a/b\" \"1.0.0\"]
                         [foo/other :version]
                         [io.opentelemetry/otel ~otel]]
  :sub [\"modules/m\"])"
        "modules/m/project.clj"
        "(defproject foo/m \"1.0.0\"
  :parent-project {:path \"../../project.clj\" :inherit [:managed-dependencies]}
  :dependencies [[foo/other]
                 [io.opentelemetry/otel]])"})
        r (run ws)]
    (is (empty? (problems r)))
    (let [root (file-edn ws "deps.edn")
          m (file-edn ws "modules/m/deps.edn")]
      (is (= {:mvn/version "1.2.3"} (get-in root [:rig/deps 'foo/other])))
      (is (= {:mvn/version "1.54.1"}
             (get-in root [:rig/deps 'io.opentelemetry/otel])))
      (is (= {:mvn/version "1.0.0"} (get-in m [:deps 'foo/other])))
      (is (= {:mvn/version "1.54.1"}
             (get-in m [:deps 'io.opentelemetry/otel]))))))

(deftest lein-sub-pool-version-var-not-static-is-a-problem
  (let [ws (temp-ws {"project.clj"
                     "(def otel (inc 0))
(defproject foo/parent \"1.0.0\"
  :managed-dependencies [[\"a/b\" ~otel]]
  :sub [\"modules/m\"])"
        "modules/m/project.clj"
        "(defproject foo/m \"1.0.0\"
  :parent-project {:path \"../../project.clj\" :inherit [:managed-dependencies]}
  :dependencies [[a/b]])"})
        r (run ws)]
    (is (= 2 (count (problems r))))
    (is (some #(re-find #"project\.clj.*cannot be resolved statically" %)
              (problems r)))
    (is (some #(re-find #"modules/m/project\.clj.*cannot be resolved statically" %)
              (problems r)))
    (is (false? (.exists (io/file ws "deps.edn"))))))

(deftest lein-sub-sibling-dep-in-the-root-dependencies
  (let [ws (temp-ws {"project.clj"
                     "(defproject foo/parent \"1.0.0\"
  :managed-dependencies [[foo/m :version]]
  :sub [\"modules/m\"]
  :dependencies [[foo/m :version]
                 [a/b \"1.0.0\"]])"
        "modules/m/project.clj"
        "(defproject foo/m \"1.0.0\"
  :parent-project {:path \"../../project.clj\" :inherit [:managed-dependencies]})"})
        r (run ws)]
    (is (empty? (problems r)))
    (let [root (file-edn ws "deps.edn")]
      (is (= {:local/root "modules/m"} (get-in root [:deps 'foo/m])))
      (is (= {:mvn/version "1.0.0"} (get-in root [:deps 'a/b])))
      (is (= {:local/root "modules/m"} (get-in root [:rig/deps 'foo/m]))))))

(deftest lein-sub-dry-run-writes-nothing
  (let [ws (temp-ws lein-sub-fixture)
        r (run ws :dry-run? true)]
    (is (empty? (problems r)))
    (is (= 3 (count (get r "edits"))))
    (is (every? true? (map #(get % "changed") (get r "edits"))))
    (is (false? (.exists (io/file ws "deps.edn"))))
    (is (false? (.exists (io/file ws "modules/m/deps.edn"))))))

(deftest lein-sub-nested-sub-is-a-problem
  (let [ws (temp-ws {"project.clj"
                     "(defproject foo/parent \"1.0.0\" :sub [\"modules/m\"])"
        "modules/m/project.clj"
        "(defproject foo/m \"1.0.0\" :sub [\"deep\"])"
        "modules/m/deep/project.clj"
        "(defproject foo/deep \"1.0.0\")"})
        r (run ws)]
    (is (= ["modules/m/project.clj: nested :sub is not supported (flatten the module hierarchy)"]
           (problems r)))
    (is (false? (.exists (io/file ws "deps.edn"))))))

(deftest leiningen-provided-profile-merges-into-base
  ;; lein keeps :provided active by default, so its deps belong to the base
  ;; manifest, not to an alias.
  (let [ws (temp-ws {"project.clj"
                     "(defproject foo/bar \"1.0\"
  :dependencies [[org.clojure/tools.logging \"1.3.0\"]]
  :profiles {:provided {:dependencies [[org.mariadb.jdbc/mariadb-java-client \"2.7.3\"]
                                      [io.netty/netty-handler \"4.1.65.Final\"]]}})\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (some #(re-find #"profile :provided merged into the base manifest" %) (warnings r)))
    (let [d (file-edn ws "deps.edn")]
      (is (= "2.7.3" (get-in d [:deps 'org.mariadb.jdbc/mariadb-java-client :mvn/version])))
      (is (= "4.1.65.Final" (get-in d [:deps 'io.netty/netty-handler :mvn/version])))
      (is (= "1.3.0" (get-in d [:deps 'org.clojure/tools.logging :mvn/version])))
      (is (nil? (get d :profiles))))))

(deftest leiningen-provided-dep-conflict-loses-to-base
  ;; The base deps are declared first in lein's effective dependency
  ;; vector, so they win the version conflict.
  (let [ws (temp-ws {"project.clj"
                     "(defproject foo/bar \"1.0\"
  :dependencies [[org.clojure/tools.logging \"1.3.0\"]]
  :profiles {:provided {:dependencies [[org.clojure/tools.logging \"1.2.99\"]
                                     [a/b \"0.1\"]]}})\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (let [d (file-edn ws "deps.edn")]
      (is (= "1.3.0" (get-in d [:deps 'org.clojure/tools.logging :mvn/version])))
      (is (= "0.1" (get-in d [:deps 'a/b :mvn/version]))))))

(deftest leiningen-provided-paths-and-jvm-opts-merge-into-base
  (let [ws (temp-ws {"project.clj"
                     "(defproject foo/bar \"1.0\"
  :jvm-opts [\"-Db=2\"]
  :profiles {:provided {:source-paths [\"src/provided\"]
                        :resource-paths [\"provided-res\"]
                        :jvm-opts [\"-Dp=1\"]}})\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (let [d (file-edn ws "deps.edn")]
      (is (= ["src" "src/provided" "resources" "provided-res"] (get d :paths)))
      (is (= ["src" "src/provided" "resources" "provided-res"] (get d :rig/artifact-dirs)))
      (is (= ["-Db=2" "-Dp=1"] (get d :jvm-opts))))))

(deftest leiningen-provided-unexpressed-keys-warn
  (let [ws (temp-ws {"project.clj"
                     "(defproject foo/bar \"1.0\"
  :profiles {:provided {:dependencies [[a/b \"1.0\"]]
                        :plugins [[some/plugin \"2.0\"]]
                        :main foo.bar}})\n"})
        r (run ws)]
    (is (empty? (problems r)))
    (is (some #(re-find #"profile :provided merged into the base manifest" %) (warnings r)))
    (is (some #(re-find #"profile :provided dropped :plugins" %) (warnings r)))
    (is (some #(re-find #"profile :provided dropped :main" %) (warnings r)))
    (is (not (some #(re-find #"profile :provided dropped \(only" %) (warnings r))))
    (is (nil? (get (file-edn ws "deps.edn") :profiles)))))
