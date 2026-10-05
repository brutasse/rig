(ns rig.resolver.floor-test
  "The bytecode floor gate (rig.resolver/floor): a built jar must stay
  loadable on the floor's JVM — no class file above 44 + floor, and no
  multi-release entry at or below the floor above it either."
  (:require [clojure.java.io :as io]
            [clojure.test :refer :all]
            [rig.resolver.floor :as floor]
            [rig.resolver.test-util :refer [delete-tree temp-dir]])
  (:import [java.util.zip ZipEntry ZipOutputStream]))

(defn- class-bytes
  "A minimal .class entry: magic + version header, the rest padding. The
  scanner reads only the header."
  [major]
  (byte-array [0xCA 0xFE 0xBA 0xBE 0 0 0 major 0 0 0 0]))

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
  (is (= 51 (floor/class-file-major (class-bytes 51))))
  (is (= 52 (floor/class-file-major (class-bytes 52))))
  (is (= 65 (floor/class-file-major (class-bytes 65)))))

(deftest floor-report-flags-only-classes-above-the-floor
  "MRJ entries above the floor never load there and are skipped;
  non-class entries are ignored."
  (let [dir (temp-dir)]
    (try
      (let [jar (fixture-jar dir {"bad/High.class" 65
                                  "ok/BelowFloor.class" 51
                                  "ok/AtFloor.class" 52
                                  "meta/build-info.edn" 65
                                  "dir/" 65
                                  "META-INF/versions/11/mrj/Mrj.class" 65})]
        (is (= {:classes 3 :violations [["bad/High.class" 65]]}
               (floor/floor-report jar 8))))
      (finally
        (delete-tree dir)))))

(deftest floor-report-scans-mrj-at-or-below-the-floor
  "An MRJ entry at a version ≤ the floor loads on it and is scanned
  against the floor's class file major; a non-numeric version segment is
  not multi-release and is scanned like any other entry."
  (let [dir (temp-dir)]
    (try
      (let [jar (fixture-jar dir {"META-INF/versions/11/Skipped.class" 65
                                  "META-INF/versions/9/AtFloor.class" 53
                                  "META-INF/versions/9/High.class" 54
                                  "META-INF/versions/x/Weird.class" 65})]
        (is (= {:classes 3
                :violations [["META-INF/versions/9/High.class" 54]
                             ["META-INF/versions/x/Weird.class" 65]]}
               (floor/floor-report jar 9))))
      (finally
        (delete-tree dir)))))

(deftest floor-report-on-a-floor-clean-jar
  (let [dir (temp-dir)]
    (try
      (let [jar (fixture-jar dir {"a/One.class" 52
                                  "b/Two.class" 52})]
        (is (= {:classes 2 :violations []} (floor/floor-report jar 8))))
      (finally
        (delete-tree dir)))))

(deftest assert-floor-throws-on-a-violation
  (let [dir (temp-dir)]
    (try
      (let [jar (fixture-jar dir {"bad/High.class" 65})]
        (is (thrown-with-msg? Exception
                              #"FLOOR VIOLATION.*bad/High\.class \(v65\)"
                              (floor/assert-floor jar 8)))
        (is (nil? (floor/assert-floor (fixture-jar (temp-dir)
                                                   {"ok/AtFloor.class" 52})
                                      8))))
      (finally
        (delete-tree dir)))))
