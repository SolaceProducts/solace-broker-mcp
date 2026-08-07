#!/usr/bin/env bash
# Scenario 1: Standalone raw curl MCP protocol tests.
# Requires: MCP server running on $MCP_URL, broker fixtures created on both brokers.

set -euo pipefail
source "$(dirname "$0")/helpers.sh"

# ── Tests ────────────────────────────────────────────────────────────────────

test_health_endpoint() {
    # /health preserves its original back-compat body (status=healthy); /livez is
    # the canonical liveness endpoint (status=alive).
    local response
    response=$(curl -sf "$MCP_URL/health")
    assert_json_field "$response" ".status" "healthy" "/health should return status healthy (legacy back-compat body)"
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
    assert_contains "$response" "describe-semp-schema" "tools/list should include describe-semp-schema" || return 1
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

test_describe_semp_schema_create_queue() {
    local response content
    response=$(mcp_call_tool "describe-semp-schema" \
        '{"operation":"config/createMsgVpnQueue"}') || return 1
    content=$(extract_content "$response")

    assert_json_field "$content" '.operation' "config/createMsgVpnQueue" \
        "response should echo the requested operation" || return 1
    assert_json_field "$content" '.method' "POST" \
        "createMsgVpnQueue is a POST" || return 1
    assert_json_field "$content" '(.attributes | length) > 0' "true" \
        "trimmed view must carry a non-empty attributes array" || return 1
    # queueName is the identifying attribute on this op; if it disappears the
    # trimmed view is broken in a way the unit tests wouldn't catch.
    assert_json_field "$content" '[.attributes[].name] | index("queueName") != null' "true" \
        "attributes must include queueName" || return 1
}

test_describe_semp_schema_monitor_rejected() {
    # Monitor operations are not indexed (config/action only). Locks in the
    # scope guarantee documented in tools-reference.md and the CHANGELOG.
    local response
    response=$(mcp_call_tool "describe-semp-schema" \
        '{"operation":"monitor/getMsgVpn"}') || return 1

    assert_contains "$response" "unknown operation" \
        "monitor/... operations must surface 'unknown operation'" || return 1
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

    # The error must be translated end-to-end (SOL-148434), not just surfaced as a
    # raw failure: flagged as an error, classified non-retryable, carrying a
    # human-friendly message, an actionable suggestion, and the original SEMP code
    # preserved for debugging.
    assert_json_field "$response" ".result.isError" "true" \
        "Nonexistent RDP should return an error result" || return 1
    assert_json_field "$response" ".result.structuredContent.retryable" "false" \
        "A not-found error is deterministic, so retryable should be false" || return 1
    assert_json_field "$response" ".result.structuredContent.sempCode" "6" \
        "Original SEMP NOT_FOUND code (6) should be preserved for debugging" || return 1
    assert_json_field "$response" ".result.structuredContent.sempStatus" "NOT_FOUND" \
        "Original SEMP status should be preserved" || return 1
    assert_json_field "$response" '.result.structuredContent.suggestions[0]' "Verify the name is correct." \
        "Error should include an actionable suggestion" || return 1
    assert_contains "$response" "restDeliveryPoint nonexistent-rdp" \
        "Translated message should name the object that was not found" || return 1
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

# ── SOL-151519: get-rdp-status summary aggregation ───────────────────────────
# Recompute each summary count from the raw queueBindings / restConsumers rows
# and require equality. The existing get-rdp-status tests above target test-rdp
# (disabled) — that RDP's bindings/consumers all report up=false but with an
# empty lastFailureReason, so its by{Binding,Consumer}LastFailureReason maps
# would always be empty. Target test-rdp-failing (enabled, unreachable remote)
# instead so lastFailureReason is populated and the grouped maps carry ≥ 1
# entry, exercising the code path the aggregation is designed for.
test_get_rdp_status_summary() {
    local broker="$1"
    local label="get-rdp-status [$broker]"
    local response content
    response=$(mcp_call_tool "get-rdp-status" \
        "$(jq -nc --arg b "$broker" '{broker:$b,msgVpnName:"default",restDeliveryPointName:"test-rdp-failing"}')") || return 1
    content=$(extract_content "$response")

    # Handler emits flat summary keys (no nesting), so the recompute helper's
    # `.summary.<field>` lookup lands correctly.
    # RequiredFields per step: up, lastFailureReason.
    local bindings_wt='[.queueBindings.data[] | select((.up | type) == "boolean" and (.lastFailureReason | type) == "string")]'
    local consumers_wt='[.restConsumers.data[] | select((.up | type) == "boolean" and (.lastFailureReason | type) == "string")]'

    # Binding counts
    assert_recompute_count "$content" "$label" "$bindings_wt" "bindingUpCount" \
        '.up == true' || return 1
    assert_recompute_count "$content" "$label" "$bindings_wt" "bindingDownCount" \
        '.up == false' || return 1
    assert_recompute_group "$content" "$label" "$bindings_wt" "byBindingLastFailureReason" \
        '.up == false and .lastFailureReason != ""' '.lastFailureReason' || return 1
    assert_json_field "$content" \
        '(.summary.bindingScannedCount) == (.queueBindings.data | length)' "true" \
        "$label: summary.bindingScannedCount must equal len(queueBindings.data)" || return 1

    # Consumer counts
    assert_recompute_count "$content" "$label" "$consumers_wt" "consumerUpCount" \
        '.up == true' || return 1
    assert_recompute_count "$content" "$label" "$consumers_wt" "consumerDownCount" \
        '.up == false' || return 1
    assert_recompute_group "$content" "$label" "$consumers_wt" "byConsumerLastFailureReason" \
        '.up == false and .lastFailureReason != ""' '.lastFailureReason' || return 1
    assert_json_field "$content" \
        '(.summary.consumerScannedCount) == (.restConsumers.data | length)' "true" \
        "$label: summary.consumerScannedCount must equal len(restConsumers.data)" || return 1

    # Non-zero coverage: test-rdp-failing has 1 queueBinding + 1 restConsumer,
    # both down with a populated lastFailureReason. Each guard catches a
    # different vacuous-0==0 pass in the handler.
    assert_json_field "$content" \
        '(.summary.bindingDownCount) >= 1' "true" \
        "$label: at least one down queueBinding expected (fixture: test-rdp-failing)" || return 1
    assert_json_field "$content" \
        '(.summary.consumerDownCount) >= 1' "true" \
        "$label: at least one down restConsumer expected (fixture: test-rdp-failing)" || return 1
    assert_json_field "$content" \
        '(.summary.byBindingLastFailureReason | length) >= 1' "true" \
        "$label: byBindingLastFailureReason must have at least one entry (fixture: test-rdp-failing)" || return 1
    assert_json_field "$content" \
        '(.summary.byConsumerLastFailureReason | length) >= 1' "true" \
        "$label: byConsumerLastFailureReason must have at least one entry (fixture: test-rdp-failing)" || return 1
}

test_get_rdp_status_summary_a() { test_get_rdp_status_summary "broker-a"; }
test_get_rdp_status_summary_b() { test_get_rdp_status_summary "broker-b"; }

# ── Run ──────────────────────────────────────────────────────────────────────

run_test "Health endpoint"                         test_health_endpoint
run_test "MCP initialize"                          test_initialize
run_test "List tools"                              test_list_tools
run_test "List brokers (both)"                     test_list_brokers
run_test "describe-semp-schema create queue"       test_describe_semp_schema_create_queue
run_test "describe-semp-schema monitor rejected"   test_describe_semp_schema_monitor_rejected
run_test "Get RDP status (broker-a)"               test_get_rdp_status_broker_a
run_test "Get RDP status not found"                test_get_rdp_status_not_found
run_test "Get queue metrics (broker-a)"            test_get_queue_metrics_broker_a
run_test "Get RDP status (broker-b)"               test_get_rdp_status_broker_b
run_test "Get queue metrics (broker-b)"            test_get_queue_metrics_broker_b
run_test "Get RDP status summary (broker-a)"       test_get_rdp_status_summary_a
run_test "Get RDP status summary (broker-b)"       test_get_rdp_status_summary_b

print_summary "Standalone tests"
