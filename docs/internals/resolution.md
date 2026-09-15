# Version resolution

How rig picks versions, and why the answer is the ecosystem's answer —
plus exactly where rig adds its own layer on top.

## The graph is tools.deps'

rig does not re-implement Maven. For every module and alias, the kernel
calls `clojure.tools.deps/create-basis` on the module's directory — the
same function the `clojure` CLI uses. POM fetching, transitive
dependencies, nearest-wins, exclusions, and repository precedence are all
tools.deps semantics, unmodified.

What rig owns is a single narrow question: *which exact version does a
floating requirement (`RELEASE`/`LATEST`) mean today?* That is the one
place where the ecosystem's own answer changes under your feet — and the
one rig wants to gate, record, and keep stable across re-locks.

## The flow of `rig lock`

1. **Read the manifests** — each `deps.edn` is hashed (raw bytes, for
   staleness) and parsed; legacy keys refuse the workspace.
2. **Select versions** — per module, `select-versions` picks the
   concrete version for each floating requirement (below).
3. **Substitute in memory** — `apply-versions` rewrites the selected
   coordinates in the *in-memory* manifest map only. The `deps.edn` on
   disk is never touched by resolution.
4. **Resolve the graph** — `create-basis` per module (base classpath,
   then each alias), now that every version is concrete.
5. **Classify the classpath** — `plan/map-classpath` maps every
   classpath root to a lock entry: a `~/.m2` artifact path → an artifact
   id, a `~/.gitlibs` checkout → a git dep, a workspace module → a local
   reference. Anything else is an error: a classpath root that is not an
   artifact, a git dep, or a declared module cannot enter the lock.
6. **Write the lock** — the kernel returns the plan *without hashes*;
   the Go binary fetches and hashes every artifact, then writes
   `deps.lock` ([the closed build](closed-build.md)).

## Version selection (`select-versions`)

Per floating coordinate, in order of precedence:

1. **Requirement override** — an explicit version passed to the command
   (e.g. `rig update org.clojure/core <v>`). Always wins; recorded in
   the lock as `explicit`.
2. **Existing lock pin** — by default (`respect-existing-pins`), a
   coordinate already pinned in `deps.lock` keeps that version. This is
   what makes re-locks stable: `rig lock` never bumps a version you
   already use, and `rig update` of one coordinate keeps the rest.
3. **Native resolution** — when the workspace's cooldowns are all zero,
   the floating requirement is left to `tools.deps` itself: rig adds
   nothing, and the result is the ecosystem's own answer, untouched.
4. **Cooldown-gated selection** — otherwise rig looks at the
   repository's version candidates and picks the newest one old enough
   (below).

### Candidates and ordering: the ecosystem's data, the ecosystem's comparator

- **Candidates** come from the standard `maven-metadata.xml` each
  repository serves for the coordinate — the same file Maven and
  tools.deps read. `LATEST` uses the metadata's `<latest>` element
  (falling back to all listed versions when a repository publishes
  none); `RELEASE` uses all listed versions.
- **Ordering** uses `DefaultArtifactVersion`, the Maven version
  comparator — the same ordering the rest of the ecosystem sees, so
  "newest" means what it means everywhere else.
- **Publication time**, for the cooldown, comes from the per-repository
  `maven-metadata-<repo-id>.xml` (a `<updated>` timestamp per version)
  that Maven repositories publish, falling back to the metadata's
  `lastUpdated`.

A candidate published less than the per-repo cooldown before resolve
time is refused, and the next-newest eligible version wins. Every
refused version is recorded in the lock's `skipped` list with its reason
(`cooldown`, `forced`, `explicit`); the user sees one line per decision.
A coordinate with *no* eligible candidate is refused outright — `rig
lock` exits 5, and the retry is `--force` (recorded as `forced`). The
mechanics and the escape hatches are on the
[security model](../concepts/security.md) page.

## Repository semantics

- The default repositories are **central** and **clojars** — the same
  pair tools.deps starts from.
- A `:mvn/repos` entry whose id matches a default **replaces** the
  built-in — the same rule tools.deps applies — so each id appears once.
- **Which repository serves a locked artifact** is decided at lock time
  and pinned into the lock (`repository` + `url`): first the repository
  Maven itself recorded in `~/.m2/…/_remote.repositories` (offline and
  exact), then a HEAD probe of each declared repository, then the first
  one. From then on, the hot build fetches from the recorded URL and
  never re-probes ([the closed build](closed-build.md)).
- Credentials for private repositories come from `~/.m2/settings.xml`,
  the same file Maven and tools.deps use; `:auth :oidc` repositories are
  fronted by rig's local auth proxy instead.

## Ecosystem interop

- **Each module stays independently resolvable.** `clojure -Sdeps`,
  editors, and any other tooling read the `deps.edn` exactly as before —
  rig's pins live in the lock, not in the manifest. Resolution never
  writes the manifest on disk; the only writers are the explicit
  `add`/`remove`/`update` commands (`edit-dep` op, format-preserving via
  `rewrite-clj` + cljfmt) and `migrate`.
- **Git dependencies** use the `tools.deps`/`tools.gitlibs` convention
  (`~/.gitlibs/libs`, `group/name:<short-sha>:jar` coordinates), so
  checkouts are shared with the `clojure` CLI and never duplicated.
- **`~/.m2/repository` is reused** as an artifact source — imported into
  the rig cache only when its bytes check out.
- **`rig outdated`** compares the lock's pins against the same
  `maven-metadata.xml`, and reports `latest`, `latest-satisfying` (same
  major), and whether the jump is breaking — the same candidate pool
  that selection uses.

## The one deliberate deviation

Cooldowns gate *selection only*. They change which version rig picks for
a floating requirement — never the graph itself, never a concrete
requirement, never the order tools.deps computes. The decision is
additive and recorded: `skipped` in the lock, `refused`/`forced` on
stderr. Set every cooldown to `"0s"` and rig's selection layer becomes
the identity: what you get is exactly what `clojure` would give you.
