(ns rig.resolver.pom
  "Maven coordinate metadata for published and built artifacts (design §7.3).

  One renderer backs two outputs: the .pom that `rig publish` deploys, and
  the copy the build embeds in every jar of a module with coordinates —
  META-INF/maven/<group>/<artifact>/pom.{xml,properties}, the convention
  jar scanners attribute through and self-versioning code reads
  (`io/resource` on pom.properties is the classic version command). Same
  coordinates, same pinned deps: the coordinates a jar carries inside it
  are the ones it deploys under."
  (:require [clojure.string :as str]))

(defn- xml-esc
  [s]
  (-> (str s)
      (str/replace "&" "&amp;")
      (str/replace "<" "&lt;")
      (str/replace ">" "&gt;")))

(defn pom-deps
  "The module's direct mvn dependencies as [[group artifact] version…].
  Git/local deps are not in a POM. Floating requirements (RELEASE/…) take
  the lock's pinned version."
  [data pins]
  (->> (get data :deps {})
       (keep (fn [[coord spec]]
               (when (or (string? spec) (get spec :mvn/version))
                 (let [[group artifact] (str/split (str coord) #"/" 2)
                       declared (or (when (string? spec) spec)
                                    (get spec :mvn/version))]
                   [group artifact (or (get pins (str coord)) declared)]))))
       (vec)))

(defn pins-of
  "coord -> version over the lock's mvn artifacts."
  [lock]
  (into {}
        (for [a (get lock :artifacts)
              :when (= "mvn" (get a :kind))]
          [(str (get a :group) "/" (get a :name)) (get a :version)])))

(defn pom
  [group artifact version deps]
  (str "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"
       "<project xmlns=\"http://maven.apache.org/POM/4.0.0\">\n"
       "  <modelVersion>4.0.0</modelVersion>\n"
       (format "  <groupId>%s</groupId>\n" (xml-esc group))
       (format "  <artifactId>%s</artifactId>\n" (xml-esc artifact))
       (format "  <version>%s</version>\n" (xml-esc version))
       "  <packaging>jar</packaging>\n"
       (when (seq deps)
         (str "  <dependencies>\n"
              (apply str
                     (for [[g a v] deps]
                       (str "    <dependency>\n"
                            (format "      <groupId>%s</groupId>\n" (xml-esc g))
                            (format "      <artifactId>%s</artifactId>\n" (xml-esc a))
                            (format "      <version>%s</version>\n" (xml-esc v))
                            "    </dependency>\n")))
              "  </dependencies>\n"))
       "</project>\n"))

(defn- properties
  "The pom.properties content, maven's own key order."
  [group artifact version]
  (str "version=" version "\n"
       "groupId=" group "\n"
       "artifactId=" artifact "\n"))

(defn embedded
  "The maven-convention entries for a module's coordinates, as
  {path content} pairs to stamp into the build's class-dir — the jar, the
  uber and the native image all carry them from there (see
  rig.resolver.build/write-pom-descriptors)."
  [group artifact version deps]
  (let [base (str "META-INF/maven/" group "/" artifact "/")]
    {(str base "pom.xml") (pom group artifact version deps)
     (str base "pom.properties") (properties group artifact version)}))
