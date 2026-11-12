(ns rig.resolver.versions
  (:require [clojure.edn :as edn]
            [clojure.java.io :as io]
            [clojure.string :as str]
            [clojure.xml :as xml])
  (:import [org.apache.maven.artifact.versioning DefaultArtifactVersion]))

(def standard-repos
  [["central" {:url "https://repo.maven.apache.org/maven2"}]
   ["clojars" {:url "https://repo.clojars.org"}]])

(defn standard-repos-of
  "standard-repos minus the entries whose id is redeclared in declared
  (:mvn/repos): redeclaring an id replaces the built-in repository, the same
  rule tools.deps applies to its own resolution."
  [declared]
  (filterv (fn [[id _]] (not (contains? declared id))) standard-repos))

(def ^:dynamic *oidc-tokens* :unset)

(defn- oidc-tokens
  "The bearer tokens for :auth :oidc repos: the RIG_REPO_TOKENS environment
  variable, a {repo-id token} map exported by rig (one token per repo, each
  from the gate that fronts it), nil when absent. Tests bind *oidc-tokens*
  (to a map or nil) instead of touching the environment."
  []
  (if (identical? *oidc-tokens* :unset)
    (when-let [s (System/getenv "RIG_REPO_TOKENS")]
      (when-not (str/blank? s)
        (edn/read-string s)))
    *oidc-tokens*))

;; The auth proxy of one :auth :oidc repository (see the section at the
;; bottom of this file): rig (the Go side) runs a local loopback proxy that
;; injects the bearer into the resolver's traffic (re-negotiating it when
;; the registry rejects it), and exports RIG_PROXY_REPOS, a {repo-id
;; proxy-base-url} map.

(def ^:dynamic *proxy-repos* :unset)

(defn proxy-map
  "The RIG_PROXY_REPOS binding exported by rig: {repo-id proxy-base-url},
   {} when absent. Tests bind *proxy-repos* (to a map or nil) instead of
   touching the environment."
  []
  (if (identical? *proxy-repos* :unset)
    (or (some-> (System/getenv "RIG_PROXY_REPOS")
                (edn/read-string))
        {})
    *proxy-repos*))

(defn- probe-base
  "The base url to probe for [id spec]: the proxy base url when id is
   mapped in proxy-map (the proxy carries the bearer and re-negotiates it
   when the registry rejects it), else the repo's own url."
  [proxy id spec]
  (or (get proxy (str id)) (get spec :url)))

(defn- with-auth
  "Sets the bearer Authorization header on conn when the repo spec is marked
  :auth :oidc and a token for repo-id is available; otherwise no-op."
  [conn repo-id spec]
  (when (= :oidc (get spec :auth))
    (when-let [t (get (oidc-tokens) (str repo-id))]
      (.setRequestProperty conn "Authorization" (str "Bearer " t)))))

(defn floating?
  [v]
  (when (string? v)
    (#{:release :latest} (keyword (str/lower-case v)))))

(defn- snapshot?
  "Maven snapshot versions end in -SNAPSHOT. Snapshots are excluded from
  latest-selection: a scaffold or versionless add must pin a release, and
  version ordering ranks 1.1.0-SNAPSHOT above the 1.0.0 it precedes, so
  without the filter the newest snapshot would win."
  [v]
  (str/ends-with? (str/upper-case (str v)) "-SNAPSHOT"))

(defn parse-duration-ms
  [s]
  (let [m (re-matches #"^\s*(\d+)([smhd]?)\s*$" (or s ""))]
    (if m
      (let [n (Long/parseLong (get m 1))]
        (case (or (get m 2) "s")
          "s" (* n 1000)
          "m" (* n 60 1000)
          "h" (* n 3600 1000)
          "d" (* n 24 3600 1000)))
      (throw (ex-info (str "bad duration: " s) {:duration s})))))

(defn- supported-url?
  "true when the JVM can open url directly. Maven transports the JVM
  cannot open (no installed protocol handler) are skipped, not probed
  (probing them would throw MalformedURLException)."
  [url]
  (let [u (try (io/as-url url) (catch Exception _ nil))]
    (and u (or (= "http" (.getProtocol u))
               (= "https" (.getProtocol u))
               (= "file" (.getProtocol u))))))

(defn- http-get
  [url repo-id spec]
  (when (supported-url? url)
    (let [u (io/as-url url)]
      (if (= "file" (.getProtocol u))
        (let [f (io/file (.getPath u))]
          (when (.exists f)
            (with-open [r (io/reader f :encoding "UTF-8")]
              (slurp r))))
        (let [conn (.openConnection u)]
          (try
            (do (.setConnectTimeout conn 15000)
                (.setReadTimeout conn 30000)
                (with-auth conn repo-id spec)
                (let [code (.getResponseCode conn)]
                  (when (<= 200 code 299)
                    (with-open [r (io/reader (.getInputStream conn) :encoding "UTF-8")]
                      (slurp r)))))
            (catch java.io.IOException _ nil)
            (finally (.disconnect conn))))))))

(defn- artifact-file
  [name version ext classifier]
  (str name "-" version (when classifier (str "-" classifier)) "." ext))

(defn- artifact-url
  [base group name version ext classifier]
  (str (str/replace base #"/+$" "")
       "/" (str/replace group #"\." "/")
       "/" name "/" version
       "/" (artifact-file name version ext classifier)))

(defn- m2-repo-of
  [m2dir artifact]
  (let [f (io/file m2dir (str/replace (:group artifact) #"\." "/")
                   (:name artifact) (:version artifact) "_remote.repositories")]
    (when (.exists f)
      (some (fn [line]
              (when-let [m (re-matches (re-pattern (str "^"
                                                        (java.util.regex.Pattern/quote
                                                         (artifact-file (:name artifact)
                                                                        (:version artifact)
                                                                        (:extension artifact)
                                                                        (:classifier artifact)))
                                                        ">([^=]+)=")) line)]
                (get m 1)))
            (line-seq (io/reader f))))))

(defn- head-200?
  [url repo-id spec]
  (when (supported-url? url)
    (let [u (io/as-url url)]
      (if (= "file" (.getProtocol u))
        (.exists (io/file (.getPath u)))
        (let [conn (.openConnection u)]
          (try
            (do (.setConnectTimeout conn 5000)
                (.setReadTimeout conn 5000)
                (.setRequestMethod conn "HEAD")
                (with-auth conn repo-id spec)
                (<= 200 (.getResponseCode conn) 299))
            (catch java.io.IOException _ false)
            (finally (.disconnect conn))))))))

(defn resolve-repo
  "Determines the [repo-id url] serving an mvn artifact. Reads the repo id Maven
  recorded in m2's _remote.repositories first (offline, exact); falls back to a
  repo probe, then to the first repo. The probe goes through the auth proxy
  when the repo id is mapped in it (the proxy carries the bearer); the
  returned url is the repo's own, for lock attribution."
  [m2dir repos artifact]
  (let [proxy (proxy-map)
        url-of (fn [base]
                 (artifact-url base (:group artifact) (:name artifact)
                               (:version artifact) (:extension artifact)
                               (:classifier artifact)))
        id (m2-repo-of m2dir artifact)
        spec (some #(when (= id (first %)) (second %)) repos)]
    (if spec
      [id (url-of (get spec :url))]
      (or (some (fn [[rid sp]]
                  (when (head-200? (url-of (probe-base proxy rid sp)) rid sp)
                    [rid (url-of (get sp :url))]))
                repos)
          (let [[fid fsp] (first repos)]
            [fid (url-of (get fsp :url))])))))

(defn- children
  [node tag]
  (for [c (:content node) :when (= (:tag c) (keyword tag))] c))

(defn- text-of
  [node]
  (first (:content node)))

(defn- ts->ms
  [s]
  (when-let [m (re-matches #"^(\d{4})(\d{2})(\d{2})(\d{2})(\d{2})(\d{2})" (or s ""))]
    (.getTimeInMillis
     (doto (java.util.GregorianCalendar.)
       (.clear)
       (.setTimeZone (java.util.TimeZone/getTimeZone "UTC"))
       (.set (Integer/parseInt (get m 1))
             (dec (Integer/parseInt (get m 2)))
             (Integer/parseInt (get m 3))
             (Integer/parseInt (get m 4))
             (Integer/parseInt (get m 5))
             (Integer/parseInt (get m 6)))))))

(defn- plain-metadata
  [url repo-id spec]
  (when-let [text (http-get url repo-id spec)]
    (let [root (xml/parse (org.xml.sax.InputSource. (java.io.StringReader. text)))
          vrs (first (children root :versioning))]
      (when vrs
        (let [vs (first (children vrs :versions))]
          {:versions (when vs
                       (->> (children vs :version)
                            (map text-of)
                            (remove nil?)
                            distinct
                            vec))
           :latest (->> (children vrs :latest) first text-of)
           :last-updated (->> (children vrs :lastUpdated) first text-of ts->ms)})))))

(defn- repo-version-timestamps
  [url repo-id spec]
  (when-let [text (http-get url repo-id spec)]
    (let [root (xml/parse (org.xml.sax.InputSource. (java.io.StringReader. text)))
          vrs (first (children root :versioning))]
      (when vrs
        (let [vs (first (children vrs :versions))]
          (when vs
            (into {}
                  (for [v (children vs :version)
                        :let [val (->> (children v :value) first text-of)
                              ms (->> (children v :updated) first text-of ts->ms)]
                        :when (and val ms)]
                    [val ms]))))))))

(defn repo-metadata
  "Returns {:by-version {v {:published-at ms|nil :repo id}} :latest #{v ...}}."
  [coord repos]
  (let [proxy (proxy-map)
        [group name] (str/split coord #"/" 2)
        base-path (str (str/replace group "." "/") "/" name "/")]
    (reduce (fn [{:keys [by-version latest] :as acc} [id spec]]
              (let [base (str (str/replace (probe-base proxy id spec) #"/+$" "") "/" base-path)]
                (if-let [md (plain-metadata (str base "maven-metadata.xml") id spec)]
                  (let [ts (repo-version-timestamps (str base (str "maven-metadata-" id ".xml")) id spec)]
                    (-> acc
                        (update :by-version
                                (fn [m]
                                  (reduce (fn [mm v]
                                            (if (contains? mm v)
                                              mm
                                              (assoc mm v {:published-at (or (get ts v)
                                                                             (:last-updated md))
                                                           :repo id})))
                                          m
                                          (:versions md))))
                        (update :latest
                                (fn [s] (if (:latest md) (conj s (:latest md)) s)))))
                  acc)))
            {:by-version {} :latest #{}}
            repos)))

(defn vcmp
  [a b]
  (.compareTo (DefaultArtifactVersion. a) (DefaultArtifactVersion. b)))

(defn newest
  "The newest of the versions by Maven ordering (vcmp); the input must be
  non-empty."
  [vs]
  (reduce (fn [a b] (if (pos? (vcmp a b)) a b)) vs))

(defn iso
  [ms]
  (when ms (.toString (java.time.Instant/ofEpochMilli ms))))

(def default-cooldown
  "The cooldown applied when no policy (request or manifest) declares one."
  "48h")

(defn cooldown-for
  [repo cooldown]
  (or (get (or (get cooldown :repos) {}) repo)
      (or (get cooldown :default) default-cooldown)))

(defn- all-cooldowns-zero?
  [cooldown]
  (let [d (parse-duration-ms (or (get cooldown :default) default-cooldown))
        rs (map parse-duration-ms (vals (or (get cooldown :repos) {})))]
    (every? zero? (cons d rs))))

(defn select-versions
  "Per floating coord: 1) requirement-overrides win (recorded \"explicit\"),
  2) an existing lock pin is kept, 3) with all cooldowns
  zero the version is left for native tools.deps resolution, 4) otherwise the
  newest candidate older than the per-repo cooldown wins; younger candidates
  are recorded \"cooldown\", a forced pick \"forced\", and a coord with no
  eligible candidate goes to :refused.

  Returns {:selected {coord version} :skipped [{...}] :refused [{...}]
           :changed? bool}."
  [project-data repos cooldown force pins overrides now-ms]
  (let [all-deps (concat (for [[c s] (get project-data :deps {})] [c s])
                         (for [al (keys (get project-data :aliases {}))
                               [c s] (get-in project-data [:aliases al :extra-deps] {})]
                           [c s]))
        req-of (fn [c]
                 (first (for [[cc s] all-deps :when (= cc c)] (get s :mvn/version))))
        coords (distinct (for [[c s] all-deps
                               :when (or (floating? (get s :mvn/version))
                                         (contains? overrides (str c)))]
                           c))
        cooldown-str (fn [repo] (cooldown-for repo cooldown))
        all-zero? (all-cooldowns-zero? cooldown)
        memo (atom {}) ;; memo across coords: repo-metadata hits the network
        step (fn [{:keys [selected skipped refused] :as st} c]
               (let [override (get overrides (str c))
                     pin (get pins (str c))
                     req (req-of c)]
                 (cond
                   override
                   (cond-> (assoc st :selected (assoc selected c override))
                     (not= pin override)
                     (update :skipped conj {:coord c :version override :reason "explicit"
                                            :published-at nil :cooldown nil}))

                   pin
                   (assoc st :selected (assoc selected c pin))

                   all-zero?
                   st

                   :else
                   (do (when-not (contains? @memo c)
                         (swap! memo assoc c (repo-metadata (str c) repos)))
                       (let [md (get @memo c)
                             byv (get md :by-version)
                             cands (if (= :latest (keyword (str/lower-case (or req ""))))
                                     (if (seq (get md :latest))
                                       (set (get md :latest))
                                       (keys byv))
                                     (keys byv))
                             age-ok (fn [v]
                                      (let [d (get byv v)
                                            pa (:published-at d)]
                                        (and pa (<= pa (- now-ms (parse-duration-ms (cooldown-str (:repo d))))))))
                             elig (fn [v]
                                    (or (true? force) (age-ok v)))
                             eligible (filter elig cands)
                             chosen (when (seq eligible) (newest eligible))
                             st1 (if chosen
                                   (cond-> (assoc st :selected (assoc selected c chosen))
                                     (and (true? force) (not (age-ok chosen)))
                                     (update :skipped conj {:coord c :version chosen :reason "forced"
                                                            :published-at (iso (get-in byv [chosen :published-at]))
                                                            :cooldown (cooldown-str (get-in byv [chosen :repo]))}))
                                   (update st :refused conj {:coord c :reason "cooldown"
                                                             :cooldown (or (get cooldown :default) default-cooldown)}))]
                         (update st1 :skipped
                                 (fn [sk]
                                   (into sk
                                         (for [v (sort cands)
                                               :when (and (not (age-ok v))
                                                          (not (= v chosen)))]
                                           {:coord c :version v :reason "cooldown"
                                            :published-at (iso (get-in byv [v :published-at]))
                                            :cooldown (cooldown-str (get-in byv [v :repo]))})))))))))]
    (let [res (reduce step {:selected {} :skipped [] :refused []} coords)]
      (assoc res :changed? (boolean (seq (:selected res)))))))

(defn select-latest
  "Picks the newest version of coord older than its repo cooldown (force
  bypasses the cooldown and the snapshot exclusion). Snapshots are never
  selected without force: versionless adds and scaffolds must pin a
  release. Returns {:version v :skipped [...]} (skipped in the lock's
  skipped-list shape) or {:reason \"not-found\"} when no repo serves the
  coord (a repo that serves only snapshots counts as serving it not).
  The single-coordinate form of select-versions, for callers with no
  manifest to select from: edit-dep's versionless add and the latest op."
  [coord-str repos cooldown force now-ms]
  (let [md (repo-metadata coord-str repos)
        byv (get md :by-version)
        all (if (seq (:latest md)) (set (:latest md)) (keys byv))
        cands (if (true? force) all (set (remove snapshot? all)))
        elig (fn [v]
               (let [d (get byv v)
                     pa (:published-at d)]
                 (and pa
                      (<= pa (- now-ms
                                (parse-duration-ms
                                 (cooldown-for (:repo d) cooldown)))))))
        chosen (when (some (fn [v] (or (true? force) (elig v))) cands)
                 (newest (filter (fn [v] (or (true? force) (elig v))) cands)))
        skipped (concat
                 (when (and (true? force) chosen (not (elig chosen)))
                   [{:coord (symbol coord-str)
                     :version chosen
                     :reason "forced"
                     :published-at (iso (get-in byv [chosen :published-at]))
                     :cooldown (cooldown-for (get-in byv [chosen :repo]) cooldown)}])
                 (for [v (sort cands)
                       :when (not (elig v))]
                   {:coord (symbol coord-str)
                    :version v
                    :reason "cooldown"
                    :published-at (iso (get-in byv [v :published-at]))
                    :cooldown (cooldown-for (get-in byv [v :repo]) cooldown)}))]
    (if (empty? cands)
      {:reason "not-found"}
      {:version chosen :skipped skipped})))

(defn apply-versions
  "Substitutes exact versions for the selected coords in the manifest map.
   Only floating (RELEASE/LATEST) requirements are substituted, unless the
   coord is explicitly overridden; concrete requirements keep their pins."
  [project-data selected overrides]
  (let [replace? (fn [c s]
                   (and (map? s) (contains? selected c)
                        (or (floating? (get s :mvn/version))
                            (contains? overrides (str c)))))
        rewrite-deps (fn [deps]
                       (into {} (for [[c s] deps]
                                  [c (if (replace? c s)
                                       (assoc s :mvn/version (get selected c))
                                       s)])))
        rewrite-aliases (fn [aliases]
                          (update-vals aliases
                                       (fn [a]
                                         (if (map? a)
                                           (update a :extra-deps (fn [e] (when e (rewrite-deps e))))
                                           a))))]
    (-> project-data
        (update :deps (fn [d] (when d (rewrite-deps d))))
        (update :aliases (fn [al] (when al (rewrite-aliases al)))))))

;; --- Auth proxy (see *proxy-repos* above): for :auth :oidc repos, the
;; --- :url of the affected :mvn/repos entries is rewritten to the proxy
;; --- base url for create-basis/calc-trace. Repo ids are kept, so lock
;; --- attribution is unchanged.

(defn with-proxy-repos
  "data with the :url of each :mvn/repos entry whose id is mapped in
   proxy-map replaced by the proxy base url; data unchanged otherwise."
  [data]
  (let [proxy (proxy-map)
        repos (get data :mvn/repos)]
    (if (and proxy repos (some (fn [[id _]] (get proxy (str id))) repos))
      (assoc data :mvn/repos
             (into {} (for [[id spec] repos]
                        [id (if (get proxy (str id))
                              (assoc spec :url (get proxy (str id)))
                              spec)])))
      data)))
