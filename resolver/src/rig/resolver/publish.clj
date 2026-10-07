(ns rig.resolver.publish
  "The pom + target half of `rig publish`/`rig install` (design §7.3).

  Computes the module's maven coordinates, renders the POM from its direct
  dependencies (floating requirements pinned from the lock), and resolves the
  deploy repository url. The artifact transfer itself (HTTP deploy, local
  m2 install) and the credentials (settings.xml) stay in Go (design §7.4)."
  (:require [clojure.java.io :as io]
            [clojure.string :as str]
            [rig.resolver.manifest :as manifest]
            [rig.resolver.pom :as pom]
            [rig.resolver.resolve :as resolve]))

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
        pins (pom/pins-of (resolve/lock-of request))]
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
                     "pom" (pom/pom (first parts) (second parts) version
                                     (pom/pom-deps data pins))}]})))
