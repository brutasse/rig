(ns rig.resolver.versions-test
  (:require [clojure.java.io :as io]
            [clojure.string :as str]
            [clojure.test :refer :all]
            [rig.resolver.versions :as versions])
  (:import [java.io File]
           [com.sun.net.httpserver HttpServer]))

(defn- rm!
  [dir]
  (when-let [f (File. dir)]
    (when (.exists f)
      (doseq [c (.listFiles f)] (rm! (.getPath c)))
      (.delete f))))

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

(defn- empty-jar!
  [f]
  (io/make-parents f)
  (with-open [zos (java.util.zip.ZipOutputStream. (io/output-stream f))]
    (.putNextEntry zos (doto (java.util.zip.ZipEntry. "META-INF/MANIFEST.MF")
                         (.setTime 0)))
    (.write zos (.getBytes "Manifest-Version: 1.0\n" "UTF-8"))
    (.closeEntry zos)))

(defn- write-artifacts
  [base group name versions]
  (doseq [v (keys versions)]
    (let [vdir (io/file base v)
          pom (io/file vdir (str name "-" v ".pom"))
          jar (io/file vdir (str name "-" v ".jar"))]
      (.mkdirs vdir)
      (spit pom (minimal-pom group name v))
      (empty-jar! jar)
      (spit (io/file vdir (str name "-" v ".pom.sha1")) (sha1-hex pom))
      (spit (io/file vdir (str name "-" v ".jar.sha1")) (sha1-hex jar)))))

(defn with-file-repo
  "Builds a file:// maven repo serving group/name with per-version
  publication times ({version published-ms}); f receives the repo base URL."
  [[group name versions] f]
  (let [dir (File. (System/getProperty "java.io.tmpdir")
                   (str "rig-versions-test-" (java.util.UUID/randomUUID)))]
    (try
      (do
        (let [base (io/file dir (str/replace group "." "/") name)]
          (.mkdirs base)
          (spit (io/file base "maven-metadata.xml")
                (str "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"
                     "<metadata>\n"
                     "  <versioning>\n"
                     "    <versions>\n"
                     (apply str (for [v (sort (keys versions))]
                                  (str "      <version>" v "</version>\n")))
                     "    </versions>\n"
                     "    <lastUpdated>" (ts (System/currentTimeMillis))
                     "</lastUpdated>\n"
                     "  </versioning>\n"
                     "</metadata>\n"))
          (spit (io/file base "maven-metadata-f.xml")
                (str "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"
                     "<metadata>\n"
                     "  <versioning>\n"
                     "    <versions>\n"
                     (apply str (for [[v ms] (sort-by key versions)]
                                  (str "      <version>\n"
                                       "        <value>" v "</value>\n"
                                       "        <updated>" (ts ms) "</updated>\n"
                                       "      </version>\n")))
                     "    </versions>\n"
                     "  </versioning>\n"
                     "</metadata>\n"))
          (write-artifacts base group name versions))
         (f (str "file://" (.getPath dir))))
      (finally (rm! (str dir))))))

(deftest apply-versions-substitutes-only-floating
  (let [data {:deps {'org.clojure/test.check {:mvn/version "1.1.0"}}
              :aliases {:test {:extra-deps {'org.clojure/test.check {:mvn/version "RELEASE"}}}}}
        out (versions/apply-versions data {'org.clojure/test.check "1.1.3"} {})]
    (is (= "1.1.0" (get-in out [:deps 'org.clojure/test.check :mvn/version]))
        "concrete requirement must keep its pin")
    (is (= "1.1.3" (get-in out [:aliases :test :extra-deps 'org.clojure/test.check :mvn/version]))
        "floating requirement is substituted")))

(deftest apply-versions-substitutes-floating-in-deps
  (let [data {:deps {'org.clojure/test.check {:mvn/version "RELEASE"}}}
        out (versions/apply-versions data {'org.clojure/test.check "1.1.3"} {})]
    (is (= "1.1.3" (get-in out [:deps 'org.clojure/test.check :mvn/version])))))

(deftest apply-versions-honors-explicit-overrides
  (let [data {:deps {'org.clojure/test.check {:mvn/version "1.1.0"}}}
        out (versions/apply-versions data {'org.clojure/test.check "1.1.3"}
                                     {"org.clojure/test.check" "1.1.3"})]
    (is (= "1.1.3" (get-in out [:deps 'org.clojure/test.check :mvn/version]))
        "explicit override replaces even concrete requirements")))

(deftest apply-versions-leaves-unselected-coords-alone
  (let [data {:deps {'org.clojure/clojure {:mvn/version "1.12.5"}}}
        out (versions/apply-versions data {'org.clojure/test.check "1.1.3"} {})]
    (is (= "1.12.5" (get-in out [:deps 'org.clojure/clojure :mvn/version])))))

(defn- old-recent
  [now]
  [(- now (* 100 3600 1000)) (- now (* 1 3600 1000))])

(deftest select-versions-skips-recent-version-during-cooldown
  (let [now (System/currentTimeMillis)
        [old recent] (old-recent now)]
    (with-file-repo ["com.rig.test" "cooldown" {"1.0.0" old
                                                "1.0.1" recent}]
      (fn [url]
        (let [repos [["f" {:url url}]]
              data {:deps {'com.rig.test/cooldown {:mvn/version "RELEASE"}}}
              res (versions/select-versions data repos {:default "48h"}
                                            false {} {} true now)]
          (is (= "1.0.0" (get-in res [:selected 'com.rig.test/cooldown]))
              "selects the version older than the cooldown")
          (is (some #(and (= "1.0.1" (:version %))
                          (= "cooldown" (:reason %)))
                    (:skipped res))
              "skips the recent version with a cooldown reason")
          (is (empty? (:refused res))))))))

(deftest select-versions-force-selects-recent-version
  (let [now (System/currentTimeMillis)
        [old recent] (old-recent now)]
    (with-file-repo ["com.rig.test" "cooldown" {"1.0.0" old
                                                "1.0.1" recent}]
      (fn [url]
        (let [repos [["f" {:url url}]]
              data {:deps {'com.rig.test/cooldown {:mvn/version "RELEASE"}}}
              res (versions/select-versions data repos {:default "48h"}
                                            true {} {} true now)]
          (is (= "1.0.1" (get-in res [:selected 'com.rig.test/cooldown]))
              "force selects the recent version anyway")
          (is (some #(and (= "1.0.1" (:version %))
                          (= "forced" (:reason %)))
                    (:skipped res))
              "the forced pick is recorded"))))))

(deftest select-versions-refuses-when-no-version-survives-cooldown
  (let [now (System/currentTimeMillis)
        r1 (- now (* 1 3600 1000))
        r2 (- now (* 2 3600 1000))]
    (with-file-repo ["com.rig.test" "cooldown" {"1.0.0" r1 "1.0.1" r2}]
      (fn [url]
        (let [repos [["f" {:url url}]]
              data {:deps {'com.rig.test/cooldown {:mvn/version "RELEASE"}}}
              res (versions/select-versions data repos {:default "48h"}
                                            false {} {} true now)]
          (is (nil? (get-in res [:selected 'com.rig.test/cooldown])))
          (is (= 1 (count (:refused res))) "nothing selected -> refused")
          (is (= "cooldown" (get-in res [:refused 0 :reason]))))))))

;; --- :auth :oidc repos: bearer header on authenticated probes ---

(defn- auth-metadata-body
  []
  (str "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"
       "<metadata><versioning>\n"
       "  <versions><version>1.0.0</version></versions>\n"
       "  <lastUpdated>" (ts (System/currentTimeMillis)) "</lastUpdated>\n"
       "</versioning></metadata>"))

(defn- start-recording-server
  "A local HTTP server answering 200 with maven metadata for every request
  and recording the Authorization header of each request.
  Returns [base-url seen (atom of header-or-nil per request) server]."
  []
  (let [seen (atom [])
        server (HttpServer/create (java.net.InetSocketAddress. "127.0.0.1" 0) 0)]
    (.createContext
     server "/"
     (reify com.sun.net.httpserver.HttpHandler
       (handle [_ exchange]
         (swap! seen conj (.get (.getRequestHeaders exchange) "Authorization"))
         (let [b (.getBytes (auth-metadata-body) "UTF-8")]
           (.sendResponseHeaders exchange 200 (count b))
           (with-open [os (.getResponseBody exchange)]
             (.write os b))))))
    (doto server (.start))
    (let [addr (.substring (str (.getAddress server)) 1)]
      [(str "http://" addr "/") seen server])))

(deftest oidc-repo-probes-send-bearer-token
  (binding [versions/*oidc-tokens* {"f" "tok-123"}]
    (let [[base-url seen server] (start-recording-server)]
      (try
        (let [md (versions/repo-metadata "com.rig.test/authed"
                                         [["f" {:url (.toString base-url)
                                                :auth :oidc}]])]
          (is (seq (get md :by-version)) "metadata fetched through the probe")
          (is (every? #(and (seq %) (= "Bearer tok-123" (first %))) @seen)
              (str "every probe sent the bearer header: " (pr-str @seen))))
        (finally (.stop server 0))))))

(deftest oidc-repos-are-authed-per-repo
  "Two :auth :oidc repos each carry the bearer of their own repo id."
  (binding [versions/*oidc-tokens* {"f" "tok-123" "g" "tok-456"}]
    (let [[url-f seen-f srv-f] (start-recording-server)
          [url-g seen-g srv-g] (start-recording-server)]
      (try
        (let [md (versions/repo-metadata "com.rig.test/multi"
                                         [["f" {:url (.toString url-f)
                                                 :auth :oidc}]
                                          ["g" {:url (.toString url-g)
                                                 :auth :oidc}]])]
          (is (seq (get md :by-version)) "metadata fetched")
          (is (seq @seen-f) "repo f was probed")
          (is (seq @seen-g) "repo g was probed")
          (is (every? #(= "Bearer tok-123" (first %)) @seen-f)
              (str "repo f probed with its own token: " (pr-str @seen-f)))
          (is (every? #(= "Bearer tok-456" (first %)) @seen-g)
              (str "repo g probed with its own token: " (pr-str @seen-g))))
        (finally (.stop srv-f 0) (.stop srv-g 0))))))

(deftest unmarked-repo-probes-send-no-auth
  (binding [versions/*oidc-tokens* {"f" "tok-123"}]
    (let [[base-url seen server] (start-recording-server)]
      (try
        (let [md (versions/repo-metadata "com.rig.test/plain"
                                         [["f" {:url (.toString base-url)}]])]
          (is (seq (get md :by-version)) "metadata fetched")
          (is (every? nil? @seen)
              (str "no Authorization header on an unmarked repo: " (pr-str @seen))))
        (finally (.stop server 0))))))

(deftest oidc-repo-without-token-probes-unauthenticated
  (binding [versions/*oidc-tokens* nil]
    (let [[base-url seen server] (start-recording-server)]
      (try
        (let [md (versions/repo-metadata "com.rig.test/no-token"
                                         [["f" {:url (.toString base-url)
                                                :auth :oidc}]])]
          (is (seq (get md :by-version)) "probe still runs (401 in production)")
          (is (every? nil? @seen)
              (str "no header without a token: " (pr-str @seen))))
        (finally (.stop server 0))))))

(deftest resolve-repo-uses-m2-recorded-repo-with-specs
  (let [m2 (io/file (System/getProperty "java.io.tmpdir")
                    (str "rig-m2-test-" (java.util.UUID/randomUUID)))]
    (try
      (do (.mkdirs (io/file m2 "com/rig/test/art/0.1.0"))
          (spit (io/file m2 "com/rig/test/art/0.1.0/_remote.repositories")
                (str "art-0.1.0.jar>f=\n"))
          (let [[id url] (versions/resolve-repo
                          (str m2)
                          [["f" {:url "https://example.com/f"}]
                           ["g" {:url "https://example.com/g"}]]
                          {:group "com.rig.test" :name "art" :version "0.1.0"
                           :extension "jar" :classifier nil})]
            (is (= "f" id) "m2-recorded repo id wins")
            (is (= "https://example.com/f/com/rig/test/art/0.1.0/art-0.1.0.jar" url)
                (str "url built from the repo spec: " url))))
      (finally (rm! (str m2))))))

(deftest resolve-repo-probes-with-bearer-token
  (binding [versions/*oidc-tokens* {"f" "tok-456"}]
    (let [[base-url seen server] (start-recording-server)]
      (try
        (let [[id url] (versions/resolve-repo
                        (str (io/file (System/getProperty "java.io.tmpdir") "no-such-m2"))
                        [["f" {:url (.toString base-url) :auth :oidc}]]
                        {:group "com.rig.test" :name "art" :version "0.1.0"
                         :extension "jar" :classifier nil})]
          (is (= "f" id) "probe found the artifact")
          (is (.startsWith url (.toString base-url)) "url from the probed repo")
          (is (every? #(and (seq %) (= "Bearer tok-456" (first %))) @seen)
              (str "HEAD probe sent the bearer header: " (pr-str @seen))))
        (finally (.stop server 0))))))


;; --- auth proxy env: RIG_PROXY_REPOS rewrites marked repo urls ---

(deftest proxy-map-defaults-to-empty
  (is (= {} (versions/proxy-map)) "unbound reads the (unset) env")
  (binding [versions/*proxy-repos* {"oidc" "http://127.0.0.1:9999/r/oidc"}]
    (is (= {"oidc" "http://127.0.0.1:9999/r/oidc"} (versions/proxy-map)))
    (binding [versions/*proxy-repos* nil]
      (is (nil? (versions/proxy-map)) "nil binding stays nil"))))

(deftest with-proxy-repos-rewrites-only-mapped-repo-ids
  (binding [versions/*proxy-repos* {"oidc" "http://127.0.0.1:9999/r/oidc"}]
    (let [data {:mvn/repos {"oidc" {:url "https://pier.example" :auth :oidc}
                            "central" {:url "https://repo.maven.apache.org/maven2"}}}]
      (is (= {"oidc" {:url "http://127.0.0.1:9999/r/oidc" :auth :oidc}
              "central" {:url "https://repo.maven.apache.org/maven2"}}
             (-> data versions/with-proxy-repos (get :mvn/repos)))
          "mapped id rewritten, others kept")
      (is (= {:mvn/repos {"central" {:url "https://repo.maven.apache.org/maven2"}}}
             (versions/with-proxy-repos
              {:mvn/repos {"central" {:url "https://repo.maven.apache.org/maven2"}}}))
          "nothing mapped, data returned unchanged")
      (is (= {} (versions/with-proxy-repos {})) "no repos, data returned unchanged"))
    (binding [versions/*proxy-repos* nil]
      (is (= {:mvn/repos {"oidc" {:url "https://pier.example"}}}
             (versions/with-proxy-repos
              {:mvn/repos {"oidc" {:url "https://pier.example"}}}))
          "no proxy binding, data returned unchanged"))))
