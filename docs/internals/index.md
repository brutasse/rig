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
`resolve` response of the kernel contains no `sha256` at all. Go fetches
(or imports) every artifact, hashes the exact bytes it stores, and writes
the finished lock. The kernel never writes a hash, and never the lock
([the closed build](closed-build.md)).

## The kernel protocol

Go launches the pinned kernel jar with the request JSON on stdin. It
reads one JSON document: the last line of stdout. Ops that spawn
subprocesses can print noise before it. Diagnostics go to stderr:

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

- `CLOJURE_CLI_ALLOW_HTTP_REPO=1` — this lets `:mvn/repos` use plain
  `http://` URLs (local dev servers, tests).
- `RIG_REPO_TOKENS` — an EDN `{repo-id bearer}` map for `:auth :oidc`
  repositories. The kernel uses it for its own metadata probes.
- `RIG_PROXY_REPOS` — for the same repositories, the base URL of the
  local loopback auth proxy of Rig. `tools.deps` cannot receive a bearer
  token of its own. So Rig rewrites the resolution traffic for those
  repos to the proxy, and the proxy forwards to the real URL with the
  bearer attached. Rig keeps the repo ids, so the lock attribution does
  not change.

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

The kernel jar is itself a supply-chain item. The pin holds the library
coordinate, the version, the git sha, the download URL, and the sha256 of
the jar. Go stamps the pin into the Go binary at release time
(`make pin`). Before any use, Go verifies the sha256 of the jar, whether
Go downloaded it freshly or found it already in the cache
(`~/.local/share/rig/kernel/<git-sha>/`). A mismatch is a hard error. The
lock records the kernel that produced it in its `resolver` block: the pin
plus the sha256 of the jar that actually ran. Go stamps it at lock time;
the kernel does not. So a lock always says who resolved it.

The jar is not byte-reproducible by default, and nothing in the flow
needs it to be. Go stamps the sha256 of the pin from the artifact as
built (`make pin`). Go stamps every other hash in the system the same
way: the hash in the lock, the `SHA256SUMS` entries, the Docker build
args. A workspace that wants byte-reproducible builds opts in with
`:rig/timestamp-string`. With it set, every entry of the built jar and
uberjar carries the pinned timestamp, and Go writes the entries in
name-sorted order. So the same sources, lock and build JVM (`:rig/jvm`)
produce a byte-identical jar. The sha256 of the pin then becomes a stable
fingerprint of the sources, lock and build config. Zip timestamps have a
2-second resolution — the limit of the format. Rig pins the runner jar
(the rig.runner class files only) the same way, and it is an install-time
artifact. The Docker image pre-seeds it into the store, local runs point
at it with `RIG_RUNNER_JAR`, and Rig never extracts or writes it at
runtime. Hot commands run on stores that can be read-only.

## Checkpoint/restore acceleration

Restore-eligible kernel ops can run on a CRaC checkpoint of a warmed
kernel instead of a cold JVM fork (~0.3 s vs 1–3 s), and every
restored run re-dumps the image so it stays current. The whole path
lives in `internal/kernelrun`: the image store (keyed to kernel jar
bytes × JVM build × engine × CPU flags), the restore/cold selector
inside `kernel.Call`, and the bootstrap that forks and parks the
checkpoint JVM. The JVM-side contract — the exact flag set, where the
kernel parks (`rig.kernel.Bootstrap`, via reflection so the jar
builds on plain JDKs), and why success is "dump-kill + image on disk,
not exit code" — is load-bearing and documented in that package's
header. Refresh dumps are deltas referencing their parents, so a
chain is deletable only as a whole (`rig crac clean`). The managed
Zulu CRaC JDK (`rig crac install`, `jdk.CRaCPins`) is pinned exactly:
image keys track JVM builds, so channel drift would strand every
image.

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
  graph resolution to `clojure.tools.deps`. It adds a selection layer for
  floating requirements and a comparison where a local coordinate beats a
  published one: [version resolution](resolution.md).
- **A build closed to the lockfile** — no EDN on the hot path, a
  validated lock, Go-computed hashes, gated sources:
  [the closed build](closed-build.md).
- **No source next to its class** — every jar the build produces drops
  the bundled `.clj`/`.cljc` that sit next to their AOT `__init.class`.
  `RT.load` loads the source instead of the class whenever the source is
  not strictly older. Sources exploded from dependency jars carry no such
  ordering. Equal timestamps would make the runtime recompile the
  namespace from the bundled source — two class identities. A source with
  no base `__init` is the only payload of the namespace, so the build
  keeps it. The same holds for a source whose only `__init` lives under
  `META-INF/versions/` (the fallback of the floor). The build refuses a
  dependency that ships a source *strictly newer* than its class. In
  that jar the source is what the runtime loads, and the build refuses to
  guess which of the two is the payload.
