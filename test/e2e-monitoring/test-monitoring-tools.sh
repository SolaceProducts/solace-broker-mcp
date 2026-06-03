#!/usr/bin/env bash
# E2E test runner for the monitoring tools.
# Assumes the Solace brokers are already running — start them with
# bash test/e2e-monitoring/setup-brokers.sh before running this script.
# This script: starts the MCP server, creates fixtures, runs cleanup on exit.

set -euo pipefail
source "$(dirname "$0")/helpers.sh"

# ── Cleanup on exit ─────────────────────────────────────────────────────────
# cleanup_fixtures handles ordering internally: it calls stop_broker_drivers
# first so no client is attached when SEMP queue deletes run.
cleanup() {
    log_info "Running cleanup ..."
    stop_server
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

# 2. Build broker-driver (CGo + libsolclient) — needed by F3 onward.
build_broker_driver

# 3. Create test fixtures (F1 multi-VPN, F2 multi-queue, F3 connected client)
create_fixtures

# 4. SEMP-direct fixture-state verification (SOL-150024 acceptance criteria).
bash "$SCRIPT_DIR/verify-fixtures.sh"

# 5. Tool-level functional tests through the MCP server (SOL-150025).
bash "$SCRIPT_DIR/tool-tests.sh"
