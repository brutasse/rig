# Day-to-day workflow

The commands you reach for every day, and the flags that matter.

## Testing

`rig test` runs the `:test` alias of the target module — the `exec-fn`
recorded in the lock. Without `-p`, every module that declares a test
exec-fn runs, in dependency order.

```sh
rig test                              # all testable modules
rig test -p modules/orchestrator      # one module
```

Extra arguments are **EDN literals** forwarded to the test runner as
key/value opts:

```sh
rig test :kaocha.filter/focus '[:unit]'
```

Keywords, strings, numbers, booleans, vectors, and maps are supported —
so any option your runner accepts works, exactly as it would with
`clj -X:test`. The child process's exit code is rig's exit code: a failing
test suite fails your shell.

A module participates when its `:test` alias declares an `:exec-fn`;
modules without one are skipped.

## Running code

```sh
rig run -p modules/orchestrator -- --env dev
```

`rig run` launches the module's `:rig/main` on its locked classpath.
Program arguments are plain strings; to keep a program's flags from being
read as rig flags, separate them with `--`:

```sh
rig run -p modules/orchestrator                    # no program args
rig run -p modules/orchestrator -- --env dev       # flags for the program
```

- `--alias <name>` runs under a different alias's classpath (e.g. a `:dev`
  alias with reloaded REPL tooling).
- The JVM options from the module and the alias are applied.
- The working directory is the module's directory.

```sh
rig repl                      # clojure.main REPL on the module classpath
rig repl --alias dev -p modules/orchestrator
```

## The escape hatch: `rig exec`

For anything rig does not have a verb for — a script, a linter, a database
client — `rig exec` runs a command with the project's locked classpath
exported:

```sh
rig exec my-tool --some-arg
rig exec -p modules/orchestrator --alias test python manage.py shell
```

The command inherits:

- `CLASSPATH` — the module's locked classpath (alias included);
- `JAVA_OPTS` — the module/alias JVM options, if any;
- the module directory as working directory.

Your command's exit code is rig's exit code.

## Code hygiene

```sh
rig lint                    # clj-kondo (from PATH), project config
rig lint -p modules/app
rig fmt                     # cljfmt (the version pinned in the kernel)
rig fmt --check             # CI: report without fixing
rig clean                   # remove target dirs of all target modules
rig clean -p modules/app
```

`lint` needs `clj-kondo` on your PATH and works without a lock — it lints
source. `fmt` uses the cljfmt pinned inside rig's kernel jar, so everyone
formats with the same version; it also works without a lock.

## What every hot command does first

`test`, `run`, `repl`, `exec`, and `build` all apply the same lock
discipline before launching anything:

1. no lock → `no lock at deps.lock (run 'rig lock')`, exit 3;
2. manifest changed → in development rig re-locks silently and prints
   `relocked (stale: <modules>)`; with `--frozen` it fails with exit 3
   instead;
3. every classpath artifact is hash-checked against the lock (secured from
   the cache or a checksum-checked `~/.m2`; missing artifacts are
   downloaded unless `--offline`).

So `rig test` is safe to run from a dirty checkout: you get either the
locked behavior or an explicit, visible re-lock — never a quiet
re-resolution.

## Module targeting

`-p, --path <module>` targets one module by its directory
(`-p modules/orchestrator`) or by a path to it. Omitted (or `-p .`) means
the root module; for multi-module commands it means *all* modules in
dependency order. `-p` is accepted by every command that takes a module.
