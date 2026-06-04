#!/usr/bin/env bash
# Build the MCP server from latest source and start it against the E2E broker.
#
# Usage:
#   bash test/e2e-basic-mcp/start-server.sh           # foreground (Ctrl-C to stop)
#   bash test/e2e-basic-mcp/start-server.sh --bg      # background (prints PID, writes pidfile)
#
# The server runs on port 9090 by default. Override with MCP_PORT env var.
# Requires both Solace brokers to be running (ports 8080 and 8082).

set -euo pipefail
source "$(dirname "$0")/helpers.sh"

MODE="foreground"
if [ "${1:-}" = "--bg" ]; then
    MODE="background"
fi

# 1. Wait for both brokers
wait_for_all_brokers 90

# 2. Build from latest source
build_server

# 3. Write temp config
CONFIG_FILE=$(mktemp /tmp/e2e-config-XXXXXX.yaml)
write_config "$CONFIG_FILE"

cleanup_on_exit() {
    stop_server
    rm -f "$CONFIG_FILE"
}

if [ "$MODE" = "background" ]; then
    # Start in background, write pidfile + config path for external stop/cleanup
    start_server "$CONFIG_FILE"
    PIDFILE="$BIN_DIR/mcp-server.pid"
    CONFIGFILE_REF="$BIN_DIR/mcp-server.config"
    echo "$MCP_SERVER_PID" > "$PIDFILE"
    echo "$CONFIG_FILE" > "$CONFIGFILE_REF"
    log_info "Server running in background (PID=$MCP_SERVER_PID, pidfile=$PIDFILE)"
    log_info "Stop with: kill \$(cat $PIDFILE) && rm -f \$(cat $CONFIGFILE_REF) $CONFIGFILE_REF"
else
    # Foreground: start server, trap Ctrl-C for clean shutdown
    trap cleanup_on_exit EXIT INT TERM
    start_server "$CONFIG_FILE"
    log_info "Server running in foreground. Press Ctrl-C to stop."
    wait "$MCP_SERVER_PID" 2>/dev/null || true
fi
