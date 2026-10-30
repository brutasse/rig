# Rig Docker image: the rig binary, with its resolver kernel jar, rig.runner
# jar, and vendored nREPL jar embedded (go:embed) — nothing to pre-seed.
# First use extracts the jars into the state dir, so the state dir must be
# writable; after that the store works read-only.
#
# Built by the release workflow (and `make image`) from the `make release`
# artifacts in rig/dist/release/. The binary is the cross-compiled build for
# TARGETARCH.
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

RUN set -eux; \
    chmod 0755 /usr/local/bin/rig; \
    rig jvm list
