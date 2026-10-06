# Commands

This page lists every command, what it does, and its flags.
`rig <command> --help` is the documentation of record. This page is the map.

## Overview

| Command | Path | Description |
|---|---|---|
| `lock` | cold | Resolve the workspace, hash every artifact, write `deps.lock`. |
| `update [coord [version]]` | cold | Re-resolve, or bump/pin one requirement (shared propagation). |
| `add <coord> [version]` | cold | Add a requirement (shared by default). |
| `remove <coord>` | cold | Remove a requirement (shared, from all modules). |
| `verify` | hot | Check every lock artifact against the cache. The CI security gate. |
| `check` | cold+hot | Lock-vs-manifest consistency + namespace load and AOT-compile per module. Never re-locks. |
| `test [opt value…]` | hot | Run the modules' test exec-fns on locked classpaths. |
| `run [args…]` | hot | Run the module's `:rig/main` on the (alias) classpath. |
| `launch [jar] [args…]` | hot | Launch the built artifact with Rig's production JVM flags (G1 + AlwaysPreTouch, exit-on-OOM, loopback JMX on 10101), overridable via `:rig/launch-opts` and the `RIG_LAUNCH_OPTS` env var. Replaces its own process with the JVM (process exec) — the app is the container's PID 1. The jar's baked launch plan, or the lock, supplies the main. Never re-locks, never uses the network. |
| `repl` | hot | `clojure.main` REPL on the (alias) classpath. |
| `exec <cmd> [args…]` | hot | Run a command with the locked classpath as `CLASSPATH`. |
| `build [--uber \| --native]` | cold | Jar / uberjar / native-image binary via the locked classpath. |
| `install` | cold | Install module jars into the local Maven repository. |
| `publish` | cold | Deploy module jars to their remote repository. |
| `outdated [--breaking]` | cold | Pinned vs available versions. |
| `tree [--alias a]` | cold | Dependency tree for a module. |
| `clean` | hot | Remove build-output directories. |
| `lint` | hot | clj-kondo on the module (no lock needed). |
| `fmt [--check]` | hot | cljfmt on the module, pinned version (no lock needed). |
| `new <group/name>` | — | Scaffold a new project. |
| `new-module <name>` | cold | Scaffold a module in the workspace, add it, re-lock. |
| `migrate [--dry-run]` | cold | Convert a legacy (`:exoscale.*` / `:slipset.*`) or Leiningen (`project.clj`) workspace to `:rig/*` in place. |
| `jvm install <major>` | — | Install the newest Temurin JDK for a major version into the Rig state dir. |
| `jvm list` | — | Installed JDKs + the system `java`. |
| `jvm uninstall <version>` | — | Remove an installed JDK. |
| `jvm update` | — | Update the rig-managed JDK for the pinned major to the newest release. |
| `graalvm install <major>` | — | Install the newest GraalVM community JDK for a major version into the Rig state dir. |
| `graalvm list` | — | Installed GraalVMs. |
| `graalvm uninstall <version>` | — | Remove an installed GraalVM. |
| `graalvm update` | — | Update the rig-managed GraalVM for the pinned major to the newest build. |
| `self-update [--check]` | — | Update the Rig binary from the GitHub releases. |
| `auth get [gate\|url]` | — | Print the bearer token of an OIDC gate (env, cache, or negotiated). |
| `version` | hot | Print the project version. |
| `info` | hot | Project and tool summary. |

*Hot* commands run from the lock (lock → hash-check → launch). They re-lock
only when the lock is stale and `--frozen` is not set. *Cold* commands do
deeper kernel work (resolve, build, publish, edit). They can write the lock
or the manifests. `launch` is hot because it never touches the kernel. It
treats the lock as inert data: it never checks staleness, never re-locks,
and never uses the network. `clean`, `version` and `info` read the
workspace and the lock — they never write the lock, stale or not.

## Locking and dependency changes

### `rig lock`

```
rig lock
```

Resolve every module of the workspace, fetch and sha256-hash every
artifact, and write `deps.lock`. The command prints skipped version
selections (cooldowns, forces) and a summary:

```
wrote /home/dev/app/deps.lock: 528 artifacts, 8 modules
```

`rig lock` refuses `--frozen` (exit 2): writing the lock is its whole
job. CI modes that must not modify the lock belong on the commands that
consume it. `--frozen` on `rig lock` used to be silently ignored while
the command rewrote the lock anyway.

When a cooldown refuses a requirement, the command aborts with exit 5:

```
refused org.clojure/tools.logging (cooldown 48h)
1 requirement(s) refused by cooldowns (retry with --force)
```

Requests to an `:auth :oidc` repository are routed through a local proxy
that injects the gate's bearer token; tools.deps itself never talks to
an authenticated repo. An artifact Maven already downloaded into
`~/.m2/repository` is accepted at lock time without refetching — Rig
verifies it against the published SHA1. A cold m2 therefore locks as
well, and every request that does go out bears the token. See
[authenticated
repositories](config.md#authenticated-repositories-auth-oidc).

### `rig update`

```
rig update [coord [version]]
```

| Form | Effect |
|---|---|
| `rig update` | Full re-resolve: floating requirements re-select the newest eligible version (cooldown-gated). Exact versions declared in the manifests stay put; the lock's current pins do not. |
| `rig update <coord> <version>` | Pin the exact version (explicit, recorded in the lock). |
| `rig update <coord>` | Newest eligible version (cooldown-gated, exit 5 if refused). |

Flags: `--alias <a>` targets the `:extra-deps` of an alias, and
`--shared-only` updates only `:rig/deps`. The global `--force` flag
bypasses cooldowns.

### `rig add` / `rig remove`

```
rig add <coord> [version]      # no version: newest eligible (cooldown-gated)
rig remove <coord>
```

`add` flags: `--alias <a>` adds the requirement under the `:extra-deps`
of an alias, and `--shared` defaults to `true`; `--shared=false` keeps
the requirement local. `-p <module>` chooses which module supplies the
`:deps` to edit. Shared propagation still applies unless you set
`--shared=false`.

### `rig migrate`

```
rig migrate [--dry-run]
```

`rig migrate` converts a legacy workspace into `:rig/*`, in place. A legacy
workspace uses the `:exoscale.project/*`, `:exoscale.deps/*`, and
`:slipset.deps-deploy/*` key namespaces (the output of `tools.project` and
`deps-modules`). The command reports every manifest:

```
deps.edn: migrated
modules/orchestrator/deps.edn: migrated
```

What it does, mechanically:

- Renames the `:exoscale.project/*` keys to `:rig/*`, including the inverted
  `:exoscale.project/bypass-test?` → `:rig/test?`.
- Materializes `:exoscale.deps/managed-dependencies` into the `:deps` of
  every module that inherits from it, then drops the pool with a warning.
  Rig does not carry the pool as a `:rig/deps` shared requirement.
  `:exoscale.deps/inherit` (`:all` or a subset) marks which coords
  inherit. The managed entry fills the keys a module leaves undeclared.
  When a module declares a version or source key, that key wins over the
  managed entry, and Rig reports each such case as a per-dep warning. Rig
  never rewrites such a key.
- Drops `:exoscale.deps/managed-aliases` (only the `:project` alias exists;
  Rig has no alias inheritance) and the `:project` aliases themselves.
- Rewrites `:slipset.deps-deploy/exec-args` into `:rig/publish`
  (`:repo`); publish enablement comes from the
  `:exoscale.project/deploy?` → `:rig/publish?` rename. A deploy repo
  with an `s3p://` URL is a blocking problem (Rig publishes to
  http/https repositories only).

`--dry-run` runs the same analysis and reports per-file changes without
writing anything. A blocking problem aborts the whole migration with exit
1, and Rig writes nothing. Blocking problems are, for example, an
inherit-only coord that is absent from the managed map, or
`:sign-releases? true`.

The same verb also converts a **Leiningen** workspace: `project.clj` →
root and per-module `deps.edn` — profiles become `:aliases`,
`:deploy-repositories` becomes `:rig/publish`, `~var` version shorthands
resolve, `:sub` monorepos decompose into sibling
`:local/root` modules. See
[Migrating from Leiningen](../migration/leiningen.md) for the full key
mapping.

After `rig migrate`, run `rig lock` and `rig check`. The check reports the
remaining drift between the module requirements and the lock (cross-module
conflicts, stale pins).

## Inspection

### `rig verify`

Re-check every lock artifact against the cache.

```
verified 528 artifacts (510 cached, 18 fetched, 2 git deps pinned by commit sha)
```

The command exits 4 on any hash mismatch. Under `--frozen`, the command
exits 3 when the lock is absent or stale. It is the intended first CI
gate.

### `rig check`

Two stages, one exit code:

1. **Consistency** (workspace-wide, read-only) checks: `stale-lock` (a
   requirement the pin does not satisfy). `conflict` (incompatible
   requirements). `drift` (a module requirement differs from the workspace
   requirement — a warning). `floating-version` (`RELEASE`/`LATEST` in a
   manifest — an error; fix it with `rig update <coord>`). `no-lib`
   (publish without `:rig/lib`). `unknown-repo` (the lock pins
   via a repo that no manifest declares). `pool-unpinned` (a
   `:rig/deps` entry the lock does not pin — a warning).
2. **Namespace load and AOT-compile**: Rig preloads the non-project
   requires on the locked base classpath of each target module, then
   AOT-compiles its namespaces in dependency order. A failing load or
   compile produces a `load-fail` error; a source the AOT plan left
   out — a data file without an `ns` form, a namespace whose name does
   not match its path — is a `plan-skip` warning. Rig reports a module that pins
   `org.clojure/clojure` below the 1.8.0 floor as `clojure-floor`, before
   it launches any JVM (the runner entry point cannot load on Clojure
   1.7.x). Stage 2 launches on the locked classpath as-is: on Clojure ≥
   1.9 the runtime itself requires `org.clojure/spec.alpha` to be present
   (it loads `clojure.core.server` at init). A module that declares no
   spec (directly or transitively) therefore fails with a `spec/alpha`
   class-not-found — a missing project dependency, not a Rig failure.

```
check: error [stale-lock] modules/app org.clojure/clojure: manifest requires "1.11.0"; lock pins "1.12.5" — run rig update
check: 1 error(s), 0 warning(s)
```

or, clean:

```
check: ok
```

A missing lock is the exception: one message, exit 3.

`-p` restricts stage 2 only. `check` never re-locks and never exits 3 for
a stale lock — it reports staleness as a problem and exits 1.

### `rig outdated`

```
mvxcvi/arrangement 1.2.0 -> 1.2.1 (latest 2.1.0, breaking)
```

`--breaking` lists only breaking updates. The command prints `up to date`
when it finds none.

### `rig tree`

Print the resolved dependency tree for a module (`-p`), one line per
occurrence with `├─`/`└─` connectors. Rig marks an occurrence that is not
in the classpath (conflict, exclusion, duplicate) with its reason.
`--alias <a>` includes the extra-deps of the alias.

### `rig info` / `rig version`

`info` prints the workspace summary: modules, lock state (and staleness),
JVM, cache dir, Rig and kernel versions. `version` prints the project
version from the `VERSION` file (module dir, then root) or the locked
module version.

## Running

### `rig test`

```
rig test [--timeout <dur>] [opt value…]
```

Run the test exec-fn of each target module (from the lock) on its locked
test classpath. Without `-p`, every module with a test exec-fn runs, in
the declaration order of `:rig/modules`. The arguments are EDN literals,
and Rig forwards them to the runner:

```
rig test :kaocha.filter/focus '[:unit]'
```

When a runner fails, Rig exits 1 and names the failing modules. A hung
run does not wedge CI: `rig test` kills a runner that has not finished
in 30 minutes and fails — `--timeout <dur>` configures the watchdog,
`0` disables it. A module that pins `org.clojure/clojure` below the
1.8.0 floor fails before anything runs (the same `clojure-floor` gate as
check).

### `rig run`

```
rig run [args…]
```

Launch the `:rig/main` of the module on the (alias) classpath. Program
flags after `--` pass through: `rig run -p modules/app -- --env dev`.
`--alias <a>` selects the classpath. No main → usage error.

### `rig launch`

```
rig launch [jar] [args…]
```

Launch the built artifact with the production JVM flags of Rig — the
entrypoint for a deployed app. `rig launch` replaces its own process with
the JVM (process exec): the app is the process itself. In a container,
the app is PID 1. Signals reach the JVM directly, and the exit code of
the JVM is the exit code of the process. On Windows, where there is no
process exec, rig forks the JVM and waits. Flags, in order:

1. **The production defaults of Rig**: G1 garbage collection
   (`-XX:+UseG1GC`, with `-XX:+AlwaysPreTouch`), exit on out-of-memory
   (`-XX:+ExitOnOutOfMemoryError`, plus
   `-XX:+HeapDumpOnOutOfMemoryError`), and loopback-only JMX on port
   **10101** (`-Dcom.sun.management.jmxremote` with `authenticate=false`
   and `ssl=false`). Rig pins the RMI port to 10101, and
   `-Djava.rmi.server.hostname=127.0.0.1` keeps JMX to same-host
   clients.
2. **The `:rig/launch-opts` of the module** — later flags override the
   defaults (last JVM flag wins). A garbage collector in
   `:rig/launch-opts`, for example `-XX:+UseZGC`, *replaces* the G1
   default: the JVM refuses to start with two collectors, so Rig drops
   its own rather than pass both.
3. **`RIG_LAUNCH_OPTS` (env var)** — per-deployment JVM flags,
   whitespace-separated, appended last; the same rules apply (last JVM
   flag wins; a collector in it replaces any earlier selection).
4. `-jar <uberjar>`, or `-cp <locked classpath> <main>` for the plain jar.

The first positional is the jar when it names an existing file. Otherwise
(in a workspace) all positionals go to the main. The jar is then the build
output of the target module from the lock (the uberjar when the module
declares one).

The launch plan (main, `:rig/launch-opts`, build JVM) comes from
`META-INF/rig/launch.json` — the descriptor `rig build` bakes into the
jar of a module that declares a main (uberjars carry it too) — so
`rig launch <jar>` works outside the workspace (a standalone container).
Without the descriptor, the lock is the plan.

`rig launch` is offline by definition: it runs what is already in the
cache, from a previous build. The lock is inert data for it: it ignores a
stale lock, and `--frozen` has no effect. Classpath artifacts must already
be in the cache. The major version of the launch JVM must exactly match
the major version of the build JVM; a different major fails with exit 2.
A missing artifact fails too — it is not fetched — as a plain error
(exit 1).

### `rig repl`

```
rig repl [--alias <a>]
```

`clojure.main` REPL on the (alias) classpath, in the module directory.

### `rig exec`

```
rig exec [--alias <a>] <command> [args…]
```

Run any command with the locked classpath that Rig exports as `CLASSPATH`
(and `JAVA_OPTS` from the module/alias JVM options), in the module
directory. `--alias <a>` selects the classpath. Rig exits with the exit code of the command. Use it for
scripts and tools Rig has no verb for.

## Authentication

### `rig auth get`

```
rig auth get [gate|url] [--flow browser|device]
```

Print the bearer token of an OIDC gate to stdout. The gate comes from the
gate config (`~/.config/rig/auth.yaml`): pass a gate name or a repository
URL the gate fronts, or nothing when the config holds a single gate. Rig
uses the `RIG_TOKEN_<GATE>` environment variable of the gate when you set
it, else the cached token of the gate, else a fresh negotiation with the
issuer of the gate. Rig verifies a freshly negotiated token against the
JWKS before caching it; a token you supply in the environment is used as
you supplied it. The output is machine-friendly: script it into a `curl`, or pipe it anywhere a
bearer belongs. See
[authenticated repositories](config.md#authenticated-repositories-auth-oidc).

## Building and publishing

### `rig build`

```
rig build [--uber | --native]
```

Build the jar of the module. `--uber` builds the uberjar (requires
`:rig/uberjar?`), and `--native` builds the native-image binary (requires
`:rig/native?` and the `:rig/jvm` pin of the workspace; the entry point
of the binary is `:rig/main`). Every build runs on the locked classpath.
With the `:rig/jvm` pin of the workspace, Rig scans every jar the build
produces against it as the bytecode floor. A class that would not load on
the pinned JVM fails the build (see [JVMs](config.md#jvms-rigjvm)).
The command prints `built <path>` on success. See
[Native images](config.md#native-images-rig-build-native).

### `rig install`

Install the module jar(s) into the local Maven repository — every module
with a `:rig/lib` without `-p`.

### `rig publish`

Deploy the module jar(s) to the remote repository from `:rig/publish`.
Rig deploys only the `:rig/publish?` modules, and Rig requires the
network. The repo (`:repo` is a `:mvn/repos` id or `clojars`) must be an
http/https Maven repository. Rig uploads the jar and the POM with
credentials from `~/.m2/settings.xml`. For a repo marked `:auth :oidc`,
Rig uploads instead with the bearer of the gate that fronts its `:url`
(the `RIG_TOKEN_<GATE>` variable of the gate, the cached token, or a
negotiated token).

## Hygiene and scaffolding

### `rig clean`

Remove the target directories of the target modules (one `cleaned <dir>`
line each).

### `rig lint`

Run clj-kondo (from PATH, with the config of the project itself) on the
module. No lock needed.

### `rig fmt`

Format the module with the cljfmt version pinned in the kernel jar —
everyone formats identically. `--check` reports without fixing. No lock
needed.

### `rig new` / `rig new-module`

```
rig new com.example/demo          # scaffold a project (workspace + module)
rig new-module reporting          # add a module to the current workspace, re-lock
```

`new` creates `<name>/deps.edn` (root, `:rig/modules`),
`<name>/modules/<name>/deps.edn`, a runnable `src`, and a test. `new-module`
scaffolds under `modules/`, adds the entry to `:rig/modules`, and
re-locks.

### `rig self-update`

Update the Rig binary from the GitHub releases (hash-verified against the
release `SHA256SUMS`, atomic replace). `--check` reports only;
`--version vX.Y.Z` targets an exact release. Release builds only. GitHub
limits unauthenticated release lookups to 60 requests per hour per IP
address; when you set `GH_TOKEN` or `GITHUB_TOKEN`, Rig authenticates the
lookup with it.

## JVM management

`rig` manages local Temurin (Eclipse Adoptium) JDKs, one per major
version. The requirement of the project is `:rig/jvm` in the root
`deps.edn` — a major (feature) version, for example `21`. The rig-managed
JDK takes precedence over the system `java`. A system JDK whose feature
version matches the pin serves only when no managed JDK exists. When
neither exists and `rig` is online, it installs the newest matching
release on demand.

### `rig jvm install`

```
rig jvm install 21        # newest 21.x
```

`<major>` is a major (feature) version. The command resolves the release
through the Adoptium API, downloads the archive for the current platform,
and verifies the sha256 against the published checksum of the API. It
extracts the JDK to the state dir
(`~/.local/share/rig/jdks/temurin-<version>/`). Rig keeps one JDK per
major version. When a JDK for the major already exists, this command
exits without changes — use `rig jvm update` to move it to the newest
release. `--offline` refuses (installing needs the network).

### `rig jvm list`

Installed managed JDKs, then the system `java` (from `JAVA_HOME` and
`PATH`):

```
installed:
  temurin 21.0.12.1+1  linux/x64  /home/…/.local/share/rig/jdks/temurin-21.0.12.1+1
system:
  21.0.5  /usr/lib/jvm/java-21-openjdk/bin/java (JAVA_HOME)
```

### `rig jvm uninstall`

```
rig jvm uninstall 21              # the installed 21.x
rig jvm uninstall 21.0.12.1+1     # exact, or unique prefix (rig jvm uninstall 21.0.12)
```

### `rig jvm update`

`rig jvm update` is a workspace command (it needs a `:rig/jvm` pin in the
lock). It updates the rig-managed JDK for the pinned major to the newest
release, and replaces the installed one. Rig keeps one JDK per major
version. When no JDK for the major exists, the command installs the
newest release. The command does not touch the manifest or the lock.
`--offline` refuses (updating needs the network).

## GraalVM management

`rig` manages GraalVM community JDKs, one per major version, for
native-image builds (`rig build --native`). Rig derives the requirement
from the `:rig/jvm` pin. You install missing GraalVMs explicitly:
`rig build --native` never downloads, it fails with a hint until
`rig graalvm install` has run.

### `rig graalvm install`

```
rig graalvm install 21        # newest 21.x build
```

`<major>` is a major (feature) version. The command resolves the build
through the `graalvm/graalvm-ce-builds` GitHub releases, downloads the
archive for the current platform, and verifies the sha256 against the
`.sha256` sidecar of the release. It extracts the GraalVM to the state dir
(`~/.local/share/rig/graal/graalvm-<version>/graal`). Rig keeps one
GraalVM per major version. When a GraalVM for the major already exists,
this command exits without changes — use `rig graalvm update` to move it
to the newest build. `--offline` refuses (installing needs the network).

### `rig graalvm list`

Installed managed GraalVMs:

```
installed:
  graalvm 21.0.2  linux/x64  /home/…/.local/share/rig/graal/graalvm-21.0.2/graal
```

### `rig graalvm uninstall`

```
rig graalvm uninstall 21             # the installed 21.x
rig graalvm uninstall 21.0.2         # exact, or unique prefix (rig graalvm uninstall 21.0)
```

### `rig graalvm update`

`rig graalvm update` is a workspace command. It needs a `graalvm` block in
the lock, so the workspace needs a `:rig/jvm` pin and a `:rig/native?`
module. It updates the rig-managed GraalVM for the pinned major to the
newest community build, and replaces the installed one. Rig keeps one
GraalVM per major version. When none exists, the command installs the
newest build. The command does not touch the manifest or the lock.
`--offline` refuses (updating needs the network).

## Global flags

| Flag | Meaning |
|---|---|
| `-p, --path <module>` | Target one module (default: all workspace modules, in `:rig/modules` order; `.` = root module). One module per command — a repeated `-p` is a usage error (exit 2). |
| `--offline` | Never use the network. Artifacts come from the cache or a checksum-checked `~/.m2`; fail if in neither. |
| `--frozen` | Never modify the lock; fail (exit 3) if it is missing or stale. The CI mode. `rig lock` refuses it — writing the lock is its whole job. |
| `--force` | Bypass cooldowns (the decision is recorded in the lock). |
| `--cache-dir <dir>` | State directory (artifact cache + kernel jars). Default `~/.local/share/rig`; honors `$XDG_DATA_HOME`. |
| `-v, --verbose` | Debug logging. |
| `--autocomplete [shell]` | Print the shell completion script to stdout: `bash`, `zsh`, `fish` or `powershell`; no value = detect the running shell. |

Shell completion (detects your shell; `-p` completes module paths,
`--alias` completes the aliases of the module):

```sh
eval "$(rig --autocomplete)"            # bash
source <(rig --autocomplete)            # zsh
rig --autocomplete | source             # fish
rig --autocomplete | Invoke-Expression  # PowerShell
```

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | failure — build/test/publish/resolve error, or a `check` error |
| 2 | usage error — bad flag, missing main, module not in the lock, … |
| 3 | lock problem — missing lock, or stale under `--frozen` |
| 4 | security failure — artifact hash mismatch / not in the lock |
| 5 | cooldown refused — retry with `--force` |

Commands that launch a child process (`run`, `repl`, `exec`, `lint`,
`fmt`) propagate the exit code of the child verbatim, above these.
`launch` is the purest form of it: it execs the JVM, so the exit code of
the JVM is the exit code of the process. `test` and `check` interpret
their runner instead: a failing runner makes Rig exit 1 with its own
message; any other runner exit code becomes a plain Rig error (exit 1).
