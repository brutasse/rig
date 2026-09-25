(ns build
  (:require [clojure.java.io :as io]
            [clojure.tools.build.api :as b])
  (:import (java.nio.file StandardCopyOption)
           (java.util.zip ZipEntry ZipFile ZipOutputStream)))

(def lib "io.github.brutasse/rig-resolver")
;; V is the release tag form (vX.Y.Z); local dev builds default to v0.1.0
;; (the version local tooling and tests expect).
(def version (or (System/getenv "RIG_RESOLVER_VERSION") "v0.1.0"))
;; The commit this build is made from; 'make pin' stamps the same value
;; into the kernel pin, so the pin and the jar always agree. ProcessBuilder
;; (not clojure.java.shell): the local CLI classpath may lack the ns.
(def git-sha
  (let [p (.start (ProcessBuilder. (into-array String ["git" "rev-parse" "HEAD"])))
        in (.getInputStream p)
        _ (.waitFor p)
        s (.trim (slurp in))]
    (if (re-matches #"^[0-9a-f]{7,40}$" s) s (apply str (repeat 40 "0")))))
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
    ;; build-info.edn lands in the uber jar via class-dir (tools.build
    ;; 0.10.5's uber has no :resource-dirs): the kernel's identity, read
    ;; at runtime by rig.resolver.resolve/resolver-id.
    (spit (io/file class-dir "build-info.edn")
          (pr-str {:version version :git-sha git-sha}))
    (b/uber {:basis basis :class-dir class-dir :uber-file uber-file :main 'Main})
    (normalize-zip uber-file (str uber-file ".tmp"))
    (java.nio.file.Files/move (.toPath (io/file (str uber-file ".tmp")))
                              (.toPath (io/file uber-file))
                               (into-array java.nio.file.CopyOption
                                          [StandardCopyOption/ATOMIC_MOVE]))))
