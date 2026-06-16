# Solace Broker MCP — convenience targets.
# Thin wrappers around the same commands CI runs. CI (.github/workflows/build-and-test.yml)
# remains the source of truth; if a target here drifts from CI, CI wins.

BINARY      := solace-broker-mcp
PKG         := ./cmd/server
# Filter VERSION through a safe-character regex so a poisoned git tag cannot
# inject shell when spliced into -ldflags or docker build-args.
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null | grep -E '^[A-Za-z0-9._+-]+$$' || echo dev)
# Deferred (=) so $(shell git describe) only runs for targets that actually use LDFLAGS.
LDFLAGS      = -s -w -X github.com/SolaceDev/solace-broker-mcp/internal/version.version=$(VERSION)
IMAGE       ?= solace-broker-mcp
IMAGE_TAG   ?= dev
E2E_DIR     := test/e2e-basic-mcp
COMPOSE_E2E := docker compose -f $(E2E_DIR)/docker-compose.yml
E2E_MON_DIR := test/e2e-monitoring
COMPOSE_E2E_MON := docker compose -f $(E2E_MON_DIR)/docker-compose.yml

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z0-9_-]+:.*?## / {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# ── Build ────────────────────────────────────────────────────────────────────

.PHONY: build
build: ## Build the server binary (version-stamped, matches Dockerfile)
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

.PHONY: build-all
build-all: ## Build every package (matches CI's `go build -v ./...`)
	go build -v ./...

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
check: build-all vet lint test-race ## Run build, vet, lint, and race-enabled tests (matches CI build/lint/test jobs; E2E runs separately)

# ── E2E ──────────────────────────────────────────────────────────────────────

.PHONY: e2e-up
e2e-up: ## Start Solace brokers for E2E tests (does not wait for health — use `e2e-all` for the full cycle)
	$(COMPOSE_E2E) up -d

.PHONY: e2e
e2e: ## Run the E2E suite (requires brokers from `make e2e-up`)
	bash $(E2E_DIR)/run-all.sh

.PHONY: e2e-down
e2e-down: ## Stop and remove E2E brokers
	$(COMPOSE_E2E) down -v

.PHONY: e2e-all
e2e-all: ## Full E2E cycle: brokers up, wait for health, run suite, tear down (tears down even on failure)
	$(COMPOSE_E2E) up -d
	@. $(E2E_DIR)/helpers.sh && wait_for_all_brokers 120 && bash $(E2E_DIR)/run-all.sh; t=$$?; \
	$(COMPOSE_E2E) down -v || echo "WARN: e2e-all teardown failed"; \
	exit $$t

.PHONY: e2e-monitoring-up
e2e-monitoring-up: ## Start brokers for the e2e-monitoring suite (use `e2e-monitoring-all` for the full cycle)
	$(COMPOSE_E2E_MON) up -d

.PHONY: e2e-monitoring
e2e-monitoring: ## Run the e2e-monitoring suite (requires brokers from `make e2e-monitoring-up`)
	bash $(E2E_MON_DIR)/test-monitoring-tools.sh

.PHONY: e2e-monitoring-down
e2e-monitoring-down: ## Stop and remove e2e-monitoring brokers
	$(COMPOSE_E2E_MON) down -v

.PHONY: e2e-monitoring-all
e2e-monitoring-all: ## Full e2e-monitoring cycle: brokers up, wait for health, run suite, tear down (tears down even on failure)
	$(COMPOSE_E2E_MON) up -d
	@. $(E2E_MON_DIR)/helpers.sh && wait_for_all_brokers 120 && bash $(E2E_MON_DIR)/test-monitoring-tools.sh; t=$$?; \
	$(COMPOSE_E2E_MON) down -v || echo "WARN: e2e-monitoring-all teardown failed"; \
	exit $$t

# ── Docker ───────────────────────────────────────────────────────────────────

.PHONY: docker
docker: ## Build the Docker image (override with IMAGE=, IMAGE_TAG=, VERSION=)
	docker build --build-arg VERSION="$(VERSION)" -t "$(IMAGE):$(IMAGE_TAG)" .
