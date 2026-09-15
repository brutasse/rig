(ns rig.resolver.edit
  (:require [clojure.java.io :as io]
            [clojure.string :as str]
            [cljfmt.core :as cljfmt]
            [rewrite-clj.node :as n]
            [rewrite-clj.zip :as z]
            [rewrite-clj.zip.seqz :as s]
            [rig.resolver.manifest :as manifest]
            [rig.resolver.resolve :as resolve]
            [rig.resolver.versions :as versions]))

(defn- format!
  [text]
  (let [f (cljfmt/reformat-string text)]
    (if (str/ends-with? f "\n") f (str f "\n"))))

(defn- edit-file
  [file f]
  (let [text (slurp file)
        out (format! (-> text z/of-string f z/root-string))]
    (when (not= text out)
      (spit file out)
      true)))

(defn- multi-line?
  [zloc]
  (some-> zloc z/string (str/includes? "\n")))

(defn- create-section
  [owner key entry]
  (-> owner
      (cond-> (multi-line? owner)
              (z/append-child* (n/newline-node "\n")))
      (s/assoc key entry)
      (s/get key)))

(defn- edit-deps-section
  [deps-map coord req]
  (let [existing (s/get deps-map coord)]
    (cond
      (and existing (nil? req))
      (-> existing z/remove z/remove)

      (and existing req)
      (let [ver-z (s/get existing :mvn/version)]
        (if (nil? ver-z)
          (throw (ex-info (str "dep " (str coord) " has no :mvn/version") {}))
          (-> ver-z (z/edit (fn [_] req)))))

      (and (nil? existing) req)
      (-> deps-map
          (cond-> (multi-line? deps-map)
                  (z/append-child* (n/newline-node "\n")))
          (z/append-child* (n/token-node coord))
          (z/append-child* (n/whitespace-node " "))
          (z/append-child* (n/map-node [(n/keyword-node :mvn/version)
                                        (n/whitespace-node " ")
                                        (n/string-node req)])))

      :else
      deps-map)))

(defn- edit-rig-deps
  [ztop coord req]
  (let [rdm (or (s/get ztop :rig/deps)
                (when req (create-section ztop :rig/deps {coord req})))]
    (when rdm
      (let [existing (s/get rdm coord)]
        (cond
          (and existing (nil? req))
          (-> existing z/remove z/remove)

          (and existing req)
          (if (map? (n/sexpr (z/node existing)))
            (let [ver-z (s/get existing :mvn/version)]
              (if (nil? ver-z)
                (throw (ex-info (str "shared dep " (str coord) " has no :mvn/version") {}))
                (-> ver-z (z/edit (fn [_] req)))))
            (-> existing (z/edit (fn [_] req))))

          (and (nil? existing) req)
          (-> rdm
              (cond-> (multi-line? rdm)
                      (z/append-child* (n/newline-node "\n")))
              (z/append-child* (n/token-node coord))
              (z/append-child* (n/whitespace-node " "))
              (z/append-child* (n/map-node [(n/keyword-node :mvn/version)
                                            (n/whitespace-node " ")
                                            (n/string-node req)])))

          :else
          rdm)))))

(defn- edit-module-section
  [ztop coord req alias]
  (if (nil? alias)
    (let [dm (or (s/get ztop :deps)
                 (when req (create-section ztop :deps {coord {:mvn/version req}})))]
      (when dm (edit-deps-section dm coord req)))
    (let [am (some-> (s/get ztop :aliases) (s/get (keyword alias)))]
      (if (nil? am)
        (throw (ex-info (str "alias :" (str alias) " not found") {}))
        (let [xdm (or (s/get am :extra-deps)
                      (when req (create-section am :extra-deps {coord {:mvn/version req}})))]
          (when xdm (edit-deps-section xdm coord req)))))))

(defn- rel-deps [m] (if (= m ".") "deps.edn" (str m "/deps.edn")))

(defn- select-latest
  "Picks the newest version of coord older than its repo cooldown (force
  bypasses). Returns {:version v :skipped [...]} (skipped in the lock's
  skipped-list shape) or {:reason \"not-found\"} when no repo serves the
  coord."
  [coord-str repos cooldown force now-ms]
  (let [md (versions/repo-metadata coord-str repos)
        byv (get md :by-version)
        cands (if (seq (:latest md)) (into #{} (:latest md)) (keys byv))
        elig (fn [v]
               (let [d (get byv v)
                     pa (:published-at d)]
                 (and pa
                      (<= pa (- now-ms
                                (versions/parse-duration-ms
                                 (versions/cooldown-for (:repo d) cooldown)))))))
        chosen (when (some (fn [v] (or (true? force) (elig v))) cands)
                 (reduce (fn [a b] (if (pos? (versions/vcmp a b)) a b))
                         (filter (fn [v] (or (true? force) (elig v))) cands)))
        skipped (concat
                  (when (and (true? force) chosen (not (elig chosen)))
                    [{:coord (symbol coord-str)
                      :version chosen
                      :reason "forced"
                      :published-at (versions/iso (get-in byv [chosen :published-at]))
                      :cooldown (versions/cooldown-for (get-in byv [chosen :repo]) cooldown)}])
                  (for [v (sort cands)
                        :when (not (elig v))]
                    {:coord (symbol coord-str)
                     :version v
                     :reason "cooldown"
                     :published-at (versions/iso (get-in byv [v :published-at]))
                     :cooldown (versions/cooldown-for (get-in byv [v :repo]) cooldown)}))]
    (if (empty? cands)
      {:reason "not-found"}
      {:version chosen :skipped skipped})))

(defn- apply-edit
  [request extra-skipped]
  (let [ws (str (:workspace request))
        args (or (:args request) {})
        coord-str (get args :coord)
        coord (symbol coord-str)
        req (get args :requirement)
        alias (get args :alias)
        shared (true? (get args :shared))
        shared-only (true? (get args :shared-only))
        module (or (get args :module) ".")
        root-file (io/file ws "deps.edn")
        mod-file (if (= module ".") root-file (io/file ws module "deps.edn"))
        action (if (nil? req) "remove" "set")
        edits (atom [])]

    (when-not (.exists root-file)
      (throw (ex-info (str "workspace manifest not found: " (.getPath root-file)) {})))
    (when (and (not shared-only) (not (.exists mod-file)))
      (throw (ex-info (str "module manifest not found: " (.getPath mod-file)) {})))

    (when (not shared-only)
      (when (edit-file mod-file (fn [zt] (or (edit-module-section zt coord req alias) zt)))
        (swap! edits conj {"file" (rel-deps module)
                           "action" action
                           "coord" coord-str
                           "requirement" req})))

    (when shared
      (when (edit-file root-file (fn [zt] (or (edit-rig-deps zt coord req) zt)))
        (swap! edits conj {"file" "deps.edn"
                           "action" action
                           "coord" coord-str
                           "requirement" req})))

    (when (and shared (not shared-only))
      (let [root-data (or (:data (manifest/read-manifest ws)) {})
            others (remove #(= (str %) module) (manifest/modules-of root-data))]
        (doseq [m others]
          (let [f (if (= m ".") root-file (io/file ws m "deps.edn"))]
            (when (.exists f)
              (let [text (slurp f)
                    zt (z/of-string text)
                    dm (s/get zt :deps)
                    entry (when dm (s/get dm coord))]
                (when (and entry (s/get entry :mvn/version))
                  (let [out (format! (z/root-string (edit-deps-section dm coord req)))]
                    (when (not= text out)
                      (spit f out)
                      (swap! edits conj {"file" (rel-deps (str m))
                                         "action" action
                                         "coord" coord-str
                                         "requirement" req}))))))))))

    (let [res (resolve/resolve-lock
               (cond-> request
                 (and req (not shared-only) (true? (get args :explicit?)))
                 (assoc :args (assoc (or (:args request) {})
                                     :requirement-overrides {coord-str req}))))
          lock (cond-> (get res "lock")
                  (seq extra-skipped)
                  (update "skipped" (fn [prev]
                                      (distinct (sort-by (juxt :coord :version :reason)
                                                         (concat prev
                                                                  (map resolve/skip-entry extra-skipped)))))))]
      (merge res {"edits" (vec @edits)
                  "lock" lock}))))

(defn edit-dep
  [request]
  (let [args (or (:args request) {})
        coord-str (get args :coord)
        req (get args :requirement)
        latest? (= "latest" (str/lower-case (or req "")))]
    (if (not latest?)
      (apply-edit (assoc-in request [:args :explicit?] true) nil)
      (let [ws (str (:workspace request))
            root-data (or (:data (manifest/read-manifest ws)) {})
            sel (select-latest coord-str (resolve/all-repos ws)
                               (resolve/cooldown-of args root-data)
                               (true? (get args :force))
                               (System/currentTimeMillis))]
        (cond
          (= "not-found" (:reason sel))
          (throw (ex-info (str "coordinate not found in any repository: " coord-str) {}))
          (nil? (:version sel))
          {"refused" [{:coord (symbol coord-str)
                       :reason "cooldown"
                       :cooldown (or (get (resolve/cooldown-of args root-data) :default) "48h")}]}
          :else
          (apply-edit (assoc-in request [:args :requirement] (:version sel))
                      (:skipped sel)))))))
