(ns rig.resolver.audit-fix-test
  "Kernel op tests for audit-fix: the plan over a file:// repo workspace,
  the edit sites and root pins, and the single re-lock. Every manifest's
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
                       :pin nil}]
                     (get resp "applied")))
              (is (= "1.0.1" (get-in (manifest dir "modules/app/deps.edn")
                                     [:deps 'com.rig.test/parent
                                      :mvn/version])))
              (is (= orig-root (slurp (io/file dir "deps.edn")))
                  "no pin entry: the declaring module's pin is the edit")
              (is (artifact-id? (get resp "lock")
                                "com.rig.test/parent:1.0.1:jar"))
              (is (artifact-id? (get resp "lock")
                                "com.rig.test/child:1.0.0:jar")
                  "the transitive stays at its own version")
              (is (some #(= "com.rig.test/parent:1.0.1:jar" %)
                        (get-in (get resp "lock")
                                ["modules" "modules/app" :classpath]))))))))))

(deftest transitive-dep-is-pinned-in-overrides
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
                                   :fixes ["1.1.0"]}
                              ])]
              (is (= [{:coord "com.rig.test/child"
                       :locked "1.0.0"
                       :target "1.1.0"}]
                     (get resp "plan")))
              (is (= [{:coord "com.rig.test/child"
                       :target "1.1.0"
                       :sites []
                       :pin {:section "overrides" :action "add" :from nil
                             :value "1.1.0"}}]
                     (get resp "applied")))
              (is (= "1.1.0" (get-in (manifest dir "deps.edn")
                                     [:rig/overrides 'com.rig.test/child]))
                  "the override pin is written at the root")
              (is (nil? (get-in (manifest dir "deps.edn")
                                 [:rig/deps 'com.rig.test/child]))
                  "no pool section is created for the pin")
              (is (re-find #"\n :rig/overrides " (slurp (io/file dir "deps.edn")))
                  "the section starts on its own line")
              (is (= orig-app (slurp (io/file dir "modules/app/deps.edn")))
                  "no module declares the coord, none is touched")
              (is (artifact-id? (get resp "lock")
                                "com.rig.test/child:1.1.0:jar"))
              (is (artifact-id? (get resp "lock")
                                "com.rig.test/parent:1.0.0:jar")))))))))

(deftest override-add-beside-an-existing-pool
  "A pin \"add\" into a root that already carries a pool: the entry
  lands in a new :rig/overrides section; the pool is untouched."
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
                                        :rig/deps {'com.rig.test/parent
                                                   "1.0.0"}})
                   "modules/app/deps.edn" (man url
                                               {:rig/lib "example/app"
                                                :paths ["src"]
                                                :deps {'com.rig.test/parent
                                                       {:mvn/version "1.0.0"}}})}
           (fn [dir]
             (lock! dir {})
             (let [resp (fix! dir [{:coord "com.rig.test/child"
                                    :locked "1.0.0"
                                    :fixes ["1.1.0"]}
                               ])]
               (is (= [{:coord "com.rig.test/child"
                        :locked "1.0.0"
                        :target "1.1.0"}]
                      (get resp "plan")))
               (is (= "1.1.0" (get-in (manifest dir "deps.edn")
                                       [:rig/overrides 'com.rig.test/child]))
                   "the override pin is written at the root")
               (is (= "1.0.0" (get-in (manifest dir "deps.edn")
                                      [:rig/deps 'com.rig.test/parent]))
                   "the existing pool entry is untouched")
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
                       :pin {:section "pool" :action "update" :from "1.0.0"
                             :value "1.1.0"}}]
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
                       :pin nil}]
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
                                   :fixes ["2.0.0"]}] {:breaking true})]
              (is (= [{:coord "com.rig.test/child"
                       :locked "1.0.0"
                       :target "2.0.0"}]
                     (get resp "plan")))
              (is (= "2.0.0" (get-in (manifest dir "deps.edn")
                                     [:rig/overrides 'com.rig.test/child])))
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
                                    :fixes ["1.9.0"]}])]
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
                                      [:rig/overrides 'com.rig.test/child])))
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
                       :pin nil}]
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
                         :pin nil}]
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
              (is (nil? (get-in (get resp "applied") [0 :pin])))
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
                                   :fixes ["1.1.0"]}])]
               (is (= [{:coord "com.rig.test/child"
                        :target "1.1.0"
                        :sites []
                        :pin {:section "overrides" :action "add" :from nil
                              :value "1.1.0"}}]
                      (get resp "applied")))
               (is (some #(= "com.rig.test/other" (str (:coord %)))
                         (get resp "refused")))
               (is (= "cooldown" (get-in resp ["refused" 0 :reason])))
               (is (seq (get resp "edits"))
                    "the fix edits applied ahead of the refusal")
               (is (= "1.1.0" (get-in (manifest dir "deps.edn")
                                      [:rig/overrides 'com.rig.test/child])))
               (is (nil? (get-in (manifest dir "deps.edn")
                                  [:rig/overrides 'com.rig.test/other])))
               (is (artifact-id? (get resp "lock")
                                  "com.rig.test/other:1.0.0:jar")
                    "MIMA resolved the floating natively from the local m2")
               (is (artifact-id? (get resp "lock")
                                  "com.rig.test/child:1.1.0:jar")))))))
      (finally (delete-tree m2)))))

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
                   "the lock is untouched"))))))))

(deftest override-pin-updates-a-vulnerable-overrides-entry
  "An existing :rig/overrides entry carries the vulnerable version: the
  entry is updated in place."
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
                                       :rig/overrides {'com.rig.test/child
                                                       "1.0.0"}})
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
                       :pin {:section "overrides" :action "update" :from "1.0.0"
                             :value "1.1.0"}}]
                     (get resp "applied")))
              (is (= "1.1.0" (get-in (manifest dir "deps.edn")
                                     [:rig/overrides 'com.rig.test/child])))
              (is (artifact-id? (get resp "lock")
                                "com.rig.test/child:1.1.0:jar")))))))))
(deftest stale-override-entry-stands
  "The override already pins the fix and the lock still carries the
  vulnerable version (the lock predates the pin): the fix stands
  stale-lock — the re-lock, not an edit, is its remedy."
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
            ;; The override pin lands in the root after the lock: the
            ;; stale lock still carries the vulnerable child.
            (spit (io/file dir "deps.edn")
                  (man url {:rig/modules ["modules/app"]
                            :rig/overrides {'com.rig.test/child
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
                  "the clean override entry is untouched")
              (is (= orig-lock (slurp (io/file dir "deps.lock")))
                  "the lock is untouched"))))))))

(deftest partial-fix-edits-the-site-and-pins-the-override
  "A coordinate declared in one module and pulled undeclared in another:
  the declaring module's pin is bumped and an override pin reaches the
  other module — one run, one re-lock."
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
                        :pin {:section "overrides" :action "add" :from nil
                              :value "1.1.0"}}]
                      (get resp "applied"))
                   "the site is edited, the override pin is written")
               (is (= "1.1.0" (get-in (manifest dir "modules/app/deps.edn")
                                      [:deps 'com.rig.test/child
                                       :mvn/version])))
               (is (= "1.1.0" (get-in (manifest dir "deps.edn")
                                      [:rig/overrides 'com.rig.test/child])))
               (is (artifact-id? (get resp "lock")
                                 "com.rig.test/child:1.1.0:jar")
                   "the declaring module moves to the fix")
                 (is (some #(= "com.rig.test/child:1.1.0:jar" %)
                           (get-in (get resp "lock")
                                   ["modules" "modules/b" :classpath]))
                     "the override pin reaches the undeclaring module"))))))))


(deftest dry-sweep-tests-the-pins-against-their-absence
  "The :dry-sweep op applies the fix edits but re-derives instead of
  re-locking: the response carries the sweep — per coordinate, per
  module, per scope, the resolved kind and version with the root's
  :rig/overrides stripped from the basis — and no lock document; the
  lock file is untouched."
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
        (with-ws {"deps.edn" (man url {:rig/modules ["modules/app"]
                                       :rig/overrides {'com.rig.test/child
                                                       "1.0.0"}})
                  "modules/app/deps.edn" (man url
                                              {:rig/lib "example/app"
                                               :paths ["src"]
                                               :deps {'com.rig.test/parent
                                                      {:mvn/version "1.0.0"}}
                                               :aliases {:test {:extra-deps
                                                                {'com.rig.test/child
                                                                 {:mvn/version "1.1.0"}}}}})}
          (fn [dir]
            (lock! dir {})
            (let [lock-text (slurp (io/file dir "deps.lock"))
                  resp (fix! dir [{:coord "com.rig.test/child"
                                   :locked "1.0.0"
                                   :fixes ["1.1.0"]}]
                             {:dry-sweep true})]
              (is (nil? (get resp "lock"))
                  "the dry re-derivation builds no lock document")
              (is (= [{:coord "com.rig.test/child"
                       :target "1.1.0"
                       :sites []
                       :pin {:section "overrides" :action "update"
                             :from "1.0.0" :value "1.1.0"}}]
                     (get resp "applied"))
                  "the fix updates the vulnerable override pin")
              (is (= "1.1.0" (get-in (manifest dir "deps.edn")
                                     [:rig/overrides 'com.rig.test/child]))
                  "the pin edit lands on disk")
              (is (= lock-text (slurp (io/file dir "deps.lock")))
                  "the lock file is untouched")
              (is (= {"com.rig.test/child"
                      {"modules/app" {"base" ["mvn" "1.0.0"]
                                      :test ["mvn" "1.1.0"]}}
                     "com.rig.test/parent"
                     {"modules/app" {"base" ["mvn" "1.0.0"]
                                     :test ["mvn" "1.0.0"]}}}
                    (get resp "sweep"))
                  "without the pin the base pulls the vulnerable 1.0.0;
                  the alias's own declaration stands"))))))))

(deftest pin-edit-drops-the-entry-and-the-emptied-section
  "The op's :pin-edits apply to the root's :rig/overrides: a nil
  requirement removes the entry — and the section itself with its last
  entry — and the call re-locks for real, carrying the lock and no
  sweep."
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
                                       :rig/overrides {'com.rig.test/child
                                                       "1.0.0"}})
                  "modules/app/deps.edn" (man url
                                              {:rig/lib "example/app"
                                               :paths ["src"]
                                               :deps {'com.rig.test/parent
                                                      {:mvn/version "1.0.0"}}})}
          (fn [dir]
            (lock! dir {})
            (let [resp (fix! dir [] {:pin-edits [{:coord "com.rig.test/child"
                                                  :requirement nil}]})]
              (is (nil? (get-in (manifest dir "deps.edn") [:rig/overrides]))
                  "the emptied section is removed with its entry")
              (is (nil? (get resp "sweep"))
                  "a real re-lock carries no sweep")
              (is (artifact-id? (get resp "lock")
                                "com.rig.test/child:1.0.0:jar")
                  "the transitive stays locked at its own version"))))))))

(deftest dry-sweep-short-circuits-without-version-pins
  "A :rig/overrides that carries no mvn version (an out-of-scope entry)
  gives the dry sweep nothing to test: the op re-locks for real, and the
  entry stands untouched."
  (let [now (System/currentTimeMillis)]
    (with-repo [["com.rig.test" "child"
                 {"1.0.0" {:published (old now)}
                  "1.1.0" {:published (old now)}}]]
      #{"central" "clojars"}
      (fn [url]
        (with-ws {"deps.edn" (man url {:rig/modules ["modules/app"]
                                       :rig/overrides {'com.rig.test/child
                                                       {:local/root "../lib"}}})
                  "modules/app/deps.edn" (man url
                                              {:rig/lib "example/app"
                                               :paths ["src"]
                                               :deps {'com.rig.test/child
                                                      {:mvn/version "1.0.0"}}})}
          (fn [dir]
            (lock! dir {})
            (let [resp (fix! dir [{:coord "com.rig.test/child"
                                   :locked "1.0.0"
                                   :fixes ["1.1.0"]}]
                             {:dry-sweep true})]
              (is (nil? (get resp "sweep"))
                  "no versioned pin: the real re-lock instead")
              (is (artifact-id? (get resp "lock")
                                "com.rig.test/child:1.1.0:jar"))
              (is (= {:local/root "../lib"}
                     (get-in (manifest dir "deps.edn")
                             [:rig/overrides 'com.rig.test/child]))
                  "the out-of-scope entry stands"))))))))

(deftest requirement-overrides-ride-the-relock
  "A caller's :requirement-overrides ride the op's re-lock ahead of the
  lock's own pins: with no targets the caller's override alone decides
  the locked version."
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
            (let [lock (lock! dir {})
                  resp (fix! dir []
                             {:requirement-overrides
                              {"com.rig.test/child" "1.1.0"}})]
              (is (artifact-id? lock "com.rig.test/child:1.0.0:jar")
                  "the recent 1.1.0 is out of cooldown; 1.0.0 locks")
              (is (artifact-id? (get resp "lock")
                                "com.rig.test/child:1.1.0:jar")
                  "the caller's override wins over the lock's pin"))))))))
