(ns rig.resolver.audit-fix
  "Kernel op: plan and apply the CVE fixes of `rig audit --fix`.

  One request, one re-lock: every fixable coordinate's edits — its
  vulnerable declaration sites (the module manifests) and, when the fix
  needs it, its pool pin (the root's :rig/deps) — are applied, then the
  workspace resolves once. A pool pin the coordinate does not already
  carry is a new manifest entry: it stands with the plan reason
  \"transitive\" unless :transitive is set. A coordinate a manifest
  already pins at a clean version stands \"stale-lock\": its remedy is the
  re-lock that picks the pin up, not an edit.

  Request:  {op \"audit-fix\" workspace ws lock \"deps.lock\"
             args {:targets [{coord g/n locked v fixes [f …] breaking? b}]  ; one
                                                            ; entry per vulnerable
                                                            ; (coordinate, locked version)
                  :offline bool :force bool :transitive bool}}
  Response: {plan    [{coord locked target reason}]        ; one row per input entry:
                                                            target nil when reason set
                                                            (no-fix, breaking, below-locked,
                                                            fix-unavailable, transitive,
                                                            stale-lock)
           applied   [{coord target sites pool}]           ; per applied coordinate: the
                                                            edit sites and the pool pin
                                                            (pool nil when none is written)
           edits     […]}                                  ; the manifest edits, edit-dep
                                                            record shape (absent when no
                                                            edit applied)
           lock      {...}                                 ; the new lock document (absent
                                                            when no edit applied)
           refused   […]}                                  ; the resolve-lock refusal shape"
  (:require [clojure.java.io :as io]
            [rig.resolver.edit :as edit]
            [rig.resolver.manifest :as manifest]
            [rig.resolver.resolve :as resolve]
            [rig.resolver.versions :as versions]))

(defn- coord-sym
  [c]
  (symbol (str c)))

(defn- entry-version
  "A deps entry's version: the spec itself when it is a string, its
  :mvn/version when it is a map (nil for a git or local spec)."
  [s]
  (if (map? s)
    (get s :mvn/version)
    (when (string? s) s)))

(defn- declaration-sites
  "Where a manifest's data declares coord: one row per entry of its :deps
  and of every alias's :deps, :extra-deps and :replace-deps —
  {:section s :alias a :version v} (version nil for a non-mvn spec, alias
  nil for the module's own :deps)."
  [data coord]
  (let [sym (coord-sym coord)
        one (fn [section alias s]
              {:section section :alias alias :version (entry-version s)})]
    (concat (when-let [s (get (get data :deps {}) sym)]
              [(one "deps" nil s)])
            (for [al (keys (get data :aliases {}))
                  [k section] [[:extra-deps "extra-deps"]
                               [:replace-deps "replace-deps"]
                               [:deps "alias-deps"]]
                  :let [s (get-in data [:aliases al k sym])]
                  :when s]
              (one section (name al) s)))))

(defn- pool-declared?
  "True when a module declares coord where the :rig/deps pool does not
  force-pin it (resolve/pool-overrides' definition: its :deps or an
  alias's :deps/:extra-deps carries the coordinate) — a module like that
  ignores a pool entry for the coordinate."
  [data coord]
  (let [sym (coord-sym coord)]
    (boolean
     (or (contains? (get data :deps {}) sym)
         (some (fn [al]
                 (or (contains? (get-in data [:aliases al :deps] {}) sym)
                     (contains? (get-in data [:aliases al :extra-deps] {}) sym)))
               (keys (get data :aliases {})))))))

(defn- id-meta
  "The lock's mvn artifacts as {id [coord version]}."
  [lock]
  (into {}
        (for [a (get lock :artifacts [])
              :when (= "mvn" (get a :kind))]
          [(get a :id)
           [(str (get a :group) "/" (get a :name)) (get a :version)]])))

(defn- module-pulls?
  "True when a lock module's classpaths (the base and every alias) carry
  coord at one of vuln-versions."
  [meta lock-module coord vuln-versions]
  (boolean
   (some (fn [ref]
           (let [m (get meta ref)]
             (and (some? m)
                  (= (first m) coord)
                  (contains? vuln-versions (second m)))))
         (concat (get-in lock-module [:classpath] [])
                 (mapcat #(get % :classpath)
                         (vals (get-in lock-module [:aliases] {})))))))

(defn- site-vulnerable?
  "True when the site carries a vulnerable version of its coordinate: its
  pin is one of vuln-versions, or it is floating and the module's
  classpaths pull one in."
  [site vuln-versions meta lock-module coord]
  (let [v (:version site)]
    (cond
      (nil? v) false
      (versions/floating? v)
      (module-pulls? meta lock-module coord vuln-versions)
      :else
      (contains? vuln-versions v))))

(defn- pool-write
  "The pool edit a coordinate needs: {:action … :from p :value v} when the
  root's :rig/deps entry must carry the fix — its own version is
  vulnerable (value: the fix, never below the entry's own), or the entry
  is absent and some module pulls a vulnerable version without declaring
  the coordinate (the pool is the only reach into such a module) — else
  nil (a clean entry already pins every non-declaring module, and a
  declaring module's pin is a site edit, not a pool's business)."
  [root-data coord target vuln-versions datas lock-modules meta]
  (let [p (entry-version (get (get root-data :rig/deps {}) (coord-sym coord)))
        needed? (some (fn [m]
                        (and (module-pulls? meta (get lock-modules m) coord vuln-versions)
                             (not (pool-declared? (get datas m) coord))))
                      (keys datas))]
    (cond
      (and p (contains? vuln-versions p))
      {:action "update" :from p :value (if (neg? (versions/vcmp target p)) p target)}

      (and (nil? p) needed?)
      {:action "add" :from nil :value target}

      :else
      nil)))

(defn- all-repos-of
  "Every module's :mvn/repos plus rig's standard repos (a redeclared id
  replaced) — the union the re-lock resolves against, the resolve-lock
  construction."
  [datas]
  (let [extra (distinct (mapcat (fn [data]
                                  (for [[id sp] (get data :mvn/repos {})] [id sp]))
                                (vals datas)))]
    (into (versions/standard-repos-of (set (map first extra)))
          (sort-by first extra))))

(defn- edit-shape
  "The manifest edits a coordinate's fix requires: its vulnerable
  declaration sites (one row per site, the edit record shape), and the
  pool write the fix needs (nil when none). A pool \"add\" is the only
  edit a fix may require that is not a bump of an entry that is already
  in a manifest — the gate of audit-fix."
  [root-data coord target vulns datas lock-modules meta]
  {:sites (vec
           (for [m (sort (keys datas))
                 s (declaration-sites (get datas m) coord)
                 :when (site-vulnerable? s vulns meta (get lock-modules m) coord)]
             {:file (if (= m ".") "deps.edn" (str m "/deps.edn"))
              :module m
              :section (:section s)
              :alias (:alias s)
              :from (:version s)}))
   :pool (pool-write root-data coord target vulns datas lock-modules meta)})

(defn- pinned-clean?
  "True when a manifest already pins coord at a version the findings do
  not carry: the root's :rig/deps entry, or a module declaration (a
  git or local spec pins no version and counts as unpinned)."
  [root-data coord vulns datas]
  (let [pv (entry-version (get (get root-data :rig/deps {}) (coord-sym coord)))]
    (boolean
     (or (and (some? pv) (not (contains? vulns pv)))
         (some (fn [m]
                 (some (fn [s]
                         (and (some? (:version s))
                              (not (contains? vulns (:version s)))))
                       (declaration-sites (get datas m) coord)))
               (keys datas))))))

(defn audit-fix
  [request]
  (let [ws (str (:workspace request))
        args (or (:args request) {})
        targets (get args :targets [])
        breaking (true? (get args :breaking))
        transitive (true? (get args :transitive))
        lock (resolve/lock-of request)
        meta (id-meta lock)
        ;; lock-of keywordizes the document, so the module keys are keywords
        ;; (:modules/app, :.) — normalize them back to the module-dir strings
        ;; the manifests and datas are keyed by.
        lock-modules (into {}
                           (for [[k v] (get lock :modules {})]
                             [(str (when-let [n (namespace k)] (str n "/"))
                                   (name k)) v]))
        root-man (manifest/read-manifest ws)
        root-data (or (:data root-man) {})
        module-dirs (manifest/modules-of root-data)
        declared-abs (into {} (for [m module-dirs]
                                [m (let [d (if (= m ".") ws (str (io/file ws m)))]
                                     (try (some-> d (io/file) (.getCanonicalPath) str)
                                          (catch Exception _ d)))]))
        local-mods (resolve/local-modules ws module-dirs declared-abs root-data)
        modules-abs (into declared-abs local-mods)
        datas (into {} (for [m (keys modules-abs)
                             :let [man (manifest/read-manifest (get modules-abs m))]
                             :when (some? (:data man))]
                         [m (or (:data man) {})]))
        by-coord (group-by :coord (for [t targets]
                                    {:coord (get t :coord)
                                     :locked (get t :locked)
                                     :fixes (get t :fixes)
                                     :breaking (or (true? (get t :breaking)) breaking)}))
        ;; One plan row per input entry: the target is picked over the
        ;; coordinate's whole fix set, so every entry of a coordinate sees
        ;; the same candidates.
        rows0 (mapcat (fn [[coord entries]]
                        (let [f-all (apply concat (map :fixes entries))
                              brk (some :breaking entries)]
                          (for [e entries]
                            (merge {:coord coord :locked (:locked e)}
                                   (versions/pick-fix (:locked e) f-all brk)))))
                      (sort-by (fn [[c _]] c) by-coord))
        ;; A fix version the repositories do not serve cannot be
        ;; re-locked: OSV records name fix versions per artifact, and a
        ;; version published only under a successor coordinate never
        ;; resolves. The coordinate then stands unfixable instead of
        ;; failing the whole op — the single applied target is the
        ;; newest of the picked ones, so that is the one checked, and a
        ;; metadata fetch that fails reads as not-served (the re-lock
        ;; is the last gate).
        fixable (into {}
                      (for [[coord rs] (group-by :coord rows0)
                            :let [ts (keep :target rs)]
                            :when (and (seq ts)
                                       (try (boolean
                                             (get-in (versions/repo-metadata (str coord)
                                                                             (all-repos-of datas))
                                                     [:by-version (versions/newest ts)]))
                                            (catch Exception _ false)))]
                        [coord (versions/newest ts)]))
        ;; The edits each fixable coordinate requires — and the gate: a
        ;; pool "add" creates an entry no manifest carries, it stands
        ;; without :transitive, whatever the sites do. A site edit and a
        ;; pool "update" touch entries that are already in a manifest:
        ;; they apply whatever the flag says. The row itself stands
        ;; transitive only when the add was the coordinate's only edit.
        shapes (into {}
                     (for [[coord target] fixable
                           :let [rows (get (group-by :coord rows0) coord)
                                 vulns (into #{} (keep :locked (filter (comp some? :target) rows)))
                                 sh (edit-shape root-data coord target vulns
                                                datas lock-modules meta)
                                 stands (and (not transitive)
                                             (some? (:pool sh))
                                             (= "add" (:action (:pool sh))))]]
                       [coord (assoc sh :vulns vulns
                                     :pool (if stands nil (:pool sh))
                                     :gated (and stands (empty? (:sites sh))))]))
        rows (for [r rows0
                   :let [coord (get r :coord)
                         fix (get fixable coord)
                         sh (get shapes coord)]]
               (cond
                 (and (some? (:target r)) (nil? fix))
                 {:coord coord :locked (get r :locked) :reason "fix-unavailable"}

                 (and (some? (:target r)) (some? sh)
                      (not (or (seq (:sites sh)) (some? (:pool sh)))))
                 ;; The fix has no edit to apply: a gated pool "add"
                 ;; stands transitive, a manifest that already pins the
                 ;; coordinate at a clean version stands stale-lock —
                 ;; the re-lock, not an edit, is its remedy.
                 {:coord coord :locked (get r :locked)
                  :reason (if (or (:gated sh)
                                  (not (pinned-clean? root-data coord (:vulns sh) datas)))
                            "transitive"
                            "stale-lock")}

                 :else r))
        applied (vec
                 (for [[coord target] (sort-by key fixable)
                       :let [sh (get shapes coord)]
                       :when (or (seq (:sites sh)) (some? (:pool sh)))]
                   {:coord coord :target target :sites (:sites sh) :pool (:pool sh)}))
        edits-spec (concat
                    (for [a applied
                          s (:sites a)]
                      {:file (:file s)
                       :coord (:coord a)
                       :requirement (:target a)
                       :section (:section s)
                       :alias (:alias s)})
                    (for [a applied
                          :let [p (:pool a)]
                          :when (some? p)]
                      {:file "deps.edn"
                       :coord (:coord a)
                       :requirement (:value p)
                       :section "pool"}))
        edits (when (seq edits-spec) (edit/apply-edits ws edits-spec))
        relock (resolve/resolve-lock
                (assoc request
                       :args (assoc args
                                    :requirement-overrides
                                    (into {} (for [a applied]
                                               [(:coord a) (:target a)])))))]
    (cond-> (cond-> {"plan" rows
                     "applied" applied}
              (seq edits) (assoc "edits" edits))
      (seq edits)
      (merge (select-keys relock ["lock"])
             (when (seq (get relock "refused"))
               {"refused" (vec (get relock "refused"))})))))
