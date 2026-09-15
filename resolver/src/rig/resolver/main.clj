(ns rig.resolver.main
  (:require [cheshire.core :as json]
            [rig.resolver.build :as build]
            [rig.resolver.check :as check]
            [rig.resolver.edit :as edit]
            [rig.resolver.git-sync]
            [rig.resolver.migrate :as migrate]
            [rig.resolver.outdated :as outdated]
            [rig.resolver.publish :as publish]
            [rig.resolver.resolve :as resolve]
            [rig.resolver.tree :as tree]))

(defn- quiet-logging! []
  (try
    (let [ctx (-> (Class/forName "org.slf4j.LoggerFactory")
                  (.getMethod "getLoggerFactory")
                  (#(.invoke % nil)))
          root (when (= "ch.qos.logback.classic.Logger" (.getName (.getClass ctx)))
                 (.getLogger ctx "root"))
          warn (when root
                 (-> (Class/forName "ch.qos.logback.classic.Level")
                     (.getField "WARN")
                     (#(.get % nil))))]
      (when root
        (.setLevel root warn)
        (when-let [app (.getAppender root "console")]
          (.setTarget app "SYSTEM_ERR"))))
    (catch Throwable _ nil)))

(defn- die [code msg]
  (.println (System/err) msg)
  (System/exit code))

(defn- with-quiet-out
  "Run f with *out* on stderr: the kernel protocol is one JSON document on
  stdout, so op side effects (compiler warnings, AOT notes) must not reach
  it."
  [f]
  (binding [*out* (java.io.PrintWriter. (System/err) true)]
    (f)))

(defn- run-op
  "Run op f, printing the result JSON and exiting. A manifest that still
  uses legacy keys exits 3 with the guard message on stderr; any other
  failure prints the stack trace and exits 1."
  [f]
  (try
    (let [result (with-quiet-out f)]
      (println (json/generate-string result))
      (System/exit 0))
    (catch Exception e
      (if (true? (get (ex-data e) :rig/legacy?))
        (do (.println (System/err) (.getMessage e))
            (System/exit 3))
        (let [w (java.io.PrintWriter. (System/err))]
          (.printStackTrace e w)
          (.flush w)
          (System/exit 1))))))

(defn -main [& args]
  (quiet-logging!)
  (let [i (.indexOf (vec args) "--request")
        req (when (>= i 0) (nth args (inc i)))]
    (when (nil? req)
      (die 2 "usage: rig-resolver --request <file|->"))
     (let [text (try (if (= req "-") (slurp *in*) (slurp req))
                    (catch java.io.IOException e
                      (die 2 (str "cannot read request: " (.getMessage e)))))
           request (try (json/parse-string text true)
                        (catch Exception e
                          (die 2 (str "malformed request JSON: " (.getMessage e)))))]
      (case (keyword (:op request))
           :resolve
           (run-op #(resolve/resolve-lock request))
           :edit-dep
           (run-op #(edit/edit-dep request))
           :check
           (run-op #(check/check request))
           :migrate
           (run-op #(migrate/migrate request))
           :tree
           (run-op #(tree/tree request))
           :outdated
           (run-op #(outdated/outdated request))
           :build
           (run-op #(build/build request))
           :publish
           (run-op #(publish/publish request))
        (die 2 (str "unknown op: " (:op request)))))))
