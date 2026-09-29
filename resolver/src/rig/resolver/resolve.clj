(ns rig.resolver.resolve
  (:require [cheshire.core :as json]
            [clojure.edn :as edn]
            [clojure.java.io :as io]
            [clojure.string :as str]
            [clojure.tools.deps :as td]
            [clojure.tools.deps.extensions :as ext]
            [rig.resolver.manifest :as manifest]
            [rig.resolver.plan :as plan]
            [rig.resolver.versions :as versions]))

;; A workspace module can enter a module's basis twice under the same lib
;; name: as a :local/root coordinate (a sibling module or :rig/deps entry)
;; and as a published :mvn/version coordinate (a published transitive POM
;; that still references the module's old published version). tools.deps
;; only defines compare-versions for same-type pairs, so the cross-type
;; comparison throws "Unable to compare versions" and the whole lock
;; fails. Monorepo semantics (matching lein): the local module always
;; wins over its published coordinates. dominates? picks the strictly
;; greater coordinate, so local must rank above mvn in both directions.
(defmethod ext/compare-versions [:local :mvn]
  [_lib _local _mvn _config] 1)

(defmethod ext/compare-versions [:mvn :local]
  [_lib _mvn _local _config] -1)

(defn- home-dir [] (System/getProperty "user.home"))
(defn- m2dir [] (str (io/file (home-dir) ".m2" "repository")))
(defn- gitlibs-dir [] (str (io/file (home-dir) ".gitlibs" "libs")))

(defn resolve-basis
  [dir aliases project-config]
  (td/create-basis {:dir (io/file dir)
                    :project (or project-config "deps.edn")
                    :aliases aliases}))

(defn- repos-of
  "The module's repos: the standard repos (any redeclared id replaced by the
  manifest's entry) plus every :mvn/repos entry."
  [data]
  (into (vec (versions/standard-repos-of (get data :mvn/repos {})))
        (for [[id sp] (sort-by key (get data :mvn/repos {}))]
          [id sp])))

(defn cooldown-of
  "The workspace cooldown policy: the request's :cooldown wins, else the root
  manifest's :rig/cooldown (+ :rig/cooldown-repos), else the 48h default."
  [args data]
  (or (get args :cooldown)
      (let [d (get data :rig/cooldown)
            r (get data :rig/cooldown-repos)]
        (when (or d r)
          {:default (or (some-> d str) "48h")
           :repos (or r {})}))
      {:default "48h"}))

(defn all-repos
  "Standard repos + every :mvn/repos entry of the workspace's manifests. An id
  redeclared in any manifest replaces its standard repo (tools.deps
  semantics), so each id appears once."
  [ws]
  (let [root-man (manifest/read-manifest ws)
        root-data (or (:data root-man) {})
        module-dirs (manifest/modules-of root-data)
        manifests (into {ws root-man}
                        (for [m (remove #(.equals % ".") module-dirs)]
                          [m (manifest/read-manifest (str (io/file ws m)))]))
        extra (distinct (mapcat (fn [man]
                                  (for [[id sp] (get-in (or (:data man) {}) [:mvn/repos] {})]
                                    [id sp]))
                                (vals manifests)))
        declared (into #{} (map first extra))]
    (into (vec (versions/standard-repos-of declared)) (sort-by first extra))))

(defn skip-entry
  "Lock shape for a version-selection skip/refusal entry: string keys,
  published_at snake_cased (the lock schema, design §6)."
  [s]
  (if (contains? s :published-at)
    (-> s (dissoc :published-at) (assoc "published_at" (get s :published-at)))
    s))

(defn lock-of
  "The existing lock document for a request: the :lock path read from the
  workspace ({} when the file is absent), or the map given inline."
  [request]
  (let [l (get request :lock)]
    (cond
      (nil? l) {}
      (string? l)
      (let [f (io/file (str (:workspace request)) l)]
        (if (.exists f) (json/parse-string (slurp f) true) {}))
      :else l)))

(defn- git-deps-of
  [data]
  (let [all (concat (for [[c s] (get data :deps {})] [c s])
                    (for [al (keys (get data :aliases {}))
                          [c s] (get-in data [:aliases al :extra-deps] {})]
                      [c s]))]
    (into {}
          (for [[c s] all
                :when (and (map? s) (:git/sha s))
                :let [cn (str c)
                      [g n] (str/split cn #"/" 2)]]
            [(str g "/" n "/" (:git/sha s))
              {:url (:git/url s) :deps/root (get s :deps/root)}]))))

(defn- build-artifact-dirs
  "The module's artifact dirs: :rig/artifact-dirs when declared; else the
  manifest's :paths limited to the module's own dirs — paths leaving the
  module and the target dir (build output) are classpath-only and must not
  be compiled or copied into the artifact (absolute :paths entries cannot
  be locked, so every entry here is relative); else the conventional
  default."
  [data]
  (or (manifest/artifact-dirs data)
      (when-let [p (manifest/paths data)]
        (let [t (manifest/target-dir data)]
          (vec (remove #(or (not (string? %))
                            (str/starts-with? % "..")
                            (= % t)
                            (str/starts-with? % (str t "/")))
                       p))))
      ["src" "resources"]))

(defn- build-plan
  [data version]
  (let [target (manifest/target-dir data)
        ;; Default uberjar file: mirror the plain jar naming (last lib segment,
        ;; or "app" when no lib is declared; no version suffix when absent).
        artifact (or (some-> (manifest/lib data) str (str/split #"/") last) "app")
        default (str target "/" artifact (when (seq version) (str "-" version)) ".jar")
        ns-compile (when-let [nsc (manifest/ns-compile data)] (vec (map name nsc)))]
    (cond-> {:artifact-dirs (build-artifact-dirs data)
             :java-src-dirs (or (manifest/java-src-dirs data) [])
             :class-dir (str target "/classes")
             :jar true
             :uberjar (when (manifest/uberjar? data)
                       {"file" (or (manifest/uberjar-file data) default)
                        "main" (manifest/main-ns data)
                        "opts" (or (manifest/uber-opts data) {})})}
      (seq ns-compile) (assoc :ns-compile ns-compile)
      (seq (manifest/javac-opts data)) (assoc :javac-opts (manifest/javac-opts data))
      (manifest/native? data)
      (assoc :native {"file" (or (manifest/native-file data)
                                 (str target "/" artifact))
                      "main" (manifest/main-ns data)
                      "opts" (or (manifest/native-opts data) [])}))))

(defn- publish-plan
  [data]
  (if (manifest/publish? data)
    (let [spec (manifest/publish-spec data)]
      (cond-> {"enabled" true
               "repo" (or (get spec :repo) "clojars")
               "sign-releases?" (when spec (get spec :sign-releases? false))}
        (get spec :installer) (assoc "installer" (get spec :installer))))
    {"enabled" false}))

(defn- module-plan
  [wsroot mdir man base-mapped alias-maps m2 gitlibs modules-abs]
  (let [data (or (:data man) {})
        version (manifest/read-version data mdir wsroot)]
    {:manifest_sha256 (:sha256 man)
     :lib (manifest/lib data)
     :version version
     :main (manifest/main-ns data)
     :prep-ensure (manifest/prep-ensure data)
     :prep-alias (manifest/prep-alias data)
     :prep-fn (manifest/prep-fn data)
     :jvm-opts (or (get data :jvm-opts) [])
     :launch-opts (or (get data :rig/launch-opts) [])
     :paths (:paths base-mapped)
     :classpath (:classpath base-mapped)
     :aliases (into {}
                    (for [[al m] alias-maps]
                      [al {:classpath (:classpath m)
                           "paths" (or (get-in data [:aliases al :extra-paths]) [])
                           "jvm-opts" (or (get-in data [:aliases al :jvm-opts]) [])
                           "env" (or (get-in data [:aliases al :env]) {})
                           "exec" (when-let [f (get-in data [:aliases al :exec-fn])]
                                    {"type" "exec-fn" "fn" (str f)})}]))
     :build (build-plan data version)
     :publish (publish-plan data)
     :test {:enabled (manifest/test? data)}
     :raw-artifacts (concat (:artifacts base-mapped)
                            (mapcat :artifacts (vals alias-maps)))}))

(defn- local-roots-of
  "The module's :local/root dependencies (its :deps and every alias's
  :extra-deps) as [workspace-relative-dir canonical-abs] pairs. The manifest
  values are module-dir-relative; the key is relative to the workspace root
  (\"../..\" when the ref escapes it). A nil key means the abs cannot be
  expressed relative to the workspace (different filesystem root)."
  [data mdir ws]
  (let [ws-path (.toPath (io/file ws))
        rel (fn [abs]
              (try (str (.relativize ws-path (.toPath (io/file abs))))
                   (catch IllegalArgumentException _ nil)))
        refs (concat (keep (fn [[_ spec]] (get spec :local/root))
                          (get data :deps {}))
                    (mapcat (fn [al]
                              (keep (fn [[_ spec]] (get spec :local/root))
                                    (get-in al [:extra-deps] {})))
                            (vals (get data :aliases {}))))]
    (for [r refs]
      (let [abs (some-> (io/file mdir r) (.getCanonicalPath) str)]
        [(rel abs) abs]))))

(defn- local-modules
  "Every :local/root dir referenced by the module manifests that is not
  itself a workspace module and holds a manifest, as
  {workspace-relative-dir canonical-abs}. Nested references are followed; the
  search stops at already-seen dirs, so cycles terminate."
  [ws module-dirs modules-abs root-data]
  (loop [dirs modules-abs
         extra {}
         pending (vec module-dirs)]
    (if (empty? pending)
      extra
      (let [m (peek pending)
            mdir (get dirs m)
            data (if (= m ".")
                   root-data
                   (some-> mdir (manifest/read-manifest) :data))
            news (into {}
                       (for [[rel abs] (local-roots-of data mdir ws)
                             :when (and (seq rel)
                                        (not (contains? dirs rel))
                                        (.exists (io/file abs "deps.edn")))]
                         [rel abs]))]
        (if (empty? news)
            (recur dirs extra (pop pending))
            (recur (into dirs news)
                   (merge extra news)
                   (vec (concat (pop pending) (keys news)))))))))

(defn resolver-id
  "Identity of this kernel. Normally from build-info.edn, baked into the
  jar at build time (build.clj); a placeholder when the namespace is
  loaded from source (build step, REPL, tests), where the resource is
  absent."
  []
  (let [info (some-> (io/resource "build-info.edn") slurp edn/read-string)
        version (or (:version info) "v0.1.0")
        sha (:git-sha info)]
    {"lib" "io.github.brutasse/rig-resolver"
     "version" version
     "git/sha" (if (and sha (re-matches #"^[0-9a-f]{7,40}$" sha)) sha (apply str (repeat 40 "0")))}))

(defn resolve-lock
  [request]
  (let [ws (str (:workspace request))
        args (or (:args request) {})
        root-man (manifest/read-manifest ws)
        root-data (or (:data root-man) {})
        cooldown (cooldown-of args root-data)
        force (true? (get args :force))
        overrides (into {} (for [[k v] (or (get args :requirement-overrides) {})]
                             [(str k) v]))
        respect-pins? (if (contains? args :respect-existing-pins)
                        (true? (get args :respect-existing-pins))
                        true)
        lock (lock-of request)
        pins (into {}
                   (for [a (get lock :artifacts)
                         :when (= "mvn" (get a :kind))]
                     [(str (get a :group) "/" (get a :name)) (get a :version)]))
        now-ms (System/currentTimeMillis)
        module-dirs (manifest/modules-of root-data)
        declared-abs (into {} (for [m module-dirs]
                                [m (let [p (if (= m ".") ws (str (io/file ws m)))]
                                     (try (some-> p (io/file) (.getCanonicalPath) str)
                                          (catch Exception _ p)))]))
        ;; A :local/root dependency that is not itself a workspace module
        ;; (e.g. a dev/ test overlay with its own manifest) is locked as a
        ;; local module: it enters the modules map so classpath
        ;; {"local" m} entries expand to its own paths, but stays out of
        ;; workspace.modules, so it is never a build/test/publish target.
        local-mods (local-modules ws module-dirs declared-abs root-data)
        modules-abs (into declared-abs local-mods)
        m2 (m2dir)
        gl (gitlibs-dir)
        manifests (into {ws root-man}
                        (for [m (concat (remove #(.equals % ".") module-dirs)
                                        (keys local-mods))]
                          [(get modules-abs m) (manifest/read-manifest (get modules-abs m))]))
        git-deps (apply merge (map (fn [man] (git-deps-of (or (:data man) {}))) (vals manifests)))
         all-repos (let [extra (distinct (mapcat (fn [man]
                                                   (for [[id sp] (get-in (or (:data man) {}) [:mvn/repos] {})]
                                                     [id sp]))
                                                 (vals manifests)))]
                     (into (vec (versions/standard-repos-of (into #{} (map first extra))))
                           (sort-by first extra)))
        state (atom {:skipped [] :refused []})
        modules (into {}
                      (for [m (concat module-dirs (keys local-mods))]
                        (let [mdir (get modules-abs m)
                              man (get manifests mdir)
                              data (or (:data man) {})
                              sel (versions/select-versions data
                                                            (repos-of data)
                                                            cooldown
                                                            force
                                                            pins
                                                            overrides
                                                            respect-pins?
                                                            now-ms)
                              _ (swap! state update :skipped into (:skipped sel))
                              _ (swap! state update :refused into (:refused sel))
                               proj (let [base (or (when (:changed? sel)
                                                    (versions/apply-versions data (:selected sel) overrides))
                                                  data)]
                                      (when (or (:changed? sel)
                                               (seq (versions/proxy-map)))
                                        (versions/with-proxy-repos base)))
                              base-basis (resolve-basis mdir [] proj)
                              base-mapped (plan/map-classpath (:classpath-roots base-basis) modules-abs m2 gl)
                              alias-maps (into {}
                                               (for [al (manifest/aliases-of data)]
                                                 [al (plan/map-classpath
                                                     (:classpath-roots (resolve-basis mdir [al] proj))
                                                     modules-abs m2 gl)]))]
                            [m (module-plan ws mdir man base-mapped alias-maps m2 gl modules-abs)])))
         artifacts (into []
                         (pmap (fn [a]
                                 (if (= "mvn" (:kind a))
                                   (let [[id url] (versions/resolve-repo m2 all-repos a)]
                                     (assoc a :repository id :url url))
                                   a))
                               (plan/merge-artifacts (mapcat :raw-artifacts (vals modules)) git-deps)))
        lock-doc {"version" 2
                  "resolver" (resolver-id)
                  "workspace" {"modules" module-dirs
                               "manifest_sha256" (some-> root-man :sha256)}
                  "cooldown" {"default" (or (get cooldown :default) "48h")
                              "repos" (or (get cooldown :repos) {})}
                  "jvm" (when-let [r (get root-data :rig/jvm)]
                          (let [requested (str r)
                                old (get lock :jvm)
                                old-version (when (and (map? old)
                                                       (= requested (get old :requested))
                                                       (get old :version))
                                              (get old :version))]
                            {"vendor" "temurin"
                             "requested" requested
                             "version" old-version}))
                  "graalvm" (when (and (get root-data :rig/jvm)
                                       (some :native (map :build (vals modules))))
                              (let [requested (str (get root-data :rig/jvm))
                                    old (get lock :graalvm)
                                    old-version (when (and (map? old)
                                                           (= requested (get old :requested))
                                                           (get old :version))
                                                  (get old :version))]
                                {"vendor" "graalvm"
                                 "requested" requested
                                 "version" old-version}))
                  "compile-jvm-opts" (when-let [o (get root-data :rig/compile-jvm-opts)]
                                       (vec o))
                  "artifacts" artifacts
                  "skipped" (mapv skip-entry
                                  (distinct (sort-by (juxt :coord :version :reason)
                                                     (:skipped @state))))
                  "modules" (into {} (for [[k v] modules] [k (dissoc v :raw-artifacts)]))}
        refused (:refused @state)]
    (cond-> {"lock" lock-doc}
      (seq refused) (assoc "refused" refused))))
