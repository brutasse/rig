# Concepts

Two files describe a Rig project. Each file has a different job:

| File | Format | Who writes it | What it says |
|---|---|---|---|
| `deps.edn` | EDN (the standard tools.deps manifest) | You, by hand | what the project *requires* and how it is laid out |
| `deps.lock` | JSON | Rig, only | what the project *builds against*: exact artifacts, hashes, classpaths |

The manifest states the requirements. The lockfile records the resolved
result. A build never uses both at once: the hot path reads only the
lockfile.

## `deps.edn` keeps its job

The `deps.edn` file remains a standard tools.deps file. The `:paths`,
`:deps`, `:aliases`, and `:mvn/repos` keys behave exactly as they do with
the Clojure CLI. Editors (CIDER, clojure-lsp), one-off `clj -Sdeps`
commands, and other tools keep working on every module. They do not need to
know that Rig exists.

Rig adds one namespace of keys: `:rig/*`. This namespace holds the tooling
configuration of the project: the library coordinate, the main namespace,
and the build and publish settings. See [the config
reference](../reference/config.md) for the full table.

## `deps.lock` is the build plan

The `deps.lock` file is the complete, hashed plan for every module. It
contains the deduplicated artifact list, with a sha256 hash per artifact.
For each module, it contains the ordered classpath, the resolved aliases,
and the build, publish, and test parameters. The resolved aliases include
how to launch the tests. The file is JSON. Only Rig writes it, and **you
commit it**.

The consequences:

- **No hidden re-resolution.** `rig test` does not ask Maven what `RELEASE`
  means today. It runs the classpath that the lockfile records.
- **Reproducible builds.** The same source, the same lockfile, and the same
  cache produce the same bytes, on your laptop or in CI.
- **A stale lock does not pass silently.** Rig detects when you change a
  manifest after Rig wrote the lock. Rig re-hashes the file bytes; it does
  not parse the EDN. In development, Rig refreshes the lock. Under
  `--frozen`, Rig fails immediately. See [the lockfile
  page](lockfile.md) for the full mechanics.

## One obvious way to do things

Every operation has exactly one command. The behavior of the command does
not depend on which shell alias, Makefile target, or other tool-invocation
shortcut you use:

- run tests → `rig test`
- build the jar → `rig build`
- change a dependency → `rig add` / `rig update` / `rig remove`
- resolve → `rig lock`

`rig <command> --help` gives the authoritative documentation for that
command.
