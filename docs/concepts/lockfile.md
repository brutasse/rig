# The lockfile

`deps.lock` is the complete, hashed build plan for the workspace. It lives
at the workspace root (next to the manifest for single-module projects),
it is JSON, and it is **always committed**.

You never edit it by hand. rig writes it; you read it to understand what a
build will do, and you diff it in code review.

## Anatomy

A real lock (trimmed) from a multi-module workspace:

```jsonc
{
  "version": 1,                          // schema version
  "tool":    { "name": "rig", "version": "0.1.0" },
  "resolver": {                          // who produced this lock
    "lib": "io.github.brutasse/rig-resolver",
    "version": "v0.1.0",
    "git/sha": "546d868c4d1c…"
  },
  "locked_at": "2026-09-18T10:00:00Z",
  "workspace": {
    "modules": [".", "modules/schemas", "modules/orchestrator"],
    "manifest_sha256": "86cbcbaf…"       // root deps.edn bytes at lock time
  },
  "cooldown": { "default": "48h", "repos": {} },

  // Every artifact in the resolved tree, deduplicated. Classpaths
  // reference ONLY these.
  "artifacts": [
    {
      "id": "org.clojure/core.cache:1.0.207:jar",
      "kind": "mvn",
      "group": "org.clojure",
      "name": "core.cache",
      "version": "1.0.207",
      "extension": "jar",
      "classifier": null,
      "repository": "central",
      "url": "https://repo1.maven.org/maven2/org/clojure/core.cache/1.0.207/core.cache-1.0.207.jar",
      "sha256": "8f44eda53336883b461fb9c87e48b1743a4d826b7d3afb04c88f7ad997565eca",
      "published_at": null,
      "git": null
    },
    {
      // Git deps are pinned by commit sha; no file hash applies. The
      // :deps/root project is expanded into classpaths by its paths.
      "id": "com.example/widgets:d41dcc4:jar",
      "kind": "git",
      "group": "com.example",
      "name": "widgets",
      "version": "d41dcc4",
      "extension": "jar",
      "classifier": null,
      "published_at": null,
      "git": {
        "url": "git@github.com:example/widgets.git",
        "sha": "0123456789abcdef0123456789abcdef01234567"
      },
      "deps/root": "modules/schemas",
      "paths": ["modules/schemas/src", "modules/schemas/resources"]
    }
  ],

  // Version candidates seen but not selected, with why. Empty when
  // nothing was skipped.
  "skipped": [],

  // Per-module resolved plans. Keys are module dirs (root = ".").
  "modules": {
    "modules/schemas": {
      "manifest_sha256": "4c1d…",        // this module's deps.edn bytes
      "lib": "com.example/schemas",
      "version": "1.0.0-SNAPSHOT",
      "main": null,
      "jvm-opts": [],
      "paths": ["src", "resources"],
      "classpath": [
        "org.clojure/clojure:1.12.5:jar",
        "org.clojure/core.specs.alpha:0.4.74:jar",
        "org.clojure/spec.alpha:0.5.238:jar"
      ],
      "aliases": {
        "test": {
          "classpath": [ /* base classpath + the alias's extra-deps */ ],
          "jvm-opts": [],
          "env": {},
          "exec": { "type": "exec-fn", "fn": "kaocha.runner/exec-fn" }
        }
      },
      "build":  { /* src-dirs, class-dir, jar/uberjar settings */ },
      "publish":{ "enabled": false, "repo": "", "sign-releases?": false },
      "test":   { "enabled": true }
    }
  }
}
```

### The important fields

- **`artifacts[].sha256`** — computed by the rig binary over the exact bytes
  it pins. Only the binary ever writes a hash into the lock.
- **`modules[].classpath`** — an ordered list. Every string entry resolves
  to an `artifacts` id; a `{"local": …}` entry expands to that module's
  source/resource directories. rig rejects a lock that violates this.
- **`modules[].aliases`** — the resolved form of your `:aliases`: classpath,
  jvm-opts, env, and `exec` (how to launch it: an `exec-fn`, a main, or
  plain). `rig test` reads this; it does not parse the manifest.
- **`manifest_sha256`** — sha256 of the raw `deps.edn` bytes of each module
  (and the root) at lock time. This is how staleness is detected: a
  cheap file hash, no EDN parsing.
- **`skipped`** — the audit trail of version-selection decisions: what was
  refused by a cooldown, what was forced past one, what was pinned
  explicitly.

## Staleness and the hot path

Every command that builds a classpath (`test`, `run`, `repl`, `exec`,
`build`, …) starts by checking the lock:

1. **No lock** → exit 3, hint: `run 'rig lock'`.
2. **A manifest changed** (its `manifest_sha256` no longer matches) →
   - default (development): rig re-resolves, refreshes the lock, prints one
     line — `relocked (stale: modules/orchestrator)` — and continues.
   - with `--frozen`: exit 3, lock untouched. This is the CI mode.
3. **Lock is current** → proceed. No resolution, no network (unless an
   artifact still has to be secured from the local sources or downloaded),
   straight to the JVM.

`rig check` follows the same staleness detection but never re-locks; it
reports. `rig lock` is always an explicit, user-initiated re-resolve.

## What a lock diff means in review

A `deps.lock` change is a build change. Review it the way you review a
lockfile in any other ecosystem:

- New/removed `artifacts` entries → a dependency was added, removed, or
  bumped; the version jumps should match the `deps.edn` diff.
- Changed `sha256` for the same version → the artifact's bytes changed.
  That is either an upstream re-publish or something wrong; treat it as a
  security review item.
- `skipped` entries appearing → a fresh release was refused by cooldown (or
  forced). The reason is recorded.
