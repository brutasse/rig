(ns rig.resolver.aot
  "AOT compile plans for a module's Clojure sources: the non-project
  namespaces to preload and the module's own namespaces in compile
  order.

  A namespace that references a sibling without requiring it — a bare
  ns/var, or a class produced only by AOT-compiling the sibling —
  resolves only when the referenced namespace is already loaded. lein's
  AOT flow provided that shared state by accident of its load/compile
  interplay; the plan reproduces it deliberately: every namespace the
  module requires is preloaded before the module's own namespaces
  compile, and those compile in dependency order."
  (:require [clojure.java.io :as io]
            [clojure.string :as str]
            [clojure.tools.namespace.dependency :as dep]
            [clojure.tools.namespace.file :as file]
            [clojure.tools.namespace.parse :as parse]))

(defn- ns-files
  "The module's .clj/.cljc sources under src-dirs, as [root file] pairs,
  with the runner's load-all skip set (the Go-side walk of rig check
  keeps the same set): no *_init.clj, no data-readables files. Missing
  dirs are skipped."
  [src-dirs]
  (->> (mapcat (fn [d]
                 (when-let [root (io/file d)]
                   (when (.isDirectory root)
                     (map (partial vector root) (file-seq root)))))
               src-dirs)
       (filter (fn [[_ ^java.io.File f]]
                 (let [n (.getName f)]
                   (and (.isFile f)
                        (or (str/ends-with? n ".clj") (str/ends-with? n ".cljc"))
                        (not (str/ends-with? n "_init.clj"))
                        (not (#{"data_readers.clj" "data_readers.cljc"} n))))))
       (sort-by (fn [[_ ^java.io.File f]] (.getPath f)))))

(defn- path-stem
  "f's loadable path under root: relative to root, extension dropped —
  what clojure.core/load looks a namespace up by."
  [^java.io.File root ^java.io.File f]
  (-> (.getPath f)
      (.substring (inc (.length (.getPath root))))
      (.replaceAll "\\.cljc?$" "")))

(defn- loadable?
  "Whether the classpath resolves ns to f: load munges the namespace
  symbol (dots to slashes, dashes to underscores) and looks for that
  path. A file below a resources root — exported clj-kondo hooks,
  templates — declares a namespace that resolves elsewhere, so it is
  jar payload, not a compilable namespace of the module."
  [[root f] ns]
  (= (path-stem root f)
     (-> (name ns) (str/replace "." "/") (str/replace "-" "_"))))

(def ^:private unreadable (keyword "rig.resolver.aot" "unreadable"))

(defn- read-decl
  "The (ns ...) declaration of f: nil when the file declares no
  namespace, `unreadable` when it does not even parse. A .clj under
  :paths that is not a readable ns form is data by definition —
  tools.deps puts config files on the classpath precisely as resources
  (a bare migratus map, say) and deps-new templates carry {{vars}} no
  reader gets past — the plan skips such files instead of dying on
  them."
  [^java.io.File f]
  (try
    (file/read-file-ns-decl f parse/clj-read-opts)
    (catch Exception _ unreadable)))

(defn plan
  "The AOT plan for the sources under src-dirs, with the declared entry
  points extra (the manifest's :rig/ns-compile, as strings).

  Returns
    {:preload [ns-str ...] every non-project namespace the module's
                           namespaces require, sorted: the shared state
                           the compile phase needs up front
     :compile [ns-str ...] the module's namespaces, dependencies first
                           (topo order — a require cycle in the module
                           is a plan error, as in tools.build), then
                           the entry points not found among the
                           sources.
     :skipped [{:file p :reason r} ...]
                           sources left out as data or because their
                           namespaces do not resolve from their paths}"
  [src-dirs extra]
  (let [parsed (for [[root f] (ns-files src-dirs)]
                 [[root f] (read-decl f)])
        skipped (vec
                 (mapcat (fn [[[root f] d]]
                           (cond
                             (nil? d)
                             [{:file (.getPath f) :reason "no ns declaration"}]

                             (= d unreadable)
                             [{:file (.getPath f) :reason "unreadable ns declaration"}]

                             (not (loadable? [root f]
                                             (parse/name-from-ns-decl d)))
                             [{:file (.getPath f)
                               :reason "namespace does not match classpath path"}]

                             :else []))
                         parsed))
        ;; A nested .clj whose namespace does not map to its classpath path
        ;; (exported hooks and templates under a resources root) is data,
        ;; not a compilable namespace.
        decls (for [[[root f] d] parsed
                    :when (and d
                               (not= d unreadable)
                               (loadable? [root f]
                                          (parse/name-from-ns-decl d)))]
                (let [ns (parse/name-from-ns-decl d)]
                  [ns (parse/deps-from-ns-decl d)]))
        ns->deps (into {} decls)
        project (set (keys ns->deps))
        preload (vec (map str
                          (sort
                           (clojure.set/difference
                            (reduce clojure.set/union #{} (vals ns->deps))
                            project))))
        ;; Project edges only: the graph's nodes are the module's
        ;; namespaces, a dependency node per required project
        ;; namespace. topo-sort yields dependencies first — the
        ;; compile order, as tools.build's nses-in-topo uses it.
        graph (reduce (fn [g [ns deps]]
                        (reduce (fn [g d] (dep/depend g ns d))
                                g
                                ;; A namespace :requiring itself is legal —
                                ;; the JVM loads and compiles it with the
                                ;; edge ignored — but a self-edge in the
                                ;; graph is "Circular dependency between X
                                ;; and X". Drop it.
                                (disj (clojure.set/intersection deps project) ns)))
                      (dep/graph)
                      ns->deps)
        ordered (vec (dep/topo-sort graph))
        stragglers (vec (clojure.set/difference project (set ordered)))
        compile (vec (map str
                          (distinct
                           (concat ordered stragglers (map symbol extra)))))]
    {:preload preload :compile compile :skipped skipped}))

(defn aot-plan
  "Kernel op: the AOT plan of the request's sources (:args :src-dirs,
  :ns-compile)."
  [request]
  (plan (get-in request [:args :src-dirs])
        (get-in request [:args :ns-compile])))
