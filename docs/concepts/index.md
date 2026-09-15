# Concepts

Two files describe a rig project, and they have strictly separated jobs:

| File | Format | Who writes it | What it says |
|---|---|---|---|
| `deps.edn` | EDN (the standard tools.deps manifest) | You, by hand | what the project *requires* and how it is laid out |
| `deps.lock` | JSON | rig, only | what the project *builds against*: exact artifacts, hashes, classpaths |

The manifest expresses requirements; the lock records the resolved outcome.
A build never looks at both at once — the hot path reads only the lock.

## `deps.edn` keeps its job

`deps.edn` is still a standard tools.deps file. `:paths`, `:deps`,
`:aliases`, `:mvn/repos` behave exactly as with the Clojure CLI. Editors
(CIDER, clojure-lsp), `clj -Sdeps`-style one-offs, and any other tooling
keep working on every module without knowing rig exists.

What rig adds is one namespace of keys, `:rig/*`, that carries project
tooling configuration — the library coordinate, the main namespace, build
and publish settings. See [the config reference](../reference/config.md)
for the full table.

## `deps.lock` is the build plan

`deps.lock` is the complete, hashed plan for every module: the deduplicated
artifact list with a sha256 per artifact, each module's ordered classpath,
its resolved aliases (including how to launch tests), and its
build/publish/test parameters. It is JSON, it is written only by rig, and
**you commit it**.

The consequences:

- **No hidden re-resolution.** `rig test` does not ask Maven what
  `RELEASE` means today; it runs the classpath the lock records.
- **Reproducible builds.** Same source + same lock + same cache → same
  bytes, on your laptop or in CI.
- **Stale locks are loud, not silent.** rig detects the moment a manifest
  changes after the lock was written (by re-hashing the file bytes — no
  EDN parsing) and either refreshes the lock in development or fails fast
  under `--frozen`. See [the lockfile page](lockfile.md) for the full
  mechanics.

## One obvious way to do things

Every operation has exactly one command, and the command's behavior does
not depend on which shell alias, Makefile target, or tool-invocation
trick you reach for:

- run tests → `rig test`
- build the jar → `rig build`
- change a dependency → `rig add` / `rig update` / `rig remove`
- release → `rig release`

`rig <command> --help` is the documentation of record for that command.
