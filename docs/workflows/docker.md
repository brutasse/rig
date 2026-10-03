# Docker

Rig ships the Docker image `ghcr.io/brutasse/rig`. The release job builds
it for every release. The tags are `vX.Y.Z` and `latest`. The image is
multi-arch (`linux/amd64`, `linux/arm64`).

The image contains:

- the `rig` binary at `/usr/local/bin/rig`;
- the resolver kernel jar, **pre-baked** into the Rig state dir at the
  exact path the pin of the binary expects
  (`/root/.local/share/rig/kernel/<git-sha>/rig-resolver.jar`). It is the
  release jar, and Rig verified its hash. Rig fetches nothing from GitHub
  on first use;
- the rig.runner jar, **pre-baked** the same way
  (`/root/.local/share/rig/runner/<git-sha>/rig-runner.jar`). Hot commands
  append it to the project classpath, so `test` and stage 2 of `check` need
  it;
- a Temurin 21 JDK, so the image is a standalone Clojure base.

## As an app base

In a dev container or CI job, `rig` must work from the first layer. The
kernel and runner jars sit pre-baked in the image, so `rig` fetches nothing
from GitHub. It fetches only from your artifact repositories.

```dockerfile
FROM ghcr.io/brutasse/rig:latest
WORKDIR /app
COPY . .
RUN rig verify --frozen && rig test --frozen
```

## As an app entrypoint

`rig launch` is the production entrypoint. It runs the built artifact with
Rig's production JVM flag set: G1, exit-on-OOM, and loopback-only JMX on
10101. You can override the flags per module with `:rig/launch-opts` and
per deployment with the `RIG_LAUNCH_OPTS` env var. `rig launch` replaces
its own process with the JVM, so the app is the container's PID 1. It is
offline by definition. `rig build` bakes the launch plan into the
artifact. [Production launch](production-launch.md) covers the full
workflow: declaring launch opts, the Docker images (slim and workspace
variants), process and lifecycle, and per-deployment overrides.

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

The second `COPY` brings the pre-baked kernel and runner jars with it, so
`rig` needs no GitHub access at runtime. It only talks to your artifact
repositories. The build consumes the committed `deps.lock`:
`rig verify --frozen` fails when the lockfile is missing or stale. Locking
is workstation work.

## Pinned JVMs

Base the image on the declared JDK. The `:rig/jvm "21"` pin in the root
`deps.edn` becomes `eclipse-temurin:21-jdk` (the jammy variant above). The
build compiles with the JDK of the image. The copied state dir holds no
rig-managed JDK for the pin, so Rig falls back to the system `java`, which
satisfies the pin. There is no `RUN rig jvm install` step. The JDK of the
image *is* the build JVM. When the base image carries no matching JDK, the
build downloads the newest matching release on demand (`--offline` fails
instead).

The runtime side of the same pin is in
[Production launch](production-launch.md#the-docker-image): the base of
the app container must carry that JDK.

## Non-root users

The state dir is under `/root`. For a non-root build user, copy it to the
home dir of that user, or point every invocation at a shared dir with
`--cache-dir /opt/rig-cache`. The dir must be writable.

## Offline builds

The kernel and runner jars are in the image, but the *artifact* cache is
cold. `rig verify` in the Dockerfile fetches from your repositories and
checks the hashes against the lockfile. For hermetic `--offline` builds,
first warm the state dir in an earlier layer (or cache it across runs). See
[CI](ci.md#hermetic-air-gapped-ci-opt-in-offline).
