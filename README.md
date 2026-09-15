# Rig - Opinionated Clojure build tool

## Documentation

User guide, workflows, configuration, and migration guides:
[rig documentation](https://brutasse.github.io/rig/).

## Install

One-shot userland install, from the GitHub release artifacts (the script is
pinned at the git sha of the latest release; the release workflow re-pins it
after each release — `PENDING-FIRST-RELEASE` is replaced by the first one):

```sh
curl -fsSL https://raw.githubusercontent.com/brutasse/rig/PENDING-FIRST-RELEASE/scripts/install-rig.sh | sh
```

Installs to `~/.local/bin` (override: `INSTALL_DIR=…`), verifies the binary
against the release's `SHA256SUMS`. The resolver kernel jar is not
installed: the rig binary fetches its pinned kernel from the matching GitHub
release on first use, hash-verified. Pass a version to install something
other than the latest release: `sh -s -- v0.2.0`.

Self-update:

```sh
rig self-update                    # update to the latest release, if newer
rig self-update --check            # report only
rig self-update --version v0.2.0   # update to a specific release
```

rig also checks for new releases at most once per 24h (TTL in the state dir;
skipped under `--offline` and on local/dev builds) and prints one line on
stderr when a newer release exists.

Releasing rig: tag `vX.Y.Z` and push it. The CI workflow
(`.github/workflows/ci.yml`) runs the test suite, and once it is green the
release job builds the kernel jar and cross-compiled rig binaries, stamps
the kernel pin, publishes the binaries + kernel jar + `SHA256SUMS` as
GitHub release assets, and re-pins the install one-liner sha in this file.

## JVMs

A project can pin its JVM with `:rig/jvm` in the root `deps.edn`
(e.g. `{:rig/jvm "21"}`). rig then manages the JDK: `rig jvm
install 21`, the lock records the exact Temurin release, and missing JDKs
are auto-installed into the rig state dir (hash-verified via the Adoptium
API). `rig new` scaffolds projects with the current LTS pin. Without the
pin, rig uses `JAVA_HOME`/`PATH`; when no JVM is found, it suggests the
current LTS to install (it never installs on its own).

## Docker

The CI workflow also publishes `ghcr.io/brutasse/rig`
(`vX.Y.Z` + `latest`, amd64/arm64): the rig binary plus its hash-pinned
kernel jar, pre-baked into the rig state dir (no first-run GitHub fetch),
on a Temurin 21 base.

```dockerfile
# standalone base, or multi-stage:
FROM ghcr.io/brutasse/rig:latest
# — or —
COPY --from=ghcr.io/brutasse/rig:latest /usr/local/bin/rig /usr/local/bin/
COPY --from=ghcr.io/brutasse/rig:latest /root/.local/share/rig /root/.local/share/rig
```

See [Docker in the docs](https://brutasse.github.io/rig/workflows/docker.html)
(pinned JVMs, non-root users, offline builds).

## Local development

Prerequisites: JDK 21+, Clojure CLI, Go 1.27+, make.

```
make dev    # build the kernel jar, stamp the kernel pin, build rig
make test   # make dev + kernel kaocha suite + Go suite (E2E vs the fresh jar)
make run WS=<workspace-dir> ARGS="lock"   # run the local rig in a workspace
make image  # build the Docker image locally (no push)
make release V=0.2.0   # package a release in rig/dist/release/ (dry run)
```

`make dev` is the whole loop: `resolver/target/rig-resolver-<V>.jar` is
built (tools.build), the kernel pin in `rig/internal/kernel/kernel.go` is
stamped (version, git sha, GitHub release URL, jar sha256 — `make pin`),
and `rig/dist/rig` is built. `V` defaults to `0.1.0` locally.

How the kernel jar reaches the rig binary:

- Local: `RIG_KERNEL_JAR=<absolute jar path>` — verified against the
  `JARSHA` pin, then used in place of the downloaded kernel.
- Published: without `RIG_KERNEL_JAR`, rig fetches the pinned kernel from
  the GitHub release assets into its cache and hash-verifies it. The pin is
  stamped per release by the release workflow.
- `make run` sets `RIG_KERNEL_JAR` for you; other invocations of
  `rig/dist/rig` need it exported.
- The Go E2E tests find the kernel jar via `RIG_TEST_KERNEL_JAR` (set by
  `make test`) or `resolver/target/rig-resolver-0.1.0.jar` and **skip**
  (not fail) when it is missing — build it first (`make kernel`) or they
  silently don't run.

## License

MIT — see [LICENSE](LICENSE).
