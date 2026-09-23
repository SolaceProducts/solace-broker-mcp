# Laptop Entra lab. Invoked from repo root later (`make entra-up`).
# Until then: make -C local/entra <target>

CERT_ROOT := $(CURDIR)/.local/certs

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
