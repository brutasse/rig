# Releases

Changelog for Rig.

## Unreleased

## v0.3.1 — 2026-10-07

### Fixed

#### Managed JDK and GraalVM installs on macOS

Temurin and GraalVM CE ship their macOS archives in the bundle
layout, with the JDK under `Contents/Home`. The managed stores
verified installs by stat'ing `bin/java` (and `bin/native-image`)
at the install root, so a complete macOS install never validated:
every command touching it failed with `corrupt install (missing
bin/java)` — rig jvm install, jvm path, and anything downstream.
Installs now resolve the home inside the bundle, and `rig jvm path`
prints the correct macOS JAVA_HOME.

## v0.3.0 — 2026-10-07

### New

#### Kernel checkpointing (CRaC)

On Linux, Rig restores the kernel JVM from a checkpoint of a warmed
run instead of forking a cold one: ~0.3 s instead of 1–3 s per lock,
check, tree — and every restored run re-dumps the checkpoint, so it
stays current with use. Everything stays opportunistic: without a
CRaC-capable JVM every command behaves exactly as before.
`rig crac install` fetches the Zulu CRaC JDK pinned by this release
(sha256-verified, into the state dir); `rig crac status` and
`rig crac clean` inspect and prune it. Image chains are keyed to the
kernel jar bytes, the JVM build and the CPU features — a rotated
kernel or JDK starts a fresh chain instead of restoring stale state,
and old-kernel chains are swept automatically by a daily boot-time
pass.
Config that rides the JVM environment in cold runs
(`RIG_REPO_TOKENS`, `RIG_PROXY_REPOS`) rides the request file for
restored runs and is authoritative there: a restored kernel never
inherits the bootstrap run's tokens. See
[Configuration — Kernel checkpointing](reference/config.md#kernel-checkpointing-crac).

#### Maven coordinates in built artifacts

A module with `:rig/lib` and a version now carries
`META-INF/maven/<group>/<artifact>/pom.{xml,properties}` in everything
it builds — jar, uberjar and native image. The pom is the deployed
one: same coordinates, same lock-pinned dependencies, as
`rig publish` deploys them. It is the convention every jar scanner
attributes through, and the classic version-self-report
(`io/resource` on `pom.properties`): an artifact built by Rig now
reports about itself the coordinates it deploys under.

#### `rig jvm path` and `rig graalvm path`

`rig jvm path <major>` and `rig graalvm path <major>` print the
`JAVA_HOME` / `GRAALVM_HOME` of the newest installed managed JDK or
GraalVM satisfying the major — a bare path on stdout, for scripts and
CI. The state dir is read only: the commands never install, never
download, and need no network; when nothing matches they fail (exit 2)
with the install hint. `setup-rig` uses them to expose the pinned
JVM's homes as action outputs.

### Fixed

* **Ubers carried no classes their AOT bytecode linked against.**
  Source-only dependencies (`org.clojure/tools.namespace`,
  `rewrite-clj`) were loaded by the build's preloads without ever
  emitting classes: the build went green while the uber died at first
  use with a `ClassNotFoundException`. Uber preloads now run with the
  compile path at the working class dir, so source-only
  dependencies — and their transitive requires — ride into the uber
  as classes, with no declaration. `:rig/ns-compile` entries now
  compile unconditionally, even when a preload already loaded them:
  asking for a compile means it.
* **Temurin's legacy JDK 8 asset names** (`jdk8u492-b09`) did not
  parse as a release version, so the managed-JDK resolver could not
  match its own installed JDK 8. They now parse as `8.0.492+09`.

## v0.2.1 -- 2026-10-07

### Fixed

* [build: mark source namespaces for native build-time
  init](https://github.com/brutasse/rig/pull/72)

## v0.2.0 — 2026-10-06

*Everything merged since the first release.*

### New

#### `rig launch` — the production entrypoint

`rig launch` runs the built artifact with the production JVM flag set of
Rig: G1 (`AlwaysPreTouch`), exit on out-of-memory (with heap dump), and
loopback-only JMX on port 10101. You can override these per module via
`:rig/launch-opts` and per deployment via the `RIG_LAUNCH_OPTS` env var
(appended last, same override rules). It replaces its own process with
the JVM (process exec) — in a container the app is PID 1 and signals
reach the JVM directly. `rig build` bakes the launch plan (main, launch
opts, build JVM) into the jar of a module that declares a main, as
`META-INF/rig/launch.json` (uberjars pick it up too). So
`rig launch <jar>` works standalone, outside the workspace — for example
as a container `ENTRYPOINT`. It runs what Rig already built and cached.
The major version of the launch JVM must match the major version of the
build JVM. See
[Configuration — Production launch](reference/config.md#production-launch-rig-launch)
and [Docker](workflows/docker.md).

#### Native-image builds

`rig build --native` compiles the locked classpath into a GraalVM
native-image binary (default `target/<lib>`, `:rig/native-file`, extra
arguments `:rig/native-opts`; the entry point is `:rig/main`). Rig
derives the GraalVM version from the `:rig/jvm` pin of the workspace
and records it in the lock (`{vendor, requested}`). Rig manages GraalVM
community JDKs itself, one per major version — `rig graalvm install /
list / uninstall / update`, downloaded from the
`graalvm/graalvm-ce-builds` GitHub releases, sha256-verified, into the
state dir. The build never downloads: when Rig has installed no GraalVM
for the major, `--native` fails with a `rig graalvm install <major>`
hint. See
[Configuration — Native images](reference/config.md#native-images-rig-build-native).

#### Leiningen migration

`rig migrate` now converts a Leiningen workspace in place (`--dry-run`
first): `project.clj` → root and per-module `deps.edn` with `:rig/*`
keys. Rig materializes `:managed-dependencies` into the `:deps` of the
modules that inherit them. A declared version or source key wins over
the pool entry, and each win is a per-dep warning. Rig decomposes
`:sub` monorepos into per-module manifests with `:local/root` sibling
deps. Rig merges `:provided` into the base (Leiningen has it active by
default). `rig migrate` reports unexpressible keys and blocking
problems (nested `:sub`, `:sign-releases true`, an `s3p://` deploy
repository). After you migrate, run `rig lock` and `rig check`. See
[Migrating from Leiningen](migration/leiningen.md) for the full key
mapping.

#### Prep libraries and native javac

Rig now honors the standard tools.deps `:deps/prep-lib` key. Before
`rig build`, `rig test`, `rig run` and `rig repl`, Rig runs the prep
function of the target module and of every local dependency module that
declares one, in dependency order. Rig checks each prep for staleness
(manifest hash, source digests, locked dependencies, function name),
and the clean step of the build never deletes what the prep `:ensure`s.
If the prep is "javac my own sources", Rig does it natively. Rig itself
compiles modules that declare `:rig/java-src-dirs`, as part of prep, on
their locked classpath. javac runs before the prep function and before
the Clojure compile, so module code can reference its own Java classes.
A module declaring both runs the javac first, then the function, each
with its own staleness stamp. See [Java sources](concepts/java.md).

#### AOT-compile in `rig check` and `rig build`

Stage 2 of `rig check` now AOT-compiles the namespaces of each module in
dependency order, and it preloads the non-project requires first.
`rig build` compiles AOT the same way. Rig does not compile a namespace
again when a preload already loaded it, so its top level never runs
twice — except the namespaces you list in `:rig/ns-compile`, which
compile unconditionally: asking for a compile means it. Code that
references a namespace it does not require — a bare ns/var, or a class
only produced by AOT-compiling a sibling namespace — previously failed
with a `ClassNotFoundException`. Both paths now compile it. New key
`:rig/ns-compile` lists extra namespaces to AOT-compile alongside the
sources of the module itself. An uber build additionally loads its
preloads through the working class dir, so dependencies that ship only
sources and no classes (`org.clojure/tools.namespace`, `rewrite-clj`)
land in the uber as classes: without them the module's AOT bytecode
references interfaces that exist nowhere on the artifact's classpath,
and the uber dies with a `ClassNotFoundException` when that code first
loads.

#### New configuration

| Key | Declared in | Effect |
|---|---|---|
| `:rig/compile-jvm-opts` | workspace root | JVM flags for the build and check JVMs (the AOT build and the check load/AOT-compile). Version-sensitive flags like `--enable-preview` require a `:rig/jvm` pin; a lock with the flag and no pin is rejected. |
| `:rig/launch-opts` | module | JVM flags for `rig launch`; baked into the artifact's launch descriptor. |
| `:rig/javac-opts` | module | javac options; when the workspace pins `:rig/jvm`, `--release <n>` is prepended unless the opts already set the source level. |
| `:rig/native?` / `:rig/native-file` / `:rig/native-opts` | module | Native-image build: enable, output path, extra arguments. |
| `:rig/ns-compile` | module | Extra namespaces AOT-compiled alongside the module's own sources. |
| `:rig/timestamp-string` | module | Fixed zip entry timestamp (entries written name-sorted) — with a pinned `:rig/jvm`, the build output is byte-reproducible. Pre-1980 values are refused. |

Rig now documents and honors the standard tools.deps `:jvm-opts` key.
It applies to dev execution — `rig run`, `rig repl`, `rig exec`, `rig
test` — and to the prep function of the module. It never applies to
`rig build`, `rig check` or `rig launch`
([JVM flags](reference/config.md#jvm-flags)).

`rig build` now derives the directories it packages from the `:paths` of
the module, when `:rig/artifact-dirs` is absent. Previously it fell back
to the hardcoded `["src" "resources"]`, so a module with non-default
source directories built an empty jar. Rig renames `:rig/src-dirs` to
`:rig/artifact-dirs`, and the lock schema bumps to **v2**. Lock-consuming
commands reject v1 locks with an unsupported-version error. `rig lock`,
`rig add`, `rig remove` and `rig update` migrate a v1 lock in place. See
[Lockfile — anatomy](concepts/lockfile.md).

### Changed

- **JVM version inputs are major-version only.** `:rig/jvm` and every
  JVM version input (`rig jvm install`, `rig graalvm install`) accept a
  major (feature) version — `"21"`, not `"21.0.10+7"`. The major is what
  matters for compatibility, and Rig manages the exact release itself.
  The `jvm` / `graalvm` blocks in `deps.lock` no longer record an exact
  `version`; lock files written by an earlier build keep theirs, which
  Rig now ignores — re-lock to rewrite them.
- **The rig-managed JDK takes precedence.** When the lock pins a JVM,
  Rig uses the rig-managed JDK for the major from the state dir, even
  when the system `java` has the same feature version. A matching system
  JDK (`JAVA_HOME`, then `PATH`) serves only when Rig installed no
  managed JDK. With no matching JDK anywhere, online Rig installs the
  newest matching release on demand; offline it fails with an install
  hint.
- **One JDK per major version.** `rig jvm install` no longer upgrades:
  when the store already holds a JDK for the major, it exits without
  changing anything. `rig jvm update` and `rig graalvm update` are now
  store-level commands: they move the rig-managed install for the pinned
  major of the workspace to the newest release, and they replace it.
  They install the newest release when the store holds none. They no
  longer write `deps.lock`, and `--frozen` no longer applies to them.
- **The shared pool force-pins.** A `:rig/deps` entry now pins the
  coordinate's version workspace-wide: a module that consumes it
  transitively gets the pool version, like Leiningen's managed pool.
  A pool entry nothing consumed no longer holds `rig check` red.
- **A hung test run now dies.** `rig test` kills the runner JVM when an
  exec-fn has not finished in 30 minutes and fails the run — `--timeout`
  configures it, `0` disables it.
- **Stricter CLI.** `rig lock --frozen` is refused (writing the lock is
  `rig lock`'s job), and a repeated `-p/--path` is refused — both usage
  errors (exit 2). Before, both quietly did something else.
- **The GitHub API lookup uses a token.** `rig graalvm install` and
  `rig self-update` resolve releases through the GitHub API, which allows
  60 requests per hour per IP address. Shared CI runners exhaust that
  pool. Rig sends the bearer token of `GH_TOKEN` or `GITHUB_TOKEN` (the
  `gh` CLI's names, in that order) on the API request, and sends it to no
  other host. GitHub Actions exports `GITHUB_TOKEN` for every step, so CI
  needs no configuration. Downloads and their `.sha256` sidecars carry no
  token.
- In a container, the declared JDK in the image base *is* the build JVM.
  The [Production launch](workflows/production-launch.md#the-docker-image)
  image builds on the `eclipse-temurin` base with Rig copied in — no
  `RUN rig jvm install` step. The runtime stage takes only the
  `rig` binary, and `rig launch` never touches the resolver kernel or
  the runner.

### Fixed

- **The AOT compile JVM.** The kernel forks one JVM to compile the
  namespaces of the module. `tools.build` chose that JVM from the
  environment: `JAVA_CMD`, then the `PATH` search, then `JAVA_HOME`. The
  host JVM compiled the code, while the kernel itself ran on the JVM Rig
  used for the workspace. A green build said nothing about the pinned
  JVM. The kernel now starts the fork on its own JVM — the JVM Rig
  launched for the workspace. `JAVA_CMD` and `JAVA_HOME` no longer select
  the fork's JVM. The build's kernel JVM also gets `JAVA_HOME` of the
  managed JDK, as the run and test JVMs already did. Code that starts a
  JVM at build time now sees the same JDK as in `rig run` and
  `rig test`.
- **Bytecode floor.** The kernel jar of v0.1.0 compiled without a
  `--release` pin and shipped a Java 21-targeted `Main.class` — it
  failed to load on any `:rig/jvm` pin below 21. Rig now compiles the
  kernel with `--release 8`. After every assembly, Rig scans the release
  jars: any class above the floor fails the build (multi-release jar
  entries only where the JVM can load them). With a `:rig/jvm` pin,
  `rig build` likewise scans every class in the jar or uberjar it
  produces — dependency classes included — against the pinned JVM, and
  fails listing the offending entries. (javac does not receive
  `--release` on Java < 9 hosts, where the flag does not exist.)
- **Sources in built jars.** The drop-sources step ran before the
  compile and wiped every `.clj`/`.cljc` from the class dir, so a
  source shipped as payload — a clj-kondo hook export, a `deps-new`
  template — vanished from the published jar. It now runs after the
  compile and drops only the sources paired with their AOT
  `__init.class` — the load-time recompile hazard (two class identities,
  `ClassCastException`) it exists to kill. Everything else ships as the
  data it is. The build never drops prep output under an `:ensure`
  path — it is an input of the build, not its output.
- **What the AOT plan compiles.** Every `.clj` under the `:paths` roots
  was treated as a namespace. A data source (a config map without an
  `ns` form), a file nested under a resources root, or a namespace
  requiring itself killed `rig check` and `rig build` outright. The plan
  now keeps only the files whose declared namespace matches their path,
  ignores self-requires, and reports what it left out — `rig check`
  shows a `plan-skip` warning instead of dying.
- **Java modules ran without their own classes.** Rig compiled a module
  declaring `:rig/java-src-dirs` into its class dir, then kept that dir
  off the dev classpath unless `:paths` named it too: `rig check`, `rig
  test` and `rig run` died with a `ClassNotFoundException` on the
  module's own Java classes. The class dir now follows the module's
  sources onto the classpath.
- **Class-dir clean.** The clean step of the build now deletes only
  class-dir content that is not under a declared `:ensure` path. A
  module whose prep owned the class dir previously lost the whole dir on
  every build. The jar shipped without the classes of the prep, and
  dependent builds failed with `ClassNotFoundException`. The staleness
  check did not notice this.
- **Lock resolver identity.** The CLI now stamps the `resolver` block of
  the lock at lock time — the kernel pin (lib/version/git sha) plus the
  sha256 of the kernel jar it actually launched. Before, the kernel
  self-reported from a build-info resource baked into the jar, which
  fell back to a placeholder (v0.1.0 + zeroed sha) whenever the resource
  was absent. `rig info` shows the short jar sha when present. Older
  locks load unchanged.
- **Non-module `:local/root` deps.** A `:local/root` dep can point at a
  directory with its own `deps.edn` that is not a declared workspace
  module (a dev/test overlay). Rig now locks such a dep as its own
  module entry and follows nested refs — its sources reach the
  classpath. Previously the lock absorbed it into the longest-prefix
  module, and `rig test` failed with `exec-fn not found`. An overlay is
  never a build/test/publish target.
- **Local module beats published coordinate.** When the same library
  enters a basis as both a `:local/root` and a `:mvn/version`
  requirement, the local module now wins in either direction. This case
  typically arises when a transitive POM references the published
  artifact of the module itself. Vanilla tools.deps cannot order the
  pair and fails. The lock records the local module, and the published
  coordinate never enters it.
- **`rig check`:**
  - a dependency declared with a classifier, for example
    `org.apache.kafka/kafka-clients` with a `$test` classifier, no
    longer reports a spurious `stale-lock … lock has no pin`. The pin
    key now includes the classifier.
  - unqualified dependency names (`aleph`) normalize to the qualified
    form the lock pins (`aleph/aleph`), with a `$classifier` suffix
    preserved.
  - Stage 2 now walks `.cljc` sources — previously it walked only `.clj`,
    so an unloadable `.cljc` namespace reported a vacuous ok.
  - `rig check` no longer requires `data_readers.clj` (a top-level map
    at a source root — the `clojure.main` classpath convention) as a
    namespace.
  - `rig check` reports a module that pins `org.clojure/clojure` below
    1.8.0 as `clojure-floor`, before it launches any JVM (the same gate
    applies to `rig test`).
  - with no lock at all, one message, exit 3 (the lock-problem code) —
    previously one stale-lock problem per module, exit 1.
- **Cache entries carry their extension.** An artifact sat in the cache
  at `<sha256>` with no suffix, so the JDK handed consumers `file:` URLs
  for classpath jars and resource copying out of them broke (a
  kaocha-cljs run died on `ENOENT`). Cache names now end in the lock's
  extension; existing caches rename into the layout on first hit.
- **`rig test` prints like the REPL.** Runner code saw
  `*print-namespace-maps*` false where the same classpath under
  `clojure -M` or lein sees true — the runner now runs under
  `clojure.main`'s bindings.
- **Publishing.** Rig drops direct-to-S3 publishing (`s3p://`
  repositories): publishing targets http/https Maven repositories only
  (S3 is what Pier provides). `rig migrate` reports a deploy repository
  with an `s3p://` URL as a blocking problem.
- **Manifest parsing.** The compact macro for namespaced maps
  (`{ns/…}`) now parses; `rig migrate` normalizes it to the non-compact
  form.
- **`rig version`** no longer prints a blank line (exit 0) when the lock
  records an empty version — it reports "no version found" (exit 1).
- **`:rig/version-fn :git-count-revs`** trims the version template's
  trailing newline out of the version — it used to name the artifact
  `app-1.0.1<newline>.jar`.
- **Leiningen migration, continued.** A bare exclusion symbol
  (`:exclusions [foo]`) now migrates to `foo/foo`, like Leiningen reads
  it — verbatim, tools.deps silently excluded nothing. Dropped Leiningen
  keys are warned about on the profile that carries them, and an entry
  appended to a manifest lands on its own line.
- **`rig build` warns when `:rig/uber-opts :exclude` excludes
  nothing.** `tools.build` matches patterns with `re-matches` (full
  match), so a prefix-style `^clojure/` silently shipped everything;
  only `dir/.*` actually excludes a dir. Every pattern that matches no
  entry, and every `:rig/uber-opts` key Rig does not consume, now gets
  named in a warning.

### Docs

- New pages: [Migrating from Leiningen](migration/leiningen.md) and
  [Java sources](concepts/java.md) (native javac, `:deps/prep-lib`,
  `:rig/javac-opts`). New reference sections:
  [JVM flags](reference/config.md#jvm-flags),
  [Native images](reference/config.md#native-images-rig-build-native)
  and [Production launch](reference/config.md#production-launch-rig-launch).
  The lockfile anatomy page now covers v2. The Docker entrypoint pattern
  uses `rig launch`.
- Messaging rework across the site.

### Under the hood

- Hot commands (`rig test`, `rig check` stage 2) now run on a separate
  pinned **runner jar** (`rig-runner`), an install-time artifact shipped
  next to the kernel jar. The Docker image pre-seeds it, so `test` and
  `check` work on read-only stores. When the runner fails, Rig fails:
  exit 1 for `rig test` and `rig check` — any other runner exit code
  becomes a plain Rig error; the dev commands (`run`, `repl`, `exec`)
  pass the child code through verbatim.
- CI now builds the kernel and runs its test suite on Temurin 8 — the
  floor gate that keeps the kernel jar loadable on the oldest supported
  JVM. CI fixed the tag-trigger behavior.
- The release workflow now propagates the released version into the
  README and docs examples (install one-liner, `self-update --version`
  examples), replacing the hand-pinning that had left stale `v0.2.0`
  examples in the docs.
- Rig removes `rig release` (breaking). A release is now a version
  commit plus `rig publish` plus a tag — the version history and the tag
  are yours, and Rig does the publish. See
  [Build, publish, release](workflows/build-publish.md#releasing).
- `rig migrate` drops the legacy `:exoscale.project/extra-clean-targets`
  with a warning: Rig has no `:rig/clean-dirs` key, `rig clean` removes
  the target dirs and nothing else.

## v0.1.0 — 2026-09-25

[GitHub release](https://github.com/brutasse/rig/releases/tag/v0.1.0).
First release of Rig — build and run Clojure projects the obvious way:
one binary, one manifest, one lockfile, one way to do each thing.

- **One binary, one manifest namespace, one lockfile.** All tool
  configuration lives under `:rig/*` in the `deps.edn` files you
  already have; standard tools.deps keys, aliases and CI keep working
  as-is. `deps.lock` pins every artifact in the resolved dependency
  tree by sha256. Classpaths reference only the lock (the closed
  build), and Rig hash-verifies every artifact before it enters a
  classpath.
- **Security at the adoption moment.** A cooldown window refuses
  freshly published versions (default 48h, per-repo overrides,
  `--force`), and `rig verify` is the CI security gate. Rig supports
  repositories behind an OIDC gate (`:auth :oidc`, `rig auth get`).
- **The full command set.** Locking and dependency changes: `lock`,
  `update`, `add`, `remove`. Consistency and execution: `verify`,
  `check` (lock-vs-manifest consistency plus namespace load per module),
  `test`, `run`, `repl`, `exec`. Artifacts: `build [--uber]`, `install`,
  `publish`, `release`. Inspection: `outdated`, `tree`, `info`,
  `version`. Hygiene: `clean`, `lint`, `fmt`. Project creation: `new`,
  `new-module`. Legacy conversion: `migrate`, for `:exoscale.*` /
  `:slipset.*` workspaces.
- **JVM management.** `:rig/jvm` pins the JVM of the project (Temurin).
  The lock records the exact release, and Rig installs missing JDKs into
  its state dir, hash-verified.
- **Installation.** A pinned one-liner installs from the GitHub release
  artifacts (sha256-verified against the `SHA256SUMS` of the release).
  The [`ghcr.io/brutasse/rig`](workflows/docker.md) Docker image ships
  multi-arch, with the kernel pre-baked and a Temurin 21 base.
  `rig self-update` checks for a release at most once per 24h.
