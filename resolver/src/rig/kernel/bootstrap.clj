(ns rig.kernel.bootstrap
  "Checkpoint bootstrap entry for the CRaC kernel (rig/internal/kernelrun).

  Runs one real request through the cold sequence, then parks inside
  CRaCMXBean.checkpointRestore() forever: the dump captures a fully-warmed
  kernel mid-park, the restore-side new main (rig.kernel.Runner) runs its
  op and RETURNS into this loop, which re-dumps the refreshed image before
  the engine kills the process. Parking in the ORIGINAL main's loop is the
  load-bearing contract rule — calling checkpointRestore from the
  restore-side main corrupts the image (spike crash, kernelrun package doc).

  Usage (kernelrun orchestrates it, after a successful cold op):
    java -XX:CRaCCheckpointTo=<dir> ... -cp <kernel.jar> rig.kernel.Bootstrap <request.json>"
  (:gen-class :name rig.kernel.Bootstrap)
  (:require [cheshire.core :as json]
            [rig.resolver.main :as main]))

(defn- checkpoint!
  "Park and dump via jdk.crac.management.CRaCMXBean. Reflection, not
  import: the uberjar is compiled on plain JDKs where the interface does
  not exist at compile time; only a CRaC JVM resolves it at runtime.
  Returns after a RESTORE resumed us; the engine kills us after the first
  dump, so returning means 'refresh this image and kill me again'."
  []
  (let [^Class c (Class/forName "jdk.crac.management.CRaCMXBean")
        get (.getMethod c "getCRaCMXBean" (into-array Class []))
        park (.getMethod c "checkpointRestore" (into-array Class []))]
    (.invoke park (.invoke get nil (object-array 0)) (object-array 0))))

(defn- warm!
  "Pay the lazy one-time costs before the dump so the image's first real
  op is not the first to pay them: the op namespaces are already required
  (and AOT-compiled) via rig.resolver.main; this round trip initializes
  cheshire's mappers, the reflection paths used on the JSON protocol."
  []
  (json/parse-string (json/generate-string {:warm (mapv str (range 200))}) true))

(defn -main
  [& [req-file]]
  (main/quiet-logging!)
  (warm!)
  (let [code (main/run-request-file req-file)]
    (.flush (System/out))
    (.flush (System/err))
    (when (pos? code)
      ;; A request that fails must not seed an image: exit like the cold
      ;; kernel would, kernelrun treats it as a silent warm failure.
      (System/exit code)))
  (loop []
    (try
      (checkpoint!)
      (catch Throwable t
        ;; No CRaC engine, or the dump failed: nothing worth parking for.
        ;; kernelrun decides image fate by core.img on disk, not this exit.
        (System/exit 0)))
    (recur)))
