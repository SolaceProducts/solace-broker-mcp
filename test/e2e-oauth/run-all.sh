#!/usr/bin/env bash
# E2E test runner for the OAuth token-exchange suite.
# Assumes: certs generated, brokers + Keycloak containers up and OAuth-profile
# configured (see the e2e-oauth-all Makefile target for the full sequence —
# this script only builds/starts the MCP server and runs the scenarios).

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/helpers.sh"

# Single cleanup trap, installed before any state is created (config file path
# is empty until write_oauth_config runs; the rm is a no-op until then).
CONFIG_FILE=""
cleanup() {
    log_info "Running cleanup ..."
    stop_server
    rm -f "$BIN_DIR/mcp-server.pid"
    [ -n "$CONFIG_FILE" ] && rm -f "$CONFIG_FILE"
}
trap cleanup EXIT INT TERM HUP

log_info "=== Solace Broker MCP — E2E OAuth Token-Exchange Test Suite ==="

# Redundant with the Makefile's own pre-flight wait (safety net for standalone
# invocation — matches e2e-common/start-server.sh's own internal re-wait).
check_build_deps
wait_for_all_brokers 60
wait_for_keycloak 60

build_server

CONFIG_FILE=$(mktemp /tmp/e2e-oauth-config-XXXXXX.yaml)
write_oauth_config "$CONFIG_FILE"

start_oauth_server "$CONFIG_FILE"
PIDFILE="$BIN_DIR/mcp-server.pid"
echo "$MCP_SERVER_PID" > "$PIDFILE"

bash "$SCRIPT_DIR/test-oauth-scenarios.sh"
