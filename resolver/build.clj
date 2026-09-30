(ns build
  (:require [clojure.java.io :as io]
            [clojure.string :as str]
            [clojure.tools.build.api :as b]
            [rig.resolver.floor :as floor])
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
;; The rig.runner jar: rig.runner's class files only. Hot commands launch
;; the runner on the project's locked classpath, so the jar must hold
;; nothing else — no bundled dependencies (they would shadow the
;; project's), no sources. It is an install-time artifact, shipped next
;; to the kernel jar, never extracted at runtime.
(def runner-file (str target "/rig-runner-" version ".jar"))
;; The kernel floor is Java 8 (Clojure 1.12's own floor; every dependency
;; in the jar is Java 8 or lower). On JDK 9+ hosts --release pins the
;; compiled classes to the floor: without it Main.class targets the build
;; host's JVM and a workspace pinned below the host fails to load the
;; kernel (UnsupportedClassVersionError). On a Java 8 host javac already
;; targets Java 8 and --release does not exist, so no flag is needed.
(def javac-opts
  (if (.startsWith (System/getProperty "java.specification.version") "1.")
    []
    ["--release" "8"]))

;; The jar floor is Java 8: class file major version 52. A workspace may
;; pin any JVM at or above Clojure 1.12's floor, below the build host, so
;; no class in either jar may exceed the floor. The --release pin above
;; covers the javac shim; the gate below covers the rest of the jar —
;; v0.1.0 shipped a v65 Main.class (javac without --release on a JDK 21
;; host) and failed to load on every pin below 21. Both jars are scanned
;; after assembly (rig.resolver/floor); a violation fails the build. MRJ
;; entries under META-INF/versions/ are skipped: the JVM only loads them
;; on that version or newer, so none of them can load on the floor.

(defn- assert-floor
  "Fail the build when the jar at path carries a class above the floor."
  [jar]
  (let [{:keys [classes violations]} (floor/floor-report jar 8)]
    (if (empty? violations)
      (println (str "floor: " (.getName (io/file jar)) " — " classes
                    " classes ≤ v52 (Java 8)"))
      (do (println (str "FLOOR VIOLATION: " (.getName (io/file jar)) " — "
                        (count violations) " class(es) above v52 (Java 8): "
                        (str/join ", " (map (fn [[n m]] (str n " (v" m ")"))
                                            violations))))
          (System/exit 1)))))

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

(defn- runner-jar
  "Zip rig.runner's class files (rig/runner*.class in class-dir) into a
  byte-identical jar: fixed entry timestamps and sorted entry names, like
  normalize-zip. Without the .clj sources there is no AOT timestamp
  ordering to preserve."
  []
  (let [tmp (str runner-file ".tmp")
        out (ZipOutputStream. (io/output-stream tmp))]
    (try
      (doseq [f (sort-by (memfn getName)
                         (file-seq (io/file class-dir "rig")))]
        (when (and (.isFile f)
                   (.startsWith (.getName f) "runner")
                   (.endsWith (.getName f) ".class"))
          (let [^ZipEntry e (ZipEntry. (str "rig/" (.getName f)))]
            (.setTime e 1577836802000)
            (.putNextEntry out e)
            (io/copy (io/input-stream f) out)
            (.closeEntry out))))
      (finally (.close out)))
    (java.nio.file.Files/move (.toPath (io/file tmp))
                             (.toPath (io/file runner-file))
                             (into-array java.nio.file.CopyOption
                                        [StandardCopyOption/ATOMIC_MOVE]))
    (assert-floor runner-file)))

(defn uber [_]
  (b/delete {:path target})
  (let [basis (b/create-basis {})]
    (b/compile-clj {:basis basis :class-dir class-dir :src-dirs ["src"]})
    (b/javac {:basis basis :class-dir class-dir :src-dirs ["java"]
              :javac-opts javac-opts})
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
                                          [StandardCopyOption/ATOMIC_MOVE]))
    (assert-floor uber-file)
    (runner-jar)))
