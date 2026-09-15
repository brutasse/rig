(ns rig.resolver.check
  "Consistency report: lock vs manifests. Reads only — no resolution, no
  network, never writes the lock. Problem kinds:
    stale-lock       (error) requirement not satisfied by the pin
    conflict         (error) two modules require mutually unsatisfiable versions
    drift            (warn)  module requirement differs from the workspace one
    floating-version (error) manifest declares RELEASE/LATEST
    no-lib           (error) publish enabled without a :rig/lib coordinate
    unknown-repo     (error) lock pins an artifact via a repo no manifest declares"
  (:require [cheshire.core :as json]
            [clojure.java.io :as io]
            [clojure.string :as str]
            [rig.resolver.manifest :as manifest]
            [rig.resolver.versions :as versions])
  (:import [org.apache.maven.artifact.versioning DefaultArtifactVersion]))

(defn- read-lock
  [ws lock]
  (cond
    (nil? lock) {}
    (string? lock)
    (let [f (io/file ws lock)]
      (if (.exists f) (json/parse-string (slurp f) true) {}))
    :else lock))

(defn- classpath-pins
  "coord -> [version …] from a module plan's classpaths (base plus aliases);
  entries look like \"group/name:version[:classifier]:extension\"."
  [plan]
  (reduce (fn [pins e]
            (if (string? e)
              (let [[coord version & _] (str/split e #":")]
                (if version (update pins coord conj version) pins))
              pins))
           {}
           (concat (get plan :classpath [])
                   (mapcat (fn [a] (get a :classpath))
                           (vals (get plan :aliases {}))))))

(defn- module-key
  "Module dir from a lock :modules key (a keyword after JSON keyify, or a
  string in-memory)."
  [m]
  (if (keyword? m)
    (if-let [n (namespace m)] (str n "/" (name m)) (name m))
    (str m)))

(defn- pins-of
  "[{module-dir {coord [version …]}} {coord [version …] over all modules}]
  from the lock's per-module classpaths (keys keyified by the JSON reader)."
  [lock]
  (let [plans (get lock :modules {})
        per (into {} (for [[m p] plans]
                       [(module-key m) (classpath-pins p)]))]
    [per (if (seq per) (apply merge-with concat (vals per)) {})]))

(defn- repo-ids-of
  [lock]
  (into #{}
        (for [a (get lock :artifacts [])
              :when (and (= "mvn" (get a :kind)) (get a :repository))]
          (get a :repository))))

(defn- reqs-of
  "All [coord version] mvn requirements of data (:deps and alias :extra-deps)."
  [data]
  (into []
        (distinct
         (for [[c s] (concat (seq (get data :deps {}))
                             (for [al (keys (get data :aliases {}))
                                   [c s] (get-in data [:aliases al :extra-deps] {})]
                               [c s]))
              :when (and (map? s) (get s :mvn/version))]
           [(str c) (str (get s :mvn/version))]))))

(defn- managed-of
  [data]
  (into {}
        (for [[c s] (get data :rig/deps)
              :let [v (if (map? s) (get s :mvn/version) (when (string? s) s))]
              :when v]
          [(str c) (str v)])))

(defn- satisfied?
  "True when requirement req (exact version, or Maven range like [1.0,2.0))
  is satisfied by pinned version pin."
  [req pin]
  (let [req (str req) pin (str pin)]
    (if-let [[_ lo-inc lo hi-inc hi] (re-matches #"^[\[(]([^,]*),([^)\]]*)[)\]]$" req)]
      (let [pv (DefaultArtifactVersion. pin)
            lo-ok (or (str/blank? lo)
                      (let [c (.compareTo pv (DefaultArtifactVersion. lo))]
                        (if (= lo-inc "[") (>= c 0) (pos? c))))
            hi-ok (or (str/blank? hi)
                      (let [c (.compareTo pv (DefaultArtifactVersion. hi))]
                        (if (= hi-inc "]") (<= c 0) (neg? c))))]
        (and lo-ok hi-ok))
      (= req pin))))

(defn check
  [request]
  (let [ws (str (:workspace request))
        lock (read-lock ws (get request :lock))
        [per-pins all-pins] (pins-of lock)
        root-man (manifest/read-manifest ws)
        root-data (or (:data root-man) {})
        module-dirs (remove #(.equals % ".") (manifest/modules-of root-data))
        mod-dir (fn [m] (str (io/file ws m)))
        manifests (into {"." root-man}
                         (for [m module-dirs] [m (manifest/read-manifest (mod-dir m))]))
        managed (managed-of root-data)
        module-reqs (into {}
                          (for [[m man] (dissoc manifests ".")
                                :when (:data man)]
                            [m (reqs-of (:data man))]))
         ;; [module coord version scope] — scope :own checks a requirement
         ;; against the module's own locked classpaths; scope :workspace
         ;; (:rig/deps shared requirements) against any locked classpath.
         sites (concat
                (for [[c v] (reqs-of root-data)] ["." c v :own])
                (for [[c v] managed] ["." c v :workspace])
                (for [m module-dirs
                      [c v] (get module-reqs m [])]
                  [m c v :own]))
        pats (atom [])
        add (fn [severity kind module coord message]
              (swap! pats conj {:severity severity
                                :kind kind
                                :module module
                                :coord coord
                                 :message message}))]

        (doseq [[m c v _] sites
                :when (versions/floating? v)]
          (add "error" "floating-version" m c
               (str "manifest uses \"" v "\"; run `rig update " c
                    "` to pin an exact version")))

        (doseq [[m c v scope] sites
                :when (not (versions/floating? v))
                :let [p (get (if (= scope :workspace) all-pins
                               (get per-pins m {})) c)]
                :when (not (and (seq p) (some #(satisfied? v %) p)))]
          (add "error" "stale-lock" m c
               (if (seq p)
                 (str (if (= scope :workspace) "workspace requires"
                        "manifest requires")
                      " \"" v "\"; lock pins \"" (first p)
                      "\" — run rig update")
                 (if (= scope :workspace)
                   (str "workspace requires \"" v
                        "\"; no module pins it — run rig update")
                   (str "manifest requires \"" v
                        "\"; lock has no pin — run rig lock")))))

        (doseq [m module-dirs
                [c v] (get module-reqs m [])
                :let [w (get managed c)]
                :when (and (some? w)
                           (not (versions/floating? v))
                           (not (versions/floating? w))
                           (not= v w))]
          (add "warn" "drift" m c
               (str "module requires \"" v "\", workspace requires \"" w "\"")))

          (let [per-coord (reduce (fn [acc [m c v _]]
                                  (if (or (= m ".") (versions/floating? v))
                                    acc
                                    (update acc c (fn [mm]
                                                    (assoc mm m (conj (get mm m) v))))))
                                {}
                                sites)
              ms-of (fn [mods] (sort (keys mods)))]
          (doseq [[c mods] (sort-by key per-coord)
                  :let [ms (ms-of mods)
                        bad (some (fn [[i j]]
                                    (let [m1 (nth ms i) m2 (nth ms j)]
                                      (some identity
                                           (for [v1 (get mods m1)
                                                 v2 (get mods m2)
                                                 :when (and (not (satisfied? v1 v2))
                                                            (not (satisfied? v2 v1)))]
                                               [m1 v1 m2 v2]))))
                                  (for [i (range (count ms))
                                        j (range (inc i) (count ms))] [i j]))]
                  :when bad]
            (let [[m1 v1 m2 v2] bad]
              (add "error" "conflict" m1 c
                   (str "requirements \"" v1 "\" (" m1 ") and \"" v2
                        "\" (" m2 ") are incompatible")))))

        (doseq [m (cons "." module-dirs)
                :let [d (or (:data (get manifests m)) {})]
                :when (and (manifest/publish? d) (not (manifest/lib d)))]
          (add "error" "no-lib" m ""
               "publish is enabled but the module has no :rig/lib coordinate"))

         (let [declared (into #{"central" "clojars"}
                              (for [m (keys manifests)
                                    :let [d (or (:data (get manifests m)) {})]
                                    id (keys (get d :mvn/repos {}))]
                                id))]
          (doseq [r (sort (repo-ids-of lock))
                  :when (not (declared r))]
            (add "error" "unknown-repo" "." r
                 (str "lock pins artifacts via repo \"" r
                      "\" which no manifest declares under :mvn/repos"))))

        (let [problems (distinct (sort-by (juxt :module :coord :kind) @pats))]
          {:ok (every? #(= "warn" (:severity %)) problems)
           :problems (vec problems)})))
