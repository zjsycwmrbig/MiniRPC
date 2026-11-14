.PHONY: help tools fmt fmt-check vet test race fault-test benchmark benchmark-smoke benchmark-http2 benchmark-http2-smoke docker-test docker-http2-test docker-tls-test docker-tls-rotation-test docker-otel-test docker-otel-mtls-test docker-otel-mtls-rotation-test build check lint proto proto-check clean

GO ?= go
GO_PACKAGES ?= ./...
GO_FILES := $(shell find . -name '*.go' -not -path './.git/*' -not -path './bin/*')
PROTOC ?= protoc
PROTOC_VERSION ?= 28.3
BIN_DIR ?= $(CURDIR)/bin
PROTO_FILES := $(shell find api -name '*.proto' -print | sort)

help: ## Show available commands
	@awk 'BEGIN {FS = ":.*## "; printf "Usage:\n  make <target>\n\nTargets:\n"} /^[a-zA-Z_-]+:.*## / {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

tools: ## Install pinned code generators into bin/
	@mkdir -p "$(BIN_DIR)"
	$(GO) build -o "$(BIN_DIR)/protoc-gen-go" google.golang.org/protobuf/cmd/protoc-gen-go
	$(GO) build -o "$(BIN_DIR)/protoc-gen-minirpc" ./cmd/protoc-gen-minirpc

fmt: ## Format all Go source files
	gofmt -w $(GO_FILES)

fmt-check: ## Fail when any Go source file is not formatted
	@test -z "$$(gofmt -l $(GO_FILES))" || (echo "Go files need formatting:"; gofmt -l $(GO_FILES); exit 1)

vet: ## Run go vet
	$(GO) vet $(GO_PACKAGES)

test: ## Run unit tests
	$(GO) test $(GO_PACKAGES)

race: ## Run tests with the race detector
	$(GO) test -race $(GO_PACKAGES)

fault-test: ## Run deterministic failover and recovery drills
	$(GO) run ./fault-injection -scenario all

benchmark: ## Run the complete local performance benchmark
	CGO_ENABLED=0 $(GO) run -trimpath ./benchmark

benchmark-smoke: ## Run a short plaintext/TLS/mTLS benchmark validation
	CGO_ENABLED=0 $(GO) run -trimpath ./benchmark \
		-duration=150ms -warmup=20ms -concurrency=16 -sizes=32 -handshakes=10

benchmark-http2: ## Run isolated-process HTTP/2 h2c performance acceptance
	CGO_ENABLED=0 ./scripts/http2-benchmark.sh

benchmark-http2-smoke: ## Run a short isolated-process HTTP/2 benchmark validation
	CGO_ENABLED=0 MINIRPC_HTTP2_BENCH_DURATION=150ms \
		MINIRPC_HTTP2_BENCH_WARMUP=20ms \
		MINIRPC_HTTP2_BENCH_CONCURRENCY=16 \
		MINIRPC_HTTP2_BENCH_HANDSHAKES=5 \
		./scripts/http2-benchmark.sh

docker-test: ## Build and smoke-test the complete Docker Compose environment
	./scripts/docker-smoke.sh

docker-http2-test: ## Verify h2c discovery and generated unary RPC
	./scripts/docker-http2-smoke.sh

docker-tls-test: ## Build and smoke-test the TLS and mutual-TLS Compose environment
	./scripts/docker-tls-smoke.sh

docker-tls-rotation-test: ## Verify live server and client TLS material rotation
	./scripts/docker-tls-rotation-smoke.sh

docker-otel-test: ## Verify OTLP trace export through an OpenTelemetry Collector
	./scripts/docker-otel-smoke.sh

docker-otel-mtls-test: ## Verify OTLP export with a custom CA and collector mutual TLS
	./scripts/docker-otel-mtls-smoke.sh

docker-otel-mtls-rotation-test: ## Verify live OTLP collector TLS material rotation
	./scripts/docker-otel-mtls-rotation-smoke.sh

build: ## Compile all packages
	$(GO) build $(GO_PACKAGES)

lint: ## Run golangci-lint
	@command -v golangci-lint >/dev/null 2>&1 || (echo "golangci-lint is not installed; see https://golangci-lint.run/welcome/install/"; exit 1)
	golangci-lint run

check-protoc:
	@command -v "$(PROTOC)" >/dev/null 2>&1 || (echo "protoc is required; install protoc $(PROTOC_VERSION) or set PROTOC"; exit 1)

proto: check-protoc tools ## Generate Protobuf messages and Mini-RPC adapters
	PATH="$(BIN_DIR):$$PATH" $(PROTOC) -I . \
		--go_out=. --go_opt=paths=source_relative \
		--minirpc_out=. --minirpc_opt=paths=source_relative \
		$(PROTO_FILES)

proto-check: proto ## Fail when checked-in generated code differs from source
	@changed="$$(git status --porcelain -- api)"; \
	if [ -n "$$changed" ]; then \
		echo "Generated API files are out of date:"; \
		echo "$$changed"; \
		git diff -- api; \
		exit 1; \
	fi

check: fmt-check proto-check vet test build ## Run the local CI-equivalent checks

clean: ## Remove local build and test artifacts
	$(GO) clean
	rm -rf "$(BIN_DIR)"
	rm -f coverage.out
