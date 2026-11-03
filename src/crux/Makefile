# Makefile for the Crux agent runtime module (github.com/hivecommons/hive/crux).
# Everything here works from this directory alone, with or without the
# src/go.work workspace: GOWORK=off is set so the targets prove the module is
# self-contained (hivecommons/hive#14138).
#
#   make build   -> bin/crux (version from `git describe`, or VERSION=...)
#   make test    -> go test ./... (crux/agent runs tmux tests; never run it
#                   from inside an agent pane — parts of crux/agent exercise
#                   tmux process management and can kill the agent's own session;
#                   see https://github.com/hivecommons/hive/blob/v5/AGENTS.md)
#   make vet     -> go vet ./...
#   make smoke   -> build, start `crux serve` on a private unix socket, drive
#                   it with `crux --server`, stop it (scripts/smoke.sh)

GO      ?= go
BIN     ?= bin/crux
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS ?= -X main.version=$(VERSION)

export GOWORK = off

.PHONY: build test vet smoke clean

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/crux

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

smoke: build
	CRUX_BIN=$(abspath $(BIN)) bash scripts/smoke.sh

clean:
	rm -rf bin .smoke
