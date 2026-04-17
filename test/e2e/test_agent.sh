#!/usr/bin/env bash
# Scenario 2: Go MCP SDK agent tests.
# Requires: MCP server running on $MCP_URL, broker fixtures created on both brokers.

set -euo pipefail
source "$(dirname "$0")/helpers.sh"

# ── Build agent ──────────────────────────────────────────────────────────────

build_agent() {
    log_info "Building agent binary ..."
    (cd "$SCRIPT_DIR/agent" && go build -o "$BIN_DIR/agent" .)
    log_info "Agent binary built: $BIN_DIR/agent"
}

# ── Tests ────────────────────────────────────────────────────────────────────

test_agent_run() {
    local output
    output=$("$BIN_DIR/agent" "$MCP_URL" 2>&1) || {
        log_fail "Agent exited with non-zero status"
        echo "$output"
        return 1
    }

    assert_contains "$output" "PASS" "Agent output should contain PASS" || return 1
    assert_contains "$output" "Both broker aliases found" "Agent should find both broker aliases" || return 1
    assert_contains "$output" "All 3 response sections present (broker-a)" "Agent should validate RDP on broker-a" || return 1
    assert_contains "$output" "All 3 response sections present (broker-b)" "Agent should validate RDP on broker-b" || return 1
    assert_contains "$output" "Queue metrics response valid (broker-a)" "Agent should validate queue metrics on broker-a" || return 1
    assert_contains "$output" "Queue metrics response valid (broker-b)" "Agent should validate queue metrics on broker-b" || return 1
}

# ── Run ──────────────────────────────────────────────────────────────────────

build_agent

run_test "Agent full run (both brokers)" test_agent_run

print_summary "Agent tests"
