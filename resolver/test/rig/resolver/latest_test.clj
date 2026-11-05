(ns rig.resolver.latest-test
  (:require [clojure.java.io :as io]
            [clojure.test :refer :all]
            [rig.resolver.latest :as latest]
            [rig.resolver.test-util :refer [delete-tree]]
            [rig.resolver.versions-test :as vtest])
  (:import [java.io File]))

(defn- with-ws
  "Runs f in a workspace whose root manifest is manifest (a workspace with
  src/ and nothing else when omitted)."
  ([f] (with-ws "{:paths [\"src\"]}\n" f))
  ([manifest f]
   (let [dir (File. (System/getProperty "java.io.tmpdir")
                    (str "rig-latest-test-" (java.util.UUID/randomUUID)))]
     (try
       (do (.mkdirs (io/file dir "src"))
           (spit (io/file dir "deps.edn") manifest)
           (f (str dir)))
       (finally (delete-tree (str dir)))))))

(defn- old-and-recent
  [now]
  [(- now (* 100 3600 1000)) (- now (* 1 3600 1000))])

(defn- file-repos
  "The args :repos entry pointing at a file:// repo serving the coord."
  [url]
  {"f" {:url url}})

(deftest newest-release-past-the-cooldown-wins
  (let [now (System/currentTimeMillis)
        [old recent] (old-and-recent now)]
    (vtest/with-file-repo ["com.rig.test" "runner" {"1.0.0" old
                                                    "1.0.1" old
                                                    "1.0.2" recent}]
      (fn [url]
        (with-ws
          (fn [dir]
            (is (= {"version" "1.0.1"}
                   (latest/latest {:workspace dir
                                   :args {:coord "com.rig.test/runner"
                                          :repos (file-repos url)}}))
                "the newest release older than the cooldown, not the newest")))))))

(deftest all-releases-inside-the-cooldown-refuse
  (let [now (System/currentTimeMillis)
        [_ recent] (old-and-recent now)]
    (vtest/with-file-repo ["com.rig.test" "runner" {"1.0.0" recent
                                                    "1.0.1" recent}]
      (fn [url]
        (with-ws
          (fn [dir]
            (is (= {"version" nil "reason" "cooldown" "cooldown" "48h"}
                   (latest/latest {:workspace dir
                                   :args {:coord "com.rig.test/runner"
                                          :repos (file-repos url)}})))))))))

(deftest cooldown-policy-of-the-workspace-applies
  (let [now (System/currentTimeMillis)
        two-hours (- now (* 2 3600 1000))]
    (vtest/with-file-repo ["com.rig.test" "runner" {"1.0.0" two-hours}]
      (fn [url]
        (with-ws "{:paths [\"src\"] :rig/cooldown \"1h\"}\n"
          (fn [dir]
            (is (= {"version" "1.0.0"}
                   (latest/latest {:workspace dir
                                   :args {:coord "com.rig.test/runner"
                                          :repos (file-repos url)}}))
                "a 1h policy admits a two-hour-old release the 48h default would refuse")))))))

(deftest coord-served-by-no-repo-is-not-found
  (vtest/with-file-repo ["com.rig.test" "other" {"1.0.0" 1}]
    (fn [url]
      (with-ws
        (fn [dir]
          (let [resp (latest/latest {:workspace dir
                                     :args {:coord "com.rig.test/missing"
                                            :repos (file-repos url)}})]
            (is (contains? #{"not-found" "cooldown"} (get resp "reason"))
                "a coordinate no repo serves answers nothing")))))))

(deftest coord-argument-is-required
  (with-ws
    (fn [dir]
      (is (thrown-with-msg? Exception #"coord"
                            (latest/latest {:workspace dir :args {}}))))))

(deftest snapshots-are-never-picked-without-force
  (let [now (System/currentTimeMillis)
        [old recent] (old-and-recent now)]
    (vtest/with-file-repo ["com.rig.test" "snap" {"1.0.0" old
                                                  "1.1.0-SNAPSHOT" recent}]
      (fn [url]
        (with-ws
          (fn [dir]
            (is (= {"version" "1.0.0"}
                   (latest/latest {:workspace dir
                                   :args {:coord "com.rig.test/snap"
                                          :repos (file-repos url)}}))
                "the newest version is a snapshot: the newest release wins")
            (is (= {"version" "1.1.0-SNAPSHOT"}
                   (latest/latest {:workspace dir
                                   :args {:coord "com.rig.test/snap"
                                          :repos (file-repos url)
                                          :force true}}))
                "force admits the snapshot")))))))

(deftest a-snapshot-only-repo-is-not-found
  (let [recent (- (System/currentTimeMillis) (* 1 3600 1000))]
    (vtest/with-file-repo ["com.rig.test" "snaps" {"1.0.0-SNAPSHOT" recent}]
      (fn [url]
        (with-ws
          (fn [dir]
            (let [resp (latest/latest {:workspace dir
                                       :args {:coord "com.rig.test/snaps"
                                              :repos (file-repos url)}})]
              (is (= "not-found" (get resp "reason"))
                  "no release is served: not the snapshot, not anything"))))))))

(deftest a-coords-batch-resolves-each-coord
  (let [now (System/currentTimeMillis)
        [old recent] (old-and-recent now)]
    (vtest/with-file-repo ["com.rig.test" "runner" {"1.0.0" old
                                                    "1.0.1" old
                                                    "1.0.2" recent}]
      (fn [url-f]
        (vtest/with-file-repo ["com.rig.test" "other" {"2.0.0" old}]
          (fn [url-g]
            (with-ws
              (fn [dir]
                (is (= {"versions" {"com.rig.test/runner" "1.0.1"}
                        "reasons" {"com.rig.test/other" "cooldown"
                                   "com.rig.test/missing" "not-found"}}
                       (latest/latest {:workspace dir
                                       :args {:coords ["com.rig.test/runner"
                                                       "com.rig.test/other"
                                                       "com.rig.test/missing"]
                                              :repos {"f" {:url url-f}
                                                      "g" {:url url-g}}}}))
                    "resolved coordinates in versions, the rest with their
                    reasons: the repo id g serves no per-version timestamps,
                    so its release reads as published now, inside the
                    cooldown")))))))))

(deftest coord-and-coords-are-mutually-exclusive
  (with-ws
    (fn [dir]
      (is (thrown-with-msg? Exception #"mutually exclusive"
                            (latest/latest {:workspace dir
                                            :args {:coord "a/b"
                                                   :coords ["c/d"]}}))))))
