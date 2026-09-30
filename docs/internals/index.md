# How Rig works

The other pages of this site describe *what* Rig does. This section
describes *how*: the two components, the boundary between them, and where
each guarantee lives in the code.

## Two components, one rule

Rig is a Go binary driving a pinned Clojure kernel:

```
┌─────────────────────────────┐         ┌──────────────────────────────┐
│  rig (Go binary)            │  JSON   │  resolver kernel (Clojure)   │
│  CLI · lockfile · cache ·   │◄───────►│  pinned uberjar, one-shot    │
│  hashing · classpath ·      │ stdin/  │  EDN · tools.deps ·          │
│  JVM launch · orchestration │ stdout  │  tools.build                 │
└─────────────────────────────┘         └──────────────────────────────┘
               │ os/exec
               ▼
        java (project JVM, classpath from the lock)
```

- **The Go binary** owns everything that touches bytes: the lockfile
  (JSON), the content-addressed artifact cache, fetching and
  sha256-hashing every artifact, classpath assembly, and JVM launch. It
  never parses `deps.edn` for resolution — it reads manifests only to
  discover `:auth :oidc` repositories and module paths, plus small
  literal CLI arguments.
- **The kernel** is the EDN authority and the resolution engine: it
  parses manifests, edits them (format-preserving), resolves dependency
  graphs with `clojure.tools.deps`, and builds jars with
  `clojure.tools.build`. It is a one-shot process: one JSON request in,
  one JSON document out.

The rule that separates the two: **the kernel resolves, Go pins.** The
kernel's `resolve` response contains no `sha256` at all — Go fetches (or
imports) every artifact, hashes the exact bytes it stores, and writes the
finished lock. The kernel never writes a hash, and never the lock
([the closed build](closed-build.md)).

## The kernel protocol

Go launches the pinned kernel jar with the request JSON on stdin and
reads one JSON document — the last line of stdout (ops that spawn
subprocesses may print noise before it). Diagnostics go to stderr:

```json
// request
{"op": "resolve", "workspace": "/path/to/ws", "lock": "deps.lock",
 "args": {"offline": false, "force": false}}
```

| Kernel exit code | Meaning |
|---|---|
| 0 | ok — response JSON on stdout |
| 1 | the op failed (stack trace on stderr) |
| 2 | bad request (unknown op, malformed JSON, missing request) |
| 3 | a manifest still uses legacy keys — `run rig migrate` |

Exit 3 is the **legacy guard**: every op that reads a manifest refuses a
workspace that still carries pre-migration keys
(`:exoscale.project/*`, `:exoscale.deps/*`,
`:slipset.deps-deploy/*`, `antq.core/*`). The kernel has no legacy
fallback, so an un-migrated workspace cannot resolve, build, or publish;
`migrate` is the one exempt op.

Go also sets a small environment for the kernel JVM:

- `CLOJURE_CLI_ALLOW_HTTP_REPO=1` — so `:mvn/repos` may be plain
  `http://` (local dev servers, tests);
- `RIG_REPO_TOKENS` — an EDN `{repo-id bearer}` map for `:auth :oidc`
  repositories, used by the kernel's own metadata probes;
- `RIG_PROXY_REPOS` — for the same repositories, the base URL of Rig's
  local loopback auth proxy. `tools.deps` cannot be given a bearer token
  of its own, so resolution traffic for those repos is rewritten to the
  proxy, which forwards to the real URL with the bearer attached. Repo
  ids are kept, so lock attribution is unchanged.

## Hot vs cold paths

**Hot commands read only the lock** — no resolution, and no network when
the cache is warm:

| Command | What the kernel contributes |
|---|---|
| `run`, `repl`, `exec`, `verify` | nothing — pure Go |
| `clean`, `lint` | nothing — no lock needed at all |
| `test` | its jar, as the *last* classpath entry: it carries `rig.runner`, the exec-fn launcher that runs the module's test alias. The project's own Clojure comes first and wins on conflicts |
| `check` (stage 2) | the kernel computes the AOT plan (`aot-plan` op), then its jar as the *last* classpath entry: the runner's aot mode preloads the plan's requires and AOT-compiles the module's namespaces into a fresh class dir that leads the locked base classpath. Stage 1 (lock vs manifests) is the cold kernel op |
| `fmt` | its jar, run directly: a pinned `cljfmt` bundled inside it |

**Cold commands run a kernel process**:

| Command | Kernel op |
|---|---|
| `lock` (and the automatic re-lock) | `resolve` |
| `add` / `remove` / `update` | `edit-dep`, then `resolve` |
| `check` | `check` — read-only consistency report (stage 1); stage 2 also takes the `aot-plan` op |
| `tree` | `tree` |
| `outdated` | `outdated` |
| `build` | `build` — `tools.build` inside the kernel JVM, on the locked classpath |
| `publish` / `install` | `publish` — POM + deploy target only; Go does the transfer |
| `release` | `publish`, orchestrated by Go over git |
| `migrate` | `migrate` |

A hot command that needs the network needs it only to fetch bytes the
lock already pins ([the closed build](closed-build.md)).

## The kernel is pinned too

The kernel jar is itself a supply-chain item. Its pin — library
coordinate, version, git sha, download URL, and the jar's own sha256 — is
stamped into the Go binary at release time (`make pin`). Before any use,
Go verifies the jar's sha256, whether it is freshly downloaded or already
in the cache (`~/.local/share/rig/kernel/<git-sha>/`); a mismatch is a
hard error. The lock records the kernel that produced it in its
`resolver` block — the pin plus the sha256 of the jar that actually ran,
stamped by Go at lock time, not by the kernel — so a lock always says who
resolved it.

The jar is not byte-reproducible by default, and nothing in the flow
needs it to be: the pin's sha256 is stamped from the artifact as built
(`make pin`), and every other hash in the system — the lock's,
`SHA256SUMS`, the Docker build args — the same way. A workspace that
wants byte-reproducible builds opts in with `:rig/timestamp-string`;
with it set, every entry of the built jar and uberjar carries the
pinned timestamp and is written in name-sorted order, so the same
sources, lock and build JVM (`:rig/jvm`) produce a byte-identical jar,
and the pin's sha256 becomes a stable fingerprint of sources, lock and
build config (zip timestamps have a 2-second resolution — the format's
limit). The runner jar (rig.runner's class files only) is pinned the
same way and is an install-time artifact: the Docker image pre-seeds it
into the store, local runs point at it with `RIG_RUNNER_JAR`, and Rig
never extracts or writes it at runtime — hot commands run on stores that
may be read-only.

## Code map

### Go (`rig/internal/`)

| Package | Job |
|---|---|
| `cli` | command implementations and hot/cold orchestration |
| `kernel` | the kernel and runner pins; one-shot invocation; fetch + verify into the store |
| `lockfile` | the lock JSON model, validation, staleness detection |
| `fetch` | artifact retrieval: cache → m2 → download, hash-verified |
| `cache` | the content-addressed store (`artifacts/<sha256>`) |
| `classpath` | lock → absolute JVM classpath |
| `digest` | file sha256 |
| `workspace` | workspace discovery (root, modules, lock path) |
| `jvm` | `java` discovery and launch |
| `jdk` | managed Temurin JDKs (Adoptium API, sha-verified) |
| `maven` | `~/.m2/settings.xml`: credentials, local repository |
| `oidc` | OIDC tokens for `:auth :oidc` repositories |
| `proxy` | the loopback auth proxy for kernel traffic |
| `ednlit` | the small EDN reader for CLI arguments and workspace manifests |
| `updater` | release checks and `rig self-update` |

### Clojure kernel (`resolver/src/`)

| Namespace | Job |
|---|---|
| `rig.resolver.main` | entry point: op dispatch, exit codes, legacy guard |
| `rig.resolver.manifest` | manifest read (byte sha256, legacy keys, `:rig/*` accessors, module version) |
| `rig.resolver.resolve` | the `resolve` op: selection → `create-basis` → lock document |
| `rig.resolver.versions` | version candidates, cooldown selection, repo attribution, auth proxy |
| `rig.resolver.plan` | classpath roots → lock entries (mvn / git / local classification) |
| `rig.resolver.check` | the consistency report |
| `rig.resolver.edit` | format-preserving manifest edits (`rewrite-clj` + cljfmt) |
| `rig.resolver.tree` | dependency tree display |
| `rig.resolver.outdated` | what's newer upstream |
| `rig.resolver.aot` | the AOT plan: the non-project require closure (preloads) and the module's namespaces in dependency order — drives the two-phase AOT compile of `build` and `check` (stage 2) |
| `rig.resolver.build` | jar/uberjar via `tools.build` from the locked classpath, with the two-phase AOT compile |
| `rig.resolver.publish` | POM + deploy target (plan-only) |
| `rig.resolver.migrate` | legacy workspace → `:rig/*`, in place |
| `rig.resolver.git-sync` | thread-safe wrapper around the `tools.gitlibs` cache |
| `rig.runner` | hot-path test/check launcher; its jar (an install-time artifact, shipped next to the kernel jar) is appended to the project's locked classpath, never the kernel jar itself. Requires Clojure ≥ 1.8.0 on the module's locked classpath: the gen-class wrapper's static initializer calls `clojure.lang.Util/loadWithClass(String, Class)` and modern-compiled namespace bodies reference `clojure.lang.Tuple`, both absent from Clojure 1.7.x — check/test report `clojure-floor` below it |
| `rig.fmt` | the cljfmt entry point bundled in the kernel jar |

## Where the guarantees live

- **Ecosystem-consistent version resolution** — the kernel delegates
  graph resolution to `clojure.tools.deps`, adding a selection layer
  for floating requirements and a local-beats-published coordinate
  comparison: [version resolution](resolution.md).
- **A build closed to the lockfile** — no EDN on the hot path, a
  validated lock, Go-computed hashes, gated sources:
  [the closed build](closed-build.md).
- **No source next to its class** — every jar the build produces drops
  the bundled `.clj`/`.cljc` that sit next to their AOT
  `__init.class`: `RT.load` loads the source instead of the class
  whenever the source is not strictly older, and sources exploded from
  dependency jars carry no such ordering (equal timestamps would make
  the runtime recompile the namespace from the bundled source — two
  class identities). A source with no base `__init` is the namespace's
  only payload and is kept, as is a source whose only `__init` lives
  under `META-INF/versions/` (the floor's fallback). A dependency that
  ships a source *strictly newer* than its class is refused at build
  time: in that jar the source is what the runtime loads, and the
  build refuses to guess which of the two is the payload.
