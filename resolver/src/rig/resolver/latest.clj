(ns rig.resolver.latest
  "Kernel op: the newest cooldown-eligible version of one coordinate, or of
  several at once, for a caller that has nothing to resolve against. `rig
  new` asks it for the test runner and the scaffold's test and dev deps, so a
  new project starts on what the repositories serve rather than on versions
  compiled into the rig binary.

  Request:  {op \"latest\" workspace ws args {:coord \"group/name\"  ; one of
                                               :coords [g/n ...]        ; the other
                                               :repos {id spec}  ; optional
                                               :cooldown policy   ; optional
                                               :force bool}}      ; optional
  Response: single :coord: {version v} or {version nil reason r cooldown c}.
  A :coords vector: {versions {g/n v} reasons {g/n r}} — versions carries the
  coordinates a release was found for, reasons the rest, where r is
  \"cooldown\" (every candidate is inside the window) or \"not-found\" (no
  repository serves a release of the coordinate).

  The workspace is read for its repositories and its cooldown policy, exactly
  as a lock run reads them; :repos replaces them for a caller with no
  workspace yet, and for tests."
  (:require [clojure.string :as str]
            [rig.resolver.manifest :as manifest]
            [rig.resolver.resolve :as resolve]
            [rig.resolver.versions :as versions]))

(defn- valid-coord?
  [c]
  (and (string? c) (str/includes? c "/")))

(defn latest
  [request]
  (let [ws (str (:workspace request))
        args (or (:args request) {})
        single (get args :coord)
        batch (get args :coords)
        _ (when (and single batch)
            (throw (ex-info "latest: :coord and :coords are mutually exclusive"
                            {:rig/friendly? true})))
        _ (when (and (nil? single) (empty? batch))
            (throw (ex-info "latest requires args :coord (a group/name coordinate) or :coords (a vector of them)"
                            {:rig/friendly? true})))
        _ (when (and single (not (valid-coord? single)))
            (throw (ex-info "latest requires args :coord (a group/name coordinate)"
                            {:rig/friendly? true})))
        _ (when (some #(not (valid-coord? %)) batch)
            (throw (ex-info "latest: :coords entries must be group/name coordinates"
                            {:rig/friendly? true})))
        root-data (or (:data (manifest/read-manifest ws)) {})
        repos (or (seq (get args :repos)) (resolve/all-repos ws))
        cooldown (resolve/cooldown-of args root-data)
        force (true? (get args :force))
        now (System/currentTimeMillis)
        sels (into {} (for [c (if (nil? single) (vec batch) [single])]
                        [c (versions/select-latest c repos cooldown force now)]))]
    (if (nil? single)
      {"versions" (into {} (for [[c sel] sels
                                 :when (:version sel)]
                             [c (:version sel)]))
       "reasons" (into {} (for [[c sel] sels
                                :when (nil? (:version sel))]
                            [c (or (:reason sel) "cooldown")]))}
      (if-let [v (:version (get sels single))]
        {"version" v}
        {"version" nil
         "reason" (or (:reason (get sels single)) "cooldown")
         "cooldown" (or (get cooldown :default) versions/default-cooldown)}))))
