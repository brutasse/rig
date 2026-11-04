(ns rig.resolver.main-test
  (:require [cheshire.core :as json]
            [clojure.java.io :as io]
            [clojure.string :as str]
            [clojure.test :refer :all]
            [rig.resolver.main :as main]
            [rig.resolver.test-util :refer [delete-tree git with-ws]]
            [rig.resolver.versions :as versions]))

(deftest dispatch-success-path
  ;; Guards the with-quiet-out/with-request-env nesting: failure paths alone
  ;; cannot catch it (the thunk is only forced on success).
  (with-ws [["deps.edn" "{:deps {org.clojure/clojure {:mvn/version \"1.12.5\"}}}"]]
    (fn [dir]
      (let [out (with-out-str
                  (is (= 0 (main/dispatch {:op "tree" :workspace dir}))))
            result (json/parse-string out false)]
        (is (string? (get result "tree")) "result JSON is the stdout printout")))))

(deftest dispatch-codes
  (testing "unknown op is a bad request: 2"
    (is (= 2 (main/dispatch {:op "nope"}))))
  (testing "op failures return nonzero, they never throw out"
    ;; :check/:tree tolerate odd workspaces (they legitimately succeed);
    ;; :migrate without a manifest always throws.
    (is (pos? (main/dispatch {:op "migrate" :workspace "/nonexistent-rig-test"}))))
  (testing "a manifest using a removed key is a 4"
    (with-ws [["deps.edn" "{:rig/lib x/y :rig/ns-compile [a.b]}"]]
      (fn [dir]
        (is (= 4 (main/dispatch {:op "tree" :workspace dir})))))))

(deftest git-procure-dispatch-codes
  "git-procure over dispatch: a malformed request is 2, a provisionable
  commit is 0 with the ok document, an unknown commit is 1 with the
  friendly message on stderr."
  (let [repo (doto (java.io.File.
                    (str (java.nio.file.Files/createTempDirectory "rig-main-git"
                                                                  (into-array java.nio.file.attribute.FileAttribute []))))
               (.deleteOnExit))
        _ (git (str repo) "init")
        _ (spit (io/file repo "file.txt") "x\n")
        _ (git (str repo) "add" "-A")
        _ (git (str repo) "commit" "-m" "x")
        sha (git (str repo) "rev-parse" "HEAD")
        lib (str "rig.maintest/" (java.util.UUID/randomUUID))
        checkout (str (System/getenv "HOME") "/.gitlibs/libs/rig.maintest/" (last (str/split lib #"/")))]
    (try
      (is (= 2 (main/dispatch {:op "git-procure"
                               :workspace "/tmp"
                               :args {:lib lib :sha sha}}))
          "no url is a bad request")
      (let [old-err (System/err)
            err-ba (java.io.ByteArrayOutputStream.)]
        (System/setErr (java.io.PrintStream. err-ba))
        (try
          (is (= 1 (main/dispatch {:op "git-procure"
                                   :workspace "/tmp"
                                   :args {:url (str "file://" repo)
                                          :lib lib
                                          :sha "b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1"}}))
              "an unknown commit is a friendly op failure")
          (is (str/includes? (.toString err-ba) "not found in")
              (.toString err-ba))
          (finally (System/setErr old-err))))
      (let [out (with-out-str
                  (is (= 0 (main/dispatch {:op "git-procure"
                                           :workspace "/tmp"
                                           :args {:url (str "file://" repo)
                                                  :lib lib
                                                  :sha sha}}))))]
        (is (= {"ok" true} (json/parse-string out false))
            (str "stdout: " out)))
      (finally
        (delete-tree checkout)
        (delete-tree (str repo))))))

(deftest run-request-file-codes
  (is (= 2 (main/run-request-file "/nonexistent/rig-request.json")))
  (let [f (java.io.File/createTempFile "rig-req" ".json")]
    (spit f "{ not json")
    (is (= 2 (main/run-request-file (.getPath f))))
    (.delete f))
  (let [f (java.io.File/createTempFile "rig-req" ".json")]
    (spit f "{\"op\":\"nope\"}")
    (is (= 2 (main/run-request-file (.getPath f))))
    (.delete f)))

(defn- leaked-heap-env
  "Simulate a restored JVM: dynvars pre-bound to the bootstrap run's values
  (what the boot-cached System/getenv would serve after a restore)."
  [f]
  (binding [versions/*oidc-tokens* {:bootstrap "leaked-token"}
            versions/*proxy-repos* {:bootstrap "http://bootstrap"}]
    (f)))

(deftest with-request-env-authoritative
  (testing "an entry-less restore request binds config ABSENT, never the
  bootstrap heap's values (a tokenless workspace must not run under the
  image donor's credentials)"
    (is (= {:via :called}
           (leaked-heap-env
            #(main/with-request-env {:op "tree"} true
               (fn []
                 (is (nil? @#'versions/*oidc-tokens*))
                 (is (= {} (versions/proxy-map)))
                 {:via :called})))))))

(deftest with-request-env-overrides
  (let [req {:op "tree"
             :env {:RIG_REPO_TOKENS "{:repo \"fresh\"}"
                   :RIG_PROXY_REPOS "{:repo \"http://fresh\"}"}}]
    (testing "request entries win over anything cached"
      (let [[tok prx] (leaked-heap-env
                       #(main/with-request-env req false
                          (fn [] [@#'versions/*oidc-tokens* (versions/proxy-map)])))]
        (is (= {:repo "fresh"} tok))
        (is (= {:repo "http://fresh"} prx))))
    (testing "cold-style (non-authoritative) absent entries fall back"
      (is (= {:bootstrap "leaked-token"}
             (leaked-heap-env
              #(main/with-request-env {:op "tree"} false
                 (fn [] @#'versions/*oidc-tokens*))))))))
