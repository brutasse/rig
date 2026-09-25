(ns rig.resolver.migrate
  "Kernel op: migrate a legacy Exoscale workspace (tools.project +
  deps-modules managed-dependencies) to the :rig/* manifest model, in place.

  The transform is mechanical: each manifest is rewritten with the effective
  dependencies that deps-modules' merge-deps would have produced (managed
  wins, :local/root canonicalized module-relative), legacy keys are renamed
  or dropped, and :slipset.deps-deploy/exec-args is translated to
  :rig/publish + :mvn/repos. Comments are preserved: only the nodes that
  actually change are edited. Files are written only when there are no
  problems; dry-run computes the edits without writing.

  Request:  {op \"migrate\" workspace ws args {:dry-run? bool}}
  Response: {edits [{file changed}] warnings [str] problems [str]}"
  (:require [clojure.edn :as edn]
            [clojure.java.io :as io]
            [clojure.string :as str]
            [cljfmt.core :as cljfmt]
            [rewrite-clj.node :as n]
            [rewrite-clj.zip :as z]
            [rewrite-clj.zip.seqz :as s]))

;; --- legacy key surface ---

(def key-rename
  {:exoscale.project/modules :rig/modules
   :exoscale.project/lib :rig/lib
   :exoscale.project/main :rig/main
   :exoscale.project/src-dirs :rig/src-dirs
   :exoscale.project/java-src-dirs :rig/java-src-dirs
   :exoscale.project/target-dir :rig/target-dir
   :exoscale.project/uberjar-file :rig/uberjar-file
   :exoscale.project/uberjar? :rig/uberjar?
   :exoscale.project/uber-opts :rig/uber-opts
   :exoscale.project/ns-compile :rig/ns-compile
   :exoscale.project/extra-clean-targets :rig/clean-dirs
   :exoscale.project/deploy? :rig/publish?
   :exoscale.project/version-file :rig/version-file
   :exoscale.project/version-template-file :rig/version-template-file})

;; Namespaces that are purely legacy: any top-level key in one of them that
;; has no rig equivalent is dropped with a warning.
(def legacy-ns #{"exoscale.project" "exoscale.deps" "slipset.deps-deploy" "antq.core"})

(def version-fn-map
  {"exoscale.tools.project.api.version/git-count-revs" :git-count-revs
   "exoscale.tools.project.api.version/epoch" :epoch})

(defn- version-fn-key
  "Canonical string form of a version-fn value (keyword or string). (str)
  of a keyword keeps the leading colon, which would never match the map."
  [v]
  (if (keyword? v)
    (str (namespace v) (when (namespace v) "/") (name v))
    (str v)))

(defn- format!
  [text]
  (let [f (cljfmt/reformat-string text)]
    (if (str/ends-with? f "\n") f (str f "\n"))))

;; --- data-level transform (validated against deps-modules merge-deps) ---

(defn- canonicalize
  "Make a managed :local/root (workspace-root-relative) module-relative."
  [dst module-dir]
  (let [bp (if (or (str/blank? module-dir) (= module-dir "."))
             []
             (str/split module-dir #"/"))
        tp (str/split dst #"/")
        i (loop [i 0]
            (if (and (< i (count bp)) (< i (count tp)) (= (nth bp i) (nth tp i)))
              (recur (inc i))
              i))
        ups (vec (replicate (- (count bp) i) ".."))
        rest (subvec tp i)]
    (if (empty? (vec (concat ups rest)))
      "."
      (str/join "/" (concat ups rest)))))

(defn- canon-dep
  [dep module-dir]
  (cond-> dep
    (contains? dep :local/root)
    (assoc :local/root (canonicalize (get dep :local/root) module-dir))))

(defn- inherit-dep
  "deps-modules semantics: the declared source keys are stripped and the
  managed entry wins (a vector inherit selects a subset of managed keys).
  A managed entry's own :exoscale.deps/inherit marker is inert residue and
  does not propagate."
  [declared managed inherit]
  (merge (dissoc declared :mvn/version :git/url :git/sha :git/tag
                 :local/root :deps/root :exoscale.deps/inherit)
         (cond-> (dissoc managed :exoscale.deps/inherit)
           (not= :all inherit) (select-keys (vec inherit)))))

(defn- materialize-deps
  "Materialize :exoscale.deps/inherit entries. Returns
  {:deps m :problems [..] :warnings [..]} (deps is nil when problems are
  reported). A dep with inherit that is missing from managed-dependencies
  keeps its declared keys (marker dropped, warning); a dep that carries
  only the marker is a problem — nothing left to resolve against."
  [dm managed module-dir]
  (let [unmanaged (filter (fn [[k v]]
                            (and (map? v)
                                 (contains? v :exoscale.deps/inherit)
                                 (nil? (get managed k))))
                          dm)
        bare (filter (fn [[_ v]] (empty? (dissoc v :exoscale.deps/inherit)))
                     unmanaged)]
    (if (seq bare)
      {:deps nil
       :problems (map (fn [[k _]]
                        (str "dep " (str k) " uses :exoscale.deps/inherit but is missing from :exoscale.deps/managed-dependencies and declares no version"))
                      bare)}
      {:deps (into {} (map (fn [[k v]]
                             (cond
                               (not (and (map? v) (contains? v :exoscale.deps/inherit)))
                               [k v]
                               (get managed k)
                               [k (canon-dep (inherit-dep v (get managed k)
                                                       (get v :exoscale.deps/inherit))
                                             module-dir)]
                               :else
                               [k (dissoc v :exoscale.deps/inherit)]))
                           dm))
        :warnings (map (fn [[k _]]
                         (str "dep " (str k) " :exoscale.deps/inherit dropped (missing from :exoscale.deps/managed-dependencies; declared keys kept)"))
                       (remove (fn [[_ v]] (empty? (dissoc v :exoscale.deps/inherit)))
                               unmanaged))
        :problems nil})))

(defn- materialize-alias
  "Materialize one alias entry's :deps/:extra-deps/:override-deps sub-maps.
  Returns [entry problems warnings]."
  [v managed module-dir]
  (loop [subs [:deps :extra-deps :override-deps]
         entry v
         problems []
         warnings []]
    (if (nil? subs)
      [entry problems warnings]
      (let [[sub & rest-subs] subs
            res (when (contains? entry sub)
                  (materialize-deps (get entry sub) managed module-dir))]
        (recur rest-subs
               (cond-> entry
                 res (assoc sub (get res :deps)))
               (concat problems (when res (get res :problems)))
               (concat warnings (when res (get res :warnings))))))))

(defn- materialize-aliases
  "Drop the :project driver alias; materialize the inherit entries inside
  each remaining alias. A top-level :exoscale.deps/inherit on a non-:project
  alias is inert (the flake build only ever activates :project) and is
  dropped with a warning. Returns {:aliases m :problems [..] :warnings [..]}."
  [am managed module-dir]
  (let [dropped (filter (fn [[k v]]
                          (and (not= k :project)
                               (map? v)
                               (contains? v :exoscale.deps/inherit)))
                        am)
        res (reduce (fn [{:keys [aliases problems warnings]} [k v]]
                      (if (= k :project)
                        {:aliases aliases :problems problems :warnings warnings}
                        (if (map? v)
                          (let [[e p w] (materialize-alias (dissoc v :exoscale.deps/inherit)
                                                           managed module-dir)]
                            {:aliases (assoc aliases k e)
                             :problems (concat problems p)
                             :warnings (concat warnings w)})
                          {:aliases (assoc aliases k v)
                           :problems problems
                           :warnings warnings})))
                    {:aliases {} :problems [] :warnings []}
                    am)]
    (assoc res
           :problems (when (seq (:problems res)) (:problems res))
           :warnings (concat (:warnings res)
                            (map (fn [[k _]]
                                   (str "alias :" (str (name k))
                                        " top-level :exoscale.deps/inherit dropped (only the :project driver alias is supported)"))
                                 dropped)))))

(defn- s3p-target
  "s3p://<bucket>/<prefix> -> [bucket prefix] (no trailing slash on the
  prefix, mirroring the Go-side Parse); nil if not an s3p url."
  [u]
  (when (str/starts-with? u "s3p://")
    (let [parts (str/split (subs u 6) #"/" 2)]
      [(first parts) (str/replace (get parts 1 "") #"/+$" "")])))

(defn- slugify
  [s]
  (str/lower-case (str/replace (str/replace s #"[^a-zA-Z0-9]" "-") #"--+" "-")))

(defn- repo-from-url
  "For a non-s3p url, find a matching existing :mvn/repos id, else derive one.
  Returns [id repos] (repos is a new {id {:url u}} or {})."
  [u all-repos]
  (let [hit (some (fn [[id sp]] (when (= u (get sp :url)) id)) all-repos)]
    (if hit
      [hit {}]
      (let [host (-> u (str/replace #"^\w+://" "") (str/split #"/") first)
            id (slugify host)]
        [id {id {:url u}}]))))

(defn- exec-args-to-publish
  "Map :slipset.deps-deploy/exec-args to the rig id-based publish model.
  Returns {:publish {:repo id} :repos {id {:url u}} :warnings [...]
  :problems [...]} (publish is nil when there is no resolvable repository)."
  [args mod-repos root-repos]
  (let [sign (true? (get args :sign-releases?))
        problems (when sign
                   [":slipset.deps-deploy/exec-args :sign-releases? true is not supported by rig"])
        warns (if (= (get args :installer) :local)
                [":slipset.deps-deploy/exec-args :installer :local -> use `rig install` (no remote publish)"]
                [])
        rep (get args :repository)
        pair (cond
               (string? rep)
               [rep {}]
               (map? rep)
               (let [u (or (get-in rep ["releases" :url]) (get-in rep [:releases :url])
                           (get-in rep ["snapshots" :url]) (get-in rep [:snapshots :url]))]
                 (cond
                   (nil? u)
                   [nil {}]
                   (str/starts-with? u "s3p://")
                   (let [[bucket prefix] (s3p-target u)
                         url (str "s3p://" bucket (when (seq prefix) (str "/" prefix)))]
                     [bucket {bucket {:url url}}])
                   :true
                   (repo-from-url u (merge root-repos mod-repos))))
               :else
               [nil {}])]
    {:publish (when (first pair) {:repo (first pair)})
     :repos (second pair)
     :warnings warns
     :problems problems}))

(defn- transform-data
  "Compute the migrated manifest data. Returns
  {:data m :warnings [..] :problems [..]}."
  [data module-dir root-data]
  (let [managed-deps (get root-data :exoscale.deps/managed-dependencies)
        warnings (atom [])
        problems (atom [])
        d (reduce (fn [acc [k v]]
                    (cond
                      (contains? acc k)
                      acc
                      (= k :exoscale.project/bypass-test?)
                      (assoc acc :rig/test? (not (true? v)))
                      (= k :exoscale.project/version-fn)
                      (if-let [kw (get version-fn-map (version-fn-key v))]
                        (assoc acc :rig/version-fn kw)
                        (do (swap! problems conj
                                   (str ":exoscale.project/version-fn " (pr-str v)
                                        " is not a known tools.project version tool"))
                            acc))
                      (= k :exoscale.deps/managed-dependencies)
                      (if (= module-dir ".")
                        ;; The entries' own :exoscale.deps/inherit markers
                        ;; are inert residue: a migrated manifest must carry
                        ;; no legacy-ns key anywhere.
                        (assoc acc :rig/deps
                               (into {} (map (fn [[lib spec]]
                                               [lib (dissoc spec :exoscale.deps/inherit)])
                                             v)))
                        (do (swap! problems conj
                                   ":exoscale.deps/managed-dependencies is only supported in the root deps.edn")
                            acc))
                      (= k :exoscale.deps/managed-aliases)
                      (let [extra (filter (fn [[ak _]] (not= ak :project)) v)]
                        (swap! warnings conj
                               (if (seq extra)
                                 (str ":exoscale.deps/managed-aliases entries "
                                      (str/join ", " (map str (map first extra)))
                                      " dropped (no rig equivalent; only the :project driver alias is supported)")
                                 ":exoscale.deps/managed-aliases dropped (only the :project driver alias)"))
                        acc)
                      (= k :slipset.deps-deploy/exec-args)
                      acc
                      (contains? key-rename k)
                      (assoc acc (get key-rename k) v)
                      (and (keyword? k) (contains? legacy-ns (namespace k)))
                      (do (swap! warnings conj (str "dropped " (str k) " (no rig equivalent)"))
                          acc)
                      :true
                      (assoc acc k v)))
                  {} data)]
    (let [pub (exec-args-to-publish (get data :slipset.deps-deploy/exec-args)
                                    (get d :mvn/repos)
                                    (get root-data :mvn/repos))]
      (swap! warnings into (or (get pub :warnings) []))
      (swap! problems into (or (get pub :problems) []))
      (let [deps-res (when (contains? d :deps)
                       (materialize-deps (get d :deps) managed-deps module-dir))
            aliases-res (when (contains? d :aliases)
                          (materialize-aliases (get d :aliases) managed-deps module-dir))]
        (swap! problems into (when deps-res (or (get deps-res :problems) [])))
        (swap! problems into (when aliases-res (or (get aliases-res :problems) [])))
        (swap! warnings into (when deps-res (get deps-res :warnings)))
        (swap! warnings into (when aliases-res (get aliases-res :warnings)))
        {:data (-> d
                   (cond-> (and (some? deps-res) (nil? (get deps-res :problems)))
                           (assoc :deps (get deps-res :deps)))
                   (cond-> (and (some? aliases-res) (nil? (get aliases-res :problems)))
                           (assoc :aliases (get aliases-res :aliases)))
                   (cond-> (get pub :publish) (assoc :rig/publish (get pub :publish)))
                   (cond-> (seq (get pub :repos))
                           (assoc :mvn/repos (merge (get d :mvn/repos) (get pub :repos)))))
         :warnings @warnings
         :problems @problems}))))

;; --- zipper-level application (comment-preserving) ---

(defn- up-to-root
  [zloc]
  (loop [u zloc]
    (if-let [p (nth u 1)]
      (if (get p :ppath) (recur (z/up u)) u)
      u)))

(defn- map-zloc
  [ztop path]
  (reduce (fn [mz k] (when mz (s/get mz k))) ztop path))

(defn- key-node
  [mz k]
  (when mz
    (loop [cz (z/down mz)]
      (when cz
        (if (= (n/sexpr (z/node cz)) k) cz (recur (z/right cz)))))))

(defn- data->node
  [v]
  (cond
    (nil? v) (n/token-node "nil")
    (keyword? v) (n/keyword-node v)
    (string? v) (n/string-node v)
    (symbol? v) (n/token-node v)
    (boolean? v) (n/token-node v)
    (number? v) (n/token-node v)
    (map? v) (n/map-node (interpose (n/whitespace-node " ")
                                    (mapcat (fn [[k vv]]
                                              [(data->node k)
                                               (n/whitespace-node " ")
                                               (data->node vv)])
                                            v)))
    (coll? v) (n/vector-node (interpose (n/whitespace-node " ")
                                        (map data->node v)))
    :else (throw (ex-info (str "migrate: cannot serialize value of type " (class v)) {}))))

(defn- rename-key
  "Rename key k1 to k2 in the map at path (value + comments preserved)."
  [ztop path k1 k2]
  (if-let [kz (some-> (map-zloc ztop path) (key-node k1))]
    (up-to-root (z/edit kz (fn [_] (n/keyword-node k2))))
    ztop))

(defn- edit-value
  "Replace the value node of key k in the map at path."
  [ztop path k node]
  (if-let [vz (some-> (map-zloc ztop path) (s/get k))]
    (up-to-root (z/edit vz (fn [_] node)))
    ztop))

(defn- drop-key
  "Remove the key + value pair of k from the map at path."
  [ztop path k]
  (if-let [vz (some-> (map-zloc ztop path) (s/get k))]
    (up-to-root (-> vz z/remove z/remove))
    ztop))

(defn- assoc-entry
  "Add entry k to the map at path."
  [ztop path k v]
  (if-let [mz (map-zloc ztop path)]
    (up-to-root (s/assoc mz k v))
    ztop))

(defn- sync-entries
  "Reconcile the entry map at path with target: update changed values in
  place (unchanged entries, and their comments, are left as-is), add
  missing entries, remove entries absent from target (located via
  original-keys)."
  [ztop path target original-keys]
  (let [zt (reduce (fn [zt [k v]]
                     (let [vz (some-> (map-zloc zt path) (s/get k))]
                       (cond
                         (nil? vz)
                         (assoc-entry zt path k v)
                         (= (n/sexpr (z/node vz)) v)
                         zt
                         :true
                         (edit-value zt path k (data->node v)))))
                   ztop
                   (seq target))
        extras (remove (set (keys target)) original-keys)]
    (reduce (fn [zt k] (drop-key zt path k)) zt extras)))

(defn- apply-migration
  "Apply the data -> target migration to the zipper."
  [ztop data target]
  (let [new-names (set (concat (vals key-rename) [:rig/test? :rig/version-fn :rig/deps]))
        renamed-keys (set (concat (keys key-rename)
                                  [:exoscale.project/bypass-test?
                                   :exoscale.project/version-fn
                                   :exoscale.deps/managed-dependencies]))
        zt (reduce (fn [zt [old new]]
                     (if (contains? data old) (rename-key zt [] old new) zt))
                   ztop (seq key-rename))
        ;; bypass-test? -> test? (inverted)
        zt (if (contains? data :exoscale.project/bypass-test?)
             (-> zt
                 (rename-key [] :exoscale.project/bypass-test? :rig/test?)
                 (edit-value [] :rig/test?
                             (n/token-node (not (true? (get data :exoscale.project/bypass-test?))))))
             zt)
        ;; version-fn -> keyword
        zt (if-let [kw (and (contains? data :exoscale.project/version-fn)
                            (get version-fn-map (version-fn-key (get data :exoscale.project/version-fn))))]
             (-> zt
                 (rename-key [] :exoscale.project/version-fn :rig/version-fn)
                 (edit-value [] :rig/version-fn (n/keyword-node kw)))
             zt)
        ;; root: managed-dependencies -> :rig/deps. The entries' own inert
        ;; :exoscale.deps/inherit markers are dropped (a migrated manifest
        ;; must carry no legacy-ns key anywhere).
        zt (if (contains? data :exoscale.deps/managed-dependencies)
             (reduce (fn [zt [lib spec]]
                       (if (contains? spec :exoscale.deps/inherit)
                         (drop-key zt [:rig/deps lib] :exoscale.deps/inherit)
                         zt))
                     (rename-key zt [] :exoscale.deps/managed-dependencies :rig/deps)
                     (get data :exoscale.deps/managed-dependencies))
             zt)
        ;; drop the legacy top-level keys that are not renamed in place
        ;; (exec-args and managed-aliases are dropped; their content is
        ;; already carried by :rig/publish and :rig/deps)
        zt (reduce (fn [zt k] (drop-key zt [] k))
                   zt
                   (filter (fn [k]
                             (and (keyword? k)
                                  (contains? legacy-ns (namespace k))
                                  (not (contains? key-rename k))
                                  (not (contains? renamed-keys k))))
                           (keys data)))
        ;; materialized deps
        zt (if (and (contains? data :deps) (contains? target :deps))
             (sync-entries zt [:deps] (get target :deps)
                           (keys (get data :deps)))
             zt)
        ;; aliases: drop :project, materialize the sub-maps, drop inert
        ;; top-level keys (e.g. :exoscale.deps/inherit). When :project was
        ;; the only alias the whole :aliases key is dropped.
        zt (if (contains? data :aliases)
             (let [others (remove #(= :project %) (keys (get data :aliases)))]
               (if (empty? others)
                 (drop-key zt [] :aliases)
                 (let [zt (drop-key zt [:aliases] :project)]
                   (reduce (fn [zt ak]
                             (let [orig (get (get data :aliases) ak)
                                   tgt (get (get target :aliases) ak)]
                               (if (and (map? orig) (map? tgt))
                                 (let [zt2 (reduce (fn [zt k] (drop-key zt [:aliases ak] k))
                                                   zt
                                                   (remove (set (keys tgt))
                                                           (keys orig)))]
                                   (reduce (fn [zt sub]
                                             (if (and (contains? tgt sub)
                                                      (contains? orig sub))
                                               (sync-entries zt [:aliases ak sub]
                                                             (get tgt sub)
                                                             (keys (get orig sub)))
                                               zt))
                                           zt2
                                           [:deps :extra-deps :override-deps]))
                                 zt)))
                           zt
                           (keys (get data :aliases))))))
             zt)
        ;; mvn/repos: ensure the publish repo entries
        zt (cond
             (and (contains? data :mvn/repos) (contains? target :mvn/repos))
             (sync-entries zt [:mvn/repos] (get target :mvn/repos)
                           (keys (get data :mvn/repos)))
             (contains? target :mvn/repos)
             (assoc-entry zt [] :mvn/repos (get target :mvn/repos))
             :true
             zt)
        ;; add the remaining new top-level keys (e.g. :rig/publish)
        zt (reduce (fn [zt [k v]]
                     (if (or (contains? (set (keys data)) k) (contains? new-names k))
                       zt
                       (assoc-entry zt [] k v)))
                   zt
                   (seq target))]
    zt))

;; --- op entry point ---

(defn migrate
  "Kernel op: migrate the legacy manifests of a workspace in place."
  [request]
  (let [ws (str (:workspace request))
        dry-run? (true? (get-in request [:args :dry-run?]))
        root-file (io/file ws "deps.edn")
        _ (when-not (.exists root-file)
            (throw (ex-info (str "workspace manifest not found: " (.getPath root-file)) {})))
        root-data (edn/read-string (slurp root-file))
        pairs (vec (remove nil?
                           (for [m (concat ["."]
                                            (some->> (or (get root-data :rig/modules)
                                                          (get root-data :exoscale.project/modules))
                                                      (map str)))]
                             (let [f (if (= m ".")
                                       root-file
                                       (io/file ws m "deps.edn"))]
                               (when (.exists f) [m f])))))
        label (fn [m] (if (= m ".") "deps.edn" (str m "/deps.edn")))
        results (mapv (fn [[m f]]
                        (let [data (edn/read-string (slurp f))
                              t (transform-data data m root-data)
                              l (label m)]
                          {:module m
                           :file f
                           :label l
                           :data data
                           :target (get t :data)
                           :warnings (map (fn [w] (str l ": " w)) (get t :warnings))
                           :problems (map (fn [w] (str l ": " w)) (get t :problems))}))
                      pairs)
        problems (mapcat :problems results)
        warnings (mapcat :warnings results)
        edits (if (seq problems)
                []
                (for [{:keys [file label data target]} results]
                  (let [text (slurp file)
                        raw (z/root-string (apply-migration (z/of-string text) data target))
                        changed? (not= text raw)]
                    (when (and (not dry-run?) changed?)
                      (spit file (format! raw)))
                    {"file" label "changed" changed?})))]
    {"edits" (vec edits)
     "warnings" (vec warnings)
     "problems" (vec problems)}))
