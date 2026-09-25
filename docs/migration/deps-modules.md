# Migrating from deps-modules

You share dependency versions across modules with
**deps-modules**: a `:exoscale.deps/managed-dependencies` map in the root
`deps.edn`, `:exoscale.deps/inherit` markers in the modules, and a
`merge-deps` step that rewrites the `deps.edn` files on disk. (If you also
run tools.project, see [that guide](tools-project.md) — the steps here
slot into its step 3.)

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
and the rewritten files), and nothing checks they agree. Forgetting the
step — or a partial merge — means the modules build against versions that
are not the ones you declared centrally, and the divergence is invisible
until something breaks.

## What replaces it

Two files, one job each:

- **`:rig/deps`** in the root — the *requirements* (versions you intend).
  It is declared once, read by `rig check` (and kept in sync by
  `rig update`), and never merged into module classpaths: each module keeps
  declaring its own `:deps`.
- **`deps.lock`** — the *pins* (versions you actually build against),
  with hashes, written by `rig lock`, committed, and checked by every
  command.

There is no merge step. There is no on-disk rewriting of module manifests
by the tooling — the only manifest edits rig makes are your explicit
`rig add`/`update`/`remove` (and the one-shot `rig migrate`),
format-preserving, in one visible diff.
And instead of drift being invisible, it is a named, reported condition:

```
check: warn [drift] modules/lib org.clojure/test.check: module requires "1.1.1", workspace requires "1.1.0"
check: error [stale-lock] modules/app com.example/shared: manifest requires "1.0.0"; lock pins "0.7.2" — run rig update
```

## The migration

`rig migrate` does the mechanical part — steps 1 and 2 — in one command
(`--dry-run` first, then the real run), reporting every file it changes.
It reproduces the merge's *effective* dependencies exactly: it introduces
no version drift, and it does not fix the drift the managed map had been
masking — that is steps 3 and 4. What each step does, and what is left to
you:

### 1. Lift the managed map into `:rig/deps`

Copy `:exoscale.deps/managed-dependencies` into `:rig/deps` at the root,
dropping any `:exoscale.deps/inherit` markers found on the entries
themselves (inert residue — a migrated manifest must carry no
`exoscale.*` key anywhere), then delete the managed map (and
`:exoscale.deps/managed-aliases` — there is no alias inheritance in rig;
aliases are per-module, and shared test deps move to the modules' `:test`
aliases or to `:rig/deps` requirements).

A before/after from a migrated project's root (trimmed):

```edn
;; before
:exoscale.deps/managed-dependencies
{org.clojure/clojure {:mvn/version "1.11.0"}
 com.example/shared {:mvn/version "0.7.2" :exclusions [com.example/ex …]}
 org.clojure/test.check {:mvn/version "1.1.0"}}

;; after
:rig/deps
{org.clojure/clojure {:mvn/version "1.11.0"}
 com.example/shared {:mvn/version "1.0.0" :exclusions [com.example/ex …]}
 org.clojure/test.check {:mvn/version "1.1.1"}}
```

The requirement maps keep their shape — `:mvn/version`, `:exclusions`,
`:local/root`, `:git` all work.

### 2. Delete the inherit markers

Strip every `:exoscale.deps/inherit …` key from the modules' coordinates.
The versions the merge used to inject are now either in the module's own
`:deps` (if it declared them) or simply gone — the module requires what
it requires, and the shared requirement in `:rig/deps` is the canonical
version for cross-module coordinates.

```edn
;; before
:deps {org.clojure/clojure {:exoscale.deps/inherit :all, :mvn/version "1.11.0"}
       com.example/shared {:exclusions […], :exoscale.deps/inherit :all, :mvn/version "1.0.0"}}

;; after
:deps {org.clojure/clojure {:mvn/version "1.11.0"}
       com.example/shared {:exclusions […], :mvn/version "1.0.0"}}
```

If a module used `:inherit [:mvn/version]` to inherit *only* the version
while keeping a local exclusion, the result is the same: the module's
own map wins on the keys it declares.

### 3. Lock and surface the drift

```sh
rig lock
rig check
```

`rig check` compares the workspace requirement (`:rig/deps`), each module
requirement, and the lock — the three places the old model let diverge.
Fix each finding deliberately:

```sh
rig update org.clojure/test.check 1.1.1   # settle the shared version
```

### 4. Drop the tooling

- Remove the `merge-deps`/`merge-aliases` Makefile targets (they are
  gone — there is nothing to merge);
- remove the deps-modules alias / `clj -Tmdeps` install from the
  toolchain;
- if the managed `:project` alias existed to carry deps-modules'
  `:exoscale.deps/inherit` machinery into the `:project` namespace, it
  disappears with the [tools.project migration](tools-project.md).

## What you lose, and why it is fine

| deps-modules feature | In rig |
|---|---|
| single version file for all modules | `:rig/deps` — declared once, read at resolve time |
| selective inheritance (`:inherit [:mvn/version]`) | not needed: a module's own requirement map already expresses exactly what it wants; shared coordinates are aligned by `rig update`, not by rewriting |
| alias synchronization (`managed-aliases`) | not needed: aliases are per-module by design; shared test deps go through `:rig/deps` requirements + each module's `:test` alias |
| `dry-run?` preview | the manifest diff *is* the preview — `rig update` prints each edit before writing |
| comments/formatting preserved on merge | rig never rewrites manifests except for your explicit dep edits, which are format-preserving |
| forgetting the sync step | no sync step exists; `rig check` fails loudly on drift instead |
