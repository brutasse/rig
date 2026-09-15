(ns rig.resolver.manifest
  (:require [clojure.edn :as edn]
            [clojure.java.io :as io]
            [clojure.string :as str]
            [clojure.tools.build.api :as b])
  (:import [java.nio.file Files]))

(defn sha256-hex
  [bytes]
  (let [digest (.digest (java.security.MessageDigest/getInstance "SHA-256") bytes)]
    (apply str (for [b digest] (format "%02x" b)))))

(def legacy-ns
  "Manifest namespaces that only exist in the legacy (pre-migrate) model."
  #{"exoscale.project" "exoscale.deps" "slipset.deps-deploy" "antq.core"})

(defn- legacy-keys-of
  "All legacy keys anywhere in the manifest data (map keys at any depth)."
  [x]
  (cond
    (map? x)
    (concat (filter (fn [k] (and (keyword? k) (contains? legacy-ns (namespace k))))
                    (keys x))
            (mapcat legacy-keys-of (vals x)))
    (sequential? x)
    (mapcat legacy-keys-of x)
    :else
    ()))

(defn read-manifest
  [dir]
  (let [file (io/file dir "deps.edn")]
    (if (.exists file)
      (let [bytes (Files/readAllBytes (.toPath file))]
        (let [data (edn/read-string (String. bytes "UTF-8"))
              legacy (set (legacy-keys-of data))]
          (when (seq legacy)
            (throw (ex-info (str (.getPath file)
                                 ": still uses legacy keys "
                                 (str/join ", " (map str (sort legacy)))
                                 " - run rig migrate")
                            {:rig/legacy? true})))
          {:dir (str dir)
           :file (str file)
           :sha256 (sha256-hex bytes)
           :data data}))
      {:dir (str dir)
       :file (str file)
       :sha256 nil
       :data nil})))

(defn modules-of
  [root-data]
  (vec (concat ["."]
               (some->> (get root-data :rig/modules)
                        (map str)
                        (vec)))))

(defn lib [data] (get data :rig/lib))
(defn main-ns [data] (get data :rig/main))
(defn src-dirs [data] (get data :rig/src-dirs))
(defn java-src-dirs [data] (get data :rig/java-src-dirs))
(defn target-dir [data] (or (get data :rig/target-dir) "target"))
(defn uberjar-file [data] (get data :rig/uberjar-file))
(defn uberjar?
  "True when the module declares an uberjar. An explicit :rig/uberjar?
  wins, including an explicit false; otherwise declaring a
  :rig/uberjar-file implies one."
  [data]
  (if (contains? data :rig/uberjar?)
    (true? (get data :rig/uberjar?))
    (boolean (uberjar-file data))))
(defn uber-opts [data] (get data :rig/uber-opts))
(defn ns-compile [data] (get data :rig/ns-compile))
(defn prep-ensure
  "The prep output paths the module declares via :deps/prep-lib :ensure —
  generated code rig does not produce itself."
  [data]
  (when-let [e (:ensure (get data :deps/prep-lib))]
    (if (string? e) [e] (when (sequential? e) (vec e)))))
(defn clean-dirs [data] (get data :rig/clean-dirs))
(defn test? [data]
  (if (contains? data :rig/test?)
    (true? (get data :rig/test?))
    true))

(defn publish-spec
  [data]
  (when-let [rig (get data :rig/publish)]
    (select-keys rig [:repo :sign-releases?])))

(defn publish? [data]
  (let [rig (get data :rig/publish?)]
    (if (nil? rig) (boolean (publish-spec data)) (true? rig))))

(defn- version-from-file
  [data dir wsroot]
  (some (fn [base]
          (when-let [f (io/file base (or (get data :rig/version-file) "VERSION"))]
            (when (.exists f)
              (first (remove str/blank? (str/split-lines (slurp f)))))))
        [dir wsroot]))

(defn- version-from-fn
  "The dynamic version (tools.project's version-fn semantics, restricted to
  keywords: the kernel jar cannot load user code). :git-count-revs replaces
  GENERATED_VERSION in the template file (module dir, then workspace root,
  default VERSION_TEMPLATE) with `git rev-list HEAD --count`; :epoch is the
  current unix time in seconds. Moving version: it changes with the repo, so
  the lock records a snapshot taken at lock time."
  [vfn data dir wsroot]
  (cond
    (= vfn :git-count-revs)
    (some (fn [base]
            (when-let [f (io/file base (or (get data :rig/version-template-file) "VERSION_TEMPLATE"))]
              (when (.exists f)
                (str/replace (slurp f) "GENERATED_VERSION" (b/git-count-revs {:dir base})))))
          [dir wsroot])
    (= vfn :epoch)
    (str (.getEpochSecond (java.time.Instant/now)))
    :else
    (throw (ex-info (str "unsupported :rig/version-fn " (pr-str vfn)
                         " (expected :git-count-revs or :epoch)")
                    {}))))

(defn read-version
  "The module version: :rig/version, then the version file (module dir, then
  workspace root), then :rig/version-fn."
  [data dir wsroot]
  (or (get data :rig/version)
      (version-from-file data dir wsroot)
      (when-let [vfn (get data :rig/version-fn)]
        (version-from-fn vfn data dir wsroot))))

(defn aliases-of
  [data]
  (->> (keys (get data :aliases {}))
       (remove #{:project})
       (map keyword)
       (sort)))
