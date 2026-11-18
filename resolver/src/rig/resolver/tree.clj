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

(defn tree
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
        root (let [lib (manifest/lib data)]
               (if lib
                 (str lib (when-let [v (manifest/read-version data mdir ws)]
                            (str ":" v)))
                 module))
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
    (if (seq refused)
      (throw (ex-info (str "tree: cannot resolve floating version(s): "
                           (str/join ", " (map (comp str :coord) refused))
                           " — run `rig lock` (online) to pin them first") {}))
      (let [t (td-tree/trace->tree trace)]
        {"tree" (str/join \newline (into [root] (render-lines t "" ws (:vmap trace))))}))))
