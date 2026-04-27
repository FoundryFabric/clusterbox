# clusterbox build / lint / release targets.
#
# Usage:
#   make build       Build the host-platform clusterbox binary into ./bin/
#                    Cross-compiles clusterboxnode for linux/{amd64,arm64}
#                    first and embeds it via internal/agentbundle.
#   make lint        Run golangci-lint (must be installed on PATH)
#   make test        Run the standard test suite
#   make fmt         Format Go code in place
#   make fmt-check   Verify code is gofmt-clean without modifying anything
#   make rel         Cross-compile release binaries for linux+darwin (amd64+arm64)
#                    plus standalone clusterboxnode-linux-{amd64,arm64} into ./dist/
#   make agents      Just rebuild the embedded clusterboxnode binaries
#   make clean       Remove ./bin, ./dist, and the embedded agent binaries
#   make help        Print this list

BINARY  := clusterbox
PKG     := github.com/foundryfabric/clusterbox
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# clusterboxnode is deployed to remote Linux nodes and must be a fully static
# binary — CGO disabled so no C toolchain is required on the build host.
#
# clusterbox is the developer CLI. It runs on the developer's own machine
# and links the 1Password Go SDK, which requires CGO (Rust FFI core).
# CGO is enabled by default; cross-compilation for linux release targets
# requires musl cross-compilers (see: docs/cross-compile.md).
NODE_GOFLAGS := CGO_ENABLED=0

# The version stamp is wired into TWO places:
#   - cmd.version       (the clusterbox CLI's --version output)
#   - agentbundle.version (the version reported by the embedded agent bundle)
# Both must match. agentbundle_test.go enforces this in CI.
LDFLAGS := -s -w \
	-X $(PKG)/cmd.version=$(VERSION) \
	-X $(PKG)/internal/agentbundle.version=$(VERSION)

# clusterboxnode is built with only its own version stamp.
NODE_LDFLAGS := -s -w -X main.version=$(VERSION)

# -trimpath gives reproducible builds: same commit ⇒ same binary bytes,
# regardless of the developer's $GOPATH or working directory.
GOBUILD     := go build -trimpath
AGENTBUILD  := $(NODE_GOFLAGS) go build -trimpath

AGENT_DIR  := internal/agentbundle/agents
AGENT_BINS := $(AGENT_DIR)/clusterboxnode-linux-amd64 $(AGENT_DIR)/clusterboxnode-linux-arm64

# Source files that, when changed, must trigger a rebuild of the embedded
# clusterboxnode binaries. Wrapped in $(shell ...) so Make re-evaluates the
# list each invocation; the 2>/dev/null swallows the case where a directory
# doesn't yet exist on a fresh clone.
NODE_SRCS := $(shell find cmd/clusterboxnode internal/node -name '*.go' 2>/dev/null) \
             $(shell find internal/node -path '*/conf/*' 2>/dev/null)

.PHONY: build
build: $(AGENT_BINS)
	$(GOBUILD) -ldflags "$(LDFLAGS)" -o bin/$(BINARY) .

.PHONY: agents
agents: $(AGENT_BINS)

# Cross-compile clusterboxnode for the embed slot. Both target rules share
# the same recipe modulo GOARCH; the explicit rules (rather than a pattern
# rule) make the dependency graph easy to reason about.
$(AGENT_DIR)/clusterboxnode-linux-amd64: $(NODE_SRCS) | $(AGENT_DIR)
	GOOS=linux GOARCH=amd64 $(AGENTBUILD) -ldflags "$(NODE_LDFLAGS)" -o $@ ./cmd/clusterboxnode

$(AGENT_DIR)/clusterboxnode-linux-arm64: $(NODE_SRCS) | $(AGENT_DIR)
	GOOS=linux GOARCH=arm64 $(AGENTBUILD) -ldflags "$(NODE_LDFLAGS)" -o $@ ./cmd/clusterboxnode

$(AGENT_DIR):
	mkdir -p $@

.PHONY: lint
lint: $(AGENT_BINS)
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not found. Install: https://golangci-lint.run/welcome/install/"; \
		exit 1; \
	}
	golangci-lint run ./...

.PHONY: test
test: $(AGENT_BINS)
	go test ./...

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: fmt-check
fmt-check:
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then \
		echo "gofmt would change these files:"; \
		echo "$$out"; \
		exit 1; \
	fi

.PHONY: rel
rel: $(AGENT_BINS) \
     dist/$(BINARY)-linux-amd64 dist/$(BINARY)-linux-arm64 \
     dist/$(BINARY)-darwin-amd64 dist/$(BINARY)-darwin-arm64 \
     dist/clusterboxnode-linux-amd64 dist/clusterboxnode-linux-arm64
	@echo "Release artifacts in ./dist:"
	@ls -lh dist/

dist/$(BINARY)-linux-amd64: $(AGENT_BINS) | dist
	GOOS=linux GOARCH=amd64 $(GOBUILD) -ldflags "$(LDFLAGS)" -o $@ .

dist/$(BINARY)-linux-arm64: $(AGENT_BINS) | dist
	GOOS=linux GOARCH=arm64 $(GOBUILD) -ldflags "$(LDFLAGS)" -o $@ .

dist/$(BINARY)-darwin-amd64: $(AGENT_BINS) | dist
	GOOS=darwin GOARCH=amd64 $(GOBUILD) -ldflags "$(LDFLAGS)" -o $@ .

dist/$(BINARY)-darwin-arm64: $(AGENT_BINS) | dist
	GOOS=darwin GOARCH=arm64 $(GOBUILD) -ldflags "$(LDFLAGS)" -o $@ .

# The standalone clusterboxnode artefacts in ./dist/ are byte-identical to
# the ones embedded under internal/agentbundle/agents/; copying avoids a
# second build.
dist/clusterboxnode-linux-amd64: $(AGENT_DIR)/clusterboxnode-linux-amd64 | dist
	cp $< $@

dist/clusterboxnode-linux-arm64: $(AGENT_DIR)/clusterboxnode-linux-arm64 | dist
	cp $< $@

dist:
	mkdir -p $@

.PHONY: clean
clean:
	rm -rf bin/ dist/
	rm -f $(AGENT_BINS)

.PHONY: help
help:
	@grep -E '^#   make' Makefile | sed -e 's/^# *//'
