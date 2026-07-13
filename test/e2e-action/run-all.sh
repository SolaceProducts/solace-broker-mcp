#!/usr/bin/env bash
# E2E test runner for the action tools (delete-queue-messages, clear-queue-stats,
# disconnect-client, clear-client-stats), driven over the MCP JSON-RPC wire.
# Assumes the Solace brokers are already running — start them with
# SUITE_DIR=test/e2e-action bash test/e2e-common/setup-brokers.sh before running.
# This script: builds the broker-driver, starts a write-enabled MCP server, runs
# the action-tool tests, and sweeps any leftover fixtures (and driver processes)
# on exit.

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/helpers.sh"

# ── Cleanup on exit ─────────────────────────────────────────────────────────
# Tests own their fixtures per-test, but sweep here too as a safety net so a
# mid-run failure never leaks queues or leaves a broker-driver client attached.
# sweep_action_fixtures reaps broker-driver processes before deleting queues.
cleanup() {
    log_info "Running cleanup ..."
    stop_server
    sweep_action_fixtures
    rm -f "$BIN_DIR/mcp-server.pid"
}
trap cleanup EXIT INT TERM HUP

# ── Main ─────────────────────────────────────────────────────────────────────

log_info "=== Solace Broker MCP — E2E Action Test Suite ==="

# 1. Build the broker-driver (CGo) — the action fixtures need messaging-layer
#    state (spooled messages, connected clients) that SEMP/curl cannot produce.
build_broker_driver

# 2. Build and start the write-enabled MCP server in the background.
bash "$SCRIPT_DIR/../e2e-common/start-server.sh" --bg

# Read back the PID so the cleanup trap can stop it.
PIDFILE="$BIN_DIR/mcp-server.pid"
if [ -f "$PIDFILE" ]; then
    MCP_SERVER_PID=$(cat "$PIDFILE")
fi

# 3. Action-tool state-change + cross-cutting tests through the MCP server.
bash "$SCRIPT_DIR/test-action-tools.sh"
