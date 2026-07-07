#!/usr/bin/env bash
# Build the MCP server from latest source and start it against the management
# brokers with write tools ENABLED (so the config tools register).
#
# Usage:
#   bash test/e2e-management/start-server.sh           # foreground (Ctrl-C to stop)
#   bash test/e2e-management/start-server.sh --bg      # background (writes pidfile)
#
# The server runs on the MCP_PORT from .env (default 9091). Requires both
# management brokers to be running (SEMP ports per test/e2e-management/.env).

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/helpers.sh"

MODE="foreground"
if [ "${1:-}" = "--bg" ]; then
    MODE="background"
fi

# Install the cleanup trap before any state is created so a failure in
# build_server or wait_for_all_brokers still removes the temp config.
CONFIG_FILE=""
cleanup_on_exit() {
    stop_server
    [ -n "$CONFIG_FILE" ] && rm -f "$CONFIG_FILE"
}
trap cleanup_on_exit EXIT INT TERM HUP

check_build_deps
wait_for_all_brokers 90
build_server

CONFIG_FILE=$(mktemp /tmp/e2e-mgmt-config-XXXXXX.yaml)
# Write tools ENABLED — the config tools (create/update/delete-*) are gated
# behind enable_write_tools and would not register otherwise.
write_config "$CONFIG_FILE" true

if [ "$MODE" = "background" ]; then
    # On the happy path we detach by clearing MCP_SERVER_PID so the trap's
    # stop_server is a no-op and the server keeps running; the trap still
    # cleans up the temp config.
    start_server "$CONFIG_FILE"
    PIDFILE="$BIN_DIR/mcp-server.pid"
    echo "$MCP_SERVER_PID" > "$PIDFILE"
    log_info "Server running in background (PID=$MCP_SERVER_PID, pidfile=$PIDFILE)"
    log_info "Stop with: kill \$(cat $PIDFILE)"
    MCP_SERVER_PID=""
else
    start_server "$CONFIG_FILE"
    log_info "Server running in foreground. Press Ctrl-C to stop."
    wait "$MCP_SERVER_PID" 2>/dev/null || true
fi
