(ns rig.resolver.check-test
  (:require [clojure.java.io :as io]
            [clojure.test :refer :all]
            [rig.resolver.check :as check]))

(defn- temp-ws
  "Create a workspace with root deps.edn text root-text and one deps.edn per
  {module-dir text}. Returns the workspace path as a string."
  [root-text module-files]
  (let [ws (doto (java.io.File.
                  (str (java.nio.file.Files/createTempDirectory "rig-check-test"
                                                                (into-array java.nio.file.attribute.FileAttribute []))))
               (.deleteOnExit))]
    (spit (io/file ws "deps.edn") root-text)
    (doseq [[m text] module-files]
      (let [f (io/file ws m "deps.edn")]
        (io/make-parents f)
        (spit f text)))
    (str ws)))

(defn- lock-with
  "A minimal lock. `artifacts` become `:artifacts`; `pins` is
  {module-dir {coord version}} and becomes that module's `:modules`
  classpath (entries \"coord:version:jar\"). The root plan (\".\") always
  has an empty classpath."
  [artifacts pins]
  {:artifacts (mapv (partial into {}) artifacts)
   :modules (into {"." {:classpath []}}
                   (for [[m c2v] pins]
                     [m {:classpath (mapv (fn [[c v]] (str c ":" v ":jar"))
                                          (seq c2v))}]))})

(deftest clean-workspace-is-ok
  (let [ws (temp-ws
            "{:rig/modules [\"modules/a\"]\n :rig/deps {org.clojure/test.check {:mvn/version \"1.1.0\"}}}\n"
            {"modules/a" "{:deps {org.clojure/test.check {:mvn/version \"1.1.0\"}}}\n"})
         res (check/check {:workspace ws
                           :lock (lock-with
                                  [{:kind "mvn" :group "org.clojure" :name "test.check"
                                    :version "1.1.0" :repository "clojars"}]
                                  {"modules/a" {"org.clojure/test.check" "1.1.0"}})})]
     (is (true? (get res :ok)))
     (is (empty? (get res :problems)))))

(deftest stale-lock-reports-requirement-vs-pin
  (let [ws (temp-ws
            "{:rig/modules [\"modules/a\"]}\n"
            {"modules/a" "{:deps {org.clojure/test.check {:mvn/version \"1.2.0\"}}}\n"})
        res (check/check {:workspace ws
                          :lock (lock-with
                                 [{:kind "mvn" :group "org.clojure" :name "test.check"
                                   :version "1.1.0"}]
                                 {"modules/a" {"org.clojure/test.check" "1.1.0"}})})]
     (is (false? (get res :ok)))
     (is (some #(and (= "stale-lock" (:kind %))
                    (= "error" (:severity %))
                    (= "modules/a" (:module %))
                    (= "manifest requires \"1.2.0\"; lock pins \"1.1.0\" — run rig update"
                       (:message %)))
              (get res :problems)))))

(deftest stale-lock-without-pin-hints-rig-lock
  (let [ws (temp-ws
            "{:rig/modules [\"modules/a\"]}\n"
            {"modules/a" "{:deps {com.example/shared {:mvn/version \"1.0.0\"}}}\n"})
         res (check/check {:workspace ws :lock (lock-with [] {})})]
     (is (some #(and (= "stale-lock" (:kind %))
                     (= "manifest requires \"1.0.0\"; lock has no pin — run rig lock"
                        (:message %)))
               (get res :problems)))))

(deftest floating-version-release-is-an-error
  (let [ws (temp-ws
            "{:rig/modules [\"modules/a\"]}\n"
            {"modules/a" "{:aliases {:test {:extra-deps
                                            {org.clojure/test.check {:mvn/version \"RELEASE\"}}}}}\n"})
        res (check/check {:workspace ws
                          :lock (lock-with
                                 [{:kind "mvn" :group "org.clojure" :name "test.check"
                                   :version "1.1.0"}]
                                 {})})]
     (is (false? (get res :ok)))
     (is (some #(and (= "floating-version" (:kind %))
                    (= "manifest uses \"RELEASE\"; run `rig update org.clojure/test.check` to pin an exact version"
                       (:message %)))
              (get res :problems)))))

(deftest drift-reports-module-vs-workspace
  (let [ws (temp-ws
            "{:rig/modules [\"modules/a\"]\n :rig/deps {org.clojure/test.check {:mvn/version \"1.1.0\"}}}\n"
            {"modules/a" "{:deps {org.clojure/test.check {:mvn/version \"1.1.1\"}}}\n"})
         res (check/check {:workspace ws
                           :lock (lock-with
                                  [{:kind "mvn" :group "org.clojure" :name "test.check"
                                    :version "1.1.0"}]
                                  {"modules/a" {"org.clojure/test.check" "1.1.1"}})})]
    (is (some #(and (= "drift" (:kind %))
                    (= "warn" (:severity %))
                    (= "module requires \"1.1.1\", workspace requires \"1.1.0\""
                       (:message %)))
              (get res :problems)))))

(deftest drift-reports-module-vs-workspace-bare-string-rig-deps
  (let [ws (temp-ws
            "{:rig/modules [\"modules/a\"]\n :rig/deps {org.clojure/test.check \"1.1.0\"}}\n"
            {"modules/a" "{:deps {org.clojure/test.check {:mvn/version \"1.1.1\"}}}\n"})
         res (check/check {:workspace ws
                           :lock (lock-with
                                  [{:kind "mvn" :group "org.clojure" :name "test.check"
                                    :version "1.1.0"}]
                                  {"modules/a" {"org.clojure/test.check" "1.1.1"}})})]
    (is (some #(and (= "drift" (:kind %))
                    (= "warn" (:severity %))
                    (= "module requires \"1.1.1\", workspace requires \"1.1.0\""
                       (:message %)))
              (get res :problems)))))

(deftest conflict-reports-incompatible-modules
  (let [ws (temp-ws
            "{:rig/modules [\"modules/a\" \"modules/b\"]}\n"
            {"modules/a" "{:deps {org.clojure/test.check {:mvn/version \"1.1.0\"}}}\n"
             "modules/b" "{:deps {org.clojure/test.check {:mvn/version \"2.0.0\"}}}\n"})
          res (check/check {:workspace ws
                            :lock (lock-with
                                   [{:kind "mvn" :group "org.clojure" :name "test.check"
                                     :version "1.1.0"}]
                                   {"modules/a" {"org.clojure/test.check" "1.1.0"}
                                    "modules/b" {"org.clojure/test.check" "2.0.0"}})})]
     (is (some #(and (= "conflict" (:kind %))
                    (= "error" (:severity %))
                    (re-find #"requirements \"1.1.0\" \(modules/a\) and \"2.0.0\" \(modules/b\) are incompatible"
                             (:message %)))
              (get res :problems)))))

(deftest range-requirement-satisfied-by-pin
  (let [ws (temp-ws
            "{:rig/modules [\"modules/a\"]}\n"
            {"modules/a" "{:deps {org.clojure/test.check {:mvn/version \"[1.1.0,)\"}}}\n"})
         res (check/check {:workspace ws
                           :lock (lock-with
                                  [{:kind "mvn" :group "org.clojure" :name "test.check"
                                    :version "1.1.5"}]
                                  {"modules/a" {"org.clojure/test.check" "1.1.5"}})})]
     (is (true? (get res :ok)))
     (is (empty? (get res :problems)))))

(deftest publish-without-lib-is-no-lib
  (let [ws (temp-ws
            "{:rig/modules [\"modules/a\"]}\n"
            {"modules/a" "{:rig/publish {:repo \"clojars\"}}\n"})
         res (check/check {:workspace ws :lock (lock-with [] {})})]
     (is (some #(= "no-lib" (:kind %)) (get res :problems)))))

(deftest unknown-repo-in-lock-is-flagged
  (let [ws (temp-ws
            "{:rig/modules [\"modules/a\"]}\n"
            {"modules/a" "{:deps {org.clojure/test.check {:mvn/version \"1.1.0\"}}}\n"})
         res (check/check {:workspace ws
                           :lock (lock-with
                                  [{:kind "mvn" :group "org.clojure" :name "test.check"
                                    :version "1.1.0" :repository "nexus-internal"}]
                                  {"modules/a" {"org.clojure/test.check" "1.1.0"}})})]
     (is (some #(and (= "unknown-repo" (:kind %))
                     (re-find #"nexus-internal" (:message %)))
               (get res :problems)))))

(deftest declared-repo-in-manifest-is-not-flagged
  (let [ws (temp-ws
            "{:rig/modules [\"modules/a\"]\n :mvn/repos {\"nexus-internal\" {:url \"https://x\"}}}\n"
            {"modules/a" "{:deps {org.clojure/test.check {:mvn/version \"1.1.0\"}}}\n"})
         res (check/check {:workspace ws
                           :lock (lock-with
                                  [{:kind "mvn" :group "org.clojure" :name "test.check"
                                    :version "1.1.0" :repository "nexus-internal"}]
                                  {"modules/a" {"org.clojure/test.check" "1.1.0"}})})]
     (is (true? (get res :ok)))
     (is (nil? (some #(= "unknown-repo" (:kind %)) (get res :problems))))))

(deftest stale-lock-handles-keyified-module-keys
  (let [ws (temp-ws
            "{:rig/modules [\"modules/a\"]}\n"
            {"modules/a" "{:deps {org.clojure/test.check {:mvn/version \"1.2.0\"}}}\n"})
        res (check/check {:workspace ws
                          :lock {:artifacts []
                                 :modules {:modules/a {:classpath
                                                       ["org.clojure/test.check:1.1.0:jar"]}}}})]
    (is (some #(and (= "stale-lock" (:kind %))
                    (= "modules/a" (:module %))
                    (= "manifest requires \"1.2.0\"; lock pins \"1.1.0\" — run rig update"
                       (:message %)))
              (get res :problems)))))

(deftest stale-lock-checks-module-own-classpath
  (let [ws (temp-ws
            "{:rig/modules [\"modules/a\" \"modules/b\"]}\n"
            {"modules/a" "{:deps {org.clojure/test.check {:mvn/version \"1.0.0\"}}}\n"
             "modules/b" "{:deps {org.clojure/test.check {:mvn/version \"1.0.0\"}}}\n"})
        res (check/check {:workspace ws
                          :lock (lock-with []
                                         {"modules/a" {"org.clojure/test.check" "1.1.0"}
                                          "modules/b" {"org.clojure/test.check" "1.0.0"}})})]
    (is (some #(and (= "stale-lock" (:kind %))
                    (= "modules/a" (:module %))
                    (= "manifest requires \"1.0.0\"; lock pins \"1.1.0\" — run rig update"
                       (:message %)))
              (get res :problems)))
    (is (nil? (some #(and (= "stale-lock" (:kind %))
                          (= "modules/b" (:module %)))
                    (get res :problems))))))

(deftest workspace-deps-pinned-by-a-module-classpath
  (let [ws (temp-ws
            "{:rig/modules [\"modules/a\"]\n :rig/deps {org.clojure/test.check {:mvn/version \"1.0.0\"}}}\n"
            {"modules/a" "{:deps {some/lib/other {:mvn/version \"1.0.0\"}}}\n"})
        res (check/check {:workspace ws
                          :lock (lock-with []
                                         {"modules/a" {"org.clojure/test.check" "1.0.0"
                                                       "some/lib/other" "1.0.0"}})})]
    (is (true? (get res :ok)))
    (is (empty? (get res :problems)))))

(deftest workspace-deps-not-pinned-at-declared-version
  (let [ws (temp-ws
            "{:rig/modules [\"modules/a\"]\n :rig/deps {org.clojure/test.check {:mvn/version \"1.0.0\"}}}\n"
            {"modules/a" "{:deps {some/lib/other {:mvn/version \"1.0.0\"}}}\n"})
        res (check/check {:workspace ws
                          :lock (lock-with []
                                         {"modules/a" {"org.clojure/test.check" "2.0.0"
                                                       "some/lib/other" "1.0.0"}})})]
    (is (some #(and (= "stale-lock" (:kind %))
                    (= "." (:module %))
                    (= "workspace requires \"1.0.0\"; lock pins \"2.0.0\" — run rig update"
                       (:message %)))
              (get res :problems)))))
