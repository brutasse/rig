# Security model

Rig protects the software supply chain in four ways: it **pins** what you
use, **verifies** what you run, applies a **cooldown** to what you
adopt, and **audits** what it locked against known vulnerabilities.

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
  automatic repair: `check` and the hot commands fail the build. A byte
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
and tools.deps use the same file, and so does Rig at lock time, when it
probes the repositories to decide which one serves an artifact.

## `check`: the CI security gate

```sh
rig check
```

```
check: verified 528 artifacts (510 cached, 18 fetched, 2 git deps pinned by commit sha)
```

`check`'s first stage re-checks every artifact in the lockfile against
the pinned hashes. On a warm cache it uses local sources and checks their
hashes. If the cache is cold, it fetches each artifact and checks its
hash. The stage runs before any JVM launches, and `check` never writes
the lockfile, so a green first stage means: "Rig pinned these exact
bytes, and they are what will run." Add `--offline`
to also prove that the build needs no network. That form needs a warm
cache, and air-gapped CI uses it. Run `check` as the first gate in a CI
pipeline (see [CI](../workflows/ci.md)).

The exit codes that matter here:

| Code | Meaning |
|---|---|
| 0 | everything matches |
| 1 | `--offline`: an artifact is in neither the cache nor `~/.m2` |
| 3 | lock missing |
| 4 | hash mismatch — artifact bytes differ from the lock |

A stale lock does not gate `check`: it is reported as a `stale-lock`
problem (exit 1), and the lock is never re-written.

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
in the lock; newly selected versions face the window.

## Known vulnerabilities: `rig audit`

Hashing pins the bytes and cooldowns gate the adoption — but neither
says whether a version you already use has a known vulnerability.
`rig audit` asks the OSV vulnerability database (osv.dev) about every
maven artifact pinned in the lock and lists each known vulnerability of
a pinned version, in the scopes it is locked into (modules, and
`module:alias` for an alias classpath):

```
audit: 1 known vulnerability in 1 of 510 pinned artifacts

com.fasterxml.jackson.core/jackson-databind 2.9.0
  used in ., modules/backend
  pulled in:
    .: com.fasterxml.jackson.dataformat/jackson-dataformat-xml:2.9.0 → com.fasterxml.jackson.core/jackson-databind:2.9.0
  GHSA-… (CVE-2024-…) Deserialization of untrusted data
    published 2024-… · severity CVSS_V3 9.8 Critical · fixed in 2.17.1 · other series: 2.21.4
```

Severity is the OSV score when the record carries one, the CVSS base
score computed from the record's vector string otherwise (CVSS v3 and
v4, in-process), with its bucket — so the report says how bad, not
just what. The fix is shown per series: the fixes in the locked
version's own series first (the upgrade you can take without crossing
series), the other series' fixes marked. `--explain` adds, per
finding, the chains of direct deps that pull the artifact in (one per
scope) — the report says not just what is vulnerable, but where it
came from.

It exits 1 when anything is found, so `rig audit` sits next to
`rig check` as a CI gate — with the difference that it needs the
network (OSV is the data source) and never joins the hermetic
`--offline` gate. A scan that cannot complete is its own exit, 4:
transient OSV failures (a network error, a 5xx, a 429) are retried,
and a scan that still fails, or that left a vulnerability record
unfetched, exits 4 — the report notes how many artifact records could
not be fetched — distinct from 1, "has findings". Only the public
repositories are scanned: an
overridden `central` or `clojars` counts as the real one, while
artifacts from other `:mvn/repos` repositories are private and skipped
(counted in the report), as are git deps, which are pinned by commit
sha. The OSV API is a trust boundary like the Maven repositories or the
Adoptium API: Rig reports what OSV knows, and pins nothing from it.

`rig audit --fix` closes the loop on the force pins it writes. A force
pin is a promise that a version must not be pulled — but the promise
ages: the pull it stops may go away (a module starts declaring the
coordinate, a dependency moves on), and the pin then only costs — it
pins a version nothing else would have picked. So the fix sweeps the
root's `:rig/overrides` section against the pins' own absence: it
re-derives the workspace with the section stripped from the basis and
judges each pin — the ones it just wrote included. A pin no module
resolves its coordinate at a vulnerable version without is stale and is
dropped; a pin the pin-less resolution still pulls vulnerable is
load-bearing and stands; a pin whose resolution OSV cannot query (a git
or local version) is kept, conservatively. The pins that stand ride the
fix's single re-lock, and the verification re-queries the dropped
pins' coordinates in the final lock: a pin the final lock shows was
load-bearing after all comes back at the version the fix left it, and
the report says so (`keep`). The section's net change — which pins were
added, removed, updated, or kept — is the `overrides` block of the
report, and the JSON's `fix.overrides` list. The `:rig/deps` pool, a
durable declaration rather than a security pin, is never swept.

## Reproducibility

A build is a function of three things: your source tree, `deps.lock`, and
the artifact cache. CI runs it against the committed lockfile:

```sh
rig check && rig test
```

The gate can use the network to fill a cold cache. Either way, Rig
hash-checks every byte against the lockfile. For the strongest claim — a
hermetic or air-gapped build — add `--offline` to the two commands. That
needs a warm cache:

```sh
rig check --offline && rig test --offline
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
