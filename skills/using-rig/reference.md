# Rig reference

Quick lookup. `rig <cmd> --help` is the documentation of record; full docs:
https://brutasse.github.io/rig/.

## Commands

| Task | Command |
|---|---|
| Project state (modules, lock, JVM, versions) | `rig info` (`-p` selects a module) |
| Add requirement | `rig add <coord> [version]` |
| Bump / pin requirement | `rig update <coord> [version]` |
| Remove requirement (shared, from all modules) | `rig remove <coord>` |
| Re-resolve + write `deps.lock` | `rig lock` |
| Verify lock + consistency + ns load/AOT | `rig check` (CI gate, stage 1) |
| Run tests on locked classpaths | `rig test [opt value…]` (stage 2) |
| Run the module's `:rig/main` | `rig run [args…]` (program flags after `--`) |
| nREPL server on the (alias) classpath | `rig nrepl [--alias <a>] [--host <i>] [--port <n>]` |
| Any command with the locked classpath as `CLASSPATH` | `rig exec [--alias <a>] <cmd> [args…]` |
| Jar / uberjar / native-image | `rig build [--uber \| --native]` |
| Launch built artifact (production JVM flags) | `rig launch [jar] [args…]` |
| Install module jars to local Maven repo | `rig install` |
| Publish to the `:rig/publish` repo | `rig publish` |
| Pinned vs available versions | `rig outdated [--breaking]` |
| Dependency tree of a module | `rig tree [-p <m>] [--alias <a>]` |
| Remove build-output dirs | `rig clean` |
| cljfmt (version pinned in the kernel) | `rig fmt [--check]` |
| Scaffold project / module | `rig new <group/name>` / `rig new-module <name>` |
| Convert legacy / Leiningen workspace | `rig migrate [--dry-run]` |

Cold commands (kernel work, may write the lock): `lock`, `add`, `remove`,
`update`, `new`, `new-module`, `build`, `install`, `publish`, `outdated`,
`tree`, `migrate`. Hot commands run from the lock and never write it:
`test`, `run`, `nrepl`, `exec`, `launch`, `clean`, `info`, `check` (verifies
the committed lock, reports staleness instead of failing it).

## Flags

- `-p, --path <module>` — target one module (`.` = root). Every module
  command accepts it; omitting it targets all modules in `:rig/modules`
  order.
- `--alias <a>` — run on an alias classpath (`test` carries the test
  runner + extra paths).
- `--shared=false` — keep a requirement local to the module (default:
  shared, propagated via root `:rig/deps`).
- `--shared-only` — update only the shared requirement.
- `--force` — bypass cooldowns (recorded in the lock).
- `--offline` — never use the network; artifacts from cache or
  checksum-checked `~/.m2` only (hermeticity proof).
- `-v, --verbose` — debug logging.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | failure (build/test/publish/resolve error, `check` error) |
| 2 | usage error (bad flag, missing main, module not in lock) |
| 3 | lock problem — missing or stale lock on a non-writing command |
| 4 | security failure — artifact hash mismatch / not in the lock |
| 5 | cooldown refused — retry with `--force` |

Commands that launch a child (`run`, `nrepl`, `exec`, `fmt`) propagate the
child's exit code.

## CI — complete example

```yaml
name: CI

on:
  push:
    branches: [main]

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1

      - name: Install rig (binary, JVM, cache)
        uses: brutasse/setup-rig@b204f6b12fde6574ebbebe0227fcaaa10391882f # v2.0.0
        with:
          version: v0.4.1   # the rig version that wrote this project's deps.lock

      - name: Check and test
        run: rig check && rig test

      - name: Build uberjars
        run: rig build --uber
```

Notes:

- Pin the action ref to a git sha and `version:` to the release that wrote
  the committed `deps.lock` (`rig info` shows it; the pins above are the
  current ones — re-check on a fresh setup). setup-rig verifies the binary
  against the release `SHA256SUMS`, installs the lock-pinned JVM,
  and caches the artifact store keyed on `deps.lock` — same lockfile = warm
  hit. The cache saves automatically at job end, even after failures.
- Git dependencies use the repo's normal git/ssh setup; their commit shas
  are in the lockfile, and checkouts are provisioned into `~/.gitlibs`
  (or `$GITLIBS`) when missing.
- Hermetic gate (prove no network is needed): `rig check --offline &&
  rig test --offline` — every artifact must already sit in the cache;
  setup-rig's `cache-hit: true` output is the fast-fail signal.
- Per-PR subset: `rig check -p modules/<m>` (`-p` narrows only the
  ns-load stage) and `rig test -p modules/<m> :kaocha.filter/focus '[:unit]'`.
- Must NOT: run `rig lock` in CI, commit `deps.lock` changes from CI.

## Test options

`rig test` forwards EDN literals to the runner:

```sh
rig test :kaocha.filter/focus '[:unit]'
rig test :plugins '[:kaocha.plugin/junit-xml]' \
         :kaocha/cli-options '{:junit-xml-file "target/junit.xml"}'
rig test :plugins '[:kaocha.plugin/cloverage]'
```

A hung run is killed after 30 minutes (watchdog; `--timeout <dur>`, `0`
disables).

## JVMs and GraalVM

- `:rig/jvm` in the root `deps.edn` pins the JVM major; Rig installs and
  manages that Temurin in its state dir (takes precedence over system
  `java`; auto-installed on first use when online).
- `rig jvm install <major>` / `update` / `list` / `path <major>` (prints
  `JAVA_HOME` for scripts/CI) / `uninstall`.
- Native builds: `rig graalvm install <major>` / `list` / `path` /
  `update` — `rig build --native` never downloads; it fails with a hint
  until the GraalVM is installed.

## Scaffold layout

```
demo/
├── deps.edn              # workspace root: :rig/modules, :rig/jvm
├── deps.lock             # committed; sha256-pins every artifact
├── dev/
│   ├── deps.edn          # dev module: app by :local/root + reloaded.repl
│   └── user.clj          # scratch ns for the dev REPL
└── modules/demo/
    ├── deps.edn          # module manifest: :rig/lib, :rig/main, :test alias
    ├── src/…
    └── test/…
```

The `dev` module is for the REPL only: not built, published, or tested. New
modules don't enter it automatically — add
`<coord> {:local/root "../modules/<m>"}` to `dev/deps.edn` + `rig lock`
when you want them in the REPL.

## Working with the nREPL session

See SKILL.md "Fixing and debugging" for the full recipe (session start,
keep-alive, reload, focused test, gate).

- Stop the server by killing the exact java process (e.g.
  `pkill -f "nrepl.cmdline --port <n>"`) — never `kill $(lsof -ti:<port>)`,
  which also signals whatever client (e.g. your own agent harness) holds an
  open connection to that port.
- A project that declares its own `nrepl/nrepl` keeps it — Rig's shipped
  jar is appended last.
