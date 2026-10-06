# Day-to-day workflow

The commands you use every day, and the flags that matter.

## Testing

`rig test` runs the `:test` alias of the target module. The `exec-fn`
recorded in the lockfile gives the command. Without `-p`, Rig runs every
module that declares a test exec-fn, in dependency order.

```sh
rig test                              # all testable modules
rig test -p modules/orchestrator      # one module
```

Pass extra arguments as **EDN literals**. Rig forwards them to the test
runner as key/value opts:

```sh
rig test :kaocha.filter/focus '[:unit]'
```

Rig supports keywords, strings, numbers, booleans, vectors, and maps. Any
option your runner accepts works, exactly as it works with `clj -X:test`.
The exit code of the child process becomes Rig's exit code: a failing test
suite fails your shell.

A module participates when its `:test` alias declares an `:exec-fn`. Rig
skips modules without one.

## Running code

```sh
rig run -p modules/orchestrator -- --env dev
```

`rig run` launches the `:rig/main` of the module on its locked classpath.
Program arguments are plain strings. To keep program flags from acting as
Rig flags, separate them with `--`:

```sh
rig run -p modules/orchestrator                    # no program args
rig run -p modules/orchestrator -- --env dev       # flags for the program
```

- `--alias <name>` runs on the classpath of a different alias, for example
  a `:dev` alias with reloaded REPL tooling.
- Rig applies the JVM options of the module and the alias.
- The working directory is the module directory.

```sh
rig repl                      # clojure.main REPL on the module classpath
rig repl --alias dev -p modules/orchestrator
```

## The escape hatch: `rig exec`

When Rig has no verb for your task — a script, a linter, a database
client — `rig exec` runs any command with the locked classpath of the
project exported:

```sh
rig exec my-tool --some-arg
rig exec -p modules/orchestrator --alias test python manage.py shell
```

The command inherits:

- `CLASSPATH` — the locked classpath of the module (alias included);
- `JAVA_OPTS` — the JVM options of the module and the alias, if any;
- the module directory as working directory.

The exit code of your command becomes Rig's exit code.

## Code hygiene

```sh
rig lint                    # clj-kondo (from PATH), project config
rig lint -p modules/app
rig fmt                     # cljfmt (the version pinned in the kernel)
rig fmt --check             # CI: report without fixing
rig clean                   # remove target dirs of all target modules
rig clean -p modules/app
```

`lint` needs `clj-kondo` on your PATH. It works without a lockfile, because
it lints source. `fmt` uses the cljfmt pinned inside the kernel jar of Rig,
so everyone formats with the same version. It also works without a
lockfile.

## What every hot command does first

`test`, `run`, `repl`, `exec`, and `build` apply the same lockfile
discipline before they launch anything:

1. no lockfile → `no lock at /home/dev/app/deps.lock (run 'rig lock')`, exit 3;
2. manifest changed → in development, Rig re-locks silently and prints
   `relocked (stale: <modules>)`; with `--frozen`, Rig fails with exit 3
   instead;
3. Rig hash-checks every classpath artifact against the lockfile, securing
   it from the cache or a checksum-checked `~/.m2`. Rig downloads missing
   artifacts unless `--offline`.

So you can run `rig test` from a dirty checkout: you get either the locked
behavior or an explicit, visible re-lock — never a quiet re-resolution.

## Module targeting

`-p, --path <module>` targets one module by its directory
(`-p modules/orchestrator`) or by a path to it. If you omit it (or pass
`-p .`), the command targets the root module; for multi-module commands,
it targets *all* modules in dependency order. Every command that takes a
module accepts `-p`.
