(ns rig.resolver.main-test
  (:require [cheshire.core :as json]
            [clojure.test :refer :all]
            [rig.resolver.main :as main]
            [rig.resolver.test-util :refer [with-ws]]
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
    (is (pos? (main/dispatch {:op "migrate" :workspace "/nonexistent-rig-test"})))))

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
