(ns rig.resolver.tree-test
  (:require [clojure.java.io :as io]
            [clojure.string :as str]
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
        (is (= 3 (count (filter #(str/ends-with? % "com.fasterxml.jackson.core/jackson-core:2.12.4") lines)))
            "a shared dep is shown under each chain that carries it")
        (is (every? #(not (str/includes? % "(")) lines)
            "every occurrence is on the classpath: no reason annotations")))))

;; --- Classpath consistency: the tree draws exactly the module's classpath
;; --- as the lock pins it — selected versions only, under every chain that
;; --- carries them; conflict losers and non-module manifests stay out.

(defn- dep-pom
  "A maven-layout pom under root for group/name version, one dependency
   per [name version] entry (group-scoped), plus an empty jar."
  [root group name version deps]
  (let [d (io/file root (str/replace group "." "/") name version)]
    (.mkdirs d)
    (spit (io/file d (str name "-" version ".pom"))
          (str "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"
               "<project xmlns=\"http://maven.apache.org/POM/4.0.0\">\n"
               "  <modelVersion>4.0.0</modelVersion>\n"
               "  <groupId>" group "</groupId>\n"
               "  <artifactId>" name "</artifactId>\n"
               "  <version>" version "</version>\n"
               (when (seq deps)
                 (str "  <dependencies>\n"
                      (apply str (for [[dn dv] deps]
                                   (str "    <dependency>\n"
                                        "      <groupId>" group "</groupId>\n"
                                        "      <artifactId>" dn "</artifactId>\n"
                                        "      <version>" dv "</version>\n"
                                        "    </dependency>\n")))
                      "  </dependencies>\n"))
               "</project>\n"))
    (spit (io/file d (str name "-" version ".jar")) "")))

(deftest tree-loser-version-not-rendered
  "A loser version of a conflict does not enter the tree: the tree is the
   lock's per-module classpath, not the full trace, so no occurrence
   carries a reason annotation."
  (let [group "rig.ktest.loser"
        root (doto (File. (str (System/getProperty "java.io.tmpdir") "/"
                               (str "rig-tree-loser-" (java.util.UUID/randomUUID))))
               (.mkdirs) (.deleteOnExit))
        [base seen srv] (start-serving (str root) nil)
        m2 (io/file (System/getProperty "user.home") ".m2" "repository"
                    "rig" "ktest" "loser")]
    (delete-tree (str m2))
    (try
      (do
        (dep-pom root group "a" "1.0.0" [["shared" "1.0.0"]])
        (dep-pom root group "b" "1.0.0" [["shared" "2.0.0"]])
        (dep-pom root group "shared" "1.0.0" [])
        (dep-pom root group "shared" "2.0.0" [])
        (with-ws [["deps.edn" (str "{:mvn/repos {\"central\" {:url \"" base "\"} \"clojars\" {:url \"" base "\"}} "
                                   ":deps {" group "/a {:mvn/version \"1.0.0\"} "
                                   group "/b {:mvn/version \"1.0.0\"}}}")]]
          (fn [dir]
            (let [lines (-> (tree/tree {:workspace dir}) (get "tree") str/split-lines)]
              (is (some #(str/ends-with? % (str group "/shared:2.0.0")) lines)
                  "the winning version is rendered")
              (is (empty? (filter #(str/includes? % (str group "/shared:1.0.0")) lines))
                  "the loser version does not enter the tree")
              (is (every? #(not (str/includes? % "(")) lines)
                  "no occurrence carries a reason annotation")))))
      (finally
        (delete-tree (str m2))
        (delete-tree (str root))
        (.stop srv 0)))))

(deftest tree-resolves-the-module-alone
  "The module's manifest alone forms the basis: the root manifest
   tools.deps ships inside its jar (org.clojure/clojure in its :deps) and
   the machine's ~/.clojure/deps.edn do not leak into the tree — the
   lock's per-module classpath carries neither."
  (let [group "rig.ktest.solo"
        root (doto (File. (str (System/getProperty "java.io.tmpdir") "/"
                               (str "rig-tree-solo-" (java.util.UUID/randomUUID))))
               (.mkdirs) (.deleteOnExit))
        [base seen srv] (start-serving (str root) nil)
        _ (repo-artifacts! (str root) group "solo" "1.0.0")
        m2 (io/file (System/getProperty "user.home") ".m2" "repository"
                    "rig" "ktest")]
    (delete-tree (str m2))
    (try
      (with-ws [["deps.edn" (str "{:mvn/repos {\"central\" {:url \"" base "\"} \"clojars\" {:url \"" base "\"}} "
                                 ":deps {" group "/solo {:mvn/version \"1.0.0\"}}}")]]
        (fn [dir]
          (let [lines (-> (tree/tree {:workspace dir}) (get "tree") str/split-lines)]
            (is (= (count lines) 2) "the module's dep and nothing else")
            (is (some #(str/ends-with? % (str group "/solo:1.0.0")) lines)
                "the declared dep")
            (is (not (some #(str/includes? % "org.clojure") lines))
                "no root-manifest or user-manifest injection")))
)      (finally
        (delete-tree (str m2))
        (delete-tree (str root))
        (.stop srv 0)))))

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
