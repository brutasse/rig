# Releases

Changelog for Rig. The full commit-level history between versions:
[compare on GitHub](https://github.com/brutasse/rig/compare/v0.1.0...HEAD).

## Pending release

*Unreleased — everything since v0.1.0.*

### New

#### `rig launch` — the production entrypoint

`rig launch` runs the built artifact with Rig's production JVM flag set:
G1 (`AlwaysPreTouch`), exit on out-of-memory (with heap dump), and
loopback-only JMX on port 10101 — overridable per module via
`:rig/launch-opts` and per deployment via the `RIG_LAUNCH_OPTS` env var
(appended last, same override rules). It replaces its own process with
the JVM (process exec) — in a container the app is PID 1 and signals
reach the JVM directly. The launch plan (main, launch opts,
build JVM) is
baked into every jar and uberjar as `META-INF/rig/launch.json`, so
`rig launch <jar>` works standalone, outside the workspace — e.g. as a
container `ENTRYPOINT`. It runs what is already built and cached;
the launch JVM's major version must match the build JVM's. See
[Configuration — Production launch](reference/config.md#production-launch-rig-launch)
and [Docker](workflows/docker.md).

#### Native-image builds

`rig build --native` compiles the locked classpath into a GraalVM
native-image binary (default `target/<lib>`, `:rig/native-file`, extra
arguments `:rig/native-opts`; the entry point is `:rig/main`). The
GraalVM version is derived from the workspace's `:rig/jvm` pin and
recorded in the lock (`{vendor, requested}`). Rig manages GraalVM
community JDKs itself, one per major version — `rig graalvm install /
list / uninstall / update`, downloaded from the
`graalvm/graalvm-ce-builds` GitHub releases, sha256-verified, into the
state dir. The build never downloads: when no GraalVM for the major is
installed, `--native` fails with a `rig graalvm install <major>` hint. See
[Configuration — Native images](reference/config.md#native-images-rig-build-native).

#### Leiningen migration

`rig migrate` now converts a Leiningen workspace in place (`--dry-run`
first): `project.clj` → root and per-module `deps.edn` with `:rig/*`
keys. `:managed-dependencies` are materialized into the `:deps` of the
modules that inherit them (a declared version or source key wins over
the pool entry — each win is a per-dep warning); `:sub` monorepos are
decomposed into per-module manifests with `:local/root` sibling deps;
`:provided` is merged into the base (Leiningen has it active by
default). Unexpressible keys and blocking problems (nested `:sub`,
`:sign-releases true`, an `s3p://` deploy repository) are reported;
after migrating, run `rig lock` and `rig check`. See
[Migrating from Leiningen](migration/leiningen.md) for the full key
mapping.

#### Prep libraries and native javac

Rig now honors the standard tools.deps `:deps/prep-lib` key: before
`rig build`, `rig test`, `rig run` and `rig repl`, the prep functions of
the target module and of every local dependency module that declares one
run in dependency order, staleness-checked (manifest hash, source
digests, locked dependencies, function name), and the build's clean step
never deletes what the prep `:ensure`s. If the prep is "javac my own
sources", Rig does it natively: modules declaring `:rig/java-src-dirs`
are compiled by Rig itself as part of prep, on their locked classpath —
javac runs before the prep function and before the Clojure compile, so
module code can reference its own Java classes. A module declaring both
runs the javac first, then the function, each with its own staleness
stamp. See [Java sources](concepts/java.md).

#### AOT-compile in `rig check` and `rig build`

`rig check`'s stage 2 now AOT-compiles each module's namespaces in
dependency order (its non-project requires preloaded first), and
`rig build` compiles AOT the same way: a namespace a preload already
loaded is not compiled again, so its top level never runs twice. Code
that references a namespace it does not require — a bare ns/var, or a
class only produced by AOT-compiling a sibling namespace — previously
failed with a `ClassNotFoundException`; both paths now compile it. New
key `:rig/ns-compile` lists extra namespaces to AOT-compile alongside
the module's own sources.

#### New configuration

| Key | Declared in | Effect |
|---|---|---|
| `:rig/compile-jvm-opts` | workspace root | JVM flags for the build and check JVMs (the AOT build and the check load/AOT-compile). Version-sensitive flags like `--enable-preview` require a `:rig/jvm` pin; a lock with the flag and no pin is rejected. |
| `:rig/launch-opts` | module | JVM flags for `rig launch`; baked into the artifact's launch descriptor. |
| `:rig/javac-opts` | module | javac options; when the workspace pins `:rig/jvm`, `--release <n>` is prepended unless the opts already set the source level. |
| `:rig/native?` / `:rig/native-file` / `:rig/native-opts` | module | Native-image build: enable, output path, extra arguments. |
| `:rig/ns-compile` | module | Extra namespaces AOT-compiled alongside the module's own sources. |
| `:rig/timestamp-string` | module | Fixed zip entry timestamp (entries written name-sorted) — with a pinned `:rig/jvm`, the build output is byte-reproducible. Pre-1980 values are refused. |

The standard tools.deps `:jvm-opts` key is now documented and
honored: it applies to dev execution — `rig run`, `rig repl`, `rig
exec`, `rig test` — and the module's prep function; it never applies to
`rig build`, `rig check` or `rig launch`
([JVM flags](reference/config.md#jvm-flags)).

`rig build` derives the directories it packages from the module's
`:paths` when `:rig/artifact-dirs` is undeclared — previously it fell
back to the hardcoded `["src" "resources"]`, so a module with
non-default source directories built an empty jar. `:rig/src-dirs` is
renamed `:rig/artifact-dirs` and the lock schema bumps to **v2**:
lock-consuming commands reject v1 locks with an unsupported-version
error; `rig lock`, `rig add`, `rig remove` and `rig update` migrate a
v1 lock in place. See
[Lockfile — anatomy](concepts/lockfile.md).

### Changed

- **JVM version inputs are major-version only.** `:rig/jvm` and every
  JVM version input (`rig jvm install`, `rig graalvm install`) accept a
  major (feature) version — `"21"`, not `"21.0.10+7"`: the major is what
  matters for compatibility, and Rig manages the exact release itself.
  The `jvm` / `graalvm` blocks in `deps.lock` no longer record an exact
  `version`; lock files written by an earlier build keep theirs, which
  Rig now ignores — re-lock to rewrite them.
- **The rig-managed JDK takes precedence.** When the lock pins a JVM, a
  rig-managed JDK for the major in the state dir is used even when the
  system `java` has the same feature version; a matching system JDK
  (`JAVA_HOME`, then `PATH`) serves only when no managed JDK is
  installed. With no matching JDK anywhere, online Rig installs the
  newest matching release on demand; offline it fails with an install
  hint.
- **One JDK per major version.** `rig jvm install` no longer upgrades:
  when a JDK for the major is already installed it exits without
  changing anything. `rig jvm update` and `rig graalvm update` are now
  store-level commands: they move the rig-managed install for the
  workspace's pinned major to the newest release, replacing it (and
  install the newest when none is installed). They no longer write
  `deps.lock`, and `--frozen` no longer applies to them.
- In a container, the declared JDK in the image base *is* the build JVM:
  the [Production launch](workflows/production-launch.md#the-docker-image)
  image builds on the `eclipse-temurin` base with Rig copied in — no
  `RUN rig jvm install` step — and the runtime stage takes only the
  `rig` binary; `rig launch` never touches the resolver kernel or the
  runner.

### Fixed

- **Bytecode floor.** v0.1.0's kernel jar was compiled without a
  `--release` pin and shipped a Java 21-targeted `Main.class` — it
  failed to load on any `:rig/jvm` pin below 21. The kernel is now
  compiled with `--release 8`, and after every assembly the release
  jars are scanned: any class above the floor fails the build
  (multi-release jar entries only where the JVM can load them). With a
  `:rig/jvm` pin, `rig build` likewise scans every class in the jar or
  uberjar it produces — dependency classes included — against the
  pinned JVM and fails listing the offending entries. (javac is not
  passed `--release` on Java < 9 hosts, where the flag does not exist.)
- **Sources in built jars.** Jars and uberjars no longer bundle the
  module's own `.clj`/`.cljc` sources: a source next to its AOT
  `__init.class` in the jar is a load-time recompile hazard (two class
  identities, `ClassCastException`). Sources whose class file is
  present are dropped as well, and the build fails in the unlikely
  case a `.class` is older than its source. Prep output under an
  `:ensure` path is never dropped — it is the build's input, not its
  output.
- **Class-dir clean.** The build's clean step now deletes only
  class-dir content that is not under a declared `:ensure` path. A
  module whose prep owns the class dir previously had it wiped on every
  build — the jar shipped without the prep's classes and dependent
  builds failed with `ClassNotFoundException`, unnoticed by the
  staleness check.
- **Lock resolver identity.** The lock's `resolver` block is now
  stamped by the CLI at lock time — the kernel pin (lib/version/git
  sha) plus the sha256 of the kernel jar actually launched — instead of
  the kernel self-reporting from a build-info resource baked into the
  jar, which fell back to a placeholder (v0.1.0 + zeroed sha) whenever
  the resource was absent. `rig info` shows the short jar sha when
  present. Older locks load unchanged.
- **Non-module `:local/root` deps.** A `:local/root` dep pointing at a
  directory with its own `deps.edn` that is not a declared workspace
  module (a dev/test overlay) is now locked as its own module entry,
  nested refs followed — its sources reach the classpath. Previously it
  was absorbed into the longest-prefix module and `rig test` failed
  with `exec-fn not found`. An overlay is never a build/test/publish
  target.
- **Local module beats published coordinate.** When the same library
  enters a basis as both a `:local/root` and a `:mvn/version`
  requirement (typically a transitive POM referencing the module's own
  published artifact), the local module now wins in either direction —
  vanilla tools.deps cannot order the pair and fails. The lock records
  the local module; the published coordinate never enters it.
- **`rig check`:**
  - a dependency declared with a classifier (e.g.
    `org.apache.kafka/kafka-clients` with a `$test` classifier) no
    longer reports a spurious `stale-lock … lock has no pin` — the
    pin key now includes the classifier;
  - unqualified dependency names (`aleph`) normalize to the qualified
    form the lock pins (`aleph/aleph`), with a `$classifier` suffix
    preserved;
  - `.cljc` sources are now walked in stage 2 — previously only `.clj`
    was, so an unloadable `.cljc` namespace reported a vacuous ok;
  - `data_readers.clj` (a top-level map at a source root — the
    `clojure.main` classpath convention) is no longer required as a
    namespace;
  - a module pinning `org.clojure/clojure` below 1.8.0 is reported as
    `clojure-floor` before any JVM is launched (the same gate applies
    to `rig test`).
- **Publishing.** Direct-to-S3 publishing (`s3p://` repositories) is
  dropped — publishing targets http/https Maven repositories only (S3
  is what Pier provides). `rig migrate` reports a deploy repository
  with an `s3p://` URL as a blocking problem.
- **Manifest parsing.** The compact macro for namespaced maps
  (`{ns/…}`) now parses; `rig migrate` normalizes it to the non-compact
  form.

### Docs

- New pages: [Migrating from Leiningen](migration/leiningen.md) and
  [Java sources](concepts/java.md) (native javac, `:deps/prep-lib`,
  `:rig/javac-opts`); new reference sections:
  [JVM flags](reference/config.md#jvm-flags),
  [Native images](reference/config.md#native-images-rig-build-native)
  and [Production launch](reference/config.md#production-launch-rig-launch);
  lockfile anatomy updated to v2; Docker entrypoint pattern with
  `rig launch`.
- Messaging rework across the site.

### Under the hood

- Hot commands (`rig test`, `rig check` stage 2) now run on a separate
  pinned **runner jar** (`rig-runner`), an install-time artifact
  shipped next to the kernel jar — the Docker image pre-seeds it, so
  `test` and `check` work on read-only stores. The runner's exit code
  is Rig's.
- CI: the kernel is built and its test suite run on Temurin 8 — the
  floor gate that keeps the kernel jar loadable on the oldest
  supported JVM; tag-trigger behavior fixed.
- The release workflow now propagates the released version into the
  README and docs examples (install one-liner, `self-update --version`
  examples), replacing the hand-pinning that had left stale `v0.2.0`
  examples in the docs.

## v0.1.0 — 2026-09-25

[GitHub release](https://github.com/brutasse/rig/releases/tag/v0.1.0).
First release of Rig — build and run Clojure projects the obvious way:
one binary, one manifest, one lockfile, one way to do each thing.

- **One binary, one manifest namespace, one lockfile.** All tool
  configuration lives under `:rig/*` in the `deps.edn` files you
  already have; standard tools.deps keys, aliases and CI keep working
  as-is. `deps.lock` pins every artifact in the resolved dependency
  tree by sha256; classpaths reference only the lock (the closed
  build), and every artifact is hash-verified before it enters a
  classpath.
- **Security at the adoption moment.** Freshly published versions are
  refused by a cooldown window (default 48h, per-repo overrides,
  `--force`); `rig verify` is the CI security gate. Repositories behind
  an OIDC gate are supported (`:auth :oidc`, `rig auth get`).
- **The full command set.** Locking and dependency changes (`lock`,
  `update`, `add`, `remove`); consistency and execution (`verify`,
  `check` — lock-vs-manifest consistency plus namespace load per
  module, `test`, `run`, `repl`, `exec`); artifacts (`build [--uber]`,
  `install`, `publish`, `release`); inspection (`outdated`, `tree`,
  `info`, `version`); hygiene (`clean`, `lint`, `fmt`); project
  creation (`new`, `new-module`); and legacy conversion (`migrate`, for
  `:exoscale.*` / `:slipset.*` workspaces).
- **JVM management.** `:rig/jvm` pins the project's JVM (Temurin); the
  lock records the exact release, and Rig installs missing JDKs into
  its state dir, hash-verified.
- **Installation.** Pinned one-liner from the GitHub release artifacts
  (sha256-verified against the release's `SHA256SUMS`); the
  [`ghcr.io/brutasse/rig`](workflows/docker.md) Docker image
  (multi-arch, kernel pre-baked, Temurin 21 base); `rig self-update`
  with a release check at most once per 24h.
