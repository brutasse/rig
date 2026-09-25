# Local development: build the kernel jar, pin its sha256 into the rig
# executor, build rig, and run the test suites. See README.md,
# "Local development".
#
# V: version for the kernel jar and release artifacts. Local dev defaults to
# 0.1.0 (what the local tests look for); the release workflow passes the tag.

V       ?= 0.1.0
REPO    := brutasse/rig
JAR     := resolver/target/rig-resolver-$(V).jar
BIN     := rig/dist/rig
PINFILE := rig/internal/kernel/kernel.go
GITSHA  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

.DEFAULT_GOAL := dev

# dev: the whole local story in one command.
dev: kernel pin rig

# kernel: build the resolver uberjar (resolver/target/).
kernel:
	cd resolver && RIG_RESOLVER_VERSION=$(V) clojure -X:build

# pin: stamp the kernel pin in kernel.go (version, git sha, GitHub release
# URL, jar sha256) from the freshly built jar. Idempotent; fails loudly if a
# field line is ever renamed.
pin: kernel
	sha=$$(sha256sum $(JAR) | cut -d' ' -f1) && \
	gitsha=$$(git rev-parse HEAD) && \
	sed -i -e "s/\(Version:[[:space:]]*\"\)[0-9][0-9A-Za-z.+-]*/\1$(V)/" \
	        -e "s/\(GitSHA:[[:space:]]*\"\)[0-9a-f]*/\1$$gitsha/" \
	        -e "s|\(URL:[[:space:]]*\"\)[^\"]*|\1https://github.com/$(REPO)/releases/download/$(V)/rig-resolver-$(V).jar|" \
	        -e "s/\(JARSHA:[[:space:]]*\"\)[0-9a-f]*/\1$$sha/" $(PINFILE) && \
	grep -qF "$$sha" $(PINFILE)

# rig: build the rig binary (rig/dist/rig).
rig:
	cd rig && go build -o dist/rig ./cmd/rig

# test: full local verification — kernel kaocha suite + Go suite
# (E2E tests run against the just-built jar).
test: dev
	cd resolver && CLOJURE_CLI_ALLOW_HTTP_REPO=1 clojure -X:test
	cd rig && RIG_TEST_KERNEL_JAR=$(abspath $(JAR)) go test ./...

# test-pier: Pier E2E — rig lock/publish/verify against a live OIDC-gated
# Maven repo. The Pier test env must be running (see the checkout's .scratch:
# fake OIDC issuer on :8090, pier on :8089, S3 on :9000).
# Usage: make test-pier PIER=/path/to/pier-checkout
PIER ?= $(HOME)/code/brutasse/rig-s3
test-pier:
	cd rig && RIG_TEST_PIER=$(abspath $(PIER)) RIG_TEST_KERNEL_JAR=$(abspath $(JAR)) \
		go test -count=1 -v -run 'TestPier' ./internal/cli/

# run: execute the locally built rig in a workspace.
# Usage: make run WS=<workspace-dir> ARGS="lock"
run:
	@test -f $(JAR) && test -f $(BIN) || { echo "make dev first (missing kernel jar or rig binary)"; exit 1; }
	cd $(or $(WS),.) && RIG_KERNEL_JAR=$(abspath $(JAR)) $(abspath $(BIN)) $(ARGS)

# release: package a release — kernel jar, stamped pin, cross-compiled
# binaries, SHA256SUMS — in rig/dist/release/. The release workflow
# publishes that directory; locally it doubles as a packaging dry-run.
# Usage: make release V=0.2.0
release: kernel pin release-binaries
	mkdir -p rig/dist/release
	cp rig/dist/rig-linux-amd64 rig/dist/rig-linux-arm64 rig/dist/rig-darwin-amd64 rig/dist/rig-darwin-arm64 rig/dist/release/
	cp $(JAR) rig/dist/release/
	cd rig/dist/release && sha256sum rig-linux-amd64 rig-linux-arm64 rig-darwin-amd64 rig-darwin-arm64 rig-resolver-$(V).jar > SHA256SUMS
	@echo "release artifacts in rig/dist/release/ — publish with:"
	@echo "  (cd rig/dist/release && sha256sum -c SHA256SUMS) && gh release create $(V) rig/dist/release/*"

# release-binaries: cross-compile rig for the release matrix with version
# stamps (Version, GitSHA via ldflags). Each artifact is checked against its
# declared architecture: a wrong-arch binary would be packaged silently and
# break the cross-arch Docker build.
release-binaries:
	@for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
		os=$${t%%/*}; arch=$${t#*/}; \
		(cd rig && GOOS=$$os GOARCH=$$arch go build -trimpath \
			-ldflags "-s -w -X github.com/brutasse/rig/internal/cli.Version=$(V) -X github.com/brutasse/rig/internal/cli.GitSHA=$(GITSHA)" \
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
# Usage: make image           # tags $(IMAGE):local
image: release
	arch=$$(uname -m); \
	case "$$arch" in \
		x86_64) arch=amd64 ;; \
		aarch64) arch=arm64 ;; \
		*) echo "unsupported arch: $$arch" >&2; exit 1 ;; \
	esac; \
	docker build \
		--build-arg V=$(V) \
		--build-arg KERNEL_GITSHA=$$(git rev-parse HEAD) \
		--build-arg JARSHA=$$(sha256sum rig/dist/release/rig-resolver-$(V).jar | cut -d' ' -f1) \
		--build-arg TARGETARCH=$$arch \
		-t $(IMAGE):local .

.PHONY: dev kernel pin rig test test-pier run release release-binaries image
