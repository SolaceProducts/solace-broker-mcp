#!/usr/bin/env bash
# Bootstrap brokers + fixtures + MCP server for the LLM-scenario suite.
#
# Wraps the e2e-monitoring helpers (containers, fixtures, broker-driver, MCP
# server) but, unlike test-monitoring-tools.sh, installs NO cleanup trap —
# the long-running fixtures (F3 receiver, F4 publisher, F5 slow consumer,
# F6 slow subscriber) stay running so the LLM scenarios can observe live
# broker state. Tear everything down with ./teardown-fixtures.sh.
#
# Safe to re-run: create_fixtures starts with cleanup_fixtures, so partial
# state from a previous run is removed before reprovisioning.

set -euo pipefail

LLM_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMMON_DIR="$(cd "$LLM_DIR/../e2e-common" && pwd)"

# shellcheck disable=SC1091
source "$LLM_DIR/config.env"

# Non-local-docker targets are assumed to be running already — the MCP
# server, brokers, and any fixtures are the operator's responsibility.
# Skip cleanly so a CI workflow can call setup unconditionally.
if [ "$BROKER_TARGET" != "local-docker" ]; then
    echo "[INFO] BROKER_TARGET=$BROKER_TARGET — skipping local fixture provisioning"
    echo "[INFO] expecting MCP server already reachable at $MCP_URL"
    exit 0
fi

# Pass SUITE_DIR=$LLM_DIR so setup-brokers.sh uses this suite's own
# docker-compose.yml (containers named solace-e2e-llm-a/b) and this
# suite's .env / bin/ / ports — not the monitoring suite's.
SUITE_DIR="$LLM_DIR" bash "$COMMON_DIR/setup-brokers.sh"

# Source LLM helpers, which set SUITE_DIR=$LLM_DIR before pulling in the
# monitoring suite's create_fixtures/cleanup_fixtures/F1–F7 helpers and
# layering fixtures.sh on top. Safe today because e2e-management/helpers.sh
# only defines sweep_config_fixtures (no name overlap). SOL-150727 will
# layer in management helpers for write-tool scenarios — if either side
# adds a colliding function name (create_fixtures, cleanup_fixtures, etc.)
# the second source silently redefines the first with no warning.
# Namespace new helpers (mon_ / mgmt_) or pull shared ones into
# e2e-common/lib.sh before that lands.
# shellcheck disable=SC1091
source "$LLM_DIR/helpers.sh"

build_broker_driver

# Reuse the MCP server if a healthy one is already running; otherwise start it.
# Trust the PID file written by start-server.sh:46-47 over a `pgrep -f` match,
# which can also hit unrelated `grep mcp-server` processes. Also confirm the
# PID still names the mcp-server binary — `kill -0` alone is satisfied by any
# PID-recycled process (rare, but the wrong-process case is silent and weird
# to debug).
MCP_PIDFILE="$BIN_DIR/mcp-server.pid"
mcp_pid_is_server() {
    local pid="$1" comm
    kill -0 "$pid" 2>/dev/null || return 1
    comm=$(ps -p "$pid" -o comm= 2>/dev/null | tr -d '[:space:]')
    [ "$comm" = "mcp-server" ]
}
if [ -f "$MCP_PIDFILE" ] && mcp_pid_is_server "$(cat "$MCP_PIDFILE")"; then
    log_info "MCP server already running (PID=$(cat "$MCP_PIDFILE")) — reusing"
else
    SUITE_DIR="$LLM_DIR" bash "$COMMON_DIR/start-server.sh" --bg
fi

create_fixtures

# Mode-2 write/destructive-tool scenarios (a2, a3, b1, b3, b4, b5) reach for
# LLM-specific standing objects (e2e-llm-action-queue-broker-{a,b},
# e2e-llm-kick-target-{a,b}). Provision AFTER create_fixtures so build_broker_driver
# has produced $BIN_DIR/broker-driver — the kick-target client needs it to hold
# its long-lived connection open.
create_llm_standing_fixtures

log_ok "Fixtures provisioned. Long-running drivers:"
ls "$BIN_DIR"/broker-driver-f*.pid 2>/dev/null || echo "  (none — F3/F4/F5/F6 expected)"
echo
log_ok "Ready. Run scenarios with:"
echo "  ./run-all.sh"
echo "  ./run-scenario.sh scenarios/f1-list-vpns.json"
echo
log_warn "Fixtures (esp. F6 slow-subscriber) are SIGSTOP'd and will leak as"
log_warn "reparented-to-init processes if not torn down. When finished, run:"
echo "  ./teardown-fixtures.sh"
