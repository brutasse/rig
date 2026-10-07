<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/rig-dark.svg">
    <source media="(prefers-color-scheme: light)" srcset="docs/assets/rig-light.svg">
    <img alt="Rig logo — derrick" src="docs/assets/rig-light.svg" width="140">
  </picture>
</p>

# Rig

Build and run Clojure projects, the obvious way: one binary, one manifest,
one lockfile, one way to do each thing.

Builds are boring and standardized — the last place you want to spend
innovation tokens. tools.build asks you to write your build as a program
in Clojure and maintain it for the life of the project; Rig takes the
opposite bet. Instead of build code you write and defend, you get one
native binary with one implementation of each operation: an authoritative
`deps.lock` that sha256-pins the entire resolved dependency tree, every
artifact hash-verified before it enters a classpath, and drift that fails
loudly instead of living silently in hand-maintained files.

## Documentation

User guide, workflows, configuration, and migration guides:
[Rig documentation](https://brutasse.github.io/rig/).

## Install

One-shot userland install, from the GitHub release artifacts (the script is
pinned at the git sha of the latest release; the release workflow re-pins it
after each release):

```sh
curl -fsSL https://raw.githubusercontent.com/brutasse/rig/f5eba1e448a746e1576933a534c4b184a005a080/scripts/install-rig.sh | sh
```

Installs to `~/.local/bin` (override: `INSTALL_DIR=…`), verifies the binary
against the release's `SHA256SUMS`. The resolver kernel jar is not
installed: the Rig binary fetches its pinned kernel from the matching GitHub
release on first use, hash-verified. Pass a version to install something
other than the latest release: `sh -s -- v0.3.0`.

Self-update:

```sh
rig self-update                    # update to the latest release, if newer
rig self-update --check            # report only
rig self-update --version v0.3.0   # update to a specific release
```

Rig also checks for new releases at most once per 24h (TTL in the state dir;
skipped under `--offline` and on local/dev builds) and prints one line on
stderr when a newer release exists. `RIG_UPDATE_CHECK=0` silences the
check.

Releasing Rig: tag `vX.Y.Z` and push it. The CI workflow
(`.github/workflows/ci.yml`) runs the test suite, and once it is green the
release job builds the kernel jar and cross-compiled Rig binaries, stamps
the kernel pin, publishes the binaries + kernel jar + `SHA256SUMS` as
GitHub release assets, and re-pins the install one-liner sha in this file.

## JVMs

A project can pin its JVM with `:rig/jvm` in the root `deps.edn`
(e.g. `{:rig/jvm "21"}` — the major version). Rig then manages the JDK:
`rig jvm install 21` installs the newest Temurin 21.x into the Rig state
dir, the rig-managed JDK takes precedence over the system `java`, and a
machine with neither gets the newest matching release auto-installed
(hash-verified via the Adoptium API); `rig jvm update` moves the
installed JDK to its newest release. `rig new` scaffolds projects with
the current LTS pin. Without the pin, Rig uses `JAVA_HOME`/`PATH`; when
no JVM is found, it suggests the current LTS to install (it never
installs on its own).

Native-image builds (`rig build --native`) work off the same pin: the
GraalVM major is derived from `:rig/jvm`. The build never downloads:
`rig build --native` requires the GraalVM installed and fails with a
hint otherwise — `rig graalvm install 21` downloads the newest 21.x CE
build from the `graalvm/graalvm-ce-builds` GitHub releases (hash-verified)
into the state dir. A native build also needs a C
compiler and a few GB of RAM, and takes minutes.

## Docker

The CI workflow also publishes `ghcr.io/brutasse/rig`
(`vX.Y.Z` + `latest`, amd64/arm64): the Rig binary plus its hash-pinned
kernel jar, pre-baked into the Rig state dir (no first-run GitHub fetch),
on a Temurin 21 base.

```dockerfile
# standalone base, or multi-stage:
FROM ghcr.io/brutasse/rig:latest
# — or —
COPY --from=ghcr.io/brutasse/rig:latest /usr/local/bin/rig /usr/local/bin/
COPY --from=ghcr.io/brutasse/rig:latest /root/.local/share/rig /root/.local/share/rig
```

See [Docker in the docs](https://brutasse.github.io/rig/workflows/docker.html)
(pinned JVMs, non-root users, offline builds). For a production
entrypoint, `rig launch` runs the built artifact with Rig's JVM flag set —
the Rig image is both build base and runtime, or the jar alone + a JRE in
a slim image.

## Local development

Prerequisites: JDK 21+, Clojure CLI, Go 1.27+, make.

```
make dev    # build the kernel jar, build rig
make test   # make dev + kernel kaocha suite + Go suite (E2E vs the fresh jar)
make run WS=<workspace-dir> ARGS="lock"   # run the local rig in a workspace
make image  # build the Docker image locally (no push)
make release V=vX.Y.Z  # package a release in rig/dist/release/ (dry run)
```

`make dev` is the whole loop: `resolver/target/rig-resolver-<V>.jar` and
`resolver/target/rig-runner-<V>.jar` are built (tools.build) and
`rig/dist/rig` is built. `V` is the release tag
form (vX.Y.Z) in both local and release builds, defaulting to the
contents of `resolver/VERSION` locally. The kernel's identity (version, git sha) is baked into the jar
at build time, so a lockfile's `resolver` block records the exact kernel
that produced it. The kernel pin in
`rig/internal/kernel/kernel.go` is a
release-time artifact — `make pin` stamps it (version, git sha, GitHub
release URL, jar sha256) and the release workflow pushes it to main, so
feature branches do not commit pin changes.

How the kernel jar and runner jar reach the Rig binary:

- Local: `RIG_KERNEL_JAR=<absolute jar path>` and
  `RIG_RUNNER_JAR=<absolute runner jar path>` — trusted local overrides,
  used in place of the downloaded artifacts (no hash check). The two
  travel together: a local kernel without a local runner is an error.
- Published: without the overrides, Rig fetches the pinned kernel and
  runner from the GitHub release assets into its cache and hash-verifies
  them against the `JARSHA`/`RunnerSHA` pins. The pin is stamped per
  release by the release workflow, which pushes it to main. The runner
  jar is an install-time artifact: the Docker image ships it pre-seeded
  (the store stays read-only), and Rig never extracts it at runtime.
- `make run` sets both for you; other invocations of `rig/dist/rig` need
  them exported.
- The Go E2E tests find the jars via `RIG_TEST_KERNEL_JAR` /
  `RIG_TEST_RUNNER_JAR` (set by `make test`) or
  `resolver/target/rig-resolver-v0.3.0.jar` /
  `resolver/target/rig-runner-v0.3.0.jar` and **skip** (not fail) when
  they are missing — build them first (`make kernel`) or they silently
  don't run.

## License

MIT — see [LICENSE](LICENSE).
