(ns rig.resolver.build-test
  (:require [cheshire.core :as json]
            [clojure.java.io :as io]
            [clojure.string :as str]
            [clojure.test :refer :all]
            [rig.resolver.build :as build]))

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

(defn- temp-dir
  []
  (doto (java.io.File. (str (java.nio.file.Files/createTempDirectory "rig-build-test"
                                                                     (into-array java.nio.file.attribute.FileAttribute []))))
    (.deleteOnExit)))

(defn- delete-tree
  "Recursively delete dir (children first). Best effort."
  [dir]
  (when-let [fs (file-seq dir)]
    (dorun (map (fn [f]
                  (when-not (.delete f)
                    (.println (System/err) (str "rig build-test: could not delete " f))))
                (reverse fs)))))

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
     :src-dirs [(str src-root) (str res-root)]
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

(deftest ns-compile-compiles-namespaces-outside-the-src-dirs
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
