# CI

The CI workflow is two commands, plus the release (`rig publish` and a
tag — [Build, publish, release](build-publish.md#releasing)):

```sh
rig check && rig test
rig build --uber
rig publish
```

## The gate

| Step | What it proves |
|---|---|
| `rig check` | the locked artifacts are byte-identical to the pinned hashes — verified from local sources on a warm cache, fetched and hash-checked on a cold one (the supply-chain gate, exit 4 on a mismatch). The lock still satisfies every manifest (no stale pins, conflicts, drift, floating versions). Every module's namespaces load on the locked classpath. No lock modification. |
| `rig test` | the tests pass on the locked classpath; a stale lock fails (exit 3) instead of re-locking. |

A stale lock is a hard error on every gate command: the hot commands exit
3, and `check` reports staleness as a problem (exit 1) — a requirement the
pin no longer satisfies, or a manifest whose bytes changed since the lock
was written. The lock is never refreshed under your feet: any deviation
from the committed lockfile fails the pipeline. The gate can use the
network to fill a cold cache. Either way, Rig hash-checks every byte
against the lockfile.

Run `rig build --uber` afterwards for artifacts (or whatever your
pipeline builds).

## A complete example

Example GitHub Actions workflow:

```yaml
name: Deploy from main

on:
  push:
    branches: [main]

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1

      - name: Install rig (binary, JVM, cache)
        uses: brutasse/setup-rig@b204f6b12fde6574ebbebe0227fcaaa10391882f # v2.0.0
        with:
          version: v0.4.0

      - name: Check and test
        run: |
          rig check
          rig test

      - name: Build uberjars
        run: rig build --uber

      - name: Release
        run: |
          rig publish
          git tag "v$(cat VERSION)"
          git push origin HEAD --tags
```

Notes:

- **setup-rig** installs the Rig binary (verified with the SHA256 hashes in
  the release `SHA256SUMS`) and the JVM pinned in `deps.lock`, and restores
  the artifact cache keyed on `deps.lock`. The action saves the cache
  automatically at the end of the job — even after a failed step — so you
  need no save step. A bare runner is enough: the host needs neither a JDK
  nor the Clojure CLI. Pin the
  action ref to a git sha, and pin `version` to a release.
- **Git dependencies** use the same git/ssh setup your repository already
  uses. Their commit shas are in the lockfile, so a green run means "this
  exact commit". A bare runner provisions the checkouts itself: when a
  checkout is missing, the command fetches the locked commit into the
  gitlibs root (`~/.gitlibs`, or `$GITLIBS` when set).
- The cache key is `deps.lock` (same lockfile → warm hit, changed
  lockfile → miss and re-fetch). The default gate can fetch on a cold
  cache, so the cache only adds speed there. The offline gate below needs
  it: a cache hit is what keeps that build hermetic.
- The release is `rig publish` and a tag on the released version: the
  version history is yours ([Build, publish, release](build-publish.md#releasing)).
  The push on `main` that triggered the workflow already carries the
  branch, so the step only pushes the tag.

## Hermetic / air-gapped CI (opt-in `--offline`)

To prove the build needs no network — or when CI itself has no network —
add `--offline` to the gate:

```sh
rig check --offline && rig test --offline
```

`--offline` refuses the network. Every artifact (and the kernel and runner
jars) must already sit in the Rig cache (`~/.local/share/rig`) or in a
checksum-checked Maven repository (`~/.m2/repository`). On a cold cache,
Rig fails with exit 1. That is the point: a green offline run is the
hermeticity proof. `--offline` refuses the git fetch too: a missing
checkout fails the gate and points at the gitlibs root. Cache the Rig
cache alongside `~/.m2/repository` and
`~/.gitlibs`, or run on a machine that has them filled. Those are exactly
the stores setup-rig caches. When its `cache-hit` output is `true`, an
offline gate has everything it needs. Check that output if the job should
fail fast on a cold cache.

## Managed JVMs

When the workspace pins a JVM (`:rig/jvm` in the root `deps.edn`), Rig uses
the rig-managed JDK for that major from the state dir. A matching system
JDK serves only when no managed JDK exists. For CI, that means:

- Do not rely on the runner's system JDK. Rig uses the managed JDK
  automatically. setup-rig installs the pinned major by default and caches
  the JVM store under its own key, so a lockfile bump never re-downloads a
  JDK. On other CI, cache `~/.local/share/rig` (it contains `jdks/`) or
  pre-install with `rig jvm install <major>`.
- Hermetic `--offline` builds need the pinned JDK already in the state dir,
  or they fail with a hint.
- Moving the pinned major to its newest release is a store-level change:
  `rig jvm update` replaces the rig-managed JDK. Rig does not write
  `deps.lock`, so there is nothing to commit.

## Per-PR testing

For branch-level CI that does not deploy, the same gate works on a
subset:

```sh
rig check -p modules/example   # -p restricts the ns-load stage
rig test -p modules/example :kaocha.filter/focus '[:unit]'
```

Stages 1 (artifact hashes) and 2 (lockfile vs manifests) of `check` are
always workspace-wide. `-p` only narrows the namespace-loading stage 3.

## What CI must NOT do

- run `rig lock` in CI — a stale lock must fail the gate (exit 3), not get
  refreshed under your feet; fixing it belongs to the PR that changed the
  requirements;
- commit `deps.lock` changes from CI — lockfile changes belong to the PR
  that changed the requirements, where you review them.
