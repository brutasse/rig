(ns rig.resolver.manifest-test
  (:require [clojure.java.io :as io]
            [clojure.string :as str]
            [clojure.test :refer :all]
            [clojure.tools.build.api :as b]
            [rig.resolver.manifest :as manifest]
            [rig.resolver.test-util :refer [temp-dir]]))

(deftest version-fn-template-trailing-newline-is-not-the-version
  "A newline-terminated template file is the normal case; the LF must not
  end up in the version (and from there in the artifact filename)."
  (with-redefs [b/git-count-revs (constantly "42")]
    (let [dir (temp-dir)]
      (spit (io/file dir "VERSION_TEMPLATE") "1.0.GENERATED_VERSION\n")
      (is (= "1.0.42"
             (manifest/read-version {:rig/version-fn :git-count-revs}
                                    (.getPath dir) (.getPath dir)))))))

(deftest version-file-first-non-blank-line-wins
  (let [dir (temp-dir)]
    (spit (io/file dir "VERSION") "\n1.2.3\nnext line ignored\n")
    (is (= "1.2.3" (manifest/read-version {} (.getPath dir) (.getPath dir))))))

(deftest read-manifest-refuses-a-removed-key
  "A removed :rig/* key is refused like a legacy one: the manifest names
  the file and the fix, and the refusal marks the removed-key exit code."
  (let [dir (temp-dir)]
    (spit (io/file dir "deps.edn")
          "{:rig/lib x/y\n :rig/ns-compile [a.b c.d]}\n")
    (let [e (try
              (manifest/read-manifest (.getPath dir))
              (catch Exception ex
                ex))]
      (is (instance? Exception e))
      (is (true? (get (ex-data e) :rig/removed?)))
      (is (re-find #"uses removed key :rig/ns-compile" (.getMessage e))))))

(deftest read-manifest-refuses-legacy-keys-before-removed-ones
  "With both a legacy key and a removed key present the legacy refusal
  wins: migrate is the single command that fixes both."
  (let [dir (temp-dir)]
    (spit (io/file dir "deps.edn")
          "{:exoscale.project/lib x/y\n :rig/ns-compile [a.b]}\n")
    (let [e (try
              (manifest/read-manifest (.getPath dir))
              (catch Exception ex
                ex))]
      (is (true? (get (ex-data e) :rig/legacy?)))
      (is (nil? (get (ex-data e) :rig/removed?))))))

(deftest auto-git-url-derives-for-the-known-hosts
  "The derivation table mirrors tools.deps' auto-git-url: each known host
  resolves, an unknown group does not."
  (is (= "https://github.com/clojure/tools.deps.git"
         (manifest/auto-git-url "io.github.clojure/tools.deps")))
  (is (= "https://gitlab.com/example/lib.git"
         (manifest/auto-git-url "com.gitlab.example/lib")))
  (is (= "https://codeberg.org/example/lib.git"
         (manifest/auto-git-url "org.codeberg.example/lib")))
  (is (= "https://bitbucket.org/example/lib.git"
         (manifest/auto-git-url "org.bitbucket.example/lib")))
  (is (= "https://example.git.beanstalkapp.com/lib.git"
         (manifest/auto-git-url "com.beanstalkapp.example/lib")))
  (is (= "https://git.sr.ht/~ht/lux/hug-strength"
         (manifest/auto-git-url "ht.sr.ht/lux/hug-strength")))
  (is (nil? (manifest/auto-git-url "com.example/lib")))
  (is (nil? (manifest/auto-git-url "com.bitbucket.example/lib")))
  (is (nil? (manifest/auto-git-url "lib"))))

(deftest git-ref-problems-refuses-every-non-pin-form
  "A git coord must pin a full commit sha: each refused form names its fix,
  and alias-declared specs are flagged with their location."
  (let [full "ba1a2bf421838802e7bdefc541b41f57582e53b6"
        url (str "file:///repo-" (java.util.UUID/randomUUID))]
    (is (empty? (manifest/git-ref-problems
                 {:deps {'com.example/lib {:git/url url :git/sha full}}}))
        "a full sha is clean")
    (is (empty? (manifest/git-ref-problems
                 {:deps {'io.github.example/lib {:git/sha full}}}))
        "a full sha on a known host is clean")
    (doseq [[label spec substr]
            [["tag" {:git/url url :git/tag "v1"} ":git/tag is not supported"]
             ["branch" {:git/url url :git/branch "main"} ":git/branch is not supported"]
             ["short-sha" {:git/url url :git/sha "ba1a2bf"} "must be a full 40-hex"]
             ["legacy-tag" {:git/url url :tag "v1" :git/sha full} ":tag is the legacy form"]
             ["legacy-sha" {:git/url url :sha full} ":sha is the legacy form"]
             ["no-pin" {:git/url url} "no commit pin"]
             ["no-url" {:git/sha full} "declare :git/url"]]]
      (let [ps (manifest/git-ref-problems {:deps {'com.example/lib spec}})]
        (is (seq ps) label)
        (is (some #(str/includes? (:message %) substr) ps)
            (str label ": " (pr-str (map :message ps))))
        (is (every? #(= "deps" (:loc %)) ps) label)))
    (let [ps (manifest/git-ref-problems
              {:aliases {:t {:extra-deps {'com.example/lib {:git/tag "v1"}}
                             :replace-deps {'com.example/lib {:git/sha "x"}}}}})]
      (is (some #(= "alias t extra-deps" (:loc %)) ps) "alias extra-deps")
      (is (some #(= "alias t replace-deps" (:loc %)) ps) "alias replace-deps")
      (is (some #(and (= "alias t extra-deps" (:loc %))
                      (str/includes? (:message %) "no commit pin")) ps)))
    (is (empty? (manifest/git-ref-problems {:deps {}})))
    (is (empty? (manifest/git-ref-problems {})))))

(deftest git-ref-problem-line-formats
  "lock and tree print \"coord: loc: message\"; the deps location carries no
  prefix."
  (let [p {:loc "deps" :coord 'com.example/lib :message "no commit pin — x"}
        q {:loc "alias t extra-deps" :coord 'com.example/lib :message "no commit pin — x"}]
    (is (= "com.example/lib: no commit pin — x"
           (manifest/git-ref-problem-line p)))
    (is (= "com.example/lib: alias t extra-deps: no commit pin — x"
           (manifest/git-ref-problem-line q)))))

(deftest git-deps-map-keys-on-the-sha-and-records-the-url
  "The lock's merge entries: \"group/name/sha\" -> {:url url :deps/root
  root}, the url the manifest's :git/url or the host-derived one. Refused
  specs contribute nothing."
  (let [full "ba1a2bf421838802e7bdefc541b41f57582e53b6"
        url "file:///repo"
        d {:deps {'com.example/lib {:git/url url
                                    :git/sha full
                                    :deps/root "clj"}
                  'io.github.example/lib {:git/sha full}}}]
    (is (= {(str "com.example/lib/" full) {:url url :deps/root "clj"}
            (str "io.github.example/lib/" full) {:url "https://github.com/example/lib.git" :deps/root nil}}
           (manifest/git-deps-map d)))))