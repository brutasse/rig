(ns build
  (:require [clojure.java.io :as io]
            [clojure.tools.build.api :as b])
  (:import (java.nio.file StandardCopyOption)
           (java.util.zip ZipEntry ZipFile ZipOutputStream)))

(def lib "io.github.brutasse/rig-resolver")
;; Release builds set RIG_RESOLVER_VERSION to the tag version; local dev
;; builds default to 0.1.0 (the version local tooling and tests expect).
(def version (or (System/getenv "RIG_RESOLVER_VERSION") "0.1.0"))
(def target "target")
(def class-dir (str target "/classes"))
(def uber-file (str target "/rig-resolver-" version ".jar"))

(defn- normalize-zip
  "Re-zip src to dst with fixed entry timestamps so the jar is
  byte-identical for identical sources. Sources (.clj/.cljc) get an
  older timestamp than everything else: RT.load only uses an AOT
  __init.class when it is strictly newer than its source, so equal
  timestamps would make the runtime recompile namespaces from the
  bundled sources (two class identities -> ClassCastException)."
  [src dst]
  (let [in (ZipFile. src)
        out (ZipOutputStream. (io/output-stream dst))
        it (.entries in)
        src-ts 1577836800000
        other-ts 1577836802000] ; 2020-01-01; +2s (zip resolution) for classes/resources
    (try
      (while (.hasMoreElements it)
        (let [^ZipEntry e (.nextElement it)
              n (ZipEntry. (.getName e))]
          (.setTime n (if (or (.endsWith (.getName e) ".clj")
                              (.endsWith (.getName e) ".cljc"))
                       src-ts
                       other-ts))
          (.putNextEntry out n)
          (io/copy (.getInputStream in e) out)
          (.closeEntry out)))
      (finally
        (.close out)
        (.close in)))))

(defn uber [_]
  (b/delete {:path target})
  (let [basis (b/create-basis {})]
    (b/compile-clj {:basis basis :class-dir class-dir :src-dirs ["src"]})
    (b/javac {:basis basis :class-dir class-dir :src-dirs ["java"]})
    (b/uber {:basis basis :class-dir class-dir :uber-file uber-file :main 'Main})
    (normalize-zip uber-file (str uber-file ".tmp"))
    (java.nio.file.Files/move (.toPath (io/file (str uber-file ".tmp")))
                              (.toPath (io/file uber-file))
                               (into-array java.nio.file.CopyOption
                                          [StandardCopyOption/ATOMIC_MOVE]))))
