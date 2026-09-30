(ns rig.resolver.resolve-test
  (:require [clojure.java.io :as io]
            [clojure.string :as str]
            [clojure.test :refer :all]
            [clojure.tools.deps.extensions :as ext]
            [rig.resolver.manifest :as manifest]
            [rig.resolver.resolve :as resolve]
            [rig.resolver.versions :as versions])
  (:import [java.lang ProcessBuilder]
           [com.sun.net.httpserver HttpServer]))

(defn- temp-ws
  "A single-module workspace with the given root deps.edn text."
  [root-text]
  (let [ws (doto (java.io.File.
                  (str (java.nio.file.Files/createTempDirectory "rig-resolve-test"
                                                                (into-array java.nio.file.attribute.FileAttribute []))))
               (.deleteOnExit))]
     (spit (io/file ws "deps.edn") root-text)
     (str ws)))

(defn- temp-ws-with-sibling
  "A workspace <base>/ws with an out-of-root module <base>/sibling (referenced
  as \"../sibling\"). Returns [ws sibling] as strings."
  [root-text sibling-text]
  (let [base (doto (java.io.File.
                    (str (java.nio.file.Files/createTempDirectory "rig-resolve-test"
                                                                  (into-array java.nio.file.attribute.FileAttribute []))))
                    (.deleteOnExit))]
    (doto (java.io.File. (str base "/ws")) (.mkdirs) (.deleteOnExit))
    (doto (java.io.File. (str base "/sibling/src")) (.mkdirs) (.deleteOnExit))
    (spit (io/file base "ws" "deps.edn") root-text)
    (spit (io/file base "sibling" "deps.edn") sibling-text)
    [(str (io/file base "ws")) (str (io/file base "sibling"))]))

(deftest jvm-pin-is-copied-into-the-lock
  (let [ws (temp-ws "{:rig/lib x/y\n :rig/jvm \"21\"}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (= {"vendor" "temurin" "requested" "21" "version" nil}
           (get lock "jvm")))))

(deftest no-jvm-pin-no-jvm-key
  (let [ws (temp-ws "{:rig/lib x/y}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (nil? (get lock "jvm")))))

(deftest jvm-pin-carries-over-when-request-unchanged
  (let [ws (temp-ws "{:rig/lib x/y\n :rig/jvm \"21\"}\n")
        lock (-> (resolve/resolve-lock
                  {:workspace ws
                   :lock {:jvm {:vendor "temurin" :requested "21" :version "21.0.10+7"}}})
                 (get "lock"))]
    (is (= "21.0.10+7" (get-in lock ["jvm" "version"])))
    (is (= "21" (get-in lock ["jvm" "requested"])))))

(deftest jvm-pin-drops-old-version-when-request-changes
  (let [ws (temp-ws "{:rig/lib x/y\n :rig/jvm \"25\"}\n")
        lock (-> (resolve/resolve-lock
                  {:workspace ws
                   :lock {:jvm {:vendor "temurin" :requested "21" :version "21.0.10+7"}}})
                 (get "lock"))]
    (is (= "25" (get-in lock ["jvm" "requested"])))
    (is (nil? (get-in lock ["jvm" "version"])))))

(deftest compile-jvm-opts-are-copied-into-the-lock
  (let [ws (temp-ws "{:rig/lib x/y\n :rig/compile-jvm-opts [\"--enable-preview\"]}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (= ["--enable-preview"] (get lock "compile-jvm-opts")))))

(deftest no-compile-jvm-opts-key-without-declaration
  (let [ws (temp-ws "{:rig/lib x/y}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (nil? (get lock "compile-jvm-opts")))))

(deftest launch-opts-are-copied-into-the-lock-module
  (let [ws (temp-ws "{:rig/lib x/y\n :rig/launch-opts [\"-Xmx1g\"]}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (= ["-Xmx1g"] (get-in lock ["modules" "." :launch-opts])))))

(deftest no-launch-opts-key-without-declaration
  (let [ws (temp-ws "{:rig/lib x/y}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (= [] (get-in lock ["modules" "." :launch-opts])))))

(deftest resolve-lock-with-two-mvn-repos
  (let [ws (temp-ws "{:rig/lib x/y\n :mvn/repos {\"central\" {:url \"https://example.com/central\"} \"corp\" {:url \"https://example.com/corp\"}}}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (= 2 (get lock "version")))
    (is (contains? (get lock "modules") "."))))

(deftest all-repos-sorts-custom-repos-by-id
  (let [ws (temp-ws "{:rig/lib x/y\n :mvn/repos {\"corp\" {:url \"https://example.com/corp\"} \"artifacts\" {:url \"https://example.com/artifacts\"}}}\n")
        repos (resolve/all-repos ws)]
    (is (= [["central" {:url "https://repo.maven.apache.org/maven2"}]
            ["clojars" {:url "https://repo.clojars.org"}]
            ["artifacts" {:url "https://example.com/artifacts"}]
            ["corp" {:url "https://example.com/corp"}]]
           repos))))

(deftest all-repos-redeclared-standard-ids-replace-the-built-ins
  (let [ws (temp-ws "{:rig/lib x/y\n :mvn/repos {\"central\" {:url \"https://example.com/central\"} \"clojars\" {:url \"https://example.com/clojars\"}}}\n")
        repos (resolve/all-repos ws)]
    (is (= [["central" {:url "https://example.com/central"}]
            ["clojars" {:url "https://example.com/clojars"}]]
           repos) "no built-in repo survives a redeclared id")
    (is (= 2 (count (distinct (map first repos)))) "no duplicate ids")))

(deftest all-repos-partial-redeclaration-keeps-the-other-standard
  (let [ws (temp-ws "{:rig/lib x/y\n :mvn/repos {\"central\" {:url \"https://example.com/central\"}}}\n")
        repos (resolve/all-repos ws)]
    (is (= [["clojars" {:url "https://repo.clojars.org"}]
            ["central" {:url "https://example.com/central"}]]
           repos))))

(deftest out-of-root-module-classpath-uses-local-ref
  ;; Regression: a :local/root module outside the workspace root (e.g.
  ;; "../proto") must classify as a {"local" m} classpath entry, not the
  ;; out-of-spec {"path" e} the kernel used to emit (which the Go lockfile
  ;; parser rejects with "bad classpath entry").
  (let [ws (temp-ws-with-sibling
            "{:rig/lib x/y\n :rig/modules [\".\" \"../sibling\"]\n :deps {sibling.lib/dep {:local/root \"../sibling\"}}}\n"
            "{:rig/lib sibling.lib/sibling\n :paths [\"src\"]}\n")]
    (let [lock (-> (resolve/resolve-lock {:workspace (first ws)})
                   (get "lock"))]
      (is (contains? (get lock "modules") "../sibling"))
      (let [cp (get-in lock ["modules" "." :classpath])
            locals (filter #(and (map? %) (get % "local")) cp)
            bad (filter #(and (map? %) (get % "path")) cp)]
        (is (some #(= "../sibling" (get % "local")) locals))
        (is (empty? bad))))))

(deftest undeclared-local-root-overlay-locks-as-its-own-module
  ;; A :local/root dep whose dir is not a workspace module (a dev/ test
  ;; overlay with its own manifest) is locked as a local module: the
  ;; classpath entry is {"local" "dev"}, expanded to the overlay's own
  ;; paths — not mis-attributed to the enclosing root module
  ;; ({"local" "."}).
  (let [ws (doto (java.io.File.
                   (str (java.nio.file.Files/createTempDirectory "rig-resolve-test"
                                                                 (into-array java.nio.file.attribute.FileAttribute []))))
                  (.deleteOnExit))]
    (try
      (do
        (doto (io/file ws "dev") (.mkdirs) (.deleteOnExit))
        (spit (io/file ws "dev" "deps.edn") "{:paths [\".\"]}\n")
        (spit (io/file ws "deps.edn")
              (str "{:rig/lib x/y\n"
                   " :aliases {:test {:extra-deps {x/test-runner {:local/root \"dev\"}}}}}\n"))
        (let [lock (-> (resolve/resolve-lock {:workspace (str ws)}) (get "lock"))]
          (is (= ["."] (get-in lock ["workspace" "modules"]))
              "the overlay is not a workspace module")
          (is (contains? (get lock "modules") "dev")
              "the overlay is locked as a module")
          (is (= ["."] (get-in lock ["modules" "dev" :paths]))
              "the overlay locks its own paths")
          (let [cp (get-in lock ["modules" "." :aliases :test :classpath])
                locals (filter #(and (map? %) (get % "local")) cp)]
            (is (= [{"local" "dev"}] locals)
                (str "the alias classpath references the overlay, got " (pr-str locals))))))
      (finally (.deleteOnExit ws)))))

(deftest nested-undeclared-local-roots-are-all-locked
  ;; An overlay whose own manifest declares a second undeclared :local/root
  ;; is followed: every level locks as a local module and reaches the
  ;; referencing classpaths.
  (let [ws (doto (java.io.File.
                   (str (java.nio.file.Files/createTempDirectory "rig-resolve-test"
                                                                 (into-array java.nio.file.attribute.FileAttribute []))))
                  (.deleteOnExit))]
    (try
      (do
        (doto (io/file ws "dev" "inner") (.mkdirs) (.deleteOnExit))
        (spit (io/file ws "dev" "inner" "deps.edn") "{:paths [\".\"]}\n")
        (spit (io/file ws "dev" "deps.edn")
              (str "{:paths [\".\"]\n"
                   " :deps {x/inner {:local/root \"inner\"}}}\n"))
        (spit (io/file ws "deps.edn")
              "{:rig/lib x/y\n :deps {x/test-runner {:local/root \"dev\"}}}\n")
        (let [lock (-> (resolve/resolve-lock {:workspace (str ws)}) (get "lock"))
              root-cp (get-in lock ["modules" "." :classpath])
              dev-cp (get-in lock ["modules" "dev" :classpath])]
          (is (= ["."] (get-in lock ["workspace" "modules"])))
          (is (contains? (get lock "modules") "dev") "the overlay locks")
          (is (contains? (get lock "modules") "dev/inner") "the nested overlay locks")
          (is (some #(= "dev" (get % "local")) root-cp)
              (str "root classpath: " (pr-str root-cp)))
          (is (some #(= "dev/inner" (get % "local")) root-cp)
              "the nested overlay reaches the root's classpath")
          (is (some #(= "dev/inner" (get % "local")) dev-cp)
              (str "dev classpath: " (pr-str dev-cp)))))
      (finally (.deleteOnExit ws)))))

(deftest uberjar-is-implicit-when-a-file-is-declared
  (let [ws (temp-ws "{:rig/lib x/y\n :rig/uberjar-file \"target/app.jar\"}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (= "target/app.jar"
           (get-in lock ["modules" "." :build :uberjar "file"])))))

(deftest graalvm-pin-appears-with-jvm-pin-and-native-module
  (let [ws (temp-ws "{:rig/lib x/y\n :rig/jvm \"21\"\n :rig/native? true}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (= {"vendor" "graalvm" "requested" "21" "version" nil}
           (get lock "graalvm")))))

(deftest no-graalvm-pin-without-jvm-pin
  (let [ws (temp-ws "{:rig/lib x/y\n :rig/native? true}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (nil? (get lock "graalvm")))))

(deftest no-graalvm-pin-without-native-module
  (let [ws (temp-ws "{:rig/lib x/y\n :rig/jvm \"21\"}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (nil? (get lock "graalvm")))))

(deftest graalvm-pin-carries-over-when-request-unchanged
  (let [ws (temp-ws "{:rig/lib x/y\n :rig/jvm \"21\"\n :rig/native? true}\n")
        lock (-> (resolve/resolve-lock
                  {:workspace ws
                   :lock {:graalvm {:vendor "graalvm" :requested "21" :version "21.0.2"}}})
                 (get "lock"))]
    (is (= "21.0.2" (get-in lock ["graalvm" "version"])))
    (is (= "21" (get-in lock ["graalvm" "requested"])))))

(deftest graalvm-pin-drops-old-version-when-request-changes
  (let [ws (temp-ws "{:rig/lib x/y\n :rig/jvm \"25\"\n :rig/native? true}\n")
        lock (-> (resolve/resolve-lock
                  {:workspace ws
                   :lock {:graalvm {:vendor "graalvm" :requested "21" :version "21.0.2"}}})
                 (get "lock"))]
    (is (= "25" (get-in lock ["graalvm" "requested"])))
    (is (nil? (get-in lock ["graalvm" "version"])))))

(deftest native-build-config-is-locked
  (let [ws (temp-ws "{:rig/lib x/y\n :rig/jvm \"21\"\n :rig/native? true}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (= {"file" "target/y" "opts" []}
           (dissoc (get-in lock ["modules" "." :build :native]) "main")))))

(deftest native-file-is-implicit-native-and-overridable
  (let [ws (temp-ws "{:rig/lib x/y\n :rig/native-file \"target/my-app\"}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (= "target/my-app"
           (get-in lock ["modules" "." :build :native "file"])))))

(deftest explicit-native-false-wins
  (let [ws (temp-ws "{:rig/lib x/y\n :rig/native-file \"target/my-app\"\n :rig/native? false}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (nil? (get-in lock ["modules" "." :build :native])))))

(deftest legacy-manifest-is-rejected
  (let [ws (temp-ws "{:exoscale.project/lib x/y\n :exoscale.project/uberjar-file \"target/app.jar\"}\n")]
    (is (thrown-with-msg? Exception #"still uses legacy keys.*run rig migrate"
                          (resolve/resolve-lock {:workspace ws})))))

(deftest explicit-uberjar?-false-disables-the-implicit-uberjar
  (let [ws (temp-ws "{:rig/lib x/y\n :rig/uberjar? false\n :rig/uberjar-file \"target/app.jar\"}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (nil? (get-in lock ["modules" "." :build :uberjar])))))

(deftest no-uberjar-without-flag-or-file
  (let [ws (temp-ws "{:rig/lib x/y}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (nil? (get-in lock ["modules" "." :build :uberjar])))))

(deftest default-uberjar-file-mirrors-the-plain-jar-naming
  (let [ws (temp-ws "{:rig/lib x/y\n :rig/version \"2.3.4\"\n :rig/uberjar? true}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (= "target/y-2.3.4.jar"
           (get-in lock ["modules" "." :build :uberjar "file"])))
    (is (nil? (get-in lock ["modules" "." :build :uberjar "main"])))))

(deftest default-uberjar-file-without-lib-or-version
  (let [ws (temp-ws "{:rig/uberjar? true}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (= "target/app.jar"
           (get-in lock ["modules" "." :build :uberjar "file"])))))

(deftest ns-compile-enters-the-build-plan
  (let [ws (temp-ws "{:rig/lib x/y\n :rig/uberjar? true\n :rig/ns-compile [a.b c.d]}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (= ["a.b" "c.d"]
           (get-in lock ["modules" "." :build :ns-compile])))))

(deftest javac-opts-enter-the-build-plan
  (let [ws (temp-ws "{:rig/lib x/y\n :rig/java-src-dirs [\"java\"]\n :rig/javac-opts [\"-encoding\" \"UTF-8\"]}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (= ["-encoding" "UTF-8"]
           (get-in lock ["modules" "." :build :javac-opts])))))

(deftest no-javac-opts-key-without-declaration
  (let [ws (temp-ws "{:rig/lib x/y}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (nil? (get-in lock ["modules" "." :build :javac-opts])))))

(deftest artifact-dirs-fall-back-to-paths
  (let [ws (temp-ws "{:rig/lib x/y\n :paths [\"generator\" \"static\"]}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (= ["generator" "static"]
           (get-in lock ["modules" "." :build :artifact-dirs])))))

(deftest artifact-dirs-declaration-wins-over-paths
  (let [ws (temp-ws "{:rig/lib x/y\n :paths [\"src\"]\n :rig/artifact-dirs [\"custom\"]}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (= ["custom"]
           (get-in lock ["modules" "." :build :artifact-dirs])))))

(deftest artifact-dirs-default-without-paths-or-declaration
  (let [ws (temp-ws "{:rig/lib x/y}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (= ["src" "resources"]
           (get-in lock ["modules" "." :build :artifact-dirs])))))

(deftest artifact-dirs-mirror-empty-paths
  (let [ws (temp-ws "{:rig/lib x/y\n :paths []}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (= [] (get-in lock ["modules" "." :build :artifact-dirs])))))

(deftest artifact-dirs-fallback-drops-external-and-target-paths
  (let [ws (temp-ws "{:rig/lib x/y\n :paths [\"src\" \"../ext\" \"target/classes\"]}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (= ["src"]
           (get-in lock ["modules" "." :build :artifact-dirs])))))

(deftest artifact-dirs-fallback-honors-custom-target-dir
  (let [ws (temp-ws "{:rig/lib x/y\n :rig/target-dir \"out\"\n :paths [\"src\" \"out/gen\"]}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (= ["src"]
           (get-in lock ["modules" "." :build :artifact-dirs])))))

(deftest legacy-ns-compile-manifest-is-rejected
  (let [ws (temp-ws "{:exoscale.project/lib x/y\n :exoscale.project/uberjar? true\n :exoscale.project/ns-compile [a.b]}\n")]
    (is (thrown-with-msg? Exception #"still uses legacy keys"
                          (resolve/resolve-lock {:workspace ws})))))

(deftest prep-lib-enters-the-lock
  (let [ws (temp-ws "{:rig/lib x/y\n :deps/prep-lib {:ensure \"target/classes\" :alias :prep :fn aot-compile}\n :aliases {:prep {:ns-default build}}}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (= ["target/classes"] (get-in lock ["modules" "." :prep-ensure])))
    (is (= "prep" (get-in lock ["modules" "." :prep-alias])))
    (is (= "build/aot-compile" (get-in lock ["modules" "." :prep-fn])))))

(deftest prep-lib-qualified-fn-passes-through
  (let [ws (temp-ws "{:rig/lib x/y\n :deps/prep-lib {:ensure \"target/classes\" :alias :prep :fn build/aot-compile}}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (= "build/aot-compile" (get-in lock ["modules" "." :prep-fn])))))

(deftest prep-lib-unqualified-fn-without-ns-default-is-rejected
  (let [ws (temp-ws "{:rig/lib x/y\n :deps/prep-lib {:ensure \"target/classes\" :alias :prep :fn aot-compile}}\n")]
    (is (thrown-with-msg? Exception #":ns-default"
                          (resolve/resolve-lock {:workspace ws})))))

(deftest no-prep-lib-no-prep-keys
  (let [ws (temp-ws "{:rig/lib x/y}\n")
        lock (-> (resolve/resolve-lock {:workspace ws})
                 (get "lock"))]
    (is (nil? (get-in lock ["modules" "." :prep-ensure])))
    (is (nil? (get-in lock ["modules" "." :prep-alias])))
    (is (nil? (get-in lock ["modules" "." :prep-fn])))))

;; version-fn

(defn- git
  "Run git in dir; throws when the command exits non-zero."
  [dir & args]
  (let [proc (.start (doto (ProcessBuilder. (into ["git" "-c" "user.name=t" "-c" "user.email=t@t" "-c" "commit.gpgsign=false" "-C" dir] args))
                       (.redirectErrorStream true)))
        out (str (clojure.string/trim (slurp (.getInputStream proc))))]
    (when (not (zero? (.waitFor proc)))
      (throw (Exception. (str "git " (str/join " " args) " failed: " out))))))

(defn- with-git-ws
  "A temp workspace that is a git repo with one commit and the given files."
  [files f]
  (let [ws (doto (java.io.File.
                    (str (java.nio.file.Files/createTempDirectory "rig-version-fn"
                                                                  (into-array java.nio.file.attribute.FileAttribute []))))
                   (.deleteOnExit))]
    (try
      (do (git (str ws) "init")
          (doseq [[rel text] files]
            (io/make-parents (io/file ws rel))
            (spit (io/file ws rel) text))
          (git (str ws) "add" "-A")
          (git (str ws) "commit" "-m" "x")
          (f (str ws)))
      (finally (.deleteOnExit ws)))))

(deftest version-fn-git-count-revs-substitutes-the-template
  (with-git-ws [["VERSION_TEMPLATE" "0.0.GENERATED_VERSION"]
                ["deps.edn" "{:rig/lib x/y\n :rig/version-fn :git-count-revs}"]]
    (fn [ws]
      (let [data (or (:data (manifest/read-manifest ws)) {})
            v (manifest/read-version data ws ws)]
        (is (= "0.0.1" v) (str "got " v))))))

(deftest version-fn-git-count-revs-uses-the-template-file-key
  (with-git-ws [["MY_TEMPLATE" "9.9.GENERATED_VERSION"]
                ["deps.edn" "{:rig/lib x/y\n :rig/version-fn :git-count-revs\n :rig/version-template-file \"MY_TEMPLATE\"}"]]
    (fn [ws]
      (let [data (or (:data (manifest/read-manifest ws)) {})
            v (manifest/read-version data ws ws)]
        (is (= "9.9.1" v) (str "got " v))))))

(deftest version-fn-epoch-is-a-second-count
  (let [ws (doto (java.io.File.
                  (str (java.nio.file.Files/createTempDirectory "rig-version-fn"
                                                                (into-array java.nio.file.attribute.FileAttribute []))))
                 (.deleteOnExit))]
    (let [data {:rig/version-fn :epoch}
          v (manifest/read-version data ws ws)]
      (is (string? v))
      (is (re-matches #"\d+" v))
      (let [now (long (.getEpochSecond (java.time.Instant/now)))]
        (is (<= (Math/abs (- now (Long/parseLong v))) 60) (str "got " v))))))

(deftest version-fn-loses-to-explicit-version-and-file
  (let [ws (doto (java.io.File.
                    (str (java.nio.file.Files/createTempDirectory "rig-version-fn"
                                                                  (into-array java.nio.file.attribute.FileAttribute []))))
                   (.deleteOnExit))]
    (try
      (do (spit (io/file ws "VERSION") "1.2.3")
          (is (= "1.2.3" (manifest/read-version {:rig/version-fn :epoch} ws ws)))
          (is (= "7.7.7" (manifest/read-version {:rig/version "7.7.7" :rig/version-fn :epoch} ws ws))))
      (finally (.deleteOnExit ws)))))

(deftest version-fn-rejects-unknown-values
  (let [ws (doto (java.io.File.
                  (str (java.nio.file.Files/createTempDirectory "rig-version-fn"
                                                                (into-array java.nio.file.attribute.FileAttribute []))))
                 (.deleteOnExit))]
    (is (thrown? Exception
                 (manifest/read-version {:rig/version-fn :nonsense} ws ws)))))

(deftest read-manifest-rejects-legacy-keys
  (with-git-ws [["VERSION_TEMPLATE" "2.0.GENERATED_VERSION"]
                ["deps.edn" "{:exoscale.project/lib x/y\n :exoscale.project/version-fn \"exoscale.tools.project.api.version/git-count-revs\"}"]]
    (fn [ws]
      (is (thrown-with-msg? Exception #"still uses legacy keys"
                            (manifest/read-manifest ws))))))

(deftest read-manifest-rejects-nested-inherit-markers
  (let [ws (temp-ws "{:rig/lib x/y\n :deps {a/b {:exoscale.deps/inherit :all}}}\n")]
    (is (thrown-with-msg? Exception #"still uses legacy keys"
                          (manifest/read-manifest ws)))))

;; --- Auth proxy: when rig exports RIG_PROXY_REPOS, the kernel rewrites the
;; --- :url of the :auth :oidc repos to the proxy base URL and lets
;; --- create-basis resolve through it (here a local server stands in for
;; --- rig's loopback proxy). Repo ids are kept, so m2's
;; --- _remote.repositories and the lock's attribution still point at the
;; --- original repo.

(defn- rm!
  [dir]
  (when-let [f (java.io.File. (str dir))]
    (when (.exists f)
      (doseq [c (.listFiles f)] (rm! (.getPath c)))
      (.delete f))))

(defn- ts
  "Maven timestamp: yyyyMMddHHmmss in UTC."
  [ms]
  (let [fmt (doto (java.text.SimpleDateFormat. "yyyyMMddHHmmss")
              (.setTimeZone (java.util.TimeZone/getTimeZone "UTC")))]
    (.format fmt (java.util.Date. ms))))

(defn- sha1-hex
  [f]
  (let [md (java.security.MessageDigest/getInstance "SHA-1")
        in (io/input-stream f)
        buf (byte-array 65536)]
    (with-open [in in]
      (loop []
        (let [n (.read in buf)]
          (when (pos? n)
            (do (.update md buf 0 n)
                (recur)))))
      (apply str (for [b (.digest md)] (format "%02x" (bit-and b 0xff)))))))

(defn- minimal-pom
  [group name version]
  (str "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"
       "<project xmlns=\"http://maven.apache.org/POM/4.0.0\">\n"
       "  <modelVersion>4.0.0</modelVersion>\n"
       "  <groupId>" group "</groupId>\n"
       "  <artifactId>" name "</artifactId>\n"
       "  <version>" version "</version>\n"
       "  <packaging>jar</packaging>\n"
       "</project>\n"))

(defn- repo-artifacts!
  "Builds a maven repo under root serving group/name at version v (pom, jar
  and .sha1 sidecars). Returns the repo base path (\"/g/p/name\")."
  [root group name v]
  (let [base (io/file root (str/replace group "." "/") name v)]
    (.mkdirs base)
    (let [pom (io/file base (str name "-" v ".pom"))
          jar (io/file base (str name "-" v ".jar"))]
      (spit pom (minimal-pom group name v))
      (with-open [zos (java.util.zip.ZipOutputStream. (io/output-stream jar))]
        (.putNextEntry zos (doto (java.util.zip.ZipEntry. "META-INF/MANIFEST.MF")
                             (.setTime 0)))
        (.write zos (.getBytes "Manifest-Version: 1.0\n" "UTF-8"))
        (.closeEntry zos))
      (spit (io/file base (str name "-" v ".pom.sha1")) (sha1-hex pom))
      (spit (io/file base (str name "-" v ".jar.sha1")) (sha1-hex jar)))
    (str "/" (str/replace group "." "/") "/" name)))

(defn- repo-metadata!
  "Writes maven-metadata.xml for group/name under root listing only
  version, with last-updated ms."
  [root group name version last-updated-ms]
  (let [base (io/file root (str/replace group "." "/") name)]
    (.mkdirs base)
    (spit (io/file base "maven-metadata.xml")
          (str "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"
               "<metadata>\n"
               "  <versioning>\n"
               "    <versions><version>" version "</version></versions>\n"
               "    <latest>" version "</latest>\n"
               "    <release>" version "</release>\n"
               "    <lastUpdated>" (ts last-updated-ms) "</lastUpdated>\n"
               "  </versioning>\n"
               "</metadata>\n"))))

(defn- start-serving
  "A local HTTP server serving files under root by path: 401 when tok is
  given and the request lacks \"Bearer <tok>\", 404 for missing files.
  Records [method path auth] per request. Returns [base-url seen server]."
  [root tok]
  (let [seen (atom [])
        server (HttpServer/create (java.net.InetSocketAddress. "127.0.0.1" 0) 0)]
    (.createContext
     server "/"
     (reify com.sun.net.httpserver.HttpHandler
       (handle [_ exchange]
         (let [path (.getPath (.getRequestURI exchange))
               method (.getRequestMethod exchange)
               auth (first (.get (.getRequestHeaders exchange) "Authorization"))
               bad-auth (and tok (not= auth (str "Bearer " tok)))
               f (when-not bad-auth (io/file (str root path)))]
           (swap! seen conj [method path auth])
           (cond
             bad-auth (.sendResponseHeaders exchange 401 -1)
             (and f (.exists f))
             (if (= method "HEAD")
               (.sendResponseHeaders exchange 200 -1)
               (do (.sendResponseHeaders exchange 200 (long (.length f)))
                   (with-open [os (.getResponseBody exchange)
                               is (io/input-stream f)]
                     (io/copy is os))))
             :else (.sendResponseHeaders exchange 404 -1))))))
    (.start server)
    [(str "http://" (.substring (str (.getAddress server)) 1) "/")
     seen server]))

(defn- m2-dir-of
  [group name]
  (io/file (System/getProperty "user.home") ".m2" "repository"
           (str/replace group "." "/") name))

(deftest resolve-lock-resolves-authed-repo-deps-via-proxy
  (let [group "rig.ktest.proxy" name "exact"
        v (str "1.0." (mod (System/currentTimeMillis) 1000000))
        orig-root (doto (java.io.File. (str (System/getProperty "java.io.tmpdir") "/"
                                            (str "rig-proxy-orig-" (java.util.UUID/randomUUID))))
                    (.mkdirs) (.deleteOnExit))
        proxy-root (doto (java.io.File. (str (System/getProperty "java.io.tmpdir") "/"
                                             (str "rig-proxy-py-" (java.util.UUID/randomUUID))))
                         (.mkdirs) (.deleteOnExit))
        [orig-base orig-seen orig-srv] (start-serving (str orig-root) "tok-e2e")
        [proxy-base proxy-seen proxy-srv] (start-serving (str proxy-root) nil)
        base-path (repo-artifacts! (str proxy-root) group name v)
        m2 (m2-dir-of group name)]
    (rm! m2)
    (try
      (binding [versions/*proxy-repos* {"oidc" proxy-base}]
        (let [ws (temp-ws (str "{:rig/lib x/y\n"
                               " :deps {" group "/" name " {:mvn/version \"" v "\"}}\n"
                               " :mvn/repos {\"oidc\" {:url \"" orig-base "\" :auth :oidc}}}\n"))
              lock (-> (resolve/resolve-lock {:workspace ws}) (get "lock"))
              art (first (filter #(= group (:group %)) (get lock "artifacts")))]
          (is (some? art) "the dep is in the lock")
          (is (= v (:version art)))
          (is (= "oidc" (:repository art)) "repo attribution kept")
          (is (= (str (subs orig-base 0 (dec (count orig-base)))
                      base-path "/" v "/" name "-" v ".jar")
                 (:url art)) "url from the original repo, not the proxy")
          (is (.exists (io/file m2 v (str name "-" v ".jar"))) "jar in m2")
          (is (.exists (io/file m2 v (str name "-" v ".pom"))) "pom in m2")
          (is (some #(= (str base-path "/" v "/" name "-" v ".jar") (nth % 1)) @proxy-seen)
              "MIMA fetched the jar through the proxy")
          (is (empty? @orig-seen) "no traffic to the original repo")))
      (finally
        (.stop orig-srv 0)
        (.stop proxy-srv 0)
        (rm! m2)
        (rm! (str orig-root))
        (rm! (str proxy-root))))))

(deftest resolve-lock-selects-floating-version-via-proxy
  (let [group "rig.ktest.proxy" name "floatsel" v "1.2.0"
        old-ms (- (System/currentTimeMillis) (* 72 3600 1000))
        orig-root (doto (java.io.File. (str (System/getProperty "java.io.tmpdir") "/"
                                            (str "rig-proxy-orig-" (java.util.UUID/randomUUID))))
                    (.mkdirs) (.deleteOnExit))
        proxy-root (doto (java.io.File. (str (System/getProperty "java.io.tmpdir") "/"
                                             (str "rig-proxy-py-" (java.util.UUID/randomUUID))))
                         (.mkdirs) (.deleteOnExit))
        [orig-base orig-seen orig-srv] (start-serving (str orig-root) "tok-e2e")
        [proxy-base proxy-seen proxy-srv] (start-serving (str proxy-root) nil)
        base-path (repo-artifacts! (str proxy-root) group name v)
        _ (repo-metadata! (str orig-root) group name v old-ms)
        m2 (m2-dir-of group name)]
    (rm! m2)
    (try
      (binding [versions/*proxy-repos* {"oidc" proxy-base}
                versions/*oidc-tokens* {"oidc" "tok-e2e"}]
        (let [ws (temp-ws (str "{:rig/lib x/y\n"
                               " :deps {" group "/" name " {:mvn/version \"RELEASE\"}}\n"
                               " :mvn/repos {\"oidc\" {:url \"" orig-base "\" :auth :oidc}}}\n"))
              lock (-> (resolve/resolve-lock {:workspace ws}) (get "lock"))
              art (first (filter #(= group (:group %)) (get lock "artifacts")))]
          (is (some? art) "the dep is in the lock")
          (is (= v (:version art)) "the kernel picks the candidate older than the cooldown")
          (is (= "oidc" (:repository art)))
          (is (some #(= (str base-path "/maven-metadata.xml") (nth % 1)) @orig-seen)
              "the kernel probe fetched the release metadata")
          (is (every? #(re-find #".*/maven-metadata.*\.xml$" (nth % 1)) @orig-seen)
              (str "only metadata hit the original repo: " (pr-str (map #(nth % 1) @orig-seen))))
          (is (every? #(= "Bearer tok-e2e" (nth % 2)) @orig-seen) "probe carried the bearer")
          (is (some #(= (str base-path "/" v "/" name "-" v ".jar") (nth % 1)) @proxy-seen)
              "the artifact was fetched through the proxy")))
      (finally
        (.stop orig-srv 0)
        (.stop proxy-srv 0)
        (rm! m2)
        (rm! (str orig-root))
        (rm! (str proxy-root))))))

(deftest resolve-lock-resolves-floating-native-via-proxy
  (let [group "rig.ktest.proxy" name "floatnative" v "1.2.0"
        orig-root (doto (java.io.File. (str (System/getProperty "java.io.tmpdir") "/"
                                            (str "rig-proxy-orig-" (java.util.UUID/randomUUID))))
                    (.mkdirs) (.deleteOnExit))
        proxy-root (doto (java.io.File. (str (System/getProperty "java.io.tmpdir") "/"
                                             (str "rig-proxy-py-" (java.util.UUID/randomUUID))))
                         (.mkdirs) (.deleteOnExit))
        [orig-base orig-seen orig-srv] (start-serving (str orig-root) "tok-e2e")
        [proxy-base proxy-seen proxy-srv] (start-serving (str proxy-root) nil)
        base-path (repo-artifacts! (str proxy-root) group name v)
        _ (repo-metadata! (str proxy-root) group name v (System/currentTimeMillis))
        m2 (m2-dir-of group name)]
    (rm! m2)
    (try
      (binding [versions/*proxy-repos* {"oidc" proxy-base}]
        (let [ws (temp-ws (str "{:rig/lib x/y\n"
                               " :deps {" group "/" name " {:mvn/version \"RELEASE\"}}\n"
                               " :mvn/repos {\"oidc\" {:url \"" orig-base "\" :auth :oidc}}}\n"))
              lock (-> (resolve/resolve-lock {:workspace ws
                                              :args {:cooldown {:default "0s"}}})
                       (get "lock"))
              art (first (filter #(= group (:group %)) (get lock "artifacts")))]
          (is (some? art) "the dep is in the lock")
          (is (= v (:version art)) "MIMA resolved RELEASE from the proxy metadata")
          (is (= "oidc" (:repository art)))
          (is (some #(= (str base-path "/maven-metadata.xml") (nth % 1)) @proxy-seen)
              "the metadata was read through the proxy")
          (is (empty? @orig-seen) "zero cooldown: no probe, no original traffic")))
      (finally
        (.stop orig-srv 0)
        (.stop proxy-srv 0)
        (rm! m2)
        (rm! (str orig-root))
        (rm! (str proxy-root))))))

;; --- Single repository: a manifest that redeclares the standard repo ids
;; --- (central, clojars) routes every probe, fetch and lock attribution
;; --- through the redeclared repo — no built-in repo is contacted.

(deftest resolve-lock-attributes-artifacts-to-the-redeclared-standard-repo
  (let [group "rig.ktest.single" name "exact"
        v (str "1.0." (mod (System/currentTimeMillis) 1000000))
        root (doto (java.io.File. (str (System/getProperty "java.io.tmpdir") "/"
                                       (str "rig-single-" (java.util.UUID/randomUUID))))
               (.mkdirs) (.deleteOnExit))
        [base seen srv] (start-serving (str root) nil)
        base-path (repo-artifacts! (str root) group name v)
        m2 (m2-dir-of group name)]
    (rm! m2)
    (try
      (let [ws (temp-ws (str "{:rig/lib x/y\n"
                             " :deps {" group "/" name " {:mvn/version \"" v "\"}}\n"
                             " :mvn/repos {\"central\" {:url \"" base "\"}\n"
                             "            \"clojars\" {:url \"" base "\"}}}\n"))
            lock (-> (resolve/resolve-lock {:workspace ws}) (get "lock"))
            art (first (filter #(= group (:group %)) (get lock "artifacts")))]
        (is (some? art) "the dep is in the lock")
        (is (= v (:version art)))
        (is (= "central" (:repository art)) "attributed to the redeclared central")
        (is (= (str (subs base 0 (dec (count base))) base-path "/" v "/" name "-" v ".jar")
               (:url art)) "the url is the redeclared repo's")
        (is (some #(= (str base-path "/" v "/" name "-" v ".jar") (nth % 1)) @seen)
            "the jar was fetched from the redeclared repo")
        (is (.exists (io/file m2 v (str name "-" v ".jar"))) "jar in m2"))
      (finally
        (.stop srv 0)
        (rm! m2)
        (rm! (str root))))))

(deftest resolve-lock-selects-floating-from-the-redeclared-standard-repos
  (let [group "rig.ktest.single" name "floatsel" v "1.2.0"
        old-ms (- (System/currentTimeMillis) (* 72 3600 1000))
        root (doto (java.io.File. (str (System/getProperty "java.io.tmpdir") "/"
                                       (str "rig-single-" (java.util.UUID/randomUUID))))
               (.mkdirs) (.deleteOnExit))
        [base seen srv] (start-serving (str root) nil)
        base-path (repo-artifacts! (str root) group name v)
        _ (repo-metadata! (str root) group name v old-ms)
        m2 (m2-dir-of group name)]
    (rm! m2)
    (try
      (let [ws (temp-ws (str "{:rig/lib x/y\n"
                             " :deps {" group "/" name " {:mvn/version \"RELEASE\"}}\n"
                             " :mvn/repos {\"central\" {:url \"" base "\"}\n"
                             "            \"clojars\" {:url \"" base "\"}}}\n"))
            lock (-> (resolve/resolve-lock {:workspace ws}) (get "lock"))
            art (first (filter #(= group (:group %)) (get lock "artifacts")))]
        (is (some? art) "the dep is in the lock")
        (is (= v (:version art)) "the kernel selected the version from the redeclared repos' metadata")
        (is (= "central" (:repository art)))
        (is (some #(= (str base-path "/maven-metadata.xml") (nth % 1)) @seen)
            "the metadata was probed on the redeclared repo"))
      (finally
        (.stop srv 0)
        (rm! m2)
        (rm! (str root))))))

;; --- Local vs published coordinate: a workspace module referenced by a
;; --- published transitive POM under its published coordinates enters a
;; --- sibling's basis as both a :local/root and a :mvn/version
;; --- coordinate (neither via a top dep — a top dep wins without a
;; --- version comparison). The kernel registers compare-versions for
;; --- the cross-type pairs so the local module always wins (monorepo
;; --- semantics, matching lein).

(deftest local-coordinate-ranks-above-mvn-coordinate
  ;; dominates? picks the strictly greater coordinate, so the local side of
  ;; the pair must rank above the mvn side in both directions.
  (is (= 1 (ext/compare-versions 'a/b {:local/root "/x"} {:mvn/version "1.0"} {})))
  (is (= -1 (ext/compare-versions 'a/b {:mvn/version "1.0"} {:local/root "/x"} {}))))

(deftest resolve-lock-local-module-wins-over-published-coordinate
  (let [group "rig.ktest.localwin"
        pv "1.0" ;; published parent version
        iv "0.1" ;; published version of the local module
        root (doto (java.io.File. (str (System/getProperty "java.io.tmpdir") "/"
                                       (str "rig-localwin-" (java.util.UUID/randomUUID))))
               (.mkdirs) (.deleteOnExit))
        [base seen srv] (start-serving (str root) nil)
        parent-path (repo-artifacts! (str root) group "parent" pv)
        _ (repo-artifacts! (str root) group "inner" iv)
        parent-pom (io/file (str root parent-path "/" pv "/parent-" pv ".pom"))
        m2p (m2-dir-of group "parent")
        m2i (m2-dir-of group "inner")]
    (rm! m2p)
    (rm! m2i)
    (try
      (do
        (spit parent-pom (str "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"
                              "<project xmlns=\"http://maven.apache.org/POM/4.0.0\">\n"
                              "  <modelVersion>4.0.0</modelVersion>\n"
                              "  <groupId>" group "</groupId>\n"
                              "  <artifactId>parent</artifactId>\n"
                              "  <version>" pv "</version>\n"
                              "  <packaging>jar</packaging>\n"
                              "  <dependencies>\n"
                              "    <dependency>\n"
                              "      <groupId>" group "</groupId>\n"
                              "      <artifactId>inner</artifactId>\n"
                              "      <version>" iv "</version>\n"
                              "    </dependency>\n"
                              "  </dependencies>\n"
                              "</project>\n"))
        (spit (io/file (str root parent-path "/" pv "/parent-" pv ".pom.sha1")) (sha1-hex parent-pom))
        (doto (io/file root "ws" "inner" "src") (.mkdirs))
        (doto (io/file root "ws" "mid") (.mkdirs))
        (spit (io/file root "ws" "deps.edn")
              (str "{:rig/lib rig.ktest.localwin/root\n"
                   " :rig/modules [\".\" \"mid\" \"inner\"]\n"
                   " :deps {" group "/parent {:mvn/version \"" pv "\"}\n"
                   "        " group "/mid {:local/root \"mid\"}}\n"
                   " :mvn/repos {\"central\" {:url \"" base "\"}\n"
                   "            \"clojars\" {:url \"" base "\"}}}\n"))
        (spit (io/file root "ws" "mid" "deps.edn")
              (str "{:rig/lib " group "/mid\n"
                   " :deps {" group "/inner {:local/root \"../inner\"}}}\n"))
        (spit (io/file root "ws" "inner" "deps.edn")
              (str "{:rig/lib " group "/inner\n :paths [\"src\"]}\n"))
        (let [ws (str (io/file root "ws"))
              lock (-> (resolve/resolve-lock {:workspace ws}) (get "lock"))
              cp (get-in lock ["modules" "." :classpath])
              arts (get lock "artifacts")]
          (is (some #(= "inner" (get % "local")) cp)
              "the local module is on the classpath")
          (is (nil? (some #(and (= "inner" (get % :name)) (= "mvn" (get % :kind))) arts))
              "the published coordinate was not selected or locked")
          (is (some #(and (= "parent" (get % :name)) (= pv (get % :version))) arts)
              "the published parent resolved normally")))
      (finally
        (.stop srv 0)
        (rm! m2p)
        (rm! m2i)
        (rm! (str root))))))
