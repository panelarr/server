# Self-documenting Makefile: `make` or `make help` lists every target with a `## description`.
.DEFAULT_GOAL := help

# Developer tools are pinned here and installed into ./bin by `make setup`,
# so every contributor and CI run uses the same versions.
SQLC_VERSION          := v1.31.1
OAPI_CODEGEN_VERSION  := v2.8.0
OASDIFF_VERSION       := v1.33.0
GOLANGCI_LINT_VERSION := v2.14.0
GOVULNCHECK_VERSION   := v1.9.0

BIN      := $(CURDIR)/bin
export PATH := $(BIN):$(PATH)
GO       ?= go
# Build tools with the same Go version as the module, so linters understand its code.
TOOLCHAIN := $(shell $(GO) env GOVERSION)
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X github.com/panelarr/server/internal/buildinfo.Version=$(VERSION)
# The spec to compare against for breaking changes.
API_BASE ?= origin/main
# The web app release embedded by `make web` (empty: keep what's there).
WEB_VERSION ?=
WEB_REPO    ?= panelarr/web
WEB_DIR     ?= ../web/dist
WEB_DIST    := internal/webui/dist

GENERATED := internal/store/db internal/api/apigen

.PHONY: help setup build test test-short lint fmt fmt-check vet vuln gen gen-check api-breaking run clean check web web-local web-clean

help: ## List targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_.-]+:.*## / {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

setup: ## Install pinned developer tools into ./bin and download modules
	@mkdir -p $(BIN)
	GOTOOLCHAIN=$(TOOLCHAIN) GOBIN=$(BIN) $(GO) install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)
	GOTOOLCHAIN=$(TOOLCHAIN) GOBIN=$(BIN) $(GO) install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@$(OAPI_CODEGEN_VERSION)
	GOTOOLCHAIN=$(TOOLCHAIN) GOBIN=$(BIN) $(GO) install github.com/oasdiff/oasdiff@$(OASDIFF_VERSION)
	GOTOOLCHAIN=$(TOOLCHAIN) GOBIN=$(BIN) $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	GOTOOLCHAIN=$(TOOLCHAIN) GOBIN=$(BIN) $(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	$(GO) mod download

build: ## Build the panelarr binary into ./bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/panelarr ./cmd/panelarr

test: ## Run all tests with the race detector (Postgres via testcontainers or PANELARR_TEST_DATABASE_URL)
	$(GO) test -race -cover ./...

test-short: ## Run tests that need no Postgres or Docker
	$(GO) test -short -race ./...

fmt: ## Format Go files
	$(GO) fmt ./...

fmt-check: ## Fail if Go files are not formatted
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "not gofmt-ed:"; echo "$$out"; exit 1; fi

vet: ## go vet
	$(GO) vet ./...

lint: ## golangci-lint
	$(BIN)/golangci-lint run ./...

vuln: ## Check dependencies and the toolchain for known vulnerabilities
	$(BIN)/govulncheck ./...

gen: ## Generate the API server from api/openapi.yaml and the SQL queries (sqlc)
	$(BIN)/oapi-codegen -config api/oapi-codegen.yaml api/openapi.yaml
	$(BIN)/sqlc generate

gen-check: gen ## Fail if generated code is out of date
	@if [ -n "$$(git status --porcelain -- $(GENERATED))" ]; then git status --short -- $(GENERATED); echo "generated code is stale: run make gen and commit"; exit 1; fi

api-breaking: ## Fail if api/openapi.yaml breaks clients compared with $(API_BASE)
	@if git cat-file -e $(API_BASE):api/openapi.yaml 2>/dev/null; then \
		git show $(API_BASE):api/openapi.yaml > $(BIN)/openapi.base.yaml && \
		$(BIN)/oasdiff breaking --fail-on ERR $(BIN)/openapi.base.yaml api/openapi.yaml; \
	else echo "$(API_BASE) has no api/openapi.yaml yet; nothing to compare"; fi

web: ## Embed the web app release WEB_VERSION (downloaded and checked against its .sha256)
	@if [ -z "$(WEB_VERSION)" ]; then echo "WEB_VERSION is empty: keeping the current web files"; exit 0; fi; \
	set -e; tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	base=https://github.com/$(WEB_REPO)/releases/download/v$(WEB_VERSION); \
	curl -fsSL -o "$$tmp/web.tar.gz" "$$base/panelarr-web-$(WEB_VERSION).tar.gz"; \
	curl -fsSL -o "$$tmp/web.sha256" "$$base/panelarr-web-$(WEB_VERSION).tar.gz.sha256"; \
	(cd "$$tmp" && echo "$$(cut -d' ' -f1 web.sha256)  web.tar.gz" | shasum -a 256 -c -); \
	$(MAKE) web-clean; tar -xzf "$$tmp/web.tar.gz" -C $(WEB_DIST); \
	echo "embedded web $(WEB_VERSION)"

web-local: ## Embed a local web build from WEB_DIR (default ../web/dist)
	@test -f $(WEB_DIR)/index.html || (echo "no web build in $(WEB_DIR): run make build in the web repo" && exit 1)
	$(MAKE) web-clean
	cp -R $(WEB_DIR)/. $(WEB_DIST)/

web-clean: ## Remove the embedded web build (the placeholder page is served)
	find $(WEB_DIST) -mindepth 1 ! -name .keep -exec rm -rf {} +

run: build ## Run the server locally (needs PANELARR_DB)
	@echo "Panelarr listening on http://localhost:8900"
	$(BIN)/panelarr serve

clean: ## Remove build artifacts (keeps installed tools)
	rm -f $(BIN)/panelarr $(BIN)/openapi.base.yaml
	rm -rf dist coverage.out

check: fmt-check vet lint vuln test ## Format check, lint, static analysis, vulnerabilities and tests (run before every report/PR)
