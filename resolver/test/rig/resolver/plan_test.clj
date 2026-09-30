(ns rig.resolver.plan-test
  "map-classpath must classify every classpath root tools.deps can produce —
  in particular git-dep source roots of alias :extra-deps (a workspace with
  a :tasks alias holding a tools.build git dep must lock and update cleanly)."
  (:require [clojure.test :refer :all]
            [rig.resolver.plan :as plan]))

(def git-libs "/home/u/.gitlibs/libs")
(def sha "0d20256c40b5a6b6adbcbdf7d0c52cf9e22e994d")
(def git-src (str git-libs "/io.github.clojure/tools.build/" sha "/src/main/clojure"))
(def git-res (str git-libs "/io.github.clojure/tools.build/" sha "/src/main/resources"))
(def m2 "/home/u/.m2/repository")
(def m2-jar (str m2 "/org/clojure/clojure/1.12.5/clojure-1.12.5.jar"))
(def m2-classified (str m2 "/org/example/lib/2.0/lib-2.0-tests.jar"))
(def mods {"." "/ws" "app" "/ws/app"})

(deftest git-dep-roots-classify-as-one-classpath-entry
  (let [r (plan/map-classpath [git-src git-res] {} m2 git-libs)]
    (is (= ["io.github.clojure/tools.build:0d20256:jar"] (:classpath r)))
    (is (= 2 (count (:artifacts r))) "one artifact record per source root")
    (is (every? #(= "git" (:kind %)) (:artifacts r)))
    (is (every? #(= sha (get-in % [:git :sha])) (:artifacts r)))
    (is (= {"src/main/clojure" 1 "src/main/resources" 1}
           (frequencies (mapcat :paths (:artifacts r)))))))

(deftest git-dep-root-with-deps-root-classifies
  (let [e (str git-libs "/io.github.clojure/tools.build/" sha "/build/src")
        r (plan/map-classpath [e] {} m2 git-libs)]
    (is (= ["io.github.clojure/tools.build:0d20256:jar"] (:classpath r)))
    (is (= ["build/src"] (get-in r [:artifacts 0 :paths])))))

(deftest relative-roots-become-paths
  (let [r (plan/map-classpath ["build" "src"] {} m2 git-libs)]
    (is (= ["build" "src"] (:paths r)))
    (is (empty? (:classpath r)))))

(deftest m2-root-classifies-as-mvn
  (let [r (plan/map-classpath [m2-jar] {} m2 git-libs)]
    (is (= ["org.clojure/clojure:1.12.5:jar"] (:classpath r)))
    (is (= {:kind "mvn"
            :group "org.clojure"
            :name "clojure"
            :version "1.12.5"
            :extension "jar"
            :classifier nil}
           (select-keys (first (:artifacts r))
                        [:kind :group :name :version :extension :classifier])))))

(deftest m2-classified-jar-keeps-the-classifier
  (let [r (plan/map-classpath [m2-classified] {} m2 git-libs)]
    (is (= ["org.example/lib:2.0:tests:jar"] (:classpath r)))))

(deftest local-module-roots-classify-by-longest-prefix
  (let [r (plan/map-classpath ["/ws/app/src" "/ws"] mods m2 git-libs)]
    (is (= [{"local" "app"} {"local" "."}] (:classpath r)))
    (is (empty? (:artifacts r)))))

(deftest unknown-absolute-root-still-throws
  (is (thrown-with-msg? clojure.lang.ExceptionInfo
                        #"unclassified classpath root /config"
                        (plan/map-classpath ["/config"] {} m2 git-libs))))

(deftest combined-basis-maps-cleanly
  "All root kinds in one basis: relative alias extra-paths, a git dep's
  src and resources roots, an m2 jar, and a local module root."
  (let [r (plan/map-classpath ["build" "src" git-src git-res m2-jar "/ws/app/src"]
                              mods m2 git-libs)]
    (is (= ["build" "src"] (:paths r)))
    (is (= ["io.github.clojure/tools.build:0d20256:jar"
            "org.clojure/clojure:1.12.5:jar"
            {"local" "app"}]
           (:classpath r)))
    (is (= 3 (count (:artifacts r))))))

(deftest merge-artifacts-joins-git-roots-and-enriches
  (let [[g1 g2] (-> (plan/map-classpath [git-src git-res] {} m2 git-libs)
                    :artifacts)
        git-deps {(str "io.github.clojure/tools.build/" sha)
                  {:url "https://github.com/clojure/tools.build.git"}}
        m (first (plan/merge-artifacts [g1 g2] git-deps))]
    (is (= "io.github.clojure/tools.build:0d20256:jar" (:id m)))
    (is (= ["src/main/clojure" "src/main/resources"] (:paths m)))
    (is (= "https://github.com/clojure/tools.build.git" (get-in m [:git :url])))))
