(ns app.core
  (:gen-class))

(defn -main [& args]
  (println (str "native hello " (apply str args))))
