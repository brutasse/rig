(ns rig.resolver.audit-fix-test
  "Kernel op tests for audit-fix: the plan over a file:// repo workspace,
  the edit sites and pool pins, and the single re-lock. Every manifest's
  :mvn/repos redeclares the standard ids (central, clojars) at the file://
  repo, so resolution never reaches the network."
  (:require [cheshire.core :as json]
            [clojure.edn :as edn]
            [clojure.java.io :as io]
            [clojure.string :as str]
            [clojure.test :refer :all]
            [rig.resolver.audit-fix :as audit-fix]
            [rig.resolver.resolve :as resolve]
            [rig.resolver.test-util :refer [delete-tree m2-dir-of sha1-hex ts with-ws]])
  (:import [java.io File]))

(defn- empty-jar!
  [f]
  (io/make-parents f)
  (with-open [zos (java.util.zip.ZipOutputStream. (io/output-stream f))]
    (.putNextEntry zos (doto (java.util.zip.ZipEntry. "META-INF/MANIFEST.MF")
                         (.setTime 0)))
    (.write zos (.getBytes "Manifest-Version: 1.0\n" "UTF-8"))
    (.closeEntry zos)))

(defn- pom
  "A maven pom with an optional dependency list [[group name version] …]."
  [group name version deps]
  (str "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"
       "<project xmlns=\"http://maven.apache.org/POM/4.0.0\">\n"
       "  <modelVersion>4.0.0</modelVersion>\n"
       (format "  <groupId>%s</groupId>\n" group)
       (format "  <artifactId>%s</artifactId>\n" name)
       (format "  <version>%s</version>\n" version)
       "  <packaging>jar</packaging>\n"
       (when (seq deps)
         (str "  <dependencies>\n"
              (apply str
                     (for [[g a v] deps]
                       (str "    <dependency>\n"
                            (format "      <groupId>%s</groupId>\n" g)
                            (format "      <artifactId>%s</artifactId>\n" a)
                            (format "      <version>%s</version>\n" v)
                            "    </dependency>\n")))
              "  </dependencies>\n"))
       "</project>\n"))

(defn- with-repo
  "specs is [[group name {version {:published ms :deps [[g a v] …]}}] …];
  repo-ids the maven repo ids stamped with per-version timestamps; f
  receives the repo base url. The repo dir is deleted afterwards."
  [specs repo-ids f]
  (let [dir (File. (System/getProperty "java.io.tmpdir")
                   (str "rig-audit-fix-repo-" (java.util.UUID/randomUUID)))]
    (try
      (do (.mkdirs dir)
          (doseq [[group name vers] (sort-by first specs)]
            (let [base (io/file dir (str/replace group "." "/") name)]
              (doseq [[v m] (sort-by key vers)]
                (let [vdir (io/file base v)
                      p (io/file vdir (str name "-" v ".pom"))
                      jar (io/file vdir (str name "-" v ".jar"))]
                  (.mkdirs vdir)
                  (spit p (pom group name v (:deps m)))
                  (empty-jar! jar)
                  (spit (io/file vdir (str name "-" v ".pom.sha1")) (sha1-hex p))
                  (spit (io/file vdir (str name "-" v ".jar.sha1")) (sha1-hex jar))))
          (let [release (last (sort (keys vers)))
                md (str "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"
                      "<metadata>\n"
                      "  <versioning>\n"
                      "    <versions>\n"
                      (apply str (for [v (sort (keys vers))]
                               (str "      <version>" v "</version>\n")))
                      "    </versions>\n"
                      "    <latest>" release "</latest>\n"
                      "    <release>" release "</release>\n"
                      "    <lastUpdated>" (ts (System/currentTimeMillis))
                      "</lastUpdated>\n"
                      "  </versioning>\n"
                      "</metadata>\n")]
            (spit (io/file base "maven-metadata.xml") md)
            (spit (io/file base "maven-metadata.xml.sha1")
                  (sha1-hex (io/file base "maven-metadata.xml")))
            (doseq [id repo-ids]
              (let [md-id (str "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"
                               "<metadata>\n"
                               "  <versioning>\n"
                               "    <versions>\n"
                               (apply str (for [[v m] (sort-by key vers)]
                                        (str "      <version>\n"
                                             "        <value>" v "</value>\n"
                                             "        <updated>" (ts (:published m))
                                             "</updated>\n"
                                             "      </version>\n")))
                               "    </versions>\n"
                               "    <latest>" release "</latest>\n"
                               "    <release>" release "</release>\n"
                               "  </versioning>\n"
                               "</metadata>\n")
                    f (io/file base (str "maven-metadata-" id ".xml"))]
                (spit f md-id)
                (spit (str (str f) ".sha1") (sha1-hex f)))))))
          (f (str "file://" (.getPath dir))))
      (finally (delete-tree (str dir))))))

(defn- man
  "A manifest's EDN text: the map's :mvn/repos replaced by the standard ids
  pointed at the repo url (offline resolution)."
  [url m]
  (pr-str (assoc m :mvn/repos {"central" {:url url}
                                  "clojars" {:url url}})))

(defn- lock!
  "Locks the workspace and writes deps.lock; returns the lock document."
  [dir args]
  (let [resp (resolve/resolve-lock {:workspace dir
                                    :lock "deps.lock"
                                    :args (or args {})})]
    (spit (io/file dir "deps.lock") (json/generate-string (get resp "lock")))
    (get resp "lock")))

(defn- fix!
  "Runs the audit-fix op; args optional."
  ([dir targets] (fix! dir targets {}))
  ([dir targets args]
   (audit-fix/audit-fix {:workspace dir
                         :lock "deps.lock"
                         :args (merge {:targets targets} (or args {}))})))

(defn- manifest
  [dir rel]
  (edn/read-string (slurp (io/file dir rel))))

(defn- artifact-id?
  [lock id]
  (some #(= id (:id %)) (get-in lock ["artifacts"])))

(defn- old [now] (- now (* 72 3600 1000)))
(defn- recent [now] (- now 3600 1000))

(defn- seed-m2!
  "Seeds the local m2 entry of group/name with maven-metadata.xml (release
  v) and the v pom + jar: MIMA's native floating resolution reads the local
  metadata, which the file:// repo does not serve it."
  [group name v]
  (let [m2 (m2-dir-of group name)]
    (.mkdirs (io/file m2 v))
    (spit (io/file m2 "maven-metadata.xml")
          (str "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"
               "<metadata>\n"
               "  <versioning>\n"
               "    <versions><version>" v "</version></versions>\n"
               "    <latest>" v "</latest>\n"
               "    <release>" v "</release>\n"
               "    <lastUpdated>" (ts (System/currentTimeMillis))
               "</lastUpdated>\n"
               "  </versioning>\n"
               "</metadata>\n"))
    (spit (io/file m2 v (str name "-" v ".pom")) (pom group name v []))
    (empty-jar! (io/file m2 v (str name "-" v ".jar")))))

(deftest direct-dep-fits-in-module-manifest
  (let [now (System/currentTimeMillis)]
    (with-repo [["com.rig.test" "parent"
                 {"1.0.0" {:published (old now)
                           :deps [["com.rig.test" "child" "1.0.0"]]}
                  "1.0.1" {:published (old now)
                           :deps [["com.rig.test" "child" "1.0.0"]]}}]
                ["com.rig.test" "child"
                 {"1.0.0" {:published (old now)}
                  "1.1.0" {:published (old now)}}]]
      #{"central" "clojars"}
      (fn [url]
        (with-ws {"deps.edn" (man url {:rig/modules ["modules/app"]})
                  "modules/app/deps.edn" (man url
                                              {:rig/lib "example/app"
                                               :paths ["src"]
                                               :deps {'com.rig.test/parent
                                                      {:mvn/version "1.0.0"}}})}
          (fn [dir]
            (lock! dir {})
            (let [orig-root (slurp (io/file dir "deps.edn"))
                  resp (fix! dir [{:coord "com.rig.test/parent"
                                   :locked "1.0.0"
                                   :fixes ["1.0.1"]}])]
              (is (= [{:coord "com.rig.test/parent"
                       :locked "1.0.0"
                       :target "1.0.1"}]
                     (get resp "plan")))
              (is (= [{:coord "com.rig.test/parent"
                       :target "1.0.1"
                       :sites [{:file "modules/app/deps.edn"
                                :module "modules/app"
                                :section "deps"
                                :alias nil
                                :from "1.0.0"}]
                       :pool nil}]
                     (get resp "applied")))
              (is (= "1.0.1" (get-in (manifest dir "modules/app/deps.edn")
                                     [:deps 'com.rig.test/parent
                                      :mvn/version])))
              (is (= orig-root (slurp (io/file dir "deps.edn")))
                  "no pool entry: the declaring module's pin is the edit")
              (is (artifact-id? (get resp "lock")
                                "com.rig.test/parent:1.0.1:jar"))
              (is (artifact-id? (get resp "lock")
                                "com.rig.test/child:1.0.0:jar")
                  "the transitive stays at its own version")
              (is (some #(= "com.rig.test/parent:1.0.1:jar" %)
                        (get-in (get resp "lock")
                                ["modules" "modules/app" :classpath]))))))))))

(deftest transitive-dep-is-pinned-in-the-pool
  (let [now (System/currentTimeMillis)]
    (with-repo [["com.rig.test" "parent"
                 {"1.0.0" {:published (old now)
                           :deps [["com.rig.test" "child" "1.0.0"]]}}]
                ["com.rig.test" "child"
                 {"1.0.0" {:published (old now)}
                  "1.1.0" {:published (old now)}}]]
      #{"central" "clojars"}
      (fn [url]
        (with-ws {"deps.edn" (man url {:rig/modules ["modules/app"]})
                  "modules/app/deps.edn" (man url
                                              {:rig/lib "example/app"
                                               :paths ["src"]
                                               :deps {'com.rig.test/parent
                                                      {:mvn/version "1.0.0"}}})}
          (fn [dir]
            (lock! dir {})
            (let [orig-app (slurp (io/file dir "modules/app/deps.edn"))
                  resp (fix! dir [{:coord "com.rig.test/child"
                                   :locked "1.0.0"
                                   :fixes ["1.1.0"]}]
                              {:transitive true})]
              (is (= [{:coord "com.rig.test/child"
                       :locked "1.0.0"
                       :target "1.1.0"}]
                     (get resp "plan")))
              (is (= [{:coord "com.rig.test/child"
                       :target "1.1.0"
                       :sites []
                       :pool {:action "add" :from nil :value "1.1.0"}}]
                     (get resp "applied")))
              (is (= "1.1.0" (get-in (manifest dir "deps.edn")
                                     [:rig/deps 'com.rig.test/child]))
                  "the pool pin is written at the root")
              (is (= orig-app (slurp (io/file dir "modules/app/deps.edn")))
                  "no module declares the coord, none is touched")
              (is (artifact-id? (get resp "lock")
                                "com.rig.test/child:1.1.0:jar"))
              (is (artifact-id? (get resp "lock")
                                "com.rig.test/parent:1.0.0:jar")))))))))

(deftest vulnerable-pool-entry-is-updated
  (let [now (System/currentTimeMillis)]
    (with-repo [["com.rig.test" "parent"
                 {"1.0.0" {:published (old now)
                           :deps [["com.rig.test" "child" "1.0.0"]]}}]
                ["com.rig.test" "child"
                 {"1.0.0" {:published (old now)}
                  "1.1.0" {:published (old now)}}]]
      #{"central" "clojars"}
      (fn [url]
        (with-ws {"deps.edn" (man url {:rig/modules ["modules/app"]
                                       :rig/deps {'com.rig.test/child
                                                  {:mvn/version "1.0.0"}}})
                  "modules/app/deps.edn" (man url
                                              {:rig/lib "example/app"
                                               :paths ["src"]
                                               :deps {'com.rig.test/parent
                                                      {:mvn/version "1.0.0"}}})}
          (fn [dir]
            (lock! dir {})
            (let [resp (fix! dir [{:coord "com.rig.test/child"
                                   :locked "1.0.0"
                                   :fixes ["1.1.0"]}])]
              (is (= [{:coord "com.rig.test/child"
                       :target "1.1.0"
                       :sites []
                       :pool {:action "update" :from "1.0.0" :value "1.1.0"}}]
                     (get resp "applied")))
              (is (= "1.1.0" (get-in (manifest dir "deps.edn")
                                     [:rig/deps 'com.rig.test/child
                                      :mvn/version])))
              (is (artifact-id? (get resp "lock")
                                "com.rig.test/child:1.1.0:jar")))))))))

(deftest clean-pool-entry-is-untouched
  "The pool pin is clean and the vulnerable version is a declaring module's
  own pin: the site is edited, the pool entry is left alone."
  (let [now (System/currentTimeMillis)]
    (with-repo [["com.rig.test" "parent"
                 {"1.0.0" {:published (old now)
                           :deps [["com.rig.test" "child" "1.0.0"]]}}]
                ["com.rig.test" "child"
                 {"1.0.0" {:published (old now)}
                  "1.1.0" {:published (old now)}}]]
      #{"central" "clojars"}
      (fn [url]
        (with-ws {"deps.edn" (man url {:rig/modules ["modules/app"]
                                       :rig/deps {'com.rig.test/child
                                                  {:mvn/version "1.1.0"}}})
                  "modules/app/deps.edn" (man url
                                              {:rig/lib "example/app"
                                               :paths ["src"]
                                               :deps {'com.rig.test/parent
                                                      {:mvn/version "1.0.0"}
                                                      'com.rig.test/child
                                                      {:mvn/version "1.0.0"}}})}
          (fn [dir]
            (lock! dir {})
            (let [orig-root (slurp (io/file dir "deps.edn"))
                  resp (fix! dir [{:coord "com.rig.test/child"
                                   :locked "1.0.0"
                                   :fixes ["1.1.0"]}])]
              (is (= [{:coord "com.rig.test/child"
                       :target "1.1.0"
                       :sites [{:file "modules/app/deps.edn"
                                :module "modules/app"
                                :section "deps"
                                :alias nil
                                :from "1.0.0"}]
                       :pool nil}]
                     (get resp "applied")))
              (is (= orig-root (slurp (io/file dir "deps.edn")))
                  "the clean pool entry is untouched")
              (is (= "1.1.0" (get-in (manifest dir "modules/app/deps.edn")
                                     [:deps 'com.rig.test/child
                                      :mvn/version]))))))))))

(deftest breaking-fix-stands-without-the-flag
  (let [now (System/currentTimeMillis)]
    (with-repo [["com.rig.test" "parent"
                 {"1.0.0" {:published (old now)
                           :deps [["com.rig.test" "child" "1.0.0"]]}}]
                ["com.rig.test" "child"
                 {"1.0.0" {:published (old now)}
                  "2.0.0" {:published (old now)}}]]
      #{"central" "clojars"}
      (fn [url]
        (with-ws {"deps.edn" (man url {:rig/modules ["modules/app"]})
                  "modules/app/deps.edn" (man url
                                              {:rig/lib "example/app"
                                               :paths ["src"]
                                               :deps {'com.rig.test/parent
                                                      {:mvn/version "1.0.0"}}})}
          (fn [dir]
            (lock! dir {})
            (let [orig-lock (slurp (io/file dir "deps.lock"))
                  resp (fix! dir [{:coord "com.rig.test/child"
                                   :locked "1.0.0"
                                   :fixes ["2.0.0"]}])]
              (is (= [{:coord "com.rig.test/child"
                       :locked "1.0.0"
                       :reason "breaking"}]
                     (get resp "plan")))
              (is (empty? (get resp "applied")))
              (is (nil? (get resp "edits")))
              (is (nil? (get resp "lock")))
              (is (= orig-lock (slurp (io/file dir "deps.lock")))
                  "the lock is untouched")
              (is (= "1.0.0" (get-in (manifest dir "modules/app/deps.edn")
                                     [:deps 'com.rig.test/parent
                                      :mvn/version]))))))))))

(deftest breaking-fix-applies-with-the-flag
  (let [now (System/currentTimeMillis)]
    (with-repo [["com.rig.test" "parent"
                 {"1.0.0" {:published (old now)
                           :deps [["com.rig.test" "child" "1.0.0"]]}}]
                ["com.rig.test" "child"
                 {"1.0.0" {:published (old now)}
                  "2.0.0" {:published (old now)}}]]
      #{"central" "clojars"}
      (fn [url]
        (with-ws {"deps.edn" (man url {:rig/modules ["modules/app"]})
                  "modules/app/deps.edn" (man url
                                              {:rig/lib "example/app"
                                               :paths ["src"]
                                               :deps {'com.rig.test/parent
                                                      {:mvn/version "1.0.0"}}})}
          (fn [dir]
            (lock! dir {})
            (let [resp (fix! dir [{:coord "com.rig.test/child"
                                   :locked "1.0.0"
                                   :fixes ["2.0.0"]}]
                              {:breaking true :transitive true})]
              (is (= [{:coord "com.rig.test/child"
                       :locked "1.0.0"
                       :target "2.0.0"}]
                     (get resp "plan")))
              (is (= "2.0.0" (get-in (manifest dir "deps.edn")
                                     [:rig/deps 'com.rig.test/child])))
              (is (artifact-id? (get resp "lock")
                                "com.rig.test/child:2.0.0:jar")))))))))

(deftest no-fix-stands
  (let [now (System/currentTimeMillis)]
    (with-repo [["com.rig.test" "parent"
                 {"1.0.0" {:published (old now)
                           :deps [["com.rig.test" "child" "1.0.0"]]}}]
                ["com.rig.test" "child"
                 {"1.0.0" {:published (old now)}}]]
      #{"central" "clojars"}
      (fn [url]
        (with-ws {"deps.edn" (man url {:rig/modules ["modules/app"]})
                  "modules/app/deps.edn" (man url
                                              {:rig/lib "example/app"
                                               :paths ["src"]
                                               :deps {'com.rig.test/parent
                                                      {:mvn/version "1.0.0"}}})}
          (fn [dir]
            (lock! dir {})
            (let [resp (fix! dir [{:coord "com.rig.test/child"
                                   :locked "1.0.0"
                                   :fixes []}])]
              (is (= [{:coord "com.rig.test/child"
                       :locked "1.0.0"
                       :reason "no-fix"}]
                     (get resp "plan")))
              (is (empty? (get resp "applied")))
              (is (nil? (get resp "lock"))))))))))

(deftest below-locked-fix-stands
  (let [now (System/currentTimeMillis)]
    (with-repo [["com.rig.test" "child"
                 {"1.0.5" {:published (old now)}
                  "1.1.0" {:published (old now)}}]]
      #{"central" "clojars"}
      (fn [url]
        (with-ws {"deps.edn" (man url {:rig/modules ["modules/app"]})
                  "modules/app/deps.edn" (man url
                                              {:rig/lib "example/app"
                                               :paths ["src"]
                                               :deps {'com.rig.test/child
                                                      {:mvn/version "1.1.0"}}})}
          (fn [dir]
            (lock! dir {})
            (let [resp (fix! dir [{:coord "com.rig.test/child"
                                   :locked "1.1.0"
                                   :fixes ["1.0.5"]}])]
              (is (= [{:coord "com.rig.test/child"
                       :locked "1.1.0"
                       :reason "below-locked"}]
                     (get resp "plan")))
              (is (empty? (get resp "applied")))
              (is (nil? (get resp "lock"))))))))))

(deftest unserved-fix-stands-without-failing-the-run
  "A fix version no repository serves stands unfixable with a named
  reason; the other fixable coordinates of the same run are applied and
  re-locked."
  (let [now (System/currentTimeMillis)]
    (with-repo [["com.rig.test" "broken"
                 {"1.0.0" {:published (old now)}}]
                ["com.rig.test" "parent"
                 {"1.0.0" {:published (old now)
                           :deps [["com.rig.test" "child" "1.0.0"]]}}]
                ["com.rig.test" "child"
                 {"1.0.0" {:published (old now)}
                  "1.1.0" {:published (old now)}}]]
       #{"central" "clojars"}
       (fn [url]
         (with-ws {"deps.edn" (man url {:rig/modules ["modules/app"]})
                   "modules/app/deps.edn" (man url
                                               {:rig/lib "example/app"
                                                :paths ["src"]
                                                :deps {'com.rig.test/parent
                                                       {:mvn/version "1.0.0"}
                                                       'com.rig.test/broken
                                                       {:mvn/version "1.0.0"}}})}
           (fn [dir]
             (lock! dir {})
             (let [resp (fix! dir [{:coord "com.rig.test/child"
                                    :locked "1.0.0"
                                    :fixes ["1.1.0"]}
                                   {:coord "com.rig.test/broken"
                                    :locked "1.0.0"
                                    :fixes ["1.9.0"]}]
                               {:transitive true})]
               (is (= [{:coord "com.rig.test/broken"
                        :locked "1.0.0"
                        :reason "fix-unavailable"}
                       {:coord "com.rig.test/child"
                        :locked "1.0.0"
                        :target "1.1.0"}]
                      (get resp "plan")))
               (is (= 1 (count (get resp "applied")))
                   "only the served fix is applied")
               (is (= "1.1.0" (get-in (manifest dir "deps.edn")
                                      [:rig/deps 'com.rig.test/child])))
               (is (= "1.0.0" (get-in (manifest dir "modules/app/deps.edn")
                                      [:deps 'com.rig.test/broken
                                       :mvn/version]))
                   "the unserved coordinate's pin is untouched")
               (is (artifact-id? (get resp "lock")
                                 "com.rig.test/child:1.1.0:jar"))
               (is (artifact-id? (get resp "lock")
                                 "com.rig.test/broken:1.0.0:jar")
                   "the unserved coordinate stays locked"))))))))

(deftest alias-extra-deps-site-is-edited
  (let [now (System/currentTimeMillis)]
    (with-repo [["com.rig.test" "parent"
                 {"1.0.0" {:published (old now)
                           :deps [["com.rig.test" "child" "1.0.0"]]}}]
                ["com.rig.test" "child"
                 {"1.0.0" {:published (old now)}
                  "1.1.0" {:published (old now)}}]]
      #{"central" "clojars"}
      (fn [url]
        (with-ws {"deps.edn" (man url {:rig/modules ["modules/app"]})
                  "modules/app/deps.edn" (man url
                                              {:rig/lib "example/app"
                                               :paths ["src"]
                                               :deps {'com.rig.test/parent
                                                      {:mvn/version "1.0.0"}}
                                               :aliases {:test
                                                          {:extra-deps
                                                           {'com.rig.test/child
                                                            {:mvn/version "1.0.0"}}}}})}
          (fn [dir]
            (lock! dir {})
            (let [resp (fix! dir [{:coord "com.rig.test/child"
                                   :locked "1.0.0"
                                   :fixes ["1.1.0"]}])]
              (is (= [{:coord "com.rig.test/child"
                       :target "1.1.0"
                       :sites [{:file "modules/app/deps.edn"
                                :module "modules/app"
                                :section "extra-deps"
                                :alias "test"
                                :from "1.0.0"}]
                       :pool nil}]
                     (get resp "applied")))
              (is (= "1.1.0" (get-in (manifest dir "modules/app/deps.edn")
                                     [:aliases :test :extra-deps
                                      'com.rig.test/child :mvn/version])))
              (is (some #(= "com.rig.test/child:1.1.0:jar" %)
                        (get-in (get resp "lock")
                                ["modules" "modules/app" :aliases :test
                                 :classpath]))))))))))

(deftest floating-site-becomes-a-concrete-pin
  "A floating requirement whose classpath pin is vulnerable is set to the
  concrete target, like rig update does."
  (let [now (System/currentTimeMillis)]
    (with-repo [["com.rig.test" "child"
                 {"1.0.0" {:published (old now)}
                  "1.1.0" {:published (recent now)}}]]
      #{"central" "clojars"}
      (fn [url]
        (with-ws {"deps.edn" (man url {:rig/modules ["modules/app"]})
                  "modules/app/deps.edn" (man url
                                              {:rig/lib "example/app"
                                               :paths ["src"]
                                               :deps {'com.rig.test/child
                                                      {:mvn/version "RELEASE"}}})}
          (fn [dir]
            (let [lock (lock! dir {})]
              (is (artifact-id? lock "com.rig.test/child:1.0.0:jar")
                  "the recent 1.1.0 is out of cooldown; 1.0.0 locks")
              (let [resp (fix! dir [{:coord "com.rig.test/child"
                                     :locked "1.0.0"
                                     :fixes ["1.1.0"]}])]
                (is (= [{:coord "com.rig.test/child"
                         :target "1.1.0"
                         :sites [{:file "modules/app/deps.edn"
                                  :module "modules/app"
                                  :section "deps"
                                  :alias nil
                                  :from "RELEASE"}]
                         :pool nil}]
                       (get resp "applied")))
                (is (= "1.1.0" (get-in (manifest dir "modules/app/deps.edn")
                                       [:deps 'com.rig.test/child
                                        :mvn/version])))
                (is (artifact-id? (get resp "lock")
                                  "com.rig.test/child:1.1.0:jar"))))))))))

(deftest multi-module-multi-version-one-target
  (let [now (System/currentTimeMillis)]
    (with-repo [["com.rig.test" "child"
                 {"1.0.0" {:published (old now)}
                  "1.1.0" {:published (old now)}
                  "1.2.0" {:published (old now)}}]]
      #{"central" "clojars"}
      (fn [url]
        (with-ws {"deps.edn" (man url {:rig/modules ["modules/app" "modules/b"]})
                  "modules/app/deps.edn" (man url
                                              {:rig/lib "example/app"
                                               :paths ["src"]
                                               :deps {'com.rig.test/child
                                                      {:mvn/version "1.0.0"}}})
                  "modules/b/deps.edn" (man url
                                            {:rig/lib "example/b"
                                             :paths ["src"]
                                             :deps {'com.rig.test/child
                                                    {:mvn/version "1.1.0"}}})}
          (fn [dir]
            (lock! dir {})
            (let [resp (fix! dir [{:coord "com.rig.test/child"
                                   :locked "1.0.0"
                                   :fixes ["1.2.0"]}
                                  {:coord "com.rig.test/child"
                                   :locked "1.1.0"
                                   :fixes ["1.2.0"]}])]
              (is (= [{:coord "com.rig.test/child"
                       :locked "1.0.0"
                       :target "1.2.0"}
                      {:coord "com.rig.test/child"
                       :locked "1.1.0"
                       :target "1.2.0"}]
                     (get resp "plan")))
              (is (= 1 (count (get resp "applied")))
                  "one target per coordinate, whatever the locked versions")
              (is (= [{:file "modules/app/deps.edn"
                       :module "modules/app"
                       :section "deps"
                       :alias nil
                       :from "1.0.0"}
                      {:file "modules/b/deps.edn"
                       :module "modules/b"
                       :section "deps"
                       :alias nil
                       :from "1.1.0"}]
                     (get-in (get resp "applied") [0 :sites])))
              (is (nil? (get-in (get resp "applied") [0 :pool])))
              (is (artifact-id? (get resp "lock")
                                "com.rig.test/child:1.2.0:jar")))))))))

(deftest other-refused-requirement-keeps-the-fix
  "A floating requirement the re-lock refuses (cooldown) rides back with the
  fix: the fix is still planned and edited, the refusal is in the response.
  The requirement enters the workspace after the first lock — an existing
  pin would keep it out of the refusal — and the local m2 is seeded for it:
  MIMA's native RELEASE resolution reads the local metadata, which the
  file:// repo does not serve it."
  (let [now (System/currentTimeMillis)
        m2 (m2-dir-of "com.rig.test" "other")]
    (delete-tree m2)
    (try
      (with-repo [["com.rig.test" "parent"
                   {"1.0.0" {:published (old now)
                             :deps [["com.rig.test" "child" "1.0.0"]]}}]
                  ["com.rig.test" "child"
                   {"1.0.0" {:published (old now)}
                    "1.1.0" {:published (old now)}}]
                  ["com.rig.test" "other"
                   {"1.0.0" {:published (recent now)}}]]
        #{"central" "clojars"}
        (fn [url]
          (with-ws {"deps.edn" (man url {:rig/modules ["modules/app"]})
                    "modules/app/deps.edn" (man url
                                                {:rig/lib "example/app"
                                                 :paths ["src"]
                                                 :deps {'com.rig.test/parent
                                                        {:mvn/version "1.0.0"}}})}
            (fn [dir]
              (lock! dir {})
              (seed-m2! "com.rig.test" "other" "1.0.0")
              (spit (io/file dir "modules/app/deps.edn")
                    (man url {:rig/lib "example/app"
                              :paths ["src"]
                              :deps {'com.rig.test/parent
                                     {:mvn/version "1.0.0"}
                                     'com.rig.test/other
                                     {:mvn/version "RELEASE"}}}))
              (let [resp (fix! dir [{:coord "com.rig.test/child"
                                   :locked "1.0.0"
                                   :fixes ["1.1.0"]}]
                               {:transitive true})]
               (is (= [{:coord "com.rig.test/child"
                        :target "1.1.0"
                        :sites []
                        :pool {:action "add" :from nil :value "1.1.0"}}]
                      (get resp "applied")))
               (is (some #(= "com.rig.test/other" (str (:coord %)))
                         (get resp "refused")))
               (is (= "cooldown" (get-in resp ["refused" 0 :reason])))
               (is (seq (get resp "edits"))
                    "the fix edits applied ahead of the refusal")
               (is (= "1.1.0" (get-in (manifest dir "deps.edn")
                                      [:rig/deps 'com.rig.test/child])))
               (is (nil? (get-in (manifest dir "deps.edn")
                                  [:rig/deps 'com.rig.test/other])))
               (is (artifact-id? (get resp "lock")
                                  "com.rig.test/other:1.0.0:jar")
                    "MIMA resolved the floating natively from the local m2")
               (is (artifact-id? (get resp "lock")
                                  "com.rig.test/child:1.1.0:jar")))))))
      (finally (delete-tree m2)))))

(deftest transitive-fix-stands-without-the-flag
  "A fix that would add a :rig/deps pool pin stands without :transitive —
  the manifests and the lock are untouched — and applies with it."
  (let [now (System/currentTimeMillis)]
    (with-repo [["com.rig.test" "parent"
                 {"1.0.0" {:published (old now)
                           :deps [["com.rig.test" "child" "1.0.0"]]}}]
                 ["com.rig.test" "child"
                  {"1.0.0" {:published (old now)}
                   "1.1.0" {:published (old now)}}]]
       #{"central" "clojars"}
       (fn [url]
         (with-ws {"deps.edn" (man url {:rig/modules ["modules/app"]})
                   "modules/app/deps.edn" (man url
                                               {:rig/lib "example/app"
                                                :paths ["src"]
                                                :deps {'com.rig.test/parent
                                                       {:mvn/version "1.0.0"}}})}
           (fn [dir]
             (lock! dir {})
             (let [targets [{:coord "com.rig.test/child"
                             :locked "1.0.0"
                             :fixes ["1.1.0"]}]
                   orig-root (slurp (io/file dir "deps.edn"))
                   orig-lock (slurp (io/file dir "deps.lock"))
                   resp (fix! dir targets)]
               (is (= [{:coord "com.rig.test/child"
                        :locked "1.0.0"
                        :reason "transitive"}]
                      (get resp "plan")))
               (is (empty? (get resp "applied")))
               (is (nil? (get resp "edits")))
               (is (nil? (get resp "lock")))
               (is (= orig-root (slurp (io/file dir "deps.edn")))
                   "no pool entry is written")
               (is (= orig-lock (slurp (io/file dir "deps.lock")))
                   "the lock is untouched")
               (let [resp (fix! dir targets {:transitive true})]
                 (is (= [{:coord "com.rig.test/child"
                          :locked "1.0.0"
                          :target "1.1.0"}]
                        (get resp "plan")))
                 (is (= "1.1.0" (get-in (manifest dir "deps.edn")
                                        [:rig/deps 'com.rig.test/child]))
                     "with the flag the pool pin is written")
                 (is (artifact-id? (get resp "lock")
                                   "com.rig.test/child:1.1.0:jar")))))))))

(deftest stale-pool-entry-stands
  "The pool already pins the fix and the lock still carries the vulnerable
  version (the lock predates the pin): the fix stands stale-lock — the
  re-lock, not an edit, is its remedy."
  (let [now (System/currentTimeMillis)]
    (with-repo [["com.rig.test" "parent"
                 {"1.0.0" {:published (old now)
                           :deps [["com.rig.test" "child" "1.0.0"]]}}]
                 ["com.rig.test" "child"
                  {"1.0.0" {:published (old now)}
                   "1.1.0" {:published (old now)}}]]
       #{"central" "clojars"}
       (fn [url]
         (with-ws {"deps.edn" (man url {:rig/modules ["modules/app"]})
                   "modules/app/deps.edn" (man url
                                               {:rig/lib "example/app"
                                                :paths ["src"]
                                                :deps {'com.rig.test/parent
                                                       {:mvn/version "1.0.0"}}})}
           (fn [dir]
             (lock! dir {})
             ;; The pool pin lands in the root after the lock: the stale
             ;; lock still carries the vulnerable child.
             (spit (io/file dir "deps.edn")
                   (man url {:rig/modules ["modules/app"]
                             :rig/deps {'com.rig.test/child
                                        {:mvn/version "1.1.0"}}}))
             (let [orig-root (slurp (io/file dir "deps.edn"))
                   orig-lock (slurp (io/file dir "deps.lock"))
                   resp (fix! dir [{:coord "com.rig.test/child"
                                    :locked "1.0.0"
                                    :fixes ["1.1.0"]}])]
               (is (= [{:coord "com.rig.test/child"
                        :locked "1.0.0"
                        :reason "stale-lock"}]
                      (get resp "plan")))
               (is (empty? (get resp "applied")))
               (is (nil? (get resp "edits")))
               (is (nil? (get resp "lock")))
               (is (= orig-root (slurp (io/file dir "deps.edn")))
                   "the clean pool entry is untouched")
               (is (= orig-lock (slurp (io/file dir "deps.lock")))
                   "the lock is untouched")))))))))

(deftest partial-fix-applies-the-sites-gates-the-pool
  "A coordinate declared in one module and pulled undeclared in another:
  without :transitive the declaring module's pin is bumped and the pool
  pin that would reach the other module stands; with it the pool pin is
  written and the re-lock pins every module."
  (let [now (System/currentTimeMillis)]
    (with-repo [["com.rig.test" "parent"
                 {"1.0.0" {:published (old now)
                           :deps [["com.rig.test" "child" "1.0.0"]]}}]
                 ["com.rig.test" "child"
                  {"1.0.0" {:published (old now)}
                   "1.1.0" {:published (old now)}}]]
       #{"central" "clojars"}
       (fn [url]
         (with-ws {"deps.edn" (man url {:rig/modules ["modules/app" "modules/b"]})
                   "modules/app/deps.edn" (man url
                                               {:rig/lib "example/app"
                                                :paths ["src"]
                                                :deps {'com.rig.test/parent
                                                       {:mvn/version "1.0.0"}
                                                       'com.rig.test/child
                                                       {:mvn/version "1.0.0"}}})
                   "modules/b/deps.edn" (man url
                                             {:rig/lib "example/b"
                                              :paths ["src"]
                                              :deps {'com.rig.test/parent
                                                     {:mvn/version "1.0.0"}}})}
           (fn [dir]
             (lock! dir {})
             (let [targets [{:coord "com.rig.test/child"
                             :locked "1.0.0"
                             :fixes ["1.1.0"]}]
                   orig-root (slurp (io/file dir "deps.edn"))
                   resp (fix! dir targets)]
               (is (= [{:coord "com.rig.test/child"
                        :locked "1.0.0"
                        :target "1.1.0"}]
                      (get resp "plan")))
               (is (= [{:coord "com.rig.test/child"
                        :target "1.1.0"
                        :sites [{:file "modules/app/deps.edn"
                                 :module "modules/app"
                                 :section "deps"
                                 :alias nil
                                 :from "1.0.0"}]
                        :pool nil}]
                      (get resp "applied"))
                   "the site is edited, the pool pin stands")
               (is (= "1.1.0" (get-in (manifest dir "modules/app/deps.edn")
                                      [:deps 'com.rig.test/child
                                       :mvn/version])))
               (is (= orig-root (slurp (io/file dir "deps.edn")))
                   "no pool entry is written")
               (is (artifact-id? (get resp "lock")
                                 "com.rig.test/child:1.1.0:jar")
                   "the declaring module moves to the fix")
               (is (some #(= "com.rig.test/child:1.0.0:jar" %)
                         (get-in (get resp "lock")
                                 ["modules" "modules/b" :classpath]))
                   "the undeclaring module keeps the vulnerable pull")
               (let [resp (fix! dir targets {:transitive true})]
                 (is (= "1.1.0" (get-in (manifest dir "deps.edn")
                                        [:rig/deps 'com.rig.test/child]))
                     "with the flag the pool pin is written")
                 (is (some #(= "com.rig.test/child:1.1.0:jar" %)
                           (get-in (get resp "lock")
                                   ["modules" "modules/b" :classpath]))
                     "the pool pin reaches the undeclaring module")
                 (is (artifact-id? (get resp "lock")
                                   "com.rig.test/child:1.1.0:jar"))))))))))
