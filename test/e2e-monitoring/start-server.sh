#!/usr/bin/env bash
# Build the MCP server from latest source and start it against the E2E brokers.
#
# Usage:
#   bash test/e2e-monitoring/start-server.sh           # foreground (Ctrl-C to stop)
#   bash test/e2e-monitoring/start-server.sh --bg      # background (prints PID, writes pidfile)
#
# The server runs on port 9090 by default. Override with MCP_PORT env var.
# Requires both Solace brokers to be running (SEMP ports per test/e2e-monitoring/.env,
# defaults: broker-a=8090, broker-b=8092).

set -euo pipefail
source "$(dirname "$0")/helpers.sh"

MODE="foreground"
if [ "${1:-}" = "--bg" ]; then
    MODE="background"
fi

# Install the cleanup trap before any state is created so a failure in
# build_server or wait_for_all_brokers (or a Ctrl-C between mktemp and the
# happy-path trap install) still removes the temp config. cleanup_on_exit
# tolerates an empty CONFIG_FILE/MCP_SERVER_PID.
CONFIG_FILE=""
cleanup_on_exit() {
    stop_server
    [ -n "$CONFIG_FILE" ] && rm -f "$CONFIG_FILE"
}
trap cleanup_on_exit EXIT INT TERM HUP

# 1. Warn early if native build deps are missing
check_build_deps

# 2. Wait for both brokers
wait_for_all_brokers 90

# 3. Build from latest source
build_server

# 4. Write temp config
CONFIG_FILE=$(mktemp /tmp/e2e-config-XXXXXX.yaml)
write_config "$CONFIG_FILE"

if [ "$MODE" = "background" ]; then
    # On the happy path we detach by clearing MCP_SERVER_PID so the trap's
    # stop_server is a no-op (it gates on that variable) and the server keeps
    # running; the trap still cleans up the temp config.
    start_server "$CONFIG_FILE"
    PIDFILE="$BIN_DIR/mcp-server.pid"
    echo "$MCP_SERVER_PID" > "$PIDFILE"
    log_info "Server running in background (PID=$MCP_SERVER_PID, pidfile=$PIDFILE)"
    log_info "Stop with: kill \$(cat $PIDFILE)"
    MCP_SERVER_PID=""
else
    # Foreground: start server; the trap installed above handles Ctrl-C.
    start_server "$CONFIG_FILE"
    log_info "Server running in foreground. Press Ctrl-C to stop."
    wait "$MCP_SERVER_PID" 2>/dev/null || true
fi
