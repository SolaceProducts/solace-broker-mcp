#!/usr/bin/env bash
# E2E test runner for the OAuth token-exchange suite.
# Assumes: certs generated, brokers + Keycloak containers up and OAuth-profile
# configured (see the e2e-oauth-all Makefile target for the full sequence —
# this script only builds/starts the MCP server and runs the scenarios).

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/helpers.sh"

# Single cleanup trap, installed before any state is created.
#
# The generated configs are deliberately NOT deleted: they live under bin/
# (gitignored, rewritten every run) and are the only record of what each phase
# actually ran with, which CI collects on failure.
CONFIG_FILE=""
RBAC_CONFIG_FILE=""
cleanup() {
    log_info "Running cleanup ..."
    stop_server
    rm -f "$BIN_DIR/mcp-server.pid"
    # Explicit, so the trap never exits non-zero on its last command.
    return 0
}
trap cleanup EXIT INT TERM HUP

# Configs live under bin/ rather than $TMPDIR so a CI-only failure can still
# show what the server actually ran with — the RBAC phase's whole meaning is
# its tool_authorization block, and a deleted temp file makes that
# unrecoverable after the fact. The workflow collects bin/*.yaml on failure.
# They hold only the in-repo test-realm secret, same as the committed .env.
new_config_path() {
    echo "$BIN_DIR/mcp-config-$1.yaml"
}

log_info "=== Solace Broker MCP — E2E OAuth Token-Exchange Test Suite ==="

# Redundant with the Makefile's own pre-flight wait (safety net for standalone
# invocation — matches e2e-common/start-server.sh's own internal re-wait).
check_build_deps
wait_for_all_brokers 60
wait_for_keycloak 60

build_server

PIDFILE="$BIN_DIR/mcp-server.pid"

# Drop rotated server logs from previous runs so the ones left behind belong to
# this run only (restart_oauth_server appends .1, .2, ... as phases advance).
rm -f "$MCP_SERVER_LOG".[0-9]*

# Each phase runs even if an earlier one fails, so a hop-2 regression does not
# hide the tool-RBAC result (and vice versa). The run still exits non-zero if
# any phase failed. Per-phase results are recorded so the verdict table below
# says WHICH phase failed — with three phases, a bare exit 1 leaves the reader
# scrolling thousands of CI log lines to find out.
suite_status=0
phase1_result=SKIP
phase2_result=SKIP
phase3_result=SKIP

# ── Phases 1-2: hop-2 token exchange, and RBAC off ──────────────────────────
# The disabled-phase server carries the REAL policy with only the flag off
# (TOOL_AUTHZ_RBAC_OFF), not an empty block — see the constant's comment.
CONFIG_FILE=$(new_config_path "rbac-off")
write_oauth_config "$CONFIG_FILE" "$TOOL_AUTHZ_RBAC_OFF"

start_oauth_server "$CONFIG_FILE"
echo "$MCP_SERVER_PID" > "$PIDFILE"

if bash "$SCRIPT_DIR/test-oauth-scenarios.sh"; then phase1_result=PASS; else phase1_result=FAIL; suite_status=1; fi
if bash "$SCRIPT_DIR/test-rbac-scenarios.sh" disabled; then phase2_result=PASS; else phase2_result=FAIL; suite_status=1; fi

# ── Phase 3: tool RBAC enabled ──────────────────────────────────────────────
# The restart lives here rather than inside test-rbac-scenarios.sh because the
# scenario scripts run as child processes: a restart there would update only
# the child's MCP_SERVER_PID, and this script's cleanup would then kill a stale
# PID and leak the running server.
RBAC_CONFIG_FILE=$(new_config_path "rbac-on")
write_oauth_config "$RBAC_CONFIG_FILE" "$TOOL_AUTHZ_RBAC"

restart_oauth_server "$RBAC_CONFIG_FILE"
echo "$MCP_SERVER_PID" > "$PIDFILE"

if bash "$SCRIPT_DIR/test-rbac-scenarios.sh" enabled; then phase3_result=PASS; else phase3_result=FAIL; suite_status=1; fi

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  e2e-oauth phase verdict"
echo "    1  hop-2 token exchange (rbac off) : $phase1_result"
echo "    2  tool RBAC disabled no-op        : $phase2_result"
echo "    3  tool RBAC enabled               : $phase3_result"
if [ "$phase1_result" = "FAIL" ] && [ "$phase3_result" = "FAIL" ]; then
    echo "    note: phase 1 failed first — phases 2/3 may be cascade failures."
    echo "          Diagnose phase 1 before reading the rest."
fi
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

exit "$suite_status"
