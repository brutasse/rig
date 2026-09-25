# How rig works

The other pages of this site describe *what* rig does. This section
describes *how*: the two components, the boundary between them, and where
each guarantee lives in the code.

## Two components, one rule

rig is a Go binary driving a pinned Clojure kernel:

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
- `RIG_PROXY_REPOS` — for the same repositories, the base URL of rig's
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
| `fmt` | its jar, run directly: a pinned `cljfmt` bundled inside it |

**Cold commands run a kernel process**:

| Command | Kernel op |
|---|---|
| `lock` (and the automatic re-lock) | `resolve` |
| `add` / `remove` / `update` | `edit-dep`, then `resolve` |
| `check` | `check` — read-only consistency report |
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
`resolver` block, so a lock always says who resolved it.

The kernel jar is built reproducibly — identical sources produce a
byte-identical jar (zip entry timestamps are normalized) — which is what
makes a stable jar sha256 pin possible.

## Code map

### Go (`rig/internal/`)

| Package | Job |
|---|---|
| `cli` | command implementations and hot/cold orchestration |
| `kernel` | the kernel pin; one-shot invocation |
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
| `s3p` | `s3p://` repository writes (SigV4) for publish |
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
| `rig.resolver.build` | jar/uberjar via `tools.build` from the locked classpath |
| `rig.resolver.publish` | POM + deploy target (plan-only) |
| `rig.resolver.migrate` | legacy workspace → `:rig/*`, in place |
| `rig.resolver.git-sync` | thread-safe wrapper around the `tools.gitlibs` cache |
| `rig.runner` | hot-path test launcher, run on the project's locked classpath |
| `rig.fmt` | the cljfmt entry point bundled in the kernel jar |

## Where the guarantees live

- **Ecosystem-consistent version resolution** — the kernel delegates
  graph resolution to `clojure.tools.deps` and adds only a selection
  layer for floating requirements: [version resolution](resolution.md).
- **A build closed to the lockfile** — no EDN on the hot path, a
  validated lock, Go-computed hashes, gated sources:
  [the closed build](closed-build.md).
