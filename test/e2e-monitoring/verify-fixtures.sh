#!/usr/bin/env bash
# SEMP-direct fixture-state verification for SOL-150024 acceptance criteria.
# Invoked by run-all.sh after create_fixtures; assumes the
# brokers are running and the fixtures have already been created.
# Exits non-zero on any failed assertion so the parent runner short-circuits.

set -euo pipefail
source "$(dirname "$0")/helpers.sh"

# Sleeps out the remainder of a fixture's settle window. Given the epoch the
# fixture finished starting and the total window, sleeps only the time not
# already elapsed — so a second caller after the window has passed is a no-op.
# When the epoch is unset (fixture state unknown), sleeps the full window
# defensively. Shared by the F4 and F5 settle waits.
#   $1 ready_epoch     seconds-since-epoch the fixture became ready ("" if unknown)
#   $2 settle_seconds  total settle window
#   $3 label           human description for the log line
wait_for_settle() {
    local ready_epoch="$1"
    local settle_seconds="$2"
    local label="$3"
    if [ -z "$ready_epoch" ]; then
        log_warn "$label: ready-epoch unset; assuming it just started"
        sleep "$settle_seconds"
        return
    fi
    local now elapsed remaining
    now=$(date +%s)
    elapsed=$((now - ready_epoch))
    remaining=$((settle_seconds - elapsed))
    if [ "$remaining" -gt 0 ]; then
        log_info "Waiting ${remaining}s for $label to settle ..."
        sleep "$remaining"
    fi
}

# ── AC 2 — F1 multi-VPN ─────────────────────────────────────────────────────
# `test-vpn` exists on both brokers with enabled=false, state=down.

verify_multi_vpn_state() {
    local label="$1"
    local broker_url="$2"
    local body
    body=$(semp_monitor_get "$broker_url" "msgVpns/test-vpn") || {
        log_fail "F1 [$label]: GET msgVpns/test-vpn failed"
        return 1
    }
    assert_json_field "$body" ".data.enabled" "false" \
        "F1 [$label]: test-vpn enabled must be false" || return 1
    assert_json_field "$body" ".data.state" "down" \
        "F1 [$label]: test-vpn state must be down" || return 1
}

test_ac2_multi_vpn_state_a() { verify_multi_vpn_state "broker-a" "$BROKER_A_URL"; }
test_ac2_multi_vpn_state_b() { verify_multi_vpn_state "broker-b" "$BROKER_B_URL"; }

# `test-vpn-empty` exists on both brokers with enabled=true, state=up, and no
# user clients connected. This is the fixture that lets list-vpns.zeroConnectionCount
# fire. The handler derives that count directly via a per-VPN getMsgVpnClients
# probe filtered by `clientUsername != #*`, so we only assert enabled+up here —
# no msgVpnConnections tripwire is needed.
verify_empty_enabled_vpn_state() {
    local label="$1"
    local broker_url="$2"
    local body
    body=$(semp_monitor_get "$broker_url" "msgVpns/test-vpn-empty") || {
        log_fail "empty-enabled-VPN [$label]: GET msgVpns/test-vpn-empty failed"
        return 1
    }
    assert_json_field "$body" ".data.enabled" "true" \
        "empty-enabled-VPN [$label]: test-vpn-empty enabled must be true" || return 1
    assert_json_field "$body" ".data.state" "up" \
        "empty-enabled-VPN [$label]: test-vpn-empty state must be up" || return 1
}

test_empty_enabled_vpn_state_a() { verify_empty_enabled_vpn_state "broker-a" "$BROKER_A_URL"; }
test_empty_enabled_vpn_state_b() { verify_empty_enabled_vpn_state "broker-b" "$BROKER_B_URL"; }

# ── AC 3 — F2 multi-queue ───────────────────────────────────────────────────
# GET .../queues on each broker lists test-queue-2 and test-queue-3 alongside
# the base test-queue. count=100 covers any system queues without paginating.

verify_multi_queue_state() {
    local label="$1"
    local broker_url="$2"
    local body
    body=$(semp_monitor_get "$broker_url" "msgVpns/$BROKER_VPN/queues?count=100") || {
        log_fail "F2 [$label]: GET msgVpns/$BROKER_VPN/queues failed"
        return 1
    }
    local q
    for q in test-queue test-queue-2 test-queue-3; do
        assert_json_field "$body" \
            "(.data | map(.queueName) | index(\"$q\")) != null" \
            "true" \
            "F2 [$label]: $q must appear in queues collection" || return 1
    done
}

test_ac3_multi_queue_state_a() { verify_multi_queue_state "broker-a" "$BROKER_A_URL"; }
test_ac3_multi_queue_state_b() { verify_multi_queue_state "broker-b" "$BROKER_B_URL"; }

# ── F8 bridges (SOL-152231) ──────────────────────────────────────────────────
# test-bridge is up in both directions once both brokers' reciprocal bridges
# exist; test-bridge-failing is down (its inboundFailureReason legitimately
# stays empty — lab-verified, see helpers.sh); test-bridge-disabled is
# admin-disabled.

verify_bridge_state() {
    local label="$1"
    local broker_url="$2"
    local body
    body=$(semp_monitor_get "$broker_url" "msgVpns/$BROKER_VPN/bridges/test-bridge,auto") || {
        log_fail "F8 [$label]: GET bridges/test-bridge,auto failed"
        return 1
    }
    assert_json_field "$body" ".data.enabled" "true" \
        "F8 [$label]: test-bridge enabled must be true" || return 1
    assert_json_field "$body" ".data.inboundState" "ready-in-sync" \
        "F8 [$label]: test-bridge inboundState must be ready-in-sync" || return 1
    assert_json_field "$body" ".data.outboundState" "ready" \
        "F8 [$label]: test-bridge outboundState must be ready" || return 1

    body=$(semp_monitor_get "$broker_url" "msgVpns/$BROKER_VPN/bridges/test-bridge-failing,auto") || {
        log_fail "F8 [$label]: GET bridges/test-bridge-failing,auto failed"
        return 1
    }
    assert_json_field "$body" \
        '.data.inboundState != "ready-in-sync" and .data.inboundState != "ready-subscribing" and .data.inboundState != "not-applicable"' "true" \
        "F8 [$label]: test-bridge-failing inboundState must not be healthy" || return 1

    body=$(semp_monitor_get "$broker_url" "msgVpns/$BROKER_VPN/bridges/test-bridge-disabled,auto") || {
        log_fail "F8 [$label]: GET bridges/test-bridge-disabled,auto failed"
        return 1
    }
    assert_json_field "$body" ".data.enabled" "false" \
        "F8 [$label]: test-bridge-disabled enabled must be false" || return 1
}

test_f8_bridge_state_a() { verify_bridge_state "broker-a" "$BROKER_A_URL"; }
test_f8_bridge_state_b() { verify_bridge_state "broker-b" "$BROKER_B_URL"; }

# ── AC 4 — F3 connected client ──────────────────────────────────────────────
# GET .../clients/<clientName> resolves on each broker, and the client's
# subscriptions list contains every topic configured by the fixture.

verify_connected_client_state() {
    local label="$1"
    local broker_url="$2"
    local client_name="$3"
    local expected_subs="$4"      # comma-separated, matches F3_SUBSCRIPTIONS

    local body
    body=$(semp_monitor_get "$broker_url" "msgVpns/$BROKER_VPN/clients/$client_name") || {
        log_fail "F3 [$label]: GET clients/$client_name failed"
        return 1
    }
    assert_json_field "$body" ".data.clientName" "$client_name" \
        "F3 [$label]: clientName must equal $client_name" || return 1

    local subs_body
    subs_body=$(semp_monitor_get "$broker_url" \
        "msgVpns/$BROKER_VPN/clients/$client_name/subscriptions?count=100") || {
        log_fail "F3 [$label]: GET clients/$client_name/subscriptions failed"
        return 1
    }
    local t
    while IFS= read -r t; do
        [ -n "$t" ] || continue
        assert_json_field "$subs_body" \
            "(.data | map(.subscriptionTopic) | index(\"$t\")) != null" \
            "true" \
            "F3 [$label]: $t must appear in client subscriptions" || return 1
    done < <(echo "$expected_subs" | tr ',' '\n')
}

test_ac4_connected_client_state_a() {
    verify_connected_client_state "broker-a" "$BROKER_A_URL" "$F3_CLIENT_NAME_A" "$F3_SUBSCRIPTIONS"
}
test_ac4_connected_client_state_b() {
    verify_connected_client_state "broker-b" "$BROKER_B_URL" "$F3_CLIENT_NAME_B" "$F3_SUBSCRIPTIONS"
}

# ── AC 5 — F4 sustained traffic ─────────────────────────────────────────────
# After ≥ 25 s of F4 runtime, msgVpns/default reports rxMsgRate (publish-side
# aggregate) well above 80 and a lower, noisier txMsgRate (delivery to the F3
# receiver). The settle wait runs once across both brokers — they start within
# ~1 s of each other, so a single window covers both samples. txMsgRate is
# polled (5 samples / ~5 s) and asserted against a lower floor because a
# single SEMP read straddles the threshold under CI load (observed 57–88).

F4_SETTLE_SECONDS=25
F4_RX_THRESHOLD=80
F4_TX_THRESHOLD=50
F4_SAMPLE_COUNT=5
F4_SAMPLE_INTERVAL=1

verify_sustained_traffic_state() {
    local label="$1"
    local broker_url="$2"
    local body rx tx peak_rx=0 peak_tx=0 i
    local samples=()
    for ((i = 1; i <= F4_SAMPLE_COUNT; i++)); do
        body=$(semp_monitor_get "$broker_url" "msgVpns/$BROKER_VPN") || {
            log_fail "F4 [$label]: GET msgVpns/$BROKER_VPN failed"
            return 1
        }
        rx=$(echo "$body" | jq -r '.data.rxMsgRate')
        tx=$(echo "$body" | jq -r '.data.txMsgRate')
        samples+=("rx=$rx,tx=$tx")
        peak_rx=$(jq -n --argjson a "$peak_rx" --argjson b "$rx" '[$a,$b]|max')
        peak_tx=$(jq -n --argjson a "$peak_tx" --argjson b "$tx" '[$a,$b]|max')
        ((i < F4_SAMPLE_COUNT)) && sleep "$F4_SAMPLE_INTERVAL"
    done
    log_info "F4 [$label]: samples=[${samples[*]}] peakRx=$peak_rx peakTx=$peak_tx (rx≥$F4_RX_THRESHOLD, tx≥$F4_TX_THRESHOLD)"
    local peaks
    peaks=$(jq -nc --argjson rx "$peak_rx" --argjson tx "$peak_tx" '{peakRx:$rx,peakTx:$tx}')
    assert_json_field "$peaks" \
        ".peakRx >= $F4_RX_THRESHOLD" "true" \
        "F4 [$label]: peak rxMsgRate must be ≥ $F4_RX_THRESHOLD (got $peak_rx)" || return 1
    assert_json_field "$peaks" \
        ".peakTx >= $F4_TX_THRESHOLD" "true" \
        "F4 [$label]: peak txMsgRate must be ≥ $F4_TX_THRESHOLD (got $peak_tx)" || return 1
}

f4_settle() {
    wait_for_settle "${F4_READY_EPOCH:-}" "$F4_SETTLE_SECONDS" \
        "F4 rate (target rx ≥ ${F4_RX_THRESHOLD} / tx ≥ ${F4_TX_THRESHOLD} msg/s)"
}
test_ac5_sustained_traffic_state_a() {
    f4_settle
    verify_sustained_traffic_state "broker-a" "$BROKER_A_URL"
}
test_ac5_sustained_traffic_state_b() {
    # f4_settle is a no-op the second time through (window already elapsed).
    f4_settle
    verify_sustained_traffic_state "broker-b" "$BROKER_B_URL"
}

# ── F5 — slow guaranteed-message consumer ───────────────────────────────────
# Detection via queue-level signals (SOL-150344), NOT the per-client
# slowSubscriber flag (which never flips for a slow-ACK consumer — SOL-150328).
# On the F5 queue, within the settle window: a consumer is bound, the per-flow
# unacked window is pinned near its ceiling, ingress outpaces egress, and the
# spool keeps growing.

F5_SETTLE_SECONDS=30

verify_slow_consumer_state() {
    local label="$1"
    local broker_url="$2"
    local body
    body=$(semp_monitor_get "$broker_url" "msgVpns/$BROKER_VPN/queues/$F5_QUEUE") || {
        log_fail "F5 [$label]: GET queues/$F5_QUEUE failed"
        return 1
    }
    local bind unacked rx tx spooled1
    bind=$(echo "$body" | jq -r '.data.bindCount')
    unacked=$(echo "$body" | jq -r '.data.txUnackedMsgCount')
    rx=$(echo "$body" | jq -r '.data.rxMsgRate')
    tx=$(echo "$body" | jq -r '.data.txMsgRate')
    spooled1=$(echo "$body" | jq -r '.data.spooledMsgCount')
    log_info "F5 [$label]: bindCount=$bind txUnackedMsgCount=$unacked (ceiling $F5_MAX_UNACKED) rxMsgRate=$rx txMsgRate=$tx spooledMsgCount=$spooled1"

    # A consumer is bound.
    assert_json_field "$body" ".data.bindCount > 0" "true" \
        "F5 [$label]: bindCount must be > 0 (got $bind)" || return 1
    # Unacked messages pin NEAR the per-flow ceiling — the slow-ACK signature.
    # "Near", not "==": a slow-but-nonzero ACK rate makes the count oscillate by
    # one (it dips just after each ACK, before the broker redelivers), so require
    # ≥ F5_NEAR_UNACKED (80% of the ceiling) rather than the exact value.
    assert_json_field "$body" ".data.txUnackedMsgCount >= $F5_NEAR_UNACKED" "true" \
        "F5 [$label]: txUnackedMsgCount must be near the $F5_MAX_UNACKED ceiling (≥ $F5_NEAR_UNACKED, got $unacked)" || return 1
    # Ingress outpaces egress: publisher feeds faster than the throttled consumer drains.
    assert_json_field "$body" ".data.rxMsgRate > .data.txMsgRate" "true" \
        "F5 [$label]: rxMsgRate must exceed txMsgRate (got rx=$rx tx=$tx)" || return 1

    # Spool is growing: re-sample after a short interval and require an increase.
    sleep 3
    local body2 spooled2
    body2=$(semp_monitor_get "$broker_url" "msgVpns/$BROKER_VPN/queues/$F5_QUEUE") || {
        log_fail "F5 [$label]: re-GET queues/$F5_QUEUE failed"
        return 1
    }
    spooled2=$(echo "$body2" | jq -r '.data.spooledMsgCount')
    log_info "F5 [$label]: spooledMsgCount $spooled1 -> $spooled2 (must be growing)"
    assert_json_field "$body2" ".data.spooledMsgCount > $spooled1" "true" \
        "F5 [$label]: spooledMsgCount must be growing (was $spooled1, now $spooled2)" || return 1
}

f5_settle() {
    wait_for_settle "${F5_READY_EPOCH:-}" "$F5_SETTLE_SECONDS" \
        "F5 slow-consumer signals"
}
test_f5_slow_consumer_a() {
    f5_settle
    verify_slow_consumer_state "broker-a" "$BROKER_A_URL"
}
test_f5_slow_consumer_b() {
    # f5_settle is a no-op the second time through (window already elapsed).
    f5_settle
    verify_slow_consumer_state "broker-b" "$BROKER_B_URL"
}

# ── F6 — slow DIRECT subscriber (per-client slowSubscriber flag) ────────────
# The counterpart to F5: a SIGSTOP'd direct subscriber under a large-payload
# flood whose TCP egress window stays shut, so the broker flags it
# slowSubscriber=true. This is the per-client signal list-slow-subscribers
# filters on — the queue-level F5 case never flips it (SOL-150328). create_*
# already polled the flag true; re-poll here so a clean diagnostic fires if the
# stall ever stops holding (the flag is over a rolling ~1 min window).

verify_slow_subscriber_state() {
    local label="$1"
    local broker_url="$2"
    local client_name="$3"
    local broker_letter="$4"
    wait_for_slow_subscriber "$broker_url" "$label" "$client_name" "$broker_letter" || return 1
}

test_f6_slow_subscriber_a() { verify_slow_subscriber_state "broker-a" "$BROKER_A_URL" "$F6_SUB_CLIENT_NAME_A" "a"; }
test_f6_slow_subscriber_b() { verify_slow_subscriber_state "broker-b" "$BROKER_B_URL" "$F6_SUB_CLIENT_NAME_B" "b"; }

# ── AC 8 — F7-spool discards ─────────────────────────────────────────────────

verify_discard_spool_state() {
    local label="$1"
    local broker_url="$2"
    local body
    body=$(semp_monitor_get "$broker_url" "msgVpns/$BROKER_VPN/queues/$F7_SPOOL_QUEUE") || {
        log_fail "F7-spool [$label]: GET queues/$F7_SPOOL_QUEUE failed"
        return 1
    }
    local count
    count=$(echo "$body" | jq -r '.data.maxMsgSpoolUsageExceededDiscardedMsgCount')
    assert_json_field "$body" \
        ".data.maxMsgSpoolUsageExceededDiscardedMsgCount > 0" "true" \
        "F7-spool [$label]: maxMsgSpoolUsageExceededDiscardedMsgCount must be > 0 (got $count)" || return 1
}

test_ac8_discard_spool_a() { verify_discard_spool_state "broker-a" "$BROKER_A_URL"; }
test_ac8_discard_spool_b() { verify_discard_spool_state "broker-b" "$BROKER_B_URL"; }

# ── AC 9 — F7-ttl discards ───────────────────────────────────────────────────

verify_discard_ttl_state() {
    local label="$1"
    local broker_url="$2"
    local body
    body=$(semp_monitor_get "$broker_url" "msgVpns/$BROKER_VPN/queues/$F7_TTL_QUEUE") || {
        log_fail "F7-ttl [$label]: GET queues/$F7_TTL_QUEUE failed"
        return 1
    }
    # Sum all three TTL-expired counter paths the broker may use: pure
    # Discarded, ToDmq, or ToDmqFailed (DMQ resolution failed). We only care
    # that expiry happened, not which path the broker took.
    local discarded to_dmq to_dmq_failed
    discarded=$(echo "$body" | jq -r '.data.maxTtlExpiredDiscardedMsgCount')
    to_dmq=$(echo "$body" | jq -r '.data.maxTtlExpiredToDmqMsgCount')
    to_dmq_failed=$(echo "$body" | jq -r '.data.maxTtlExpiredToDmqFailedMsgCount')
    assert_json_field "$body" \
        "(.data.maxTtlExpiredDiscardedMsgCount + .data.maxTtlExpiredToDmqMsgCount + .data.maxTtlExpiredToDmqFailedMsgCount) > 0" "true" \
        "F7-ttl [$label]: total TTL-expired count must be > 0 (discarded=$discarded toDmq=$to_dmq toDmqFailed=$to_dmq_failed)" || return 1
}

test_ac9_discard_ttl_a() { verify_discard_ttl_state "broker-a" "$BROKER_A_URL"; }
test_ac9_discard_ttl_b() { verify_discard_ttl_state "broker-b" "$BROKER_B_URL"; }

# ── Run ──────────────────────────────────────────────────────────────────────

run_test "AC 2 — F1 multi-VPN state (broker-a)" test_ac2_multi_vpn_state_a
run_test "AC 2 — F1 multi-VPN state (broker-b)" test_ac2_multi_vpn_state_b
run_test "empty-enabled-VPN state (broker-a)"  test_empty_enabled_vpn_state_a
run_test "empty-enabled-VPN state (broker-b)"  test_empty_enabled_vpn_state_b
run_test "AC 3 — F2 multi-queue state (broker-a)" test_ac3_multi_queue_state_a
run_test "AC 3 — F2 multi-queue state (broker-b)" test_ac3_multi_queue_state_b
run_test "F8 — bridge state (broker-a)" test_f8_bridge_state_a
run_test "F8 — bridge state (broker-b)" test_f8_bridge_state_b
run_test "AC 4 — F3 connected client (broker-a)" test_ac4_connected_client_state_a
run_test "AC 4 — F3 connected client (broker-b)" test_ac4_connected_client_state_b
run_test "AC 5 — F4 sustained traffic (broker-a)" test_ac5_sustained_traffic_state_a
run_test "AC 5 — F4 sustained traffic (broker-b)" test_ac5_sustained_traffic_state_b
run_test "F5 — slow consumer queue signals (broker-a)" test_f5_slow_consumer_a
run_test "F5 — slow consumer queue signals (broker-b)" test_f5_slow_consumer_b
run_test "F6 — slow subscriber flag (broker-a)" test_f6_slow_subscriber_a
run_test "F6 — slow subscriber flag (broker-b)" test_f6_slow_subscriber_b
run_test "AC 8 — F7-spool discard count (broker-a)" test_ac8_discard_spool_a
run_test "AC 8 — F7-spool discard count (broker-b)" test_ac8_discard_spool_b
run_test "AC 9 — F7-ttl discard count (broker-a)" test_ac9_discard_ttl_a
run_test "AC 9 — F7-ttl discard count (broker-b)" test_ac9_discard_ttl_b

print_summary "Fixture verification"
