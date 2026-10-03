# The closed build

The claim from the [security model](../concepts/security.md) — *an
artifact that is not in the lock cannot enter a classpath* — is worth
more than words. This page shows the code that enforces each part of it.

## No manifest on the hot path

Every command that builds a classpath (`test`, `run`, `repl`, `exec`,
`build`, …) goes through the same two Go steps:

1. **Staleness check** — `lockfile.Stale` re-hashes the `deps.edn` of
   each module *as raw bytes* and compares it to the `manifest_sha256`
   of the lock. No EDN parsing: the Go binary has no EDN reader in the
   hot path at all (the one bounded literal reader is for CLI
   arguments). A changed manifest is a changed plan — re-lock and
   continue in development, exit 3 under `--frozen`.
2. **Classpath assembly** — `classpath.Build` reads
   `modules[].classpath` (or the classpath of the alias) from the lock
   and expands each entry. It expands an artifact id against the
   `artifacts` list of the lock, and a `{"local": …}` reference against
   the `modules` of the lock. An entry that resolves to nothing is an
   error. The classpath order is exactly the order of the lock.

The manifest can add nothing: the build never reads it.

## The lock is validated, not trusted

`lockfile.Validate` runs before anything uses the lock:

- the schema version must match.
- every `mvn` artifact carries a well-formed `sha256`, and every `git`
  dep a commit sha.
- every classpath entry of every module and alias must reference an
  artifact id or a module that exists in the lock.
- a pinned JVM version must satisfy the requested one.

A hand-edited or corrupted lock fails validation and the command exits
before touching the network.

## Only the Go binary writes hashes

The `resolve` response of the kernel has no hashes. After it returns,
`completeLock` in the Go binary:

1. fetches **every** `mvn` artifact through `fetch.GetNew`, in parallel,
   and computes the sha256 over the exact bytes.
2. stamps the shas into the lock.
3. runs `Validate` before it writes `deps.lock`.

From then on, the `sha256` in the lock is a fact about bytes the Go
binary held — not an assertion from a repository, a POM, or a
pre-existing `~/.m2`.

## Where bytes come from — and the gate at each source

For a pinned artifact, `fetch.Get` tries sources in order; each source
has its own gate:

| Source | Gate |
|---|---|
| Rig cache (`artifacts/<sha256>`) | the entry is re-hashed before use — a content-addressed file that does not hash to its own name is a mismatch, not a cache hit |
| `~/.m2/repository` copy | accepted **only** when its bytes hash to the locked sha. A poisoned m2 copy is skipped, never used |
| Download from the lock's `url` | the body is hashed as it is written to the cache; a difference from the locked sha aborts with exit 4 — no self-heal, no retry with a different sha |

At first lock, the sha does not exist yet, so the gates differ: nothing
exists to hash against. Rig tries the url→sha recorded by the cache
(re-verified), then the m2 copy only when **self-consistent**, then a
fresh download. Self-consistent means its content matches the Maven
`.sha1` sidecar stored beside it. Whatever source wins, Go computes the
sha256 over the bytes it keeps.

**Rig pins the URL too.** Go fixes `artifacts[].url` at lock time
([repo attribution](resolution.md#repository-semantics)), so a hot build
only ever downloads from a URL that appears in a reviewed lock diff.
Changing a repository changes the lock.

## `verify`: tampering surfaces, never heals

`rig verify` uses `VerifyGet`, the security-gate variant of the fetch.
If a cache entry fails its hash, `rig verify` **reports a mismatch**. It
never re-fetches the entry: a re-fetch could replace tampered bytes with
clean ones and hide the incident. `rig verify` obtains missing entries
from m2 or the repository, and checks them the same way. That is the
difference between "the build works" and "the build proves what it ran."

## Git dependencies: commit-pinned, never fetched on the hot path

A git dep is an artifact pinned by **commit sha**, expanded from
`~/.gitlibs/libs/<group>/<name>/<sha>`. The hot path only checks that
the checkout exists and points at the `paths` of the lock. When the
checkout does not exist, the command exits with `run 'rig lock'`. Git
sync happens at resolve time, serialized behind one JVM-wide lock.
`tools.deps` expands subtrees in parallel, and the `tools.gitlibs`
check-then-act cache is not thread-safe. So the kernel wraps its entry
points (`rig.resolver.git-sync`).

## Offline, frozen, and the exit codes

| Flag / state | Effect | Exit |
|---|---|---|
| `--offline` | no network, ever: cache or m2 only | 1 when an artifact is in neither |
| `--frozen` | the lock is read-only; a stale lock fails | 3 |
| hash mismatch | abort, no self-heal | 4 |
| cooldown refused everything | `rig lock` fails; retry with `--force` | 5 |

`--offline` applies to the kernel too. `rig lock --offline` refuses
rather than probe repositories. `rig outdated --offline` is a straight
error when the lock has pins to compare, because it needs the metadata.

## Drift without re-resolving: `rig check`

`rig check` (a kernel op, read-only — no network, never writes the lock)
compares the manifests against the lock:

| Problem | Severity |
|---|---|
| `stale-lock` — a requirement the lock's pin does not satisfy | error |
| `conflict` — two modules require mutually unsatisfiable versions | error |
| `drift` — a module's requirement differs from the workspace's shared one | warn |
| `floating-version` — a manifest still says `RELEASE`/`LATEST` | error |
| `no-lib` — publish enabled without a `:rig/lib` coordinate | error |
| `unknown-repo` — the lock pins an artifact via a repo no manifest declares | error |

Rig checks version *ranges* (`[1.0,2.0)`) with the same Maven comparator
as resolution. So a range requirement and its pin agree or disagree the
way they do everywhere else in the ecosystem.
