# Migrating from Leiningen

You have a project with a `project.clj` — `lein test`, `lein run`,
`lein uberjar`, maybe a Makefile that wraps them. No tools.project, no
deps-modules, no `deps.edn` yet. `rig migrate` converts the manifest in
one command: it writes a new root `deps.edn` in the `:rig/*` model and
leaves `project.clj` untouched.

## What you have today

A typical `project.clj`:

```clojure
(def version (slurp "VERSION"))
(defproject com.example/example version
 :dependencies [[org.clojure/clojure "1.12.1"]
                [some/tool "0.4.0"]]
 :repositories {"my-registry" {:url "https://registry.example.com"}}
 :profiles {:test {:plugins [[lein-test-report-junit-xml "0.2.0"]]}
            :dev {:jvm-opts ["-Dexample.debug=true"]
                  :resource-paths ["test/resources"]
                  :dependencies [[lambdaisland/kaocha "1.0.669"]]}
            :docgen {:dependencies [[codox "0.10.8"]]}
            :uberjar {:aot :all}}
 :main example.main
 :plugins [[some/deploy-wagon "1.0.2"]]
 :resource-paths ["resources"])
```

What Rig gives you in exchange: a lockfile with hash-pinned,
full-tree-resolved dependencies, offline builds, and a single command
surface (`rig test`, `rig build`, `rig publish`). What it does not: the
Leiningen plugin and task machinery. Nothing in Rig executes plugins, so
Rig drops that surface with a warning, not a translation.

## What `rig migrate` does

```sh
rig migrate --dry-run   # preview the warnings; nothing is written
rig migrate             # writes deps.edn; project.clj is not touched
```

One command, one `deps.edn` per manifest: the root, plus one per `:sub`
module. If both `deps.edn` and `project.clj` exist, `deps.edn` wins (the
legacy/tools.deps migration path runs instead); if neither does, the
command fails. You can run `rig migrate` again after the conversion. The
generated manifest has no legacy content left, so the second run reports
nothing to do.

The generated manifest for the example above:

```edn
{:rig/lib com.example/example
 :rig/main example.main
 :rig/uberjar? true
 :mvn/repos {"my-registry" {:url "https://registry.example.com"}}
 :paths ["src" "resources"]
 :deps {org.clojure/clojure {:mvn/version "1.12.1"}
        some/tool {:mvn/version "0.4.0"}}
 :aliases
 {:test {:extra-deps {lambdaisland/kaocha {:mvn/version "1.66.1034"}}
         :extra-paths ["test/resources" "test"]
         :jvm-opts ["-Dexample.debug=true"]
         :exec-fn kaocha.runner/exec-fn}
  :dev {:extra-deps {lambdaisland/kaocha {:mvn/version "1.0.669"}}
        :extra-paths ["test/resources"]
        :jvm-opts ["-Dexample.debug=true"]}}}
```

(The example also produces the warning `project.clj: the project's
kaocha 1.0.669 predates kaocha.runner/exec-fn …` — see below.)

Rig reports every decision: dropped content produces a warning on stdout
(`project.clj: profile :docgen dropped (…)`). Blocking problems abort the
run and write nothing: `:sign-releases true` in a deploy repository, an
unparseable file, no `defproject` form, or a missing `:parent-project`
path.

## The key mapping

| `project.clj` | Rig |
|---|---|
| `defproject` coordinate | `:rig/lib` |
| `:main` | `:rig/main` |
| version literal (`"1.2.3"`) | `:rig/version` |
| version var whose body is `(slurp "…")` | `:rig/version-file` — omitted for the `VERSION` default |
| `:dependencies` | `:deps` (symbol coordinates; `:exclusions`, `:local/root`, `:git/url` + `:git/sha` pass through; `~var` versions resolve from a top-level literal / `(slurp "…")` def) |
| `:managed-dependencies` | versionless deps' versions materialized to `:mvn/version` — a literal pin → the pin, the `:version` token → the project's own version; the dep's own `:exclusions` win over the pool entry's; unreferenced pins drop with a warning |
| `:parent-project` | `:managed-dependencies` inherited via `:inherit [:managed-dependencies]` materialized from the parent manifest (parent pool as base, own entries win); other inherited keys drop with a warning; a missing parent path is a blocking problem |
| `:sub` | root `:rig/modules`, one `deps.edn` per module (`project.clj` files untouched); sibling-module deps → `:local/root` (module-relative, winning over any declared version); the root's `:managed-dependencies` pins move to the root's `:rig/deps`; nested `:sub` is a blocking problem (flatten the module hierarchy) |
| `:repositories` | `:mvn/repos` |
| `:deploy-repositories` | `:rig/publish` (only the first entry is migrated; the rest are dropped with a warning; the `:clojars` shorthand maps to the Rig `clojars` repo) — migrate does not set `:rig/publish?`; enable the modules you want to publish yourself |
| `:source-paths` / `:resource-paths` | `:paths` — omitted when the dirs are just `["src"]`; a dir set other than `src` + `resources` is also carried as `:rig/artifact-dirs` |
| `:java-source-paths` | `:rig/java-src-dirs` |
| `:javac-options` | `:rig/javac-opts` |
| `:test-paths` | the `:test` alias's `:extra-paths` |
| `:profiles` → `:test`, `:dev` | `:aliases` — `:dependencies` → `:extra-deps`, path keys → `:extra-paths`, `:jvm-opts` → `:jvm-opts` |
| `:profiles` → `:uberjar` | `:rig/uberjar? true` |
| `:profiles` → `:provided` | merged into the base manifest — `:dependencies` → `:deps` (a base declaration wins the version conflict), path keys → `:paths`, `:jvm-opts` → `:jvm-opts`; lein keeps `:provided` active by default, so it belongs to the base build |
| `:uberjar-name` | `:rig/uberjar-file` — `target/` + the name; a missing `.jar` suffix is appended |

Rig expands single-segment coordinates (`aero` → `aero/aero`), the way
Leiningen reads them — `rig new` by contrast requires the full
`group/name` form.

## The `:test` alias

Rig always emits the `:test` alias, because `rig test` hard-requires a
test exec-fn.

Lein applies the `:dev` profile to the test task by default. So the
generated `:test` alias is the `:test` profile layered over `:dev`:
dependency and `:jvm-opts` lists concatenate (dev first), and path lists
merge without duplicates. Test code that needs a dev-only dependency
stays loadable. The `:dev` profile also becomes its own `:dev` alias
when it carries expressible content, for manual development use.

- `:extra-deps` — the layered `:dependencies` plus `lambdaisland/kaocha`
  at the version the project effectively pins for tests (the version
  from the `:test` profile, else from the `:dev` profile, else the
  top-level `:dependencies`). If the project pins no such version, Rig
  uses the `rig new` pin (`1.66.1034`).
- `:extra-paths` — the layered path keys plus the effective
  `:test-paths`, default `["test"]`.
- `:exec-fn kaocha.runner/exec-fn`.

Kaocha added `kaocha.runner/exec-fn` in `1.0.937`. When the effective
pin of the project predates it (a `:dev` that still pins `1.0.669`,
say), the `:test` alias uses the `rig new` pin instead, with a warning.
The `:dev` alias keeps its own version for manual runs.

## What gets dropped

Rig drops Leiningen features with no Rig equivalent **with a warning per
occurrence** — nothing disappears silently:

- `:plugins` (top-level or per-profile) — the wagon, test-report, and
  cljfmt plugins have no Rig counterpart.
- `:aliases` (Leiningen task aliases) — lein task invocations become
  `rig exec`.
- `:aot`, `:global-vars`, and `:native-image` (the `:graalvm` profile).
- Every profile other than `:test`, `:dev`, `:uberjar`, and `:provided`
  (a `:docgen` profile that only adds dependencies becomes a `rig exec`
  command).
- `:managed-dependencies` pins no versionless dep references — the pool
  exists to supply versions to declared deps. Rig drops pins that
  nothing references, with a warning, and the transitive version
  constraints are gone.
- A computed or undefined `~var` (lein-replace interpolation
  Rig cannot evaluate) — a dep whose version comes from one is a
  blocking problem. Rig names the var and the dep. Top-level
  `(def var "literal")` and `(def var (slurp "…"))` defs resolve.

Version vars Rig cannot interpret (neither a string literal nor a
`(slurp "…")` body) produce a warning and no version key. The manifest
then carries no `:rig/version`. That is correct when the var reads the
default `VERSION` file, like a wrapped form
`(.trim (try (slurp "VERSION") (catch Exception _ "1.0.0-SNAPSHOT")))`.

## The command surface

| Leiningen | Rig |
|---|---|
| `lein test` | `rig test` |
| `lein run` | `rig run` |
| `lein repl` | `rig repl` |
| `lein jar` | `rig build` |
| `lein uberjar` | `rig build --uber` |
| `lein deploy` | `rig publish` |
| `lein clean` | `rig clean` |
| `lein deps` / `lein classpath` | `rig tree` / `rig exec` (exports the locked `CLASSPATH`) |
| `lein update-deps` | `rig update` |
| `lein version` / `lein info` | `rig version` / `rig info` |
| `lein <task>` | `rig exec` |

After the conversion, the workflow is:

```sh
rig migrate          # one-time, writes deps.edn
rig lock             # pin the tree
rig check            # must be clean before you retire lein
rig test
```

Then swap the Makefile/CI invocations, and in CI the frozen gate
([details](../workflows/ci.md)):

```sh
rig verify --frozen && rig check --frozen && rig test --frozen
```

`project.clj` is inert at that point — nothing reads it. Delete it (or
keep it as a reference; `deps.edn` wins while both exist).

## What stays unchanged

- **`deps.edn` is real `deps.edn`.** `:paths`, `:deps`, `:aliases`,
  `:mvn/repos` behave exactly as tools.deps defines. CIDER,
  clojure-lsp, and any `clj -Sdeps` one-liner work on the result.
- **Your repositories** — including private ones — keep working through
  `~/.m2/settings.xml`, the same file as today.
- **`clj` remains available** for the edge cases Rig has no verb for;
  `rig exec` exports the locked `CLASSPATH` for scripts.

## Gotchas

- **Exact versions only.** `RELEASE`/`LATEST` in `:dependencies` migrate
  as-is and then become `rig check` errors (`floating-version`); pin
  them with `rig update <coord>`.
- **A `:provided` dep loses its scope.** Lein activates `:provided` by
  default, so its dependencies merge into base `:deps` and become
  regular (compile-scope) dependencies in the published POM. For a
  library, downstream consumers who ship the same artifact themselves
  (say, a JDBC driver) can now hit version conflicts. The provided
  scope of lein would have kept those artifacts out.
- **`~/.m2` credentials carry over.** Deploying through an authenticated
  repository needs the same settings.xml as `lein deploy` did.
- **A `:sub` monorepo migrates as a whole.** The root gains
  `:rig/modules`, and each module gets its own `deps.edn` next to the
  `project.clj`, which Rig leaves untouched. Rig does not touch a
  `:sub` declared by a module (nested `:sub` is a blocking problem —
  flatten the hierarchy). Rig does not touch `project.clj` files that
  are not in the `:sub` list of the root.
- **Materialized sibling refs become published-artifact requirements.**
  A `:version` self/sibling ref materializes to a `:mvn/version`
  requirement for the version of the project itself (typically
  `-SNAPSHOT`). `rig lock` then succeeds only when the repository has
  that artifact, or after you rewrite the ref to `:local/root` in a
  decomposed workspace. Even without the rewrite, a decomposed
  workspace can lock. A published coordinate for a workspace module
  loses to the local module, whether Rig sees it as a direct dependency
  or through a transitive POM. See [local modules beat published
  coordinates](../internals/resolution.md#local-modules-beat-published-coordinates).
