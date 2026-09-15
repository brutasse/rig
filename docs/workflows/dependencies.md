# Dependency management

All dependency changes go through four verbs. Each one edits the manifest
**and** re-locks, so a dependency change is always a coherent commit:
manifest diff + `deps.lock` diff.

## Adding

```sh
rig add org.clojure/data.json 2.0.0        # explicit version
rig add org.clojure/data.json             # newest eligible version
rig add com.example/thing -p modules/app --shared=false  # one module only…
rig add com.example/thing --alias test    # …or under an alias's :extra-deps
```

By default, the requirement is added **shared**: it is recorded in the
workspace's `:rig/deps` and propagated to every module that declares the
coordinate. `-p` chooses which module's `:deps` receives the edit;
`--shared=false` keeps the requirement local to that module. Without a
version, rig selects the newest available version, subject to the
[cooldown](../concepts/security.md#cooldowns-the-adoption-window).

A successful add prints what it did:

```
deps.edn: set org.clojure/data.json "2.0.0"
deps.edn: set org.clojure/data.json "2.0.0"
pinned org.clojure/data.json 2.0.0 (explicit)
wrote deps.lock: 33 artifacts, 2 modules
```

The `set` lines list each manifest edit (the target module's `:deps` and the
root's `:rig/deps`, plus any other affected modules); the last line confirms
the new lock.

If the re-resolve fails — a bad version, an artifact that doesn't exist,
a repo you can't reach — rig reverts the manifest edits byte-for-byte,
prints `reverted N manifest file(s): the edit failed`, and exits
non-zero; the lock is not updated. A failed change leaves no trace:
cooldown refusals (exit 5) and unknown coordinates are refused before
any edit happens.

## Removing

```sh
rig remove org.clojure/data.json
```

Removes the shared requirement (from `:rig/deps` and from every module
declaring it) and re-locks:

```
deps.edn: remove org.clojure/data.json
deps.edn: remove org.clojure/data.json
wrote deps.lock: 32 artifacts, 2 modules
```

## Updating

`rig update` has three forms:

```sh
rig update                                  # re-resolve: refresh floating versions
rig update org.clojure/clojure              # bump one coord to newest eligible
rig update org.clojure/clojure 1.12.5       # pin one coord to an exact version
```

- **No arguments** — a full re-resolve that keeps existing pins
  (`respect-existing-pins`); only requirements that were floating
  (`RELEASE`/`LATEST` — see `check`'s `floating-version` finding) re-select.
  This is also what `rig release` does internally when the version
  changes.
- **coord + version** — an explicit pin. This is your judgment; it always
  wins, and the lock records it as `pinned … (explicit)`.
- **coord only** — newest eligible version, cooldown-gated. If the newest
  is too fresh, you get a refusal (exit 5) naming the cooldown:

  ```
  refused org.clojure/tools.logging (cooldown 48h)
  1 requirement(s) refused by cooldowns (retry with --force)
  ```

  `--force` takes the fresh version anyway and records it in the lock as
  `forced …`.

All forms propagate **shared** by default (root `:rig/deps` + every module
declaring the coordinate), format-preserving — comments and layout are
untouched, one visible diff per file. `--shared-only` updates just the
shared requirement; `--alias <a>` targets an alias's `:extra-deps`.

## What to look at after a change

1. `git diff` — the manifest edits should be exactly the version strings
   you expected, nothing else.
2. The lock output — `wrote deps.lock: N artifacts, M modules`; review the
   `deps.lock` diff for the new/changed `artifacts` entries.
3. `rig check` — confirms the new lock satisfies every manifest.

## Finding your way: `outdated` and `tree`

```sh
rig outdated
```

```
mvxcvi/arrangement 1.2.0 -> 1.2.1 (latest 2.1.0, breaking)
lambdaisland/kaocha 1.66.1034 -> 1.91.1392 (latest 1.91.1392)
org.clojure/clojure 1.12.5 -> 1.13.0-alpha7 (latest 1.13.0-alpha7)
```

Pinned version, newest version still satisfying the requirement, and the
overall latest — with breaking updates marked. `--breaking` lists only
breaking ones; `up to date` is the whole output when there is nothing.

```sh
rig tree -p modules/orchestrator
```

```
com.example/orchestrator:1.0.0-SNAPSHOT
  org.clojure/clojure:1.11.0
    org.clojure/spec.alpha:0.3.218
    org.clojure/core.specs.alpha:0.2.62
  …
```

The resolved dependency graph for a module; `--alias test` includes the
test alias's extra-deps.
