(ns rig.resolver.plan
  (:require [clojure.string :as str]))

(defn- m2-parse
  [m2dir entry]
  (let [prefix (str m2dir "/")]
    (when (str/starts-with? entry prefix)
      (let [rel (subs entry (count prefix))
            parts (str/split rel #"/")
            n (count parts)]
        (when (>= n 4)
          (let [filename (nth parts (dec n))
                version (nth parts (- n 2))
                name (nth parts (- n 3))
                group (str/join "." (subvec parts 0 (- n 3)))
                ext (last (str/split filename #"\."))
                noext (subs filename 0 (- (count filename) (inc (count ext))))
                base (str name "-" version)
                classifier (when (and (not= noext base)
                                      (str/starts-with? noext (str base "-")))
                             (subs noext (inc (count base))))]
            {:id (str group "/" name ":" version
                      (when classifier (str ":" classifier))
                      ":" ext)
             :kind "mvn"
             :group group
             :name name
             :version version
             :extension ext
             :classifier classifier}))))))

(defn- git-parse
  [gitlibs entry]
  (let [prefix (str gitlibs "/")]
    (when (str/starts-with? entry prefix)
      (let [rel (subs entry (count prefix))
            parts (str/split rel #"/")
            full-sha (get parts 2)]
        (when (and (>= (count parts) 3) (re-matches #"^[0-9a-f]{40}$" full-sha))
          (let [group (get parts 0)
                name (get parts 1)
                short (subs full-sha 0 7)]
            {:id (str group "/" name ":" short ":jar")
             :kind "git"
             :group group
             :name name
             :version short
             :extension "jar"
             :classifier nil
             :git {:url nil :sha full-sha}
             :deps/root nil
             :paths [(str/join "/" (drop 3 parts))]}))))))

(defn- local-module
  [modules-abs entry]
  (let [m (reduce (fn [best [rel abs]]
                    (if (or (= entry abs)
                            (str/starts-with? entry (str abs "/")))
                      (if (and best (> (count (second best)) (count abs)))
                        best
                        [rel abs])
                      best))
                  nil modules-abs)]
    (some-> m first)))

(defn map-classpath
  [roots modules-abs m2dir gitlibs]
  (reduce (fn [acc e]
            (cond
              (not (str/starts-with? e "/"))
              (update acc :paths (fnil conj []) e)

              (some? (m2-parse m2dir e))
              (let [a (m2-parse m2dir e)]
                (-> acc
                    (update :classpath (fnil conj []) (:id a))
                    (update :artifacts (fnil conj []) a)))

              (some? (git-parse gitlibs e))
              (let [g (git-parse gitlibs e)]
                (if (some #(= (:id g) %) (:classpath acc))
                  (update acc :artifacts (fnil conj []) g)
                  (-> acc
                      (update :classpath (fnil conj []) (:id g))
                      (update :artifacts (fnil conj []) g))))

              (some? (local-module modules-abs e))
              (let [m (local-module modules-abs e)]
                (if (some #(and (map? %) (= m (get % "local"))) (:classpath acc))
                  acc
                  (update acc :classpath (fnil conj []) {"local" m})))

              :else
              (throw (ex-info
                     (str "unclassified classpath root " e
                          " — not an artifact, git dep, or workspace module; "
                          "declare it under :rig/modules or :deps")
                     {:path e}))))
          {:classpath [] :paths [] :artifacts []}
          roots))

(defn merge-artifacts
  [artifacts git-deps]
  (for [[_id group] (group-by :id artifacts)]
    (let [a (first group)
          paths (vec (distinct (mapcat :paths group)))
          m (merge a (when (seq paths) {:paths paths}))]
      (if (= "git" (:kind m))
        (let [key (str (:group m) "/" (:name m) "/" (get-in m [:git :sha]))
              d (get git-deps key)]
          (-> m
              (assoc-in [:git :url] (or (get-in m [:git :url]) (get d :url)))
              (assoc :deps/root (or (:deps/root m) (get d :deps/root)))))
        m))))
