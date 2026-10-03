# Version resolution

How Rig picks versions, and why the answer is the answer of the
ecosystem — plus exactly where Rig adds its own layer on top.

## The graph is tools.deps'

Rig does not re-implement Maven. For every module and alias, the kernel
calls `clojure.tools.deps/create-basis` on the directory of the module —
the same function the `clojure` CLI uses. POM fetching, transitive
dependencies, nearest-wins, exclusions, and repository precedence are
tools.deps semantics, unmodified. Rig adds one exception on top of the
Maven comparator: a local module beats a published coordinate
([local modules beat published
coordinates](#local-modules-beat-published-coordinates)).

What Rig owns is a single narrow question: *which exact version does a
floating requirement (`RELEASE`/`LATEST`) mean today?* That is the one
place where the answer of the ecosystem itself changes over time. It is
also the one Rig wants to gate, record, and keep stable across re-locks.

## The flow of `rig lock`

1. **Read the manifests** — Rig hashes each `deps.edn` as raw bytes, for
   staleness, and parses it. Legacy keys refuse the workspace.
2. **Select versions** — per module, `select-versions` picks the
   concrete version for each floating requirement (below).
3. **Substitute in memory** — `apply-versions` rewrites the selected
   coordinates in the *in-memory* manifest map only. Resolution never
   touches the `deps.edn` on disk.
4. **Resolve the graph** — `create-basis` per module (base classpath,
   then each alias), now that every version is concrete.
5. **Classify the classpath** — `plan/map-classpath` maps every
   classpath root to a lock entry. A `~/.m2` artifact path becomes an
   artifact id, a `~/.gitlibs` checkout becomes a git dep, and a
   workspace module becomes a local reference. Anything else is an
   error: a classpath root that is not an artifact, a git dep, or a
   declared module cannot enter the lock.
6. **Write the lock** — the kernel returns the plan *without hashes*.
   The Go binary fetches and hashes every artifact, then writes
   `deps.lock` ([the closed build](closed-build.md)).

## Version selection (`select-versions`)

Per floating coordinate, in order of precedence:

1. **Requirement override** — an explicit version passed to the command,
   for example `rig update org.clojure/core <v>`. It always wins, and
   Rig records it in the lock as `explicit`.
2. **Existing lock pin** — by default (`respect-existing-pins`), a
   coordinate already pinned in `deps.lock` keeps that version. This is
   what makes re-locks stable: `rig lock` never bumps a version you
   already use, and `rig update` of one coordinate keeps the rest.
3. **Native resolution** — when all cooldowns of the workspace are
   zero, Rig leaves the floating requirement to `tools.deps` itself. Rig
   adds nothing, and the result is the answer of the ecosystem,
   untouched.
4. **Cooldown-gated selection** — otherwise Rig looks at the version
   candidates of the repository and picks the newest one old enough
   (below).

### Candidates and ordering: the ecosystem's data, the ecosystem's comparator

- **Candidates** come from the standard `maven-metadata.xml` each
  repository serves for the coordinate — the same file Maven and
  tools.deps read. `LATEST` uses the `<latest>` element of the
  metadata. It falls back to all listed versions when a repository
  publishes none. `RELEASE` uses all listed versions.
- **Ordering** uses `DefaultArtifactVersion`, the Maven version
  comparator — the same ordering the rest of the ecosystem sees, so
  "newest" means what it means everywhere else.
- **Publication time**, for the cooldown, comes from the per-repository
  `maven-metadata-<repo-id>.xml` that Maven repositories publish. It
  holds a `<updated>` timestamp per version, and falls back to the
  `lastUpdated` of the metadata.

Rig refuses a candidate published less than the per-repo cooldown before
resolve time, and the next-newest eligible version wins. Rig records
every refused version in the `skipped` list of the lock, with its reason
(`cooldown`, `forced`, `explicit`). You see one line per decision. Rig
refuses a coordinate with *no* eligible candidate outright. `rig lock`
exits 5, and the retry is `--force` (Rig records it as `forced`). The
mechanics and the escape hatches are on the
[security model](../concepts/security.md) page.

## Repository semantics

- The default repositories are **central** and **clojars** — the same
  pair tools.deps starts from.
- A `:mvn/repos` entry whose id matches a default **replaces** the
  built-in — the same rule tools.deps applies — so each id appears once.
- **Which repository serves a locked artifact** — Rig decides this at
  lock time and pins the answer into the lock (`repository` + `url`). It
  checks in order: first the repository Maven itself recorded in
  `~/.m2/…/_remote.repositories` (offline and exact), then a HEAD probe
  of each declared repository, then the first one. From then on, the hot
  build fetches from the recorded URL and never re-probes
  ([the closed build](closed-build.md)).
- Credentials for private repositories come from `~/.m2/settings.xml`,
  the same file Maven and tools.deps use. The local auth proxy of Rig
  serves `:auth :oidc` repositories instead.

## Ecosystem interop

- **Each module stays independently resolvable.** `clojure -Sdeps`,
  editors, and any other tooling read the `deps.edn` exactly as before —
  the pins of Rig live in the lock, not in the manifest. Resolution
  never writes the manifest on disk; the only writers are the explicit
  `add`/`remove`/`update` commands (`edit-dep` op, format-preserving via
  `rewrite-clj` + cljfmt) and `migrate`.
- **Git dependencies** use the `tools.deps`/`tools.gitlibs` convention
  (`~/.gitlibs/libs`, `group/name:<short-sha>:jar` coordinates), so Rig
  shares checkouts with the `clojure` CLI and never duplicates them.
- Rig reuses **`~/.m2/repository`** as an artifact source. It imports an
  artifact into the Rig cache only when the hash of its bytes matches.
- **`rig outdated`** compares the pins of the lock against the same
  `maven-metadata.xml`. It reports `latest`, `latest-satisfying` (same
  major), and whether the jump breaks compatibility, from the same
  candidate pool that selection uses.

## Deliberate deviations

**Cooldowns gate *selection only*.** They change which version Rig picks
for a floating requirement — never the graph itself, never a concrete
requirement, never the order tools.deps computes. The decision adds
information, and Rig records it: `skipped` in the lock,
`refused`/`forced` on stderr. Set every cooldown to `"0s"`. Then the
selection layer of Rig becomes the identity: what you get is exactly
what `clojure` would give you.

## Local modules beat published coordinates

The second deviation lives in the graph, not in selection. When the same
library enters a basis as both a `:local/root` requirement and a
`:mvn/version` requirement, vanilla tools.deps cannot order the two, and
resolution fails. This case typically arises when a transitive POM
references the published artifact of the module itself. Rig registers
`compare-versions` methods for the cross-type pair so that the local
module dominates, in either direction. The lock records the local
module, and the published coordinate never enters it.

The ranking follows the own dominance rule of tools.deps:
strictly-greater wins. So both directions agree, and the ranking matches
what a workspace implies. The module is source you control, and a
published artifact under the same coordinate must not shadow it.
