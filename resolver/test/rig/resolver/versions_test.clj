(ns rig.resolver.versions-test
  (:require [clojure.java.io :as io]
            [clojure.string :as str]
            [clojure.test :refer :all]
            [rig.resolver.test-util :refer [delete-tree sha1-hex minimal-pom ts]]
            [rig.resolver.versions :as versions])
  (:import [java.io File]
           [com.sun.net.httpserver HttpServer]))

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
  publication times ({version published-ms}); f receives the repo base URL.
  The optional latest is written as the repo's <latest> marker."
  [[group name versions latest] f]
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
                     (when latest (str "    <latest>" latest "</latest>\n"))
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
      (finally (delete-tree (str dir))))))

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
                                            false {} {} now)]
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
                                            true {} {} now)]
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
                                            false {} {} now)]
          (is (nil? (get-in res [:selected 'com.rig.test/cooldown])))
          (is (= 1 (count (:refused res))) "nothing selected -> refused")
          (is (= "cooldown" (get-in res [:refused 0 :reason]))))))))

(deftest select-versions-accepts-per-repo-cooldowns
  "all-cooldowns-zero? folds the per-repo cooldowns in: an (apply zero?
  ...) crashed with ArityException as soon as :repos was non-empty."
  (let [data {:deps {'com.rig.test/cooldown {:mvn/version "RELEASE"}}}]
    (is (= {:selected {} :skipped [] :refused [] :changed? false}
           (versions/select-versions data [] {:default "0s" :repos {"f" "0s"}}
                                     false {} {}
                                     (System/currentTimeMillis)))
        "all-zero per-repo cooldowns defer to native resolution")))

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

(deftest oidc-repos-are-authed-per-repo
  "Two :auth :oidc repos each carry the bearer of their own repo id — and
  every probe of a marked repo carries one at all (the single-repo case is
  contained here)."
  (binding [versions/*oidc-tokens* {"f" "tok-123" "g" "tok-456"}
            versions/*settings-servers* {}]
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
          (is (every? #(and (seq %) (= "Bearer tok-123" (first %))) @seen-f)
              (str "repo f probed with its own token: " (pr-str @seen-f)))
          (is (every? #(= "Bearer tok-456" (first %)) @seen-g)
              (str "repo g probed with its own token: " (pr-str @seen-g))))
        (finally (.stop srv-f 0) (.stop srv-g 0))))))

(deftest unmarked-repo-probes-send-no-auth
  (binding [versions/*oidc-tokens* {"f" "tok-123"}
            versions/*settings-servers* {}]
    (let [[base-url seen server] (start-recording-server)]
      (try
        (let [md (versions/repo-metadata "com.rig.test/plain"
                                         [["f" {:url (.toString base-url)}]])]
          (is (seq (get md :by-version)) "metadata fetched")
          (is (every? nil? @seen)
              (str "no Authorization header on an unmarked repo: " (pr-str @seen))))
        (finally (.stop server 0))))))

(deftest oidc-repo-without-token-probes-unauthenticated
  (binding [versions/*oidc-tokens* nil
            versions/*settings-servers* {}]
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
      (finally (delete-tree (str m2))))))

(deftest resolve-repo-probes-with-bearer-token
  (binding [versions/*oidc-tokens* {"f" "tok-456"}
            versions/*settings-servers* {}]
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

;; --- :auth basic repos: ~/.m2/settings.xml credentials on the probes ---

(defn- start-refusing-server
  "A local HTTP server answering every request with status and no body.
   Returns [base-url server]."
  [status]
  (let [server (HttpServer/create (java.net.InetSocketAddress. "127.0.0.1" 0) 0)]
    (.createContext
     server "/"
     (reify com.sun.net.httpserver.HttpHandler
       (handle [_ exchange]
         (.sendResponseHeaders exchange status 0))))
    (doto server (.start))
    (let [addr (.substring (str (.getAddress server)) 1)]
      [(str "http://" addr "/") server])))

(deftest settings-server-probes-with-basic-auth
  "A repo id with credentials in the user settings.xml probes
  basic-authenticated — the same file the Go fetcher reads at build
  time, so a private repository attributes to itself instead of 401ing."
  (binding [versions/*oidc-tokens* nil
            versions/*settings-servers* {"f" ["user" "pass"]}]
    (let [[base-url seen server] (start-recording-server)]
      (try
        (let [[id url] (versions/resolve-repo
                        (str (io/file (System/getProperty "java.io.tmpdir") "no-such-m2"))
                        [["f" {:url (.toString base-url)}]]
                        {:group "com.rig.test" :name "art" :version "0.1.0"
                         :extension "jar" :classifier nil})]
          (is (= "f" id) "probe found the artifact")
          (is (every? #(= "Basic dXNlcjpwYXNz" (first %)) @seen)
              (str "HEAD probe sent the basic credentials: " (pr-str @seen))))
        (finally (.stop server 0))))))

(deftest resolve-repo-refuses-when-no-repo-serves
  "Every probe definitive (404/401): the lock must fail with a per-repo
   report rather than attribute the artifact to a repository that
   refuses it — that attribution is an undownloadable url on a cold
   .m2."
  (binding [versions/*oidc-tokens* nil
            versions/*settings-servers* {}]
    (let [[base-404 srv-404] (start-refusing-server 404)
          [base-401 srv-401] (start-refusing-server 401)]
      (try
        (let [e (try
                  (versions/resolve-repo
                   (str (io/file (System/getProperty "java.io.tmpdir") "no-such-m2"))
                   [["a" {:url (.toString base-404)}]
                    ["b" {:url (.toString base-401)}]]
                   {:group "com.rig.test" :name "art" :version "0.1.0"
                    :extension "jar" :classifier nil})
                  (catch Exception e e))]
          (is (instance? clojure.lang.ExceptionInfo e) "the error is a friendly ex-info")
          (is (.contains (.getMessage e) "no repository serves com.rig.test/art:0.1.0")
              (str (.getMessage e)))
          (is (.contains (.getMessage e) "a: not found")
              (str "the 404 is reported: " (.getMessage e)))
          (is (.contains (.getMessage e) "b: authentication required")
              (str "the 401 is reported: " (.getMessage e)))
          (is (.contains (.getMessage e) "add credentials for \"b\" to ~/.m2/settings.xml")
              (str "the missing credentials are named: " (.getMessage e)))
          (is (true? (get (ex-data e) :rig/friendly?)) "the runner prints it plainly"))
        (finally (.stop srv-404 0) (.stop srv-401 0))))))

(deftest resolve-repo-attributes-the-first-2xx
  "A 404 does not block a later repo: the first 2xx in probe order wins."
  (binding [versions/*oidc-tokens* nil
            versions/*settings-servers* {}]
    (let [[base-404 srv-404] (start-refusing-server 404)
          [base-200 _ srv-200] (start-recording-server)]
      (try
        (let [[id url] (versions/resolve-repo
                        (str (io/file (System/getProperty "java.io.tmpdir") "no-such-m2"))
                        [["a" {:url (.toString base-404)}]
                         ["b" {:url (.toString base-200)}]]
                        {:group "com.rig.test" :name "art" :version "0.1.0"
                         :extension "jar" :classifier nil})]
          (is (= "b" id) "the second repo served the artifact")
          (is (.startsWith url (.toString base-200)) "url from the serving repo"))
        (finally (.stop srv-404 0) (.stop srv-200 0))))))

(deftest resolve-repo-falls-back-when-a-probe-is-inconclusive
  "A 405 (HEAD not supported) is inconclusive: the first repository is a
   stand-in attribution, verified later by the build's sha256 check."
  (binding [versions/*oidc-tokens* nil
            versions/*settings-servers* {}]
    (let [[base-405 srv-405] (start-refusing-server 405)
          [base-404 srv-404] (start-refusing-server 404)]
      (try
        (let [[id url] (versions/resolve-repo
                        (str (io/file (System/getProperty "java.io.tmpdir") "no-such-m2"))
                        [["a" {:url (.toString base-405)}]
                         ["b" {:url (.toString base-404)}]]
                        {:group "com.rig.test" :name "art" :version "0.1.0"
                         :extension "jar" :classifier nil})]
          (is (= "a" id) "the first repo is the stand-in")
          (is (.startsWith url (.toString base-405)) "url from the first repo"))
        (finally (.stop srv-405 0) (.stop srv-404 0))))))

(deftest settings-servers-parses-namespaced-servers
  "The user settings.xml is read namespace-agnostically (Maven ships the
   settings schema namespace) into {repo-id [username password]}; an
   entry without all three fields is dropped."
  (let [dir (File. (System/getProperty "java.io.tmpdir")
                   (str "rig-settings-test-" (java.util.UUID/randomUUID)))
        old (System/getProperty "user.home")]
    (try
      (do (.mkdirs (io/file dir ".m2"))
          (spit (io/file dir ".m2/settings.xml")
                (str "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"
                     "<settings xmlns=\"http://maven.apache.org/SETTINGS/1.0.0\">\n"
                     "  <servers>\n"
                     "    <server>\n"
                     "      <id>private</id>\n"
                     "      <username>u</username>\n"
                     "      <password>p</password>\n"
                     "    </server>\n"
                     "    <server>\n"
                     "      <id>incomplete</id>\n"
                     "      <username>u</username>\n"
                     "    </server>\n"
                     "  </servers>\n"
                     "</settings>"))
          (System/setProperty "user.home" (str dir))
          (is (= {"private" ["u" "p"]} (versions/settings-servers))
              "namespaced servers parsed, incomplete entries dropped"))
      (finally (System/setProperty "user.home" old)
               (delete-tree (str dir))))))

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

(deftest pick-fix-same-major
  (is (= {:target "1.12.0"}
         (versions/pick-fix "1.11.0" ["1.11.1" "1.12.0"] false))
      "the newest fix at or above the locked version in its own major")
  (is (= {:target "0.6.26"}
         (versions/pick-fix "0.6.17" ["0.6.20" "0.6.26"] false))
      "0.x majors: 0.6.20 and 0.6.26 are the same major as 0.6.17")
  (is (= {:target "1.12.0"}
         (versions/pick-fix "1.11.0" ["1.12.0-beta" "1.12.0"] false))
      "Maven ordering: the release beats its pre-release"))

(deftest pick-fix-breaking
  (is (= {:reason "breaking"}
         (versions/pick-fix "1.2.0" ["2.0.0"] false))
      "a higher-major fix is not picked without the flag")
  (is (= {:target "2.0.0"}
         (versions/pick-fix "1.2.0" ["2.0.0"] true))
      "the higher-major fix joins the candidates with the flag")
  (is (= {:target "2.0.0"}
         (versions/pick-fix "1.2.0" ["2.0.0" "1.2.5"] true))
      "with the flag, the newest candidate wins, whatever its major")
  (is (= {:reason "breaking"}
         (versions/pick-fix "1.0.0" ["0.5.0" "2.0.0"] false))
      "a below-locked fix beside a higher-major one still reports breaking")
  (is (= {:reason "breaking"}
         (versions/pick-fix "0.9.0" ["1.0.0"] false))
      "0.x to 1.x is a major jump"))

(deftest pick-fix-unfixable
  (is (= {:reason "no-fix"}
         (versions/pick-fix "1.0.0" [] false))
      "no fix listed")
  (is (= {:reason "below-locked"}
         (versions/pick-fix "1.5.0" ["1.4.0"] false))
      "fixes only below the locked version are a rollback, a human call")
  (is (= {:reason "no-fix"}
         (versions/pick-fix "1.0.0" ["1.0.1-SNAPSHOT"] false))
      "a snapshot-only fix set is an empty one")
  (is (= {:target "1.0.2"}
         (versions/pick-fix "1.0.0" ["1.0.1-SNAPSHOT" "1.0.2"] false))
      "snapshots are excluded, releases stand"))
