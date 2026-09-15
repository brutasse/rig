# Getting started

## Install

One-shot userland install from the GitHub release artifacts:

```sh
curl -fsSL https://raw.githubusercontent.com/brutasse/rig/PENDING-FIRST-RELEASE/scripts/install-rig.sh | sh
```

The script installs `rig` to `~/.local/bin` (override with `INSTALL_DIR=…`)
and verifies the binary against the release's `SHA256SUMS` file before
installing it. Install a specific release instead of the latest:

```sh
curl -fsSL …/scripts/install-rig.sh | sh -s -- v0.2.0
```

The resolver kernel — a pinned Clojure jar that rig uses for the "cold"
commands — is not installed with the binary. On first use, rig fetches it
from the matching GitHub release and hash-verifies it into its state
directory. Cold commands are the ones that resolve, build, publish, or edit
manifests (`lock`, `update`, `build`, `check`, …); the day-to-day commands
(`test`, `run`, `repl`, …) run purely on the binary and the lock.

### Keeping up to date

```sh
rig self-update                    # update to the latest release, if newer
rig self-update --check            # report only, change nothing
rig self-update --version v0.2.0   # update to a specific release
```

The binary must live in a writable directory for `self-update` to work
(`~/.local/bin` from the installer is). rig also checks for new releases at
most once per 24 hours and prints one line on stderr when a newer release
exists; the check is skipped under `--offline` and on local/dev builds.

## Prerequisites

- **A JDK on `PATH` or in `JAVA_HOME`** — or let rig install one: a
  `:rig/jvm` pin (or `rig jvm install <version>`) fetches a hash-verified
  Temurin into the rig state dir. When no JVM is found at all, the error
  tells you which version to install (the current LTS).
- **`clj-kondo` on `PATH`**, only for `rig lint`.
- **`~/.m2/settings.xml`** with credentials for your private Maven
  repositories, if you use them (same file Maven and tools.deps use today).

That is the whole list. No Clojure CLI required — rig does not shell out to
`clj`.

## Your first project

Scaffold a project:

```sh
rig new com.example/demo
```

```
created project demo (com.example/demo)
next: cd demo && rig lock && rig test
```

The root manifest pins the current LTS JVM (`:rig/jvm "25"` — omitted under
`--offline`, or when the Adoptium lookup fails); `rig lock` installs it into
the rig state dir on first use, so the JDK prerequisite above is optional
for new projects.

The layout:

```
demo/
├── deps.edn                          # workspace root: :rig/modules
└── modules/demo/
    ├── deps.edn                      # the module manifest
    ├── src/com/example/demo/core.clj # a runnable main
    └── test/com/example/demo/core_test.clj
```

The module `deps.edn` is a standard tools.deps manifest plus the `:rig/*`
keys:

```edn
{:rig/lib com.example/demo
 :rig/main com.example.demo.core
 :paths ["src"]
 :deps {org.clojure/clojure {:mvn/version "1.12.5"}}
 :aliases
 {:test {:extra-deps {lambdaisland/kaocha {:mvn/version "1.66.1034"}}
         :extra-paths ["test"]
         :exec-fn kaocha.runner/exec-fn}}}
```

`:rig/lib` is the Maven coordinate the module publishes under, `:rig/main`
the namespace `rig run` launches, and the `:test` alias declares how `rig
test` runs tests (any test runner's `exec-fn` works).

Lock and run the tests:

```sh
cd demo
rig lock
```

```
wrote /…/demo/deps.lock: 29 artifacts, 2 modules
```

`rig lock` resolves every module, fetches and sha256-hashes every artifact
in the tree, and writes `deps.lock` at the workspace root. Commit the lock —
it is part of your source.

```sh
rig test
```

```
1 tests, 1 assertions, 0 failures.
```

Run the main:

```sh
rig run -p modules/demo rig
```

```
hello rig
```

## Orientation

`rig info` summarizes where you stand:

```
project:	/home/…/demo
type:	workspace
modules:	., modules/demo
lock:	2026-09-18T10:00:00Z by io.github.brutasse/rig-resolver 0.1.0 (546d868c4d1c), 29 artifacts
java:	/usr/bin/java (21.0.12.1)
cache:	/home/…/.local/share/rig
rig:	dev (dev)
kernel:	io.github.brutasse/rig-resolver 0.1.0 (546d868c4d1c)
```

`rig version` prints the project version: the `VERSION` file of the target
module (falling back to the workspace root), or the locked version of the
module when no file exists.

## Single module vs workspace

A directory with a `deps.edn` but no `:rig/modules` key is a
**single-module** project: the directory itself is the module, and
`deps.lock` lives next to the manifest.

A `deps.edn` containing `:rig/modules` is a **workspace**: the listed
directories are modules, and the root's `deps.edn` may also carry shared
requirements under `:rig/deps` and its own `:deps`/`:aliases` (resolved like
any module). Most multi-module projects are workspaces; see
[workspaces](concepts/workspace.md) for the model and
[onboarding a plain tools.deps project](migration/plain-tools-deps.md) for
turning one into a workspace.
