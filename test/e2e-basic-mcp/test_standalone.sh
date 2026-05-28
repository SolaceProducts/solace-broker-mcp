#!/usr/bin/env bash
# Scenario 1: Standalone raw curl MCP protocol tests.
# Requires: MCP server running on $MCP_URL, broker fixtures created on both brokers.

set -euo pipefail
source "$(dirname "$0")/helpers.sh"

# ── Tests ────────────────────────────────────────────────────────────────────

test_health_endpoint() {
    local response
    response=$(curl -sf "$MCP_URL/health")
    assert_json_field "$response" ".status" "healthy" "Health endpoint should return status healthy"
}

test_initialize() {
    local session_id
    session_id=$(mcp_initialize)
    [ -n "$session_id" ] || { log_fail "Empty session ID"; return 1; }
    log_info "  Got session: $session_id"
}

test_list_tools() {
    local session_id
    session_id=$(mcp_initialize)

    local response
    response=$(mcp_request "$session_id" '{
        "jsonrpc": "2.0",
        "id": 2,
        "method": "tools/list",
        "params": {}
    }')

    assert_contains "$response" "get-rdp-status" "tools/list should include get-rdp-status" || return 1
    assert_contains "$response" "list-brokers" "tools/list should include list-brokers" || return 1
    assert_contains "$response" "get-queue-metrics" "tools/list should include get-queue-metrics" || return 1
    assert_contains "$response" "get-client-details" "tools/list should include get-client-details" || return 1
    assert_contains "$response" "list-client-subscriptions" "tools/list should include list-client-subscriptions" || return 1
}

test_list_brokers() {
    local session_id
    session_id=$(mcp_initialize)

    local response
    response=$(mcp_request "$session_id" '{
        "jsonrpc": "2.0",
        "id": 3,
        "method": "tools/call",
        "params": {
            "name": "list-brokers",
            "arguments": {}
        }
    }')

    assert_contains "$response" "broker-a" "list-brokers should include 'broker-a'" || return 1
    assert_contains "$response" "broker-b" "list-brokers should include 'broker-b'" || return 1
}

# ── Broker A tests ───────────────────────────────────────────────────────────

test_get_rdp_status_broker_a() {
    local session_id
    session_id=$(mcp_initialize)

    local response
    response=$(mcp_request "$session_id" '{
        "jsonrpc": "2.0",
        "id": 4,
        "method": "tools/call",
        "params": {
            "name": "get-rdp-status",
            "arguments": {
                "broker": "broker-a",
                "msgVpnName": "default",
                "restDeliveryPointName": "test-rdp"
            }
        }
    }')

    assert_contains "$response" "rdpStatus" "Response should contain rdpStatus step" || return 1
    assert_contains "$response" "queueBindings" "Response should contain queueBindings step" || return 1
    assert_contains "$response" "restConsumers" "Response should contain restConsumers step" || return 1
    assert_contains "$response" "test-rdp" "Response should mention the RDP name" || return 1
}

test_get_rdp_status_not_found() {
    local session_id
    session_id=$(mcp_initialize)

    local response
    response=$(mcp_request "$session_id" '{
        "jsonrpc": "2.0",
        "id": 5,
        "method": "tools/call",
        "params": {
            "name": "get-rdp-status",
            "arguments": {
                "broker": "broker-a",
                "msgVpnName": "default",
                "restDeliveryPointName": "nonexistent-rdp"
            }
        }
    }')

    if echo "$response" | grep -q '"error"'; then
        return 0
    fi
    log_fail "Nonexistent RDP should return an error indication"
    log_fail "  Response: $(echo "$response" | head -3)"
    return 1
}

test_get_queue_metrics_broker_a() {
    local session_id
    session_id=$(mcp_initialize)

    local response
    response=$(mcp_request "$session_id" '{
        "jsonrpc": "2.0",
        "id": 6,
        "method": "tools/call",
        "params": {
            "name": "get-queue-metrics",
            "arguments": {
                "broker": "broker-a",
                "msgVpnName": "default",
                "queueName": "test-queue"
            }
        }
    }')

    assert_contains "$response" "queueMetrics" "Response should contain queueMetrics step" || return 1
    assert_contains "$response" "test-queue" "Response should mention the queue name" || return 1
}

# ── Broker B tests ───────────────────────────────────────────────────────────

test_get_rdp_status_broker_b() {
    local session_id
    session_id=$(mcp_initialize)

    local response
    response=$(mcp_request "$session_id" '{
        "jsonrpc": "2.0",
        "id": 7,
        "method": "tools/call",
        "params": {
            "name": "get-rdp-status",
            "arguments": {
                "broker": "broker-b",
                "msgVpnName": "default",
                "restDeliveryPointName": "test-rdp"
            }
        }
    }')

    assert_contains "$response" "rdpStatus" "Response should contain rdpStatus step" || return 1
    assert_contains "$response" "queueBindings" "Response should contain queueBindings step" || return 1
    assert_contains "$response" "restConsumers" "Response should contain restConsumers step" || return 1
    assert_contains "$response" "test-rdp" "Response should mention the RDP name" || return 1
}

test_get_queue_metrics_broker_b() {
    local session_id
    session_id=$(mcp_initialize)

    local response
    response=$(mcp_request "$session_id" '{
        "jsonrpc": "2.0",
        "id": 8,
        "method": "tools/call",
        "params": {
            "name": "get-queue-metrics",
            "arguments": {
                "broker": "broker-b",
                "msgVpnName": "default",
                "queueName": "test-queue"
            }
        }
    }')

    assert_contains "$response" "queueMetrics" "Response should contain queueMetrics step" || return 1
    assert_contains "$response" "test-queue" "Response should mention the queue name" || return 1
}

# ── Run ──────────────────────────────────────────────────────────────────────

run_test "Health endpoint"                    test_health_endpoint
run_test "MCP initialize"                    test_initialize
run_test "List tools"                        test_list_tools
run_test "List brokers (both)"               test_list_brokers
run_test "Get RDP status (broker-a)"         test_get_rdp_status_broker_a
run_test "Get RDP status not found"          test_get_rdp_status_not_found
run_test "Get queue metrics (broker-a)"      test_get_queue_metrics_broker_a
run_test "Get RDP status (broker-b)"         test_get_rdp_status_broker_b
run_test "Get queue metrics (broker-b)"      test_get_queue_metrics_broker_b

print_summary "Standalone tests"
