(ns rig.resolver.tree
  "Dependency tree for display (design §7.3): the resolved dependency graph
   of one module, rendered as an indented tree. Every occurrence of a lib is
   shown; one not selected into the classpath (conflict, exclusion,
   duplicate) carries its trace reason. Floating requirements are pinned to
   the lock's versions first, so with a warm ~/.m2 repository no network is
   needed."
  (:require [clojure.java.io :as io]
            [clojure.string :as str]
            [clojure.tools.deps.extensions :as ext]
            [clojure.tools.deps.tree :as td-tree]
            [rig.resolver.manifest :as manifest]
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

(defn- render-lines
  "The children of a traced node as tree lines: one line per occurrence, in
   trace order, `prefix` continued with `├── ` or `└── `; an
   occurrence not included in the classpath is annotated with its trace
   reason (e.g. `jackson-core:2.12.4 (same-version)`) and not expanded."
  [node prefix ws]
  (let [kids (sort-by :step (vals (:children node)))]
    (loop [i 0 lines []]
      (if (= i (count kids))
        lines
        (let [kid (nth kids i)
              last? (= i (dec (count kids)))
              line (str prefix
                        (if last? "└── " "├── ")
                        (node-id (:lib kid) (:coord kid) ws)
                        (when-not (true? (:include kid))
                          (str " (" (name (or (:reason kid) "omitted")) ")")))]
          (recur (inc i)
                 (into (conj lines line)
                       (render-lines kid
                                     (str prefix (if last? "    " "│   "))
                                     ws))))))))

(defn tree
  [request]
  (let [ws (str (:workspace request))
        args (or (:args request) {})
        module (or (get args :module) ".")
        alias (get args :alias)
        mdir (if (= module ".") ws (str (io/file ws module)))
        man (manifest/read-manifest mdir)
        data (or (:data man) {})
        pins (into {}
                   (for [a (get (resolve/lock-of request) :artifacts)
                         :when (= "mvn" (get a :kind))]
                     [(str (get a :group) "/" (get a :name)) (get a :version)]))
        sel (versions/select-versions data
                                      (resolve/all-repos ws)
                                      (resolve/cooldown-of args data)
                                      (true? (get args :force))
                                      pins
                                      {}
                                      true
                                      (System/currentTimeMillis))
        refused (:refused sel)
        proj (let [base (or (when (:changed? sel) (versions/apply-versions data (:selected sel) {}))
                            data)]
               (when (or (:changed? sel)
                         (seq (versions/proxy-map)))
                 (versions/with-proxy-repos base)))
        root (let [lib (manifest/lib data)]
               (if lib
                 (str lib (when-let [v (manifest/read-version data mdir ws)]
                            (str ":" v)))
                 module))
        t (td-tree/trace->tree
           (td-tree/calc-trace {:dir (io/file mdir)
                                :project (or proj "deps.edn")
                                :aliases (when (seq alias) [(keyword alias)])}))]
    (if (seq refused)
      (throw (ex-info (str "tree: cannot resolve floating version(s): "
                           (apply str (interpose ", " (map str (map :coord refused))))
                           " — run `rig lock` (online) to pin them first") {}))
      {"tree" (apply str (interpose \newline
                                    (into [root] (render-lines t "" ws))))})))
