#!/usr/bin/env bash
# E2E test runner for the monitoring tools.
# Assumes the Solace brokers are already running — start them with
# bash test/e2e-monitoring/setup-brokers.sh before running this script.
# This script: starts the MCP server, creates fixtures, runs cleanup on exit.

set -euo pipefail
source "$(dirname "$0")/helpers.sh"

# ── Cleanup on exit ─────────────────────────────────────────────────────────
# Order matters: stop_broker_drivers must run before cleanup_fixtures because
# the broker refuses to delete a queue with an attached client.
cleanup() {
    log_info "Running cleanup ..."
    stop_server
    stop_broker_drivers
    cleanup_fixtures
    rm -f "$BIN_DIR/mcp-server.pid"
}
trap cleanup EXIT INT TERM HUP

# ── Main ─────────────────────────────────────────────────────────────────────

log_info "=== Solace Broker MCP — E2E Test Suite ==="

# 1. Build and start MCP server in background
bash "$SCRIPT_DIR/start-server.sh" --bg

# Read back the PID so the cleanup trap can stop it
PIDFILE="$BIN_DIR/mcp-server.pid"
if [ -f "$PIDFILE" ]; then
    MCP_SERVER_PID=$(cat "$PIDFILE")
fi

# 2. Create test fixtures (F1 multi-VPN + F2 multi-queue)
create_fixtures

# 3. Real assertions land in step 13 (SEMP-direct fixture verification)
#    and SOL-150025 (tool-level tests). For now this is the scaffold only.

log_ok "Scaffold run complete (no assertions yet)"
