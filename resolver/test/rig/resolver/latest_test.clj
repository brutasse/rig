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
