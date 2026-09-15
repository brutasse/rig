(ns rig.resolver.outdated
  (:require [rig.resolver.resolve :as resolve]
            [rig.resolver.versions :as versions]))

(defn- major-of
  [v]
  (when-let [[_ m] (re-find #"^(\d+)" (str v))]
    (Integer/parseInt m)))

(defn- newest
  [vs]
  (reduce (fn [a b] (if (pos? (versions/vcmp a b)) a b)) vs))

(defn update-entry
  "The outdated entry for a [coord current] pin given repo-metadata, or nil
  when current is already the newest candidate. breaking = the newest
  candidate has a higher numeric major; latest-satisfying = the newest
  candidate sharing current's major (current itself when none is newer)."
  [[coord current] md]
  (let [byv (get md :by-version)
        cands (if (seq (:latest md)) (into #{} (:latest md)) (keys byv))
        latest (when (seq cands) (newest (seq cands)))]
    (when (and latest (pos? (versions/vcmp latest current)))
      (let [cm (major-of current)
            lm (major-of latest)
            pool (if cm (filter #(= cm (major-of %)) cands) cands)
            newer (filter #(pos? (versions/vcmp % current)) pool)
            sat (if (seq newer) (newest (seq newer)) current)]
        {"coord" coord
         "current" current
         "latest-satisfying" sat
         "latest" latest
         "breaking" (boolean (and cm lm (> lm cm)))}))))

(defn outdated
  [request]
  (let [ws (str (:workspace request))
        args (or (:args request) {})
        offline (true? (get args :offline))
        breaking-only (true? (get args :breaking))
        repos (resolve/all-repos ws)
        lock (resolve/lock-of request)
        pins (into {}
                   (for [a (get lock :artifacts)
                         :when (= "mvn" (get a :kind))]
                     [(str (get a :group) "/" (get a :name)) (get a :version)]))]
    (if (and offline (seq pins))
      (throw (ex-info "outdated requires the network: drop --offline" {}))
      (let [updates (->> (pmap (fn [[coord cur]]
                                 (update-entry [coord cur]
                                               (versions/repo-metadata coord repos)))
                               (sort pins))
                          (remove nil?)
                          (sort-by (fn [e] (get e "coord"))))]
        {"updates" (vec (if breaking-only
                          (filter #(true? (get % "breaking")) updates)
                          updates))}))))
