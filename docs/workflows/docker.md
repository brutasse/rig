# Docker

rig ships a Docker image, `ghcr.io/brutasse/rig`, built by the release job
for every release — tags `vX.Y.Z` and `latest`, multi-arch
(`linux/amd64`, `linux/arm64`).

The image contains:

- the `rig` binary at `/usr/local/bin/rig`;
- the resolver kernel jar, **pre-baked** into the rig state dir at the exact
  path the binary's pin expects
  (`/root/.local/share/rig/kernel/<git-sha>/rig-resolver.jar`). The release
  jar, hash-verified — no GitHub fetch on first use;
- a Temurin 21 JDK, so the image is a standalone Clojure base.

## As an app base

```dockerfile
FROM ghcr.io/brutasse/rig:latest
WORKDIR /app
COPY . .
RUN rig lock
RUN rig verify --frozen && rig test --frozen
```

## As an app entrypoint

`rig launch` is the production entrypoint: it runs the built artifact with
rig's JVM flag set (G1, exit-on-OOM, loopback-only JMX on a single port,
10101), overridable per module via `:jvm-opts`. Two patterns:

### The workspace image

The rig image is both the build base and the runtime:

```dockerfile
FROM ghcr.io/brutasse/rig:latest
WORKDIR /app
COPY . .
RUN rig verify --frozen && rig build --uber --frozen -p modules/app
EXPOSE 10101
ENTRYPOINT ["rig", "launch"]
```

`CMD` arguments (e.g. `docker run img --env prod`) are passed to the app's
main; the jar comes from the lock. Launch never checks lock staleness — it
runs what was built, offline, with no state dir needed at runtime.

### The slim image

The artifact is self-describing (the launch plan is baked into the jar), so
the runtime needs only a JRE and the rig binary:

```dockerfile
FROM eclipse-temurin:21-jre-jammy
COPY --from=ghcr.io/brutasse/rig:latest /usr/local/bin/rig /usr/local/bin/
WORKDIR /app
COPY target/app-uber.jar /app/app.jar
EXPOSE 10101
ENTRYPOINT ["rig", "launch", "/app/app.jar"]
```

No workspace, no lock: `rig launch /app/app.jar` reads the jar's
`META-INF/rig/launch.json`. (The plain, non-uber jar needs the workspace —
its classpath lives in the lock.)

## Multi-stage

When your app image has its own base, copy the rig pieces out of the image:

```dockerfile
FROM eclipse-temurin:21-jdk-jammy
WORKDIR /app
COPY --from=ghcr.io/brutasse/rig:latest /usr/local/bin/rig /usr/local/bin/
COPY --from=ghcr.io/brutasse/rig:latest /root/.local/share/rig /root/.local/share/rig
COPY . .
RUN rig lock
RUN rig verify --frozen && rig test --frozen
```

The second `COPY` brings the pre-baked kernel jar with it, so `rig` needs no
GitHub access at runtime — it only talks to your artifact repositories.

## Pinned JVMs

If the project pins a JVM (`:rig/jvm` in the root `deps.edn`), pre-install
it so the build doesn't download it:

```dockerfile
RUN rig jvm install 21      # or the exact version from deps.lock
```

The JDK lands in the same state dir that was copied above.

## Non-root users

The state dir is under `/root`. For a non-root build user, copy it to that
user's home, or point every invocation at a shared dir with
`--cache-dir /opt/rig-cache` (it must be writable).

## Offline builds

The kernel jar is in the image, but the *artifact* cache is cold: `rig lock`
/ `rig verify` in the Dockerfile fetch from your repositories (hash-checked
against the lock). For hermetic `--offline` builds, warm the state dir in an
earlier layer (or cache it across runs) first — see
[CI](ci.md#hermetic-air-gapped-ci-opt-in-offline).
