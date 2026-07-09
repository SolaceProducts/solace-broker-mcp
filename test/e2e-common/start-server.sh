#!/usr/bin/env bash
# Build the MCP server from latest source and start it against the E2E brokers.
#
# Usage:
#   SUITE_DIR=/path/to/suite bash test/e2e-common/start-server.sh [--bg]
#
# Options:
#   --bg  Run in background (writes pidfile); default is foreground
#
# SUITE_DIR must point to the suite directory containing helpers.sh.
# The server runs on MCP_PORT from the suite's .env (default 9090).

set -euo pipefail

# Parse arguments
MODE="foreground"
SUITE_DIR="${SUITE_DIR:-}"

for arg in "$@"; do
    case "$arg" in
        --bg) MODE="background" ;;
        *)
            echo "Unknown argument: $arg" >&2
            echo "Usage: SUITE_DIR=/path/to/suite $0 [--bg]" >&2
            exit 1
            ;;
    esac
done

if [ -z "$SUITE_DIR" ]; then
    echo "Usage: SUITE_DIR=/path/to/suite $0 [--bg]" >&2
    exit 1
fi
export SUITE_DIR

source "$SUITE_DIR/helpers.sh"

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

CONFIG_FILE=$(mktemp /tmp/e2e-config-XXXXXX.yaml)
write_config "$CONFIG_FILE"

if [ "$MODE" = "background" ]; then
    start_server "$CONFIG_FILE"
    PIDFILE="$BIN_DIR/mcp-server.pid"
    echo "$MCP_SERVER_PID" > "$PIDFILE"
    log_info "Server running in background (PID=$MCP_SERVER_PID, pidfile=$PIDFILE)"
    log_info "Stop with: kill \$(cat $PIDFILE)"
    # Clear so the trap's stop_server is a no-op and the server keeps running
    MCP_SERVER_PID=""
else
    start_server "$CONFIG_FILE"
    log_info "Server running in foreground. Press Ctrl-C to stop."
    wait "$MCP_SERVER_PID" 2>/dev/null || true
fi
