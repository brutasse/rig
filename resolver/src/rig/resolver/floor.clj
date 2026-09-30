(ns rig.resolver.floor
  "The bytecode floor gate: a built jar must stay loadable on the floor's
  JVM. Every .class entry is scanned for its class-file major version; a
  class above the floor's major (44 + floor) is a violation. Multi-release
  entries under META-INF/versions/ only load on that JVM version or
  newer, so an entry at a version above the floor is skipped (it can
  never load there) while an entry at or below the floor is scanned like
  any other class."
  (:require [clojure.java.io :as io]
            [clojure.string :as str])
  (:import (java.util.zip ZipEntry ZipFile)))

(defn class-file-major
  "The class-file major version of a .class entry (the big-endian
  unsigned short at byte offset 6)."
  [^bytes b]
  (+ (* (bit-and 0xff (nth b 6)) 256)
     (bit-and 0xff (nth b 7))))

(defn- entry-bytes
  "All bytes of a zip entry stream."
  [^java.io.InputStream is]
  (let [baos (java.io.ByteArrayOutputStream.)]
    (io/copy is baos)
    (.toByteArray baos)))

(defn- mrj-version
  "The multi-release feature version of an entry under
  META-INF/versions/<V>/ (V as an int), nil for a non-MRJ entry. A
  non-numeric version segment is not multi-release and is scanned like
  any other entry."
  [name]
  (let [prefix "META-INF/versions/"
        rest (when (str/starts-with? name prefix) (subs name (count prefix)))
        seg (when rest
              (let [i (.indexOf rest "/")]
                (subs rest 0 (if (neg? i) (count rest) i))))]
    (when (and seg (re-matches #"[0-9]+" seg))
      (Integer/parseInt seg))))

(defn floor-report
  "Scan every .class entry of the jar at path against the floor, a JVM
  feature version (8, 11, 21…). Returns {:classes n :violations [[entry
  major] …]} — classes whose major exceeds the floor's (44 + floor). MRJ
  entries above the floor never load on it and are skipped."
  [jar floor]
  (let [major-floor (+ 44 floor)
        zf (ZipFile. jar)]
    (try
      (reduce (fn [acc ^ZipEntry e]
                (let [name (.getName e)
                      v (mrj-version name)]
                  (if (or (.isDirectory e)
                          (not (.endsWith name ".class"))
                          (and v (> v floor)))
                    acc
                    (let [major (with-open [is (.getInputStream zf e)]
                                (class-file-major (entry-bytes is)))]
                      (-> acc (update :classes inc)
                          (update :violations
                                  (fn [vs] (if (> major major-floor)
                                             (conj vs [name major])
                                             vs))))))))
                {:classes 0 :violations []}
                (iterator-seq (.entries zf)))
      (finally (.close zf)))))

(defn assert-floor
  "Scan the jar at path against the floor (a JVM feature version) and fail
  the build when any class exceeds it: the jar must load on the floor's
  JVM. Prints one line on a clean jar (the gate ran) and returns nil;
  throws on a violation."
  [jar floor]
  (let [{:keys [classes violations]} (floor-report jar floor)]
    (if (seq violations)
      (throw (ex-info
              (str "FLOOR VIOLATION: " (.getName (io/file jar)) " — "
                   (count violations) " class(es) above the bytecode floor "
                   "(Java " floor ", class file v" (+ 44 floor) "): "
                   (str/join ", " (map (fn [[n m]] (str n " (v" m ")"))
                                       violations)))
              {:jar (str jar) :floor floor :violations violations}))
      (do (println (str "floor: " (.getName (io/file jar)) " — " classes
                        " classes ≤ Java " floor
                        " (class file v" (+ 44 floor) ")"))
          nil))))
