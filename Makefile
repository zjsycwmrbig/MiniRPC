.PHONY: help fmt fmt-check vet test race build check lint clean

GO ?= go
GO_PACKAGES ?= ./...
GO_FILES := $(shell find . -name '*.go' -not -path './.git/*')

help: ## Show available commands
	@awk 'BEGIN {FS = ":.*## "; printf "Usage:\n  make <target>\n\nTargets:\n"} /^[a-zA-Z_-]+:.*## / {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

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

build: ## Compile all packages
	$(GO) build $(GO_PACKAGES)

lint: ## Run golangci-lint
	@command -v golangci-lint >/dev/null 2>&1 || (echo "golangci-lint is not installed; see https://golangci-lint.run/welcome/install/"; exit 1)
	golangci-lint run

check: fmt-check vet test build ## Run the local CI-equivalent checks

clean: ## Remove local build and test artifacts
	$(GO) clean
	rm -f coverage.out
