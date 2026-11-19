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

(defn- edit-section
  "Edits the coord's requirement in the named section of a manifest node:
  \"pool\" the root's :rig/deps (string or map entries), else a deps
  section — \"deps\" (the module's :deps) or an alias section (\"alias-deps\",
  \"extra-deps\" or \"replace-deps\" under :aliases/<alias>/, which needs
  the alias). req nil removes the entry. Returns the edited node, or nil
  when nothing was edited."
  [zt section alias coord req]
  (case section
    "pool"
    (edit-rig-deps zt coord req)

    (let [owner (if (nil? alias)
                  zt
                  (or (some-> (s/get zt :aliases) (s/get (keyword alias)))
                      (throw (ex-info (str "alias :" (str alias) " not found") {}))))
          key (case section
                "deps" :deps
                "alias-deps" :deps
                "replace-deps" :replace-deps
                :extra-deps)
          dm (or (s/get owner key)
                 (when req (create-section owner key {coord {:mvn/version req}})))]
      (when dm (edit-deps-section dm coord req)))))

(defn- rel-deps [m] (if (= m ".") "deps.edn" (str m "/deps.edn")))

(defn apply-edits
  "Applies [{:file f :coord c :requirement req :section s :alias a} …] to
  the workspace's manifests (file workspace-relative, section as
  edit-section) and returns the edit records the edit-dep response
  carries, one per file actually changed. req nil removes the entry.
  The audit-fix op uses it for a batch of edits across several files in
  one call, ahead of its single re-lock."
  [ws edits]
  (reduce (fn [records e]
            (let [f (io/file ws (get e :file))
                  changed? (edit-file (str f)
                                      (fn [zt]
                                        (or (edit-section zt (get e :section)
                                                          (get e :alias)
                                                          (symbol (get e :coord))
                                                          (get e :requirement))
                                            zt)))]
              (if changed?
                (conj records {"file" (get e :file)
                               "action" (if (nil? (get e :requirement)) "remove" "set")
                               "coord" (get e :coord)
                               "requirement" (get e :requirement)})
                records)))
          []
          edits))

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
        _ (when-not (.exists root-file)
            (throw (ex-info (str "workspace manifest not found: " (.getPath root-file)) {})))
        _ (when (and (not shared-only) (not (.exists mod-file)))
            (throw (ex-info (str "module manifest not found: " (.getPath mod-file)) {})))
        edits (-> []
                  ;; module manifest (skipped for shared-only edits)
                  (cond-> (and (not shared-only)
                               (edit-file mod-file (fn [zt] (or (edit-section zt (if (nil? alias) "deps" "extra-deps")
                                                                              alias coord req)
                                                                zt))))
                    (conj {"file" (rel-deps module)
                           "action" action
                           "coord" coord-str
                           "requirement" req}))
                  ;; root manifest, for a shared requirement
                  (cond-> (and shared
                               (edit-file root-file (fn [zt] (or (edit-rig-deps zt coord req) zt))))
                    (conj {"file" "deps.edn"
                           "action" action
                           "coord" coord-str
                           "requirement" req})))]
    (let [edits (if (and shared (not shared-only))
                  ;; propagate the shared requirement into the other modules
                  (let [root-data (or (:data (manifest/read-manifest ws)) {})
                        others (remove #(= (str %) module) (manifest/modules-of root-data))]
                    (reduce (fn [edits m]
                              (let [f (if (= m ".") root-file (io/file ws m "deps.edn"))]
                                (if-not (.exists f)
                                  edits
                                  (let [text (slurp f)
                                        zt (z/of-string text)
                                        dm (s/get zt :deps)
                                        entry (when dm (s/get dm coord))]
                                    (if-not (and entry (s/get entry :mvn/version))
                                      edits
                                      (let [out (format! (z/root-string (edit-deps-section dm coord req)))]
                                        (if (= text out)
                                          edits
                                          (do (spit f out)
                                              (conj edits {"file" (rel-deps (str m))
                                                           "action" action
                                                           "coord" coord-str
                                                           "requirement" req})))))))))
                            edits
                            others))
                  edits)
          res (resolve/resolve-lock
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
      (merge res {"edits" edits
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
            sel (versions/select-latest coord-str (resolve/all-repos ws)
                                        (resolve/cooldown-of args root-data)
                                        (true? (get args :force))
                                        (System/currentTimeMillis))]
        (cond
          (= "not-found" (:reason sel))
          (throw (ex-info (str "coordinate not found in any repository: " coord-str) {}))
          (nil? (:version sel))
          {"refused" [{:coord (symbol coord-str)
                       :reason "cooldown"
                       :cooldown (or (get (resolve/cooldown-of args root-data) :default) versions/default-cooldown)}]}
          :else
          (apply-edit (assoc-in request [:args :requirement] (:version sel))
                      (:skipped sel)))))))
