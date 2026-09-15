# Onboarding a plain tools.deps project

You have a project that is just `deps.edn` files and the Clojure CLI —
`clj -X:test`, `clj -Spath`, maybe a small Makefile. No tools.project, no
deps-modules. This is the easiest onboarding: **your manifests already are
rig manifests** (minus the `:rig/*` keys), and `rig lock` works on them
untouched.

## Single module

### Step 1 — lock (zero changes)

```sh
cd my-lib
rig lock
```

```
wrote deps.lock: 41 artifacts, 1 modules
```

Commit `deps.lock`. That is the whole security core: full-tree pinning,
hash verification, offline builds. Your `deps.edn` is unchanged; editors
and `clj` keep working exactly as before.

### Step 2 — adopt the `:rig/*` keys you need

Add only what you want to use:

```edn
{:rig/lib com.example/my-lib          ;; for build/install/publish
 :rig/main com.example.mylib.core     ;; for rig run
 :rig/uberjar? true                   ;; for rig build --uber
 :rig/publish? true                   ;; for rig publish/release
 :paths ["src"]
 :deps {org.clojure/clojure {:mvn/version "1.12.5"}}}
```

Each key is independent — a library that never publishes needs only
`:rig/lib` (for `build`/`install`). The full list with defaults:
[configuration](../reference/config.md).

### Step 3 — give it a test alias

If your tests run via `clj -X:test` with some exec-fn, that alias already
exists and `rig test` uses it as-is. If your tests are ad hoc
(`clojure.test` across files), add the standard alias:

```edn
:aliases
{:test {:extra-deps {lambdaisland/kaocha {:mvn/version "1.66.1034"}}
        :extra-paths ["test"]
        :exec-fn kaocha.runner/exec-fn}}
```

```sh
rig test
```

### Step 4 — swap the entry points

```
before                          after
clj -X:test                     rig test
clj -X:test :kaocha.filter/focus '[:unit]'    rig test :kaocha.filter/focus '[:unit]'
clj -M -m com.example.mylib.core   rig run -p .          (or: rig run)
clj                             rig repl
clojure -Spath                  rig tree / rig exec (CLASSPATH is exported)
```

A minimal Makefile, if you have one:

```make
test:  rig test
repl:  rig repl
jar:   rig build
uber:  rig build --uber
clean: rig clean
```

And in CI, the frozen gate instead of "cache m2 and run the CLI"
([details](../workflows/ci.md)):

```sh
rig verify --frozen && rig check --frozen && rig test --frozen
```

## Multi module

If you already have several `deps.edn` directories (say `modules/lib` and
`modules/app`) with `:local/root` cross-references, you have a workspace
waiting to be declared:

1. **Declare the modules** in the root `deps.edn`:

   ```edn
   {:rig/modules ["modules/lib" "modules/app"]}
   ```

   (`rig new` scaffolds this shape; `rig new-module <name>` adds to it.)

2. **Move shared versions to `:rig/deps`** if the modules repeat the same
   requirement strings:

   ```edn
   :rig/deps {org.clojure/clojure {:mvn/version "1.12.5"}
              com.example/my-lib {:local/root "modules/lib"}}
   ```

   Each module keeps declaring its own `:deps` (self-contained, as today);
   `:rig/deps` is the canonical shared requirement, and `rig check` warns
   on any drift between the two.

3. **Give each module its `:rig/*` keys** (`:rig/lib`, `:rig/main`, …) as
   in the single-module steps.

4. `rig lock` — the lock now covers every module, with local dependencies
   expanded in each classpath, and `rig test` runs them in dependency
   order.

## What stays unchanged

- **`deps.edn` is still `deps.edn`.** `:paths`, `:deps`, `:aliases`,
  `:mvn/repos` behave exactly as tools.deps defines. CIDER, clojure-lsp,
  and any `clj -Sdeps` one-liner keep working on every module.
- **`clj` remains available** for the edge cases rig has no verb for —
  and `rig exec` exports the locked `CLASSPATH` for scripts that want it.
- **Your repositories** — including private ones — keep working through
  `~/.m2/settings.xml`, the same file as today.

## Gotchas

- **Exact versions only.** `RELEASE` and `LATEST` in a manifest are
  `rig check` errors (`floating-version`): the lock pins whatever resolved
  at lock time, so floating versions get pinned explicitly —
  `rig update org.clojure/test.check` — instead of drifting per build.
  (Your first `rig lock` may select the current latest for any floating
  coord; that selection is what gets pinned.)
- **Cooldowns on first adoption.** The first `rig add`/`rig update`
  without a version may refuse a release published in the last 48 hours
  (exit 5). That is the feature; `--force` overrides, and the decision is
  recorded in the lock.
- **`deps.lock` is committed source.** Review lock diffs like any other
  diff; a changed `sha256` for an unchanged version is a review item.
- **One manifest per module.** rig finds the project by walking up from
  your directory to the nearest `deps.lock` (or `:rig/modules` root) — so
  run rig from inside the project, as you do with `clj`.
