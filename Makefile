# Solace Broker MCP — convenience targets.
# Thin wrappers around the same commands CI runs. CI (.github/workflows/build-and-test.yml)
# remains the source of truth; if a target here drifts from CI, CI wins.

BINARY      := solace-broker-mcp
PKG         := ./cmd/server
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X github.com/SolaceDev/solace-broker-mcp/internal/version.version=$(VERSION)
IMAGE       ?= solace-broker-mcp
IMAGE_TAG   ?= dev
COMPOSE_E2E := docker compose -f test/e2e/docker-compose.yml

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z0-9_-]+:.*?## / {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# ── Build ────────────────────────────────────────────────────────────────────

.PHONY: build
build: ## Build the server binary
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

.PHONY: run
run: ## Run the server from source (uses ./broker-config.yaml)
	go run $(PKG)

.PHONY: clean
clean: ## Remove build artifacts
	rm -f $(BINARY)

# ── Test & lint ──────────────────────────────────────────────────────────────

.PHONY: test
test: ## Run unit tests
	go test -v ./...

.PHONY: test-race
test-race: ## Run unit tests with the race detector (matches CI)
	go test -race -v ./...

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: lint
lint: ## golangci-lint (CI pins v2.11.4)
	golangci-lint run

.PHONY: check
check: vet lint test-race ## Run vet, lint, and race-enabled tests

# ── E2E ──────────────────────────────────────────────────────────────────────

.PHONY: e2e-up
e2e-up: ## Start Solace brokers for E2E tests
	$(COMPOSE_E2E) up -d

.PHONY: e2e
e2e: ## Run the E2E suite (requires brokers from `make e2e-up`)
	bash test/e2e/run_all.sh

.PHONY: e2e-down
e2e-down: ## Stop and remove E2E brokers
	$(COMPOSE_E2E) down -v

# ── Docker ───────────────────────────────────────────────────────────────────

.PHONY: docker
docker: ## Build the Docker image (override with IMAGE=, IMAGE_TAG=, VERSION=)
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(IMAGE_TAG) .
