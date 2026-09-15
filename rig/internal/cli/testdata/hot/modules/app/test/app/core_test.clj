(ns app.core-test
  (:require [app.core :as core]
            [clojure.test :refer [deftest is]]))

(deftest add-works
  (is (= 3 (core/add 1 2))))

(deftest add-again
  (is (= 4 (core/add 2 2))))
