(ns rig.resolver.tree
  "Dependency tree for display (design §7.3): the module's classpath,
   rendered as an indented tree — the same graph the lock pins per module
   (and per module+alias with --alias): one line per chain, every artifact
   a selected version, no loser version of a conflict. The basis is built
   exactly as the lock builds it (rig.resolver.resolve): no root/user
   manifest, the module's repos, the :rig/deps pool pins. Floating
   requirements are pinned to the lock's versions first, so with a warm
   ~/.m2 repository no network is needed."
  (:require [clojure.java.io :as io]
            [clojure.string :as str]
            [clojure.tools.deps.extensions :as ext]
            [clojure.tools.deps.tree :as td-tree]
            [rig.resolver.manifest :as manifest]
            [rig.resolver.pom :as pom]
            [rig.resolver.resolve :as resolve]
            [rig.resolver.versions :as versions]))

(defn- node-id
  "Display id of a traced lib: group/name:version (mvn),
   group/name:<tag|sha7> (git), local:<root> (local modules, relative to the
   workspace when they live in it)."
  [lib coord ws]
  (let [t (ext/coord-type coord)]
    (case t
      :local (let [root (get-in coord [:local/root])]
               (str "local:"
                    (if (and ws root (str/starts-with? root (str ws "/")))
                      (subs root (inc (count ws)))
                      root)))
      (str lib
           (when-let [v (case t
                          :mvn (get-in coord [:mvn/version])
                          :git (or (get-in coord [:git/tag])
                                   (some-> (get-in coord [:git/sha]) (subs 0 7))))]
             (str ":" v))))))

(defn- on-classpath?
  "True when the traced occurrence sits on the module's classpath: its
  coordinate is the version selected for the lib in the final trace (a
  loser version of a conflict is not, neither is a lib orphaned by a later
  win), and it is not cut from this path by an exclusion. render-lines
  draws only such occurrences and recurses only into drawn ones, so every
  drawn chain is one of the classpath's."
  [vmap lib coord-id reason]
  (and (not= :excluded reason)
       (= coord-id (get-in vmap [lib :select]))))

(defn- render-lines
  "The children of a traced node as tree lines: one line per occurrence on
  the classpath, in trace order, `prefix` continued with `├── ` or
  `└── `; a drawn occurrence is expanded like any other."
  [node prefix ws vmap]
  (let [kids (into [] (filter (fn [kid] (on-classpath? vmap (:lib kid) (:coord-id kid) (:reason kid)))
                              (sort-by :step (vals (:children node)))))]
    (mapcat (fn [[i kid]]
              (let [last? (= i (dec (count kids)))
                    line (str prefix
                              (if last? "└── " "├── ")
                              (node-id (:lib kid) (:coord kid) ws))]
                (cons line
                      (render-lines kid
                                    (str prefix (if last? "    " "│   "))
                                    ws
                                    vmap))))
            (map-indexed vector kids))))

(defn- trace-for
  "The module's dependency trace, with the basis built exactly as the
  lock builds it (see the ns doc): no root/user manifest, the module's
  repos, the :rig/deps pool pins, floating requirements pinned to the
  lock's versions. Returns the calc-trace result plus the manifest and
  the refused floating versions (an empty seq when the lock covers
  everything)."
  [request]
  (let [ws (str (:workspace request))
        args (or (:args request) {})
        module (or (get args :module) ".")
        alias (get args :alias)
        mdir (if (= module ".") ws (str (io/file ws module)))
        man (manifest/read-manifest mdir)
        data (or (:data man) {})
        root-data (or (:data (manifest/read-manifest ws)) {})
        pins (pom/pins-of (resolve/lock-of request))
        sel (versions/select-versions data
                                      (resolve/repos-of data)
                                      (resolve/cooldown-of args root-data)
                                      (true? (get args :force))
                                      pins
                                      {}
                                      (System/currentTimeMillis))
        refused (:refused sel)
        pool-ov (resolve/pool-overrides root-data data)
        ;; The same project map the lock resolves from (basis-project in
        ;; rig.resolver.resolve): one construction, so the tree and the
        ;; lock's per-module classpaths are built from identical bases.
        proj (-> (if (:changed? sel)
                   (versions/apply-versions data (:selected sel) {})
                   data)
                 (versions/with-proxy-repos)
                 (resolve/with-local-exclusions (resolve/root-injected-deps))
                 (resolve/basis-project mdir))
        trace (do
                ;; A git dep is a commit pin; refuse the wrong forms before
                ;; tools.deps' trace sees the manifest (its errors have no
                ;; :path and would surface as stack traces).
                (when-let [gs (seq (manifest/git-ref-problems data))]
                  (throw (ex-info (str "invalid git dep: "
                                       (str/join "; " (map manifest/git-ref-problem-line gs)))
                                  {:rig/friendly? true})))
                (td-tree/calc-trace {:dir (io/file mdir)
                                     :project proj
                                     :root nil
                                     :user nil
                                     :aliases (when (seq alias) [(keyword alias)])
                                     :args (when (seq pool-ov) {:override-deps pool-ov})}))]
    {:ws ws :mdir mdir :module module :data data :trace trace :refused refused}))

(defn- refuse-floating
  "An op cannot report on a classpath the lock does not pin: name the
  floating versions and point at `rig lock`."
  [refused]
  (when (seq refused)
    (throw (ex-info (str "cannot resolve floating version(s): "
                         (str/join ", " (map (comp str :coord) refused))
                         " — run `rig lock` (online) to pin them first")
                    {:rig/friendly? true}))))

(defn tree
  [request]
  (let [{:keys [ws mdir module data trace refused]} (trace-for request)]
    (refuse-floating refused)
    (let [t (td-tree/trace->tree trace)
          root (let [lib (manifest/lib data)]
                 (if lib
                   (str lib (when-let [v (manifest/read-version data mdir ws)]
                              (str ":" v)))
                   module))]
      {"tree" (str/join \newline (into [root] (render-lines t "" ws (:vmap trace))))})))

(defn- walk-chains
  "Extend `path` (the display ids from a direct dep down to `node`)
  through `node`'s children, in trace order, keeping only the
  classpath's occurrences (the render-lines filter): a child whose id
  is one of `goal` records its chain in `acc`, and is expanded like any
  other — a shared dep carries one chain per chain that pulls it."
  [node path ws vmap goal acc]
  (let [kids (into [] (filter (fn [kid] (on-classpath? vmap (:lib kid) (:coord-id kid) (:reason kid)))
                              (sort-by :step (vals (:children node)))))]
    (reduce (fn [acc kid]
              (let [id (node-id (:lib kid) (:coord kid) ws)
                    path (conj path id)
                    acc (if (contains? goal id) (update acc id (fnil conj []) path) acc)]
                (walk-chains kid path ws vmap goal acc)))
            acc
            kids)))

(defn explain
  "The puller chains of the requested artifacts for one module (and
  alias): target display id (group/name:version) -> the ordered chains
  from the module's direct deps down to it, per walk-chains. The same
  basis as tree — so the chains are one of the lock's per-module
  (module+alias) classpaths."
  [request]
  (let [{:keys [ws trace refused]} (trace-for request)
        targets (get-in request [:args :targets])]
    (refuse-floating refused)
    {"chains" (walk-chains (td-tree/trace->tree trace) [] ws (:vmap trace)
                           (set (or targets []))
                           {})}))
