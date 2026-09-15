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
