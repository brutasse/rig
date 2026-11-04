(ns rig.resolver.git-sync
  "Serializes tools.gitlibs cache operations under concurrent use.

  tools.deps expands dependency subtrees in parallel (pmap inside
  expand-deps), so a single create-basis can hit the same git coordinate
  from multiple threads at once. tools.gitlibs' cache primitives are
  check-then-act (bare mirror clone, worktree checkout) and are not
  thread-safe: concurrent clones collide (\"destination path already
  exists\", and on some git versions the mirror aborts with SIGABRT,
  corrupting it for later serial retries), and concurrent worktree
  adds can leave a half-populated checkout.

  Wrapping the public entry points with one JVM-wide lock makes every
  cache operation atomic; the locked section is network-bound anyway,
  and contention is only paid while the same coordinate is being
  provisioned in parallel. The lock is reentrant, so a re-load of this
  ns cannot double-deadlock."
  (:require [clojure.tools.gitlibs :as gitlibs]))

(def ^:private lock (Object.))

(doseq [v [#'gitlibs/tags #'gitlibs/resolve #'gitlibs/procure]]
  (let [orig @v]
    (alter-var-root v (constantly (fn [& args] (locking lock (apply orig args)))))))

(defn procure
  "Provision one locked git dep's checkout under the gitlibs root — the
  same tools.gitlibs procure the lock uses: mirror clone plus a worktree
  at the sha, adopting an existing checkout. Idempotent; safe to run on
  every hot command. A commit the mirror does not hold, or a failed
  fetch, throws a friendly error (the caller re-locks online or restores
  the cache)."
  [url lib sha]
  (if (or (not (string? url)) (empty? url)
          (not (string? lib)) (empty? lib)
          (not (string? sha)) (empty? sha))
    (throw (ex-info "git-procure: url, lib and sha must be non-empty strings"
                    {:rig/bad-request? true}))
    (if (not (re-matches #"^[0-9a-f]{40}$" sha))
      (throw (ex-info (str "git-procure: sha must be a full 40-hex commit sha (got \""
                           sha "\")")
                      {:rig/bad-request? true}))
      (try
        (if (gitlibs/procure url (symbol lib) sha)
          {"ok" true}
          (throw (ex-info (str "commit \"" (subs sha 0 7)
                               "\" not found in " url)
                          {:rig/friendly? true})))
        (catch Throwable e
          (throw (ex-info (str "git dep fetch failed: " url
                               (when-let [m (.getMessage e)]
                                 (str " — " m)))
                          {:rig/friendly? true})))))))
