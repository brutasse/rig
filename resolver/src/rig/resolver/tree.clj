(ns rig.resolver.tree
  "Dependency tree for display (design §7.3): the resolved dependency graph
  of one module. Floating requirements are pinned to the lock's versions
  first, so with a warm ~/.m2 repository no network is needed."
  (:require [clojure.java.io :as io]
            [clojure.tools.deps.extensions :as ext]
            [clojure.tools.deps.tree :as td-tree]
            [rig.resolver.manifest :as manifest]
            [rig.resolver.resolve :as resolve]
            [rig.resolver.versions :as versions]))

(defn- node-id
  "Display id of a traced lib: group/name:version (mvn),
  group/name:<tag|sha7> (git), local:<root> (local modules)."
  [lib coord]
  (let [t (ext/coord-type coord)]
    (case t
      :local (str "local:" (get-in coord [:local/root]))
      (str lib
           (when-let [v (case t
                          :mvn (get-in coord [:mvn/version])
                          :git (or (get-in coord [:git/tag])
                                   (some-> (get-in coord [:git/sha]) (subs 0 7))))]
             (str ":" v))))))

(defn- flatten-nodes
  "Every traced node as {\"coord\" … \"depth\" n \"via\" [root … direct
  parent]}; the first (shallowest) occurrence of a coord wins."
  [tree root]
  (loop [q (map (fn [n] [n [root]]) (vals (:children tree)))
         seen {}]
    (if (empty? q)
      (vals seen)
      (let [[n via] (first q)
            id (node-id (:lib n) (:coord n))
            kids (map (fn [c] [c (conj via id)]) (vals (:children n)))]
        (if (and (true? (:include n)) (not (contains? seen id)))
          (recur (concat (rest q) kids)
                 (assoc seen id {"coord" id "depth" (count via) "via" (vec via)}))
          (recur (concat (rest q) kids) seen))))))

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
                                :aliases (when (seq alias) [(keyword alias)])}))
        nodes (->> (flatten-nodes t root)
                   (sort-by (juxt :depth :coord)))]
    (if (seq refused)
      (throw (ex-info (str "tree: cannot resolve floating version(s): "
                           (apply str (interpose ", " (map str (map :coord refused))))
                           " — run `rig lock` (online) to pin them first") {}))
      {"root" root "nodes" (vec nodes)})))
