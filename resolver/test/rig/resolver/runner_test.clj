(ns rig.resolver.runner-test
  (:require [clojure.test :refer :all]
            [rig.runner :as runner]))

(defn fake-exec-fn [opts]
  (assoc opts :ran true))

(deftest apply-exec-fn-applies-opts
  (is (= {:k 1 :ran true}
         (runner/apply-exec-fn "rig.resolver.runner-test/fake-exec-fn" {:k 1}))))

(deftest apply-exec-fn-returns-exec-fn-result
  (is (= {:ran true}
         (runner/apply-exec-fn "rig.resolver.runner-test/fake-exec-fn" {}))))

(deftest apply-exec-fn-unknown-throws
  (is (thrown-with-msg? Exception
                        #"exec-fn not found"
                        (runner/apply-exec-fn "no.such/thing" {}))))

(deftest aot-args-splits-at-separator
  (let [[preloads compiles] (runner/aot-args ["dep.a" "dep.b" "--" "app.core" "app.util"])]
    (is (= ["dep.a" "dep.b"] preloads))
    (is (= ["app.core" "app.util"] compiles))))

(deftest aot-args-empty-sides
  (is (= [[] ["app.core"]] (runner/aot-args ["--" "app.core"])))
  (is (= [["dep.a"] nil] (runner/aot-args ["dep.a"])))
  (is (= [[] nil] (runner/aot-args []))))
