(ns rig.resolver.build
  "Builds modules from a locked classpath via tools.build.

  No re-resolution: the classpath arrives fully resolved (absolute paths)
  from the Go side, and is turned into a tools.build basis. Each module is
  compiled (and optionally java-compiled), then jarred and/or ubered per the
  :rig/build config recorded in the lock."
  (:require [cheshire.core :as json]
            [clojure.java.io :as io]
            [clojure.string :as str]
            [clojure.tools.build.api :as b]
            [clojure.tools.build.util.file :as file]
            [clojure.tools.namespace.find :as find]))

(defn- basis
  "Fabricate a tools.build basis from [{:id ... :paths [...]} ...] (lock
  order). :libs is keyed by entry id, which is what b/uber iterates: mvn
  artifacts, git deps and local modules go in; the module's own \"paths:…\"
  entry does not (its compiled classes come from class-dir)."
  [classpath]
  (let [paths (mapcat :paths classpath)]
    {:classpath-roots paths
     :classpath paths
     :deps {}
     :libs (into {}
                 (for [e classpath
                       :when (not (str/starts-with? (get e :id) "paths:"))]
                   [(get e :id) {:paths (into #{} (:paths e))}]))}))

(defn- write-launch-descriptor
  "Writes the build's :launch config into the class-dir, where the jar and
  the uber pick it up as META-INF/rig/launch.json — the artifact's own
  launch plan (`rig launch` reads it). Absent when :launch is not in the
  config."
  [class-dir launch]
  (when launch
    (let [f (io/file class-dir "META-INF/rig/launch.json")]
      (io/make-parents f)
      (spit f (json/encode launch)))))

(defn- build-module [cfg]
  (let [basis (basis (get cfg :classpath))
        class-dir (get cfg :class-dir)
        src-dirs (get cfg :src-dirs)
        java-src-dirs (get cfg :java-src-dirs)
        copy-dirs (filter #(and % (.isDirectory (io/file %))) src-dirs)
        extra (get cfg :ns-compile)]
    ;; The compile classpath puts the class-dir first, so any .class left from
    ;; a previous build shadows the source and its namespace is never
    ;; recompiled (silent stale bytecode). Every build starts from a clean
    ;; class-dir; full AOT is the price of correctness here.
    (file/delete class-dir)
    ;; Copy src/resource dirs into the class-dir so resources (and sources)
    ;; land in the jar/uber. compile-clj only compiles .clj; it never copies.
    (when (seq copy-dirs)
      (b/copy-dir {:src-dirs copy-dirs :target-dir class-dir}))
    ;; :ns-compile replaces the default (src dirs) set, so pass the union:
    ;; the module's own namespaces plus the declared entry points. The
    ;; compile script needs symbols (clojure.core/compile munges them).
    (b/compile-clj (cond-> {:basis basis :src-dirs src-dirs :class-dir class-dir}
                     (seq extra)
                     (assoc :ns-compile
                            (distinct (map symbol
                                           (concat
                                            (mapcat (fn [d] (find/find-namespaces-in-dir (io/file d) find/clj))
                                                    src-dirs)
                                            extra))))))
    (when (seq java-src-dirs)
      ;; b/javac's option key is :javac-opts; the module config carries the
      ;; lockfile spelling :compile-opts.
      (b/javac (cond-> {:basis basis :src-dirs java-src-dirs :class-dir class-dir}
                 (seq (get cfg :compile-opts))
                 (assoc :javac-opts (get cfg :compile-opts)))))
    (write-launch-descriptor class-dir (get cfg :launch))
    (cond-> {:class-dir class-dir}
      (get cfg :jar?)
      (assoc :jar (do
                     (b/jar {:basis basis
                             :class-dir class-dir
                             :jar-file (get cfg :jar-file)
                             :main (get cfg :main)})
                     (get cfg :jar-file)))
      (get cfg :uber?)
      (assoc :uber (do
                      (b/uber {:basis basis
                               :class-dir class-dir
                               :uber-file (get cfg :uber-file)
                               :main (get cfg :main)
                               :exclude (get cfg :exclude)})
                      (get cfg :uber-file))))))

(defn build
  "Errors propagate: as a kernel op, rig.resolver/main maps them to exit 1."
  [request]
  (let [builds (get-in request [:args :builds])]
    {:results (vec
               (for [[module cfg] builds]
                 (assoc (build-module cfg) :module module)))}))
