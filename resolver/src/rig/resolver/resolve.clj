(ns rig.resolver.resolve
  (:require [cheshire.core :as json]
            [clojure.java.io :as io]
            [clojure.string :as str]
            [clojure.tools.deps :as td]
            [rig.resolver.manifest :as manifest]
            [rig.resolver.plan :as plan]
            [rig.resolver.versions :as versions]))

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

(defn- build-plan
  [data version]
  (let [target (manifest/target-dir data)
        ;; Default uberjar file: mirror the plain jar naming (last lib segment,
        ;; or "app" when no lib is declared; no version suffix when absent).
        artifact (or (some-> (manifest/lib data) str (str/split #"/") last) "app")
        default (str target "/" artifact (when (seq version) (str "-" version)) ".jar")
        ns-compile (when-let [nsc (manifest/ns-compile data)] (vec (map name nsc)))]
    (cond-> {:src-dirs (or (manifest/src-dirs data) ["src" "resources"])
             :java-src-dirs (or (manifest/java-src-dirs data) [])
             :class-dir (str target "/classes")
             :jar true
             :uberjar (when (manifest/uberjar? data)
                       {"file" (or (manifest/uberjar-file data) default)
                        "main" (manifest/main-ns data)
                        "opts" (or (manifest/uber-opts data) {})})}
      (seq ns-compile) (assoc :ns-compile ns-compile))))

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

(defn resolver-id
  []
  (let [sha (System/getenv "RIG_RESOLVER_SHA")]
    {"lib" "io.github.brutasse/rig-resolver"
     "version" (or (System/getenv "RIG_RESOLVER_VERSION") "0.1.0")
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
        modules-abs (into {} (for [m module-dirs]
                               [m (let [p (if (= m ".") ws (str (io/file ws m)))]
                                    (try (some-> p (io/file) (.getCanonicalPath) str)
                                         (catch Exception _ p)))]))
        m2 (m2dir)
        gl (gitlibs-dir)
        manifests (into {ws root-man}
                        (for [m (remove #(.equals % ".") module-dirs)]
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
                      (for [m module-dirs]
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
        lock-doc {"version" 1
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
                  "artifacts" artifacts
                  "skipped" (mapv skip-entry
                                  (distinct (sort-by (juxt :coord :version :reason)
                                                     (:skipped @state))))
                  "modules" (into {} (for [[k v] modules] [k (dissoc v :raw-artifacts)]))}
        refused (:refused @state)]
    (cond-> {"lock" lock-doc}
      (seq refused) (assoc "refused" refused))))
