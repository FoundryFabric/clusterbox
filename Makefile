# clusterbox build / lint / release targets.
#
# Usage:
#   make build       Build the host-platform binary into ./bin/clusterbox
#   make lint        Run golangci-lint (must be installed on PATH)
#   make test        Run the standard test suite
#   make fmt         Format Go code in place
#   make fmt-check   Verify code is gofmt-clean without modifying anything
#   make rel         Cross-compile release binaries for linux+darwin (amd64+arm64)
#   make clean       Remove ./bin and ./dist
#   make help        Print this list

BINARY  := clusterbox
PKG     := github.com/foundryfabric/clusterbox
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(PKG)/cmd.version=$(VERSION)

# CGO is intentionally disabled. The SQLite driver (modernc.org/sqlite) is
# pure Go, so cross-compilation works without a C toolchain on the host.
GOFLAGS := CGO_ENABLED=0

.PHONY: build
build:
	$(GOFLAGS) go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) .

.PHONY: lint
lint:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not found. Install: https://golangci-lint.run/welcome/install/"; \
		exit 1; \
	}
	golangci-lint run ./...

.PHONY: test
test:
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
rel: dist/$(BINARY)-linux-amd64 dist/$(BINARY)-linux-arm64 dist/$(BINARY)-darwin-amd64 dist/$(BINARY)-darwin-arm64
	@echo "Release artifacts in ./dist:"
	@ls -lh dist/

dist/$(BINARY)-linux-amd64:
	GOOS=linux GOARCH=amd64 $(GOFLAGS) go build -ldflags "$(LDFLAGS)" -o $@ .

dist/$(BINARY)-linux-arm64:
	GOOS=linux GOARCH=arm64 $(GOFLAGS) go build -ldflags "$(LDFLAGS)" -o $@ .

dist/$(BINARY)-darwin-amd64:
	GOOS=darwin GOARCH=amd64 $(GOFLAGS) go build -ldflags "$(LDFLAGS)" -o $@ .

dist/$(BINARY)-darwin-arm64:
	GOOS=darwin GOARCH=arm64 $(GOFLAGS) go build -ldflags "$(LDFLAGS)" -o $@ .

.PHONY: clean
clean:
	rm -rf bin/ dist/

.PHONY: help
help:
	@grep -E '^#   make' Makefile | sed -e 's/^# *//'
