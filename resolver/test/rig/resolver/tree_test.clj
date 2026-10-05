(ns rig.resolver.tree-test
  (:require [clojure.string :as str]
            [clojure.test :refer :all]
            [rig.resolver.test-util :refer [delete-tree with-ws start-serving repo-metadata! repo-artifacts! m2-dir-of]]
            [rig.resolver.tree :as tree]
            [rig.resolver.versions :as versions])
  (:import [java.io File]))

(deftest tree-basic
  (with-ws [["deps.edn" "{:deps {org.clojure/clojure {:mvn/version \"1.12.5\"}
                              cheshire/cheshire {:mvn/version \"5.10.2\"}}}"]]
    (fn [dir]
      (let [lines (-> (tree/tree {:workspace dir}) (get "tree") str/split-lines)]
        (is (= "." (first lines))
            "root on the first line")
        (is (some #(= % "├── org.clojure/clojure:1.12.5") lines)
            "first top-level dep, in declaration order")
        (is (some #(= % "└── cheshire/cheshire:5.10.2") lines)
            "last top-level dep")
        (is (some #(= % "│   ├── org.clojure/spec.alpha:0.5.238") lines)
            "transitive dep under clojure")
        (is (some #(= % "    │   └── com.fasterxml.jackson.core/jackson-core:2.12.4 (same-version)") lines)
            "a shared dep is shown under each parent, marked, not expanded")
        (is (= 2 (count (filter #(re-find #"\(same-version\)$" %) lines)))
            "…once per parent")))))

(deftest tree-alias
  (with-ws [["deps.edn" "{:deps {org.clojure/clojure {:mvn/version \"1.12.5\"}}
                          :aliases {:extra {:extra-deps {cheshire/cheshire {:mvn/version \"5.10.2\"}}}}}"]]
    (fn [dir]
      (let [text (fn [req] (-> (tree/tree req) (get "tree")))]
        (is (not (str/includes? (text {:workspace dir}) "cheshire/cheshire"))
            "alias dep hidden without the alias")
        (is (str/includes? (text {:workspace dir :args {:alias "extra"}})
                           "cheshire/cheshire:5.10.2")
            "alias dep shown with the alias")))))

(deftest tree-floats-pinned-by-lock
  (with-ws [["deps.edn" "{:deps {org.clojure/clojure {:mvn/version \"RELEASE\"}}}"]
            ["deps.lock" "{\"version\":1,\"artifacts\":[{\"kind\":\"mvn\",\"group\":\"org.clojure\",\"name\":\"clojure\",\"version\":\"1.12.5\"}]}"]]
    (fn [dir]
      (let [text (-> (tree/tree {:workspace dir :lock "deps.lock"}) (get "tree"))]
        (is (str/includes? text "org.clojure/clojure:1.12.5")
            "floating RELEASE pinned to the lock version")))))

(deftest tree-module
  (with-ws [["deps.edn" "{:rig/modules [\"modules/app\"]}"]
            ["modules/app/deps.edn" "{:rig/lib example/app
                                      :rig/version \"0.1.0\"
                                      :deps {org.clojure/clojure {:mvn/version \"1.12.5\"}}}"]]
    (fn [dir]
      (let [lines (-> (tree/tree {:workspace dir :args {:module "modules/app"}})
                      (get "tree") str/split-lines)]
        (is (= "example/app:0.1.0" (first lines))
            "root is the module's lib")
        (is (some #(= % "└── org.clojure/clojure:1.12.5") lines)
            "the module's dep under its root")))))

;; --- Auth proxy: the tree rewrites :auth :oidc repos to the proxy base URL
;; --- (exported by rig as RIG_PROXY_REPOS) so calc-trace resolves through it.
;; --- A local server stands in for rig's loopback proxy.

(deftest tree-floats-via-proxy
  (let [group "rig.ktest.proxy" name "treefloat" v "2.0.0"
        orig-root (doto (File. (str (System/getProperty "java.io.tmpdir") "/"
                                   (str "rig-tree-orig-" (java.util.UUID/randomUUID))))
                    (.mkdirs) (.deleteOnExit))
        proxy-root (doto (File. (str (System/getProperty "java.io.tmpdir") "/"
                                     (str "rig-tree-py-" (java.util.UUID/randomUUID))))
                     (.mkdirs) (.deleteOnExit))
        [orig-base orig-seen orig-srv] (start-serving (str orig-root) "tok-e2e")
        [proxy-base proxy-seen proxy-srv] (start-serving (str proxy-root) nil)
        base-path (repo-artifacts! (str proxy-root) group name v)
        _ (repo-metadata! (str proxy-root) group name v (System/currentTimeMillis))
        m2 (m2-dir-of group name)]
    (delete-tree (str m2))
    (try
      (binding [versions/*proxy-repos* {"oidc" proxy-base}]
        (with-ws [["deps.edn" (str "{:deps {" group "/" name " {:mvn/version \"RELEASE\"}}"
                                   " :mvn/repos {\"oidc\" {:url \"" orig-base "\" :auth :oidc}}}")]]
          (fn [dir]
            (let [resp (tree/tree {:workspace dir
                                   :args {:cooldown {:default "0s"}}})
                  coord (str group "/" name ":" v)]
              (is (str/includes? (get resp "tree") coord)
                  "floating RELEASE resolved through the proxy")
              (is (some #(= (str base-path "/maven-metadata.xml") (nth % 1)) @proxy-seen)
                  "metadata read through the proxy")
              (is (some #(= (str base-path "/" v "/" name "-" v ".jar") (nth % 1)) @proxy-seen)
                  "artifact fetched through the proxy")
              (is (empty? @orig-seen) "zero cooldown: no probe, no original traffic")))))
      (finally
        (.stop orig-srv 0)
        (.stop proxy-srv 0)
        (delete-tree (str m2))
        (delete-tree (str orig-root))
        (delete-tree (str proxy-root))))))
