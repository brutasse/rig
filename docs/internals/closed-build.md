# The closed build

The claim from the [security model](../concepts/security.md) — *an
artifact that is not in the lock cannot enter a classpath* — is worth
more than words. This page shows where each part of it is enforced, in
code.

## No manifest on the hot path

Every command that builds a classpath (`test`, `run`, `repl`, `exec`,
`build`, …) goes through the same two Go steps:

1. **Staleness check** — `lockfile.Stale` re-hashes each module's
   `deps.edn` *as raw bytes* and compares to the lock's
   `manifest_sha256`. No EDN parsing: the Go binary has no EDN reader in
   the hot path at all (the one bounded literal reader is for CLI
   arguments). A changed manifest is a changed plan — re-lock and
   continue in development, exit 3 under `--frozen`.
2. **Classpath assembly** — `classpath.Build` reads
   `modules[].classpath` (or the alias's classpath) from the lock and
   expands each entry: an artifact id against the lock's `artifacts`
   list, a `{"local": …}` reference against the lock's `modules`. An
   entry that resolves to nothing is an error. The classpath order is
   exactly the lock's order.

The manifest can add nothing: it is never read.

## The lock is validated, not trusted

`lockfile.Validate` runs before anything uses the lock:

- the schema version must match;
- every `mvn` artifact carries a well-formed `sha256`, every `git` dep a
  commit sha;
- every classpath entry of every module and alias must reference an
  artifact id or a module that exists in the lock;
- a pinned JVM version must satisfy the requested one.

A hand-edited or corrupted lock fails validation and the command exits
before touching the network.

## Only the Go binary writes hashes

The kernel's `resolve` response has no hashes. After it returns,
`completeLock` in the Go binary:

1. resolves the exact JVM version for a `:rig/jvm` pin (Adoptium API);
2. fetches **every** `mvn` artifact through `fetch.GetNew`, in parallel —
   computing the sha256 over the exact bytes;
3. stamps the shas into the lock;
4. runs `Validate` before writing `deps.lock`.

From then on, the `sha256` in the lock is a fact about bytes the Go
binary held — not an assertion from a repository, a POM, or a pre-existing
`~/.m2`.

## Where bytes come from — and the gate at each source

For a pinned artifact, `fetch.Get` tries sources in order; each source
has its own gate:

| Source | Gate |
|---|---|
| Rig cache (`artifacts/<sha256>`) | the entry is re-hashed before use — a content-addressed file that does not hash to its own name is a mismatch, not a cache hit |
| `~/.m2/repository` copy | accepted **only** when its bytes hash to the locked sha. A poisoned m2 copy is skipped, never used |
| Download from the lock's `url` | the body is hashed as it is written to the cache; a difference from the locked sha aborts with exit 4 — no self-heal, no retry with a different sha |

At first lock (the sha is not known yet) the gates differ, because there
is nothing to hash against: the cache's recorded url→sha (re-verified),
the m2 copy only when **self-consistent** — its content matches the
Maven `.sha1` sidecar stored beside it — then a fresh download. Whatever
source wins, the sha256 is computed by Go over the bytes it keeps.

**The URL is pinned too.** `artifacts[].url` is fixed at lock time
([repo attribution](resolution.md#repository-semantics)), so a hot build
only ever downloads from a URL that appears in a reviewed lock diff.
Changing a repository changes the lock.

## `verify`: tampering surfaces, never heals

`rig verify` uses `VerifyGet`, the security-gate variant of the fetch: a
cache entry that fails its hash is **reported as a mismatch** — it is
never re-fetched, because a re-fetch could replace tampered bytes with
clean ones and hide the incident. Missing entries are obtained from m2 or
the repository and checked the same way. That is the difference between
"the build works" and "the build proves what it ran."

## Git dependencies: commit-pinned, never fetched on the hot path

A git dep is an artifact pinned by **commit sha**, expanded from
`~/.gitlibs/libs/<group>/<name>/<sha>`. The hot path only checks that
the checkout exists and points at the lock's `paths`; when it is missing,
the command exits with `run 'rig lock'`. Git sync happens at resolve
time, serialized behind one JVM-wide lock — `tools.deps` expands
subtrees in parallel and `tools.gitlibs`' check-then-act cache is not
thread-safe, so the kernel wraps its entry points
(`rig.resolver.git-sync`).

## Offline, frozen, and the exit codes

| Flag / state | Effect | Exit |
|---|---|---|
| `--offline` | no network, ever: cache or m2 only | 1 when an artifact is in neither |
| `--frozen` | the lock is read-only; a stale lock fails | 3 |
| hash mismatch | abort, no self-heal | 4 |
| cooldown refused everything | `rig lock` fails; retry with `--force` | 5 |

`--offline` applies to the kernel too: `rig lock --offline` refuses
rather than probe repositories, and `rig outdated --offline` is a
straight error when the lock has pins to compare (it needs the
metadata).

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

Version *ranges* (`[1.0,2.0)`) are satisfied with the same Maven
comparator as resolution, so a range requirement and its pin agree or
disagree the way they do everywhere else in the ecosystem.
