# Getting started

## Install

Run this command to install Rig from the GitHub release artifacts:

```sh
curl -fsSL https://raw.githubusercontent.com/brutasse/rig/1357c1500e43de592cbbfa24b4ac3e4500f530f8/scripts/install-rig.sh | sh
```

The script installs `rig` to `~/.local/bin`. Set `INSTALL_DIR=…` to choose
another directory. The script compares the binary with the `SHA256SUMS` file
of the release before it installs the binary. To install a specific release
instead of the latest release, run:

```sh
curl -fsSL …/scripts/install-rig.sh | sh -s -- v0.1.0
```

The binary does not include the resolver kernel. The resolver kernel is a
pinned Clojure jar. Rig uses it for the cold commands. On first use, Rig
fetches the kernel jar — and the runner jar that matches it, for the hot
commands — from the pinned GitHub release, verifies their hashes, and
stores them in the Rig state directory. Cold commands resolve, build, publish,
or edit manifests (`lock`, `update`, `build`, `check`, …). The day-to-day
commands (`test`, `run`, `repl`, …) run on the binary and the lockfile
alone.

### Keep Rig up to date

```sh
rig self-update                    # update to the latest release, if newer
rig self-update --check            # report only, change nothing
rig self-update --version v0.1.0   # update to a specific release
```

`self-update` needs a writable directory for the binary. The installer puts
the binary in `~/.local/bin`, which is writable. Rig also checks for new
releases at most once per 24 hours. When a newer release exists, Rig prints
one line on stderr. The `--offline` flag and local/dev builds skip this
check.

## Prerequisites

- **A JDK on `PATH` or in `JAVA_HOME`** — or let Rig install one. When a
  `:rig/jvm` pin names a major version, Rig can install a JDK. When no
  rig-managed JDK and no matching system JDK exist, Rig fetches the newest
  hash-verified Temurin for that major into the Rig state directory.
  `rig jvm install <major>` does the same. When Rig finds no JVM at all, the
  error names the version you must install (the current LTS).
- **`clj-kondo` on `PATH`**, only for `rig lint`.
- **`~/.m2/settings.xml`** with credentials for your private Maven
  repositories, if you use them. This is the same file that Maven and
  tools.deps use today.

Rig does not need the Clojure CLI. Rig does not run
`clj` as an external command.

## Your first project

Scaffold a project:

```sh
rig new com.example/demo
```

```
created project demo (com.example/demo)
next: cd demo && rig lock && rig test
```

The root manifest pins the current LTS JVM: `:rig/jvm "25"`. Rig omits the
pin under `--offline`, or when the Adoptium lookup fails. On first use, Rig
installs that JVM into the Rig state directory. For new projects, the JDK
prerequisite is therefore optional.

The layout:

```
demo/
├── deps.edn                          # workspace root: :rig/modules
└── modules/demo/
    ├── deps.edn                      # the module manifest
    ├── src/com/example/demo/core.clj # a runnable main
    └── test/com/example/demo/core_test.clj
```

The module `deps.edn` combines a standard tools.deps manifest with the
`:rig/*` keys:

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

The `:rig/lib` key is the Maven coordinate under which the module publishes.
The `:rig/main` key is the namespace that `rig run` launches. The `:test`
alias declares how `rig test` runs the tests. The `exec-fn` of any test
runner works.

Lock and run the tests:

```sh
cd demo
rig lock
```

```
wrote /…/demo/deps.lock: 29 artifacts, 2 modules
```

`rig lock` resolves every module, fetches every artifact in the tree, hashes
each artifact with sha256, and writes `deps.lock` at the workspace root.
Commit the lockfile: it is part of your source.

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

`rig info` summarizes the state of the project:

```
project:	/home/…/demo
type:	workspace
modules:	., modules/demo
lock:	2026-09-18T10:00:00Z by io.github.brutasse/rig-resolver v0.1.0 (546d868c4d1c, 823c7b174dfe), 29 artifacts
java:	/usr/bin/java (21.0.12.1)
cache:	/home/…/.local/share/rig
rig:	dev (dev)
kernel:	io.github.brutasse/rig-resolver v0.1.0 (546d868c4d1c)
```

`rig version` prints the project version. Rig reads the `VERSION` file of the
target module, or of the workspace root when the module has no `VERSION`
file. When no file exists, Rig prints the locked version of the module.

## Single module vs workspace

A directory with a `deps.edn` but no `:rig/modules` key is a
**single-module** project. The directory itself is the module, and
`deps.lock` lives next to the manifest.

A `deps.edn` that contains `:rig/modules` is a **workspace**. The listed
directories are modules. The `deps.edn` of the root can also carry shared
requirements under `:rig/deps`, plus its own `:deps` and `:aliases`. Rig
resolves them like the requirements of any module. Most multi-module
projects are workspaces. See [workspaces](concepts/workspace.md) for the
model, and [onboarding a plain tools.deps project](migration/plain-tools-deps.md)
to convert a project into a workspace.
