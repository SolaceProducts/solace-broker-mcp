# Laptop Entra lab. From repo root: make entra-up / entra-run / entra-down / …

CERT_ROOT  := $(CURDIR)/.local/certs
TEMPLATE   := $(CURDIR)/broker-config.entra.yaml.template
CONFIG_FILE := $(CURDIR)/.local/broker-config.entra.yaml
ENV_FILE   := $(CURDIR)/.env

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z0-9_-]+:.*?## / {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: certs
certs: ## Generate self-signed MCP certs under .local/certs/ (idempotent)
	@./scripts/gen-dev-certs.sh $(CERT_ROOT)

.PHONY: certs-clean
certs-clean: ## Delete generated certs (restart Claude after the next certs)
	rm -rf $(CERT_ROOT)

.PHONY: brokers-up
brokers-up: ## Start mcp-entra-solace* brokers and PATCH Entra OAuth (converge)
	@./scripts/setup-entra-brokers.sh

.PHONY: brokers-down
brokers-down: ## Remove mcp-entra-solace* containers only (keeps .local/ and .env)
	@./scripts/teardown-entra-brokers.sh

# Rendered YAML is gitignored under .local/. Edit the template, not the output.
.PHONY: config
config: ## Render broker-config.entra.yaml (absolute cert paths)
	@if [ ! -f "$(TEMPLATE)" ]; then echo "missing $(TEMPLATE)"; exit 1; fi
	@mkdir -p $(CURDIR)/.local
	@sed -e 's|@@CERT_ROOT@@|$(CERT_ROOT)|g' "$(TEMPLATE)" > "$(CONFIG_FILE)"
	@echo "rendered $(CONFIG_FILE)"

.PHONY: config-clean
config-clean: ## Delete the rendered config
	rm -f $(CONFIG_FILE)

# Builds from MCP_REPO via `go run -C`. This package never starts the server
# as a background job; `run` is blocking on purpose.
.PHONY: run
run: config ## Run the MCP server (blocking) against this lab
	@if [ -z "$(MCP_REPO)" ]; then \
	  echo "MCP_REPO is required — path to a solace-broker-mcp checkout. See README.md." >&2; \
	  exit 1; \
	fi
	@if [ ! -d "$(MCP_REPO)" ]; then echo "MCP_REPO is not a directory: $(MCP_REPO)" >&2; exit 1; fi
	@if [ ! -f "$(ENV_FILE)" ]; then \
	  echo "missing $(ENV_FILE) — copy .env.example and set MCP_SERVER_CLIENT_SECRET"; \
	  exit 1; \
	fi
	@if ! grep -q '^MCP_SERVER_CLIENT_SECRET=.\+' "$(ENV_FILE)"; then \
	  echo "MCP_SERVER_CLIENT_SECRET is empty — copy .env.example and set the mcp-broker client secret"; \
	  exit 1; \
	fi
	@$(MAKE) claude-cmd
	CONFIG_FILE=$(CONFIG_FILE) \
	ENV_FILE=$(ENV_FILE) \
	MCPGODEBUG=disablelocalhostprotection=1 \
	go run -C $(MCP_REPO) ./cmd/server
# ^^^ MCPGODEBUG=disablelocalhostprotection=1 is a TEMPORARY LAB-ONLY workaround.
# The MCP Go SDK rejects requests whose Host header is non-loopback when the
# listener is on 127.0.0.1 (DNS-rebinding protection). The lab reaches the server
# at mcp-lab.solacetest.com:19090, so we must bypass the check here only.
# Do NOT carry this flag into production deployments or CI.

.PHONY: claude-cmd
claude-cmd: ## Print env + command to launch Claude trusting the lab certs
	@if [ ! -f $(CERT_ROOT)/combined-ca-bundle.crt ]; then \
	  echo "CA bundle missing — run 'make certs' first."; exit 1; \
	fi
	@echo ""
	@echo "Run this in a FRESH terminal (copy the whole line), then reconnect"
	@echo "with mcp-agent Application (client) ID REDACTED"
	@echo "(no client secret). Restart Claude if it was already running."
	@echo ""
	@echo "  NODE_EXTRA_CA_CERTS=\"$(CERT_ROOT)/combined-ca-bundle.crt\" NO_PROXY=mcp-lab.solacetest.com claude"
	@echo ""
