# Dependency management

All dependency changes go through four commands. Each command edits the
manifest **and** re-locks. So a dependency change is always a coherent
commit: manifest diff plus `deps.lock` diff.

## Adding

```sh
rig add org.clojure/data.json 2.0.0        # explicit version
rig add org.clojure/data.json             # newest eligible version
rig add com.example/thing -p modules/app --shared=false  # one module only…
rig add com.example/thing --alias test    # …or under an alias's :extra-deps
```

By default, Rig adds the requirement as **shared**: Rig records it in the
`:rig/deps` of the workspace and propagates it to every module that
declares the coordinate. `-p` chooses which module's `:deps` receives the
edit. `--shared=false` keeps the requirement local to that module. Without
a version, Rig selects the newest available version, subject to the
[cooldown](../concepts/security.md#cooldowns-the-adoption-window).

A successful `rig add` prints what it did:

```
deps.edn: set org.clojure/data.json "2.0.0"
deps.edn: set org.clojure/data.json "2.0.0"
pinned org.clojure/data.json 2.0.0 (explicit)
wrote deps.lock: 33 artifacts, 2 modules
```

The `set` lines list each manifest edit (the `:deps` of the target module
and the `:rig/deps` of the root, plus any other affected modules). The last
line confirms the new lockfile.

If the re-resolve fails — a bad version, an artifact that does not exist,
a repo you cannot reach — Rig reverts the manifest edits byte-for-byte.
It prints `reverted N manifest file(s): the edit failed` and exits
non-zero. Rig does not update the lockfile. A failed change leaves no
trace: Rig rejects cooldown refusals (exit 5) and unknown coordinates
before it makes any edit.

## Removing

```sh
rig remove org.clojure/data.json
```

`rig remove` deletes the shared requirement (from `:rig/deps` and from
every module declaring it) and re-locks:

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
  (`respect-existing-pins`). Only floating requirements (`RELEASE` or
  `LATEST` — see the `floating-version` finding of `check`) re-select.
- **coord + version** — an explicit pin. This is your judgment, and it
  always wins. The lockfile records it as `pinned … (explicit)`.
- **coord only** — the newest eligible version, gated by the cooldown. If
  the newest version is too fresh, Rig refuses it (exit 5) and names the
  cooldown:

  ```
  refused org.clojure/tools.logging (cooldown 48h)
  1 requirement(s) refused by cooldowns (retry with --force)
  ```

  `--force` takes the fresh version anyway and records it in the lockfile
  as `forced …`.

All forms propagate **shared** by default (the root `:rig/deps` plus every
module declaring the coordinate). Rig preserves the format: comments and
layout stay untouched, one visible diff per file. `--shared-only` updates
just the shared requirement. `--alias <a>` targets the `:extra-deps` of an
alias.

## What to look at after a change

1. `git diff` — the manifest edits should be exactly the version strings
   you expected, nothing else.
2. The lock output — `wrote deps.lock: N artifacts, M modules`; review the
   `deps.lock` diff for new and changed `artifacts` entries.
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

`rig outdated` prints the pinned version, the newest version that still
satisfies the requirement, and the overall latest version. It marks
breaking updates. `--breaking` lists only breaking updates. When nothing
changed, the whole output is `up to date`.

```sh
rig tree -p modules/orchestrator
```

```
com.example/orchestrator:1.0.0-SNAPSHOT
├── org.clojure/clojure:1.11.0
│   ├── org.clojure/spec.alpha:0.3.218
│   └── org.clojure/core.specs.alpha:0.2.62
└── …
```

`rig tree` prints the resolved dependency tree of a module, one line per
occurrence. Rig marks an occurrence that is not in the classpath (conflict,
exclusion, duplicate) with its reason. `--alias test` includes the
extra-deps of the test alias.
