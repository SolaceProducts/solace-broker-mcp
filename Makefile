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
E2E_MGMT_DIR := test/e2e-management
COMPOSE_E2E_MGMT := docker compose -f $(E2E_MGMT_DIR)/docker-compose.yml
E2E_ACT_DIR := test/e2e-action
COMPOSE_E2E_ACT := docker compose -f $(E2E_ACT_DIR)/docker-compose.yml

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
	bash $(E2E_MON_DIR)/run-all.sh

.PHONY: e2e-monitoring-down
e2e-monitoring-down: ## Stop and remove e2e-monitoring brokers
	$(COMPOSE_E2E_MON) down -v

.PHONY: e2e-monitoring-all
e2e-monitoring-all: ## Full e2e-monitoring cycle: brokers up, wait for health, run suite, tear down (tears down even on failure)
	$(COMPOSE_E2E_MON) up -d
	@. $(E2E_MON_DIR)/helpers.sh && wait_for_all_brokers 120 && bash $(E2E_MON_DIR)/run-all.sh; t=$$?; \
	$(COMPOSE_E2E_MON) down -v || echo "WARN: e2e-monitoring-all teardown failed"; \
	exit $$t

.PHONY: e2e-management-up
e2e-management-up: ## Start brokers for the e2e-management suite (use `e2e-management-all` for the full cycle)
	$(COMPOSE_E2E_MGMT) up -d

.PHONY: e2e-management
e2e-management: ## Run the e2e-management suite (requires brokers from `make e2e-management-up`)
	bash $(E2E_MGMT_DIR)/run-all.sh

.PHONY: e2e-management-down
e2e-management-down: ## Stop and remove e2e-management brokers
	$(COMPOSE_E2E_MGMT) down -v

.PHONY: e2e-management-all
e2e-management-all: ## Full e2e-management cycle: brokers up, wait for health, run suite, tear down (tears down even on failure)
	$(COMPOSE_E2E_MGMT) up -d
	@. $(E2E_MGMT_DIR)/helpers.sh && wait_for_all_brokers 120 && bash $(E2E_MGMT_DIR)/run-all.sh; t=$$?; \
	$(COMPOSE_E2E_MGMT) down -v || echo "WARN: e2e-management-all teardown failed"; \
	exit $$t

.PHONY: e2e-action-up
e2e-action-up: ## Start brokers for the e2e-action suite (use `e2e-action-all` for the full cycle)
	$(COMPOSE_E2E_ACT) up -d

.PHONY: e2e-action
e2e-action: ## Run the e2e-action suite (requires brokers from `make e2e-action-up`)
	bash $(E2E_ACT_DIR)/run-all.sh

.PHONY: e2e-action-down
e2e-action-down: ## Stop and remove e2e-action brokers
	$(COMPOSE_E2E_ACT) down -v

.PHONY: e2e-action-all
e2e-action-all: ## Full e2e-action cycle: brokers up, wait for health, run suite, tear down (tears down even on failure)
	$(COMPOSE_E2E_ACT) up -d
	@. $(E2E_ACT_DIR)/helpers.sh && wait_for_all_brokers 120 && bash $(E2E_ACT_DIR)/run-all.sh; t=$$?; \
	$(COMPOSE_E2E_ACT) down -v || echo "WARN: e2e-action-all teardown failed"; \
	exit $$t

# ── Docker ───────────────────────────────────────────────────────────────────

.PHONY: docker
docker: ## Build the Docker image (override with IMAGE=, IMAGE_TAG=, VERSION=)
	docker build --build-arg VERSION="$(VERSION)" -t "$(IMAGE):$(IMAGE_TAG)" .

# ── Local OAuth dev environment ─────────────────────────────────────────────
# Wires up a Keycloak container with TLS + realm import so the MCP server's
# OAuth path (JWT validation, RFC 8693 token exchange) can be exercised end
# to end on localhost.  Every target below is idempotent — safe to re-run.

OAUTH_COMPOSE_DIR := dev/oauth-token-exchange
OAUTH_COMPOSE     := podman compose -f $(OAUTH_COMPOSE_DIR)/docker-compose.yaml
OAUTH_CERT_ROOT   := $(CURDIR)/.local/certs

.PHONY: certs
certs: ## Generate self-signed dev certs under .local/certs/ (idempotent)
	@./scripts/gen-dev-certs.sh $(OAUTH_CERT_ROOT)

.PHONY: certs-clean
certs-clean: ## Delete generated dev certs
	rm -rf $(OAUTH_CERT_ROOT)

.PHONY: oauth-up
oauth-up: certs ## Start the Keycloak dev container (health-gated by oauth-init)
	$(OAUTH_COMPOSE) up -d

.PHONY: oauth-init
oauth-init: oauth-up ## Apply post-startup Keycloak init (disable HSTS, reset user passwords)
	@./scripts/keycloak-init.sh

.PHONY: oauth-down
oauth-down: ## Stop and remove the Keycloak dev container
	-$(OAUTH_COMPOSE) down

.PHONY: oauth-reset
oauth-reset: oauth-down oauth-init ## Full reset: tear down Keycloak, restart, re-init

.PHONY: dev-up
dev-up: oauth-init ## Bring the Keycloak dev environment up (certs + Keycloak + init)
	@echo ""
	@echo "▶ Keycloak ready. For the two-broker OAuth stack (recommended for end-to-end):"
	@echo "    make dev-up-full"
	@echo "▶ Or launch the MCP server against Keycloak-only:"
	@echo "    make run-oauth"

.PHONY: dev-up-full
dev-up-full: dev-up ## Bring up Keycloak + configure the two Solace brokers, and stage broker-config
	@./scripts/setup-oauth-brokers.sh
	@if [ ! -f broker-config.oauth-test.yaml ]; then \
	  cp broker-config.oauth-test.example.yaml broker-config.oauth-test.yaml; \
	  echo ""; \
	  echo "▶ Wrote broker-config.oauth-test.yaml from the template."; \
	  echo "  Edit it and replace REPLACE_WITH_MCP_SERVER_CLIENT_SECRET with the"; \
	  echo "  Keycloak client secret (Clients → mcp-server-client → Credentials)."; \
	else \
	  echo ""; \
	  echo "▶ broker-config.oauth-test.yaml already exists — leaving it as-is."; \
	fi
	@echo ""
	@echo "▶ Two manual steps remain (see dev/oauth-token-exchange/README.md):"
	@echo "    1. In another terminal: make run-oauth"
	@echo "    2. Launch Claude Code with the CA bundle:"
	@echo "         NODE_EXTRA_CA_CERTS=$(OAUTH_CERT_ROOT)/combined-ca-bundle.crt claude"

.PHONY: run-oauth
run-oauth: oauth-init ## Run the MCP server against the local OAuth dev environment
	ENABLE_UNRELEASED_BROKER_OAUTH=true \
	SSL_CERT_FILE=$(OAUTH_CERT_ROOT)/keycloak/keycloak.crt \
	CONFIG_FILE=broker-config.oauth-test.yaml \
	go run $(PKG)
