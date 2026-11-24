SHELL := /usr/bin/env bash

BIN        := llmspan
PKG        := github.com/llmspan/llmspan
CMD        := ./cmd/llmspan
DIST       := dist

VERSION    := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
  -X $(PKG)/internal/buildinfo.Version=$(VERSION) \
  -X $(PKG)/internal/buildinfo.Commit=$(COMMIT) \
  -X $(PKG)/internal/buildinfo.Date=$(BUILD_DATE)

GO       ?= go
GOFLAGS  ?= -trimpath
TESTOPTS ?= -race -count=1 -timeout=180s

# Fails when the embedded price snapshot is older than 90 days (see docs/PRICING.md).
check-prices:
	LLMSPAN_CHECK_PRICES=1 $(GO) test -count=1 ./internal/pricing -run TestSnapshotFreshness

.PHONY: check-prices all build run test load-test lint vet tidy fmt clean docker docker-build docker-push compose-up compose-up-insecure compose-down release demo demo-real

all: lint test build

build:
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN) $(CMD)

# Uses config.yaml if you've created one, otherwise falls back to the
# checked-in example so `git clone && make run` works immediately.
run: build
	@if [ -f config.yaml ]; then \
	  ./$(BIN) up --config config.yaml; \
	else \
	  echo "config.yaml not found; using config.example.yaml"; \
	  ./$(BIN) up --config config.example.yaml; \
	fi

test:
	$(GO) test $(TESTOPTS) ./...

# Go-native load harness. Asserts p99 added latency < 5ms, zero 5xx,
# no goroutine leak. Override via LOAD_RPS / LOAD_DURATION /
# LOAD_P99_BUDGET — see test/load/load_test.go.
load-test:
	$(GO) test -tags loadtest -count=1 -timeout=2m ./test/load/...

vet:
	$(GO) vet ./...

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed; running 'go vet' as a fallback"; \
		$(GO) vet ./...; \
	fi

tidy:
	$(GO) mod tidy

fmt:
	$(GO) fmt ./...

clean:
	rm -rf $(BIN) $(DIST) coverage.txt coverage.html

# Cross-compile a release set (linux/amd64, linux/arm64, darwin/arm64).
release: clean
	@mkdir -p $(DIST)
	@for target in linux/amd64 linux/arm64 darwin/arm64; do \
	  os=$${target%%/*}; arch=$${target##*/}; \
	  out=$(DIST)/$(BIN)-$$os-$$arch; \
	  echo ">> $$out"; \
	  GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
	    $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $$out $(CMD); \
	done

docker-build:
	docker build \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  --build-arg BUILD_DATE=$(BUILD_DATE) \
	  -t llmspan:$(VERSION) -t llmspan:latest .

# Brings up the demo stack with llmspan listening on HTTPS :4443. The
# self-signed cert is generated on first run by the tls-init sidecar.
# Clients point OPENAI_BASE_URL=https://localhost:4443/v1 and either
# trust the cert or pass `curl -k`.
compose-up:
	docker compose -f deploy/compose/docker-compose.yml up -d --build

# Plaintext-loopback escape hatch — only for a 30-second
# `curl http://localhost:4000/v1/...` demo. Teaches the wrong trust
# model for production. Prefer `compose-up` everywhere else.
compose-up-insecure:
	docker compose \
	  -f deploy/compose/docker-compose.yml \
	  -f deploy/compose/docker-compose.insecure.yml \
	  up -d --build

# Keyless demo: mock upstream + synthetic traffic, no API key needed.
# Open Grafana at http://localhost:3000 -> "LLM Observability — llmspan".
demo:
	docker compose \
	  -f deploy/compose/docker-compose.yml \
	  -f deploy/compose/docker-compose.insecure.yml \
	  -f deploy/compose/docker-compose.keyless.yml \
	  up -d --build

# Real-data demo: llmspan in front of an Ollama on the host (llama3.2:1b;
# run `ollama serve` and `ollama pull llama3.2:1b` first). Tokens and latency are real; cost uses an illustrative rate.
demo-real:
	docker compose \
	  -f deploy/compose/docker-compose.yml \
	  -f deploy/compose/docker-compose.insecure.yml \
	  -f deploy/compose/docker-compose.ollama.yml \
	  up -d --build

compose-down:
	docker compose -f deploy/compose/docker-compose.yml down -v --remove-orphans
