(ns rig.resolver.main
  (:require [cheshire.core :as json]
            [clojure.edn :as edn]
            [rig.resolver.aot :as aot]
            [rig.resolver.build :as build]
            [rig.resolver.check :as check]
            [rig.resolver.edit :as edit]
            [rig.resolver.git-sync]
            [rig.resolver.migrate :as migrate]
            [rig.resolver.outdated :as outdated]
            [rig.resolver.publish :as publish]
            [rig.resolver.resolve :as resolve]
            [rig.resolver.tree :as tree]
            [rig.resolver.versions :as versions]))

(defn quiet-logging! []
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

(defn- env-override
  "Parsed :env value for key k in request, nil when absent/blank."
  [request k]
  (some-> (get-in request [:env k]) not-empty edn/read-string))

(defn with-request-env
  "Bind the env dynvars from the request's :env map (raw environment
  strings, parsed like the environment path in versions.clj does).

  System/getenv is boot-cached inside a JVM, so a kernel restored from a
  CRaC checkpoint reads the BOOTSTRAP run's environment forever. With
  authoritative?, this JVM must not trust its own environment at all (it is
  a restore): absent :env entries bind to ABSENT rather than falling back —
  otherwise a tokenless workspace restoring an image that was bootstrapped
  by an authenticated one would read that workspace's tokens out of the
  heap. Without it (cold run, fresh environment) absent entries fall back
  as they always have, keeping pre-request-env callers working."
  [request authoritative? f]
  (let [tok (env-override request :RIG_REPO_TOKENS)
        prx (env-override request :RIG_PROXY_REPOS)
        f (if (or authoritative? (some? tok))
            #(binding [versions/*oidc-tokens* tok] (f))
            f)
        f (if (or authoritative? (some? prx))
            #(binding [versions/*proxy-repos* (or prx {})] (f))
            f)]
    (f)))

(defn- run-request
  "dispatch implementation; authoritative?=true marks the CRaC entry-point
  path (its JVM's environment belongs to the bootstrap run), see
  with-request-env."
  [request authoritative?]
  (let [thunk (case (keyword (:op request))
                :resolve #(resolve/resolve-lock request)
                :edit-dep #(edit/edit-dep request)
                :check #(check/check request)
                :aot-plan #(aot/aot-plan request)
                :migrate #(migrate/migrate request)
                :tree #(tree/tree request)
                :outdated #(outdated/outdated request)
                :build #(build/build request)
                :publish #(publish/publish request)
                ;; The throw must live INSIDE the thunk: it has to be
                ;; caught below and mapped to exit 2, not escape run-request.
                (fn []
                  (throw (ex-info (str "unknown op: " (:op request))
                                  {:rig/friendly? true :rig/bad-request? true}))))]
    (try
      (let [result (with-request-env request authoritative? #(with-quiet-out thunk))]
        (println (json/generate-string result))
        0)
      (catch Exception e
        (cond
          (true? (get (ex-data e) :rig/bad-request?))
          (do (.println (System/err) (.getMessage e))
              2)

          (true? (get (ex-data e) :rig/legacy?))
          (do (.println (System/err) (.getMessage e))
              3)

          (true? (get (ex-data e) :rig/friendly?))
          (do (.println (System/err) (.getMessage e))
              1)

          :else
          (let [w (java.io.PrintWriter. (System/err))]
            (.printStackTrace e w)
            (.flush w)
            1))))))

(defn dispatch
  "Run one request against the FRESH process environment: print the result
  JSON as the last stdout line, return the process exit code. Never calls
  System/exit: -main exits with the code, and the CRaC entry points
  (rig.kernel.bootstrap/runner, which reach this via run-request-file) must
  not — the runner returning IS the checkpoint-refresh trigger.

  Codes: 0 ok; 1 op failure (a :rig/friendly? error prints just its
  message, anything else its stack trace); 2 bad request; 3 manifest still
  uses legacy keys."
  [request]
  (run-request request false))

(defn run-request-file
  "Read a request JSON file and run it with the REQUEST as the authoritative
  env source (the CRaC entry points' environment, after a restore, is the
  bootstrap run's — see with-request-env). Returns the exit code; -main
  keeps its own stdin/usage handling."
  [path]
  (if-let [text (try (slurp path)
                     (catch Exception _ nil))]
    (try
      (run-request (json/parse-string text true) true)
      (catch Exception e
        (.println (System/err) (str "malformed request JSON: " (.getMessage e)))
        2))
    (do
      (.println (System/err) (str "cannot read request: " path))
      2)))

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
      (System/exit (dispatch request)))))
