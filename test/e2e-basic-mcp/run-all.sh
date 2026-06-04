#!/usr/bin/env bash
# Master E2E test runner.
# Assumes the Solace broker is already running (start it manually or via docker compose).
# This script: builds and starts the MCP server, creates fixtures, runs both test scenarios, cleans up.

set -euo pipefail
source "$(dirname "$0")/helpers.sh"

# ── Results collection ──────────────────────────────────────────────────────
export E2E_RESULTS_DIR
E2E_RESULTS_DIR=$(mktemp -d /tmp/e2e-results-XXXXXX)

# ── Cleanup on exit ─────────────────────────────────────────────────────────
cleanup() {
    log_info "Running cleanup ..."
    stop_server
    cleanup_fixtures
    rm -f "$BIN_DIR/mcp-server.pid"
    rm -rf "$E2E_RESULTS_DIR"
}
trap cleanup EXIT

# ── Main ─────────────────────────────────────────────────────────────────────

log_info "=== Solace Broker MCP — E2E Test Suite ==="
echo ""

# 1. Build and start MCP server via start-server.sh --bg
bash "$SCRIPT_DIR/start-server.sh" --bg

# Read back the PID so our cleanup trap can stop it
PIDFILE="$BIN_DIR/mcp-server.pid"
if [ -f "$PIDFILE" ]; then
    MCP_SERVER_PID=$(cat "$PIDFILE")
fi

# 2. Create test fixtures
create_fixtures

# 3. Run Scenario 1: Standalone
log_info ""
log_info "=== Scenario 1: Standalone (curl) ==="
log_info ""
if bash "$SCRIPT_DIR/test-standalone.sh"; then
    STANDALONE_EXIT=0
else
    STANDALONE_EXIT=$?
fi

# 4. Run Scenario 2: Agent
log_info ""
log_info "=== Scenario 2: Agent (Go MCP SDK) ==="
log_info ""
if bash "$SCRIPT_DIR/test-agent.sh"; then
    AGENT_EXIT=0
else
    AGENT_EXIT=$?
fi

# 5. Summary table
TOTAL_RUN=0
TOTAL_PASSED=0
TOTAL_FAILED=0
SAW_STANDALONE=0
SAW_AGENT=0

echo ""
echo "┏━━━━━━━━━━━━━━━━━━━━━━━━━┳━━━━━━━┳━━━━━━━━━┳━━━━━━━━━┓"
echo "┃ Scenario                ┃  Run  ┃ Passed  ┃ Failed  ┃"
echo "┣━━━━━━━━━━━━━━━━━━━━━━━━━╋━━━━━━━╋━━━━━━━━━╋━━━━━━━━━┫"

if [ -f "$E2E_RESULTS_DIR/results.txt" ]; then
    while IFS='|' read -r label run passed failed; do
        printf "┃ %-23s ┃ %5s ┃ %7s ┃ %7s ┃\n" "$label" "$run" "$passed" "$failed"
        TOTAL_RUN=$((TOTAL_RUN + run))
        TOTAL_PASSED=$((TOTAL_PASSED + passed))
        TOTAL_FAILED=$((TOTAL_FAILED + failed))
        case "$label" in
            "Standalone tests") SAW_STANDALONE=1 ;;
            "Agent tests")      SAW_AGENT=1      ;;
        esac
    done < "$E2E_RESULTS_DIR/results.txt"
fi

# Scenarios that exited non-zero before reaching print_summary leave no line in
# results.txt — emit a placeholder row so they don't silently vanish from the table.
if [ "$SAW_STANDALONE" -eq 0 ] && [ "$STANDALONE_EXIT" -ne 0 ]; then
    printf "┃ %-23s ┃ %5s ┃ %7s ┃ %7s ┃\n" "Standalone tests" "CRASH" "--" "--"
fi
if [ "$SAW_AGENT" -eq 0 ] && [ "$AGENT_EXIT" -ne 0 ]; then
    printf "┃ %-23s ┃ %5s ┃ %7s ┃ %7s ┃\n" "Agent tests" "CRASH" "--" "--"
fi

echo "┣━━━━━━━━━━━━━━━━━━━━━━━━━╋━━━━━━━╋━━━━━━━━━╋━━━━━━━━━┫"
printf "┃ %-23s ┃ %5s ┃ %7s ┃ %7s ┃\n" "TOTAL" "$TOTAL_RUN" "$TOTAL_PASSED" "$TOTAL_FAILED"
echo "┗━━━━━━━━━━━━━━━━━━━━━━━━━┻━━━━━━━┻━━━━━━━━━┻━━━━━━━━━┛"
echo ""

if [ "$STANDALONE_EXIT" -eq 0 ] && [ "$AGENT_EXIT" -eq 0 ]; then
    log_ok "All E2E scenarios passed"
    exit 0
else
    [ "$STANDALONE_EXIT" -ne 0 ] && log_fail "Standalone scenario failed"
    [ "$AGENT_EXIT" -ne 0 ] && log_fail "Agent scenario failed"
    exit 1
fi
