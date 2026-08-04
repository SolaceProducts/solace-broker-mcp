# Solace Broker MCP — convenience targets.
# Thin wrappers around the same commands CI runs. CI (.github/workflows/build-and-test.yml)
# remains the source of truth; if a target here drifts from CI, CI wins.

BINARY      := solace-broker-mcp
PKG         := ./cmd/server
# Filter VERSION through a safe-character regex so a poisoned git tag cannot
# inject shell when spliced into -ldflags or docker build-args.
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null | grep -E '^[A-Za-z0-9._+-]+$$' || echo dev)
# Deferred (=) so $(shell git describe) only runs for targets that actually use LDFLAGS.
LDFLAGS      = -s -w -X github.com/SolaceProducts/solace-broker-mcp/internal/version.version=$(VERSION)
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
E2E_OAUTH_DIR := test/e2e-oauth
COMPOSE_E2E_OAUTH := docker compose -f $(E2E_OAUTH_DIR)/docker-compose.yml

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z0-9_-]+:.*?## / {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# ── Git hooks ────────────────────────────────────────────────────────────────

# Installs the hook as an untracked copy under .git/, read out of a TRUSTED REF
# (HOOKS_REF, default origin/main) rather than out of the working tree — rationale
# in the header of .githooks/prepare-commit-msg. Reading the payload from the tree
# would let a hostile branch have a reviewer install a persistent payload just by
# re-running this target, which CONTRIBUTING.md tells them to do. Use
# `HOOKS_REF=HEAD make hooks` when you are editing the hook itself — HEAD is a
# commit, so commit the edit first; this target never reads the working tree.
# Hence also the refusals below: a hooks directory anywhere but inside this repo's
# git dir means git runs the hook either from the checked-out tree or from a
# directory shared with every other repo on the machine. Fails closed.
# `git rev-parse --git-path` honours a custom core.hooksPath and resolves the
# shared hooks directory from a linked worktree, where .git is a file.
# Overwrites only a hook carrying our own marker, so an unrelated
# prepare-commit-msg (ticket prefixer, commit-lint) survives.
HOOK_MARKER := solace-broker-mcp:prepare-commit-msg
HOOK_SRC    := .githooks/prepare-commit-msg

.PHONY: hooks
hooks: ## Install the repo's git hooks (DCO sign-off) from a trusted ref — default origin/main, HOOKS_REF=HEAD for your own committed edit; re-run after pulling hook changes
	@set -e; \
	hooks_dir="$$(git rev-parse --git-path hooks)"; \
	mkdir -p "$$hooks_dir"; \
	hooks_abs="$$(cd "$$hooks_dir" && pwd -P)"; \
	git_dir="$$(cd "$$(git rev-parse --git-common-dir)" && pwd -P)"; \
	top="$$(cd "$$(git rev-parse --show-toplevel)" && pwd -P)"; \
	case "$$hooks_abs/" in \
	  "$$git_dir"/*) ;; \
	  "$$top"/*) \
	    echo "refusing to install: core.hooksPath points inside the working tree ($$hooks_abs)." >&2; \
	    echo "Hooks there run the version in whatever branch is checked out, including a fork's PR." >&2; \
	    echo "See the header of .githooks/prepare-commit-msg. Fix with:" >&2; \
	    echo "  git config --unset core.hooksPath          # if it is set for this repo" >&2; \
	    echo "  git config --global --unset core.hooksPath  # if it is set for your user" >&2; \
	    echo "Check which with: git config --show-origin --get core.hooksPath" >&2; \
	    exit 1;; \
	  *) \
	    echo "refusing to install: hooks dir ($$hooks_abs) is outside this repo's git dir." >&2; \
	    echo "See the header of $(HOOK_SRC)." >&2; \
	    echo "Check where it comes from with: git config --show-origin --get core.hooksPath" >&2; \
	    exit 1;; \
	esac; \
	ref="$${HOOKS_REF:-origin/main}"; \
	if ! git rev-parse --verify -q "$$ref^{commit}" >/dev/null; then \
	  echo "refusing to install: cannot resolve HOOKS_REF ($$ref)." >&2; \
	  echo "make hooks installs from a trusted ref, never from the checked-out tree" >&2; \
	  echo "(see the header of $(HOOK_SRC)). Fetch that ref, or name another:" >&2; \
	  echo "  git fetch origin main" >&2; \
	  echo "  HOOKS_REF=upstream/main make hooks   # if your remote is not called origin" >&2; \
	  echo "  HOOKS_REF=HEAD make hooks            # install YOUR commit; only when editing the hook" >&2; \
	  exit 1; \
	fi; \
	if ! git cat-file -e "$$ref:$(HOOK_SRC)" 2>/dev/null; then \
	  echo "refusing to install: $(HOOK_SRC) does not exist at $$ref." >&2; \
	  echo "If you are on a branch that adds or changes it: HOOKS_REF=HEAD make hooks" >&2; \
	  exit 1; \
	fi; \
	dest="$$hooks_dir/prepare-commit-msg"; \
	if [ -e "$$dest" ] && ! grep -q "$(HOOK_MARKER)" "$$dest"; then \
	  echo "refusing to overwrite $$dest — it is not this repo's hook." >&2; \
	  echo "Move it aside and re-run make hooks. To keep both, install ours under another" >&2; \
	  echo "name in the hooks directory and call THAT from yours — never .githooks/, which" >&2; \
	  echo "is the checked-out tree (see the header of $(HOOK_SRC))." >&2; \
	  exit 1; \
	fi; \
	tmp="$$dest.dco-install.$$$$"; \
	trap 'rm -f "$$tmp"' EXIT HUP INT TERM; \
	git cat-file blob "$$ref:$(HOOK_SRC)" >"$$tmp"; \
	if ! grep -q "$(HOOK_MARKER)" "$$tmp"; then \
	  echo "refusing to install: $$ref:$(HOOK_SRC) does not carry the marker" >&2; \
	  echo "$(HOOK_MARKER), so it is not this repo's hook. Check what HOOKS_REF ($$ref) points at." >&2; \
	  exit 1; \
	fi; \
	chmod 755 "$$tmp"; \
	mv "$$tmp" "$$dest"; \
	echo "installed $$dest (from $$ref)"

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

.PHONY: bench
bench: ## Run benchmarks repo-wide (matches CI; -run=^$ skips regular tests so only Benchmark* funcs execute)
	go test -run=^$$ -bench=. -benchmem ./...

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

.PHONY: e2e-oauth-up
e2e-oauth-up: ## Start Keycloak + brokers for the e2e-oauth suite and configure OAuth profiles (use `e2e-oauth-all` for the full cycle)
	@. $(E2E_OAUTH_DIR)/helpers.sh && ensure_tls_certs
	$(COMPOSE_E2E_OAUTH) up -d
	@. $(E2E_OAUTH_DIR)/helpers.sh && wait_for_all_brokers 120 && wait_for_keycloak 120
	bash $(E2E_OAUTH_DIR)/configure-oauth-profiles.sh

.PHONY: e2e-oauth
e2e-oauth: ## Run the e2e-oauth suite (requires `e2e-oauth-up` first)
	bash $(E2E_OAUTH_DIR)/run-all.sh

.PHONY: e2e-oauth-down
e2e-oauth-down: ## Stop and remove e2e-oauth Keycloak + brokers
	$(COMPOSE_E2E_OAUTH) down -v

.PHONY: e2e-oauth-all
e2e-oauth-all: ## Full e2e-oauth cycle: certs, Keycloak+brokers up, configure OAuth profiles, run suite, tear down (tears down even on failure)
	@. $(E2E_OAUTH_DIR)/helpers.sh && ensure_tls_certs
	$(COMPOSE_E2E_OAUTH) up -d
	@. $(E2E_OAUTH_DIR)/helpers.sh && wait_for_all_brokers 120 && wait_for_keycloak 120 \
	&& bash $(E2E_OAUTH_DIR)/configure-oauth-profiles.sh && bash $(E2E_OAUTH_DIR)/run-all.sh; t=$$?; \
	$(COMPOSE_E2E_OAUTH) down -v || echo "WARN: e2e-oauth-all teardown failed"; \
	exit $$t

# ── Docker ───────────────────────────────────────────────────────────────────

.PHONY: docker
docker: ## Build the Docker image (override with IMAGE=, IMAGE_TAG=, VERSION=)
	docker build --build-arg VERSION="$(VERSION)" -t "$(IMAGE):$(IMAGE_TAG)" .
