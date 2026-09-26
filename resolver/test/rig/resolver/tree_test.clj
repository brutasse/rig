(ns rig.resolver.tree-test
  (:require [clojure.java.io :as io]
            [clojure.string :as str]
            [clojure.test :refer :all]
            [rig.resolver.tree :as tree]
            [rig.resolver.versions :as versions])
  (:import [java.io File]
           [com.sun.net.httpserver HttpServer]))

(defn- rm!
  [dir]
  (when-let [f (File. dir)]
    (when (.exists f)
      (doseq [c (.listFiles f)] (rm! (.getPath c)))
      (.delete f))))

(defn- with-ws
  [files f]
  (let [dir (File. (System/getProperty "java.io.tmpdir")
                   (str "rig-tree-test-" (java.util.UUID/randomUUID)))]
    (try
      (do (.mkdirs dir)
          (doseq [[rel text] files]
            (io/make-parents (io/file dir rel))
            (spit (io/file dir rel) text))
          (f (str dir)))
      (finally (rm! (str dir))))))

(deftest tree-basic
  (with-ws [["deps.edn" "{:deps {org.clojure/clojure {:mvn/version \"1.12.5\"}
                              cheshire/cheshire {:mvn/version \"5.10.2\"}}}"]]
    (fn [dir]
      (let [lines (-> (tree/tree {:workspace dir}) (get "tree") str/split-lines)]
        (is (= "." (first lines))
            "root on the first line")
        (is (some #(= % "├── org.clojure/clojure:1.12.5") lines)
            "first top-level dep, in declaration order")
        (is (some #(= % "└── cheshire/cheshire:5.10.2") lines)
            "last top-level dep")
        (is (some #(= % "│   ├── org.clojure/spec.alpha:0.5.238") lines)
            "transitive dep under clojure")
        (is (some #(= % "    │   └── com.fasterxml.jackson.core/jackson-core:2.12.4 (same-version)") lines)
            "a shared dep is shown under each parent, marked, not expanded")
        (is (= 2 (count (filter #(re-find #"\(same-version\)$" %) lines)))
            "…once per parent")))))

(deftest tree-alias
  (with-ws [["deps.edn" "{:deps {org.clojure/clojure {:mvn/version \"1.12.5\"}}
                          :aliases {:extra {:extra-deps {cheshire/cheshire {:mvn/version \"5.10.2\"}}}}}"]]
    (fn [dir]
      (let [text (fn [req] (-> (tree/tree req) (get "tree")))]
        (is (not (str/includes? (text {:workspace dir}) "cheshire/cheshire"))
            "alias dep hidden without the alias")
        (is (str/includes? (text {:workspace dir :args {:alias "extra"}})
                           "cheshire/cheshire:5.10.2")
            "alias dep shown with the alias")))))

(deftest tree-floats-pinned-by-lock
  (with-ws [["deps.edn" "{:deps {org.clojure/clojure {:mvn/version \"RELEASE\"}}}"]
            ["deps.lock" "{\"version\":1,\"artifacts\":[{\"kind\":\"mvn\",\"group\":\"org.clojure\",\"name\":\"clojure\",\"version\":\"1.12.5\"}]}"]]
    (fn [dir]
      (let [text (-> (tree/tree {:workspace dir :lock "deps.lock"}) (get "tree"))]
        (is (str/includes? text "org.clojure/clojure:1.12.5")
            "floating RELEASE pinned to the lock version")))))

(deftest tree-module
  (with-ws [["deps.edn" "{:rig/modules [\"modules/app\"]}"]
            ["modules/app/deps.edn" "{:rig/lib example/app
                                      :rig/version \"0.1.0\"
                                      :deps {org.clojure/clojure {:mvn/version \"1.12.5\"}}}"]]
    (fn [dir]
      (let [lines (-> (tree/tree {:workspace dir :args {:module "modules/app"}})
                      (get "tree") str/split-lines)]
        (is (= "example/app:0.1.0" (first lines))
            "root is the module's lib")
        (is (some #(= % "└── org.clojure/clojure:1.12.5") lines)
            "the module's dep under its root")))))

;; --- Auth proxy: the tree rewrites :auth :oidc repos to the proxy base URL
;; --- (exported by rig as RIG_PROXY_REPOS) so calc-trace resolves through it.
;; --- A local server stands in for rig's loopback proxy.

(defn- ts
  "Maven timestamp: yyyyMMddHHmmss in UTC."
  [ms]
  (let [fmt (doto (java.text.SimpleDateFormat. "yyyyMMddHHmmss")
              (.setTimeZone (java.util.TimeZone/getTimeZone "UTC")))]
    (.format fmt (java.util.Date. ms))))

(defn- sha1-hex
  [f]
  (let [md (java.security.MessageDigest/getInstance "SHA-1")
        in (io/input-stream f)
        buf (byte-array 65536)]
    (with-open [in in]
      (loop []
        (let [n (.read in buf)]
          (when (pos? n)
            (do (.update md buf 0 n)
                (recur)))))
      (apply str (for [b (.digest md)] (format "%02x" (bit-and b 0xff)))))))

(defn- minimal-pom
  [group name version]
  (str "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"
       "<project xmlns=\"http://maven.apache.org/POM/4.0.0\">\n"
       "  <modelVersion>4.0.0</modelVersion>\n"
       "  <groupId>" group "</groupId>\n"
       "  <artifactId>" name "</artifactId>\n"
       "  <version>" version "</version>\n"
       "  <packaging>jar</packaging>\n"
       "</project>\n"))

(defn- repo-artifacts!
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

(defn- repo-metadata!
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

(defn- start-serving
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
               (.sendResponseHeaders exchange 200 -1)
               (do (.sendResponseHeaders exchange 200 (long (.length f)))
                   (with-open [os (.getResponseBody exchange)
                               is (io/input-stream f)]
                     (io/copy is os))))
             :else (.sendResponseHeaders exchange 404 -1))))))
    (.start server)
    [(str "http://" (.substring (str (.getAddress server)) 1) "/")
     seen server]))

(defn- m2-dir-of
  [group name]
  (io/file (System/getProperty "user.home") ".m2" "repository"
           (str/replace group "." "/") name))

(deftest tree-floats-via-proxy
  (let [group "rig.ktest.proxy" name "treefloat" v "2.0.0"
        orig-root (doto (File. (str (System/getProperty "java.io.tmpdir") "/"
                                   (str "rig-tree-orig-" (java.util.UUID/randomUUID))))
                    (.mkdirs) (.deleteOnExit))
        proxy-root (doto (File. (str (System/getProperty "java.io.tmpdir") "/"
                                     (str "rig-tree-py-" (java.util.UUID/randomUUID))))
                     (.mkdirs) (.deleteOnExit))
        [orig-base orig-seen orig-srv] (start-serving (str orig-root) "tok-e2e")
        [proxy-base proxy-seen proxy-srv] (start-serving (str proxy-root) nil)
        base-path (repo-artifacts! (str proxy-root) group name v)
        _ (repo-metadata! (str proxy-root) group name v (System/currentTimeMillis))
        m2 (m2-dir-of group name)]
    (rm! (str m2))
    (try
      (binding [versions/*proxy-repos* {"oidc" proxy-base}]
        (with-ws [["deps.edn" (str "{:deps {" group "/" name " {:mvn/version \"RELEASE\"}}"
                                   " :mvn/repos {\"oidc\" {:url \"" orig-base "\" :auth :oidc}}}")]]
          (fn [dir]
            (let [resp (tree/tree {:workspace dir
                                   :args {:cooldown {:default "0s"}}})
                  coord (str group "/" name ":" v)]
              (is (str/includes? (get resp "tree") coord)
                  "floating RELEASE resolved through the proxy")
              (is (some #(= (str base-path "/maven-metadata.xml") (nth % 1)) @proxy-seen)
                  "metadata read through the proxy")
              (is (some #(= (str base-path "/" v "/" name "-" v ".jar") (nth % 1)) @proxy-seen)
                  "artifact fetched through the proxy")
              (is (empty? @orig-seen) "zero cooldown: no probe, no original traffic")))))
      (finally
        (.stop orig-srv 0)
        (.stop proxy-srv 0)
        (rm! (str m2))
        (rm! (str orig-root))
        (rm! (str proxy-root))))))
