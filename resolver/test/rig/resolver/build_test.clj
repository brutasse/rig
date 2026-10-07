(ns rig.resolver.build-test
  (:require [cheshire.core :as json]
            [clojure.java.io :as io]
            [clojure.string :as str]
            [clojure.test :refer :all]
            [clojure.tools.build.tasks.process :as process]
            [rig.resolver.build :as build]
            [rig.resolver.test-util :refer [delete-tree temp-dir]]))

(defn- dep-jars
  "The dependency jars of the running JVM's classpath (directories excluded).
  In this environment the clojure jar does not bundle clojure.spec.alpha, so a
  bare clojure classpath cannot launch a JVM — the build must carry the full
  resolved dependency set so the forked compile resolves deps and the uberjar
  is self-contained."
  []
  (filter #(str/ends-with? % ".jar")
          (str/split (System/getProperty "java.class.path")
                     (re-pattern (str (java.io.File/pathSeparatorChar))))))

(defn- cp-entries
  "Classpath entries (one per dependency jar) for the build request."
  []
  (map (fn [p] {:id p :paths [p]}) (dep-jars)))

(defn- java
  []
  (str (System/getProperty "java.home") "/bin/java"))

(defn- run-cmd
  "Run a command. Returns {:exit int :out string :err string}."
  [& cmd]
  (let [p (.start (ProcessBuilder. (into-array String cmd)))
        out (slurp (.getInputStream p))
        err (slurp (.getErrorStream p))]
    {:exit (.waitFor p) :out out :err err}))

(defn- zip-names
  "The entry names of the jar at path."
  [path]
  (let [zf (java.util.zip.ZipFile. (io/file path))]
    (try
      (set (map (fn [e] (str (.getName ^java.util.zip.ZipEntry e)))
                (java.util.Collections/list (.entries zf))))
      (finally (.close zf)))))

(defn- zip-entries
  "The [name time] pairs of the jar at path, in stored entry order."
  [path]
  (let [zf (java.util.zip.ZipFile. (io/file path))]
    (try
      (vec (for [^java.util.zip.ZipEntry e (java.util.Collections/list (.entries zf))]
             [(.getName e) (.getTime e)]))
      (finally (.close zf)))))

(defn- entry-string
  "The content of the named entry of the jar at path, as a string."
  [path name]
  (let [zf (java.util.zip.ZipFile. (io/file path))]
    (try
      (with-open [is (.getInputStream zf (.getEntry zf name))]
        (slurp is))
      (finally (.close zf)))))

(defn- manifest-attrs
  "The main manifest attributes of the jar at path, as a string-keyed map."
  [path]
  (let [zf (java.util.zip.ZipFile. (io/file path))]
    (try
      (when-let [e (.getEntry zf "META-INF/MANIFEST.MF")]
        (with-open [is (.getInputStream zf e)]
          (let [mf (java.util.jar.Manifest.)
                attrs (.getMainAttributes mf)]
            (.read mf is)
            (into {} (for [^java.util.jar.Attributes$Name k (.toArray (.keySet attrs))]
                       [(str k) (str (.get attrs k))])))))
      (finally (.close zf)))))

(defn- launch-entry
  "The parsed META-INF/rig/launch.json entry of the jar at path, nil when
  absent."
  [path]
  (let [zf (java.util.zip.ZipFile. (io/file path))]
    (try
      (when-let [e (.getEntry zf "META-INF/rig/launch.json")]
        (with-open [is (.getInputStream zf e)]
          (json/parse-string (slurp is))))
      (finally (.close zf)))))

(defn- cfg
  [ws src-root res-root uber?]
  (let [class-dir (str ws "/target/classes")
        jar-file (str ws "/target/fixture.jar")
        uber-file (str ws "/target/fixture-uber.jar")]
    {:dir ws
     :classpath (concat [{:id "paths:." :paths [(str src-root)]}] (cp-entries))
     :artifact-dirs [(str src-root) (str res-root)]
     :java-src-dirs []
     :class-dir class-dir
     :main "example.core"
     :jar? (not uber?)
     :jar-file jar-file
     :uber? uber?
     :uber-file uber-file
     :exclude []}))

(deftest build-compiles-jars-and-ubers
  (let [ws-dir (temp-dir)
        ws (str ws-dir)
        src-root (io/file ws-dir "src")
        src-file (io/file src-root "example" "core.clj")
        res-root (io/file ws-dir "resources")
        res-file (io/file res-root "msg.txt")]
    (io/make-parents src-file)
    (spit src-file
          "(ns example.core\n  (:gen-class))\n(defn -main [& args]\n  (println (str \"hello \" (apply str args))))\n")
    (io/make-parents res-file)
    (spit res-file "from-resources")
    (try
      (let [jar-cfg (cfg ws src-root res-root false)
            jar-result (build/build {:args {:builds {"." jar-cfg}}})
            jar-file (str ws "/target/fixture.jar")]
          (is (= jar-file (get-in jar-result [:results 0 :jar])))
         (is (.exists (io/file jar-file)))
         (is (contains? (zip-names jar-file) "msg.txt"))
         (let [p (run-cmd (java) "-cp" (str jar-file ":" (System/getProperty "java.class.path"))
                          "clojure.main" "-m" "example.core" "world")]
           (is (zero? (:exit p)) (str "jar run failed: " (:out p) (:err p)))
           (is (re-find #"hello world" (:out p)))))
      (let [uber-cfg (cfg ws src-root res-root true)
            uber-result (build/build {:args {:builds {"." uber-cfg}}})
            uber-file (str ws "/target/fixture-uber.jar")]
         (is (= uber-file (get-in uber-result [:results 0 :uber])))
         (is (.exists (io/file uber-file)))
         (is (contains? (zip-names uber-file) "msg.txt"))
         (let [p (run-cmd (java) "-jar" uber-file "there")]
           (is (zero? (:exit p)) (str "uber run failed: " (:out p) (:err p)))
           (is (re-find #"hello there" (:out p)))))
         (finally
           (delete-tree ws-dir)))))

(deftest uber-exclude-patterns-that-match-nothing-warn
  ;; tools.build full-matches :exclude patterns (re-matches), so a prefix
  ;; pattern excludes nothing — silently. The build must say so.
  (let [ws-dir (temp-dir)
        ws (str ws-dir)
        src-root (io/file ws-dir "src")
        src-file (io/file src-root "example" "core.clj")]
    (io/make-parents src-file)
    (spit src-file "(ns example.core\n  (:gen-class))\n(defn -main [& args] (println \"x\"))\n")
    (try
      (let [out (with-out-str
                  (build/build {:args {:builds {"." (assoc (cfg ws src-root src-root true)
                                                           :exclude ["^clojure/" "example/core.class"])}}}))]
        (is (re-find #"uberjar :exclude pattern \"\^clojure/\" matched no entry" out)
            (pr-str out))
        (is (not (re-find #"example/core.class\" matched no entry" out))
            (pr-str out)))
      (finally
        (delete-tree ws-dir)))))

(deftest lib-jar-carries-no-dependency-classes
  "Loading the plan's preloads under AOT bindings emits the dependency's
  classes. They are the dependency's own bytecode (already in its jar)
  and must not land in the module's class-dir: a library jar shipping
  other projects' classes breaks publishing."
  (let [ws-dir (temp-dir)
        ws (str ws-dir)
        src-root (io/file ws-dir "src")
        src-file (io/file src-root "example" "core.clj")
        dep-root (io/file ws-dir "dep-src")
        dep-file (io/file dep-root "dep" "util.clj")]
    (io/make-parents dep-file)
    (spit dep-file "(ns dep.util)\n(defn twice [x] (* 2 x))\n")
    (io/make-parents src-file)
    (spit src-file
          "(ns example.core\n  (:gen-class)\n  (:require [dep.util :as u]))\n(defn -main [& args]\n  (println (u/twice 21)))\n")
    (try
      (let [c (assoc (cfg ws src-root src-root false)
                     :classpath (concat [{:id "paths:." :paths [(str src-root) (str dep-root)]}]
                                        (cp-entries)))]
        (build/build {:args {:builds {"." c}}})
        (let [names (zip-names (str ws "/target/fixture.jar"))]
          (is (contains? names "example/core.class"))
          (let [dep-classes (vec (filter #(str/starts-with? % "dep/") names))]
            (is (empty? (filter #(str/starts-with? % "dep/") names))
                (str "jar carries dependency classes: " dep-classes)))))
      (finally
        (delete-tree ws-dir)))))

(deftest built-jars-carry-no-module-sources
  "The jar and the uber carry the module's classes and resources but no
  .clj/.cljc of the module's own: a source next to its AOT __init.class
  in the jar is an RT.load recompile hazard. Prep output under an
  :ensure path survives the drop, as in the clean."
  (let [ws-dir (temp-dir)
        ws (str ws-dir)
        src-root (io/file ws-dir "src")
        src-file (io/file src-root "example" "core.clj")
        cljc-file (io/file src-root "example" "other.cljc")
        res-root (io/file ws-dir "resources")
        res-file (io/file res-root "msg.txt")
        class-dir (io/file ws "target/classes")
        prep-source (io/file class-dir "prepped" "prepped.clj")
        jar-file (str ws "/target/fixture.jar")
        uber-file (str ws "/target/fixture-uber.jar")]
    (io/make-parents src-file)
    (spit src-file "(ns example.core)\n(def x 1)\n")
    (io/make-parents cljc-file)
    (spit cljc-file "(ns example.other)\n(def y 2)\n")
    (io/make-parents res-file)
    (spit res-file "from-resources")
    (io/make-parents prep-source)
    (spit prep-source "(ns example.prepped)\n(def z 3)\n")
    (try
      (let [ensure [(str (io/file class-dir "prepped"))]
            base (assoc (cfg ws src-root res-root false) :prep-ensure ensure)]
        (build/build {:args {:builds {"." base}}})
        (let [names (zip-names jar-file)]
          (is (contains? names "example/core__init.class") names)
          (is (contains? names "example/other__init.class") names)
          (is (contains? names "msg.txt") names)
          (is (contains? names "prepped/prepped.clj")
              "prep output under :ensure survives the drop")
          (is (not (contains? names "example/core.clj")) names)
          (is (not (contains? names "example/other.cljc")) names))
        (build/build {:args {:builds {"."
                                      (assoc (cfg ws src-root res-root true)
                                            :prep-ensure ensure)}}})
        (let [names (zip-names uber-file)]
          (is (contains? names "example/core__init.class") names)
          (is (contains? names "prepped/prepped.clj") names)
          (is (not (contains? names "example/core.clj")) names)
          (is (not (contains? names "example/other.cljc")) names)))
      (finally
        (delete-tree ws-dir)))))

(deftest jar-ships-unreachable-source-payload
  "A .clj below the resources root that the classpath cannot resolve
  (exported clj-kondo hooks) is jar payload: the build must neither fail
  on it in the AOT plan nor drop it from the jar."
  (let [ws-dir (temp-dir)
        ws (str ws-dir)
        src-root (io/file ws-dir "src")
        res-root (io/file ws-dir "resources")
        hook (io/file res-root "clj-kondo.exports" "acme" "hooks.clj")]
    (io/make-parents (io/file src-root "example" "core.clj"))
    (spit (io/file src-root "example" "core.clj")
          "(ns example.core\n  (:gen-class))\n(defn -main [& args]\n  (println \"hi\"))\n")
    (io/make-parents hook)
    (spit hook "(ns acme.hooks)\n")
    (try
      (build/build {:args {:builds {"." (cfg ws src-root res-root false)}}})
      (let [names (zip-names (str ws "/target/fixture.jar"))]
        (is (contains? names "example/core__init.class") names)
        (is (contains? names "clj-kondo.exports/acme/hooks.clj")
            "the unreachable source is the payload and survives")
        (is (not (contains? names "example/core.clj"))
            "the compiled module's source is still dropped"))
      (finally
        (delete-tree ws-dir)))))

(deftest jars-drop-sources-paired-with-their-aot-class
  "A .clj/.cljc next to its AOT __init.class is redundant and an RT.load
  recompile hazard (RT.load loads the source unless the class is strictly
  newer, and sources exploded from dependency jars carry no such
  ordering): the build drops it. A source with no base __init is the
  namespace's only payload and is kept, verbatim, as are .cljs; an __init
  that exists only under META-INF/versions/ is not a base class, so the
  source it alone pairs with stays as the floor's fallback."
  (let [ws-dir (temp-dir)
        ws (str ws-dir)
        src-root (io/file ws-dir "src")
        src-file (io/file src-root "example" "core.clj")
        res-root (io/file ws-dir "resources")
        dep-jar (str ws-dir "/dep.jar")
        class-bytes (byte-array [0xCA 0xFE 0xBA 0xBE 0 0 0 52 0 0 0 0])]
    (io/make-parents src-file)
    (spit src-file
          "(ns example.core\n  (:gen-class))\n(defn -main [& args]\n  (println (str \"hello \" (apply str args))))\n")
    (io/make-parents res-root)
    (with-open [zos (java.util.zip.ZipOutputStream. (io/output-stream dep-jar))]
      (let [write-entry (fn [n bs]
                          (let [^java.util.zip.ZipEntry e (java.util.zip.ZipEntry. n)]
                            (.setTime e 1577836800000)
                            (.putNextEntry zos e)
                            (when bs (.write zos bs))
                            (.closeEntry zos)))]
        (write-entry "dep/" nil)
        (write-entry "dep/paired.clj" (.getBytes "paired-source"))
        (write-entry "dep/paired__init.class" class-bytes)
        (write-entry "dep/dual.cljc" (.getBytes "dual-source"))
        (write-entry "dep/dual__init.class" class-bytes)
        (write-entry "dep/alone.clj" (.getBytes "alone-source"))
        (write-entry "dep/async.cljs" (.getBytes "cljs-source"))
        (write-entry "dep/data_readers.clj" (.getBytes ":readers {}"))
        (write-entry "dep/mrj.clj" (.getBytes "mrj-source"))
        (write-entry "META-INF/versions/11/dep/mrj__init.class" class-bytes)))
    (try
      (let [cfg (-> (cfg ws src-root res-root true)
                    (update :classpath conj {:id dep-jar :paths [dep-jar]}))
            res (build/build {:args {:builds {"." cfg}}})
            uber-file (str ws "/target/fixture-uber.jar")]
        (is (= uber-file (get-in res [:results 0 :uber])))
        (let [names (zip-names uber-file)]
          (is (contains? names "dep/paired__init.class") names)
          (is (not (contains? names "dep/paired.clj")) "paired .clj is dropped")
          (is (contains? names "dep/dual__init.class") names)
          (is (not (contains? names "dep/dual.cljc")) "paired .cljc is dropped")
          (is (contains? names "dep/alone.clj") "unpaired source is the only payload")
          (is (contains? names "dep/async.cljs") names)
          (is (contains? names "dep/data_readers.clj") names)
          (is (contains? names "dep/mrj.clj")
              "a versioned-only __init is not a base class")
          (is (contains? names "META-INF/versions/11/dep/mrj__init.class") names)
          (is (contains? names "example/core__init.class") names)
          (is (contains? names "dep/") "directory entries survive"))
        (is (= "alone-source" (entry-string uber-file "dep/alone.clj"))
            "kept entries are copied verbatim")
        (let [p (run-cmd (java) "-jar" uber-file "there")]
          (is (zero? (:exit p)) (str "uber run failed: " (:out p) (:err p)))
          (is (re-find #"hello there" (:out p)))))
      (finally
        (delete-tree ws-dir)))))

(deftest build-refuses-a-dep-that-ships-source-newer-than-its-class
  "A dep jar whose .clj is strictly newer than its AOT __init.class is
  self-inconsistent (the runtime loads the source, and the jar-level
  strip would silently run the stale class): the uber build fails
  before compiling. Equal timestamps pass — the class is same-build,
  and class-wins is the fix, not the hazard (the
  jars-drop-sources-paired-with-their-aot-class fixture shares one
  timestamp across all entries and builds fine)."
  (let [ws-dir (temp-dir)
        ws (str ws-dir)
        src-root (io/file ws-dir "src")
        src-file (io/file src-root "example" "core.clj")
        res-root (io/file ws-dir "resources")
        dep-jar (str ws-dir "/dep.jar")
        class-bytes (byte-array [0xCA 0xFE 0xBA 0xBE 0 0 0 52 0 0 0 0])]
    (io/make-parents src-file)
    (spit src-file "(ns example.core)\n")
    (io/make-parents res-root)
    (with-open [zos (java.util.zip.ZipOutputStream. (io/output-stream dep-jar))]
      (let [write-entry (fn [n bs ts]
                          (let [^java.util.zip.ZipEntry e (java.util.zip.ZipEntry. n)]
                            (.setTime e ts)
                            (.putNextEntry zos e)
                            (when bs (.write zos bs))
                            (.closeEntry zos)))]
        (write-entry "dep/stale.clj" (.getBytes "stale-source") 1600000002000)
        (write-entry "dep/stale__init.class" class-bytes 1600000000000)))
    (try
      (is (thrown-with-msg? Exception
                            #"(?s)STALE AOT.*dep/stale\.clj"
                            (build/build
                             {:args {:builds {"."
                                               (-> (cfg ws src-root res-root true)
                                                   (update :classpath
                                                           conj {:id dep-jar
                                                                :paths [dep-jar]}))}}}))
          "the build must fail before compiling")
      (finally
        (delete-tree ws-dir)))))

(deftest jars-pin-entry-times-and-sort-when-a-timestamp-is-declared
  "With :rig/timestamp-string set, every entry of the built jar is
  stamped with it and emitted in name-sorted order: same sources, lock
  and build JVM then yield a byte-identical jar."
  (let [ws-dir (temp-dir)
        ws (str ws-dir)
        src-root (io/file ws-dir "src")
        src-file (io/file src-root "example" "core.clj")
        res-root (io/file ws-dir "resources")
        aa-file (io/file res-root "aa.txt")
        zz-file (io/file res-root "zz.txt")
        jar-file (str ws "/target/fixture.jar")
        pinned (.toEpochMilli (java.time.Instant/parse "2026-01-01T00:00:00Z"))]
    (io/make-parents src-file)
    (spit src-file "(ns example.core)\n")
    (io/make-parents aa-file)
    (spit aa-file "aa")
    (spit zz-file "zz")
    (try
      (build/build
       {:args {:builds {"."
                         (assoc (cfg ws src-root res-root false)
                                :timestamp-string "2026-01-01T00:00:00Z")}}})
      (let [entries (zip-entries jar-file)
            names (map first entries)]
        (is (= names (sort names)) (str "entries not name-sorted: " names))
        (is (every? #(= pinned (second %)) entries)
            (str "entries not stamped with the pin: " entries)))
      (finally
        (delete-tree ws-dir)))))

(deftest two-builds-with-a-pinned-timestamp-are-byte-identical
  "Two builds of the same sources and build config, with
  :rig/timestamp-string set, produce byte-identical jars: the pin is
  the switch that makes the jar's sha256 a stable fingerprint of
  sources, lock and build JVM."
  (let [make-src (fn [ws]
                   (let [f (io/file ws "src" "example" "core.clj")]
                     (io/make-parents f)
                     (spit f "(ns example.core\n  (:gen-class))\n(defn -main [& args]\n  (println (str \"hello \" (apply str args))))\n")))
        ws1 (temp-dir)
        ws2 (temp-dir)]
    (make-src ws1)
    (make-src ws2)
    (try
      (doseq [ws [ws1 ws2]]
        (let [src-root (io/file ws "src")
              res-root (io/file ws "resources")
              res-file (io/file res-root "msg.txt")]
          (io/make-parents res-file)
          (spit res-file "from-resources")
          (build/build
           {:args {:builds {"."
                            (assoc (cfg ws src-root res-root false)
                                   :timestamp-string "2026-01-01T00:00:00Z")}}})))
      (is (= (vec (java.nio.file.Files/readAllBytes (.toPath (io/file ws1 "target" "fixture.jar"))))
             (vec (java.nio.file.Files/readAllBytes (.toPath (io/file ws2 "target" "fixture.jar")))))
          "the pinned builds must be byte-identical")
      (finally
        (delete-tree ws1)
        (delete-tree ws2)))))

(deftest build-refuses-an-unusable-timestamp-string
  ":rig/timestamp-string must be an ISO-8601 instant at or after
  1980-01-01: zip timestamps cannot represent it earlier, and the
  writer would clamp silently. Unparseable and too-early values fail
  before the compile."
  (let [ws-dir (temp-dir)
        ws (str ws-dir)
        src-root (io/file ws-dir "src")
        src-file (io/file src-root "example" "core.clj")
        res-root (io/file ws-dir "resources")]
    (io/make-parents src-file)
    (spit src-file "(ns example.core)\n")
    (io/make-parents res-root)
    (try
      (is (thrown-with-msg? Exception
                            #"bad :rig/timestamp-string"
                            (build/build
                             {:args {:builds {"."
                                               (assoc (cfg ws src-root res-root false)
                                                      :timestamp-string "not-a-date")}}}))
          "an unparseable pin must be refused")
      (is (thrown-with-msg? Exception
                            #"before 1980-01-01"
                            (build/build
                             {:args {:builds {"."
                                               (assoc (cfg ws src-root res-root false)
                                                      :timestamp-string "1979-12-31T23:59:59Z")}}}))
          "a pre-1980 pin must be refused")
      (finally
        (delete-tree ws-dir)))))

(deftest no-main-leaves-no-main-class-attribute
  (let [ws-dir (temp-dir)
        ws (str ws-dir)
        src-root (io/file ws-dir "src")
        src-file (io/file src-root "example" "core.clj")]
    (io/make-parents src-file)
    (spit src-file "(ns example.core)\n(def x 1)\n")
    (try
      (let [cfg (dissoc (cfg ws src-root (io/file ws-dir "resources") false) :main)
            jar-file (str ws "/target/fixture.jar")]
        (build/build {:args {:builds {"." cfg}}})
        (is (not (contains? (manifest-attrs jar-file) "Main-Class"))))
      (finally
        (delete-tree ws-dir)))))

(deftest build-classes-only
  "A build config with neither jar? nor uber? compiles into the class-dir
  only (the native-image path): no jar or uber is produced."
  (let [ws-dir (temp-dir)
        ws (str ws-dir)
        src-root (io/file ws-dir "src")
        src-file (io/file src-root "example" "core.clj")]
    (io/make-parents src-file)
    (spit src-file "(ns example.core)\n(defn add [a b] (+ a b))\n")
    (try
      (let [cfg (assoc (cfg ws src-root (io/file ws-dir "resources") false)
                       :jar? false :uber? false)
            res (get-in (build/build {:args {:builds {"." cfg}}}) [:results 0])]
        (is (= (str ws "/target/classes") (:class-dir res)))
        (is (nil? (:jar res)))
        (is (nil? (:uber res)))
        (is (not (.exists (io/file ws "target/fixture.jar"))))
        ;; AOT of a plain ns yields the __init and fn classes, no ns class.
        (is (.exists (io/file ws "target/classes/example/core__init.class")))
        (is (.exists (io/file ws "target/classes/example/core$add.class"))))
      (finally
        (delete-tree ws-dir)))))

(deftest rebuild-recompiles-after-source-changes
  "The class-dir of a previous build must not shadow changed sources (stale
  bytecode): a rebuild picks up the new source content."
  (let [ws-dir (temp-dir)
        ws (str ws-dir)
        src-root (io/file ws-dir "src")
        src-file (io/file src-root "example" "core.clj")
        res-root (io/file ws-dir "resources")
        run-jar (fn []
                  (run-cmd (java) "-cp" (str ws "/target/fixture.jar:" (System/getProperty "java.class.path"))
                           "clojure.main" "-e" "(require (quote example.core)) (example.core/greet)"))]
    (io/make-parents src-file)
    (spit src-file "(ns example.core)\n(defn greet []\n  (println \"one\"))\n")
    (try
      (build/build {:args {:builds {"." (cfg ws src-root res-root false)}}})
      (is (re-find #"one" (:out (run-jar))))
      (spit src-file "(ns example.core)\n(defn greet []\n  (println \"two\"))\n")
      (build/build {:args {:builds {"." (cfg ws src-root res-root false)}}})
      (is (re-find #"two" (:out (run-jar))) "stale class-dir shadowed the changed source")
      (finally
        (delete-tree ws-dir)))))

(deftest ns-compile-compiles-namespaces-outside-the-artifact-dirs
  (let [ws-dir (temp-dir)
        ws (str ws-dir)
        src-root (io/file ws-dir "src")
        dep-src (io/file ws-dir "dep-src")
        res-root (io/file ws-dir "resources")
        src-file (io/file src-root "example" "core.clj")
        entry-file (io/file dep-src "entry" "main.clj")]
    (io/make-parents src-file)
    (spit src-file "(ns example.core)\n(def x 1)\n")
    (io/make-parents entry-file)
    (spit entry-file "(ns entry.main)\n(def y 2)\n")
    (try
      (let [base (cfg ws src-root res-root false)
            cfg (assoc base :classpath
                       (conj (:classpath base) {:id "paths:dep" :paths [(str dep-src)]})
                       :ns-compile ["entry.main"])]
        (build/build {:args {:builds {"." cfg}}})
        (is (contains? (zip-names (str ws "/target/fixture.jar")) "entry/main__init.class")))
      (finally
        (delete-tree ws-dir)))))

(deftest build-bakes-launch-descriptor
  "A :launch config lands in the jar and the uber as META-INF/rig/launch.json;
  a build without one leaves no entry (a rebuild must not leave a stale one)."
  (let [ws-dir (temp-dir)
        ws (str ws-dir)
        src-root (io/file ws-dir "src")
        src-file (io/file src-root "example" "core.clj")
        res-root (io/file ws-dir "resources")
        jar-file (str ws "/target/fixture.jar")
        uber-file (str ws "/target/fixture-uber.jar")
        launch {"version" 1 "rig" "test" "main" "example.core"
                "jvm-opts" ["-Xmx2g"] "java" 21 "uber" false}]
    (io/make-parents src-file)
    (spit src-file "(ns example.core\n  (:gen-class))\n(defn -main [& args]\n  (println (str \"hello \" (apply str args))))\n")
    (try
      (build/build {:args {:builds {"." (assoc (cfg ws src-root res-root false) :launch launch)}}})
      (is (= launch (launch-entry jar-file)))
      (build/build {:args {:builds {"." (assoc (cfg ws src-root res-root true)
                                              :launch (assoc launch "uber" true))}}})
      (is (= (assoc launch "uber" true) (launch-entry uber-file)))
      (build/build {:args {:builds {"." (cfg ws src-root res-root false)}}})
      (is (nil? (launch-entry jar-file)))
      (finally
        (delete-tree ws-dir)))))

(deftest javac-receives-javac-opts
  "b/javac receives :javac-opts from the build config: a valid opt compiles,
  an unknown one fails the build (proving the opts reach the javac line)."
  (let [ws-dir (temp-dir)
        ws (str ws-dir)
        src-root (io/file ws-dir "src")
        java-root (io/file ws-dir "java")
        java-file (io/file java-root "example" "Greeter.java")
        javac (javax.tools.ToolProvider/getSystemJavaCompiler)]
    (when javac
      (io/make-parents (io/file src-root "example" "core.clj"))
      (spit (io/file src-root "example" "core.clj") "(ns example.core)\n(def x 1)\n")
      (io/make-parents java-file)
      (spit java-file "package example;\n\npublic class Greeter {\n  public static String greet(String n) {\n    return \"hi \" + n;\n  }\n}\n")
      (let [cfg (-> (cfg ws src-root (io/file ws-dir "resources") false)
                    (dissoc :main)
                    (assoc :java-src-dirs [(str java-root)]
                           :javac-opts ["-encoding" "UTF-8"]))]
        (try
          (let [result (build/build {:args {:builds {"." cfg}}})
                jar-file (str ws "/target/fixture.jar")]
            (is (= jar-file (get-in result [:results 0 :jar])))
            (is (contains? (zip-names jar-file) "example/Greeter.class"))
          (is (thrown? Exception
                       (build/build {:args {:builds {"."
                                                     (assoc cfg :javac-opts ["--definitely-not-a-javac-flag"])}}}))
               "bogus javac opt must reach javac and fail the build"))
          (finally
            (delete-tree ws-dir)))))))

(deftest aot-receives-compile-jvm-opts
  "The AOT fork receives :compile-jvm-opts from the build config as JVM
  flags: a valid flag compiles, an unknown one fails the build (proving
  the flags reach the fork's java line)."
  (let [ws-dir (temp-dir)
        ws (str ws-dir)
        src-root (io/file ws-dir "src")
        src-file (io/file src-root "example" "core.clj")]
    (io/make-parents src-file)
    (spit src-file "(ns example.core)\n(def x 1)\n")
    (try
      (let [cfg (assoc (cfg ws src-root (io/file ws-dir "resources") false)
                       :compile-jvm-opts ["-Drig.aot.opt=1"])]
        (build/build {:args {:builds {"." cfg}}})
        (is (.exists (io/file ws "target/classes/example/core__init.class")))
        (is (thrown? Exception
                     (build/build {:args {:builds {"."
                                                   (assoc cfg :compile-jvm-opts
                                                          ["--definitely-not-a-jvm-flag"])}}}))
            "bogus jvm opt must reach the fork and fail the build"))
      (finally
        (delete-tree ws-dir)))))

(deftest aot-fork-runs-on-the-kernels-jvm
  "The AOT fork is the kernel's own JVM: rig launches the kernel with the
  workspace's picked JDK, so the compile must not resolve its java from the
  environment ($JAVA_CMD, PATH, $JAVA_HOME) — that is the host's JVM, and a
  green build would say nothing about the pinned one. The environment lookup
  is stubbed to a host JVM and the fork's command line captured, so the
  assertion does not depend on how this machine happens to link its java."
  (let [ws-dir (temp-dir)
        ws (str ws-dir)
        src-root (io/file ws-dir "src")
        seen (atom nil)]
    (io/make-parents (io/file src-root "example" "core.clj"))
    (spit (io/file src-root "example" "core.clj") "(ns example.core)\n(def x 1)\n")
    (try
      (with-redefs [process/java-executable (fn [] "/host/jvm/bin/java")
                    process/process (fn [args]
                                      (reset! seen (:command-args args))
                                      {:exit 0})]
        (build/build {:args {:builds {"." (assoc (cfg ws src-root (io/file ws-dir "resources") false)
                                                 :compile-jvm-opts ["-Drig.aot.opt=1"])}}}))
      (is (= (.getPath (io/file (System/getProperty "java.home") "bin" "java"))
             (first @seen))
          (str "the AOT fork runs on: " (pr-str (first @seen))))
      (is (= "-Drig.aot.opt=1" (second @seen))
          (str "the fork's first flag: " (pr-str (second @seen))))
      (finally
        (delete-tree ws-dir)))))

(deftest clojure-compiles-against-own-java-classes
  "A namespace referencing the module's own Java class must compile: javac
  output lands in the class-dir, which leads the compile classpath, so the
  Java sources have to be compiled before the Clojure ones."
  (let [ws-dir (temp-dir)
        ws (str ws-dir)
        src-root (io/file ws-dir "src")
        java-root (io/file ws-dir "java")
        jar-file (str ws "/target/fixture.jar")
        javac (javax.tools.ToolProvider/getSystemJavaCompiler)]
    (when javac
      (io/make-parents (io/file src-root "example" "core.clj"))
      (spit (io/file src-root "example" "core.clj")
            "(ns example.core)\n(defn hi [] (example.Greeter/greet))\n")
      (io/make-parents (io/file java-root "example" "Greeter.java"))
      (spit (io/file java-root "example" "Greeter.java")
            "package example;\n\npublic class Greeter {\n  public static String greet() {\n    return \"hi\";\n  }\n}\n")
      (let [cfg (-> (cfg ws src-root (io/file ws-dir "resources") false)
                    (dissoc :main)
                    (assoc :java-src-dirs [(str java-root)]))]
        (try
          (let [result (build/build {:args {:builds {"." cfg}}})]
            (is (= jar-file (get-in result [:results 0 :jar])))
            (is (contains? (zip-names jar-file) "example/Greeter.class"))
            (is (contains? (zip-names jar-file) "example/core__init.class"))
            (let [p (run-cmd (java) "-cp" (str jar-file ":" (System/getProperty "java.class.path"))
                             "clojure.main" "-e" "(require (quote example.core)) (println (example.core/hi))")]
              (is (zero? (:exit p)) (str "jar run failed: " (:out p) (:err p)))
              (is (re-find #"hi" (:out p)))))
          (finally
            (delete-tree ws-dir)))))))

(deftest prep-ensure-content-survives-the-clean
  "A build must not destroy the module's prep output: when the prep :ensure
  covers the class-dir, its content (the build's input) survives the clean
  and lands in the jar. (Here the prep javacs the module's java sources
  into target/classes, which is also the class-dir, and the build
  declares no java-src-dirs of its own.)"
  (let [ws-dir (temp-dir)
        ws (str ws-dir)
        src-root (io/file ws-dir "src")
        res-root (io/file ws-dir "resources")
        class-dir (io/file ws "target/classes")
        prep-class (io/file class-dir "com" "example" "PrepGenerated.class")
        jar-file (str ws "/target/fixture.jar")]
    (io/make-parents (io/file src-root "com" "example" "Thing.java"))
    (spit (io/file src-root "com" "example" "Thing.java")
          "package com.example;\n\npublic class Thing {}\n")
    (io/make-parents prep-class)
    (spit prep-class "prep-output")
    (spit (io/file class-dir "stale.txt") "prep-note")
    (try
      (let [cfg (-> (cfg ws src-root res-root false)
                    (dissoc :main)
                    (assoc :java-src-dirs []
                           :prep-ensure [(str class-dir)]))]
        (build/build {:args {:builds {"." cfg}}})
        (is (.exists prep-class) "prep output must survive the build's clean")
        (is (.exists (io/file class-dir "stale.txt")) "prep output must survive the build's clean")
        (let [names (zip-names jar-file)]
          (is (contains? names "com/example/PrepGenerated.class") names)
          (is (contains? names "stale.txt") names)
          (is (contains? names "com/example/Thing.java") names)))
      (finally
        (delete-tree ws-dir)))))

(deftest clean-wipes-non-prep-content
  "The clean still wipes the build's own stale output: class-dir content
  that is not under a prep :ensure path is deleted, while ensure content
  survives."
  (let [ws-dir (temp-dir)
        ws (str ws-dir)
        src-root (io/file ws-dir "src")
        src-file (io/file src-root "example" "core.clj")
        res-root (io/file ws-dir "resources")
        class-dir (io/file ws "target/classes")
        gen-file (io/file class-dir "generated" "gen.bin")
        stale-class (io/file class-dir "example" "gone__init.class")
        jar-file (str ws "/target/fixture.jar")]
    (io/make-parents src-file)
    (spit src-file "(ns example.core)\n(defn add [a b] (+ a b))\n")
    (io/make-parents gen-file)
    (spit gen-file "generated")
    (io/make-parents stale-class)
    (spit stale-class "stale")
    (try
      (let [cfg (-> (cfg ws src-root res-root false)
                    (assoc :prep-ensure [(str (io/file class-dir "generated"))]))]
        (build/build {:args {:builds {"." cfg}}})
        (is (.exists gen-file) "ensure content must survive the clean")
        (is (not (.exists stale-class)) "stale non-prep content must be wiped")
        (let [names (zip-names jar-file)]
          (is (contains? names "generated/gen.bin") names)
          (is (not (contains? names "example/gone__init.class")) names)
          (is (contains? names "example/core__init.class") names)))
      (finally
        (delete-tree ws-dir)))))

(deftest build-fails-when-a-dependency-class-exceeds-the-floor
  "The floor (cfg :floor, the pinned JVM's feature version) gates every
  jar the build produces: a classpath dependency carrying a class above
  it fails the uber build (the class lands in the uber); the same build
  passes at a floor at or above the class, and without a floor (no pin,
  no scan)."
  (let [ws-dir (temp-dir)
        ws (str ws-dir)
        src-root (io/file ws-dir "src")
        src-file (io/file src-root "example" "core.clj")
        res-root (io/file ws-dir "resources")
        dep-jar (str ws-dir "/dep.jar")
        class-bytes (byte-array [0xCA 0xFE 0xBA 0xBE 0 0 0 65 0 0 0 0])]
    (io/make-parents src-file)
    (spit src-file "(ns example.core)\n(def x 1)\n")
    (with-open [zos (java.util.zip.ZipOutputStream. (io/output-stream dep-jar))]
      (let [^java.util.zip.ZipEntry e (java.util.zip.ZipEntry. "dep/High.class")]
        (.setTime e 1577836800000)
        (.putNextEntry zos e)
        (.write zos class-bytes)
        (.closeEntry zos)))
    (let [cfg (fn []
                (-> (cfg ws src-root res-root true)
                    (update :classpath
                            conj {:id dep-jar :paths [dep-jar]})))]
      (try
        (is (thrown-with-msg? Exception
                              #"FLOOR VIOLATION.*dep/High\.class \(v65\)"
                              (build/build {:args {:builds {"."
                                                            (assoc (cfg) :floor 8)}}})))
        (is (some? (build/build {:args {:builds {"."
                                                 (assoc (cfg) :floor 21)}}}))
            "a class at the floor (v65 = Java 21) must pass")
        (is (some? (build/build {:args {:builds {"." (cfg)}}}))
            "no floor in the config: no scan")
        (finally
          (delete-tree ws-dir))))))

(deftest build-embeds-maven-coordinates
  (let [ws-dir (temp-dir)
        ws (str ws-dir)
        src-root (io/file ws-dir "src")
        src-file (io/file src-root "example" "core.clj")
        res-root (io/file ws-dir "resources")
        payload (io/file res-root "META-INF" "maven" "other" "lib" "pom.xml")
        stale (io/file res-root "META-INF" "maven" "acme" "widget" "pom.xml")]
    (io/make-parents src-file)
    (spit src-file "(ns example.core)\n")
    (io/make-parents payload)
    (spit payload "dependency-payload")
    (io/make-parents stale)
    (spit stale "stale-shipped-pom")
    (spit (io/file ws-dir "deps.edn")
          "{:deps {org/dep {:mvn/version \"1.x\"}} :rig/lib acme/widget :rig/version \"9.9.9\"}")
    (let [request (fn [cfg]
                    {:workspace ws
                     :lock {:artifacts [{:kind "mvn" :group "org" :name "dep" :version "2.0.0"}]}
                     :args {:builds {"." cfg}}})]
      (try
        (doseq [uber? [false true]]
          (let [jar-file (str ws "/target/" (if uber? "fixture-uber.jar" "fixture.jar"))
                pom-path "META-INF/maven/acme/widget/pom.xml"
                props-path "META-INF/maven/acme/widget/pom.properties"
                _ (build/build (request (cfg ws src-root res-root uber?)))
                pom (entry-string jar-file pom-path)]
            (is (contains? (zip-names jar-file) props-path) (str "uber?=" uber?))
            (is (= "version=9.9.9\ngroupId=acme\nartifactId=widget\n"
                   (entry-string jar-file props-path))
                (str "uber?=" uber?))
            (is (str/includes? pom "<groupId>acme</groupId>") (str "uber?=" uber?))
            (is (str/includes? pom "<version>9.9.9</version>") (str "uber?=" uber?))
            (is (str/includes? pom "<version>2.0.0</version>")
                "dependency takes the lock pin, not the declared requirement")
            (is (not (str/includes? pom "stale-shipped"))
                "the canonical path is replaced, not shipped alongside")
            (is (= "dependency-payload" (entry-string jar-file "META-INF/maven/other/lib/pom.xml"))
                "another coordinate's payload passes through")))
        ;; A module that declares no coordinates ships no coordinates — and the
        ;; previous build's embedding leaves no residue (the zip is rebuilt).
        (spit (io/file ws-dir "deps.edn") "{:deps {}}")
        (build/build (request (cfg ws src-root res-root false)))
        (is (not (contains? (zip-names (str ws "/target/fixture.jar"))
                            "META-INF/maven/acme/widget/pom.properties"))
            "the previous embedding left no residue")
        (finally
          (delete-tree ws-dir))))))
