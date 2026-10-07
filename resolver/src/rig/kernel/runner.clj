(ns rig.kernel.runner
  "Restore-side entry for the CRaC kernel (rig/internal/kernelrun).

  Runs one fresh request on the warmed, restored kernel. SUCCESS RETURNS:
  returning from main lands back in rig.kernel.bootstrap's parked
  checkpointRestore loop, which re-dumps the refreshed image and the engine
  kills the process (exit-kill + core.img on disk is kernelrun's success
  signal). Op failures System/exit with the cold kernel's codes instead, so
  a failing op never rewrites the image."
  (:gen-class :name rig.kernel.Runner)
  (:require [rig.resolver.main :as main]))

(defn -main
  "java -cp <kernel.jar> rig.kernel.Runner <request.json>"
  [& [req-file]]
  (let [code (main/run-request-file req-file)]
    ;; The spike flushes both streams here: buffered result bytes must not
    ;; sit in heap buffers across the refresh dump (they would replay into
    ;; the next generation's image).
    (.flush (System/out))
    (.flush (System/err))
    (when (pos? code)
      (System/exit code))))
