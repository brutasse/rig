# Rig Docker image: the rig binary + its hash-pinned resolver kernel jar
# and rig.runner jar (both install-time artifacts, pre-seeded into the
# store at the exact paths the binary's pin expects, so the image works
# with a read-only root file system).
#
# Built by the release workflow (and `make image`) from the `make release`
# artifacts in rig/dist/release/. The binary is the cross-compiled build for
# TARGETARCH; each jar is hash-verified against its build arg at build time.
#
# Use it two ways:
#
#   FROM ghcr.io/brutasse/rig:latest            # standalone (Temurin 21 + rig)
#
#   COPY --from=ghcr.io/brutasse/rig:latest /usr/local/bin/rig /usr/local/bin/
#   COPY --from=ghcr.io/brutasse/rig:latest /root/.local/share/rig /root/.local/share/rig

FROM eclipse-temurin:21-jdk-jammy

# TARGETARCH is set per-platform by buildx; plain `docker build` injects the
# host arch. No default: one would override the per-platform value in every
# stage and silently select the wrong binary.
ARG TARGETARCH
ARG V
ARG KERNEL_GITSHA
ARG JARSHA
ARG RUNNERSHA

COPY rig/dist/release/rig-linux-${TARGETARCH} /usr/local/bin/rig
COPY rig/dist/release/rig-resolver-${V}.jar /kernel.jar
COPY rig/dist/release/rig-runner-${V}.jar /runner.jar

RUN set -eux; \
    chmod 0755 /usr/local/bin/rig; \
    install -d -o root -g root /root/.local/share/rig/kernel/${KERNEL_GITSHA} \
                            /root/.local/share/rig/runner/${KERNEL_GITSHA}; \
    cp /kernel.jar /root/.local/share/rig/kernel/${KERNEL_GITSHA}/rig-resolver.jar; \
    cp /runner.jar /root/.local/share/rig/runner/${KERNEL_GITSHA}/rig-runner.jar; \
    rm -f /kernel.jar /runner.jar; \
    echo "${JARSHA}  /root/.local/share/rig/kernel/${KERNEL_GITSHA}/rig-resolver.jar" \
        | sha256sum -c -; \
    echo "${RUNNERSHA}  /root/.local/share/rig/runner/${KERNEL_GITSHA}/rig-runner.jar" \
        | sha256sum -c -; \
    rig jvm list
