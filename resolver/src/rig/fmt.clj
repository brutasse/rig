(ns rig.fmt
  "CLI shim for the cljfmt bundled in this jar, so 'rig fmt' runs a pinned
  cljfmt via java without a second download. Forwards the whole command line
   (\"check\"/\"format\" plus cljfmt options) to cljfmt.main/-main, which
   prints and exits itself."
  (:gen-class)
  (:require [cljfmt.main :as m]))

(defn -main [& args]
  (apply m/-main (vec args)))
