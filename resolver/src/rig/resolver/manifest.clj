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

(def removed-keys
  "Keys the :rig/* model has removed: a manifest carrying one is refused
  at read time, like a legacy one — the fix is dropping the key, not
  migrating."
  #{:rig/ns-compile})

(defn- keys-of
  "The keys anywhere in the manifest data (map keys at any depth) that
  key? matches."
  [key? x]
  (cond
    (map? x)
    (concat (filter key? (keys x))
            (mapcat (partial keys-of key?) (vals x)))
    (sequential? x)
    (mapcat (partial keys-of key?) x)
    :else
    ()))

(defn read-manifest
  [dir]
  (let [file (io/file dir "deps.edn")]
    (if (.exists file)
      (let [bytes (Files/readAllBytes (.toPath file))
            data (edn/read-string (String. bytes "UTF-8"))
            legacy (set (keys-of #(and (keyword? %)
                                       (contains? legacy-ns (namespace %)))
                                 data))
            removed (set (keys-of (partial contains? removed-keys) data))]
        (when (seq legacy)
          (throw (ex-info (str (.getPath file)
                               ": still uses legacy keys "
                               (str/join ", " (map str (sort legacy)))
                               " - run rig migrate")
                          {:rig/legacy? true})))
        (when (seq removed)
          (throw (ex-info (str (.getPath file)
                               ": uses removed key "
                               (str/join ", " (map str (sort removed)))
                               " - drop it: :rig/main and the module's :gen-class namespaces compile without a declaration")
                          {:rig/removed? true})))
        {:dir (str dir)
         :file (str file)
         :sha256 (sha256-hex bytes)
         :data data})
      {:dir (str dir)
       :file (str file)
       :sha256 nil
       :data nil})))

(defn modules-of
  [root-data]
  (into ["."] (map str (get root-data :rig/modules))))

(defn lib [data] (get data :rig/lib))
(defn main-ns [data] (get data :rig/main))
(defn artifact-dirs [data] (get data :rig/artifact-dirs))
(defn paths [data] (get data :paths))
(defn java-src-dirs [data] (get data :rig/java-src-dirs))
(defn javac-opts [data] (get data :rig/javac-opts))
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
(defn timestamp-string [data] (get data :rig/timestamp-string))
(defn native-file [data] (get data :rig/native-file))
(defn native?
  "True when the module declares a native-image build. An explicit
  :rig/native? wins, including an explicit false; otherwise declaring a
  :rig/native-file implies one."
  [data]
  (if (contains? data :rig/native?)
    (true? (get data :rig/native?))
    (boolean (native-file data))))
(defn native-opts [data] (get data :rig/native-opts))
(defn prep-ensure
  "The prep output paths the module declares via :deps/prep-lib :ensure —
  generated code rig does not produce itself."
  [data]
  (when-let [e (:ensure (get data :deps/prep-lib))]
    (if (string? e) [e] (when (sequential? e) (vec e)))))

(defn prep-alias
  "The name (string) of the deps alias under which the module's
  :deps/prep-lib :fn runs."
  [data]
  (some-> (get-in data [:deps/prep-lib :alias]) name))

(defn prep-fn
  "The module's :deps/prep-lib :fn as a fully qualified var name string:
  :fn when it is qualified, else qualified with the prep alias's
  :ns-default — the form rig invokes on the locked alias classpath."
  [data]
  (when-let [f (get-in data [:deps/prep-lib :fn])]
    (if (symbol? f)
      (if (namespace f)
        (str f)
        (format "%s/%s"
                (or (get-in data [:aliases (get-in data [:deps/prep-lib :alias]) :ns-default])
                    (throw (ex-info (str ":deps/prep-lib :fn " (name f)
                                         " is unqualified and the prep alias declares no :ns-default")
                                    {})))
                (name f)))
      (throw (ex-info (str "bad :deps/prep-lib :fn " (pr-str f) " (want a symbol)") {})))))
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
          (let [f (io/file base (or (get data :rig/version-file) "VERSION"))]
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
            (let [f (io/file base (or (get data :rig/version-template-file) "VERSION_TEMPLATE"))]
              (when (.exists f)
                ;; trim-newline: a template is a text file, so its trailing
                ;; LF is an artifact of editing, not part of the version —
                ;; left in, it lands in the version string and the artifact
                ;; filename ("built target/app-1.0.1\n.jar").
                (str/trim-newline (str/replace (slurp f) "GENERATED_VERSION" (b/git-count-revs {:dir base}))))))
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

;; --- Git deps ----------------------------------------------------------------

(def git-coord-keys
  "Keys that mark a dep spec as a git coord: tools.deps' git coord keys
  (:git/url, :git/sha, :git/tag, the legacy :sha) plus :git/branch —
  which tools.deps never supported (its spec rejects it) but Rig names
  in its own error — and the legacy :tag."
  #{:git/url :git/sha :git/tag :git/branch :sha :tag})

(defn git-coord?
  [s]
  (and (map? s) (some git-coord-keys (keys s))))

(defn git-deps-of
  "The manifest's git coords as [{:loc loc :coord c :spec s}]: the :deps
  plus every alias's :extra-deps and :replace-deps (the specs that enter
  a basis). loc is \"deps\" or \"alias <a> <kind>\"."
  [data]
  (into []
        (for [[loc deps] (concat [["deps" (get data :deps {})]]
                                 (for [al (keys (get data :aliases {}))
                                       k [:extra-deps :replace-deps]]
                                   [(str "alias " (name al) " " (name k))
                                    (get-in data [:aliases al k])]))
              :when (map? deps)
              [c s] deps
              :when (git-coord? s)]
          {:loc loc :coord c :spec s})))

(def full-sha-re #"^[0-9a-f]{40}$")

(def ^:private git-hosts
  "The git hosts tools.deps derives a :git/url from: [group-re url-template]
  pairs, the url template taking the group's owner and the coord's name."
  [[#"^(?:com|io).github.([^.]+)$" "https://github.com/%s/%s.git"]
   [#"^(?:com|io).gitlab.([^.]+)$" "https://gitlab.com/%s/%s.git"]
   [#"^(?:org|page).codeberg.([^.]+)$" "https://codeberg.org/%s/%s.git"]
   [#"^(?:org|io).bitbucket.([^.]+)$" "https://bitbucket.org/%s/%s.git"]
   [#"^(?:com|io).beanstalkapp.([^.]+)$" "https://%s.git.beanstalkapp.com/%s.git"]
   [#"^ht.sr.([^.]+)$" "https://git.sr.ht/~%s/%s"]])

(defn auto-git-url
  "The :git/url tools.deps derives for coord when its group maps to a
  known git host (tools.deps' auto-git-url table), nil otherwise — the
  coord must then declare :git/url."
  [coord]
  (let [c (symbol (str coord))
        group (namespace c)
        name (name c)]
    (when group
      (some (fn [[re url]]
              (when-let [m (re-matches re group)]
                (format url (second m) name)))
            git-hosts))))

(defn git-ref-problems
  "The manifest's git coords Rig refuses, as [{:loc loc :coord c :message
  m}]. A git coord is a commit pin: :git/sha, a full 40-hex sha. :git/tag
  and :git/branch are not supported — a movable ref is not a pin — and
  neither are the legacy :tag/:sha keys. A coord that declares no
  :git/url must derive one from a known git host."
  [data]
  (into []
        (for [{:keys [loc coord spec]} (git-deps-of data)
              :let [c (str coord)
                    sha (get spec :git/sha)
                    problems (concat
                              (when (contains? spec :git/tag)
                                [":git/tag is not supported — a tag moves; pin the commit it names with :git/sha"])
                              (when (contains? spec :tag)
                                [":tag is the legacy form — use :git/sha, the full commit sha"])
                              (when (contains? spec :git/branch)
                                [":git/branch is not supported — a branch moves; pin the commit it names with :git/sha"])
                              (when (contains? spec :sha)
                                [":sha is the legacy form — use :git/sha, the full commit sha"])
                              (when (and (contains? spec :git/sha)
                                         (not (and (string? sha) (re-matches full-sha-re sha))))
                                [(str ":git/sha must be a full 40-hex commit sha (got " (pr-str sha) ")")])
                              (when (and (not (contains? spec :git/sha))
                                         (not (contains? spec :sha)))
                                ["no commit pin — declare :git/sha, the full 40-hex commit sha"])
                              (when (and (not (contains? spec :git/url))
                                         (nil? (auto-git-url c)))
                                ["declare :git/url (the group does not map to a known git host)"]))]
              :when (seq problems)
              message problems]
          {:loc loc :coord c :message message})))

(defn git-ref-problem-detail
  "One problem as \"<loc>: <message>\" (the loc prefix absent for :deps)."
  [{:keys [loc message]}]
  (str (when-not (= loc "deps") (str loc ": ")) message))

(defn git-ref-problem-line
  "One problem as \"<coord>: <loc>: <message>\" (the loc prefix absent for
  :deps) — the manifest-relative line lock and tree print."
  [{:keys [coord] :as p}]
  (str (str coord) ": " (git-ref-problem-detail p)))

(defn git-deps-map
  "The manifest's git deps as {\"group/name/sha\" {:url url :deps/root
  root}} — the lock's merge entries keyed on the commit pin, url the
  manifest's :git/url or the one derived from the group (auto-git-url).
  A spec Rig refuses contributes nothing: git-ref-problems names it
  before resolution runs."
  [data]
  (into {}
        (for [{:keys [coord spec]} (git-deps-of data)
              :when (re-matches full-sha-re (or (get spec :git/sha) ""))
              :let [c (str coord)
                    [g n] (str/split c #"/" 2)
                    url (or (get spec :git/url) (auto-git-url c))]]
          [(str g "/" n "/" (get spec :git/sha))
           {:url url :deps/root (get spec :deps/root)}])))
