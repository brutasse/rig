# Docker

Rig ships a Docker image, `ghcr.io/brutasse/rig`, built by the release job
for every release — tags `vX.Y.Z` and `latest`, multi-arch
(`linux/amd64`, `linux/arm64`).

The image contains:

- the `rig` binary at `/usr/local/bin/rig`;
- the resolver kernel jar, **pre-baked** into the Rig state dir at the exact
  path the binary's pin expects
  (`/root/.local/share/rig/kernel/<git-sha>/rig-resolver.jar`). The release
  jar, hash-verified — no GitHub fetch on first use;
- the rig.runner jar, **pre-baked** the same way
  (`/root/.local/share/rig/runner/<git-sha>/rig-runner.jar`) — hot commands
  append it to the project classpath, so `test` and `check` stage 2 need it;
- a Temurin 21 JDK, so the image is a standalone Clojure base.

## As an app base

For a dev container or CI job where `rig` must work from the first
layer: the kernel and runner jars are pre-baked in the image, so `rig`
fetches nothing from GitHub — only your artifact repositories.

```dockerfile
FROM ghcr.io/brutasse/rig:latest
WORKDIR /app
COPY . .
RUN rig verify --frozen && rig test --frozen
```

## As an app entrypoint

`rig launch` is the production entrypoint: it runs the built artifact
with Rig's production JVM flag set (G1, exit-on-OOM, loopback-only JMX
on 10101) — overridable per module via `:rig/launch-opts` and per
deployment via the `RIG_LAUNCH_OPTS` env var — and replaces its own
process with the JVM, so the app is the container's PID 1. It is
offline by definition, and the launch plan is baked into the artifact
by `rig build`. The full workflow — declaring launch opts, the Docker
images (slim and workspace variants), process & lifecycle, and
per-deployment overrides — is in
[Production launch](production-launch.md).

## Multi-stage

When your app image has its own base, copy the Rig pieces out of the image:

```dockerfile
FROM eclipse-temurin:21-jdk-jammy
WORKDIR /app
COPY --from=ghcr.io/brutasse/rig:latest /usr/local/bin/rig /usr/local/bin/
COPY --from=ghcr.io/brutasse/rig:latest /root/.local/share/rig /root/.local/share/rig
COPY . .
RUN rig verify --frozen && rig test --frozen
```

The second `COPY` brings the pre-baked kernel and runner jars with it, so `rig` needs no
GitHub access at runtime — it only talks to your artifact repositories. The build
consumes the committed `deps.lock`: `rig verify --frozen` fails when it is missing
or stale; locking is workstation work.

## Pinned JVMs

Base the image on the declared JDK — `:rig/jvm "21"` in the root
`deps.edn` becomes `eclipse-temurin:21-jdk` (the jammy variant above) —
and the build compiles with the image's JDK: no rig-managed JDK for the
pin is installed in the copied state dir, so Rig falls back to the
system `java`, which satisfies the pin. There is no `RUN rig jvm
install` step — the image's JDK *is* the build JVM. When the base image
carries no matching JDK, the build downloads the newest matching
release on demand (`--offline` fails instead).

The runtime side of the same pin — the app's container base must carry
that JDK — is in
[Production launch](production-launch.md#the-docker-image).

## Non-root users

The state dir is under `/root`. For a non-root build user, copy it to that
user's home, or point every invocation at a shared dir with
`--cache-dir /opt/rig-cache` (it must be writable).

## Offline builds

The kernel and runner jars are in the image, but the *artifact* cache is
cold: `rig verify` in the Dockerfile fetches from your repositories
(hash-checked against the lock). For hermetic `--offline` builds, warm the
state dir in an earlier layer (or cache it across runs) first — see
[CI](ci.md#hermetic-air-gapped-ci-opt-in-offline).
