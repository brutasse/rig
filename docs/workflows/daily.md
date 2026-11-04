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

A module's `:test` alias can carry reporters as `:extra-deps` without
activating them. A scaffolded module ships the kaocha JUnit XML and
coverage reporters this way — dormant until a run loads them with
`:plugins`:

```sh
# JUnit XML for CI: the plugin writes to :junit-xml-file
rig test :plugins '[:kaocha.plugin/junit-xml]' \
         :kaocha/cli-options '{:junit-xml-file "target/junit.xml"}'

# coverage: prints a table and writes target/coverage/
rig test :plugins '[:kaocha.plugin/cloverage]'
```

The paths are relative to the module directory (the runner's working
directory).

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
rig nrepl                     # nREPL server on the module classpath
rig nrepl --alias dev -p modules/orchestrator
```

## The development REPL: the dev module

Scaffolded projects carry a workspace-level `dev` module: one development
module for the whole workspace, not one per module. Its `deps.edn` loads
the app module by `:local/root` and pins `reloaded.repl`
([the reloaded workflow](https://cognitect.com/blog/2013/06/04/clojure-workflow-reloaded));
`dev/user.clj` is the scratch namespace the REPL opens in.

```sh
rig nrepl -p dev
```

The server runs in that terminal; quitting the console stops it.

With the app open, work in `user.clj`. After a save, reload the changed
namespaces — CIDER's `cider-ns-refresh` needs the cider-nrepl middleware
(see Connecting the editor below); without it, re-evaluate `user` — and
`(reloaded/go)` restarts the system on the new code. When the app grows
a component system, wire the reloaded workflow to it — `user.clj`'s
docstring carries the incantation:

```clj
(require '[reloaded.repl :as reloaded])
(reloaded/set-init! #'start-system)
(reloaded/go)     ;; start, or reload changed namespaces and restart
(reloaded/stop)
(reloaded/reset)  ;; stop, unload every namespace, start again
```

A new module does not enter `dev` on its own: `rig new-module` leaves it
out, so the development classpath stays what you asked for. Add a module
to `dev/deps.edn` when you want it in the REPL —
`example/other {:local/root "../modules/other"}` — then `rig lock`.

The dev module is not built, published, or tested: it declares no
`:rig/lib`, no test exec-fn, no build. It exists for the REPL.

## Connecting the editor

nREPL writes the port of a running server to `.nrepl-port` in the module
directory and removes the file when the JVM exits. A hard kill leaves the
file behind, so add `.nrepl-port` to your `.gitignore`.

Recent CIDER suggests the running server at the connect prompt.
`cider-connect` (C-c C-x c s) offers the port from the file when your
buffer is inside the module. A machine-wide process scan finds a running
`nrepl.cmdline` from any buffer. The port file works on any platform; the
scan needs Linux or macOS. Without the scan, type the port from the
startup banner.

Rig starts the server without nREPL middleware, so CIDER's completion,
debugger, and refresh stay off. To turn them on, put `cider/cider-nrepl`
on the classpath and list the middleware in a `dev/.nrepl.edn`;
`nrepl.cmdline` reads that file from the module directory:

```edn
{:middleware [^:optional cider.nrepl/cider-middleware]}
```

Declare `cider/cider-nrepl` as an `:extra-dep` of a `:dev` alias in
`dev/deps.edn` and start the server with `rig nrepl -p dev --alias dev`.
The `^:optional` marker skips the middleware when it is missing from the
classpath.

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
rig fmt                     # cljfmt (the version pinned in the kernel)
rig fmt --check             # CI: report without fixing
rig clean                   # remove target dirs of all target modules
rig clean -p modules/app
```

`fmt` uses the cljfmt pinned inside the kernel jar of Rig, so everyone
formats with the same version. It works without a lockfile.

## What every hot command does first

`test`, `run`, `nrepl`, `exec`, and `build` apply the same lockfile
discipline before they launch anything:

1. no lockfile → `no lock at /home/dev/app/deps.lock (run 'rig lock')`, exit 3;
2. manifest changed → `lock is stale (manifest changed in: <modules>); run
   'rig lock' to refresh`, exit 3. The hot commands never write the
   lockfile; run `rig lock` (or `rig add` / `rig remove` / `rig update`)
   and re-run;
3. Rig hash-checks every classpath artifact against the lockfile, securing
   it from the cache or a checksum-checked `~/.m2`. Rig downloads missing
   artifacts unless `--offline`.

So `rig test` from a dirty checkout runs the locked classpath, or fails
with the hint. You never get a quiet re-resolution.

## Module targeting

`-p, --path <module>` targets one module by its directory
(`-p modules/orchestrator`) or by a path to it. If you omit it (or pass
`-p .`), the command targets the root module; for multi-module commands,
it targets *all* modules in dependency order. Every command that takes a
module accepts `-p`.
