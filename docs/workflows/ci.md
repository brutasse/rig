# CI

The CI story is three commands, plus the release:

```sh
rig verify --frozen && rig check --frozen && rig test --frozen
rig build --uber --frozen
rig release
```

## The gate

| Step | What it proves |
|---|---|
| `rig verify --frozen` | the locked artifacts are byte-identical to the pinned hashes — verified from local sources on a warm cache, fetched and hash-checked on a cold one. No lock modification. This is the supply-chain gate. |
| `rig check --frozen` | the lock still satisfies every manifest (no stale pins, conflicts, drift, floating versions), and every module's namespaces load on the locked classpath. |
| `rig test --frozen` | the tests pass on the locked classpath, and if the lock were stale, fail instead of re-locking. |

`--frozen` is what makes this a gate: any deviation from the committed
lock is a hard error (exit 3), not a silent refresh. The gate may use the
network to fill a cold cache — every byte is hash-checked against the lock
either way.

Run `rig build --uber --frozen` afterwards for artifacts (or whatever
your pipeline builds).

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
    container:
      image: clojure:openjdk-17-tools-deps-slim-bullseye
      volumes:
        - ${{ github.workspace }}:${{ github.workspace }}

    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1

      - name: Install rig + restore cache
        uses: brutasse/setup-rig@v1
        with:
          version: v0.1.0
          token: ${{ github.token }}

      - name: Verify, check and test (frozen)
        run: |
          rig verify --frozen
          rig check --frozen
          rig test --frozen

      - name: Build uberjars (frozen)
        run: rig build --uber --frozen

      - name: Release
        run: rig release

      - name: Save rig cache
        if: always()
        uses: brutasse/setup-rig/save@v1
```

Notes:

- **setup-rig** installs the rig binary (SHA256-verified against the
  release `SHA256SUMS`) and restores the artifact cache keyed on
  `deps.lock`; the `setup-rig/save` step (last) persists what the run
  populated. Pin the action ref to a tag or git sha, and `version` to a
  release.
- **Git dependencies** use the same git/ssh setup your repository already
  uses; their commit shas are in the lock, so a green run means "this
  exact commit".
- The cache is keyed on `deps.lock` (same lock → warm hit, changed lock →
  miss and re-fetch). It is optional speed for the default gate, which can
  fetch on a cold cache, but required for the offline gate below: a cache
  hit is what keeps that build hermetic.
- `rig release` pushes the branch and the tag, so the workflow can run
  from a push on `main` (or keep the release manual — `rig release
  --dry-run` in CI is a cheap plan check).

## Hermetic / air-gapped CI (opt-in `--offline`)

To prove the build needs no network — or when CI itself has no network —
add `--offline` to the gate:

```sh
rig verify --frozen --offline && rig check --frozen --offline && rig test --frozen --offline
```

`--offline` refuses the network, so every artifact (and the kernel jar)
must already be in the rig cache (`~/.local/share/rig`) or a
checksum-checked Maven repo (`~/.m2/repository`). On a cold cache it fails
(exit 1) — that is the point: a green offline run is the hermeticity
proof. Cache the rig cache alongside `~/.m2/repository`/`~/.gitlibs`, or
run where they are pre-populated.

## Managed JVMs

When the workspace pins a JVM (`:rig/jvm` in the root `deps.edn`), the lock
records the exact JDK version and `rig` installs it into the state dir on
first use. For CI that means:

- Do not rely on the runner's system JDK: the pinned JDK is used
  automatically, downloading on the first run (~200 MB) and hitting the
  state dir afterwards. Cache `~/.local/share/rig` (it contains `jdks/`)
  or pre-install with `rig jvm install <version>`.
- Hermetic `--offline` builds need the pinned JDK already installed in the
  state dir, or they fail with a hint.
- Bumping the JDK is a lock change: run `rig jvm update` locally and commit
  the updated `deps.lock` — the same review flow as a dependency bump.

## Per-PR testing

For branch-level CI that doesn't deploy, the same frozen trio works on a
subset:

```sh
rig verify --frozen
rig check --frozen -p modules/example   # -p restricts the ns-load stage
rig test --frozen -p modules/example :kaocha.filter/focus '[:unit]'
```

`check`'s stage 1 (lock vs manifests) is always workspace-wide; `-p`
only narrows the namespace-loading stage 2.

## What CI must NOT do

- run plain `rig test` without `--frozen` — a stale lock would re-lock in
  CI and the failure you wanted would disappear;
- commit `deps.lock` changes from CI — lock changes belong to the PR that
  changed the requirements, where they get reviewed.
