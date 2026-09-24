# Laptop Entra lab. From repo root: make entra / entra-up / entra-down / …
# Live YAML is Hop 1 mcp_client_auth + broker_oauth token-exchange with
# per-broker auth.audience, so this checkout loads it. MCP_REPO defaults
# here; override only to run against a different checkout. The template
# carries jwt-bearer and auth.target as comments for later.

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
# Tenant/app GUIDs come from .env (gitignored). Copy .env.example.
.PHONY: config
config: ## Render broker-config.entra.yaml (absolute cert paths + Entra IDs)
	@if [ ! -f "$(TEMPLATE)" ]; then echo "missing $(TEMPLATE)"; exit 1; fi
	@if [ ! -f "$(ENV_FILE)" ]; then echo "missing $(ENV_FILE) — copy .env.example"; exit 1; fi
	@mkdir -p $(CURDIR)/.local
	@set -a && . "$(ENV_FILE)" && set +a && \
	  : "$${ENTRA_TENANT_ID:?empty ENTRA_TENANT_ID}" && \
	  : "$${MCP_BROKER_CLIENT_ID:?empty MCP_BROKER_CLIENT_ID}" && \
	  : "$${MCP_RESOURCE_URL:?empty MCP_RESOURCE_URL}" && \
	  : "$${MCP_ACCESS_AS_USER_SCOPE:?empty MCP_ACCESS_AS_USER_SCOPE}" && \
	  : "$${BROKER_RESOURCE_APP_ID:?empty BROKER_RESOURCE_APP_ID}" && \
	  : "$${OBO_PROBE_RESOURCE_APP_ID:?empty OBO_PROBE_RESOURCE_APP_ID}" && \
	  sed \
	    -e 's|@@CERT_ROOT@@|$(CERT_ROOT)|g' \
	    -e "s|@@ENTRA_TENANT_ID@@|$${ENTRA_TENANT_ID}|g" \
	    -e "s|@@MCP_BROKER_CLIENT_ID@@|$${MCP_BROKER_CLIENT_ID}|g" \
	    -e "s|@@MCP_RESOURCE_URL@@|$${MCP_RESOURCE_URL}|g" \
	    -e "s|@@MCP_ACCESS_AS_USER_SCOPE@@|$${MCP_ACCESS_AS_USER_SCOPE}|g" \
	    -e "s|@@BROKER_RESOURCE_APP_ID@@|$${BROKER_RESOURCE_APP_ID}|g" \
	    -e "s|@@OBO_PROBE_RESOURCE_APP_ID@@|$${OBO_PROBE_RESOURCE_APP_ID}|g" \
	    "$(TEMPLATE)" > "$(CONFIG_FILE)"
	@echo "rendered $(CONFIG_FILE)"

.PHONY: config-clean
config-clean: ## Delete the rendered config
	rm -f $(CONFIG_FILE)

# MCP_REPO defaults to this repo root, which loads the live lab YAML.
# Override to `go run` a different checkout. Blocking on purpose.
# $(CURDIR) here is local/entra/, so the repo root is two levels up.
MCP_REPO ?= $(abspath $(CURDIR)/../..)

.PHONY: run
run: certs config ## Run the MCP server (blocking) against this lab
	@if [ ! -d "$(MCP_REPO)" ]; then echo "MCP_REPO is not a directory: $(MCP_REPO)" >&2; exit 1; fi
	@if [ ! -f "$(ENV_FILE)" ]; then \
	  echo "missing $(ENV_FILE) — copy .env.example and set the required keys"; \
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
	go -C $(MCP_REPO) run ./cmd/server
# ^^^ Lab-only. The SDK rejects a non-loopback Host against a 127.0.0.1
# listener. This Host is mcp-lab.solacetest.com. Do not copy this flag into
# product, operator docs, or CI.

.PHONY: claude-cmd
claude-cmd: ## Print env + Claude mcp remove/add + launch line
	@if [ ! -f $(CERT_ROOT)/combined-ca-bundle.crt ]; then \
	  echo "CA bundle missing — run 'make certs' first."; exit 1; \
	fi
	@if [ ! -f "$(ENV_FILE)" ]; then echo "missing $(ENV_FILE) — copy .env.example"; exit 1; fi
	@set -a && . "$(ENV_FILE)" && set +a && \
	: "$${LAB_OPERATOR_UPN:?empty LAB_OPERATOR_UPN}" && \
	: "$${MCP_RESOURCE_HOST:?empty MCP_RESOURCE_HOST}" && \
	: "$${MCP_AGENT_CLIENT_ID:?empty MCP_AGENT_CLIENT_ID}" && \
	: "$${MCP_RESOURCE_URL:?empty MCP_RESOURCE_URL}" && \
	echo "" && \
	echo "Claude is a separate process. Quit Claude first. Copy the REAL cert path" && \
	echo "(do not invent a '…' placeholder). mcp-agent client ID, no secret." && \
	echo "Sign in as $${LAB_OPERATOR_UPN}." && \
	echo "" && \
	echo "  claude mcp list" && \
	echo "  claude mcp remove solace-oauth-dev" && \
	echo "  claude mcp remove solace-entra-lab" && \
	echo "  # if it still appears: claude mcp remove <name> --scope user" && \
	echo "  #                         --scope project  or  --scope local" && \
	echo "" && \
	echo "  NODE_EXTRA_CA_CERTS=\"$(CERT_ROOT)/combined-ca-bundle.crt\" NO_PROXY=$${MCP_RESOURCE_HOST} \\" && \
	echo "    claude mcp add --transport http --client-id $${MCP_AGENT_CLIENT_ID} \\" && \
	echo "    solace-entra-lab $${MCP_RESOURCE_URL}" && \
	echo "" && \
	echo "Then start Claude (same env, fresh terminal):" && \
	echo "" && \
	echo "  NODE_EXTRA_CA_CERTS=\"$(CERT_ROOT)/combined-ca-bundle.crt\" NO_PROXY=$${MCP_RESOURCE_HOST} claude" && \
	echo ""
