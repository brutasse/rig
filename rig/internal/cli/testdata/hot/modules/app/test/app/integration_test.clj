(ns ^:integration app.integration-test
  (:require [app.core :as core]
            [clojure.test :refer [deftest is]]))

(deftest this-fails
  (is false))
