# Rig Docker image: the rig binary, with its resolver kernel jar, rig.runner
# jar, and vendored nREPL jar embedded (go:embed) and pre-materialized into
# the store at their content-sha paths — no first-run extract, the image
# works with a read-only root file system for the tool jars. The artifact
# cache is still a runtime store: warm it in an earlier layer or mount it.
#
# Built by the release workflow (and `make image`) from the `make release`
# artifacts in rig/dist/release/ and the embed dir the binaries were built
# from: the binary is the cross-compiled build for TARGETARCH, the jars are
# the bytes it embeds.
#
# Use it two ways:
#
#   FROM ghcr.io/brutasse/rig:latest            # standalone (Temurin 21 + rig)
#
#   COPY --from=ghcr.io/brutasse/rig:latest /usr/local/bin/rig /usr/local/bin/

FROM eclipse-temurin:21-jdk-jammy

# TARGETARCH is set per-platform by buildx; plain `docker build` injects the
# host arch. No default: one would override the per-platform value in every
# stage and silently select the wrong binary.
ARG TARGETARCH

COPY rig/dist/release/rig-linux-${TARGETARCH} /usr/local/bin/rig
COPY rig/internal/kernel/embed/ /embed/

# Materialize the embedded jars into the store at their content-sha paths:
# materialize() adopts a cached copy when its digest matches, so a first use
# reads the store and writes nothing. The shas are computed from the baked
# bytes, so the paths are what the binary expects at runtime — no build
# args, nothing to keep in sync. test -s fails on the 0-byte placeholders a
# clean tree keeps in embed/.
RUN set -eux; \
    chmod 0755 /usr/local/bin/rig; \
    test -s /embed/rig-resolver.jar; \
    test -s /embed/rig-runner.jar; \
    test -s /embed/nrepl-1.8.0.jar; \
    k=$(sha256sum /embed/rig-resolver.jar | cut -d' ' -f1); \
    r=$(sha256sum /embed/rig-runner.jar | cut -d' ' -f1); \
    n=$(sha256sum /embed/nrepl-1.8.0.jar | cut -d' ' -f1); \
    install -d -o root -g root \
        /root/.local/share/rig/kernel/$k \
        /root/.local/share/rig/runner/$r \
        /root/.local/share/rig/nrepl/$n; \
    cp /embed/rig-resolver.jar /root/.local/share/rig/kernel/$k/rig-resolver.jar; \
    cp /embed/rig-runner.jar /root/.local/share/rig/runner/$r/rig-runner.jar; \
    cp /embed/nrepl-1.8.0.jar /root/.local/share/rig/nrepl/$n/nrepl.jar; \
    rm -rf /embed; \
    rig jvm list
