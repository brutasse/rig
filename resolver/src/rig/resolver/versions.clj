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
          (with-open [in (io/input-stream f)]
            (String. (.readAllBytes in) "UTF-8"))))
      (let [conn (.openConnection u)]
        (try
          (do (.setConnectTimeout conn 15000)
              (.setReadTimeout conn 30000)
              (with-auth conn repo-id spec)
              (let [code (.getResponseCode conn)]
                (when (<= 200 code 299)
                  (with-open [in (.getInputStream conn)]
                    (String. (.readAllBytes in) "UTF-8")))))
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
  repo probe, then to the first repo."
  [m2dir repos artifact]
  (let [url-of (fn [spec]
                 (artifact-url (get spec :url) (:group artifact) (:name artifact)
                               (:version artifact) (:extension artifact)
                               (:classifier artifact)))
        id (m2-repo-of m2dir artifact)
        spec (some #(when (= id (first %)) (second %)) repos)]
    (if spec
      [id (url-of spec)]
      (or (some (fn [[rid sp]]
                  (let [u (url-of sp)]
                    (when (head-200? u rid sp) [rid u])))
                repos)
          (let [[fid fsp] (first repos)]
            [fid (url-of fsp)])))))

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
  (let [[group name] (str/split coord #"/" 2)
         base-path (str (str/replace group "." "/") "/" name "/")]
    (reduce (fn [{:keys [by-version latest] :as acc} [id spec]]
              (let [base (str (str/replace (get spec :url) #"/+$" "") "/" base-path)]
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

(defn iso
  [ms]
  (when ms (.toString (java.time.Instant/ofEpochMilli ms))))

(defn cooldown-for
  [repo cooldown]
  (or (get (or (get cooldown :repos) {}) repo)
      (or (get cooldown :default) "48h")))

(defn- all-cooldowns-zero?
  [cooldown]
  (let [d (parse-duration-ms (or (get cooldown :default) "48h"))
        rs (map (fn [s] (parse-duration-ms s)) (vals (or (get cooldown :repos) {})))]
    (apply zero? (cons d rs))))

(defn select-versions
  "Per floating coord: 1) requirement-overrides win (recorded \"explicit\"),
  2) an existing lock pin is kept when respect-pins?, 3) with all cooldowns
  zero the version is left for native tools.deps resolution, 4) otherwise the
  newest candidate older than the per-repo cooldown wins; younger candidates
  are recorded \"cooldown\", a forced pick \"forced\", and a coord with no
  eligible candidate goes to :refused.

  Returns {:selected {coord version} :skipped [{...}] :refused [{...}]
           :changed? bool}."
  [project-data repos cooldown force pins overrides respect-pins? now-ms]
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
        zero? (all-cooldowns-zero? cooldown)
        meta (atom {})
        selected (atom {})
        skipped (atom [])
        refused (atom [])]
    (doseq [c coords]
       (let [override (get overrides (str c))
             pin (get pins (str c))
             req (req-of c)]
         (cond
           override
           (do (swap! selected assoc c override)
               (when (not= pin override)
                 (swap! skipped conj {:coord c :version override :reason "explicit"
                                      :published-at nil :cooldown nil})))

          (and pin respect-pins?)
          (swap! selected assoc c pin)

          zero?
          nil

          :else
           (do (when-not (contains? @meta c)
                 (swap! meta assoc c (repo-metadata (str c) repos)))
               (let [md (get @meta c)
                     byv (get md :by-version)
                     cands (if (= :latest (keyword (str/lower-case (or req ""))))
                             (if (seq (get md :latest))
                               (into #{} (get md :latest))
                               (keys byv))
                             (keys byv))
                     age-ok (fn [v]
                             (let [d (get byv v)
                                   pa (:published-at d)]
                               (and pa (<= pa (- now-ms (parse-duration-ms (cooldown-str (:repo d))))))))
                     elig (fn [v]
                            (or (true? force) (age-ok v)))
                     eligible (filter elig cands)
                     chosen (when (seq eligible)
                              (reduce (fn [a b] (if (pos? (vcmp a b)) a b)) eligible))]
                 (if chosen
                   (do (swap! selected assoc c chosen)
                       (when (and (true? force) (not (age-ok chosen)))
                         (swap! skipped conj {:coord c :version chosen :reason "forced"
                                              :published-at (iso (get-in byv [chosen :published-at]))
                                              :cooldown (cooldown-str (get-in byv [chosen :repo]))})))
                   (swap! refused conj {:coord c :reason "cooldown"
                                        :cooldown (or (get cooldown :default) "48h")}))
                 (doseq [v (sort cands)
                         :when (and (not (age-ok v))
                                    (not (= v chosen)))]
                   (swap! skipped conj {:coord c :version v :reason "cooldown"
                                        :published-at (iso (get-in byv [v :published-at]))
                                        :cooldown (cooldown-str (get-in byv [v :repo]))})))))))
     {:selected @selected
      :skipped @skipped
      :refused @refused
      :changed? (boolean (seq @selected))}))

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
                          (into {} (for [[k a] aliases]
                                     [k (if (map? a)
                                          (update a :extra-deps (fn [e] (when e (rewrite-deps e))))
                                          a)])))]
    (-> project-data
        (update :deps (fn [d] (when d (rewrite-deps d))))
        (update :aliases (fn [al] (when al (rewrite-aliases al)))))))

;; --- Auth proxy: for :auth :oidc repos, rig (the Go side) runs a local
;; --- loopback proxy that injects the bearer into the resolver's traffic.
;; --- It exports RIG_PROXY_REPOS, a {repo-id proxy-base-url} map, and we
;; --- rewrite the :url of the affected :mvn/repos entries so that
;; --- create-basis/calc-trace resolve through it. Repo ids are kept, so
;; --- lock attribution is unchanged.

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
