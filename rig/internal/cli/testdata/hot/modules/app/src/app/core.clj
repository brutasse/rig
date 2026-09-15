(ns app.core
  (:gen-class))

(defn add [a b] (+ a b))

(defn -main [& args]
  (println (str "hello " (apply str args))))
