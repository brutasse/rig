# The lockfile

`deps.lock` is the complete, hashed build plan for the workspace. It lives
at the workspace root, or next to the manifest for single-module projects.
The file is JSON. **Always commit it**.

You never edit it by hand. Rig writes the file. You read it to understand
what a build will do. You diff it in code review.

## Anatomy

A real lockfile from a multi-module workspace, trimmed:

```jsonc
{
  "version": 2,                          // schema version
  "tool":    { "name": "rig", "version": "0.1.0" },
  "resolver": {                          // who produced this lock: the pin,
                                         // plus the sha256 of the kernel jar
                                         // that ran — stamped by rig, not by
                                         // the kernel
    "lib": "io.github.brutasse/rig-resolver",
    "version": "v0.1.0",
    "git/sha": "546d868c4d1c…",
    "sha256": "9b0e1f2c…"
  },
  "locked_at": "2026-09-18T10:00:00Z",
  "workspace": {
    "modules": [".", "modules/schemas", "modules/orchestrator"],
    "manifest_sha256": "86cbcbaf…"       // root deps.edn bytes at lock time
  },
  "cooldown": { "default": "48h", "repos": {} },

  // The pinned JVM (from :rig/jvm in the root manifest): the major
  // (feature) version the project runs on. Absent when there is no pin.
  "jvm":    { "vendor": "temurin", "requested": "21" },

  // JVM flags for the build/validate JVMs (from :rig/compile-jvm-opts in
  // the root manifest). Absent when undeclared.
  "compile-jvm-opts": ["--enable-preview"],

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
      "launch-opts": [],
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
      "build":  { /* artifact-dirs, class-dir, jar/uberjar settings */ },
      "publish":{ "enabled": false, "repo": "", "sign-releases?": false },
      "test":   { "enabled": true }
    }
  }
}
```

### The important fields

- **`artifacts[].sha256`** — the hash the Rig binary computes over the
  exact bytes it pins. Only the Rig binary writes a hash into the lockfile.
- **`modules[].classpath`** — an ordered list. Every string entry resolves
  to an `artifacts` id. A `{"local": …}` entry expands to the source and
  resource directories of that module. Rig rejects a lockfile that breaks
  this rule.
- **`modules[].aliases`** — the resolved form of your `:aliases`: classpath,
  jvm-opts, env, and `exec`. The `exec` key is present only for
  `:exec-fn` aliases and records the function; `rig test` launches
  through it. `rig test` never parses the manifest.
- **`manifest_sha256`** — the sha256 hash of the raw `deps.edn` bytes of
  each module (and of the root) at lock time. Rig detects staleness with
  this cheap file hash. It does not parse the EDN.
- **`skipped`** — the audit trail of the version-selection decisions: which
  versions a cooldown refused, which versions you forced past a cooldown,
  and which versions you pinned explicitly.

## Staleness and the hot path

Every command that builds a classpath (`test`, `run`, `repl`, `exec`,
`build`, …) first checks the lockfile:

1. **No lockfile** → Rig exits with code 3 and hints: `run 'rig lock'`.
2. **Lockfile with an unsupported schema version** (an older Rig wrote it) →
   Rig exits with an "unsupported lock version" error. Re-lock: `rig
   lock`, `rig add`, `rig remove` and `rig update` read the old lockfile
   without schema-checking it, keep its pins, and write the current
   schema.
3. **A manifest changed** (its `manifest_sha256` no longer matches):
   - Default (development): Rig re-resolves, refreshes the lockfile, prints
     one line — `relocked (stale: modules/orchestrator)` — and continues.
   - With `--frozen`: Rig exits with code 3. Rig does not touch the
     lockfile. This is the CI mode.
4. **Lockfile is current** → Rig proceeds. No resolution, no network,
   unless Rig must first secure an artifact from the local sources or
   download it. Rig goes straight to the JVM.

`rig check` uses the same staleness detection, but it never re-locks. It
only reports. `rig lock` is always an explicit re-resolve that you start.

## What a lock diff means in review

A `deps.lock` change is a build change. Review it like you review a
lockfile change in any other ecosystem:

- New or removed `artifacts` entries → someone added, removed, or bumped a
  dependency. The version jumps should match the `deps.edn` diff.
- Changed `sha256` for the same version → the bytes of the artifact
  changed. That is either an upstream re-publish or something wrong. Treat
  it as a security review item.
- `skipped` entries appear → a cooldown refused a fresh release, or someone
  forced a release past a cooldown. The lockfile records the reason.
