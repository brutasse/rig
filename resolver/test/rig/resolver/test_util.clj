(ns rig.resolver.test-util
  "Shared fixtures for the resolver tests: temp dirs and workspaces, a
  maven-repo materialiser, and a file-serving HTTP server. Consolidated
  from byte-identical copies that lived in the individual test
  namespaces."
  (:require [clojure.java.io :as io]
            [clojure.string :as str])
  (:import [com.sun.net.httpserver HttpServer]))

(defn sha1-hex
  "The SHA-1 hex digest of a file (checksums in repo-metadata fixtures)."
  [f]
  (let [md (java.security.MessageDigest/getInstance "SHA-1")
        buf (byte-array 65536)]
    (with-open [in (io/input-stream f)]
      (loop [n (.read in buf)]
        (when (pos? n)
          (.update md buf 0 n)
          (recur (.read in buf)))))
    (apply str (for [b (.digest md)] (format "%02x" (bit-and b 0xff))))))

(defn delete-tree
  "Recursively delete dir (a string or File; children first). Best effort:
  warns on a file it could not remove, silent when dir does not exist."
  [dir]
  (let [d (io/file dir)]
    (when (.exists d)
      (doseq [f (reverse (file-seq d))]
        (when-not (.delete f)
          (println (str "could not delete " f)))))))

(defn temp-dir
  "A fresh empty temp directory, deleted on JVM exit. Returns a File."
  []
  (doto (io/file (str (java.nio.file.Files/createTempDirectory
                       "rig-test"
                       (into-array java.nio.file.attribute.FileAttribute []))))
    (.deleteOnExit)))

(defn with-ws
  "Call f with the path (string) of a temp workspace holding the given
  {relative-path text} files; deletes it afterwards."
  [files f]
  (let [dir (io/file (System/getProperty "java.io.tmpdir")
                     (str "rig-ws-" (java.util.UUID/randomUUID)))]
    (try
      (.mkdirs dir)
      (doseq [[rel text] files]
        (io/make-parents (io/file dir rel))
        (spit (io/file dir rel) text))
      (f (str dir))
      (finally
        (delete-tree dir)))))

(defn ts
  "Maven timestamp: yyyyMMddHHmmss in UTC."
  [ms]
  (let [fmt (doto (java.text.SimpleDateFormat. "yyyyMMddHHmmss")
              (.setTimeZone (java.util.TimeZone/getTimeZone "UTC")))]
    (.format fmt (java.util.Date. ms))))

(defn minimal-pom
  [group name version]
  (str "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"
       "<project xmlns=\"http://maven.apache.org/POM/4.0.0\">\n"
       "  <modelVersion>4.0.0</modelVersion>\n"
       "  <groupId>" group "</groupId>\n"
       "  <artifactId>" name "</artifactId>\n"
       "  <version>" version "</version>\n"
       "  <packaging>jar</packaging>\n"
       "</project>\n"))

(defn repo-metadata!
  "Writes maven-metadata.xml for group/name under root listing only
  version, with last-updated ms."
  [root group name version last-updated-ms]
  (let [base (io/file root (str/replace group "." "/") name)]
    (.mkdirs base)
    (spit (io/file base "maven-metadata.xml")
          (str "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"
               "<metadata>\n"
               "  <versioning>\n"
               "    <versions><version>" version "</version></versions>\n"
               "    <latest>" version "</latest>\n"
               "    <release>" version "</release>\n"
               "    <lastUpdated>" (ts last-updated-ms) "</lastUpdated>\n"
               "  </versioning>\n"
               "</metadata>\n"))))

(defn repo-artifacts!
  "Builds a maven repo under root serving group/name at version v (pom, jar
  and .sha1 sidecars). Returns the repo base path (\"/g/p/name\")."
  [root group name v]
  (let [base (io/file root (str/replace group "." "/") name v)]
    (.mkdirs base)
    (let [pom (io/file base (str name "-" v ".pom"))
          jar (io/file base (str name "-" v ".jar"))]
      (spit pom (minimal-pom group name v))
      (with-open [zos (java.util.zip.ZipOutputStream. (io/output-stream jar))]
        (.putNextEntry zos (doto (java.util.zip.ZipEntry. "META-INF/MANIFEST.MF")
                             (.setTime 0)))
        (.write zos (.getBytes "Manifest-Version: 1.0\n" "UTF-8"))
        (.closeEntry zos))
      (spit (io/file base (str name "-" v ".pom.sha1")) (sha1-hex pom))
      (spit (io/file base (str name "-" v ".jar.sha1")) (sha1-hex jar)))
    (str "/" (str/replace group "." "/") "/" name)))

(defn m2-dir-of
  [group name]
  (io/file (System/getProperty "user.home") ".m2" "repository"
           (str/replace group "." "/") name))

(defn start-serving
  "A local HTTP server serving files under root by path: 401 when tok is
  given and the request lacks \"Bearer <tok>\", 404 for missing files.
  Records [method path auth] per request. Returns [base-url seen server]."
  [root tok]
  (let [seen (atom [])
        server (HttpServer/create (java.net.InetSocketAddress. "127.0.0.1" 0) 0)]
    (.createContext
     server "/"
     (reify com.sun.net.httpserver.HttpHandler
       (handle [_ exchange]
         (let [path (.getPath (.getRequestURI exchange))
               method (.getRequestMethod exchange)
               auth (first (.get (.getRequestHeaders exchange) "Authorization"))
               bad-auth (and tok (not= auth (str "Bearer " tok)))
               f (when-not bad-auth (io/file (str root path)))]
           (swap! seen conj [method path auth])
           (cond
             bad-auth (.sendResponseHeaders exchange 401 -1)
             (and f (.exists f))
             (if (= method "HEAD")
               (.sendResponseHeaders exchange 200 (long (.length f)))
               (do (.sendResponseHeaders exchange 200 (long (.length f)))
                   (with-open [os (.getResponseBody exchange)
                               is (io/input-stream f)]
                     (io/copy is os))))
             :else (.sendResponseHeaders exchange 404 -1))))))
    (.start server)
    [(str "http://" (.substring (str (.getAddress server)) 1) "/")
     seen server]))
