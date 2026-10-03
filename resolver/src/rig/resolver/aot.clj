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
  "The module's .clj/.cljc sources under src-dirs, with the runner's
  load-all skip set (the Go-side walk of rig check keeps the same
  set): no *_init.clj, no data-readables files. Missing dirs are
  skipped."
  [src-dirs]
  (->> (mapcat (fn [d]
                 (when-let [f (io/file d)]
                   (when (.isDirectory f) (file-seq f))))
                src-dirs)
       (filter (fn [^java.io.File f]
                 (let [n (.getName f)]
                   (and (.isFile f)
                        (or (str/ends-with? n ".clj") (str/ends-with? n ".cljc"))
                        (not (str/ends-with? n "_init.clj"))
                        (not (#{"data_readers.clj" "data_readers.cljc"} n))))))
       (sort-by str)))

(defn- read-decl
  "The (ns ...) declaration of f: nil when the file declares no
  namespace, an exception naming f on a read failure. A file the
  module cannot load is a plan failure, not something to skip — the
  check must not turn green because a broken source was left out of
  the plan."
  [^java.io.File f]
  (try
    (file/read-file-ns-decl f parse/clj-read-opts)
    (catch Exception e
      (throw (ex-info (str "cannot read the ns declaration of " (.getPath f))
                      {:file (.getPath f)} e)))))

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
                           sources.}"
  [src-dirs extra]
  (let [decls (for [f (ns-files src-dirs)]
                (let [d (read-decl f)]
                  (when-not d
                    (throw (ex-info (str "no ns declaration in " (.getPath f))
                                    {:file (.getPath f)})))
                  [f d]))
        ns->deps (into {} (for [[_ d] decls]
                            [(parse/name-from-ns-decl d)
                             (parse/deps-from-ns-decl d)]))
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
    {:preload preload :compile compile}))

(defn aot-plan
  "Kernel op: the AOT plan of the request's sources (:args :src-dirs,
  :ns-compile)."
  [request]
  (plan (get-in request [:args :src-dirs])
        (get-in request [:args :ns-compile])))
