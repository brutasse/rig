(ns rig.resolver.edit-test
  (:require [clojure.edn :as edn]
            [clojure.java.io :as io]
            [clojure.string :as str]
            [clojure.test :refer :all]
            [cljfmt.core :as cljfmt]
            [rig.resolver.edit :as edit]
            [rig.resolver.versions :as versions]
            [rig.resolver.versions-test :as vtest])
  (:import [java.io File]))

(def root-text
  "{:rig/modules [\"modules/lib\" \"modules/app\"]
   :rig/deps {org.clojure/clojure \"1.12.5\"
              org.clojure/tools.logging \"1.3.1\"}
   :deps {org.clojure/clojure {:mvn/version \"1.12.5\"}}}
  ")

(def lib-text
  "{:rig/lib example/lib
   :paths [\"src\"]
   :deps {org.clojure/clojure {:mvn/version \"1.12.5\"}
          org.clojure/tools.logging {:mvn/version \"1.3.1\"}}}
  ")

(def app-text
  "{:rig/lib example/app
   :paths [\"src\"]
   :deps {org.clojure/clojure {:mvn/version \"1.12.5\"}
          ;; local dep
          example/lib {:local/root \"../lib\"}}
   :aliases {:test {:extra-deps {lambdaisland/kaocha {:mvn/version \"1.66.1034\"}}
                    :exec-fn kaocha.runner/exec-fn}}}
  ")

(defn- rm!
  [dir]
  (when-let [f (File. dir)]
    (when (.exists f)
      (doseq [c (.listFiles f)] (rm! (.getPath c)))
      (.delete f))))

(defn- write-fixture
  [dir root lib app]
  (doseq [[rel text] [["deps.edn" root]
                      ["modules/lib/deps.edn" lib]
                      ["modules/app/deps.edn" app]]
        :when text]
    (io/make-parents (io/file dir rel))
    (spit (io/file dir rel) (cljfmt/reformat-string text)))
  (io/make-parents (io/file dir "modules/lib/src"))
  (io/make-parents (io/file dir "modules/app/src")))

(defn- read-manifest
  [dir rel]
  (edn/read-string (slurp (io/file dir rel))))

(defn- cljfmt-clean?
  [text]
  (= text (cljfmt/reformat-string text)))

(defn- artifact-version
  [lock id]
  (some (fn [a] (when (= id (:id a)) (:version a)))
        (get-in lock ["artifacts"])))

(defn- with-ws
  [root lib app f]
  (let [dir (File. (System/getProperty "java.io.tmpdir")
                    (str "rig-edit-test-" (java.util.UUID/randomUUID)))]
    (try
      (do (.mkdirs dir)
          (let [ws (str dir)]
            (write-fixture ws root lib app)
            (f ws)))
      (finally (rm! (str dir))))))

(deftest update-shared-req-propagates
  (with-ws root-text lib-text app-text
    (fn [dir]
      (let [orig-lib (slurp (io/file dir "modules/lib/deps.edn"))
            resp (edit/edit-dep {:workspace dir
                                 :args {:module "modules/lib"
                                        :coord "org.clojure/clojure"
                                        :requirement "1.12.4"
                                        :shared true}})]
        (is (= "1.12.4" (get-in (read-manifest dir "modules/lib/deps.edn")
                                [:deps 'org.clojure/clojure :mvn/version])))
        (is (= "1.12.4" (get-in (read-manifest dir "deps.edn")
                                [:rig/deps 'org.clojure/clojure])))
        (is (= "1.12.4" (get-in (read-manifest dir "deps.edn")
                                [:deps 'org.clojure/clojure :mvn/version])))
        (is (= "1.12.4" (get-in (read-manifest dir "modules/app/deps.edn")
                                [:deps 'org.clojure/clojure :mvn/version])))
        (is (= "1.3.1" (get-in (read-manifest dir "modules/lib/deps.edn")
                               [:deps 'org.clojure/tools.logging :mvn/version])))
        (let [orig (str/split-lines orig-lib)
              new (str/split-lines (slurp (io/file dir "modules/lib/deps.edn")))]
          (is (= (count orig) (count new)))
          (is (= 1 (count (filter identity (map not= orig new))))
              "single-line diff"))
        (is (str/includes? (slurp (io/file dir "modules/app/deps.edn"))
                           ";; local dep"))
        (is (cljfmt-clean? (slurp (io/file dir "modules/lib/deps.edn"))))
        (is (= #{"deps.edn" "modules/lib/deps.edn" "modules/app/deps.edn"}
               (into #{} (map #(get % "file")) (get resp "edits"))))
        (is (every? #(= "set" (get % "action")) (get resp "edits")))
        (is (= "1.12.4" (artifact-version (get resp "lock")
                                          "org.clojure/clojure:1.12.4:jar")))
        (is (some #(= "org.clojure/clojure:1.12.4:jar" %)
                  (get-in (get resp "lock") ["modules" "modules/lib" :classpath])))
        (is (some #(and (= "org.clojure/clojure" (str (:coord %)))
                        (= "explicit" (:reason %)))
                  (get-in resp ["lock" "skipped"]))
            "explicit version recorded in skipped")))))

(deftest update-shared-map-entry-preserves-exclusions
  (let [root-map
        "{:rig/modules [\"modules/lib\"]
          :rig/deps {cheshire/cheshire {:mvn/version \"5.10.2\"
                                        :exclusions [org.clojure/clojure]}}
          :deps {org.clojure/clojure {:mvn/version \"1.12.5\"}}}
         "
        lib-map
        "{:rig/lib example/lib
          :paths [\"src\"]
          :deps {org.clojure/clojure {:mvn/version \"1.12.5\"}
                 cheshire/cheshire {:mvn/version \"5.10.2\"
                                   :exclusions [org.clojure/clojure]}}}
         "]
    (with-ws root-map lib-map nil
      (fn [dir]
        (let [resp (edit/edit-dep {:workspace dir
                                   :args {:module "modules/lib"
                                          :coord "cheshire/cheshire"
                                          :requirement "5.11.0"
                                          :shared true}})]
          (is (= "5.11.0" (get-in (read-manifest dir "deps.edn")
                                  [:rig/deps 'cheshire/cheshire :mvn/version])))
          (is (= #{'org.clojure/clojure}
                 (set (get-in (read-manifest dir "deps.edn")
                              [:rig/deps 'cheshire/cheshire :exclusions])))
              ":exclusions survive the shared update")
          (is (= "5.11.0" (get-in (read-manifest dir "modules/lib/deps.edn")
                                  [:deps 'cheshire/cheshire :mvn/version])))
          (is (= #{'org.clojure/clojure}
                 (set (get-in (read-manifest dir "modules/lib/deps.edn")
                              [:deps 'cheshire/cheshire :exclusions]))))
          (is (= #{"deps.edn" "modules/lib/deps.edn"}
                 (into #{} (map #(get % "file")) (get resp "edits"))))
          (is (= "5.11.0" (artifact-version (get resp "lock")
                                            "cheshire/cheshire:5.11.0:jar"))))))))

(deftest update-alias-extra-deps-shared
  (with-ws root-text lib-text app-text
    (fn [dir]
      (let [resp (edit/edit-dep {:workspace dir
                                 :args {:module "modules/app"
                                        :coord "lambdaisland/kaocha"
                                        :requirement "1.67.1055"
                                        :alias "test"
                                        :shared true}})]
        (is (= "1.67.1055" (get-in (read-manifest dir "modules/app/deps.edn")
                                   [:aliases :test :extra-deps
                                    'lambdaisland/kaocha :mvn/version])))
        (is (nil? (get-in (read-manifest dir "modules/app/deps.edn")
                          [:deps 'lambdaisland/kaocha]))
            "the alias update does not add the coord to the module :deps")
        (is (= "1.67.1055" (get-in (read-manifest dir "deps.edn")
                                   [:rig/deps 'lambdaisland/kaocha :mvn/version]))
            "shared=true records the alias requirement in :rig/deps")
        (is (cljfmt-clean? (slurp (io/file dir "modules/app/deps.edn"))))
        (is (= #{"deps.edn" "modules/app/deps.edn"}
               (into #{} (map #(get % "file")) (get resp "edits"))))
        (is (some #(= "lambdaisland/kaocha:1.67.1055:jar" %)
                  (get-in (get resp "lock")
                          ["modules" "modules/app" :aliases :test :classpath])))))))

(deftest update-shared-only
  (with-ws root-text lib-text app-text
    (fn [dir]
      (let [resp (edit/edit-dep {:workspace dir
                                 :args {:module "."
                                        :coord "org.clojure/clojure"
                                        :requirement "1.12.4"
                                        :shared true
                                        :shared-only true}})]
        (is (= "1.12.4" (get-in (read-manifest dir "deps.edn")
                                [:rig/deps 'org.clojure/clojure])))
        (is (= "1.12.5" (get-in (read-manifest dir "deps.edn")
                                [:deps 'org.clojure/clojure :mvn/version]))
            "the root facade :deps is untouched")
        (is (= "1.12.5" (get-in (read-manifest dir "modules/lib/deps.edn")
                                [:deps 'org.clojure/clojure :mvn/version])))
        (is (= "1.12.5" (get-in (read-manifest dir "modules/app/deps.edn")
                                [:deps 'org.clojure/clojure :mvn/version])))
        (is (= #{"deps.edn"} (into #{} (map #(get % "file")) (get resp "edits")))
            "only the workspace manifest is edited")
        (is (some #(= "org.clojure/clojure:1.12.5:jar" %)
                  (get-in (get resp "lock") ["modules" "modules/lib" :classpath]))
            ":rig/deps does not leak into module resolution")))))

(deftest add-shared-req
  (with-ws root-text lib-text app-text
    (fn [dir]
      (let [orig-app (slurp (io/file dir "modules/app/deps.edn"))
            resp (edit/edit-dep {:workspace dir
                                 :args {:module "modules/app"
                                        :coord "org.clojure/core.specs.alpha"
                                        :requirement "0.4.74"
                                        :shared true}})]
        (is (= "0.4.74" (get-in (read-manifest dir "modules/app/deps.edn")
                                [:deps 'org.clojure/core.specs.alpha :mvn/version])))
         (is (= "0.4.74" (get-in (read-manifest dir "deps.edn")
                                 [:rig/deps 'org.clojure/core.specs.alpha :mvn/version])))
         (is (nil? (get-in (read-manifest dir "deps.edn")
                           [:deps 'org.clojure/core.specs.alpha]))
            "root facade not declaring the coord is untouched")
        (is (nil? (get-in (read-manifest dir "modules/lib/deps.edn")
                          [:deps 'org.clojure/core.specs.alpha]))
            "module not declaring the coord is untouched")
        (let [new (slurp (io/file dir "modules/app/deps.edn"))
              o (str/split-lines orig-app)
              n (str/split-lines new)]
          (is (= (inc (count o)) (count n)) "one new line")
          (is (= 1 (count (filter #(str/includes? % "org.clojure/core.specs.alpha") n)))
              "one new entry line")
          (is (= (last (filter #(str/includes? % "org.clojure/core.specs.alpha") n))
                 "        org.clojure/core.specs.alpha {:mvn/version \"0.4.74\"}}"))
          (is (= (last (filter #(str/includes? % "example/lib") n))
                 (subs (last (filter #(str/includes? % "example/lib") o))
                       0 (dec (count (last (filter #(str/includes? % "example/lib") o))))))
              "closing brace moved to the new entry line")
          (is (cljfmt-clean? new)))
        (is (cljfmt-clean? (slurp (io/file dir "deps.edn"))))
        (is (= #{"deps.edn" "modules/app/deps.edn"}
               (into #{} (map #(get % "file")) (get resp "edits"))))
        (is (= "0.4.74" (artifact-version (get resp "lock")
                                          "org.clojure/core.specs.alpha:0.4.74:jar")))))))

(deftest add-to-alias-extra-deps
  (with-ws root-text lib-text app-text
    (fn [dir]
      (let [resp (edit/edit-dep {:workspace dir
                                 :args {:module "modules/app"
                                        :coord "org.clojure/tools.logging"
                                        :requirement "1.2.4"
                                        :alias "test"
                                        :shared false}})]
        (is (= "1.2.4" (get-in (read-manifest dir "modules/app/deps.edn")
                               [:aliases :test :extra-deps
                                'org.clojure/tools.logging :mvn/version])))
        (is (= "1.3.1" (get-in (read-manifest dir "deps.edn")
                               [:rig/deps 'org.clojure/tools.logging]))
            "shared=false leaves root :rig/deps alone")
        (is (cljfmt-clean? (slurp (io/file dir "modules/app/deps.edn"))))
        (is (= #{"modules/app/deps.edn"}
               (into #{} (map #(get % "file")) (get resp "edits"))))
         (is (some #(= "org.clojure/tools.logging:1.2.4:jar" %)
                   (get-in (get resp "lock")
                           ["modules" "modules/app" :aliases :test :classpath])))))))

(deftest remove-shared-req
  (let [app-with (str/replace app-text
                              "example/lib {:local/root \"../lib\"}}"
                              "example/lib {:local/root \"../lib\"}\n          org.clojure/core.specs.alpha {:mvn/version \"0.4.74\"}}")
        lib-with (str/replace lib-text
                              "org.clojure/tools.logging {:mvn/version \"1.3.1\"}}"
                              "org.clojure/tools.logging {:mvn/version \"1.3.1\"}\n          org.clojure/core.specs.alpha {:mvn/version \"0.4.74\"}}")
        root-with (str/replace root-text
                               "org.clojure/tools.logging \"1.3.1\"}"
                               "org.clojure/tools.logging \"1.3.1\"\n              org.clojure/core.specs.alpha \"0.4.74\"}")]
    (with-ws root-with lib-with app-with
      (fn [dir]
        (let [orig-app (slurp (io/file dir "modules/app/deps.edn"))
              resp (edit/edit-dep {:workspace dir
                                   :args {:module "modules/app"
                                          :coord "org.clojure/core.specs.alpha"
                                          :requirement nil
                                          :shared true}})]
          (is (nil? (get-in (read-manifest dir "modules/app/deps.edn")
                            [:deps 'org.clojure/core.specs.alpha])))
          (is (nil? (get-in (read-manifest dir "deps.edn")
                            [:rig/deps 'org.clojure/core.specs.alpha])))
          (is (nil? (get-in (read-manifest dir "modules/lib/deps.edn")
                            [:deps 'org.clojure/core.specs.alpha])))
          (let [new (slurp (io/file dir "modules/app/deps.edn"))
                o (str/split-lines orig-app)
                n (str/split-lines new)]
            (is (= (dec (count o)) (count n)) "one line removed")
            (is (not (some #(str/includes? % "org.clojure/core.specs.alpha") n))
                "dep line gone")
            (is (= (last (filter #(str/includes? % "example/lib") n))
                   (str (last (filter #(str/includes? % "example/lib") o)) "}"))
                "closing brace moved back to the previous entry line")
            (is (str/includes? new ";; local dep"))
            (is (cljfmt-clean? new)))
          (is (every? #(= "remove" (get % "action")) (get resp "edits")))
          (is (every? #(nil? (get % "requirement")) (get resp "edits")))
          (is (= "0.4.74" (artifact-version (get resp "lock")
                                            "org.clojure/core.specs.alpha:0.4.74:jar"))
              "still in the lock: pulled transitively by org.clojure/clojure"))))))

(deftest single-module-same-file
  (let [one-module "{:paths [\"src\"]
                     :deps {org.clojure/clojure {:mvn/version \"1.12.5\"}}}
                     "]
    (with-ws one-module nil nil
      (fn [dir]
        (let [resp (edit/edit-dep {:workspace dir
                                   :args {:module "."
                                          :coord "org.clojure/tools.logging"
                                          :requirement "1.3.1"
                                          :shared true}})]
          (is (= "1.3.1" (get-in (read-manifest dir "deps.edn")
                                 [:deps 'org.clojure/tools.logging :mvn/version])))
          (is (= "1.3.1" (get-in (read-manifest dir "deps.edn")
                                 [:rig/deps 'org.clojure/tools.logging]))
              ":rig/deps section created in the same file")
          (is (cljfmt-clean? (slurp (io/file dir "deps.edn"))))
          (is (= #{"deps.edn"} (into #{} (map #(get % "file")) (get resp "edits"))))
          (is (= 2 (count (get resp "edits"))))
          (is (some #(= "org.clojure/tools.logging:1.3.1:jar" %)
                    (get-in (get resp "lock") ["modules" "." :classpath]))))))))

(deftest latest-selects-newest-eligible
  (with-ws root-text lib-text app-text
    (fn [dir]
      (let [resp (edit/edit-dep {:workspace dir
                                 :args {:module "."
                                        :coord "org.clojure/core.specs.alpha"
                                        :requirement "latest"
                                        :shared false}})
            got (get-in (read-manifest dir "deps.edn")
                        [:deps 'org.clojure/core.specs.alpha :mvn/version])
            md (versions/repo-metadata "org.clojure/core.specs.alpha"
                                       versions/standard-repos)
            byv (get md :by-version)
            cands (if (seq (:latest md)) (into #{} (:latest md)) (keys byv))
            eligible (filter (fn [v]
                               (let [pa (get-in byv [v :published-at])]
                                 (and pa
                                      (<= pa (- (System/currentTimeMillis)
                                                (versions/parse-duration-ms "48h"))))))
                             cands)
            want (reduce (fn [a b] (if (pos? (versions/vcmp a b)) a b)) eligible)]
        (is (string? got) "a concrete version is written, not latest")
        (is (re-find #"^\d+\." got))
        (is (= want got) "newest eligible version wins")
        (is (some #(= (str "org.clojure/core.specs.alpha:" got ":jar") (str %))
                  (get-in (get resp "lock") ["modules" "." :classpath]))
            "selected version is pinned in the lock")))))

(deftest latest-cooldown-refused
  (with-ws root-text lib-text app-text
    (fn [dir]
      (let [orig (slurp (io/file dir "deps.edn"))
            resp (edit/edit-dep {:workspace dir
                                 :args {:module "."
                                        :coord "org.clojure/core.specs.alpha"
                                        :requirement "latest"
                                        :shared false
                                        :cooldown {:default "99999d"}}})]
        (is (nil? (get resp "lock")) "no lock on refusal")
        (is (nil? (get resp "edits")))
        (is (= 1 (count (get resp "refused"))))
        (is (= "org.clojure/core.specs.alpha"
               (str (get-in resp ["refused" 0 :coord]))))
        (is (= "cooldown" (get-in resp ["refused" 0 :reason])))
        (is (= "99999d" (get-in resp ["refused" 0 :cooldown])))
        (is (= orig (slurp (io/file dir "deps.edn"))) "manifest untouched"))
      (let [resp (edit/edit-dep {:workspace dir
                                 :args {:module "."
                                        :coord "org.clojure/core.specs.alpha"
                                        :requirement "latest"
                                        :shared false
                                        :cooldown {:default "99999d"}
                                        :force true}})]
        (is (string? (get-in (read-manifest dir "deps.edn")
                             [:deps 'org.clojure/core.specs.alpha :mvn/version]))
            "force selects anyway")
        (is (some #(= "forced" (:reason %)) (get-in resp ["lock" "skipped"]))
            "forced pick is recorded")))))

(deftest latest-not-found
  (with-ws root-text lib-text app-text
    (fn [dir]
      (is (thrown-with-msg? Exception #"coordinate not found"
          (edit/edit-dep {:workspace dir
                          :args {:module "."
                                 :coord "org.rig.test/nonexistent-thing"
                                 :requirement "latest"
                                 :shared false}}))))))

(deftest latest-file-repo-cooldown
  (let [now (System/currentTimeMillis)
        old (- now (* 100 3600 1000))
        recent (- now 3600 1000)
        fresh-ws (fn [dir url]
                   (io/make-parents (io/file dir "modules/app/src"))
                   (spit (io/file dir "deps.edn")
                         (str "{:rig/modules [\"modules/app\"]\n"
                              " :mvn/repos {\"f\" {:url \"" url "\"}}}\n"))
                   (spit (io/file dir "modules/app/deps.edn")
                         (str "{:rig/lib example/app\n"
                              " :paths [\"src\"]\n"
                              " :deps {com.rig.test/cooldown {:mvn/version \"RELEASE\"}}\n"
                              " :mvn/repos {\"f\" {:url \"" url "\"}}}\n")))]
    (vtest/with-file-repo ["com.rig.test" "cooldown" {"1.0.0" old
                                                      "1.0.1" recent}]
      (fn [url]
        (let [dir (File. (System/getProperty "java.io.tmpdir")
                         (str "rig-edit-cooldown-" (java.util.UUID/randomUUID)))]
          (try
            (do (.mkdirs dir)
                (fresh-ws (str dir) url)
                (let [resp (edit/edit-dep {:workspace (str dir)
                                          :args {:module "modules/app"
                                                 :coord "com.rig.test/cooldown"
                                                 :requirement "latest"
                                                 :shared false}})]
                  (is (= "1.0.0"
                         (get-in (read-manifest dir "modules/app/deps.edn")
                                 [:deps 'com.rig.test/cooldown :mvn/version]))
                      "the newest version older than the cooldown is written")
                  (is (some #(and (= "com.rig.test/cooldown" (str (:coord %)))
                                  (= "1.0.1" (:version %))
                                  (= "cooldown" (:reason %)))
                            (get-in resp ["lock" "skipped"]))
                      "the fresh version is recorded as skipped"))
                (let [resp (edit/edit-dep {:workspace (str dir)
                                           :args {:module "modules/app"
                                                  :coord "com.rig.test/cooldown"
                                                  :requirement "latest"
                                                  :shared false
                                                  :force true}})]
                  (is (= "1.0.1"
                         (get-in (read-manifest dir "modules/app/deps.edn")
                                 [:deps 'com.rig.test/cooldown :mvn/version]))
                      "force takes the fresh version")
                  (is (some #(= "forced" (:reason %))
                            (get-in resp ["lock" "skipped"]))
                      "the forced pick is recorded"))
                (is (cljfmt-clean? (slurp (io/file dir "modules/app/deps.edn")))
                    "the manifest stays cljfmt-clean"))
            (finally (rm! (str dir)))))))))

(deftest latest-file-repo-refused-when-everything-is-fresh
  (let [now (System/currentTimeMillis)
        fresh-ws (fn [dir url]
                   (io/make-parents (io/file dir "modules/app/src"))
                   (spit (io/file dir "deps.edn")
                         (str "{:rig/modules [\"modules/app\"]\n"
                              " :mvn/repos {\"f\" {:url \"" url "\"}}}\n"))
                   (spit (io/file dir "modules/app/deps.edn")
                         (str "{:rig/lib example/app\n"
                              " :paths [\"src\"]\n"
                              " :deps {com.rig.test/cooldown {:mvn/version \"RELEASE\"}}\n"
                              " :mvn/repos {\"f\" {:url \"" url "\"}}}\n")))]
    (vtest/with-file-repo ["com.rig.test" "cooldown" {"1.0.0" (- now 3600 1000)
                                                      "1.0.1" (- now 7200 1000)}]
      (fn [url]
        (let [dir (File. (System/getProperty "java.io.tmpdir")
                         (str "rig-edit-cooldown-" (java.util.UUID/randomUUID)))]
          (try
            (do (.mkdirs dir)
                (fresh-ws (str dir) url)
                (let [orig (slurp (io/file dir "modules/app/deps.edn"))
                      resp (edit/edit-dep {:workspace (str dir)
                                          :args {:module "modules/app"
                                                 :coord "com.rig.test/cooldown"
                                                 :requirement "latest"
                                                 :shared false}})]
                  (is (nil? (get resp "lock")) "no lock on refusal")
                  (is (= 1 (count (get resp "refused"))))
                  (is (= "com.rig.test/cooldown"
                         (str (get-in resp ["refused" 0 :coord]))))
                  (is (= "cooldown" (get-in resp ["refused" 0 :reason])))
                  (is (= orig (slurp (io/file dir "modules/app/deps.edn")))
                      "manifest untouched on refusal")))
            (finally (rm! (str dir)))))))))
