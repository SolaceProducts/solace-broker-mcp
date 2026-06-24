#!/usr/bin/env bash
# MCP tool-level functional tests for SOL-150025 (tools 1–12: Block A 1–9,
# Block B 10–12 — list-slow-subscribers, list-queue-discards, get-discard-stats).
# Invoked by test-monitoring-tools.sh after verify-fixtures.sh; assumes the MCP
# server is running and the F1–F7 fixtures have been created.
#
# Where verify-fixtures.sh asserts broker *state* via SEMP-direct calls, this
# file exercises each MVP monitoring *tool* through the MCP server (JSON-RPC
# over HTTP) using mcp_call_tool, then unwraps the tool payload with
# extract_content so assertions run against the tool's real output.
# Exits non-zero on any failed assertion so the parent runner short-circuits.

set -euo pipefail
source "$(dirname "$0")/helpers.sh"

# Reap fixture broker-drivers on any exit path (Ctrl-C, SIGHUP from a closed
# terminal, set -e failure, normal completion). The F6 slow-direct-subscriber
# runs under SIGSTOP, so without this trap a direct invocation that's
# interrupted leaves an orphan that can't receive SIGTERM. Idempotent and
# harmless when invoked from test-monitoring-tools.sh — its own EXIT trap
# then sees no pidfiles and no-ops.
trap stop_broker_drivers EXIT INT TERM HUP

# ── Tool 1: list-vpns (F1 multi-VPN) ─────────────────────────────────────────
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
    # No duplicates across the full (uncapped) set — guards against page-stitching
    # bugs emitting the same VPN twice (PR goal: pagination has no gaps/duplicates).
    assert_json_field "$content" \
        '(.vpns.data | map(.msgVpnName)) as $n | ($n | length) == ($n | unique | length)' "true" \
        "list-vpns [$broker]: VPN names must be unique (no pagination duplicates)" || return 1
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

# ── Tool 2: get-vpn-health (F1 multi-VPN; VPN-scoped) ────────────────────────
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

# ── Tool 3: list-queues (F2 multi-queue; VPN-scoped) ─────────────────────────
# Primary: the default-VPN queue collection includes F2's test-queue,
# test-queue-2, test-queue-3 plus the F7 discard queues (AC 6). Pagination:
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
    for q in test-queue test-queue-2 test-queue-3 "$F7_SPOOL_QUEUE" "$F7_TTL_QUEUE"; do
        assert_json_field "$content" \
            "(.queues.data | map(.queueName) | index(\"$q\")) != null" "true" \
            "list-queues [$broker]: $q must be present in default VPN" || return 1
    done
    # No duplicates across the full (uncapped) set — page stitching must not
    # repeat a queue (PR goal: pagination has no gaps/duplicates).
    assert_json_field "$content" \
        '(.queues.data | map(.queueName)) as $n | ($n | length) == ($n | unique | length)' "true" \
        "list-queues [$broker]: queue names must be unique (no pagination duplicates)" || return 1
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

# ── Tool 4: list-clients (F3 connected client; VPN-scoped) ───────────────────
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
    # No duplicates across the full (uncapped) set — page stitching must not
    # repeat a client (PR goal: pagination has no gaps/duplicates).
    assert_json_field "$content" \
        '(.clients.data | map(.clientName)) as $n | ($n | length) == ($n | unique | length)' "true" \
        "list-clients [$broker]: client names must be unique (no pagination duplicates)" || return 1
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

# ── Tool 5: get-client-details (F3 connected client; named-object lookup) ─────
# F3 case (AC 8): a named lookup of the F3 client returns that client's details
# with slowSubscriber=false.
# Envelope: {"clientDetails":{"data":{...}}} — a single object.
#
# Note: there is deliberately no slowSubscriber=true case here. A slow
# guaranteed-message consumer (slow to ACK) does NOT flip the per-client
# slowSubscriber flag — that flag tracks TCP-egress stalls, mainly for direct
# subscribers (SOL-150328/SOL-150344). The F5 slow-consumer signal is surfaced
# by get-queue-metrics instead and is asserted in the Tool 9 section below.

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

# ── Tool 6: list-client-subscriptions (F3 connected client) ──────────────────
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
    # No duplicates across the full (uncapped) set — page stitching must not
    # repeat a subscription (PR goal: pagination has no gaps/duplicates).
    assert_json_field "$content" \
        '(.subscriptions.data | map(.subscriptionTopic)) as $n | ($n | length) == ($n | unique | length)' "true" \
        "list-client-subscriptions [$broker]: subscription topics must be unique (no pagination duplicates)" || return 1
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

# ── Tool 7: get-message-rates (F4 sustained traffic; VPN-level) ──────────────
# Value check (AC 10): under F4's sustained ~100 msg/s load the default VPN's
# rxMsgRate (publish-side aggregate, ~1100+) sits well above 80 and is read
# directly. txMsgRate (delivery to the F3 receiver) is inherently lower and
# noisier than the publish rate — observed 57–88 across CI runs — so we sample
# 5 times over ~5 s and assert the peak ≥ a lower, empirically-grounded floor.
# F4 is settled by the orchestrator's verify-fixtures step (which waits ≥ 25 s
# on F4_READY_EPOCH) before this file runs, and the F4 driver keeps publishing
# throughout — so the rates are live when read here. No pagination or
# VPN-scoping variants (VPN-level tool).
# Envelope: {"rates":{"data":{...}}} — a single object.

F4_RX_THRESHOLD=80
F4_TX_THRESHOLD=50
F4_SAMPLE_COUNT=5
F4_SAMPLE_INTERVAL=1

test_get_message_rates() {
    local broker="$1"
    local response content rx tx peak_rx=0 peak_tx=0 i
    local samples=()
    for ((i = 1; i <= F4_SAMPLE_COUNT; i++)); do
        response=$(mcp_call_tool "get-message-rates" \
            "$(jq -nc --arg b "$broker" '{broker:$b,msgVpnName:"default"}')") || return 1
        content=$(extract_content "$response")
        rx=$(echo "$content" | jq -r '.rates.data.rxMsgRate')
        tx=$(echo "$content" | jq -r '.rates.data.txMsgRate')
        samples+=("rx=$rx,tx=$tx")
        peak_rx=$(jq -n --argjson a "$peak_rx" --argjson b "$rx" '[$a,$b]|max')
        peak_tx=$(jq -n --argjson a "$peak_tx" --argjson b "$tx" '[$a,$b]|max')
        ((i < F4_SAMPLE_COUNT)) && sleep "$F4_SAMPLE_INTERVAL"
    done
    log_info "get-message-rates [$broker]: samples=[${samples[*]}] peakRx=$peak_rx peakTx=$peak_tx (rx≥$F4_RX_THRESHOLD, tx≥$F4_TX_THRESHOLD)"
    local peaks
    peaks=$(jq -nc --argjson rx "$peak_rx" --argjson tx "$peak_tx" '{peakRx:$rx,peakTx:$tx}')
    assert_json_field "$peaks" ".peakRx >= $F4_RX_THRESHOLD" "true" \
        "get-message-rates [$broker]: peak rxMsgRate must be ≥ $F4_RX_THRESHOLD (got $peak_rx)" || return 1
    assert_json_field "$peaks" ".peakTx >= $F4_TX_THRESHOLD" "true" \
        "get-message-rates [$broker]: peak txMsgRate must be ≥ $F4_TX_THRESHOLD (got $peak_tx)" || return 1
}

test_get_message_rates_a() { test_get_message_rates "broker-a"; }
test_get_message_rates_b() { test_get_message_rates "broker-b"; }

# ── Tool 8: list-rdps (base fixture) ─────────────────────────────────────────
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
    # No duplicates across the full (uncapped) set — page stitching must not
    # repeat an RDP (PR goal: pagination has no gaps/duplicates).
    assert_json_field "$content" \
        '(.rdps.data | map(.restDeliveryPointName)) as $n | ($n | length) == ($n | unique | length)' "true" \
        "list-rdps [$broker]: RDP names must be unique (no pagination duplicates)" || return 1
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

# ── Tool 9: get-queue-metrics (F5 slow guaranteed-message consumer) ──────────
# Value check: get-queue-metrics surfaces the slow-consumer diagnostic that the
# per-client slowSubscriber flag cannot (SOL-150328/SOL-150344). On the F5 queue
# ($F5_QUEUE, maxDeliveredUnackedMsgsPerFlow=$F5_MAX_UNACKED) a bound consumer
# ACKs slowly while a publisher floods the topic, so: a consumer is bound,
# unacked messages pin near the per-flow ceiling, ingress outruns egress, and
# the spool backs up over time. This mirrors verify-fixtures.sh's SEMP-direct
# verify_slow_consumer_state, but exercises the same signal through the MCP tool
# envelope. The orchestrator's verify-fixtures step waits out the F5 settle
# window before this file runs, and the F5 driver keeps publishing throughout,
# so the signals are live when read here. The basic-mcp suite already smoke-tests
# get-queue-metrics' plumbing on an idle queue; this is the diagnostic half it
# structurally cannot cover (no traffic fixture there).
# Envelope: {"queueMetrics":{"data":{...}}} — a single object.

test_get_queue_metrics_slow_consumer() {
    local broker="$1"
    local response content bind unacked rx tx spooled1 spooled2

    response=$(mcp_call_tool "get-queue-metrics" \
        "$(jq -nc --arg b "$broker" --arg q "$F5_QUEUE" \
            '{broker:$b,msgVpnName:"default",queueName:$q}')") || return 1
    content=$(extract_content "$response")

    bind=$(echo "$content" | jq -r '.queueMetrics.data.bindCount')
    unacked=$(echo "$content" | jq -r '.queueMetrics.data.txUnackedMsgCount')
    rx=$(echo "$content" | jq -r '.queueMetrics.data.rxMsgRate')
    tx=$(echo "$content" | jq -r '.queueMetrics.data.txMsgRate')
    spooled1=$(echo "$content" | jq -r '.queueMetrics.data.spooledMsgCount')
    log_info "get-queue-metrics [$broker]: bindCount=$bind txUnackedMsgCount=$unacked (ceiling $F5_MAX_UNACKED) rxMsgRate=$rx txMsgRate=$tx spooledMsgCount=$spooled1"

    # A consumer is bound to the slow-consumer queue.
    assert_json_field "$content" '.queueMetrics.data.bindCount > 0' "true" \
        "get-queue-metrics [$broker]: bindCount must be > 0 (got $bind)" || return 1
    # Unacked messages pin NEAR the per-flow ceiling — the slow-ACK signature.
    # "Near", not "==": a slow-but-nonzero ACK rate makes the count oscillate by
    # one, so assert ≥ F5_NEAR_UNACKED (80% of the ceiling) rather than exact equality.
    assert_json_field "$content" ".queueMetrics.data.txUnackedMsgCount >= $F5_NEAR_UNACKED" "true" \
        "get-queue-metrics [$broker]: txUnackedMsgCount must be near the $F5_MAX_UNACKED ceiling (≥ $F5_NEAR_UNACKED, got $unacked)" || return 1
    # Ingress outruns egress while the consumer lags.
    assert_json_field "$content" '.queueMetrics.data.rxMsgRate > .queueMetrics.data.txMsgRate' "true" \
        "get-queue-metrics [$broker]: rxMsgRate must exceed txMsgRate (got rx=$rx tx=$tx)" || return 1

    # Spool backs up: a second sample taken a moment later is strictly larger.
    sleep 2
    response=$(mcp_call_tool "get-queue-metrics" \
        "$(jq -nc --arg b "$broker" --arg q "$F5_QUEUE" \
            '{broker:$b,msgVpnName:"default",queueName:$q}')") || return 1
    content=$(extract_content "$response")
    spooled2=$(echo "$content" | jq -r '.queueMetrics.data.spooledMsgCount')
    log_info "get-queue-metrics [$broker]: spooledMsgCount $spooled1 -> $spooled2 (must be growing)"
    assert_json_field "$content" ".queueMetrics.data.spooledMsgCount > $spooled1" "true" \
        "get-queue-metrics [$broker]: spooledMsgCount must be growing (was $spooled1, now $spooled2)" || return 1
}

test_get_queue_metrics_slow_consumer_a() { test_get_queue_metrics_slow_consumer "broker-a"; }
test_get_queue_metrics_slow_consumer_b() { test_get_queue_metrics_slow_consumer "broker-b"; }

# ── Tool 10: list-slow-subscribers (F6 slow direct subscriber) ──────────────
# Value check (AC 12): the F6 fixture SIGSTOPs a direct subscriber under a
# large-payload flood so its TCP egress window stalls and the broker flags it
# slowSubscriber=true. The tool filters server-side on where=slowSubscriber==true,
# so its data must include this broker's F6 client. This is the per-client flag
# the slow-ACK F5 consumer can never trip (SOL-150328) — F6 exists to exercise it.
# Cross-broker isolation (FR-8): the other broker's F6 client must be absent.
# Filter validation: every returned client actually carries slowSubscriber=true.
# Pagination: with a single qualifying client, maxResults=1 returns exactly one
# entry — it confirms the cap is honored but, like list-rdps, cannot demonstrate
# multi-page truncation (no second slow subscriber exists to drop).
# Envelope: {"slowSubscribers":{"data":[...],"truncated":bool}}.
#
# Precondition re-arm: slowSubscriber is computed over a rolling ~1 min window
# (see wait_for_slow_subscriber) and can drop back to false between the
# verify-fixtures F6 check and this test. Each test re-polls the flag via the
# broker before asserting on the tool; the poll returns immediately when the
# flag is already set, and respawns the subscriber if the broker has reaped
# it (HTTP 400) — see respawn_slow_subscriber_on in helpers.sh.

test_list_slow_subscribers() {
    local broker="$1" broker_url="$2" own="$3" other="$4" broker_letter="$5"
    local response content
    wait_for_slow_subscriber "$broker_url" "$broker" "$own" "$broker_letter" || return 1
    response=$(mcp_call_tool "list-slow-subscribers" \
        "$(jq -nc --arg b "$broker" '{broker:$b,msgVpnName:"default"}')") || return 1
    content=$(extract_content "$response")
    assert_json_field "$content" \
        "(.slowSubscribers.data | map(.clientName) | index(\"$own\")) != null" "true" \
        "list-slow-subscribers [$broker]: F6 client $own must be present" || return 1
    # Cross-broker isolation (FR-8): the other broker's F6 client must be absent.
    assert_json_field "$content" \
        "(.slowSubscribers.data | map(.clientName) | index(\"$other\")) == null" "true" \
        "list-slow-subscribers [$broker]: must NOT include other broker's client $other" || return 1
    # Server-side where filter: every returned client really is a slow subscriber.
    assert_json_field "$content" '.slowSubscribers.data | all(.slowSubscriber == true)' "true" \
        "list-slow-subscribers [$broker]: every returned client must have slowSubscriber=true" || return 1
    # No duplicates across the full (uncapped) set — page stitching must not
    # repeat a client (PR goal: pagination has no gaps/duplicates).
    assert_json_field "$content" \
        '(.slowSubscribers.data | map(.clientName)) as $n | ($n | length) == ($n | unique | length)' "true" \
        "list-slow-subscribers [$broker]: client names must be unique (no pagination duplicates)" || return 1
}

test_list_slow_subscribers_pagination() {
    local broker="$1" broker_url="$2" client_name="$3" broker_letter="$4"
    local response content
    # Same rolling-window hazard as the presence test above — re-arm first.
    wait_for_slow_subscriber "$broker_url" "$broker" "$client_name" "$broker_letter" || return 1
    response=$(mcp_call_tool "list-slow-subscribers" \
        "$(jq -nc --arg b "$broker" '{broker:$b,msgVpnName:"default",maxResults:1}')") || return 1
    content=$(extract_content "$response")
    assert_json_field "$content" '.slowSubscribers.data | length' "1" \
        "list-slow-subscribers [$broker]: maxResults=1 must return exactly 1 slow subscriber" || return 1
}

test_list_slow_subscribers_a() { test_list_slow_subscribers "broker-a" "$BROKER_A_URL" "$F6_SUB_CLIENT_NAME_A" "$F6_SUB_CLIENT_NAME_B" "a"; }
test_list_slow_subscribers_b() { test_list_slow_subscribers "broker-b" "$BROKER_B_URL" "$F6_SUB_CLIENT_NAME_B" "$F6_SUB_CLIENT_NAME_A" "b"; }
test_list_slow_subscribers_pagination_a() { test_list_slow_subscribers_pagination "broker-a" "$BROKER_A_URL" "$F6_SUB_CLIENT_NAME_A" "a"; }
test_list_slow_subscribers_pagination_b() { test_list_slow_subscribers_pagination "broker-b" "$BROKER_B_URL" "$F6_SUB_CLIENT_NAME_B" "b"; }

# ── Tool 11: list-queue-discards (F7 spool + TTL discards; per-queue) ─────────
# Value check (AC 13): list-queue-discards returns each queue's per-category
# discard counters. F7-spool's queue overflows its 1 MB spool quota
# (maxMsgSpoolUsageExceededDiscardedMsgCount > 0) and F7-ttl's queue expires
# messages by its 1 s TTL with no DMQ route (maxTtlExpiredDiscardedMsgCount > 0).
# These are the exact per-queue fields AC 13 names; the broker-wide aggregates
# are covered by get-discard-stats (Tool 12). VPN scoping: every returned queue
# belongs to the queried VPN. Pagination: maxResults=1 caps to one entry and
# flags truncated (the default VPN holds many queues).
# Envelope: {"queueDiscards":{"data":[...],"truncated":bool}}.

test_list_queue_discards() {
    local broker="$1"
    local response content spool ttl
    response=$(mcp_call_tool "list-queue-discards" \
        "$(jq -nc --arg b "$broker" '{broker:$b,msgVpnName:"default"}')") || return 1
    content=$(extract_content "$response")
    spool=$(echo "$content" | jq -r \
        "(.queueDiscards.data[] | select(.queueName==\"$F7_SPOOL_QUEUE\") | .maxMsgSpoolUsageExceededDiscardedMsgCount) // 0")
    # Sum all three TTL-expired paths (Discarded + ToDmq + ToDmqFailed): a
    # queue that inherits a default DMQ routes expired messages via ToDmq
    # (or ToDmqFailed if the DMQ can't be resolved) instead of pure Discarded.
    ttl=$(echo "$content" | jq -r \
        "(.queueDiscards.data[] | select(.queueName==\"$F7_TTL_QUEUE\") | (.maxTtlExpiredDiscardedMsgCount + .maxTtlExpiredToDmqMsgCount + .maxTtlExpiredToDmqFailedMsgCount)) // 0")
    log_info "list-queue-discards [$broker]: $F7_SPOOL_QUEUE spool-exceeded=$spool $F7_TTL_QUEUE ttl-expired-total=$ttl"
    assert_json_field "$content" \
        "(.queueDiscards.data[] | select(.queueName==\"$F7_SPOOL_QUEUE\") | .maxMsgSpoolUsageExceededDiscardedMsgCount) > 0" "true" \
        "list-queue-discards [$broker]: $F7_SPOOL_QUEUE maxMsgSpoolUsageExceededDiscardedMsgCount must be > 0 (got $spool)" || return 1
    assert_json_field "$content" \
        "(.queueDiscards.data[] | select(.queueName==\"$F7_TTL_QUEUE\") | (.maxTtlExpiredDiscardedMsgCount + .maxTtlExpiredToDmqMsgCount + .maxTtlExpiredToDmqFailedMsgCount)) > 0" "true" \
        "list-queue-discards [$broker]: $F7_TTL_QUEUE total TTL-expired count must be > 0 (got $ttl)" || return 1
    # VPN scoping: every returned queue belongs to the default VPN.
    assert_json_field "$content" '.queueDiscards.data | all(.msgVpnName == "default")' "true" \
        "list-queue-discards [$broker]: every queue must be scoped to the default VPN" || return 1
    # No duplicates across the full (uncapped) set — page stitching must not
    # repeat a queue (PR goal: pagination has no gaps/duplicates).
    assert_json_field "$content" \
        '(.queueDiscards.data | map(.queueName)) as $n | ($n | length) == ($n | unique | length)' "true" \
        "list-queue-discards [$broker]: queue names must be unique (no pagination duplicates)" || return 1
}

test_list_queue_discards_pagination() {
    local broker="$1"
    local response content
    response=$(mcp_call_tool "list-queue-discards" \
        "$(jq -nc --arg b "$broker" '{broker:$b,msgVpnName:"default",maxResults:1}')") || return 1
    content=$(extract_content "$response")
    assert_json_field "$content" '.queueDiscards.data | length' "1" \
        "list-queue-discards [$broker]: maxResults=1 must return exactly 1 queue" || return 1
    assert_json_field "$content" '.queueDiscards.truncated' "true" \
        "list-queue-discards [$broker]: maxResults=1 must flag truncated=true" || return 1
}

test_list_queue_discards_a()            { test_list_queue_discards "broker-a"; }
test_list_queue_discards_b()            { test_list_queue_discards "broker-b"; }
test_list_queue_discards_pagination_a() { test_list_queue_discards_pagination "broker-a"; }
test_list_queue_discards_pagination_b() { test_list_queue_discards_pagination "broker-b"; }

# ── Tool 12: get-discard-stats (F7 discards; broker-wide + per-VPN aggregates) ─
# Value check (AC 13, aggregate half): get-discard-stats is a NATIVE SEMPv1 tool
# returning pre-aggregated discard totals — not the per-queue breakdown (that's
# Tool 11). Its envelope therefore has NO step-id wrapper and NO `.data`: the
# tool payload is the StructuredContent object itself.
#   • Broker-wide (omit vpnName): {clientDiscards, spoolDiscards}. The F7 fixtures
#     drive the broker-wide spool aggregate up — totalTtlExpiredDiscardMessages
#     maps directly to F7-ttl, and totalDiscardedMessages is the roll-up that any
#     spool discard (TTL or quota) feeds.
#   • Per-VPN (vpnName set): {vpnName, clientDiscards} — spoolDiscards is omitted
#     because SEMPv1 exposes no per-VPN spool breakdown.
# Note: the parameter is `vpnName`, NOT `msgVpnName`.

test_get_discard_stats_broker_wide() {
    local broker="$1"
    local response content ttl total
    response=$(mcp_call_tool "get-discard-stats" \
        "$(jq -nc --arg b "$broker" '{broker:$b}')") || return 1
    content=$(extract_content "$response")
    # Sum all three TTL-expired paths (Discarded + ToDmq + ToDmqFailures): a
    # queue that inherits a default DMQ routes expired messages via ToDmq
    # (or ToDmqFailures if the DMQ can't be resolved) instead of pure Discarded.
    ttl=$(echo "$content" | jq -r '((.spoolDiscards.totalTtlExpiredDiscardMessages // 0) + (.spoolDiscards.totalTtlExpiredToDmqMessages // 0) + (.spoolDiscards.totalTtlExpiredToDmqFailures // 0))')
    total=$(echo "$content" | jq -r '.spoolDiscards.totalDiscardedMessages // 0')
    log_info "get-discard-stats [$broker] broker-wide: spool totalTtlExpired(all-paths)=$ttl totalDiscardedMessages=$total"
    # Broker-wide envelope carries both client- and spool-level aggregates.
    assert_json_field "$content" '.clientDiscards | type' "object" \
        "get-discard-stats [$broker]: broker-wide must include clientDiscards object" || return 1
    assert_json_field "$content" '.spoolDiscards | type' "object" \
        "get-discard-stats [$broker]: broker-wide must include spoolDiscards object" || return 1
    # F7 discards surface in the spool aggregate: TTL-expired (F7-ttl, directly
    # mapped) and the total roll-up (fed by both F7-ttl and F7-spool).
    assert_json_field "$content" '((.spoolDiscards.totalTtlExpiredDiscardMessages // 0) + (.spoolDiscards.totalTtlExpiredToDmqMessages // 0) + (.spoolDiscards.totalTtlExpiredToDmqFailures // 0)) > 0' "true" \
        "get-discard-stats [$broker]: total TTL-expired count must be > 0 (got $ttl)" || return 1
    assert_json_field "$content" '.spoolDiscards.totalDiscardedMessages > 0' "true" \
        "get-discard-stats [$broker]: spoolDiscards.totalDiscardedMessages must be > 0 (got $total)" || return 1
}

test_get_discard_stats_per_vpn() {
    local broker="$1"
    local response content
    response=$(mcp_call_tool "get-discard-stats" \
        "$(jq -nc --arg b "$broker" '{broker:$b,vpnName:"default"}')") || return 1
    content=$(extract_content "$response")
    assert_json_field "$content" '.vpnName' "default" \
        "get-discard-stats [$broker]: per-VPN call must echo vpnName=default" || return 1
    assert_json_field "$content" '.clientDiscards | type' "object" \
        "get-discard-stats [$broker]: per-VPN must include clientDiscards object" || return 1
    # SEMPv1 exposes no per-VPN spool breakdown, so spoolDiscards must be absent.
    assert_json_field "$content" 'has("spoolDiscards")' "false" \
        "get-discard-stats [$broker]: per-VPN must NOT include spoolDiscards" || return 1
}

test_get_discard_stats_broker_wide_a() { test_get_discard_stats_broker_wide "broker-a"; }
test_get_discard_stats_broker_wide_b() { test_get_discard_stats_broker_wide "broker-b"; }
test_get_discard_stats_per_vpn_a()     { test_get_discard_stats_per_vpn "broker-a"; }
test_get_discard_stats_per_vpn_b()     { test_get_discard_stats_per_vpn "broker-b"; }

# ── Tool 13: get-broker-status (broker-wide; SOL-150724) ─────────────────────
# Shape + value check on the curated point-in-time status snapshot. The tool
# is broker-wide (no VPN/queue scope) and runs against the base Dockerized
# broker — no dedicated fixture; the ambient F4 sustained traffic and the
# default spool config are what make memory/spool readings non-trivial.
# Envelope: {"version":{...},"system":{...},"memory":{...},"spool":{...}} —
# four step keys, each carrying the curated camelCase fields documented in
# docs/internal/semp/get-broker-status-curated-fields.md.

test_get_broker_status() {
    local broker="$1"
    local response content
    response=$(mcp_call_tool "get-broker-status" \
        "$(jq -nc --arg b "$broker" '{broker:$b}')") || return 1
    content=$(extract_content "$response")

    # Envelope shape: all four step keys present.
    assert_json_field "$content" '.version | type' "object" \
        "get-broker-status [$broker]: .version must be an object" || return 1
    assert_json_field "$content" '.system | type' "object" \
        "get-broker-status [$broker]: .system must be an object" || return 1
    assert_json_field "$content" '.memory | type' "object" \
        "get-broker-status [$broker]: .memory must be an object" || return 1
    assert_json_field "$content" '.spool | type' "object" \
        "get-broker-status [$broker]: .spool must be an object" || return 1

    # version: description identifies a Solace broker; uptime is positive.
    assert_json_field "$content" '(.version.description | type) == "string" and (.version.description | contains("Solace"))' "true" \
        "get-broker-status [$broker]: .version.description must be a Solace string" || return 1
    assert_json_field "$content" '.version.uptime.totalSecs > 0' "true" \
        "get-broker-status [$broker]: .version.uptime.totalSecs must be > 0" || return 1

    # system: uptime + restart context, scaling-tier limits, resource pair.
    assert_json_field "$content" '.system.systemUptimeSeconds > 0' "true" \
        "get-broker-status [$broker]: .system.systemUptimeSeconds must be > 0" || return 1
    # lastRestartReason is an empty string on brokers that haven't been
    # intentionally restarted (typical for the Dockerized fixtures), so only
    # assert the shape — the field is present as a string.
    assert_json_field "$content" '(.system.lastRestartReason | type) == "string"' "true" \
        "get-broker-status [$broker]: .system.lastRestartReason must be a string" || return 1
    assert_json_field "$content" '(.system.maxConnections | type) == "number"' "true" \
        "get-broker-status [$broker]: .system.maxConnections must be numeric" || return 1
    assert_json_field "$content" '(.system.maxQueueMessages | type) == "number"' "true" \
        "get-broker-status [$broker]: .system.maxQueueMessages must be numeric" || return 1
    assert_json_field "$content" '(.system.maxSubscriptions | type) == "number"' "true" \
        "get-broker-status [$broker]: .system.maxSubscriptions must be numeric" || return 1
    # cpuCores is the "available" half of the under-scaling pair. The
    # "required" counterpart (cpuCoresRequired) is only emitted by appliance /
    # cloud brokers — Dockerized PubSub+ Standard omits it — so we don't assert
    # it here.
    assert_json_field "$content" '(.system.cpuCores | type) == "number"' "true" \
        "get-broker-status [$broker]: .system.cpuCores must be numeric" || return 1

    # memory: utilization percentages bounded to [0, 100].
    assert_json_field "$content" '.memory.physicalMemoryUsagePercent >= 0 and .memory.physicalMemoryUsagePercent <= 100' "true" \
        "get-broker-status [$broker]: .memory.physicalMemoryUsagePercent must be in [0,100]" || return 1
    assert_json_field "$content" '.memory.subscriptionMemoryUsagePercent >= 0 and .memory.subscriptionMemoryUsagePercent <= 100' "true" \
        "get-broker-status [$broker]: .memory.subscriptionMemoryUsagePercent must be in [0,100]" || return 1

    # spool: HA / datapath state plus disk-usage indicator. The envelope nests
    # the curated fields under .spool.messageSpoolInfo (see spool_response.go).
    # Dockerized brokers in this suite run with the spool enabled (see F5/F7
    # fixtures), so configStatus and operationalStatus must be non-empty here.
    # datapathUp and activeDiskPartitionUsage are strings as emitted by SEMPv1.
    assert_json_field "$content" '(.spool.messageSpoolInfo.configStatus | type) == "string" and (.spool.messageSpoolInfo.configStatus | length) > 0' "true" \
        "get-broker-status [$broker]: .spool.messageSpoolInfo.configStatus must be a non-empty string" || return 1
    assert_json_field "$content" '(.spool.messageSpoolInfo.operationalStatus | type) == "string" and (.spool.messageSpoolInfo.operationalStatus | length) > 0' "true" \
        "get-broker-status [$broker]: .spool.messageSpoolInfo.operationalStatus must be a non-empty string" || return 1
    assert_json_field "$content" '(.spool.messageSpoolInfo.datapathUp | type) == "string"' "true" \
        "get-broker-status [$broker]: .spool.messageSpoolInfo.datapathUp must be a string" || return 1
    assert_json_field "$content" '(.spool.messageSpoolInfo.activeDiskPartitionUsage | type) == "string"' "true" \
        "get-broker-status [$broker]: .spool.messageSpoolInfo.activeDiskPartitionUsage must be a string" || return 1
}

test_get_broker_status_a() { test_get_broker_status "broker-a"; }
test_get_broker_status_b() { test_get_broker_status "broker-b"; }

# ── Run ──────────────────────────────────────────────────────────────────────

run_test "Tool 1 — list-vpns (broker-a)"               test_list_vpns_a
run_test "Tool 1 — list-vpns (broker-b)"               test_list_vpns_b
run_test "Tool 1 — list-vpns pagination (broker-a)"    test_list_vpns_pagination_a
run_test "Tool 1 — list-vpns pagination (broker-b)"    test_list_vpns_pagination_b

run_test "Tool 2 — get-vpn-health default (broker-a)"  test_get_vpn_health_default_a
run_test "Tool 2 — get-vpn-health default (broker-b)"  test_get_vpn_health_default_b
run_test "Tool 2 — get-vpn-health test-vpn (broker-a)" test_get_vpn_health_testvpn_a
run_test "Tool 2 — get-vpn-health test-vpn (broker-b)" test_get_vpn_health_testvpn_b

run_test "Tool 3 — list-queues (broker-a)"             test_list_queues_a
run_test "Tool 3 — list-queues (broker-b)"             test_list_queues_b
run_test "Tool 3 — list-queues pagination (broker-a)"  test_list_queues_pagination_a
run_test "Tool 3 — list-queues pagination (broker-b)"  test_list_queues_pagination_b
run_test "Tool 3 — list-queues VPN scope (broker-a)"   test_list_queues_vpn_scope_a
run_test "Tool 3 — list-queues VPN scope (broker-b)"   test_list_queues_vpn_scope_b

run_test "Tool 4 — list-clients (broker-a)"            test_list_clients_a
run_test "Tool 4 — list-clients (broker-b)"            test_list_clients_b
run_test "Tool 4 — list-clients pagination (broker-a)" test_list_clients_pagination_a
run_test "Tool 4 — list-clients pagination (broker-b)" test_list_clients_pagination_b

run_test "Tool 5 — get-client-details F3 (broker-a)"   test_get_client_details_f3_a
run_test "Tool 5 — get-client-details F3 (broker-b)"   test_get_client_details_f3_b

run_test "Tool 6 — list-client-subscriptions (broker-a)"            test_list_client_subscriptions_a
run_test "Tool 6 — list-client-subscriptions (broker-b)"            test_list_client_subscriptions_b
run_test "Tool 6 — list-client-subscriptions pagination (broker-a)" test_list_client_subscriptions_pagination_a
run_test "Tool 6 — list-client-subscriptions pagination (broker-b)" test_list_client_subscriptions_pagination_b

run_test "Tool 7 — get-message-rates (broker-a)"       test_get_message_rates_a
run_test "Tool 7 — get-message-rates (broker-b)"       test_get_message_rates_b

run_test "Tool 8 — list-rdps (broker-a)"               test_list_rdps_a
run_test "Tool 8 — list-rdps (broker-b)"               test_list_rdps_b
run_test "Tool 8 — list-rdps pagination (broker-a)"    test_list_rdps_pagination_a
run_test "Tool 8 — list-rdps pagination (broker-b)"    test_list_rdps_pagination_b

run_test "Tool 9 — get-queue-metrics slow consumer (broker-a)" test_get_queue_metrics_slow_consumer_a
run_test "Tool 9 — get-queue-metrics slow consumer (broker-b)" test_get_queue_metrics_slow_consumer_b

run_test "Tool 10 — list-slow-subscribers (broker-a)"            test_list_slow_subscribers_a
run_test "Tool 10 — list-slow-subscribers (broker-b)"            test_list_slow_subscribers_b
run_test "Tool 10 — list-slow-subscribers pagination (broker-a)" test_list_slow_subscribers_pagination_a
run_test "Tool 10 — list-slow-subscribers pagination (broker-b)" test_list_slow_subscribers_pagination_b

run_test "Tool 11 — list-queue-discards (broker-a)"             test_list_queue_discards_a
run_test "Tool 11 — list-queue-discards (broker-b)"             test_list_queue_discards_b
run_test "Tool 11 — list-queue-discards pagination (broker-a)"  test_list_queue_discards_pagination_a
run_test "Tool 11 — list-queue-discards pagination (broker-b)"  test_list_queue_discards_pagination_b

run_test "Tool 12 — get-discard-stats broker-wide (broker-a)"   test_get_discard_stats_broker_wide_a
run_test "Tool 12 — get-discard-stats broker-wide (broker-b)"   test_get_discard_stats_broker_wide_b
run_test "Tool 12 — get-discard-stats per-VPN (broker-a)"       test_get_discard_stats_per_vpn_a
run_test "Tool 12 — get-discard-stats per-VPN (broker-b)"       test_get_discard_stats_per_vpn_b

run_test "Tool 13 — get-broker-status (broker-a)"               test_get_broker_status_a
run_test "Tool 13 — get-broker-status (broker-b)"               test_get_broker_status_b

print_summary "MCP tool tests"
