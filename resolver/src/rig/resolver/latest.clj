(ns rig.resolver.latest
  "Kernel op: the newest cooldown-eligible version of one coordinate, for a
  caller that has nothing to resolve against. `rig new` asks it for the test
  runner to scaffold, so a new project starts on what the repositories serve
  rather than on a version compiled into the rig binary.

  Request:  {op \"latest\" workspace ws args {:coord \"group/name\"
                                             :repos {id spec}  ; optional
                                             :cooldown policy   ; optional
                                             :force bool}}      ; optional
  Response: {version v} or {version nil reason r cooldown c}, where r is
  \"cooldown\" (every candidate is inside the window) or \"not-found\" (no
  repository serves the coordinate).

  The workspace is read for its repositories and its cooldown policy, exactly
  as a lock run reads them; :repos replaces them for a caller with no
  workspace yet, and for tests."
  (:require [clojure.string :as str]
            [rig.resolver.manifest :as manifest]
            [rig.resolver.resolve :as resolve]
            [rig.resolver.versions :as versions]))

(defn latest
  [request]
  (let [ws (str (:workspace request))
        args (or (:args request) {})
        coord (get args :coord)
        _ (when-not (and (string? coord) (str/includes? coord "/"))
            (throw (ex-info "latest requires args :coord (a group/name coordinate)"
                            {:rig/friendly? true})))
        root-data (or (:data (manifest/read-manifest ws)) {})
        repos (or (seq (get args :repos)) (resolve/all-repos ws))
        cooldown (resolve/cooldown-of args root-data)
        sel (versions/select-latest coord repos cooldown
                                    (true? (get args :force))
                                    (System/currentTimeMillis))]
    (if-let [v (:version sel)]
      {"version" v}
      {"version" nil
       "reason" (or (:reason sel) "cooldown")
       "cooldown" (or (get cooldown :default) versions/default-cooldown)})))
