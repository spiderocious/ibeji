# Ìbejì — one repo, one version.
#
# The daemon, the CLI and the MCP server are three entrypoints on one Go module,
# so a version mismatch between the app and the API it consumes is impossible by
# construction. The Electron app is a client of the daemon's local HTTP API.

SHELL      := /bin/bash
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE       ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -s -w \
              -X main.version=$(VERSION) \
              -X main.commit=$(COMMIT) \
              -X main.buildDate=$(DATE)
BIN        := bin
GOFLAGS    ?=

.DEFAULT_GOAL := help

## help: list the targets
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //' | awk -F: '{printf "  \033[36m%-16s\033[0m%s\n", $$1, $$2}'

## build: build the daemon, CLI and MCP server
.PHONY: build
build: $(BIN)/ibeji-daemon $(BIN)/ibeji $(BIN)/ibeji-mcp

$(BIN)/ibeji-daemon: FORCE
	@mkdir -p $(BIN)
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $@ ./cmd/ibeji-daemon

$(BIN)/ibeji: FORCE
	@mkdir -p $(BIN)
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $@ ./cmd/ibeji

$(BIN)/ibeji-mcp: FORCE
	@mkdir -p $(BIN)
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $@ ./cmd/ibeji-mcp

## test: run the Go test suite with the race detector
.PHONY: test
test:
	go test -race -count=1 ./...

## test-short: skip the slow and keychain-touching tests
.PHONY: test-short
test-short:
	IBEJI_SKIP_KEYCHAIN_TESTS=1 go test -short -count=1 ./...

## cover: test with a coverage report
.PHONY: cover
cover:
	go test -race -count=1 -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | tail -1

## vet: go vet across the module
.PHONY: vet
vet:
	go vet ./...

## fmt: format and tidy
.PHONY: fmt
fmt:
	gofmt -s -w .
	go mod tidy

## check: what CI runs — fmt verification, vet, race tests
.PHONY: check
check: vet
	@test -z "$$(gofmt -s -l . | tee /dev/stderr)" || { echo 'gofmt -s -w . needed'; exit 1; }
	go test -race -count=1 ./...

## e2e: the end-to-end runs — pairing, exec, spaces, then the window
.PHONY: e2e
e2e: build
	./test/e2e-pair.sh
	./test/e2e-exec.sh
	./test/e2e-spaces.sh
	./test/e2e-app.sh

## smoke: build the window and check it against a running daemon
.PHONY: smoke
smoke:
	cd app && npm run smoke
	cd app && npm run modal-check

## app: install the Electron app's dependencies
.PHONY: app
app:
	cd app && npm install

## app-dev: run the Electron app against a running daemon
.PHONY: app-dev
app-dev:
	cd app && npm run dev

## run: build, then run the daemon in the foreground with debug logging
.PHONY: run
run: $(BIN)/ibeji-daemon
	IBEJI_LOG=debug $(BIN)/ibeji-daemon

## doctor: build the CLI and run the diagnostic
.PHONY: doctor
doctor: $(BIN)/ibeji
	$(BIN)/ibeji doctor

## clean: remove build output
.PHONY: clean
clean:
	rm -rf $(BIN) coverage.out

.PHONY: FORCE
FORCE:
