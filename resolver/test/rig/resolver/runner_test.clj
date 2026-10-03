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

(deftest with-main-bindings-flips-namespace-maps
  (let [unbound (binding [*print-namespace-maps* false]
                  (with-out-str (pr {:app/a 1})))
        bound (binding [*print-namespace-maps* false]
                (with-out-str (runner/with-main-bindings #(pr {:app/a 1}))))]
    (is (= "{:app/a 1}" unbound))
    (is (= "#:app{:a 1}" bound))))
