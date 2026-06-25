#!/usr/bin/env bash
# Reverse setup-fixtures.sh: stop MCP server, reap broker-driver processes,
# delete SEMP fixtures. Leaves the broker containers up so a follow-up
# setup-fixtures.sh is fast.

set -euo pipefail

LLM_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E_DIR="$(cd "$LLM_DIR/.." && pwd)"

# shellcheck disable=SC1091
source "$LLM_DIR/config.env"

# Mirror setup-fixtures.sh — only local-docker provisions anything to
# tear down. Non-local targets manage their own lifecycle.
if [ "$BROKER_TARGET" != "local-docker" ]; then
    echo "[INFO] BROKER_TARGET=$BROKER_TARGET — nothing to tear down"
    exit 0
fi

# shellcheck disable=SC1091
source "$E2E_DIR/helpers.sh"

stop_server || true
cleanup_fixtures || true
rm -f "$BIN_DIR/mcp-server.pid"

log_ok "Fixtures torn down. Brokers still running."
echo "To stop brokers too:"
echo "  docker compose -f $E2E_DIR/docker-compose.yml down -v"
