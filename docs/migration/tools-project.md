# Migrating from tools.project

You run a project (typically multi-module) with
`clojure -T:project <target>` commands from a Makefile. Every `deps.edn`
carries `:exoscale.project/*` keys and a copy-pasted `:project` alias. If
you also share versions through [deps-modules](deps-modules.md), the same
steps migrate that layer.

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

- The same `:project` alias appears in every file, and the git shas
  drift between copies.
- There is no lockfile, no hashing, and no full-tree pinning.
- `merge-deps` rewrites files on disk. If you forget to run it, the
  managed map and the modules can diverge.
- ~25 configuration keys spread over three namespaces
  (`:exoscale.project/*`, `:exoscale.deps/*`, `:slipset.deps-deploy/*`).

## The key mapping

| Legacy | Rig |
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
| `:exoscale.project/src-dirs` | `:rig/artifact-dirs` |
| `:exoscale.project/java-src-dirs` | `:rig/java-src-dirs` |
| `:exoscale.project/javac-opts` | `:rig/javac-opts` |
| `:exoscale.project/extra-clean-targets` | drop — `rig clean` removes the target directory; use `rig exec` for anything else |
| `:exoscale.project/modules` | `:rig/modules` |
| `:exoscale.deps/managed-dependencies` + `:exoscale.deps/inherit` + `merge-deps` | materialized into the modules' `:deps` by `rig migrate` (declared keys win over the pool); the pool is dropped, pins land in `deps.lock` — no ongoing merge step |
| `:exoscale.deps/managed-aliases` + `merge-aliases` | nothing — aliases are per-module |
| `:project` alias (every file) | nothing — Rig is a native binary |
| `:exoscale.project/tasks` | nothing — `rig exec` is the escape hatch |

Full key reference: [configuration](../reference/config.md).

## The command mapping

| Legacy (Makefile / `-T:project`) | Rig |
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
| `release` | `rig publish` + `git tag` — [Releasing](../workflows/build-publish.md#releasing) |
| `task` | `rig exec` |
| `test` | `rig test` |
| `version` / `info` | `rig version` / `rig info` |

Modules that declared `:deps/prep-lib` keep working — Rig runs the prep
function automatically, staleness-checked. If the function is just "javac
my own sources into my class dir", Rig does that natively now: declare
`:rig/java-src-dirs` and drop the prep library. See
[Java sources](../concepts/java.md).

## Step 1 — add the lock (do this first, ship it)

Your manifests are already fully self-contained (that was the invariant
tools.project enforced), so Rig can lock them untouched:

```sh
rig lock
git add deps.lock
```

Commit it. Nothing else changes, and Rig hash-pins every build. This is
also your parity baseline:

```sh
# the locked classpath must equal what the old tooling resolved
clojure -Spath -M -N:<module-ns>   # per module, per alias
```

A byte-compare between the `clojure -Spath` output and the classpath of
the lock proves that step 2 will not change what runs. Compare per module
and per alias.

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

After: the Rig verbs, directly.

```sh
rig check
rig test :kaocha.filter/focus '[:unit]'
rig lint
rig build --uber
rig publish
git tag "v$(cat VERSION)"
```

Rig is a native binary with the verbs, so you no longer need the Makefile
wrapper that fed `clojure -T:project`. If the Makefile only wrapped the
old tooling, delete it here or in step 5. If it carries anything else
(docker-compose conveniences, local overrides), keep it, and the
surviving targets become one-liners around the Rig verbs.

`merge-deps` disappears with this step, and you no longer need the
`git config safe.directory` workaround. Run the test suite and the
byte-compare from step 1. When the tests pass, ship.

## Step 3 — adopt the workspace model

Now the manifests themselves — you do not edit them by hand. `rig
migrate` does the whole mechanical rewrite in one command. It preserves
formatting and reports every file it changes. Run it with `--dry-run`
first, then run it for real:

```sh
rig migrate --dry-run   # preview
rig migrate             # apply
```

Per `deps.edn` it:

1. It renames the `:exoscale.project/*` keys to `:rig/*` (see the table
   above). `:slipset.deps-deploy/exec-args` becomes `:rig/publish`, with
   the default `{:repo "clojars" :sign-releases? false}`.
2. It deletes every `:project` alias — all of them. The copies drift,
   and there is no canonical one to keep.
3. It deletes the `:exoscale.deps/inherit` markers from every coordinate.
4. In the root, it renames `:exoscale.project/modules` to `:rig/modules`,
   moves the managed versions into `:rig/deps` as plain requirements, and
   deletes `:exoscale.deps/managed-dependencies`,
   `:exoscale.deps/managed-aliases`, and
   `:exoscale.project/extra-deps-files`.

It reproduces the effective dependencies of the legacy merge exactly. It
introduces no version drift, and it fixes none. The `check` step below
surfaces that drift instead of hiding it. Blocking problems (an unknown
version-fn, `:sign-releases? true`) abort the run and write nothing.
Resolve them and run the command again.

The `:rig/deps` key of the root keeps full requirement maps: versions,
exclusions, `:local/root`, and `:git`. It is not a new format:

```edn
:rig/deps {org.clojure/clojure {:mvn/version "1.11.0"}
           com.example/shared {:mvn/version "1.0.0"
                              :exclusions [com.example/ex com.example/schemas …]}
           org.clojure/test.check {:mvn/version "1.1.1"}}
```

Then re-lock, and let `check` find the problems:

```sh
rig lock
rig check
```

On a real project, this is where hidden drift surfaces. A project
migrated with this process got:

```
check: error [stale-lock] modules/app com.example/shared: manifest requires "1.0.0"; lock pins "0.7.2" — run rig update
check: error [floating-version] modules/app org.clojure/test.check: manifest uses "RELEASE"; run `rig update org.clojure/test.check` to pin an exact version
check: warn [drift] modules/lib org.clojure/test.check: module requires "1.1.1", workspace requires "1.1.0"
```

The managed map said `com.example/shared 0.7.2`, while `modules/app`
actually ran `1.0.0`. `test.check` was `RELEASE` in one module, `1.1.0`
in the managed map, and `1.1.1` in another. The managed map pinned a
network library to an older version than a module did. For each finding,
choose the intended version in `:rig/deps` and run
`rig update <coord> <version>`. That one command updates the shared
requirement, every declaring module, and the lock, in one visible diff.

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
  run: |
    rig publish
    git tag "v$(cat VERSION)"
    git push origin HEAD --tags
```

## Step 5 — clean out

- Delete Makefile targets that only wrapped the old tooling (keep the
  Makefile for the rest: docker-compose conveniences and local
  overrides).
- Drop tools.project and deps-modules from the toolchain. Nothing
  references them anymore.
- Your team replaces the `clj -T` knowledge with the `rig <verb>`
  commands.

## Worked example

A real multi-module project (eight modules, hundreds of locked artifacts)
went through exactly these steps:

1. Run `rig lock` on the untouched legacy repo, and commit `deps.lock`.
2. You swap the Makefile and both GitHub workflows to the Rig verbs.
   The locked classpath stays byte-identical to `clojure -Spath` on all
   module/alias combinations.
3. `rig migrate` rewrote all 8 manifests from `:exoscale.*` to
   `:rig/*`, with comments preserved and zero legacy keys left.
   `rig check` surfaced the drift above. `rig update` settled it:
   test.check → 1.1.1 everywhere, and the drifted coordinates → their
   intended versions.
4. CI uses the frozen gate: `rig verify --frozen && rig check --frozen && rig test --frozen`, `rig build --uber --frozen`, and `rig publish` plus a tag.
5. You delete the legacy targets.

Result: `rig test` green, `rig build --uber` green, and the same
byte-identical classpath — with hashing, cooldowns, and a lockfile around
it.
