# Local development: build the kernel jar, build rig, and run the test
# suites. The kernel, runner, and nREPL jars ship embedded in the rig
# binary: 'make rig' copies the built jars into internal/kernel/embed/
# and go:embed pulls them in. See README.md, "Local development".
#
# First build on a clean tree: the committed embed jars for the kernel
# and the runner are 0-byte placeholders, so the placeholder rig cannot
# run kernel ops. 'make kernel' then builds the resolver with the rig on
# PATH (a release binary, as 'lock-refresh' does); every later build
# self-hosts on dist/rig, which embeds the previous cycle's jars.
#
# V: version for the kernel jar and release artifacts, in release tag form
# (vX.Y.Z). Local dev defaults to the contents of resolver/VERSION (what
# the local tests look for). The kernel builds are stamped from that file
# (the rig build reads it), so V must equal it: 'make release' stamps it
# from the tag itself (the 'prepare' target), and the release workflow
# pushes that stamp to main after publishing.

V       ?= $(shell cat resolver/VERSION 2>/dev/null || echo v0.1.0)
REPO    := brutasse/rig
JAR     := resolver/target/rig-resolver-$(V).jar
RUNNER  := resolver/runner/target/rig-runner-$(V).jar
BIN     := rig/dist/rig
RIG     ?= rig
EMBED   := rig/internal/kernel/embed
GITSHA  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
# The rig that builds the kernel jars: the embedded one once a build has
# filled it, the release rig on PATH for the bootstrap of a clean tree.
KBOOT    = $(shell test -s $(EMBED)/rig-resolver.jar && echo "$(abspath $(BIN))" || echo "$(RIG)")

.DEFAULT_GOAL := dev

# dev: the whole local story in one command.
dev: kernel rig

# kernel: build the resolver uberjar and the runner jar (resolver/target/
# and resolver/runner/target/) with rig — self-hosted: the rig binary
# runs its embedded kernel, so the previous cycle's kernel builds the next
# one. The builder's op shapes the artifact, so an op fix must be
# released before it can shape its successor's build (v0.3.0 shipped the
# link-closure fix precisely so it could build what comes next). The jar
# version comes from resolver/VERSION (rig's version-file), so V must
# match it; 'make release' stamps VERSION from the tag first ('prepare').
# --frozen: this workspace commits its lock, so the lock is a build input. A
# cold command without it re-resolves and rewrites deps.lock whenever a
# manifest moved: a dependency bump that skipped 'rig lock' would build green
# here and leave the drift uncommitted instead of failing.
kernel: rig
	@test "$$(cat resolver/VERSION)" = "$(V)" || { \
		echo "resolver/VERSION ($$(cat resolver/VERSION)) != V ($(V)): run 'make prepare V=$(V)'" >&2; exit 1; }
	cd resolver && $(KBOOT) build --frozen --uber
	cd resolver && $(KBOOT) build -p runner --frozen

# rig: build the rig binary (rig/dist/rig) with the local kernel jars
# embedded (a clean tree keeps the committed placeholders until 'make
# kernel' produces the first ones; 'make dev' runs both in order).
rig:
	test ! -f $(JAR) || cp $(JAR) $(EMBED)/rig-resolver.jar
	test ! -f $(RUNNER) || cp $(RUNNER) $(EMBED)/rig-runner.jar
	cd rig && go build -o dist/rig ./cmd/rig

# lock-refresh: re-bake resolver/deps.lock from the manifests. One 'rig lock'
# at the workspace root covers every module of the workspace — it rewrites the
# root and the runner module in the same file, so there is no '-p runner'
# variant and no second lock. It runs the rig on PATH, not a build of these
# sources: the lock records which rig wrote it, and the CI re-lock on
# renovate/* branches should stamp a release there, not a dev build of the
# branch it is fixing.
lock-refresh:
	cd resolver && $(RIG) lock

# check: rig's own workspace against its own lock. The frozen build already
# refuses a stale lock; this names the coordinate that drifted, runs the rest
# of stage 1 (conflicts, drift, floating versions, unknown repos) and stage 2
# (every module's namespaces load and compile).
check: dev
	cd resolver && $(abspath $(BIN)) check

# test: full local verification — rig check on this workspace, the kernel
# suite (rig test: the runner runs the :test alias's exec-fn on the locked
# test classpath, on the workspace's managed Temurin 8) + Go suite (E2E tests
# run against the just-built jars). No Clojure CLI, no host JDK: rig runs
# everything.
test: dev check
	cd resolver && CLOJURE_CLI_ALLOW_HTTP_REPO=1 $(abspath $(BIN)) test
	cd rig && RIG_TEST_KERNEL_JAR=$(abspath $(JAR)) RIG_TEST_RUNNER_JAR=$(abspath $(RUNNER)) \
		go test ./...

# test-pier: Pier E2E — rig lock/publish/verify against a live OIDC-gated
# Maven repo. The Pier test env must be running (see the checkout's .scratch:
# fake OIDC issuer on :8090, pier on :8089, S3 on :9000).
# Usage: make test-pier PIER=/path/to/pier-checkout
PIER ?= $(HOME)/code/brutasse/rig-s3
test-pier:
	cd rig && RIG_TEST_PIER=$(abspath $(PIER)) RIG_TEST_KERNEL_JAR=$(abspath $(JAR)) \
		go test -count=1 -v -run 'TestPier' ./internal/cli/

# test-native: native-image E2E — builds the testdata/native fixture into a
# GraalVM native binary and runs it. GRAALVM must point at a GraalVM home
# (bin/native-image); the target fails with a hint when it is unset.
# Usage: make test-native GRAALVM=/opt/graalvm
GRAALVM ?=
test-native:
	@test -n "$(GRAALVM)" || { echo "set GRAALVM=/path/to/graalvm"; exit 1; }
	cd rig && RIG_TEST_GRAALVM=$(abspath $(GRAALVM)) RIG_TEST_KERNEL_JAR=$(abspath $(JAR)) \
		go test -count=1 -v -timeout 30m -run 'TestHotBuildNative' ./internal/cli/

# run: execute the locally built rig in a workspace.
# Usage: make run WS=<workspace-dir> ARGS="lock"
run:
	@test -f $(JAR) && test -f $(RUNNER) && test -f $(BIN) \
		|| { echo "make dev first (missing kernel jar, runner jar, or rig binary)"; exit 1; }
	cd $(or $(WS),.) && RIG_KERNEL_JAR=$(abspath $(JAR)) RIG_RUNNER_JAR=$(abspath $(RUNNER)) $(abspath $(BIN)) $(ARGS)

# prepare: stamp V into the version file and re-bake the lock, so the
# module version snapshot and the jar names follow the tag. 'release' runs
# it first: pushing a tag is the whole release ceremony, no prepare-release
# commit. The rig on PATH writes the lock (a release build, per
# lock-refresh). The kernel gate stays strict: a dev build whose VERSION
# drifted from the lock still fails.
prepare:
	echo "$(V)" > resolver/VERSION
	cd resolver && $(RIG) lock

# release: package a release — version stamp (prepare), kernel jar, runner
# jar, cross-compiled binaries with the jars and version stamps embedded,
# SHA256SUMS — in rig/dist/release/. The release workflow publishes that
# directory and lands the version stamp on main; locally it doubles as a
# packaging dry-run (clean the worktree afterwards: VERSION, deps.lock).
# The kernel and runner jars are still published as release assets: older
# rig binaries (pre-embedding) fetch them at runtime.
# Usage: make release V=vX.Y.Z
release: prepare kernel release-binaries
	mkdir -p rig/dist/release
	cp rig/dist/rig-linux-amd64 rig/dist/rig-linux-arm64 rig/dist/rig-darwin-amd64 rig/dist/rig-darwin-arm64 rig/dist/release/
	cp $(JAR) rig/dist/release/
	cp $(RUNNER) rig/dist/release/
	cd rig/dist/release && sha256sum rig-linux-amd64 rig-linux-arm64 rig-darwin-amd64 rig-darwin-arm64 rig-resolver-$(V).jar rig-runner-$(V).jar > SHA256SUMS
	@echo "release artifacts in rig/dist/release/ — publish with:"
	@echo "  (cd rig/dist/release && sha256sum -c SHA256SUMS) && gh release create $(V) rig/dist/release/*"

# release-binaries: cross-compile rig for the release matrix with version
# stamps (cli and kernel identity via ldflags; the kernel build ran first,
# so the embedded jars are the ones built from this tag). Each artifact is
# checked against its declared architecture: a wrong-arch binary would be
# packaged silently and break the cross-arch Docker build.
release-binaries:
	test ! -f $(JAR) || cp $(JAR) $(EMBED)/rig-resolver.jar
	test ! -f $(RUNNER) || cp $(RUNNER) $(EMBED)/rig-runner.jar
	@for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
		os=$${t%%/*}; arch=$${t#*/}; \
		(cd rig && GOOS=$$os GOARCH=$$arch go build -trimpath \
			-ldflags "-s -w -X github.com/brutasse/rig/internal/cli.Version=$(V) -X github.com/brutasse/rig/internal/cli.GitSHA=$(GITSHA) -X github.com/brutasse/rig/internal/kernel.Version=$(V) -X github.com/brutasse/rig/internal/kernel.GitSHA=$(GITSHA)" \
			-o dist/rig-$$os-$$arch ./cmd/rig) || exit 1; \
		case "$$t" in \
		linux/amd64) want=x86-64 ;; \
		linux/arm64) want=aarch64 ;; \
		darwin/amd64) want=x86_64 ;; \
		darwin/arm64) want=arm64 ;; \
		esac; \
		file rig/dist/rig-$$os-$$arch | grep -q "$$want" \
			|| { echo "release-binaries: rig-$$os-$$arch: wrong architecture (want $$want)" >&2; file rig/dist/rig-$$os-$$arch >&2; exit 1; } \
	done

IMAGE ?= ghcr.io/brutasse/rig

# image: build the Docker image locally (no push) from the release artifacts.
# The rig binary embeds its kernel/runner/nREPL jars: the image is the
# binary alone, no store pre-seed.
# Usage: make image           # tags $(IMAGE):local
image: release
	arch=$$(uname -m); \
	case "$$arch" in \
		x86_64) arch=amd64 ;; \
		aarch64) arch=arm64 ;; \
		*) echo "unsupported arch: $$arch" >&2; exit 1 ;; \
	esac; \
	docker build \
		--build-arg TARGETARCH=$$arch \
		-t $(IMAGE):local .

.PHONY: dev kernel lock-refresh prepare rig check test test-pier run release release-binaries image
