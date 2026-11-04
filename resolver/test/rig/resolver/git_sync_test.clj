(ns rig.resolver.git-sync-test
  "The git-procure op: bad requests are 2, a provisionable commit returns
  ok, and failures are friendly (unknown commit, unreachable repo)."
  (:require [clojure.java.io :as io]
            [clojure.string :as str]
            [clojure.test :refer :all]
            [rig.resolver.git-sync :as git-sync]
            [rig.resolver.test-util :refer [delete-tree git]]))

(defn- git-repo
  "A git repo with one commit; returns [dir sha] as strings."
  []
  (let [repo (doto (java.io.File.
                    (str (java.nio.file.Files/createTempDirectory "rig-git-sync-test"
                                                                  (into-array java.nio.file.attribute.FileAttribute []))))
               (.deleteOnExit))
        _ (git (str repo) "init")
        _ (spit (io/file repo "file.txt") "x\n")
        _ (git (str repo) "add" "-A")
        _ (git (str repo) "commit" "-m" "x")]
    [(str repo) (git (str repo) "rev-parse" "HEAD")]))

(deftest procure-refuses-malformed-requests
  "url, lib and sha must be non-empty strings, and the sha a full 40-hex
  commit sha: the refusal is a bad request (kernel exit 2)."
  (doseq [args [[:nope "g/n" "ba1a2bf421838802e7bdefc541b41f57582e53b6"]
                ["file:///repo" :nope "ba1a2bf421838802e7bdefc541b41f57582e53b6"]
                ["file:///repo" "g/n" nil]
                ["file:///repo" "g/n" ""]
                ["file:///repo" "g/n" "ba1a2bf"]]]
    (let [e (try
              (apply git-sync/procure args)
              (catch Exception ex
                ex))]
      (is (instance? Exception e) (pr-str args))
      (is (true? (get (ex-data e) :rig/bad-request?))
          (str (pr-str args) ": " (.getMessage e))))))

(deftest procure-provisions-a-locked-commit
  "A commit the repo holds is checked out under the gitlibs root — the
  same checkout the lock builds; a re-procure adopts it and is idempotent."
  (let [[repo sha] (git-repo)
        lib (str "rig.gittest/" (java.util.UUID/randomUUID))]
    (try
      (let [checkout (git-sync/procure (str "file://" repo) lib sha)]
        (is (= {"ok" true} checkout) "the op reports ok")
        (let [dir (str (System/getenv "HOME") "/.gitlibs/libs/rig.gittest/" (last (str/split lib #"/")))]
          (is (.exists (io/file dir sha)) "the checkout is under <gitlibs>/libs/<group>/<name>/<sha>"))
        (is (= {"ok" true} (git-sync/procure (str "file://" repo) lib sha))
            "re-procuring adopts the existing checkout"))
      (finally
        (delete-tree (str (System/getenv "HOME") "/.gitlibs/libs/rig.gittest/" (last (str/split lib #"/"))))
        (delete-tree repo)))))

(deftest procure-fails-friendly-on-an-unknown-commit
  "A full sha the repo does not hold is a friendly failure (kernel exit 1),
  naming the short sha and the url."
  (let [[repo _] (git-repo)
        lib (str "rig.gittest/" (java.util.UUID/randomUUID))
        unknown "b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1"]
    (try
      (let [e (try
                (git-sync/procure (str "file://" repo) lib unknown)
                (catch Exception ex
                  ex))]
        (is (true? (get (ex-data e) :rig/friendly?)))
        (is (re-find #"commit \"b1b1b1b\" not found in file://" (.getMessage e))
            (.getMessage e)))
      (finally
        (delete-tree (str (System/getenv "HOME") "/.gitlibs/libs/rig.gittest/" (last (str/split lib #"/"))))
        (delete-tree repo)))))

(deftest procure-fails-friendly-on-an-unreachable-repo
  "A clone failure is a friendly failure naming the url and git's
  message."
  (let [lib (str "rig.gittest/" (java.util.UUID/randomUUID))]
    (try
      (let [e (try
                (git-sync/procure "file:///no-such-repo-rig-test" lib
                                  "b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1")
                (catch Exception ex
                  ex))]
        (is (true? (get (ex-data e) :rig/friendly?)))
        (is (str/includes? (.getMessage e) "git dep fetch failed"))
        (is (str/includes? (.getMessage e) "file:///no-such-repo-rig-test")))
      (finally
        (delete-tree (str (System/getenv "HOME") "/.gitlibs/libs/rig.gittest/" (last (str/split lib #"/"))))))))
