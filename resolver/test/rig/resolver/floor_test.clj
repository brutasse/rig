(ns rig.resolver.floor-test
  "The toolchain build's bytecode floor gate (build.clj): both release
  jars must stay at the Java 8 class-file major (v52) so workspaces
  pinned below the build host can load them."
  (:require [clojure.java.io :as io]
            [clojure.test :refer :all]
            [build :as build-script])
  (:import [java.util.zip ZipEntry ZipOutputStream]))

(defn- class-bytes
  "A minimal .class entry: magic + version header, the rest padding. The
  scanner reads only the header."
  [major]
  (byte-array [0xCA 0xFE 0xBA 0xBE 0 0 0 major 0 0 0 0]))

(defn- temp-dir
  []
  (doto (java.io.File. (str (java.nio.file.Files/createTempDirectory "rig-floor-test"
                                                                     (into-array java.nio.file.attribute.FileAttribute []))))
    (.deleteOnExit)))

(defn- delete-tree
  "Recursively delete dir (children first). Best effort."
  [dir]
  (when-let [fs (file-seq dir)]
    (dorun (map (fn [f]
                  (when-not (.delete f)
                    (println (str "rig floor-test: could not delete " f))))
                (reverse fs)))))

(defn- fixture-jar
  "A jar at dir/fixture.jar with one entry per {name major} pair."
  [dir entries]
  (let [jar (str dir "/fixture.jar")]
    (with-open [zos (ZipOutputStream. (io/output-stream jar))]
      (doseq [[name major] (sort entries)]
        (let [^ZipEntry e (ZipEntry. name)]
          (.setTime e 1577836800000)
          (.putNextEntry zos e)
          (.write zos (class-bytes major))
          (.closeEntry zos))))
    jar))

(deftest class-file-major-reads-the-header
  (is (= 51 (build-script/class-file-major (class-bytes 51))))
  (is (= 52 (build-script/class-file-major (class-bytes 52))))
  (is (= 65 (build-script/class-file-major (class-bytes 65)))))

(deftest floor-report-flags-only-classes-above-the-floor
  "MRJ entries under META-INF/versions/ are skipped (only loaded on that
  version or newer, never on the floor); non-class entries are ignored."
  (let [dir (temp-dir)]
    (try
      (let [jar (fixture-jar dir {"bad/High.class" 65
                                  "ok/BelowFloor.class" 51
                                  "ok/AtFloor.class" 52
                                  "meta/build-info.edn" 65
                                  "dir/" 65
                                  "META-INF/versions/11/mrj/Mrj.class" 65})]
        (is (= {:classes 3 :violations [["bad/High.class" 65]]}
               (build-script/floor-report jar))))
      (finally
        (delete-tree dir)))))

(deftest floor-report-on-a-floor-clean-jar
  (let [dir (temp-dir)]
    (try
      (let [jar (fixture-jar dir {"a/One.class" 52
                                  "b/Two.class" 52})]
        (is (= {:classes 2 :violations []} (build-script/floor-report jar))))
      (finally
        (delete-tree dir)))))
