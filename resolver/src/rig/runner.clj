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
         Exit 1 when any fails."
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
                             (take-while identity (iterate ex-cause e))))))

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

(defn -main [mode & rest]
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
                                        (str/ends-with? (.getName f) ".clj")
                                        (not (str/ends-with? (.getName f) "_init.clj")))]
                        (str/join "."
                                 (map #(str/replace % #"\.clj$" "")
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

    (do
      (binding [*out* *err*]
        (println (str "rig: unknown runner mode: " mode)))
      (System/exit 2))))
