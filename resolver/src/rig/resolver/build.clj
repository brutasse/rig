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
           (java.nio.file StandardCopyOption)
           (java.nio.file.attribute FileAttribute)
           (java.util.zip ZipEntry ZipFile ZipOutputStream)))

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
      (doseq [f (file-seq root)
              :when (and (not (identical? f root))
                         (not (under-ensure? (str f) ensures)))]
        (file/delete f)))))

(defn- drop-sources
  "Deletes from the class-dir the sources paired with their AOT
  __init.class (see build-module; runs after the compile): a source next
  to its AOT class in the jar is an RT.load recompile hazard. Unpaired
  sources survive — a classpath-unreachable .clj below a resources root
  (exported clj-kondo hooks, templates) is jar payload with no class to
  pair with, and deleting it silently stripped the payload from the
  published jar. Prep output under an :ensure path is the build's input,
  not its output, and survives — the same carve-out as clean-class-dir."
  [class-dir ensures]
  (let [root (io/file class-dir)]
    (when (.exists root)
      (doseq [f (file-seq root)
              :let [n (str f)
                    ext (cond (str/ends-with? n ".cljc") 5
                              (str/ends-with? n ".clj") 4
                              :else 0)]
              :when (and (pos? ext)
                         (not (identical? f root))
                         (.isFile f)
                         (not (under-ensure? n ensures))
                         (.exists (io/file (str (subs n 0 (- (count n) ext))
                                                "__init.class"))))]
        (file/delete f)))))

(defn- pinned-time-ms
  "The build's :rig/timestamp-string as epoch millis, or nil when the
  build declares no pin: the entry timestamps of every jar it builds,
  and the switch for byte-identical output (see finalize-jar). An
  ISO-8601 instant string; zip timestamps have a 2-second resolution
  and cannot represent times before 1980-01-01 — the writer would
  clamp silently, so both are refused here."
  [s]
  (when s
    (let [t (try
              (java.time.Instant/parse s)
              (catch java.time.format.DateTimeParseException _
                (throw (ex-info (str "bad :rig/timestamp-string " (pr-str s)
                                     " (want an ISO-8601 instant, e.g. \"2026-01-01T00:00:00Z\")")
                                {}))))]
      (when (< (.toEpochMilli t) 315532800000)
        (throw (ex-info (str ":rig/timestamp-string " s
                             " is before 1980-01-01: zip timestamps cannot represent it")
                        {})))
      (.toEpochMilli t))))

(defn- finalize-jar
  "Re-zips the jar, dropping every .clj/.cljc that sits next to its AOT
  __init.class. RT.load loads the source instead of the class whenever the
  source is not strictly older, and the sources b/uber pulls in from
  exploded dependency jars carry no such ordering — equal timestamps would
  make the runtime recompile the namespace from the bundled source (two
  class identities). A source with no base __init is the namespace's only
  payload and is kept; an __init that exists only under
  META-INF/versions/ is not a base class, so the source it alone pairs
  with stays as the floor's fallback. Without a pinned time (epoch
  millis, the build's :rig/timestamp-string), every other entry is
  copied verbatim — order, bytes, timestamps. With one, the entries are
  emitted in name-sorted order, all stamped with it: same sources, lock
  and build JVM then yield a byte-identical jar."
  [jar ts]
  (let [tmp (str jar ".strip-tmp")
        in (ZipFile. jar)]
    (try
      (do
        (let [entries (java.util.Collections/list (.entries in))
              inits (into #{}
                          (for [^ZipEntry e entries
                                :let [n (.getName e)]
                                :when (str/ends-with? n "__init.class")
                                :when (not (str/starts-with? n "META-INF/versions/"))]
                            n))]
          (with-open [out (ZipOutputStream. (io/output-stream tmp))]
            (doseq [^ZipEntry e (if ts (sort-by #(.getName %) entries) entries)]
              (let [n (.getName e)
                    ext (cond (str/ends-with? n ".cljc") 5
                              (str/ends-with? n ".clj") 4
                              :else 0)]
                (when (not (and (pos? ext)
                                (contains? inits
                                          (str (subs n 0 (- (count n) ext))
                                               "__init.class"))))
                  (let [e2 (ZipEntry. n)]
                    (.setTime e2 (or ts (.getTime e)))
                    (.putNextEntry out e2)
                    (io/copy (.getInputStream in e) out)
                    (.closeEntry out)))))))
        (Files/move (.toPath (io/file tmp))
                    (.toPath (io/file jar))
                    (into-array java.nio.file.CopyOption
                               [StandardCopyOption/ATOMIC_MOVE])))
      (finally
        (.close in)
        (when (.exists (io/file tmp)) (io/delete-file tmp))))
    jar))

(defn- assert-no-stale-aot
  "Fail the build when a classpath jar ships a .clj/.cljc strictly newer
  than its AOT __init.class: in that jar the source is what the runtime
  loads, so the bundled class is stale, and the jar-level strip would
  drop the source and silently run the class. A jar like that is
  self-inconsistent — refuse to guess which is the payload. Equal
  timestamps pass: the class is same-build, and class-wins is the fix,
  not the hazard. Jar paths only — directory entries are working
  copies, not shipped artifacts."
  [classpath]
  (let [stale (for [p (mapcat :paths classpath)
                    :when (str/ends-with? p ".jar")
                    :let [zf (ZipFile. p)
                          times (try
                                  (reduce
                                   (fn [m ^ZipEntry e]
                                     (assoc m (.getName e) (.getTime e)))
                                   {}
                                   (java.util.Collections/list
                                    (.entries zf)))
                                  (finally (.close zf)))
                          bad (for [n (sort (keys times))
                                    :let [ext (cond (str/ends-with? n ".cljc") 5
                                                    (str/ends-with? n ".clj") 4
                                                    :else 0)
                                          init (when (pos? ext)
                                                 (str (subs n 0
                                                            (- (count n) ext))
                                                      "__init.class"))]
                                    :when (and init
                                               (not (str/starts-with? n
                                                                      "META-INF/versions/"))
                                               (some? (get times init))
                                               (> (get times n)
                                                  (get times init)))]
                                  n)]
                    :when (seq bad)]
                  (str p " ships " (str/join ", " bad)
                       " strictly newer than its AOT class"))]
    (when (seq stale)
      (throw (ex-info
              (str "STALE AOT — refusing to build. A dependency ships source\n"
                   "strictly newer than its AOT class; the runtime would load\n"
                   "the source, and the jar-level strip would run the stale\n"
                   "class instead:\n"
                   (str/join "\n" (map (partial str "  ") stale)))
              {:stale stale})))))

(defn- script-text
  "The two-phase compile script: the preloads run under AOT bindings whose
   *compile-path* is a scratch dir — classes emitted by loading a
   dependency are the dependency's own bytecode, already on the classpath
   in its jar, and emitting them where the jar step packages would make
   library jars ship other projects' classes. Each module namespace then
   compiles into the class-dir staging dir — skipped when a preload
   already loaded it, so its top level never runs twice. A module's
   compile loads nothing the preloads did not already load, so only
   pathological dynamic requires can emit to the staging dir."
  [class-dir scratch-dir plan]
  (str
   "(with-bindings\n"
   " {#'clojure.core/*compile-files* true\n"
   "  #'clojure.core/*compile-path* " (pr-str scratch-dir) "}\n"
   (when (seq (:preload plan))
     (str " (require "
          (str/join " " (map (fn [ns] (str "'" ns)) (:preload plan)))
          ")\n"))
   ")\n"
   "(with-bindings\n"
   " {#'clojure.core/*compile-files* true\n"
   "  #'clojure.core/*compile-path* " (pr-str class-dir) "}\n"
   (str/join "\n" (map (fn [ns]
                          (str " (when-not (find-ns '" ns ")\n"
                               "   (compile '" ns "))"))
                        (:compile plan)))
   "\n (System/exit 0)\n)"))

(defn- java-exe
  "The java executable of the running JVM. The AOT fork is the kernel's own
  JVM: rig launches the kernel with the workspace's picked JDK, so the
  compile happens on the pinned JVM. tools.build's default lookup ($JAVA_CMD,
  then java on PATH, then $JAVA_HOME) ignores the running JVM, and a green
  build would silently compile on the host's java."
  []
  (.getPath (io/file (System/getProperty "java.home") "bin"
                     (if (str/starts-with? (System/getProperty "os.name") "Win")
                       "java.exe" "java"))))

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
  fork runs on the kernel's own JVM (java-exe) and inherits the
  workspace's :rig/compile-jvm-opts as JVM flags — the flags must reach
  the JVM that compiles, not just the kernel."
  [basis class-dir artifact-dirs extra compile-jvm-opts]
  (let [plan (aot/plan artifact-dirs extra)]
    (when (seq (:compile plan))
      (let [working (.toFile (Files/createTempDirectory "rig-aot-"
                                                        (into-array FileAttribute [])))
            wdir (file/ensure-dir (io/file working "classes"))
            pdir (file/ensure-dir (io/file working "preload-classes"))
            script (io/file working "compile.clj")
            _ (io/make-parents class-dir)
            _ (spit script (script-text (.getPath wdir) (.getPath pdir) plan))
            args (process/java-command {:java-cmd (java-exe)
                                        :cp [(.getPath wdir) (str class-dir)]
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

(defn- rel-files
  "Files under root, paths relative to root, slash-separated."
  [root]
  (let [r (.getPath (io/file root))
        n (inc (count r))]
    (for [f (file-seq (io/file root))
          :when (.isFile f)]
      (subs (.getPath f) n))))

(defn- warn-unmatched-excludes
  "tools.build matches :exclude patterns with re-matches — a FULL match
  against entry names — so a plausible prefix pattern like \"^clojure/\"
  silently excludes nothing. Warn for every pattern that matches no entry
  in this build's own entry universe (class-dir, classpath dirs, classpath
  jar entries). Jar DIRECTORY entries drop out of the universe: a pattern
  that only hits container entries excludes no class."
  [basis class-dir patterns]
  (when (seq patterns)
    (let [jar-entries (fn [^java.io.File f]
                        (with-open [zf (ZipFile. f)]
                          (doall (map #(.getName ^ZipEntry %)
                                      (enumeration-seq (.entries zf))))))
          root-names (fn [root]
                       (let [f (io/file root)]
                         (cond
                           (.isDirectory f) (rel-files f)
                           (str/ends-with? (str f) ".jar") (jar-entries f))))
          names (into #{}
                      (comp (mapcat root-names)
                            (remove #(.endsWith ^String % "/")))
                      (cons class-dir (:classpath-roots basis)))]
      (doseq [p patterns
              :when (and (string? p)
                         (not (some #(re-matches (re-pattern p) %) names)))]
        (println (str "uberjar :exclude pattern " (pr-str p) " matched no entry — "
                      "patterns are FULL matches against entry names "
                      "(re-matches): use \"dir/.*\", not \"^dir/\""))))))

(defn- build-module [cfg]
  (let [ts (pinned-time-ms (get cfg :timestamp-string))
        basis (basis (get cfg :classpath))
        class-dir (get cfg :class-dir)
        artifact-dirs (get cfg :artifact-dirs)
        java-src-dirs (get cfg :java-src-dirs)
        copy-dirs (filter #(and % (.isDirectory (io/file %))) artifact-dirs)
        extra (get cfg :ns-compile)
        ensure (get cfg :prep-ensure)]
    ;; A stale-AOT dep jar would ship a source the jar-level strip drops,
    ;; running the stale class: refuse before the compile spends minutes.
    ;; Uber only — a plain jar never carries dependency sources.
    (when (get cfg :uber?)
      (assert-no-stale-aot (get cfg :classpath)))
    ;; Wipe the build's own previous output, not the prep's (see
    ;; clean-class-dir).
    (clean-class-dir class-dir (or ensure []))
    ;; Copy src/resource dirs into the class-dir so resources land in the
    ;; jar/uber. compile-clj only compiles .clj; it never copies.
    (when (seq copy-dirs)
      (b/copy-dir {:src-dirs copy-dirs :target-dir class-dir}))
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
    ;; The copy dragged the module's Clojure sources into the class-dir;
    ;; now that the compile paired them with classes, drop them again
    ;; (see drop-sources).
    (drop-sources class-dir (or ensure []))
    (write-launch-descriptor class-dir (get cfg :launch))
    (let [result (cond-> {:class-dir class-dir}
                   (get cfg :jar?)
                   (assoc :jar (do
                                 (b/jar {:basis basis
                                         :class-dir class-dir
                                         :jar-file (get cfg :jar-file)
                                         :main (get cfg :main)})
                                 (finalize-jar (get cfg :jar-file) ts)))
                   (get cfg :uber?)
                   (assoc :uber (do
                                  (warn-unmatched-excludes basis class-dir (get cfg :exclude))
                                  (b/uber {:basis basis
                                           :class-dir class-dir
                                           :uber-file (get cfg :uber-file)
                                           :main (get cfg :main)
                                           :exclude (get cfg :exclude)})
                                  (finalize-jar (get cfg :uber-file) ts))))]
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
