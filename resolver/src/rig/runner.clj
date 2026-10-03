(ns rig.runner
  "Hot-path runner. Launched on the project's locked classpath by rig
  (project classpath first, this jar last, so the project's own Clojure
  wins). Modes:

    test <exec-fn> <opts-file>
        Apply the EDN map read from opts-file to the exec-fn var
        (e.g. \"kaocha.runner/exec-fn\"). Exit 1 when it returns false,
        exit 1 (with a stack trace) when it throws, exit 0 otherwise.

    load <ns> ...
         Require each namespace with :reload. Exit 1 when any fails.

    load-all <src-dir> ...
         Scan the src dirs for .clj files, derive each namespace from its
         path (src/a/b.clj -> a.b), and require each with :reload.
         Exit 1 when any fails.

    aot <class-dir> <preload> ... -- <ns> ...
         Under *compile-files* with *compile-path* = class-dir, require
         each preload namespace, then AOT-compile each namespace after
         the \"--\" (skipping one a preload already loaded). Exit 1 when
         any fails."
  (:gen-class)
  (:require [clojure.edn :as edn]
            [clojure.java.io :as io]
            [clojure.string :as str]))

(defn- failure-message
  "The exception's message, then each cause in the chain: compilers wrap
  runtime errors (e.g. as 'Syntax error macroexpanding'), so the
  top-level message alone can hide the real problem."
  [e]
  (apply str (interpose "\n  caused by: "
                        (map (fn [x] (str (.getMessage x)))
                             (take-while identity (iterate #(.getCause ^java.lang.Throwable %) e))))))

(defn apply-exec-fn
  "Resolve exec-fn (e.g. \"kaocha.runner/exec-fn\"), loading its namespace
  when needed, and apply it to the single opts map. Returns the result."
  [exec-fn opts]
  (let [sym (symbol exec-fn)]
    (when (namespace sym)
      (try (require (symbol (namespace sym)))
           (catch Exception _ nil)))
    (let [f (resolve sym)]
      (when-not f
        (throw (ex-info (str "exec-fn not found: " exec-fn) {:exec-fn exec-fn})))
      (apply @f [opts]))))

(defn with-main-bindings
  "Run f under the dynamic bindings clojure.main establishes for user
  code: *print-namespace-maps* true. rig launches this jar's -main
  directly, skipping clojure.main, so without this the same code prints
  {:app/a 1} under rig test and #:app{:a 1} under clojure -M — suites
  asserting on printed maps flip. The var is resolved at runtime: a
  literal binding form would bake in a core field reference that
  projects pinned below 1.9 (the var's introduction) do not have."
  [f]
  (if-let [v (resolve 'clojure.core/*print-namespace-maps*)]
    (do
      (push-thread-bindings {v true})
      (try
        (f)
        (finally
          (pop-thread-bindings))))
    (f)))

(defn aot-args
  "Split the aot-mode argv into [preloads compiles]: the namespaces before
  the first \"--\" preload, the ones after it compile. split-with returns
  [taken dropped], so both halves come out of the same destructuring —
  taking the tail with & would nest `dropped` in one element and leave the
  compile list permanently nil."
  [args]
  (let [[preloads dropped] (split-with (complement #{"--"}) args)]
    [preloads (next dropped)]))

(defn- dispatch
  [mode rest]
  (case mode
    "test"
    (let [exec-fn (first rest)
          opts-file (second rest)
          opts (edn/read-string (slurp opts-file))]
      (try
        (let [result (apply-exec-fn exec-fn opts)]
          (flush)
          (System/exit (if (false? result) 1 0)))
        (catch Exception e
          (let [w (java.io.PrintWriter. (System/err))]
            (.printStackTrace e w)
            (.flush w))
          (System/exit 1))))

    "load"
    (let [failed (some (fn [ns-str]
                         (try
                           (require (symbol ns-str) :reload)
                           nil
                           (catch Exception e
                             (println (str "rig: failed to load " ns-str ": "
                                           (failure-message e)))
                             ns-str)))
                       rest)]
      (flush)
      (System/exit (if (nil? failed) 0 1)))

    "load-all"
    (let [ns-strs (->> (for [d rest
                             f (file-seq (io/file d))
                             :when (and (.isFile f)
                                        (or (str/ends-with? (.getName f) ".clj")
                                            (str/ends-with? (.getName f) ".cljc"))
                                        (not (str/ends-with? (.getName f) "_init.clj"))
                                        ;; A data-readables file (a top-level
                                        ;; map, not an ns form) is a classpath
                                        ;; convention, not a namespace:
                                        ;; requiring it fails to compile. Exact
                                        ;; basename match, so a real ns named
                                        ;; e.g. mydata_readers is unaffected.
                                        (not (#{"data_readers.clj" "data_readers.cljc"}
                                              (.getName f))))]
                         (str/join "."
                                   (map #(str/replace % #"\.cljc?$" "")
                                        (str/split (str (.relativize (.toPath (io/file d))
                                                                     (.toPath f)))
                                                   #"[\\/]"))))
                       distinct
                       sort)
          failed (some (fn [ns-str]
                         (try
                           (require (symbol ns-str) :reload)
                           nil
                           (catch Exception e
                             (println (str "rig: failed to load " ns-str ": "
                                           (failure-message e)))
                             ns-str)))
                       ns-strs)]
      (flush)
      (System/exit (if (nil? failed) 0 1)))

    "aot"
    (let [[class-dir & args] rest
          [preloads compiles] (aot-args args)
          fail (fn [op ns-str]
                 (try (op ns-str) nil
                      (catch Exception e
                        (println (str "rig: failed to load " ns-str ": "
                                      (failure-message e)))
                        ns-str)))
          failed (binding [*compile-files* true
                           *compile-path* class-dir]
                   (let [preload-fail
                         (some (fn [ns-str] (fail #(require (symbol %)) ns-str))
                               preloads)]
                     (or preload-fail
                         (some (fn [ns-str]
                                 (fail #(when-not (find-ns (symbol %))
                                          (compile (symbol %)))
                                       ns-str))
                               compiles))))]
      (flush)
      (System/exit (if (nil? failed) 0 1)))

    (do
      (binding [*out* *err*]
        (println (str "rig: unknown runner mode: " mode)))
      (System/exit 2))))

(defn -main
  "rig launches this directly, skipping clojure.main — dispatch the mode
  under clojure.main's bindings (see with-main-bindings)."
  [mode & rest]
  (with-main-bindings #(dispatch mode rest)))
