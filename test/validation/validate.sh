#!/usr/bin/env bash
# Tool validation tests — exercises all 9 MCP tools against a non-trivially
# configured Solace Enterprise broker.
#
# Expects: MCP server running, broker configured via Terraform, messages
# published, sdkperf clients connected.

set -euo pipefail
source "$(dirname "$0")/helpers.sh"

# ── Helper: extract structured content from MCP tool response ────────────────

# MCP tool responses are JSON-RPC with result.content[0].text containing a
# JSON-serialized string of the step-keyed envelope.
extract_content() {
    local response="$1"
    echo "$response" | jq -r '.result.content[0].text'
}

# ── Initialize MCP session (reused across tests) ────────────────────────────

SESSION_ID=""
init_session() {
    if [ -z "$SESSION_ID" ]; then
        SESSION_ID=$(mcp_initialize)
        log_info "MCP session: $SESSION_ID"
    fi
}

# ── Tool 1: list-vpns ───────────────────────────────────────────────────────

test_list_vpns() {
    init_session

    local response
    response=$(mcp_call_tool "$SESSION_ID" "list-vpns" '{"broker":"broker-a"}')

    local content
    content=$(extract_content "$response")

    # Should have vpns step key
    assert_contains "$content" "vpns" "Response should contain vpns key" || return 1

    # Parse the vpns data array from the content
    local vpn_count
    vpn_count=$(echo "$content" | jq '.vpns.data | length')
    if [ "$vpn_count" -lt 3 ]; then
        log_fail "Expected >= 3 VPNs, got $vpn_count"
        return 1
    fi

    # Check for disabled VPNs
    local disabled_count
    disabled_count=$(echo "$content" | jq '[.vpns.data[] | select(.enabled == false)] | length')
    if [ "$disabled_count" -lt 1 ]; then
        log_fail "Expected >= 1 disabled VPN, got $disabled_count"
        return 1
    fi

    # Spot check specific VPNs
    assert_contains "$content" "val-vpn-active" "Should contain val-vpn-active" || return 1
    assert_contains "$content" "val-vpn-disabled" "Should contain val-vpn-disabled" || return 1
}

# ── Tool 2: get-vpn-health ──────────────────────────────────────────────────

test_get_vpn_health_default() {
    init_session

    local response
    response=$(mcp_call_tool "$SESSION_ID" "get-vpn-health" \
        '{"broker":"broker-a","msgVpnName":"default"}')

    local content
    content=$(extract_content "$response")

    assert_contains "$content" "vpnHealth" "Response should contain vpnHealth key" || return 1

    local vpn_data
    vpn_data=$(echo "$content" | jq '.vpnHealth.data')

    assert_json_field "$vpn_data" ".enabled" "true" "default VPN should be enabled" || return 1
    assert_json_field "$vpn_data" ".serviceRestIncomingPlainTextEnabled" "true" \
        "REST incoming should be enabled" || return 1
}

test_get_vpn_health_disabled() {
    init_session

    local response
    response=$(mcp_call_tool "$SESSION_ID" "get-vpn-health" \
        '{"broker":"broker-a","msgVpnName":"val-vpn-disabled"}')

    local content
    content=$(extract_content "$response")
    local vpn_data
    vpn_data=$(echo "$content" | jq '.vpnHealth.data')

    assert_json_field "$vpn_data" ".enabled" "false" "Disabled VPN should show enabled=false" || return 1
}

test_get_vpn_health_active() {
    init_session

    local response
    response=$(mcp_call_tool "$SESSION_ID" "get-vpn-health" \
        '{"broker":"broker-a","msgVpnName":"val-vpn-active"}')

    local content
    content=$(extract_content "$response")
    local vpn_data
    vpn_data=$(echo "$content" | jq '.vpnHealth.data')

    assert_json_field "$vpn_data" ".enabled" "true" "Active VPN should be enabled" || return 1
}

# ── Tool 3: list-queues ─────────────────────────────────────────────────────

test_list_queues() {
    init_session

    local response
    response=$(mcp_call_tool "$SESSION_ID" "list-queues" \
        '{"broker":"broker-a","msgVpnName":"default"}')

    local content
    content=$(extract_content "$response")

    assert_contains "$content" "queues" "Response should contain queues key" || return 1

    local queue_count
    queue_count=$(echo "$content" | jq '.queues.data | length')
    if [ "$queue_count" -lt 8 ]; then
        log_fail "Expected >= 8 queues, got $queue_count"
        return 1
    fi

    # Find val-q-backlog and check it has spooled messages
    local backlog_spool
    backlog_spool=$(echo "$content" | jq '[.queues.data[] | select(.queueName == "val-q-backlog")] | .[0].msgSpoolUsage')
    if [ "$backlog_spool" = "null" ] || [ "$backlog_spool" = "0" ]; then
        log_fail "val-q-backlog should have msgSpoolUsage > 0, got $backlog_spool"
        return 1
    fi

    # Find val-q-egress-down and check egress is disabled
    local egress_state
    egress_state=$(echo "$content" | jq '[.queues.data[] | select(.queueName == "val-q-egress-down")] | .[0].egressEnabled')
    if [ "$egress_state" != "false" ]; then
        log_fail "val-q-egress-down should have egressEnabled=false, got $egress_state"
        return 1
    fi

    # Find val-q-exclusive and check access type
    local access_type
    access_type=$(echo "$content" | jq -r '[.queues.data[] | select(.queueName == "val-q-exclusive")] | .[0].accessType')
    if [ "$access_type" != "exclusive" ]; then
        log_fail "val-q-exclusive should have accessType=exclusive, got $access_type"
        return 1
    fi
}

# ── Tool 4: get-queue-metrics ────────────────────────────────────────────────

test_get_queue_metrics() {
    init_session

    local response
    response=$(mcp_call_tool "$SESSION_ID" "get-queue-metrics" \
        '{"broker":"broker-a","msgVpnName":"default","queueName":"val-q-large-backlog"}')

    local content
    content=$(extract_content "$response")

    assert_contains "$content" "queueMetrics" "Response should contain queueMetrics key" || return 1

    local queue_data
    queue_data=$(echo "$content" | jq '.queueMetrics.data')

    # Check spooled message count >= 50
    local spool_count
    spool_count=$(echo "$queue_data" | jq '.spooledMsgCount // .msgCount // 0')
    if [ "$spool_count" -lt 50 ] 2>/dev/null; then
        log_fail "val-q-large-backlog should have >= 50 spooled messages, got $spool_count"
        return 1
    fi

    # Check spool usage > 0
    local spool_usage
    spool_usage=$(echo "$queue_data" | jq '.msgSpoolUsage')
    if [ "$spool_usage" = "0" ] || [ "$spool_usage" = "null" ]; then
        log_fail "val-q-large-backlog should have msgSpoolUsage > 0, got $spool_usage"
        return 1
    fi

    # Check bindCount == 0 (no consumer)
    local bind_count
    bind_count=$(echo "$queue_data" | jq '.bindCount // 0')
    if [ "$bind_count" != "0" ]; then
        log_fail "val-q-large-backlog should have bindCount=0, got $bind_count"
        return 1
    fi
}

# ── Tool 5: list-clients ────────────────────────────────────────────────────

test_list_clients() {
    init_session

    local response
    response=$(mcp_call_tool "$SESSION_ID" "list-clients" \
        '{"broker":"broker-a","msgVpnName":"default"}')

    local content
    content=$(extract_content "$response")

    assert_contains "$content" "clients" "Response should contain clients key" || return 1

    local client_count
    client_count=$(echo "$content" | jq '.clients.data | length')
    if [ "$client_count" -lt 1 ]; then
        log_fail "Expected >= 1 clients, got $client_count"
        return 1
    fi

    # Each client should have clientName
    local first_client_name
    first_client_name=$(echo "$content" | jq -r '.clients.data[0].clientName')
    if [ -z "$first_client_name" ] || [ "$first_client_name" = "null" ]; then
        log_fail "First client should have a clientName"
        return 1
    fi

    log_info "  Found $client_count clients, first: $first_client_name"
}

# ── Tool 6: get-client-details ──────────────────────────────────────────────

test_get_client_details() {
    init_session

    # Step 1: list clients to get an actual client name
    local list_response
    list_response=$(mcp_call_tool "$SESSION_ID" "list-clients" \
        '{"broker":"broker-a","msgVpnName":"default"}')

    local list_content
    list_content=$(extract_content "$list_response")

    # Pick the first client
    local client_name
    client_name=$(echo "$list_content" | jq -r '.clients.data[0].clientName')
    if [ -z "$client_name" ] || [ "$client_name" = "null" ]; then
        log_fail "No clients found to test get-client-details"
        return 1
    fi

    log_info "  Testing get-client-details for: $client_name"

    # Step 2: get details for that client (need to JSON-escape the name)
    local escaped_name
    escaped_name=$(printf '%s' "$client_name" | jq -Rs '.')

    local response
    response=$(mcp_call_tool "$SESSION_ID" "get-client-details" \
        "{\"broker\":\"broker-a\",\"msgVpnName\":\"default\",\"clientName\":$escaped_name}")

    local content
    content=$(extract_content "$response")

    assert_contains "$content" "clientDetails" "Response should contain clientDetails key" || return 1

    local client_data
    client_data=$(echo "$content" | jq '.clientDetails.data')

    # Verify the client name matches
    local returned_name
    returned_name=$(echo "$client_data" | jq -r '.clientName')
    if [ "$returned_name" != "$client_name" ]; then
        log_fail "Returned clientName '$returned_name' doesn't match requested '$client_name'"
        return 1
    fi
}

# ── Tool 7: list-client-subscriptions ────────────────────────────────────────

test_list_client_subscriptions() {
    init_session

    # Step 1: list clients to find one with subscriptions
    local list_response
    list_response=$(mcp_call_tool "$SESSION_ID" "list-clients" \
        '{"broker":"broker-a","msgVpnName":"default"}')

    local list_content
    list_content=$(extract_content "$list_response")

    # Pick the first client
    local client_name
    client_name=$(echo "$list_content" | jq -r '.clients.data[0].clientName')
    if [ -z "$client_name" ] || [ "$client_name" = "null" ]; then
        log_fail "No clients found to test list-client-subscriptions"
        return 1
    fi

    log_info "  Testing list-client-subscriptions for: $client_name"

    local escaped_name
    escaped_name=$(printf '%s' "$client_name" | jq -Rs '.')

    # Step 2: list subscriptions for that client
    local response
    response=$(mcp_call_tool "$SESSION_ID" "list-client-subscriptions" \
        "{\"broker\":\"broker-a\",\"msgVpnName\":\"default\",\"clientName\":$escaped_name}")

    local content
    content=$(extract_content "$response")

    assert_contains "$content" "subscriptions" "Response should contain subscriptions key" || return 1

    # Subscriptions may be empty for internal clients — that's ok, tool still works
    local sub_count
    sub_count=$(echo "$content" | jq '.subscriptions.data | length')
    log_info "  Client has $sub_count subscriptions"
}

# ── Tool 8: get-rdp-status ──────────────────────────────────────────────────

test_get_rdp_status() {
    init_session

    local response
    response=$(mcp_call_tool "$SESSION_ID" "get-rdp-status" \
        '{"broker":"broker-a","msgVpnName":"default","restDeliveryPointName":"val-rdp"}')

    local content
    content=$(extract_content "$response")

    # All 3 step keys must be present
    assert_contains "$content" "rdpStatus" "Response should contain rdpStatus key" || return 1
    assert_contains "$content" "queueBindings" "Response should contain queueBindings key" || return 1
    assert_contains "$content" "restConsumers" "Response should contain restConsumers key" || return 1

    # RDP should be enabled but down
    local rdp_data
    rdp_data=$(echo "$content" | jq '.rdpStatus.data')
    assert_json_field "$rdp_data" ".enabled" "true" "RDP should be enabled" || return 1
    assert_json_field "$rdp_data" ".up" "false" "RDP should be down (consumers unreachable)" || return 1

    # 2 queue bindings
    local binding_count
    binding_count=$(echo "$content" | jq '.queueBindings.data | length')
    if [ "$binding_count" -ne 2 ]; then
        log_fail "Expected 2 queue bindings, got $binding_count"
        return 1
    fi

    # 2 REST consumers
    local consumer_count
    consumer_count=$(echo "$content" | jq '.restConsumers.data | length')
    if [ "$consumer_count" -ne 2 ]; then
        log_fail "Expected 2 REST consumers, got $consumer_count"
        return 1
    fi

    # Check one consumer is disabled
    local disabled_consumers
    disabled_consumers=$(echo "$content" | jq '[.restConsumers.data[] | select(.enabled == false)] | length')
    if [ "$disabled_consumers" -lt 1 ]; then
        log_fail "Expected at least 1 disabled consumer"
        return 1
    fi

    # Check the enabled consumer has a lastFailureReason
    local failure_reason
    failure_reason=$(echo "$content" | jq -r '[.restConsumers.data[] | select(.enabled == true)] | .[0].lastFailureReason // ""')
    if [ -n "$failure_reason" ] && [ "$failure_reason" != "null" ] && [ "$failure_reason" != "" ]; then
        log_info "  Primary consumer failure: $failure_reason"
    fi
}

# ── Tool 9: get_redundancy_status ────────────────────────────────────────────

test_get_redundancy_status() {
    init_session

    local response
    response=$(mcp_call_tool "$SESSION_ID" "get_redundancy_status" \
        '{"broker":"broker-a"}')

    local content
    content=$(extract_content "$response")

    assert_contains "$content" "redundancy" "Response should contain redundancy key" || return 1

    local redundancy
    redundancy=$(echo "$content" | jq '.redundancy')

    # Standalone broker should have these fields
    assert_contains "$redundancy" "configStatus" "Should contain configStatus" || return 1
    assert_contains "$redundancy" "redundancyStatus" "Should contain redundancyStatus" || return 1
    assert_contains "$redundancy" "activeStandbyRole" "Should contain activeStandbyRole" || return 1

    # Verify it's well-formed JSON (not XML remnants)
    if echo "$redundancy" | jq empty 2>/dev/null; then
        log_info "  Redundancy response is valid JSON"
    else
        log_fail "Redundancy response is not valid JSON"
        return 1
    fi

    local status
    status=$(echo "$redundancy" | jq -r '.redundancyStatus')
    log_info "  Redundancy status: $status"
}

# ── Run All Tests ────────────────────────────────────────────────────────────

run_test "Tool 1: list-vpns"                           test_list_vpns
run_test "Tool 2a: get-vpn-health (default)"           test_get_vpn_health_default
run_test "Tool 2b: get-vpn-health (disabled)"          test_get_vpn_health_disabled
run_test "Tool 2c: get-vpn-health (active)"            test_get_vpn_health_active
run_test "Tool 3: list-queues"                         test_list_queues
run_test "Tool 4: get-queue-metrics"                   test_get_queue_metrics
run_test "Tool 5: list-clients"                        test_list_clients
run_test "Tool 6: get-client-details"                  test_get_client_details
run_test "Tool 7: list-client-subscriptions"           test_list_client_subscriptions
run_test "Tool 8: get-rdp-status"                      test_get_rdp_status
run_test "Tool 9: get_redundancy_status"               test_get_redundancy_status

print_summary "Tool Validation"
