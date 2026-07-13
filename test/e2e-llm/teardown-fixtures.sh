#!/usr/bin/env bash
# Reverse setup-fixtures.sh: stop MCP server, reap broker-driver processes,
# delete SEMP fixtures. Leaves the broker containers up so a follow-up
# setup-fixtures.sh is fast.

set -euo pipefail

LLM_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E_DIR="$(cd "$LLM_DIR/../e2e-monitoring" && pwd)"

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

# stop_server (lib.sh) only kills $MCP_SERVER_PID, which is empty in a fresh
# shell. Load the PID from the pidfile written by start-server.sh --bg, and
# confirm it still names the mcp-server binary before signalling — same
# guard setup-fixtures.sh uses to avoid killing a PID-recycled process.
MCP_PIDFILE="$BIN_DIR/mcp-server.pid"
if [ -f "$MCP_PIDFILE" ]; then
    pid=$(cat "$MCP_PIDFILE")
    if kill -0 "$pid" 2>/dev/null \
        && [ "$(ps -p "$pid" -o comm= 2>/dev/null | tr -d '[:space:]')" = "mcp-server" ]; then
        MCP_SERVER_PID="$pid"
    fi
fi

stop_server || true
cleanup_fixtures || true
rm -f "$MCP_PIDFILE"

log_ok "Fixtures torn down. Brokers still running."
echo "To stop brokers too:"
echo "  docker compose -f $E2E_DIR/docker-compose.yml down -v"
