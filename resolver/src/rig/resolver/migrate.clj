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
            [clojure.pprint :as pprint]
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

(defn- slugify
  [s]
  (str/lower-case (str/replace (str/replace s #"[^a-zA-Z0-9]" "-") #"--+" "-")))

(defn- repo-from-url
  "Find a matching existing :mvn/repos id for url, else derive one.
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
  :problems [...]} (publish is nil when there is no resolvable repository;
  an s3p:// deploy url is a problem — rig publishes to http/https only)."
  [args mod-repos root-repos]
  (let [sign (true? (get args :sign-releases?))
        warns (if (= (get args :installer) :local)
                [":slipset.deps-deploy/exec-args :installer :local -> use `rig install` (no remote publish)"]
                [])
        rep (get args :repository)
        u (when (map? rep)
            (or (get-in rep ["releases" :url]) (get-in rep [:releases :url])
                (get-in rep ["snapshots" :url]) (get-in rep [:snapshots :url])))
        pair (cond
               (string? rep)
               [rep {}]
               (nil? u)
               [nil {}]
               (str/starts-with? u "s3p://")
               [nil {}]
               :true
               (repo-from-url u (merge root-repos mod-repos)))
        problems (concat (when sign
                           [":slipset.deps-deploy/exec-args :sign-releases? true is not supported by rig"])
                         (when (and u (str/starts-with? u "s3p://"))
                           [(str ":slipset.deps-deploy/exec-args :repository " u
                                 " is not supported (rig publishes to http/https repositories only; reconfigure :rig/publish)")]))]
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

;; --- leiningen (project.clj) conversion ---
;;
;; When the workspace has no deps.edn but does have a project.clj, `migrate`
;; generates a new root deps.edn in the :rig/* model; project.clj is left
;; untouched (the user deletes it in the cleanup step). The transform is
;; mechanical: what cannot be expressed (profiles other than :test/:dev,
;; plugins, lein task aliases, native-image builds, ...) is dropped with a
;; warning; what cannot be guessed is a problem that blocks the write.

(def lein-handled-keys
  #{:main :dependencies :repositories :deploy-repositories :profiles
    :source-paths :resource-paths :test-paths :jvm-opts})

(def default-kaocha "1.66.1034")

;; The first kaocha release with kaocha.runner/exec-fn.
(def kaocha-exec-fn-min "1.0.937")

(defn- ver-segments
  "Numeric dot-separated version segments, or nil when not fully numeric."
  [v]
  (let [ss (str/split (str v) #"\.")]
    (when (every? #(re-matches #"\d+" %) ss)
      (map #(Long/parseLong %) ss))))

(defn- ver<
  "True when numeric version a is < b; nil when either is not fully
  numeric."
  [a b]
  (let [sa (ver-segments a)
        sb (ver-segments b)]
    (when (and sa sb)
      (loop [sa sa
             sb sb]
        (let [x (or (first sa) 0)
              y (or (first sb) 0)]
          (if (= x y)
            (if (and (seq sa) (seq sb))
              (recur (rest sa) (rest sb))
              false)
            (< x y)))))))

(defn- kaocha-predates-exec-fn
  "True when the kaocha version is numeric and predates
  kaocha-exec-fn-min (when kaocha.runner/exec-fn was introduced)."
  [v]
  (true? (ver< v kaocha-exec-fn-min)))

(defn- top-level-forms
  "The sexpr of every top-level form in the CLJ text (nil for the ones
  that do not sexpr to data)."
  [text]
  (loop [c (z/of-string text) acc []]
    (if (nil? c)
      acc
      (recur (z/right c)
             (conj acc (try (n/sexpr (z/node c))
                            (catch Exception _ nil)))))))

(defn- defproject-of
  "The [coord version-form pairs] of the first top-level defproject form,
  else nil (absent or malformed)."
  [forms]
  (let [dp (some (fn [f]
                   (when (and (sequential? f) (= (first f) 'defproject)) f))
                 forms)]
    (when (and dp (>= (count dp) 3) (even? (count (nthnext dp 3))))
      [(nth dp 1) (nth dp 2) (into {} (map vec (partition 2 (nthnext dp 3))))])))

(defn- slurp-file-of
  "The string argument of the first (slurp \"...\") form under data."
  [data]
  (cond
    (and (sequential? data)
         (= (first data) 'slurp)
         (string? (second data)))
    (second data)
    (sequential? data)
    (some (fn [el] (when (sequential? el) (slurp-file-of el)))
          data)))

(defn- version-info
  "Resolve the defproject version form to the rig version keys. Returns
  {:version s} | {:version-file s} | {:warnings [..]} (no version keys
  when the form is unrecognizable)."
  [forms vform]
  (cond
    (string? vform)
    {:version vform}
    (symbol? vform)
    (if-let [d (some (fn [f]
                       (when (and (sequential? f)
                                   (= (first f) 'def)
                                   (= (second f) vform))
                         f))
                     forms)]
      (let [body (nth d 2)
            f (slurp-file-of body)]
        (cond
          (string? body) {:version body}
          (some? f) {:version-file f}
          :else {:warnings [(str "cannot interpret version var " (str vform)
                                 " (" (pr-str body) " is not a string literal or a (slurp \"...\") call); no :rig/version emitted")]}))
      {:warnings [(str "version var " (str vform) " is not defined in project.clj; no :rig/version emitted")]})
    :else
    {:warnings [(str "version form " (pr-str vform) " is not a string or a var; no :rig/version emitted")]}))

(defn- to-sym
  "x as a symbol when it is a string that parses as one (else x as is)."
  [x]
  (if (string? x)
    (try (symbol x) (catch Exception _ x))
    x))

(def lein-dep-options #{:exclusions :local/root :git/url :git/sha})

(defn- dep-entries
  "leiningen :dependencies (or a profile's) entries ->
  {:deps {coord spec} :warnings [..] :problems [..]}."
  [entries]
  (reduce (fn [{:keys [deps warnings problems] :as acc} e]
            (if (and (sequential? e)
                     (>= (count e) 2)
                     (or (string? (first e)) (symbol? (first e))))
              (let [seg (str (first e))
                    coord (if (str/includes? seg "/") seg (str seg "/" seg))
                    dep-sym (to-sym coord)
                    tail (nthnext e 1)
                    ver (when (string? (first tail)) (first tail))
                    opt-vec (vec (if (string? (first tail)) (next tail) tail))]
                (if (odd? (count opt-vec))
                  (merge acc {:problems (conj problems
                         (str "dep " coord " has an odd number of option elements"))})
                  (let [opts (into {} (map vec (partition 2 opt-vec)))
                        unknown (filter #(not (contains? lein-dep-options %)) (keys opts))
                        warns (map (fn [k]
                                     (str "dep " coord ": leiningen option " (str k)
                                          " is not supported by rig (dropped)"))
                                   unknown)
                        root (get opts :local/root)
                        git (select-keys opts [:git/url :git/sha])]
                    (cond
                      (some? root)
                      (merge acc {:deps (assoc deps dep-sym {:local/root (str root)})
                            :warnings (concat warnings warns)})
                      (or (get git :git/url) (get git :git/sha))
                      (if (= 2 (count git))
                        (merge acc {:deps (assoc deps dep-sym (into {} (sort-by key git)))
                              :warnings (concat warnings warns)})
                        (merge acc {:problems (conj problems
                               (str "dep " coord " declares :git/url and :git/sha incompletely"))}))
                      (some? ver)
                      (let [spec (cond-> {:mvn/version ver}
                                   (contains? opts :exclusions)
                                   (assoc :exclusions
                                          (vec (map (fn [x]
                                                      (if (symbol? x) x (symbol x)))
                                                    (get opts :exclusions)))))]
                        (merge acc {:deps (assoc deps dep-sym spec)
                              :warnings (concat warnings warns)}))
                      :true
                      (merge acc {:problems (conj problems
                             (str "dep " coord " declares no version, :local/root or :git/url"))})))))
              (merge acc {:problems (conj problems (str "dep " (pr-str e) " is not a [group artifact ...] entry"))})))
          {:deps {} :warnings [] :problems []}
          (when (sequential? entries) entries)))

(defn- repos-of
  "leiningen :repositories (map {id url-or-spec} or vector [id url]
  pairs) -> {:repos {id {:url u}} :warnings [..]}."
  [repos]
  (let [norm (fn [v] (str (if (map? v) (or (get v :url) (get v "url")) v)))]
    (cond
      (nil? repos)
      {:repos {} :warnings []}
      (map? repos)
      {:repos (into {} (for [[id v] repos] [id {:url (norm v)}])) :warnings []}
      (sequential? repos)
      (reduce (fn [{:keys [repos warnings] :as acc} e]
                (if (and (sequential? e) (>= (count e) 2))
                  (if (= :proxy (first e))
                    (merge acc {:warnings (conj warnings
                           (str "proxy repository " (str (second e)) " is not supported (dropped)"))})
                    (merge acc {:repos (assoc repos (str (first e)) {:url (norm (second e))})}))
                  (merge acc {:warnings (conj warnings
                         (str "repository entry " (pr-str e) " is not understood (dropped)"))})))
              {:repos {} :warnings []}
              repos)
      :else
      {:repos {} :warnings [(str ":repositories value " (class repos) " is not understood")]})))

(defn- first-deploy-spec
  "The spec map of the first :deploy-repositories entry ({id spec} map or
  [id spec] vector entries); nil when absent or not a spec map."
  [repos]
  (let [e (first (if (map? repos) (vals repos) repos))]
    (cond
      (map? e) e
      (and (sequential? e) (map? (second e))) (second e)
      :else nil)))

(defn- lein-publish
  "leiningen :deploy-repositories -> an exec-args-to-publish result (rig
  publishes to one repository, so only the first entry is used, with a
  warning when there are more); nil when there are none."
  [repos mod-repos]
  (when (some? repos)
    (let [n (count repos)
          spec (first-deploy-spec repos)
          sign (true? (when (map? spec) (get spec :sign-releases)))
          url (when (map? spec) (get spec :url))]
      (cond
        (nil? spec)
        {:publish nil :repos {} :warnings []
         :problems [(str "first :deploy-repositories entry "
                         (pr-str (first (if (map? repos) (vals repos) repos)))
                     " is not a spec map")]}
        (nil? url)
        {:publish nil :repos {}
         :warnings [":deploy-repositories entry has no :url; no :rig/publish emitted"]
         :problems []}
        :else
        (let [res (exec-args-to-publish
                   {:repository {"releases" {:url (str url)}}}
                   mod-repos {})
              problems (concat (when sign
                                 [":deploy-repositories :sign-releases true is not supported by rig"])
                               (when (str/starts-with? (str url) "s3p://")
                                 [(str ":deploy-repositories " (str url)
                                       " is not supported (rig publishes to http/https repositories only; reconfigure :rig/publish)")]))]
          (cond-> (assoc res :problems (vec problems))
            (and (> n 1) (seq (get res :publish)))
            (update :warnings conj
                    "only the first :deploy-repositories entry is migrated (rig publishes to one repository)")))))))

(defn- effective-paths
  "The effective leiningen path list (with :append/:prepend/:replace
  forms) against a default."
  [default ps]
  (let [[rep prep app] (reduce (fn [[rep prep app] x]
                                 (if (and (sequential? x) (keyword? (first x)))
                                   (case (first x)
                                     :replace [(next x) prep app]
                                     :prepend [rep (concat (next x) prep) app]
                                     :append [rep prep (concat app (next x))]
                                     [rep prep app])
                                   [rep prep app]))
                               [nil [] []] (or ps []))]
    (vec (if (sequential? rep)
           (concat prep rep app)
           (concat prep default app)))))

(defn- added-paths
  "The paths a leiningen path list adds, without its default (for an
  alias's :extra-paths)."
  [ps]
  (vec (map str (or ps []))))

(defn- kaocha-version
  "The version of lambdaisland/kaocha declared in a deps map, else nil."
  [deps]
  (some (fn [[k sp]]
          (when (and (map? sp) (= (str k) "lambdaisland/kaocha"))
            (get sp :mvn/version)))
        deps))

(defn- merged-test-profile
  "Lein applies :dev to the test task by default, so the effective
  :test profile is :test layered over :dev: dependency and jvm-opts
  lists concatenate (dev first), path lists concatenate without
  duplicates, other keys prefer :test."
  [dev test]
  (let [d (or dev {})
        t (or test {})
        m (merge d t)]
    (cond-> m
      (some? (get d :dependencies))
      (update :dependencies (fn [_] (vec (concat (get d :dependencies)
                                                (or (get t :dependencies) [])))))
      (some? (get d :jvm-opts))
      (update :jvm-opts (fn [_] (vec (concat (get d :jvm-opts)
                                            (or (get t :jvm-opts) [])))))
      (some? (get d :resource-paths))
      (update :resource-paths (fn [_] (vec (distinct (concat (get d :resource-paths)
                                                             (or (get t :resource-paths) []))))))
      (some? (get d :source-paths))
      (update :source-paths (fn [_] (vec (distinct (concat (get d :source-paths)
                                                           (or (get t :source-paths) []))))))
      (some? (get d :test-paths))
      (update :test-paths (fn [_] (vec (distinct (concat (get d :test-paths)
                                                         (or (get t :test-paths) [])))))))))

(defn- profile-alias
  "One leiningen profile -> [alias warnings problems]. The :test profile
  always yields an alias with a kaocha :exec-fn (rig test hard-requires
  one); other profiles yield an alias only when they carry expressible
  content."
  [name p test-defaults kaocha-ver]
  (let [test? (= name :test)
        map? (or (nil? p) (map? p))
        p (or p {})
        dres (dep-entries (get p :dependencies))
        d (get dres :deps)
        dw (get dres :warnings)
        dp (get dres :problems)
        extra-deps (if test?
                     (merge d (when kaocha-ver
                                {(to-sym "lambdaisland/kaocha") {:mvn/version kaocha-ver}}))
                     (when (seq d) d))
        extra-paths (vec (concat (added-paths (get p :source-paths))
                                 (added-paths (get p :resource-paths))
                                 (when test?
                                   (effective-paths test-defaults (get p :test-paths)))))
        jvm-opts (when (seq (get p :jvm-opts)) (vec (get p :jvm-opts)))
        alias (cond-> {}
                extra-deps (assoc :extra-deps extra-deps)
                (seq extra-paths) (assoc :extra-paths extra-paths)
                jvm-opts (assoc :jvm-opts jvm-opts)
                test? (assoc :exec-fn (symbol "kaocha.runner/exec-fn")))
        dropped (filter (fn [k]
                          (not (contains? (if test?
                                            #{:dependencies :source-paths :resource-paths :test-paths :jvm-opts}
                                            #{:dependencies :source-paths :resource-paths :jvm-opts})
                                          k)))
                        (keys p))]
    [alias
     (concat (when-not map?
               [(str "profile :" (str (clojure.core/name name)) " is not a map (no rig equivalent)")])
             (map (fn [k]
                    (str "profile :" (str (clojure.core/name name)) " dropped " (str k) " (no rig equivalent)"))
                  dropped)
             dw)
     dp]))
(defn- lein-migrate
  "Convert a Leiningen project.clj to a new root deps.edn in the :rig/*
  model. Returns {\"edits\" [{file changed}] \"warnings\" [str]
  \"problems\" [str]}."
  [ws lein-file dry-run?]
  (let [text (slurp lein-file)
        forms (try (top-level-forms text)
                   (catch Exception e
                     (throw (ex-info (str "project.clj: parse error: " (.getMessage e)) {}))))
        dp (defproject-of forms)]
    (if (nil? dp)
      {"edits" [] "warnings" [] "problems" ["project.clj: no defproject form found"]}
      (let [prefix (fn [m] (str "project.clj: " m))
            [coord vform pairs] dp
            lib (to-sym coord)
            main (to-sym (get pairs :main))
            vinfo (version-info forms vform)
            deps-res (dep-entries (get pairs :dependencies))
            repos-res (repos-of (get pairs :repositories))
            pub (lein-publish (get pairs :deploy-repositories) (get repos-res :repos))
            source-paths (effective-paths ["src"] (get pairs :source-paths))
            resource-paths (effective-paths ["resources"] (get pairs :resource-paths))
            main-paths (vec (concat source-paths resource-paths))
            test-defaults (effective-paths ["test"] (get pairs :test-paths))
            profiles (or (get pairs :profiles) {})
            merged-test (merged-test-profile (get profiles :dev) (get profiles :test))
            ;; Effective kaocha pin for the test classpath: the merged
            ;; :test profile's (later declarations win), else the base.
            declared-kaocha (or (kaocha-version (get (dep-entries (get merged-test :dependencies)) :deps))
                                (kaocha-version (get deps-res :deps)))
            kaocha-bump (and declared-kaocha (kaocha-predates-exec-fn declared-kaocha))
            kaocha-ver (if kaocha-bump
                         default-kaocha
                         (or declared-kaocha default-kaocha))
            test (profile-alias :test merged-test test-defaults kaocha-ver)
            [test-alias test-w test-p] test
            dev (if (some? (get profiles :dev))
                  (profile-alias :dev (get profiles :dev) test-defaults kaocha-ver)
                  [{} [] []])
            [dev-alias dev-w dev-p] dev
            aliases (cond-> {:test test-alias}
                      (seq dev-alias) (assoc :dev dev-alias))
            target (into {}
                         (remove (fn [[_ v]] (nil? v))
                                 [[:rig/lib lib]
                                  [:rig/main main]
                                  [:rig/version (get vinfo :version)]
                                  [:rig/version-file
                                   (when (and (some? (get vinfo :version-file))
                                              (not= (get vinfo :version-file) "VERSION"))
                                     (get vinfo :version-file))]
                                  [:rig/uberjar? (when (some? (get profiles :uberjar)) true)]
                                  [:rig/publish (get pub :publish)]
                                  [:mvn/repos
                                   (let [m (merge (get repos-res :repos) (get pub :repos))]
                                     (when (seq m) m))]
                                  [:paths (when (not= main-paths ["src"]) main-paths)]
                                  [:rig/src-dirs
                                   (when (not= main-paths ["src" "resources"]) main-paths)]
                                  [:deps (when (seq (get deps-res :deps)) (get deps-res :deps))]
                                  [:aliases aliases]
                                  [:jvm-opts
                                   (when (seq (get pairs :jvm-opts)) (vec (get pairs :jvm-opts)))]]))
            problems (concat (get deps-res :problems)
                             (get pub :problems)
                             test-p
                             dev-p)
            warnings (concat (get vinfo :warnings)
                             (get deps-res :warnings)
                             (get repos-res :warnings)
                             (get pub :warnings)
                             test-w
                             dev-w
                             (when kaocha-bump
                               [(str "the project's kaocha " declared-kaocha
                                     " predates kaocha.runner/exec-fn (introduced in "
                                     kaocha-exec-fn-min "), so the :test alias uses the rig default pin "
                                     default-kaocha)])
                             (map (fn [k]
                                    (str "dropped " (str k) " (no rig equivalent)"))
                                  (filter (fn [k] (not (contains? lein-handled-keys k)))
                                          (keys pairs)))
                             (map (fn [k]
                                    (str "profile :" (str (name k))
                                         " dropped (only :test and :dev migrate to :aliases)"))
                                  (filter (fn [k] (not (contains? #{:test :dev :uberjar} k)))
                                          (keys profiles)))
                             (when (some? (get profiles :uberjar))
                               ["profile :uberjar dropped (:rig/uberjar? true emitted for `rig build --uber`)"]))]
        (if (seq problems)
          {"edits" [] "warnings" (vec (map prefix warnings)) "problems" (vec (map prefix problems))}
          (let [out (format! (with-out-str
            (binding [*print-namespace-maps* false]
              (pprint/pprint target))))]
            (when-not dry-run? (spit (io/file ws "deps.edn") out))
            {"edits" [{"file" "deps.edn" "changed" true}]
             "warnings" (vec (map prefix warnings))
             "problems" []}))))))

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

(defn- migrate-deps-edn
  "The legacy deps.edn -> :rig/* transform, in place."
  [ws root-file dry-run?]
  (let [root-data (edn/read-string (slurp root-file))
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

(defn migrate
  "Kernel op: migrate the legacy manifests of a workspace in place, or
  convert a Leiningen project.clj to a new root deps.edn."
  [request]
  (let [ws (str (:workspace request))
        dry-run? (true? (get-in request [:args :dry-run?]))
        root-file (io/file ws "deps.edn")
        lein-file (io/file ws "project.clj")]
    (cond
      (.exists root-file)
      (migrate-deps-edn ws root-file dry-run?)
      (.exists lein-file)
      (lein-migrate ws lein-file dry-run?)
      :true
      (throw (ex-info (str "workspace manifest not found: " (.getPath root-file)
                           " (no deps.edn or project.clj)") {})))))

