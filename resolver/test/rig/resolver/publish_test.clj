(ns rig.resolver.publish-test
  (:require [clojure.java.io :as io]
            [clojure.test :refer :all]
            [rig.resolver.publish :as publish])
  (:import [java.io File]))

(defn- rm!
  [dir]
  (when-let [f (File. dir)]
    (when (.exists f)
      (doseq [c (.listFiles f)] (rm! (.getPath c)))
      (.delete f))))

(defn- with-ws
  [files f]
  (let [dir (File. (System/getProperty "java.io.tmpdir")
                   (str "rig-publish-test-" (java.util.UUID/randomUUID)))]
    (try
      (do (.mkdirs dir)
          (doseq [[rel text] files]
            (io/make-parents (io/file dir rel))
            (spit (io/file dir rel) text))
          (f (str dir)))
      (finally (rm! (str dir))))))

(defn- entry [resp] (first (get resp "published")))

(defmacro throws?
  "Passes when expr throws an exception with a message matching re."
  [re & expr]
  `(try
     (do ~@expr
         (is false (str "expected an exception matching " '~re)))
     (catch Exception e#
       (is (re-find ~re (.getMessage e#))
           (str "threw with the wrong message: " (.getMessage e#))))))


(deftest publish-basic
  (with-ws [["deps.edn" "{:rig/modules [\"modules/app\"]}"]
            ["modules/app/deps.edn"
             "{:rig/lib example/app
               :rig/version \"1.0.0\"
               :rig/publish? true
               :rig/publish {:repo \"clojars\"}
               :deps {org.clojure/clojure \"1.11.0\"
                      com.example/thing {:mvn/version \"2.0.0\"}}}"]]
    (fn [dir]
      (let [resp (publish/publish {:workspace dir :args {:module "modules/app"}})
            e (entry resp)]
        (is (= "example/app" (get e "coord")))
        (is (= "1.0.0" (get e "version")))
        (is (= "clojars" (get e "repo")))
        (is (= "https://clojars.org/repo" (get e "url")))
        (is (every? #(re-find % (get e "pom"))
                    [#"<groupId>example</groupId>"
                     #"<artifactId>app</artifactId>"
                     #"<version>1.0.0</version>"]))
        (is (re-find #"(?s)<groupId>org.clojure</groupId>.*?<artifactId>clojure</artifactId>.*?<version>1.11.0</version>"
                     (get e "pom"))
            "string dep in the pom")
        (is (re-find #"(?s)<groupId>com.example</groupId>.*?<artifactId>thing</artifactId>.*?<version>2.0.0</version>"
                     (get e "pom"))
            "map dep in the pom")))))

(deftest publish-floats-pinned-by-lock
  (with-ws [["deps.edn" "{:rig/modules [\"modules/app\"]}"]
            ["modules/app/deps.edn"
             "{:rig/lib example/app
               :rig/version \"1.0.0\"
               :deps {com.example/thing {:mvn/version \"RELEASE\"}}}"]
            ["deps.lock"
             "{\"version\":1,\"artifacts\":[{\"kind\":\"mvn\",\"group\":\"com.example\",\"name\":\"thing\",\"version\":\"2.5.0\"}]}"]]
    (fn [dir]
      (let [resp (publish/publish {:workspace dir
                                   :lock "deps.lock"
                                   :args {:module "modules/app"}})]
        (is (re-find #"<version>2.5.0</version>" (get (entry resp) "pom"))
            "floating RELEASE pinned to the lock version")))))

(deftest publish-repo-url-from-mvn-repos
  (with-ws [["deps.edn" "{:rig/modules [\"modules/app\"]}"]
            ["modules/app/deps.edn"
             "{:rig/lib example/app
               :rig/version \"1.0.0\"
               :rig/publish? true
               :rig/publish {:repo \"corp\"}
               :mvn/repos {\"corp\" {:url \"https://artifacts.example.test\"}}}"]]
    (fn [dir]
      (let [resp (publish/publish {:workspace dir :args {:module "modules/app"}})]
        (is (= "corp" (get (entry resp) "repo")))
        (is (= "https://artifacts.example.test" (get (entry resp) "url")))))))

(deftest publish-root-repo-fallback
  (with-ws [["deps.edn" "{:rig/modules [\"modules/app\"]
                          :mvn/repos {\"corp\" {:url \"https://artifacts.example.test\"}}}"]
            ["modules/app/deps.edn"
             "{:rig/lib example/app
               :rig/version \"1.0.0\"
               :rig/publish? true
               :rig/publish {:repo \"corp\"}}"]]
    (fn [dir]
      (let [resp (publish/publish {:workspace dir :args {:module "modules/app"}})]
        (is (= "https://artifacts.example.test" (get (entry resp) "url")))
        (is (re-find #"<project" (get (entry resp) "pom"))
            "pom rendered even with no deps")
        (is (nil? (re-find #"<dependencies>" (get (entry resp) "pom")))
            "no <dependencies> section without deps")))))

(deftest publish-version-from-file
  (with-ws [["deps.edn" "{:rig/modules [\"modules/app\"]}"]
            ["VERSION" "3.2.1"]
            ["modules/app/deps.edn" "{:rig/lib example/app}"]]
    (fn [dir]
      (let [resp (publish/publish {:workspace dir :args {:module "modules/app"}})]
        (is (= "3.2.1" (get (entry resp) "version")))))))

(deftest publish-root-module
  (with-ws [["deps.edn" "{:rig/lib example/root :rig/version \"0.9.0\"}"]]
    (fn [dir]
      (let [resp (publish/publish {:workspace dir :args {:module "."}})]
        (is (= "example/root" (get (entry resp) "coord")))
        (is (= "0.9.0" (get (entry resp) "version")))))))

(deftest publish-no-lib
  (with-ws [["deps.edn" "{:rig/version \"1.0.0\"}"]]
    (fn [dir]
      (throws? #".*has no :rig/lib coordinate.*"
               (publish/publish {:workspace dir})))))

(deftest publish-no-version
  (with-ws [["deps.edn" "{:rig/lib example/app}"]]
    (fn [dir]
      (throws? #".*has no version.*"
               (publish/publish {:workspace dir})))))

(deftest publish-unknown-repo
  (with-ws [["deps.edn"
             "{:rig/lib example/app
               :rig/version \"1.0.0\"
               :rig/publish? true
               :rig/publish {:repo \"nope\"}}"]]
    (fn [dir]
      (throws? #".*unknown repository.*"
               (publish/publish {:workspace dir})))))

(deftest publish-rejects-legacy-exec-args
  (with-ws [["deps.edn"
             "{:rig/lib example/app
               :rig/version \"1.0.0\"
               :slipset.deps-deploy/exec-args {:repository {:releases {:url \"https://x\"}}}}"]]
    (fn [dir]
      (throws? #".*still uses legacy keys.*"
               (publish/publish {:workspace dir})))))

(deftest publish-signing-not-supported
  (with-ws [["deps.edn"
             "{:rig/lib example/app
               :rig/version \"1.0.0\"
               :rig/publish? true
                :rig/publish {:repo \"clojars\" :sign-releases? true}}"]]
    (fn [dir]
      (throws? #".*:sign-releases\? is not supported.*"
               (publish/publish {:workspace dir})))))
