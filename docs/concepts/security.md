# Security model

Rig protects the software supply chain in three ways. It **pins** what you
use. It **verifies** what you run. It applies a **cooldown** to what you
adopt.

## Every artifact is hash-pinned

`deps.lock` records a sha256 hash for every Maven artifact in the resolved
tree. The Rig binary itself computes each hash over the exact bytes it
stores. Rig never trusts a hash from a repository, a POM, or an existing
`~/.m2`.

Before each JVM launch, Rig checks every classpath artifact against the
lockfile:

- the hash matches an entry in the content-addressed cache, or in a
  checksum-checked `~/.m2` copy → Rig uses it;
- missing → Rig downloads it and hashes it (unless `--offline`);
- **mismatch → the command aborts with exit code 4.** There is no
  automatic repair: `verify` and the hot commands fail the build. A byte
  difference in a pinned artifact is a supply-chain incident, not an
  inconvenience.

An artifact that is not in the lockfile cannot enter a classpath. The
build classpath stays closed.

### Where bytes come from

When the cache does not hold an artifact, Rig tries these sources in order:

1. the Rig cache (`~/.local/share/rig` by default);
2. the local `~/.m2/repository` copy. Rig accepts it only when the copy is
   self-consistent (its Maven `.sha1` matches its own content) or when it
   hashes to the sha the lockfile already pins. Rig skips a poisoned m2
   copy. It never uses one;
3. a download from the repository URL, over HTTPS.

Credentials for private repositories come from `~/.m2/settings.xml`. Maven
and tools.deps use the same file.

## `verify`: the CI security gate

```sh
rig verify --frozen
```

```
verified 528 artifacts (510 cached, 18 fetched, 2 git deps pinned by commit sha)
```

`verify` re-checks every artifact in the lockfile against the pinned
hashes. On a warm cache it uses local sources and checks their hashes. On a
cold cache it fetches each artifact and checks its hash. With `--frozen`,
`verify` refuses to change the lockfile. A green `verify --frozen` means:
"Rig pinned these exact bytes, and they are what will run." Add `--offline`
to also prove that the build needs no network. That form needs a warm
cache, and air-gapped CI uses it. Place it as the first gate in a CI
pipeline (see [CI](../workflows/ci.md)).

The exit codes that matter here:

| Code | Meaning |
|---|---|
| 0 | everything matches |
| 1 | `--offline`: an artifact is in neither the cache nor `~/.m2` |
| 3 | lock missing or stale under `--frozen` |
| 4 | hash mismatch — artifact bytes differ from the lock |

## Cooldowns: the adoption window

Hashing protects the versions you already use. It says nothing about a
version you are about to adopt. A compromised release is dangerous in the
first hours after publication, before someone detects it, yanks it, or
writes about it.

Rig therefore refuses to *select* a version published less than the
cooldown window before the resolve time:

- the default window is **48 hours**. Set it per workspace with
  `:rig/cooldown`, for example `"48h"`, `"72h"`, or `"0s"` to disable.
- set per-repository overrides in `:rig/cooldown-repos`, for example
  `{"corp" "72h"}`;
- Rig takes the version age from the repository's `maven-metadata.xml`
  timestamps — the metadata it already reads to list versions, not an
  HTTP header.

When Rig refuses a fresh version, Rig reports it and picks the newest
*older* eligible version:

```
skipped org.clojure/test.check 1.1.5 (published 26h ago, cooldown 48h); using 1.1.0
```

Rig records every decision in the `skipped` list of the lockfile, and
prints it:

- `skipped …` — a cooldown refused the version, and Rig selected an older
  version;
- `forced …` — `--force` bypassed the cooldown;
- `pinned … (explicit)` — you named the version yourself
  (`rig update <coord> <version>`). That is your judgment, and it always
  wins.

Rig never re-applies a cooldown to an already-locked version: cooldowns
apply at selection time only. A plain `rig lock` keeps the pins already
in the lock; newly selected versions face the window. (`rig update` with
no arguments does not keep the lock's pins — it re-selects floating
requirements.)

## Reproducibility

A build is a function of three things: your source tree, `deps.lock`, and
the artifact cache. CI runs the frozen form of that function:

```sh
rig verify --frozen && rig check --frozen && rig test --frozen
```

The gate can use the network to fill a cold cache. Either way, Rig
hash-checks every byte against the lockfile. For the strongest claim — a
hermetic or air-gapped build — add `--offline` to the three commands. That
needs a warm cache:

```sh
rig verify --frozen --offline && rig check --frozen --offline && rig test --frozen --offline
```

If that passes on a machine with no network access, the bytes that ran are
exactly the bytes your lockfile pinned. Getting them needed no network.

## Managed JVMs

When a workspace pins a JVM (`:rig/jvm`), the lockfile records the major
(feature) version the project runs on. That is what matters for
compatibility. Every machine runs that major. A machine uses the
rig-managed JDK for that major when the state dir has one. The managed JDK
takes precedence over the system `java`. When the state dir has no managed
JDK, the machine uses a matching system JDK. When a machine has neither and
has
network access, Rig installs the newest matching release; an offline
machine fails with a hint instead. The exact patch release can differ
between machines (21.0.10 vs 21.0.11). The major never differs.

When Rig installs a managed JDK, Rig downloads the archive from the release
assets of the vendor. Before Rig extracts the archive, it verifies it
against the sha256 the Adoptium API publishes for that exact build. This is
the same trust class as a Maven repository checksum. Rig never launches a
managed JDK it has not verified.

## Residual trust

The boundaries of this model:

- The resolver kernel Rig invokes is a pinned jar (version, git sha, and
  jar sha256). You trust it like any tool on your machine.
- The integrity of the graph above the artifact bytes (POMs, repository
  metadata) relies on the repositories. Rig itself always hash-pins the
  final bytes of every jar.
- Rig verifies each managed JDK against the checksum the Adoptium API
  publishes for the exact release Rig installs. The API is a trust
  boundary, like a Maven repository.
- `--force` and explicit version pins are escape hatches you own. The
  lockfile records that you used them.
- You can collapse the repository trust boundary into a single
  [Pier](https://brutasse.github.io/pier/) instance: one OIDC-gated URL for
  the public world and for the corporate jars
  ([all artifacts through one repository](../workflows/pier.md)). The
  per-artifact hash pins of Rig do not change.
