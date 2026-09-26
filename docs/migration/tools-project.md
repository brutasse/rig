# Migrating from tools.project

You are running a project (typically multi-module) driven by
`clojure -T:project <target>` from a Makefile, with `:exoscale.project/*`
keys in every `deps.edn` and a `:project` alias copy-pasted into each one.
If you also share versions through
[deps-modules](deps-modules.md), that layer migrates with the same steps.

## What you have today

A typical legacy root `deps.edn`:

```edn
{:deps {com.example/my-lib {:local/root "modules/lib", :exoscale.deps/inherit :all}
        com.example/my-app {:local/root "modules/app", :exoscale.deps/inherit :all}}
 :aliases
 {:project {:extra-deps {io.github.exoscale/tools.project {:git/sha "5f24196…"}}
            :ns-default exoscale.tools.project
            :exoscale.deps/inherit :all
            :jvm-opts ["-Dclojure.main.report=stderr"]}}
 :exoscale.deps/managed-dependencies {org.clojure/clojure {:mvn/version "1.11.0"}
                                      com.example/shared {:mvn/version "0.7.2"}}
 :exoscale.deps/managed-aliases {:project {:ns-default exoscale.tools.project
                                           :extra-deps {io.github.exoscale/tools.project
                                                        {:git/sha "5f24196…"}}}}
 :exoscale.project/version-file "VERSION"
 :exoscale.project/modules ["modules/lib" "modules/app"]}
```

And each module repeats the tooling keys and the `:project` alias:

```edn
{:exoscale.project/lib com.example/my-app
 :exoscale.project/main myapp.main
 :exoscale.project/uberjar? true
 :exoscale.project/uberjar-file "target/my-app.jar"
 :deps {org.clojure/clojure {:exoscale.deps/inherit :all, :mvn/version "1.11.0"}}
 :aliases
 {:test {:extra-deps {lambdaisland/kaocha {:exoscale.deps/inherit :all,
                                           :mvn/version "1.66.1034"}}
         :exec-fn kaocha.runner/exec-fn}
  :project {:extra-deps {io.github.exoscale/tools.project {:git/sha "5f24196…"}}
            :ns-default exoscale.tools.project
            :exoscale.deps/inherit :all
            :jvm-opts ["-Dclojure.main.report=stderr"]}}}
```

Limitations of this setup:

- the `:project` alias is copy-pasted in every file, with drifting git
  shas between them;
- there is no lockfile, no hashing, no full-tree pinning;
- `merge-deps` rewrites files on disk, and forgetting to run it means the
  managed map and the modules can diverge;
- ~25 configuration keys spread over three namespaces
  (`:exoscale.project/*`, `:exoscale.deps/*`, `:slipset.deps-deploy/*`).

## The key mapping

| Legacy | rig |
|---|---|
| `:exoscale.project/lib` | `:rig/lib` |
| `:exoscale.project/version` | `:rig/version` |
| `:exoscale.project/version-file` | `:rig/version-file` |
| `:exoscale.project/main` | `:rig/main` |
| `:exoscale.project/uberjar?` | `:rig/uberjar?` |
| `:exoscale.project/uberjar-file` | `:rig/uberjar-file` |
| `:exoscale.project/uber-opts` | `:rig/uber-opts` |
| `:exoscale.project/bypass-test?` | `:rig/test?` (inverted) |
| `:exoscale.project/deploy?` | `:rig/publish?` |
| `:slipset.deps-deploy/exec-args` | `:rig/publish` |
| `:exoscale.project/target-dir` | `:rig/target-dir` |
| `:exoscale.project/src-dirs` | `:rig/src-dirs` |
| `:exoscale.project/java-src-dirs` | `:rig/java-src-dirs` |
| `:exoscale.project/javac-opts` | `:rig/javac-opts` |
| `:exoscale.project/extra-clean-targets` | drop — `rig clean` removes the target directory; use `rig exec` for anything else |
| `:exoscale.project/modules` | `:rig/modules` |
| `:exoscale.deps/managed-dependencies` + `:exoscale.deps/inherit` + `merge-deps` | `:rig/deps` (requirements) + `deps.lock` (pins) — no on-disk rewriting |
| `:exoscale.deps/managed-aliases` + `merge-aliases` | nothing — aliases are per-module |
| `:project` alias (every file) | nothing — rig is a native binary |
| `:exoscale.project/tasks` | nothing — `rig exec` is the escape hatch |

Full key reference: [configuration](../reference/config.md).

## The command mapping

| Legacy (Makefile / `-T:project`) | rig |
|---|---|
| `init` | `rig new` |
| `add-module` | `rig new-module` |
| `check` | `rig check` (stronger: + drift/conflict/floating detection) |
| `clean` | `rig clean` |
| `deploy` | `rig publish` |
| `install` | `rig install` |
| `jar` | `rig build` |
| `uberjar` | `rig build --uber` |
| `lint` | `rig lint` |
| `format-check` / `format-fix` | `rig fmt --check` / `rig fmt` |
| `outdated` | `rig outdated` |
| `merge-deps` / `merge-aliases` | **gone** — `rig lock` / `rig update` |
| `prep` | folded into `build`/`test`/`run`/`repl` (auto-run, staleness-checked) |
| `release` | `rig release` |
| `task` | `rig exec` |
| `test` | `rig test` |
| `version` / `info` | `rig version` / `rig info` |

## Step 1 — add the lock (do this first, ship it)

Your manifests are already fully self-contained (that was the invariant
tools.project enforced), so rig can lock them untouched:

```sh
rig lock
git add deps.lock
```

Commit it. Nothing else changes; every build is now hash-pinned. This is
also your parity baseline:

```sh
# the locked classpath must equal what the old tooling resolved
clojure -Spath -M -N:<module-ns>   # per module, per alias
```

A byte-compare between the `clojure -Spath` output and the lock's
classpath, per module and per alias, proves step 2 will not change what
runs.

## Step 2 — swap the command surface

Today the invocations live in the Makefile (a real migrated project,
trimmed):

```make
CLJ=clojure -J-Dclojure.main.report=stderr

check: ## Loads all namespaces
	$(CLJ) -T:project check

test-unit: ## Runs unit tests
	$(CLJ) -T:project test :kaocha.filter/focus '[:unit]'

merge-deps: ## Merge dependencies on all modules from :managed-deps
	$(CLJ) -T:project merge-deps

lint: ## runs linting on all modules
	$(CLJ) -T:project lint

uberjar: ## Build uberjar(s)
	$(CLJ) -T:project uberjar

release: ## Release jar modules & tag versions
	git config --global --add safe.directory '*'
	$(CLJ) -T:project release
```

After: the rig verbs, directly.

```sh
rig check
rig test :kaocha.filter/focus '[:unit]'
rig lint
rig build --uber
rig release
```

rig is a native binary with the verbs — the Makefile wrapper that existed
to feed `clojure -T:project` is no longer needed. If the Makefile only
wrapped the old tooling, delete it here or in step 5. If it carries
anything else (docker-compose conveniences, local overrides), keep it,
and the surviving targets become one-liners around the rig verbs.

`merge-deps` disappears with the step; the `git config safe.directory`
workaround is no longer needed either. Run the test suite and the
byte-compare from step 1. When green, ship.

## Step 3 — adopt the workspace model

Now the manifests themselves — and this step is not done by hand. `rig
migrate` does the whole mechanical rewrite in one command (`--dry-run`
first, then the real run), format-preserving, reporting every file it
changes:

```sh
rig migrate --dry-run   # preview
rig migrate             # apply
```

Per `deps.edn` it:

1. renames the `:exoscale.project/*` keys to `:rig/*` (the table above);
   `:slipset.deps-deploy/exec-args` becomes `:rig/publish`
   (default `{:repo "clojars" :sign-releases? false}`);
2. deletes every `:project` alias (all of them — the copies drift, there is
   no canonical one to keep);
3. deletes the `:exoscale.deps/inherit` markers from every coordinate; and
4. in the root, renames `:exoscale.project/modules` → `:rig/modules`, lifts
   the managed versions into `:rig/deps` as plain requirements, and deletes
   `:exoscale.deps/managed-dependencies`, `:exoscale.deps/managed-aliases`,
   and `:exoscale.project/extra-deps-files`.

It reproduces the legacy merge's *effective* dependencies exactly: no
version drift is introduced, and none is fixed — drift is what `check`
surfaces below, not hidden. Blocking problems (unknown version-fn,
`:sign-releases? true`, …) abort with nothing written; resolve them and
re-run.

The root's `:rig/deps` keeps full requirement maps — versions,
exclusions, `:local/root`, `:git` — it is not a new format:

```edn
:rig/deps {org.clojure/clojure {:mvn/version "1.11.0"}
           com.example/shared {:mvn/version "1.0.0"
                              :exclusions [com.example/ex com.example/schemas …]}
           org.clojure/test.check {:mvn/version "1.1.1"}}
```

Then re-lock and let `check` do the forensics:

```sh
rig lock
rig check
```

On a real project this is where previously hidden drift surfaces — a
project migrated with this process got:

```
check: error [stale-lock] modules/app com.example/shared: manifest requires "1.0.0"; lock pins "0.7.2" — run rig update
check: error [floating-version] modules/app org.clojure/test.check: manifest uses "RELEASE"; run `rig update org.clojure/test.check` to pin an exact version
check: warn [drift] modules/lib org.clojure/test.check: module requires "1.1.1", workspace requires "1.1.0"
```

— the managed map said `com.example/shared 0.7.2` while `modules/app`
actually ran `1.0.0`; `test.check` was `RELEASE` in one module, `1.1.0` in
the managed map, `1.1.1` in another; a network library was pinned to an
older version in the managed map than in a module. For each finding, choose
the intended version in `:rig/deps` and run `rig update <coord> <version>`
— one command updates the shared requirement, every declaring module, and
the lock, in one visible diff.

## Step 4 — harden CI

Replace the old workflow steps with the frozen gate
([full example in the CI page](../workflows/ci.md)):

```yaml
- name: Verify, check and test (frozen)
  run: |
    rig verify --frozen
    rig check --frozen
    rig test --frozen

- name: Build uberjars (frozen)
  run: rig build --uber --frozen

- name: Release
  run: rig release
```

## Step 5 — clean out

- Delete Makefile targets that only wrapped the old tooling (keep the
  Makefile for what is left — docker-compose conveniences, local
  overrides, …).
- Drop tools.project and deps-modules from the toolchain; nothing
  references them anymore.
- The `clj -T` knowledge in your team's heads becomes `rig <verb>`.

## Worked example

A real multi-module project (eight modules, hundreds of locked artifacts)
went through exactly these steps:

1. `rig lock` on the untouched legacy repo → `deps.lock` committed;
2. Makefile and both GitHub workflows swapped to the rig verbs; locked
   classpath verified **byte-identical** to `clojure -Spath` on all
   module/alias combinations;
3. `rig migrate` rewrote all 8 manifests `:exoscale.*` → `:rig/*` (comments
   preserved, zero legacy keys left); `rig check` surfaced the drift above;
   `rig update` settled it (test.check → 1.1.1 everywhere, the drifted
   coordinates → their intended versions);
4. CI on the frozen gate: `rig verify --frozen &&
   rig check --frozen && rig test --frozen`, `rig build --uber --frozen`,
   `rig release`;
5. legacy targets deleted.

Result: `rig test` green, `rig build --uber` green, and the same
byte-identical classpath — with hashing, cooldowns, and a lockfile around
it.
