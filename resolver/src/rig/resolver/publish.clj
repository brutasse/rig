(ns rig.resolver.publish
  "The pom + target half of `rig publish`/`rig install` (design §7.3).

  Computes the module's maven coordinates, renders the POM from its direct
  dependencies (floating requirements pinned from the lock), and resolves the
  deploy repository url. The artifact transfer itself (HTTP deploy, local
  m2 install) and the credentials (settings.xml) stay in Go (design §7.4)."
  (:require [clojure.java.io :as io]
            [clojure.string :as str]
            [rig.resolver.manifest :as manifest]
            [rig.resolver.resolve :as resolve]))

(defn- xml-esc
  [s]
  (-> (str s)
      (str/replace "&" "&amp;")
      (str/replace "<" "&lt;")
      (str/replace ">" "&gt;")))

(defn- pom-deps
  "The module's direct mvn dependencies as [[group artifact] version…].
  Git/local deps are not in a POM. Floating requirements (RELEASE/…) take
  the lock's pinned version."
  [data pins]
  (->> (get data :deps {})
       (keep (fn [[coord spec]]
               (when (or (string? spec) (get spec :mvn/version))
                 (let [[group artifact] (str/split (str coord) #"/" 2)
                       declared (or (when (string? spec) spec)
                                    (get spec :mvn/version))]
                    [group artifact (or (get pins (str coord)) declared)]))))
        (vec)))

(defn- pom
  [group artifact version deps]
  (str "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"
       "<project xmlns=\"http://maven.apache.org/POM/4.0.0\">\n"
       "  <modelVersion>4.0.0</modelVersion>\n"
       (format "  <groupId>%s</groupId>\n" (xml-esc group))
       (format "  <artifactId>%s</artifactId>\n" (xml-esc artifact))
       (format "  <version>%s</version>\n" (xml-esc version))
       "  <packaging>jar</packaging>\n"
       (when (seq deps)
         (str "  <dependencies>\n"
              (apply str
                     (for [[g a v] deps]
                       (str "    <dependency>\n"
                            (format "      <groupId>%s</groupId>\n" (xml-esc g))
                            (format "      <artifactId>%s</artifactId>\n" (xml-esc a))
                            (format "      <version>%s</version>\n" (xml-esc v))
                            "    </dependency>\n")))
              "  </dependencies>\n"))
       "</project>\n"))

(defn- pins-of
  "coord -> version over the lock's mvn artifacts."
  [lock]
  (into {}
        (for [a (get lock :artifacts)
              :when (= "mvn" (get a :kind))]
          [(str (get a :group) "/" (get a :name)) (get a :version)])))

(defn- repo-url
  "The deploy url for repo id: the CLOJARS_URL env override (clojars only),
  then the module/root manifest :mvn/repos entry, then the clojars default."
  [repo module-data root-data]
  (let [url (or (when (= repo "clojars") (System/getenv "CLOJARS_URL"))
                (get-in module-data [:mvn/repos repo :url])
                (get-in root-data [:mvn/repos repo :url])
                (when (= repo "clojars") "https://clojars.org/repo"))]
    (when (nil? url)
      (throw (ex-info (str "publish: unknown repository " (pr-str repo)
                           " (no :mvn/repos entry in the module or root manifest)")
                      {})))
    url))

(defn publish
  "Kernel op: the publish plan for one module. Args: {module} — the lock
  comes from the request envelope. Response:
  {\"published\": [{coord version repo url pom}]}, pom = the POM content."
  [request]
  (let [ws (str (:workspace request))
        args (or (:args request) {})
        m (str (get args :module "."))
        mdir (if (= m ".") ws (str (io/file ws m)))
        man (manifest/read-manifest mdir)
        data (or (:data man) {})
        root-data (or (:data (manifest/read-manifest ws)) {})
        lib (manifest/lib data)
        parts (str/split (str lib) #"/" 2)
        version (manifest/read-version data mdir ws)
        spec (manifest/publish-spec data)
        repo (or (get spec :repo) "clojars")
        pins (pins-of (resolve/lock-of request))]
    (cond
      (nil? lib)
      (throw (ex-info (str "publish: module " m " has no :rig/lib coordinate") {}))
      (nil? (second parts))
      (throw (ex-info (str "publish: bad :rig/lib " (pr-str lib)
                           " (want group/name)") {}))
      (nil? version)
      (throw (ex-info (str "publish: module " m " has no version (:rig/version, a VERSION file, or :rig/version-fn)") {}))
      (true? (get spec :sign-releases?))
      (throw (ex-info (str "publish: :sign-releases? is not supported (set it "
                           "to false)")
                      {}))
      :else
      {"published" [{"coord" (str (first parts) "/" (second parts))
                     "version" version
                     "repo" repo
                     "url" (repo-url repo data root-data)
                     "pom" (pom (first parts) (second parts) version
                                (pom-deps data pins))}]})))
