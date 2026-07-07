#!/usr/bin/env bash
# E2E test runner for the management (config API) tools.
# Assumes the Solace brokers are already running — start them with
# bash test/e2e-management/setup-brokers.sh before running this script.
# This script: starts a write-enabled MCP server, runs the config-tool tests,
# and sweeps any leftover config fixtures on exit.

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/helpers.sh"

# ── Cleanup on exit ─────────────────────────────────────────────────────────
# The tests own their fixtures per-test, but sweep here too as a safety net so a
# mid-run failure never leaks config objects into the next run.
cleanup() {
    log_info "Running cleanup ..."
    stop_server
    sweep_config_fixtures
    rm -f "$BIN_DIR/mcp-server.pid"
}
trap cleanup EXIT INT TERM HUP

# ── Main ─────────────────────────────────────────────────────────────────────

log_info "=== Solace Broker MCP — E2E Management (Config) Test Suite ==="

# 1. Build and start a write-enabled MCP server in the background.
bash "$SCRIPT_DIR/start-server.sh" --bg

# Read back the PID so the cleanup trap can stop it.
PIDFILE="$BIN_DIR/mcp-server.pid"
if [ -f "$PIDFILE" ]; then
    MCP_SERVER_PID=$(cat "$PIDFILE")
fi

# 2. Config-tool round-trip + cross-cutting tests through the MCP server.
bash "$SCRIPT_DIR/config-tool-tests.sh"
