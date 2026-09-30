(ns rig.resolver.build
  "Builds modules from a locked classpath via tools.build.

  No re-resolution: the classpath arrives fully resolved (absolute paths)
  from the Go side, and is turned into a tools.build basis. Each module is
  (optionally java-) then AOT-compiled, then jarred and/or ubered per the
  :rig/build config recorded in the lock."
  (:require [cheshire.core :as json]
            [clojure.java.io :as io]
            [clojure.string :as str]
            [clojure.tools.build.api :as b]
            [clojure.tools.build.tasks.process :as process]
            [clojure.tools.build.util.file :as file]
            [rig.resolver.aot :as aot]
            [rig.resolver.floor :as floor])
  (:import (java.nio.file Files)
           (java.nio.file.attribute FileAttribute)))

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
  launch plan (`rig launch` reads it). Deletes a stale descriptor left by
  a previous build when this build declares no :launch, so no plan ships
  with the artifact it no longer declares."
  [class-dir launch]
  (let [f (io/file class-dir "META-INF/rig/launch.json")]
    (if launch
      (do (io/make-parents f)
          (spit f (json/encode launch)))
      (when (.exists f) (io/delete-file f)))))

(defn- under-ensure?
  "True when path is one of the prep :ensure paths or lives under one.
  Paths arrive from the Go side (filepath.Join) and from file-seq, both
  platform-native, so plain string comparison is safe."
  [path ensures]
  (some (fn [en]
          (or (= path en)
              (str/starts-with? (str path "/") (str en "/"))))
        ensures))

(defn- clean-class-dir
  "Wipes the build's own previous output from the class-dir: a .class left
  from a previous build would shadow the source and its namespace would
  never be recompiled (silent stale bytecode). Full AOT is the price of
  correctness. Content under the module's :deps/prep-lib :ensure paths is
  prep output — input to the build, not its output — and survives: when a
  prep owns the class-dir (its :ensure covers it), wiping would destroy
  what the prep guarantees and the jar would ship without it."
  [class-dir ensures]
  (let [root (io/file class-dir)]
    (when (and (.exists root) (not (under-ensure? (str root) ensures)))
      (dorun (for [f (file-seq root)
                   :when (and (not (identical? f root))
                               (not (under-ensure? (str f) ensures)))]
               (file/delete f))))))

(defn- drop-sources
  "Deletes the .clj/.cljc the copy step brought into the class-dir (see
  build-module): a source next to its AOT __init.class in the jar is an
  RT.load recompile hazard. Prep output under an :ensure path is the
  build's input, not its output, and survives — the same carve-out as
  clean-class-dir."
  [class-dir ensures]
  (let [root (io/file class-dir)]
    (when (.exists root)
      (dorun (for [f (file-seq root)
                   :when (and (not (identical? f root))
                              (.isFile f)
                              (not (under-ensure? (str f) ensures))
                              (or (str/ends-with? (str f) ".clj")
                                  (str/ends-with? (str f) ".cljc")))]
               (file/delete f))))))

(defn- script-text
  "The two-phase compile script: the preloads run under the AOT bindings,
  so a dependency that requires the module's code AOTs it on load (parity
  with tools.build's require side effects), then each module namespace
  compiles — skipped when a preload already loaded it, so its top level
  never runs twice."
  [class-dir plan]
  (str
   "(with-bindings\n"
   " {#'clojure.core/*compile-files* true\n"
   "  #'clojure.core/*compile-path* " (pr-str class-dir) "}\n"
   (when (seq (:preload plan))
     (str " (require "
          (str/join " " (map (fn [ns] (str "'" ns)) (:preload plan)))
          ")\n"))
   (str/join "\n" (map (fn [ns]
                         (str " (when-not (find-ns '" ns ")\n"
                              "   (compile '" ns "))"))
                       (:compile plan)))
   "\n (System/exit 0)\n)"))

(defn- aot-compile
  "Two-phase AOT compile of the module's own namespaces, in place of
  b/compile-clj: the plan's preloads (the non-project namespaces the
  module requires) load first — a bare ns/var or AOT-class reference
  resolves only when the referenced namespace is loaded — then the
  module's namespaces compile in dependency order. The fork mirrors
  tools.build's compile-clj: a working class dir leads the compile
  classpath, clojure.main runs the script, and on success the working
  dir is copied into the class-dir; on failure the working dir is
  preserved (script and command line included) for inspection. The
  fork inherits the workspace's :rig/compile-jvm-opts as JVM flags —
  the flags must reach the JVM that compiles, not just the kernel."
  [basis class-dir artifact-dirs extra compile-jvm-opts]
  (let [plan (aot/plan artifact-dirs extra)]
    (when (seq (:compile plan))
      (let [working (.toFile (Files/createTempDirectory "rig-aot-"
                                                        (into-array FileAttribute [])))
            wdir (file/ensure-dir (io/file working "classes"))
            script (io/file working "compile.clj")
            _ (io/make-parents class-dir)
            _ (spit script (script-text (.getPath wdir) plan))
            args (process/java-command {:cp [(.getPath wdir) (str class-dir)]
                                        :java-opts compile-jvm-opts
                                        :basis basis
                                        :main 'clojure.main
                                        :main-args [(.getCanonicalPath script)]})
            _ (spit (io/file working "compile.args")
                    (str/join " " (:command-args args)))
            exit (:exit (process/process args))]
        (if (zero? exit)
          (do (b/copy-dir {:src-dirs [(.getPath wdir)] :target-dir class-dir})
              (file/delete working))
          (throw (ex-info (str "Clojure compilation failed, working dir "
                               "preserved: " (.toString working))
                          {:exit exit})))))))

(defn- build-module [cfg]
  (let [basis (basis (get cfg :classpath))
        class-dir (get cfg :class-dir)
        artifact-dirs (get cfg :artifact-dirs)
        java-src-dirs (get cfg :java-src-dirs)
        copy-dirs (filter #(and % (.isDirectory (io/file %))) artifact-dirs)
        extra (get cfg :ns-compile)
        ensure (get cfg :prep-ensure)]
    ;; Wipe the build's own previous output, not the prep's (see
    ;; clean-class-dir).
    (clean-class-dir class-dir (or ensure []))
    ;; Copy src/resource dirs into the class-dir so resources land in the
    ;; jar/uber. compile-clj only compiles .clj; it never copies.
    (when (seq copy-dirs)
      (b/copy-dir {:src-dirs copy-dirs :target-dir class-dir}))
    ;; The copy drags the module's Clojure sources along; drop them again
    ;; (see drop-sources).
    (drop-sources class-dir (or ensure []))
    ;; javac before the AOT compile: the module's own Java classes must
    ;; already sit in the class-dir (second on the compile classpath,
    ;; after the working class dir) so that Clojure code referencing
    ;; them compiles.
    (when (seq java-src-dirs)
      (b/javac {:basis basis :src-dirs java-src-dirs :class-dir class-dir
                :javac-opts (get cfg :javac-opts)}))
    ;; Two-phase AOT compile: the module's non-project requires preload,
    ;; then its namespaces (plus the :ns-compile entry points) compile in
    ;; dependency order.
    (aot-compile basis class-dir artifact-dirs extra
                 (get cfg :compile-jvm-opts))
    (write-launch-descriptor class-dir (get cfg :launch))
    (let [result (cond-> {:class-dir class-dir}
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
                                  (get cfg :uber-file))))]
      ;; The workspace's pinned JVM is the bytecode floor of every jar
      ;; this build produces: a class above it would fail to load there.
      ;; No floor in the config (the workspace pins no JVM) means no scan.
      (when-let [floor (get cfg :floor)]
        (doseq [f (filter some? (map (partial get result) [:jar :uber]))]
          (floor/assert-floor f floor)))
      result)))

(defn build
  "Errors propagate: as a kernel op, rig.resolver/main maps them to exit 1."
  [request]
  (let [builds (get-in request [:args :builds])]
    {:results (vec
               (for [[module cfg] builds]
                 (assoc (build-module cfg) :module module)))}))
