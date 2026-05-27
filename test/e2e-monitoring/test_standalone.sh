#!/usr/bin/env bash
# Scenario 1: Standalone raw curl MCP protocol tests.
# Requires: MCP server running on $MCP_URL, broker fixtures created on both brokers.

set -euo pipefail
source "$(dirname "$0")/helpers.sh"

# ── Tests ────────────────────────────────────────────────────────────────────

test_health_endpoint() {
    local response
    response=$(curl -sf "$MCP_URL/health")
    assert_json_field "$response" ".status" "ok" "Health endpoint should return status ok"
}

test_initialize() {
    local session_id
    session_id=$(mcp_initialize)
    [ -n "$session_id" ] || { log_fail "Empty session ID"; return 1; }
    log_info "  Got session: $session_id"
}

test_list_tools() {
    local sid response
    sid=$(mcp_initialize)
    response=$(mcp_request "$sid" '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}')
    for tool in get-rdp-status list-brokers get-queue-metrics get-client-details list-client-subscriptions; do
        assert_contains "$response" "$tool" "tools/list should include $tool" || return 1
    done
}

test_list_brokers() {
    local response
    response=$(mcp_call_tool "list-brokers" '{}')
    assert_contains "$response" "broker-a" "list-brokers should include 'broker-a'" || return 1
    assert_contains "$response" "broker-b" "list-brokers should include 'broker-b'" || return 1
}

# ── Per-broker tests (parameterized) ─────────────────────────────────────────

_check_rdp_status() {
    local broker="$1"
    local args response
    args=$(jq -nc --arg b "$broker" \
        '{broker:$b,msgVpnName:"default",restDeliveryPointName:"test-rdp"}')
    response=$(mcp_call_tool "get-rdp-status" "$args")
    assert_contains "$response" "rdpStatus"     "Response should contain rdpStatus step"     || return 1
    assert_contains "$response" "queueBindings" "Response should contain queueBindings step" || return 1
    assert_contains "$response" "restConsumers" "Response should contain restConsumers step" || return 1
    assert_contains "$response" "test-rdp"      "Response should mention the RDP name"       || return 1
}

_check_queue_metrics() {
    local broker="$1"
    local args response
    args=$(jq -nc --arg b "$broker" \
        '{broker:$b,msgVpnName:"default",queueName:"test-queue"}')
    response=$(mcp_call_tool "get-queue-metrics" "$args")
    assert_contains "$response" "queueMetrics" "Response should contain queueMetrics step" || return 1
    assert_contains "$response" "test-queue"   "Response should mention the queue name"    || return 1
}

test_get_rdp_status_broker_a()    { _check_rdp_status broker-a; }
test_get_rdp_status_broker_b()    { _check_rdp_status broker-b; }
test_get_queue_metrics_broker_a() { _check_queue_metrics broker-a; }
test_get_queue_metrics_broker_b() { _check_queue_metrics broker-b; }

test_get_rdp_status_not_found() {
    local args response
    args=$(jq -nc \
        '{broker:"broker-a",msgVpnName:"default",restDeliveryPointName:"nonexistent-rdp"}')
    response=$(mcp_call_tool "get-rdp-status" "$args")
    if echo "$response" | grep -q '"error"'; then
        return 0
    fi
    log_fail "Nonexistent RDP should return an error indication"
    log_fail "  Response: $(echo "$response" | head -3)"
    return 1
}

# ── Run ──────────────────────────────────────────────────────────────────────

run_test "Health endpoint"                   test_health_endpoint
run_test "MCP initialize"                    test_initialize
run_test "List tools"                        test_list_tools
run_test "List brokers (both)"               test_list_brokers
run_test "Get RDP status (broker-a)"         test_get_rdp_status_broker_a
run_test "Get RDP status not found"          test_get_rdp_status_not_found
run_test "Get queue metrics (broker-a)"      test_get_queue_metrics_broker_a
run_test "Get RDP status (broker-b)"         test_get_rdp_status_broker_b
run_test "Get queue metrics (broker-b)"      test_get_queue_metrics_broker_b

print_summary "Standalone tests"
