#!/usr/bin/env bash
# MCP tool-level functional tests for SOL-150025 (Block A: tools 1–9).
# Invoked by test-monitoring-tools.sh after verify-fixtures.sh; assumes the MCP
# server is running and the F1–F6 fixtures have been created.
#
# Where verify-fixtures.sh asserts broker *state* via SEMP-direct calls, this
# file exercises each MVP monitoring *tool* through the MCP server (JSON-RPC
# over HTTP) using mcp_call_tool, then unwraps the tool payload with
# extract_content so assertions run against the tool's real output.
# Exits non-zero on any failed assertion so the parent runner short-circuits.

set -euo pipefail
source "$(dirname "$0")/helpers.sh"

# ── Tool 2: list-vpns (F1 multi-VPN) ─────────────────────────────────────────
# Primary: the VPN collection includes the base `default` VPN and F1's
# `test-vpn`. Pagination: maxResults=1 returns exactly one entry and flags the
# result truncated, while the uncapped default call returns the full set.
# Envelope: list-vpns returns {"vpns":{"data":[...],"truncated":bool}} — the
# step id `vpns` keys the payload.

test_list_vpns() {
    local broker="$1"
    local response content
    response=$(mcp_call_tool "list-vpns" "$(jq -nc --arg b "$broker" '{broker:$b}')") || return 1
    content=$(extract_content "$response")
    assert_json_field "$content" \
        '(.vpns.data | map(.msgVpnName) | index("default")) != null' "true" \
        "list-vpns [$broker]: default VPN must be present" || return 1
    assert_json_field "$content" \
        '(.vpns.data | map(.msgVpnName) | index("test-vpn")) != null' "true" \
        "list-vpns [$broker]: F1 test-vpn must be present" || return 1
}

test_list_vpns_pagination() {
    local broker="$1"
    local response content
    # maxResults=1 caps the result to one entry and marks it truncated.
    response=$(mcp_call_tool "list-vpns" \
        "$(jq -nc --arg b "$broker" '{broker:$b,maxResults:1}')") || return 1
    content=$(extract_content "$response")
    assert_json_field "$content" '.vpns.data | length' "1" \
        "list-vpns [$broker]: maxResults=1 must return exactly 1 VPN" || return 1
    assert_json_field "$content" '.vpns.truncated' "true" \
        "list-vpns [$broker]: maxResults=1 must flag truncated=true" || return 1
    # The uncapped call returns every VPN (≥ 2: default + test-vpn), untruncated.
    response=$(mcp_call_tool "list-vpns" "$(jq -nc --arg b "$broker" '{broker:$b}')") || return 1
    content=$(extract_content "$response")
    assert_json_field "$content" '(.vpns.data | length) >= 2' "true" \
        "list-vpns [$broker]: uncapped call must return all VPNs" || return 1
    assert_json_field "$content" '.vpns.truncated' "false" \
        "list-vpns [$broker]: uncapped call must not be truncated" || return 1
}

test_list_vpns_a()            { test_list_vpns "broker-a"; }
test_list_vpns_b()            { test_list_vpns "broker-b"; }
test_list_vpns_pagination_a() { test_list_vpns_pagination "broker-a"; }
test_list_vpns_pagination_b() { test_list_vpns_pagination "broker-b"; }

# ── Tool 3: get-vpn-health (F1 multi-VPN; VPN-scoped) ────────────────────────
# Value check (AC 5): the base `default` VPN reports enabled=true with services
# up; F1's `test-vpn` reports enabled=false / state=down. This is the case
# FR-0's extract_content exists for — a substring match on the raw envelope
# cannot reliably tell "enabled":false from "enabled":true.
# Envelope: {"vpnHealth":{"data":{...}}} — a single object, not a collection.

test_get_vpn_health_default() {
    local broker="$1"
    local response content
    response=$(mcp_call_tool "get-vpn-health" \
        "$(jq -nc --arg b "$broker" '{broker:$b,msgVpnName:"default"}')") || return 1
    content=$(extract_content "$response")
    assert_json_field "$content" '.vpnHealth.data.enabled' "true" \
        "get-vpn-health [$broker]: default VPN must be enabled" || return 1
    assert_json_field "$content" '.vpnHealth.data.state' "up" \
        "get-vpn-health [$broker]: default VPN state must be up" || return 1
    assert_json_field "$content" '.vpnHealth.data.serviceSmfPlainTextUp' "true" \
        "get-vpn-health [$broker]: default VPN SMF service must be up" || return 1
}

test_get_vpn_health_testvpn() {
    local broker="$1"
    local response content
    response=$(mcp_call_tool "get-vpn-health" \
        "$(jq -nc --arg b "$broker" '{broker:$b,msgVpnName:"test-vpn"}')") || return 1
    content=$(extract_content "$response")
    assert_json_field "$content" '.vpnHealth.data.enabled' "false" \
        "get-vpn-health [$broker]: test-vpn must be disabled (enabled=false)" || return 1
    assert_json_field "$content" '.vpnHealth.data.state' "down" \
        "get-vpn-health [$broker]: test-vpn state must be down" || return 1
}

test_get_vpn_health_default_a() { test_get_vpn_health_default "broker-a"; }
test_get_vpn_health_default_b() { test_get_vpn_health_default "broker-b"; }
test_get_vpn_health_testvpn_a() { test_get_vpn_health_testvpn "broker-a"; }
test_get_vpn_health_testvpn_b() { test_get_vpn_health_testvpn "broker-b"; }

# ── Tool 4: list-queues (F2 multi-queue; VPN-scoped) ─────────────────────────
# Primary: the default-VPN queue collection includes F2's test-queue,
# test-queue-2, test-queue-3 plus the F6 discard queues (AC 6). Pagination:
# maxResults=1 caps to one entry and flags truncated. VPN scoping: every
# returned queue belongs to the queried VPN, and the F1 test-vpn (which owns no
# queues) surfaces none of the default VPN's queues.
# Envelope: {"queues":{"data":[...],"truncated":bool}}.

test_list_queues() {
    local broker="$1"
    local response content q
    response=$(mcp_call_tool "list-queues" \
        "$(jq -nc --arg b "$broker" '{broker:$b,msgVpnName:"default"}')") || return 1
    content=$(extract_content "$response")
    for q in test-queue test-queue-2 test-queue-3 "$F6_SPOOL_QUEUE" "$F6_TTL_QUEUE"; do
        assert_json_field "$content" \
            "(.queues.data | map(.queueName) | index(\"$q\")) != null" "true" \
            "list-queues [$broker]: $q must be present in default VPN" || return 1
    done
}

test_list_queues_pagination() {
    local broker="$1"
    local response content
    response=$(mcp_call_tool "list-queues" \
        "$(jq -nc --arg b "$broker" '{broker:$b,msgVpnName:"default",maxResults:1}')") || return 1
    content=$(extract_content "$response")
    assert_json_field "$content" '.queues.data | length' "1" \
        "list-queues [$broker]: maxResults=1 must return exactly 1 queue" || return 1
    assert_json_field "$content" '.queues.truncated' "true" \
        "list-queues [$broker]: maxResults=1 must flag truncated=true" || return 1
}

test_list_queues_vpn_scope() {
    local broker="$1"
    local response content
    # Default-VPN call: every returned queue belongs to the default VPN.
    response=$(mcp_call_tool "list-queues" \
        "$(jq -nc --arg b "$broker" '{broker:$b,msgVpnName:"default"}')") || return 1
    content=$(extract_content "$response")
    assert_json_field "$content" '.queues.data | all(.msgVpnName == "default")' "true" \
        "list-queues [$broker]: every queue must be scoped to the default VPN" || return 1
    # F1's test-vpn owns no queues — scoping must not leak the default VPN's.
    response=$(mcp_call_tool "list-queues" \
        "$(jq -nc --arg b "$broker" '{broker:$b,msgVpnName:"test-vpn"}')") || return 1
    content=$(extract_content "$response")
    assert_json_field "$content" \
        '(.queues.data | map(.queueName) | index("test-queue")) == null' "true" \
        "list-queues [$broker]: test-vpn scope must not include default's test-queue" || return 1
}

test_list_queues_a()            { test_list_queues "broker-a"; }
test_list_queues_b()            { test_list_queues "broker-b"; }
test_list_queues_pagination_a() { test_list_queues_pagination "broker-a"; }
test_list_queues_pagination_b() { test_list_queues_pagination "broker-b"; }
test_list_queues_vpn_scope_a()  { test_list_queues_vpn_scope "broker-a"; }
test_list_queues_vpn_scope_b()  { test_list_queues_vpn_scope "broker-b"; }

# ── Tool 5: list-clients (F3 connected client; VPN-scoped) ───────────────────
# Primary: the default-VPN client list includes this broker's deterministic F3
# client. Cross-broker isolation (FR-8): the list must NOT include the *other*
# broker's F3 client — this catches broker-routing bugs. VPN scoping: every
# returned client belongs to the default VPN. Pagination: maxResults=1 caps to
# one entry and flags truncated — the default VPN always holds ≥ 3 clients (the
# F3 client plus the internal #client and the #rdp/test-rdp consumer).
# Envelope: {"clients":{"data":[...],"truncated":bool}}.

test_list_clients() {
    local broker="$1" own="$2" other="$3"
    local response content
    response=$(mcp_call_tool "list-clients" \
        "$(jq -nc --arg b "$broker" '{broker:$b,msgVpnName:"default"}')") || return 1
    content=$(extract_content "$response")
    assert_json_field "$content" \
        "(.clients.data | map(.clientName) | index(\"$own\")) != null" "true" \
        "list-clients [$broker]: F3 client $own must be present" || return 1
    # Cross-broker isolation (FR-8): the other broker's client must be absent.
    assert_json_field "$content" \
        "(.clients.data | map(.clientName) | index(\"$other\")) == null" "true" \
        "list-clients [$broker]: must NOT include other broker's client $other" || return 1
    assert_json_field "$content" '.clients.data | all(.msgVpnName == "default")' "true" \
        "list-clients [$broker]: every client must be scoped to the default VPN" || return 1
}

test_list_clients_pagination() {
    local broker="$1"
    local response content
    response=$(mcp_call_tool "list-clients" \
        "$(jq -nc --arg b "$broker" '{broker:$b,msgVpnName:"default",maxResults:1}')") || return 1
    content=$(extract_content "$response")
    assert_json_field "$content" '.clients.data | length' "1" \
        "list-clients [$broker]: maxResults=1 must return exactly 1 client" || return 1
    assert_json_field "$content" '.clients.truncated' "true" \
        "list-clients [$broker]: maxResults=1 must flag truncated=true" || return 1
}

test_list_clients_a()            { test_list_clients "broker-a" "$F3_CLIENT_NAME_A" "$F3_CLIENT_NAME_B"; }
test_list_clients_b()            { test_list_clients "broker-b" "$F3_CLIENT_NAME_B" "$F3_CLIENT_NAME_A"; }
test_list_clients_pagination_a() { test_list_clients_pagination "broker-a"; }
test_list_clients_pagination_b() { test_list_clients_pagination "broker-b"; }

# ── Tool 6: get-client-details (F3 connected client; named-object lookup) ─────
# F3 case (AC 8): a named lookup of the F3 client returns that client's details
# with slowSubscriber=false.
# Envelope: {"clientDetails":{"data":{...}}} — a single object.
#
# TODO(SOL-150025 / F5): add the slow-subscriber half (AC 8: F5 client →
# slowSubscriber=true) once an F5 (slow subscriber) fixture exists. SOL-150024
# does not currently ship one in helpers.sh (no F5_* constants / create_slow_*
# helper), so there is no client to look up yet.

test_get_client_details_f3() {
    local broker="$1" client="$2"
    local response content
    response=$(mcp_call_tool "get-client-details" \
        "$(jq -nc --arg b "$broker" --arg c "$client" \
            '{broker:$b,msgVpnName:"default",clientName:$c}')") || return 1
    content=$(extract_content "$response")
    assert_json_field "$content" '.clientDetails.data.clientName' "$client" \
        "get-client-details [$broker]: named lookup must return $client" || return 1
    assert_json_field "$content" '.clientDetails.data.slowSubscriber' "false" \
        "get-client-details [$broker]: F3 client must report slowSubscriber=false" || return 1
}

test_get_client_details_f3_a() { test_get_client_details_f3 "broker-a" "$F3_CLIENT_NAME_A"; }
test_get_client_details_f3_b() { test_get_client_details_f3 "broker-b" "$F3_CLIENT_NAME_B"; }

# ── Tool 7: list-client-subscriptions (F3 connected client) ──────────────────
# Primary: the F3 client's subscriptions include every topic the fixture
# configured ($F3_SUBSCRIPTIONS). The client also carries an auto-generated
# #P2P/... inbox subscription, so assert the configured topics are *present*
# rather than asserting an exact count. Pagination: maxResults=1 returns exactly
# one subscription while the uncapped call returns the full set — this tool has
# no followPages, so its envelope leaves `truncated` null and the data length is
# the reliable signal that the cap was honored.
# Envelope: {"subscriptions":{"data":[...]}}.

test_list_client_subscriptions() {
    local broker="$1" client="$2"
    local response content t
    response=$(mcp_call_tool "list-client-subscriptions" \
        "$(jq -nc --arg b "$broker" --arg c "$client" \
            '{broker:$b,msgVpnName:"default",clientName:$c}')") || return 1
    content=$(extract_content "$response")
    while IFS= read -r t; do
        [ -n "$t" ] || continue
        assert_json_field "$content" \
            "(.subscriptions.data | map(.subscriptionTopic) | index(\"$t\")) != null" "true" \
            "list-client-subscriptions [$broker]: $client must subscribe to $t" || return 1
    done < <(echo "$F3_SUBSCRIPTIONS" | tr ',' '\n')
}

test_list_client_subscriptions_pagination() {
    local broker="$1" client="$2"
    local response content
    response=$(mcp_call_tool "list-client-subscriptions" \
        "$(jq -nc --arg b "$broker" --arg c "$client" \
            '{broker:$b,msgVpnName:"default",clientName:$c,maxResults:1}')") || return 1
    content=$(extract_content "$response")
    assert_json_field "$content" '.subscriptions.data | length' "1" \
        "list-client-subscriptions [$broker]: maxResults=1 must return exactly 1 subscription" || return 1
    # Uncapped call returns the full set (≥ 2) — proves the cap dropped entries.
    response=$(mcp_call_tool "list-client-subscriptions" \
        "$(jq -nc --arg b "$broker" --arg c "$client" \
            '{broker:$b,msgVpnName:"default",clientName:$c}')") || return 1
    content=$(extract_content "$response")
    assert_json_field "$content" '(.subscriptions.data | length) > 1' "true" \
        "list-client-subscriptions [$broker]: uncapped call must return more than 1" || return 1
}

test_list_client_subscriptions_a() { test_list_client_subscriptions "broker-a" "$F3_CLIENT_NAME_A"; }
test_list_client_subscriptions_b() { test_list_client_subscriptions "broker-b" "$F3_CLIENT_NAME_B"; }
test_list_client_subscriptions_pagination_a() { test_list_client_subscriptions_pagination "broker-a" "$F3_CLIENT_NAME_A"; }
test_list_client_subscriptions_pagination_b() { test_list_client_subscriptions_pagination "broker-b" "$F3_CLIENT_NAME_B"; }

# ── Tool 8: get-message-rates (F4 sustained traffic; VPN-level) ──────────────
# Value check (AC 10): under F4's sustained ~100 msg/s load the default VPN
# reports rxMsgRate ≥ 80 and txMsgRate ≥ 80. F4 is settled by the orchestrator's
# verify-fixtures step (which waits ≥ 25 s on F4_READY_EPOCH) before this file
# runs, and the F4 driver keeps publishing throughout — so the rates are live
# when read here. No pagination or VPN-scoping variants (VPN-level tool).
# Envelope: {"rates":{"data":{...}}} — a single object.

F4_RATE_THRESHOLD=80

test_get_message_rates() {
    local broker="$1"
    local response content rx tx
    response=$(mcp_call_tool "get-message-rates" \
        "$(jq -nc --arg b "$broker" '{broker:$b,msgVpnName:"default"}')") || return 1
    content=$(extract_content "$response")
    rx=$(echo "$content" | jq -r '.rates.data.rxMsgRate')
    tx=$(echo "$content" | jq -r '.rates.data.txMsgRate')
    log_info "get-message-rates [$broker]: rxMsgRate=$rx txMsgRate=$tx (threshold ≥ $F4_RATE_THRESHOLD)"
    assert_json_field "$content" ".rates.data.rxMsgRate >= $F4_RATE_THRESHOLD" "true" \
        "get-message-rates [$broker]: rxMsgRate must be ≥ $F4_RATE_THRESHOLD (got $rx)" || return 1
    assert_json_field "$content" ".rates.data.txMsgRate >= $F4_RATE_THRESHOLD" "true" \
        "get-message-rates [$broker]: txMsgRate must be ≥ $F4_RATE_THRESHOLD (got $tx)" || return 1
}

test_get_message_rates_a() { test_get_message_rates "broker-a"; }
test_get_message_rates_b() { test_get_message_rates "broker-b"; }

# ── Tool 9: list-rdps (base fixture) ─────────────────────────────────────────
# Primary: the default-VPN RDP collection includes the base test-rdp.
# Pagination: the base fixture provisions a single RDP, so maxResults=1 returns
# that one entry untruncated. This confirms the cap is accepted and the full set
# is returned, but cannot demonstrate multi-page truncation — no second RDP
# exists to drop.
# Envelope: {"rdps":{"data":[...],"truncated":bool}}.

test_list_rdps() {
    local broker="$1"
    local response content
    response=$(mcp_call_tool "list-rdps" \
        "$(jq -nc --arg b "$broker" '{broker:$b,msgVpnName:"default"}')") || return 1
    content=$(extract_content "$response")
    assert_json_field "$content" \
        '(.rdps.data | map(.restDeliveryPointName) | index("test-rdp")) != null' "true" \
        "list-rdps [$broker]: test-rdp must be present" || return 1
}

test_list_rdps_pagination() {
    local broker="$1"
    local response content
    response=$(mcp_call_tool "list-rdps" \
        "$(jq -nc --arg b "$broker" '{broker:$b,msgVpnName:"default",maxResults:1}')") || return 1
    content=$(extract_content "$response")
    assert_json_field "$content" '.rdps.data | length' "1" \
        "list-rdps [$broker]: maxResults=1 must return exactly 1 RDP" || return 1
    assert_json_field "$content" '.rdps.truncated' "false" \
        "list-rdps [$broker]: single base RDP must report truncated=false" || return 1
}

test_list_rdps_a()            { test_list_rdps "broker-a"; }
test_list_rdps_b()            { test_list_rdps "broker-b"; }
test_list_rdps_pagination_a() { test_list_rdps_pagination "broker-a"; }
test_list_rdps_pagination_b() { test_list_rdps_pagination "broker-b"; }

# ── Run ──────────────────────────────────────────────────────────────────────

run_test "Tool 2 — list-vpns (broker-a)"               test_list_vpns_a
run_test "Tool 2 — list-vpns (broker-b)"               test_list_vpns_b
run_test "Tool 2 — list-vpns pagination (broker-a)"    test_list_vpns_pagination_a
run_test "Tool 2 — list-vpns pagination (broker-b)"    test_list_vpns_pagination_b

run_test "Tool 3 — get-vpn-health default (broker-a)"  test_get_vpn_health_default_a
run_test "Tool 3 — get-vpn-health default (broker-b)"  test_get_vpn_health_default_b
run_test "Tool 3 — get-vpn-health test-vpn (broker-a)" test_get_vpn_health_testvpn_a
run_test "Tool 3 — get-vpn-health test-vpn (broker-b)" test_get_vpn_health_testvpn_b

run_test "Tool 4 — list-queues (broker-a)"             test_list_queues_a
run_test "Tool 4 — list-queues (broker-b)"             test_list_queues_b
run_test "Tool 4 — list-queues pagination (broker-a)"  test_list_queues_pagination_a
run_test "Tool 4 — list-queues pagination (broker-b)"  test_list_queues_pagination_b
run_test "Tool 4 — list-queues VPN scope (broker-a)"   test_list_queues_vpn_scope_a
run_test "Tool 4 — list-queues VPN scope (broker-b)"   test_list_queues_vpn_scope_b

run_test "Tool 5 — list-clients (broker-a)"            test_list_clients_a
run_test "Tool 5 — list-clients (broker-b)"            test_list_clients_b
run_test "Tool 5 — list-clients pagination (broker-a)" test_list_clients_pagination_a
run_test "Tool 5 — list-clients pagination (broker-b)" test_list_clients_pagination_b

run_test "Tool 6 — get-client-details F3 (broker-a)"   test_get_client_details_f3_a
run_test "Tool 6 — get-client-details F3 (broker-b)"   test_get_client_details_f3_b

run_test "Tool 7 — list-client-subscriptions (broker-a)"            test_list_client_subscriptions_a
run_test "Tool 7 — list-client-subscriptions (broker-b)"            test_list_client_subscriptions_b
run_test "Tool 7 — list-client-subscriptions pagination (broker-a)" test_list_client_subscriptions_pagination_a
run_test "Tool 7 — list-client-subscriptions pagination (broker-b)" test_list_client_subscriptions_pagination_b

run_test "Tool 8 — get-message-rates (broker-a)"       test_get_message_rates_a
run_test "Tool 8 — get-message-rates (broker-b)"       test_get_message_rates_b

run_test "Tool 9 — list-rdps (broker-a)"               test_list_rdps_a
run_test "Tool 9 — list-rdps (broker-b)"               test_list_rdps_b
run_test "Tool 9 — list-rdps pagination (broker-a)"    test_list_rdps_pagination_a
run_test "Tool 9 — list-rdps pagination (broker-b)"    test_list_rdps_pagination_b

print_summary "MCP tool tests"
