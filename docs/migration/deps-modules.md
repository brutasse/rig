# Migrating from deps-modules

You share dependency versions across modules with **deps-modules**: a
`:exoscale.deps/managed-dependencies` map in the root `deps.edn`,
`:exoscale.deps/inherit` markers in the modules, and a `merge-deps` step
that rewrites the `deps.edn` files on disk. (If you also run
tools.project, see [that guide](tools-project.md). The steps here fit
inside its step 3.)

## What you have today

Root `deps.edn`:

```edn
{:exoscale.deps/managed-dependencies
 {org.clojure/clojure {:mvn/version "1.10.2"}
  com.example/thing-core {:mvn/version "1.0.0"}
  com.example/thing-not-core {:mvn/version "2.0.0" :exclusions [something/else]}}

 :exoscale.deps/managed-aliases
 {:dev {:extra-deps {lambdaisland/kaocha {:mvn/version "1.66.1034"}}}}}
```

Module `deps.edn`, before the merge:

```edn
{:paths ["src"]
 :deps {org.clojure/clojure {:exoscale.deps/inherit :all}}
 :aliases
  {:dev {:extra-deps {com.example/thing-core {:exoscale.deps/inherit [:mvn/version]}
                      com.example/thing-not-core {:exoscale.deps/inherit :all}}}}}
```

And the sync step — run it after any change to the managed map, commit
the rewritten files:

```sh
clj -T:deps-modules exoscale.deps-modules/merge-deps
# Writing deps.edn
# Writing modules/foo1/deps.edn
# Writing modules/foo2/deps.edn
# Done merging files
```

The limitation: the merged values live in two places (the managed map
and the rewritten files), and nothing checks they agree. If you forget
the step, or run a partial merge, the modules build against versions that
are not the ones you declared centrally. The divergence stays invisible
until something breaks.

## What replaces it

Two files, one job each:

- **`:rig/deps`** in the root — the *requirements* (versions you intend).
  You declare them once. `rig check` reads them, and `rig update` keeps
  them in sync. Rig never merges them into a module classpath: each
  module still declares its own `:deps`.
- **`deps.lock`** — the *pins* (versions you actually build against),
  with hashes. `rig lock` writes the file, you commit it, and every
  command checks it.

The migration itself does not create `:rig/deps`: `rig migrate`
materializes the pool into the `:deps` of the modules and drops it, so
each module stays self-contained. `:rig/deps` is what you opt into when
you want a shared requirement across modules (step 3 below).

There is no merge step. The tooling does not rewrite module manifests on
disk. The only manifest edits Rig makes are your explicit
`rig add`/`update`/`remove` (and the one-shot `rig migrate`),
format-preserving, in one visible diff. Drift is not invisible; it is a
named, reported condition:

```
check: warn [drift] modules/lib org.clojure/test.check: module requires "1.1.1", workspace requires "1.1.0"
check: error [stale-lock] modules/app com.example/shared: manifest requires "1.0.0"; lock pins "0.7.2" — run rig update
```

## The migration

`rig migrate` does the mechanical part — steps 1 and 2 — in one command
(`--dry-run` first, then the real run), reporting every file it changes.
Each module ends up declaring what it declared before. The pool fills
only the keys a module left undeclared. A declared version/source key
wins over the pool. Each such win produces a warning, not a silent
rewrite. The migration does not fix the drift the managed map masked.
That is steps 3 and 4. Each step below describes one action and your
part in it:

### 1. Materialize the managed map into the modules, then drop it

Copy each pool entry into the `:deps` of every module that inherits it
(`:exoscale.deps/inherit :all` or a key subset). Drop any
`:exoscale.deps/inherit` markers on the entries themselves. They are
inert residue. A migrated manifest must carry no `exoscale.*` key
anywhere. Then delete the managed map, and delete
`:exoscale.deps/managed-aliases`. Rig has no alias inheritance, so
aliases are per-module. Shared test dependencies move to the `:test`
aliases of the modules, or to `:rig/deps` requirements:

```
migrate: deps.edn: :exoscale.deps/managed-dependencies dropped (versions materialized into the modules' :deps; not carried as :rig/deps shared requirements)
```

The next example shows a migrated module (trimmed). A comment notes the
pool values the merge used to inject:

```edn
;; pool: org.clojure/clojure {:mvn/version "1.10.2"}
;;       com.example/thing-core {:mvn/version "1.0.0"}

;; before
:deps {org.clojure/clojure {:exoscale.deps/inherit :all}
       com.example/thing-core {:exoscale.deps/inherit [:mvn/version]
                               :exclusions [something/else]}}

;; after
:deps {org.clojure/clojure {:mvn/version "1.10.2"}
       com.example/thing-core {:mvn/version "1.0.0"
                               :exclusions [something/else]}}
```

The pool does not become a root `:rig/deps` shared requirement. Pins no
module declared were inert in the old model, and a shared requirement
would turn them into live workspace-wide constraints. If you want the old
"single version file" back, add the shared requirements to `:rig/deps`
deliberately in step 3. `rig update` keeps them in sync with the modules.

### 2. Declared values win over the pool

Where a module declared a version/source key the pool also carries
(`:mvn/version`, `:git/*`, `:local/root`), Rig keeps the value of the
module. A repo whose CI never ran `merge-deps` built against the declared
versions, and the migration does not change what it built against. Each
such win over a differing pool value produces a per-dep warning:

```
migrate: modules/app/deps.edn: dep com.example/shared: :mvn/version "1.0.0" (managed "0.7.2") (declared kept; merge-deps would have used the managed value)
```

The pool still fills the keys the module did not declare. A vector
inherit selects a subset of the keys of the pool. A pool `:local/root`
is workspace-root-relative, so Rig rewrites it to a module-relative path
before the merge. A `:local/root` of a module is already module-relative,
and Rig keeps it as-is.

### 3. Lock and surface the remaining drift

```sh
rig lock
rig check
```

The migration already warned about every declared-vs-managed divergence.
`rig check` compares each module requirement against the lock, and
against `:rig/deps` if you added shared requirements above. These are the
places the old model let diverge. Fix each finding deliberately:

```sh
rig update org.clojure/test.check 1.1.1   # settle a cross-module version
```

### 4. Drop the tooling

- Remove the `merge-deps`/`merge-aliases` Makefile targets. There is no
  merge step anymore.
- Remove the deps-modules alias / `clj -Tmdeps` install from the
  toolchain.
- If the managed `:project` alias existed to carry the
  `:exoscale.deps/inherit` machinery of deps-modules into the `:project`
  namespace, it disappears with the
  [tools.project migration](tools-project.md).

## What you lose, and why it is fine

| deps-modules feature | In Rig |
|---|---|
| single version file for all modules | `:rig/deps` — declared once, read at resolve time |
| selective inheritance (`:inherit [:mvn/version]`) | not needed: a module's own requirement map already expresses exactly what it wants; shared coordinates are aligned by `rig update`, not by rewriting |
| alias synchronization (`managed-aliases`) | not needed: aliases are per-module by design; shared test deps go through `:rig/deps` requirements + each module's `:test` alias |
| `dry-run?` preview | the manifest diff *is* the preview — `rig update` prints each edit before writing |
| comments/formatting preserved on merge | Rig never rewrites manifests except for your explicit dep edits, which are format-preserving |
| forgetting the sync step | no sync step exists; `rig check` fails loudly on drift instead |
