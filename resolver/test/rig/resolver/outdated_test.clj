(ns rig.resolver.outdated-test
  (:require [clojure.java.io :as io]
            [clojure.test :refer :all]
            [rig.resolver.outdated :as outdated])
  (:import [java.io File]))

(defn- rm!
  [dir]
  (when-let [f (File. dir)]
    (when (.exists f)
      (doseq [c (.listFiles f)] (rm! (.getPath c)))
      (.delete f))))

(deftest update-entry-up-to-date
  (let [md {:by-version {"1.2.4" {:published-at nil :repo "central"}} :latest #{}}]
    (is (nil? (outdated/update-entry ["g/n" "1.2.4"] md))
        "no entry when current is already the newest")))

(deftest update-entry-non-breaking
  (let [md {:by-version {"1.2.4" {} "1.3.1" {}} :latest #{}}]
    (is (= {"coord" "g/n" "current" "1.2.4"
            "latest-satisfying" "1.3.1" "latest" "1.3.1" "breaking" false}
           (outdated/update-entry ["g/n" "1.2.4"] md)))))

(deftest update-entry-breaking
  (let [md {:by-version {"1.2.4" {} "1.3.1" {} "2.0.0" {}} :latest #{}}]
    (is (= {"coord" "g/n" "current" "1.2.4"
            "latest-satisfying" "1.3.1" "latest" "2.0.0" "breaking" true}
           (outdated/update-entry ["g/n" "1.2.4"] md)))))

(deftest update-entry-only-breaking-available
  (let [md {:by-version {"1.2.4" {} "2.0.0" {}} :latest #{}}]
    (is (= {"coord" "g/n" "current" "1.2.4"
            "latest-satisfying" "1.2.4" "latest" "2.0.0" "breaking" true}
           (outdated/update-entry ["g/n" "1.2.4"] md))
        "latest-satisfying falls back to current when no same-major bump")))

(deftest update-entry-non-semver
  (let [md {:by-version {"alpha1" {} "alpha2" {}} :latest #{}}]
    (is (= {"coord" "g/n" "current" "alpha1"
            "latest-satisfying" "alpha2" "latest" "alpha2" "breaking" false}
           (outdated/update-entry ["g/n" "alpha1"] md))
        "non-numeric versions are never breaking")))

(deftest update-entry-latest-tag-preferred
  (let [md {:by-version {"1.0" {} "1.1" {}} :latest #{"1.1"}}]
    (is (= "1.1" (get (outdated/update-entry ["g/n" "1.0"] md) "latest"))
        "cands follow the :latest tag when present, like select-latest")))

(deftest outdated-empty-lock
  (let [dir (io/file (System/getProperty "java.io.tmpdir")
                     (str "rig-outdated-test-" (java.util.UUID/randomUUID)))]
    (try
      (do (.mkdirs (io/file dir "src"))
          (spit (io/file dir "deps.edn") "{:paths [\"src\"]}\n")
          (is (= {"updates" []}
                 (outdated/outdated {:workspace (str dir) :args {}}))
              "no pins -> no updates, no network"))
      (finally (rm! (str dir))))))

(deftest outdated-offline-with-pins-throws
  (let [dir (io/file (System/getProperty "java.io.tmpdir")
                     (str "rig-outdated-test-" (java.util.UUID/randomUUID)))]
    (try
      (do (.mkdirs (io/file dir "src"))
          (spit (io/file dir "deps.edn") "{:paths [\"src\"]}\n")
          (spit (io/file dir "deps.lock")
                "{\"version\":1,\"artifacts\":[{\"kind\":\"mvn\",\"group\":\"g\",\"name\":\"n\",\"version\":\"1.0.0\"}]}")
          (is (thrown-with-msg? Exception #".*network.*"
              (outdated/outdated {:workspace (str dir)
                                  :lock "deps.lock"
                                  :args {:offline true}}))))
      (finally (rm! (str dir))))))
