# Rig

Rig builds and runs Clojure projects the obvious way: one binary, one
manifest, one lockfile, one way to do each thing.

No task-alias incantations, no Makefile-only knowledge, no sync step. The
command you type is the thing that happens.

## Why Rig

tools.build's bet is that your build is a program, written in the language
you already know, and that the flexibility is worth the code. Rig takes
the opposite bet: builds are boring and standardized — the last place you
want to spend innovation tokens. You don't write a build program, maintain
one, or debug its drift; you adopt one, and it behaves the same way on
every project you touch.

## What it gives you

- **A standard, not a program.** Every operation — resolve, lock, test,
  build, publish, release — has one implementation, maintained in one
  place and upgraded under you, instead of build code to write, defend,
  and keep current per project.
- **A native CLI.** `rig test`, `rig build --uber`, `rig release` — plain
  verbs with `--help` that tells the truth.
- **A lockfile you can trust.** `deps.lock` pins every artifact in the
  resolved dependency tree by sha256. Your build is a pure function of the
  source tree and the lock; nothing is re-resolved behind your back.
- **Security at the adoption moment.** Every jar is hash-verified before it
  enters a classpath, and freshly published versions are refused by a
  cooldown window until they have had time to be noticed.
- **Drift that fails loudly.** If a module's requirement, the shared
  requirement, and the lock disagree, `rig check` says so — instead of the
  divergence living silently in three hand-maintained files.
- **One config namespace.** All tool config lives under `:rig/*` in the
  `deps.edn` files you already have. Standard tools.deps keys, aliases,
  editors, and CI keep working as-is.

## Thirty seconds

```sh
rig new com.example/demo   # scaffold a project
cd demo
rig lock                   # resolve, hash, and write deps.lock
rig test                   # run the tests
```

That is the whole loop. `rig info` tells you where you stand;
[`rig --help`](reference/commands.md) lists everything else.

## Where to look next

- [Getting started](getting-started.md) — install Rig, walk through a first project.
- [Concepts](concepts/index.md) — the manifest/lockfile model, the security model, workspaces.
- [Workflows](workflows/daily.md) — day-to-day development, dependency management,
  build/publish/release, and CI.
- [Reference](reference/commands.md) — every command and every `:rig/*` config key.
- [Migration](migration/index.md) — coming from tools.project, from deps-modules, or
  from a plain tools.deps project.
- [Internals](internals/index.md) — how the pieces work: ecosystem-consistent
  version resolution, and the closed build that keeps deps to the lockfile.
