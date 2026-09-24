# Commands

Every command, what it does, and its flags. `rig <command> --help` is the
documentation of record; this page is the map.

## Overview

| Command | Path | Description |
|---|---|---|
| `lock` | cold | Resolve the workspace, hash every artifact, write `deps.lock`. |
| `update [coord [version]]` | cold | Re-resolve, or bump/pin one requirement (shared propagation). |
| `add <coord> [version]` | cold | Add a requirement (shared by default). |
| `remove <coord>` | cold | Remove a requirement (shared, from all modules). |
| `verify` | hot | Check every lock artifact against the cache. The CI security gate. |
| `check` | cold+hot | Lock-vs-manifest consistency + namespace load per module. Never re-locks. |
| `test [opt value…]` | hot | Run the modules' test exec-fns on locked classpaths. |
| `run [args…]` | hot | Run the module's `:rig/main` on the (alias) classpath. |
| `repl` | hot | `clojure.main` REPL on the (alias) classpath. |
| `exec <cmd> [args…]` | hot | Run a command with the locked classpath as `CLASSPATH`. |
| `build [--uber]` | cold | Jar / uberjar via the locked classpath. |
| `install` | cold | Install module jars into the local Maven repository. |
| `publish` | cold | Deploy module jars to their remote repository. |
| `release [--dry-run]` | cold+git | Strip-snapshot → publish → commit → tag → bump → commit → push. |
| `outdated [--breaking]` | cold | Pinned vs available versions. |
| `tree [--alias a]` | cold | Dependency tree for a module. |
| `clean` | hot | Remove build-output directories. |
| `lint` | hot | clj-kondo on the module (no lock needed). |
| `fmt [--check]` | hot | cljfmt on the module, pinned version (no lock needed). |
| `new <group/name>` | — | Scaffold a new project. |
| `new-module <name>` | cold | Scaffold a module in the workspace, add it, re-lock. |
| `migrate [--dry-run]` | cold | Convert a legacy (`:exoscale.*` / `:slipset.*`) workspace to `:rig/*` in place. |
| `jvm install <version>` | — | Install a Temurin JDK into the rig state dir. |
| `jvm list` | — | Installed JDKs + the system `java`. |
| `jvm uninstall <version>` | — | Remove an installed JDK. |
| `jvm update` | — | Bump the locked JVM to the newest release satisfying `:rig/jvm`. |
| `self-update [--check]` | — | Update the rig binary from the GitHub releases. |
| `auth get [gate\|url]` | — | Print the bearer token of an OIDC gate (env, cache, or negotiated). |
| `version` | hot | Print the project version. |
| `info` | hot | Project and tool summary. |

*Hot* commands run from the lock (lock → hash-check → launch), re-locking
only when the lock is stale and `--frozen` is not set. *Cold* commands do
deeper kernel work (resolve, build, publish, edit) and may write the lock
or manifests.

## Locking and dependency changes

### `rig lock`

```
rig lock
```

Resolve every module of the workspace, fetch and sha256-hash every
artifact, write `deps.lock`. Prints skipped version selections (cooldowns,
forces) and a summary:

```
wrote deps.lock: 528 artifacts, 8 modules
```

A requirement refused by cooldown aborts with exit 5:

```
refused org.clojure/tools.logging (cooldown 48h)
1 requirement(s) refused by cooldowns (retry with --force)
```

Maven requirements that live only in `:auth :oidc` repos are pre-seeded
into the local `~/.m2/repository` (every request bearing the token) before
resolution, so a cold m2 locks without tools.deps ever talking to an
authenticated repo — see [authenticated
repositories](config.md#authenticated-repositories-auth-oidc).

### `rig update`

```
rig update [coord [version]]
```

| Form | Effect |
|---|---|
| `rig update` | Re-resolve keeping existing pins; floating versions re-select (cooldown-gated). |
| `rig update <coord> <version>` | Pin the exact version (explicit, recorded in the lock). |
| `rig update <coord>` | Newest eligible version (cooldown-gated, exit 5 if refused). |

Flags: `--alias <a>` (target an alias's `:extra-deps`), `--shared-only`
(update only `:rig/deps`). Global `--force` bypasses cooldowns.

### `rig add` / `rig remove`

```
rig add <coord> [version]      # no version: newest eligible (cooldown-gated)
rig remove <coord>
```

`add` flags: `--alias <a>` (under an alias's `:extra-deps`),
`--shared` (default `true`; `--shared=false` keeps the requirement local).
`-p <module>` chooses which module's `:deps` receives the edit; shared
propagation still applies unless `--shared=false`.

### `rig migrate`

```
rig migrate [--dry-run]
```

One-shot conversion of a legacy workspace (the `:exoscale.project/*`,
`:exoscale.deps/*`, `:slipset.deps-deploy/*` key namespaces, as produced by
`tools.project` + `deps-modules`) into `:rig/*`, in place. Every manifest is
reported:

```
deps.edn: migrated
modules/orchestrator/deps.edn: migrated
```

What it does, mechanically:

- Renames the `:exoscale.project/*` keys to `:rig/*` (incl. the inverted
  `:exoscale.project/bypass-test?` → `:rig/test?`).
- Lifts `:exoscale.deps/managed-dependencies` into `:rig/deps` at the root
  and applies the merge semantics it used to: managed requirements win over
  module declarations; `:exoscale.deps/inherit` (`:all` or a subset) marks
  which coords inherit. The resulting *effective* dependencies are
  reproduced exactly — the migration introduces no version drift (fixing
  drift is what `rig check` + `rig update` are for).
- Drops `:exoscale.deps/managed-aliases` (only the `:project` alias exists;
  rig has no alias inheritance) and the `:project` aliases themselves.
- Rewrites `:slipset.deps-deploy/exec-args` into `:rig/publish?` +
  `:rig/publish` (`:repo`, `:sign-releases?`; s3p URLs carry over as
  `:mvn/repos` entries).

`--dry-run` runs the same analysis and reports per-file changes without
writing anything. Blocking problems (e.g. an inherit marker on a coord that
is not in the managed map, `:sign-releases? true`) abort the whole
migration with exit 1 and nothing is written.

After `rig migrate`, run `rig lock` and `rig check`: the check reports the
real drift the managed map had been masking.

## Inspection

### `rig verify`

Re-check every lock artifact against the cache.

```
verified 528 artifacts (510 cached, 18 fetched, 2 git deps pinned by commit sha)
```

Exit 4 on any hash mismatch; exit 3 if the lock is missing or stale under
`--frozen`. The intended first CI gate.

### `rig check`

Two stages, one exit code:

1. **Consistency** (workspace-wide, read-only): `stale-lock` (requirement
   not satisfied by the pin), `conflict` (incompatible requirements),
   `drift` (module requirement ≠ workspace requirement — warning),
   `floating-version` (`RELEASE`/`LATEST` in a manifest — error; fix with
   `rig update <coord>`), `no-lib` (publish/build without `:rig/lib`),
   `unknown-repo` (lock pins via a repo no manifest declares).
2. **Namespace load**: load every namespace of each target module on its
   locked base classpath; a failing load is a `load-fail` error.

```
check: error [stale-lock] modules/app org.clojure/clojure: manifest requires "1.11.0"; lock pins "1.12.5" — run rig update
check: 1 error(s), 0 warning(s)
```

or, clean:

```
check: ok
```

`-p` restricts stage 2 only. `check` never re-locks and never exits 3 for
a stale lock — it reports staleness as a problem and exits 1.

### `rig outdated`

```
mvxcvi/arrangement 1.2.0 -> 1.2.1 (latest 2.1.0, breaking)
```

`--breaking` lists only breaking updates. `up to date` when nothing.

### `rig tree`

Print the resolved dependency tree for a module (`-p`), one line per
occurrence with `├─`/`└─` connectors; an occurrence not in the classpath
(conflict, exclusion, duplicate) is marked with its reason. `--alias <a>`
includes the alias's extra-deps.

### `rig info` / `rig version`

`info` prints the workspace summary: modules, lock state (and staleness),
JVM, cache dir, rig and kernel versions. `version` prints the project
version from the `VERSION` file (module dir, then root) or the locked
module version.

## Running

### `rig test`

```
rig test [opt value…]
```

Run each target module's test exec-fn (from the lock) on its locked test
classpath. Without `-p`, every module with a test exec-fn runs, in
dependency order. Arguments are EDN literals forwarded to the runner:

```
rig test :kaocha.filter/focus '[:unit]'
```

The runner's exit code is rig's.

### `rig run`

```
rig run [args…]
```

Launch the module's `:rig/main` on the (alias) classpath. Program flags
after `--` pass through: `rig run -p modules/app -- --env dev`.
`--alias <a>` selects the classpath. No main → usage error.

### `rig repl`

```
rig repl [--alias <a>]
```

`clojure.main` REPL on the (alias) classpath, in the module directory.

### `rig exec`

```
rig exec <command> [args…]
```

Run any command with the locked classpath exported as `CLASSPATH` (and
`JAVA_OPTS` from the module/alias JVM options), in the module directory.
The command's exit code is rig's. The escape hatch for scripts and tools
rig has no verb for.

## Authentication

### `rig auth get`

```
rig auth get [gate|url] [--flow browser|device]
```

Print the bearer token of an OIDC gate to stdout. The gate comes from the
gate config (`~/.config/rig/auth.yaml`): pass a gate name or a repository
URL the gate fronts, or nothing when the config holds a single gate. The
token is the gate's `RIG_TOKEN_<GATE>` environment variable when set,
else the gate's cached token, else a fresh negotiation with the gate's
issuer, verified against its JWKS. Machine-friendly output — script it
into a `curl`, or pipe it anywhere a bearer is expected. See
[authenticated repositories](config.md#authenticated-repositories-auth-oidc).

## Building and publishing

### `rig build`

```
rig build [--uber]
```

Build the module's jar (or uberjar with `--uber`) on the locked classpath.
`built <path>` on success.

### `rig install`

Install the module jar(s) into the local Maven repository — every module
with a `:rig/lib` without `-p`.

### `rig publish`

Deploy the module jar(s) to the remote repository from `:rig/publish`.
Only `:rig/publish?` modules; network required. Two transports:

- **HTTP repos** (the default, `:repo` is a `:mvn/repos` id or `clojars`):
  jar + POM uploaded with credentials from `~/.m2/settings.xml`; a repo
  marked `:auth :oidc` is uploaded with the bearer of the gate that
  fronts its `:url` (the gate's `RIG_TOKEN_<GATE>`, cached token, or
  negotiated) instead.
- **`s3p://` repos** (`:mvn/repos` entry whose URL is
  `s3p://<bucket>[/<prefix>]`): a direct SigV4 write to the bucket —
  jar, POM and their `.sha1`/`.md5` sidecars under `<prefix>/<maven-path>`.
  Credentials: the settings.xml server for the repo id, then the AWS
  environment (`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`
  [+ `AWS_SESSION_TOKEN`]), then `~/.aws/credentials` / `~/.aws/config`
  (`AWS_PROFILE` honored).

### `rig release`

The full release sequence (see [build/publish/release](../workflows/build-publish.md)):
strip-snapshot → publish all → commit VERSION → tag → bump-and-snapshot →
commit → push. `--dry-run` prints the plan without doing anything.

## Hygiene and scaffolding

### `rig clean`

Remove the target directories of the target modules (one `cleaned <dir>`
line each).

### `rig lint`

Run clj-kondo (from PATH, with the project's own config) on the module.
No lock needed.

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

Update the rig binary from the GitHub releases (hash-verified against the
release `SHA256SUMS`, atomic replace). `--check` reports only;
`--version vX.Y.Z` targets an exact release. Release builds only.

## JVM management

`rig` manages local Temurin (Eclipse Adoptium) JDKs.
The project's requirement lives in `:rig/jvm` (root `deps.edn`); the lock
records the exact version; missing JDKs are installed on demand.

### `rig jvm install`

```
rig jvm install 21           # newest 21.x
rig jvm install 21.0.10+7    # exact release
```

Resolves the release through the Adoptium API, downloads the archive for
the current platform, verifies its sha256 against the API's published
checksum, and extracts it to the state dir
(`~/.local/share/rig/jdks/temurin-<version>/`). A no-op when the version is
already installed. `--offline` refuses (installing needs the network).

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
rig jvm uninstall 21.0.12.1+1   # exact, or unique prefix (rig jvm uninstall 21.0.12)
```

### `rig jvm update`

Workspace command: bumps the exact `jvm.version` recorded in `deps.lock` to
the newest release satisfying the manifest's `:rig/jvm` pin, and saves the
lock. The manifest is untouched. Refused under `--frozen`.

## Global flags

| Flag | Meaning |
|---|---|
| `-p, --path <module>` | Target one module (default: all, in dependency order; `.` = root module). |
| `--offline` | Never use the network. Artifacts come from the cache or a checksum-checked `~/.m2`; fail if in neither. |
| `--frozen` | Never modify the lock; fail (exit 3) if it is missing or stale. The CI mode. |
| `--force` | Bypass cooldowns (the decision is recorded in the lock). |
| `--cache-dir <dir>` | State directory (artifact cache + kernel jars). Default `~/.local/share/rig`; honors `$XDG_DATA_HOME`. |
| `-v, --verbose` | Debug logging. |
| `--autocomplete [shell]` | Print the shell completion script to stdout: `bash`, `zsh`, `fish` or `powershell`; no value = detect the running shell. |

Shell completion (detects your shell; `-p` completes module paths, `--alias`
completes the module's aliases):

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

Commands that launch a child process (`test`, `run`, `repl`, `exec`,
`lint`, `fmt`) propagate the child's exit code verbatim, above these.
