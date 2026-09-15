# Security model

rig's supply-chain story has three parts: **pin** what you use, **verify**
what you run, and **cool down** what you adopt.

## Every artifact is hash-pinned

`deps.lock` records a sha256 for every Maven artifact in the resolved
tree. The hash is computed by the rig binary itself, over the exact bytes
it stores — never trusted from a repository, a POM, or a pre-existing
`~/.m2`.

Before any JVM launch, rig checks each classpath artifact against the
lock:

- present in the content-addressed cache or in a checksum-checked
  `~/.m2` copy and matching → used;
- missing → downloaded and hashed (unless `--offline`);
- **mismatch → the command aborts with exit 4.** There is no self-heal:
  `verify` and the hot commands fail the build, because a byte difference
  in a pinned artifact is a supply-chain incident, not an inconvenience.

An artifact that is not in the lock cannot enter a classpath. The build
classpath is closed.

### Where bytes come from

When rig needs an artifact it is not caching, sources are tried in order:

1. the rig cache (`~/.local/share/rig` by default);
2. the local `~/.m2/repository` copy — accepted only when it is
   self-consistent (its Maven `.sha1` matches its own content) or when it
   hashes to the sha the lock already pins. A poisoned m2 copy is skipped,
   never used;
3. a download from the repository URL, over HTTPS.

Credentials for private repositories come from `~/.m2/settings.xml`, the
same file Maven and tools.deps use.

## `verify`: the CI security gate

```sh
rig verify --frozen
```

```
verified 528 artifacts (510 cached, 18 fetched, 2 git deps pinned by commit sha)
```

`verify` re-checks every lock artifact against the pinned hashes: from
local, hash-checked sources on a warm cache, or fetched and hash-checked on
a cold one. With `--frozen` it refuses to modify the lock. A green
`verify --frozen` means "these exact bytes, that we pinned, are what will
run." Add `--offline` to also prove the build needs no network — that form
needs a warm cache and is the one used by air-gapped CI. It is the first
gate in a CI pipeline (see [CI](../workflows/ci.md)).

Exit codes that matter here:

| Code | Meaning |
|---|---|
| 0 | everything matches |
| 1 | `--offline`: an artifact is in neither the cache nor `~/.m2` |
| 3 | lock missing or stale under `--frozen` |
| 4 | hash mismatch — artifact bytes differ from the lock |

## Cooldowns: the adoption window

Hashing protects the versions you already use. It says nothing about a
version you are about to adopt — a compromised release is dangerous
precisely in the first hours, before it is detected, yanked, or written
up.

rig therefore refuses to *select* a version that was published less than
the cooldown window before resolve time:

- the default window is **48 hours**, set per workspace with
  `:rig/cooldown` (e.g. `"48h"`, `"72h"`, `"0s"` to disable);
- per-repository overrides go in `:rig/cooldown-repos`, e.g.
  `{"corp" "72h"}`;
- version age comes from repository metadata / `Last-Modified`.

When a fresh version is refused, rig says so and picks the newest
*older* eligible version:

```
skipped org.clojure/test.check 1.1.5 (published 26h ago, cooldown 48h); using 1.1.0
```

Every decision is recorded in the lock's `skipped` list and printed:

- `skipped …` — refused by cooldown, older version selected;
- `forced …` — `--force` bypassed the cooldown;
- `pinned … (explicit)` — you named the version yourself
  (`rig update <coord> <version>`); that is your judgment and it always
  wins.

Already-locked versions are never re-aged: cooldowns apply at selection
time only. `rig update` with no changes keeps existing pins.

## Reproducibility

A build is a function of three things: your source tree, `deps.lock`, and
the artifact cache. CI runs the frozen form of that function:

```sh
rig verify --frozen && rig check --frozen && rig test --frozen
```

The gate may use the network to fill a cold cache; every byte is
hash-checked against the lock either way. For the strongest claim —
hermeticity, or an air-gapped build — add `--offline` to the trio (needs a
warm cache):

```sh
rig verify --frozen --offline && rig check --frozen --offline && rig test --frozen --offline
```

If that passes on a machine with no network access, the bytes that ran
are exactly the bytes your lock pinned, and getting them needed no network.

## Managed JVMs

When a workspace pins a JVM (`:rig/jvm`), the lock records the **exact**
release (e.g. `21.0.12.1+1`), so the JVM is part of the build function: the
same lock means the same JDK version on every machine, including CI.

The JDK archive is downloaded from the vendor's release assets and verified
against the sha256 published by the Adoptium API for that exact build,
before it is extracted — the same trust class as a Maven repository
checksum. rig never launches a JDK it has not verified.

## Residual trust

Be honest about the boundaries:

- The resolver kernel rig invokes is a pinned jar (version + git sha +
  jar sha256), trusted like any tool on your machine.
- Graph integrity above the artifact bytes (POMs, repository metadata)
  relies on the repositories. The final bytes of every jar are always
  hash-pinned by rig itself.
- Managed JDKs are verified against the checksum the Adoptium API
  publishes for the exact release the lock pins; the API is a trust
  boundary, like a Maven repository.
- `--force` and explicit version pins are escape hatches you own; the lock
  records that you used them.
- You can collapse the repository trust boundary to a single
  [Pier](https://brutasse.github.io/pier/) instance — one OIDC-gated URL
  for the public world and the corporate jars
  ([all artifacts through one repository](../workflows/pier.md)). rig's
  per-artifact hash pins are unchanged.
