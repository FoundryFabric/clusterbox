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
#   make agents           Just rebuild the embedded clusterboxnode binaries
#   make k3s-assets       Download k3s binaries into internal/node/k3s/assets/
#                         (required before building with -tags k3s_assets)
#   make tailscale-assets Fetch tailscale/tailscaled binaries into internal/node/tailscale/assets/
#                         (required before building with -tags tailscale_assets)
#   make clean            Remove ./bin, ./dist, embedded agent binaries, and k3s/tailscale assets
#   make help             Print this list

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
             $(shell find internal/node -path '*/conf/*' 2>/dev/null) \
             $(shell find internal/node/k3s/assets -type f 2>/dev/null)

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

# K3S_VERSION selects the k3s release to fetch; falls back to the pinned default.
K3S_VERSION ?= v1.32.3+k3s1
K3S_ASSETS_DIR := internal/node/k3s/assets

.PHONY: k3s-assets
k3s-assets:
	@mkdir -p $(K3S_ASSETS_DIR)
	@echo "Downloading k3s $(K3S_VERSION) (amd64)..."
	@curl -fSL --retry 3 \
		"https://github.com/k3s-io/k3s/releases/download/$(subst +,%2B,$(K3S_VERSION))/k3s" \
		-o $(K3S_ASSETS_DIR)/k3s-linux-amd64
	@chmod +x $(K3S_ASSETS_DIR)/k3s-linux-amd64
	@echo "Downloading k3s $(K3S_VERSION) (arm64)..."
	@curl -fSL --retry 3 \
		"https://github.com/k3s-io/k3s/releases/download/$(subst +,%2B,$(K3S_VERSION))/k3s-arm64" \
		-o $(K3S_ASSETS_DIR)/k3s-linux-arm64
	@chmod +x $(K3S_ASSETS_DIR)/k3s-linux-arm64
	@printf '%s' "$(K3S_VERSION)" > $(K3S_ASSETS_DIR)/k3s.version.tmp
	@mv $(K3S_ASSETS_DIR)/k3s.version.tmp $(K3S_ASSETS_DIR)/k3s.version
	@echo "k3s assets written to $(K3S_ASSETS_DIR)/ (version: $(K3S_VERSION))"

.PHONY: clean
clean:
	rm -rf bin/ dist/
	rm -f $(AGENT_BINS)
	rm -f $(K3S_ASSETS_DIR)/k3s-linux-amd64 $(K3S_ASSETS_DIR)/k3s-linux-arm64

# tailscale-assets — download tailscale and tailscaled binaries for both arches
# from pkgs.tailscale.com and place them under internal/node/tailscale/assets/
# so that a subsequent build with -tags tailscale_assets embeds them.
#
# Usage:
#   make tailscale-assets                       (uses default version)
#   TAILSCALE_VERSION=1.80.0 make tailscale-assets
TAILSCALE_VERSION ?= 1.78.0
TAILSCALE_ASSET_DIR := internal/node/tailscale/assets

.PHONY: tailscale-assets
tailscale-assets:
	@echo "Fetching Tailscale $(TAILSCALE_VERSION) binaries..."
	@mkdir -p $(TAILSCALE_ASSET_DIR)
	@for arch in amd64 arm64; do \
		url="https://pkgs.tailscale.com/stable/tailscale_$(TAILSCALE_VERSION)_linux_$${arch}.tgz"; \
		tmpdir=$$(mktemp -d); \
		trap "rm -rf $$tmpdir" EXIT; \
		echo "  downloading $${url}"; \
		curl -fsSL "$${url}" | tar -xz -C "$${tmpdir}"; \
		ts=$$(find "$${tmpdir}" -name tailscale  -not -name tailscaled -type f | head -1); \
		tsd=$$(find "$${tmpdir}" -name tailscaled -type f | head -1); \
		if [ -z "$$ts" ] || [ -z "$$tsd" ]; then \
			echo "ERROR: could not locate tailscale/tailscaled in $${tmpdir}"; \
			rm -rf "$${tmpdir}"; \
			exit 1; \
		fi; \
		cp "$$ts"  "$(TAILSCALE_ASSET_DIR)/tailscale-linux-$${arch}"; \
		cp "$$tsd" "$(TAILSCALE_ASSET_DIR)/tailscaled-linux-$${arch}"; \
		rm -rf "$${tmpdir}"; \
		echo "  wrote tailscale-linux-$${arch} and tailscaled-linux-$${arch}"; \
	done
	@tmpver=$(TAILSCALE_ASSET_DIR)/tailscale.version.tmp; \
	printf '%s' "$(TAILSCALE_VERSION)" > "$$tmpver"; \
	mv "$$tmpver" "$(TAILSCALE_ASSET_DIR)/tailscale.version"
	@echo "Tailscale assets ready (version $(TAILSCALE_VERSION))"

.PHONY: help
help:
	@grep -E '^#   make' Makefile | sed -e 's/^# *//'
