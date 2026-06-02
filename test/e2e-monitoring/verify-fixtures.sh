#!/usr/bin/env bash
# SEMP-direct fixture-state verification for SOL-150024 acceptance criteria.
# Invoked by test-monitoring-tools.sh after create_fixtures; assumes the
# brokers are running and the fixtures have already been created.
# Exits non-zero on any failed assertion so the parent runner short-circuits.

set -euo pipefail
source "$(dirname "$0")/helpers.sh"

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
# After ≥ 25 s of F4 runtime, msgVpns/default reports rxMsgRate ≥ 80 and
# txMsgRate ≥ 80 on each broker. The settle wait runs once across both
# brokers — they start within ~1 s of each other, so a single window covers
# both samples.

F4_SETTLE_SECONDS=25
F4_RATE_THRESHOLD=80

wait_for_f4_settle() {
    if [ -z "${F4_READY_EPOCH:-}" ]; then
        log_warn "F4_READY_EPOCH unset; assuming F4 just started"
        sleep "$F4_SETTLE_SECONDS"
        return
    fi
    local now elapsed remaining
    now=$(date +%s)
    elapsed=$((now - F4_READY_EPOCH))
    remaining=$((F4_SETTLE_SECONDS - elapsed))
    if [ "$remaining" -gt 0 ]; then
        log_info "Waiting ${remaining}s for F4 rate to settle (target ≥ ${F4_RATE_THRESHOLD} msg/s) ..."
        sleep "$remaining"
    fi
}

verify_sustained_traffic_state() {
    local label="$1"
    local broker_url="$2"
    local body
    body=$(semp_monitor_get "$broker_url" "msgVpns/$BROKER_VPN") || {
        log_fail "F4 [$label]: GET msgVpns/$BROKER_VPN failed"
        return 1
    }
    local rx tx
    rx=$(echo "$body" | jq -r '.data.rxMsgRate')
    tx=$(echo "$body" | jq -r '.data.txMsgRate')
    log_info "F4 [$label]: rxMsgRate=$rx txMsgRate=$tx (threshold ≥ $F4_RATE_THRESHOLD)"
    assert_json_field "$body" \
        ".data.rxMsgRate >= $F4_RATE_THRESHOLD" "true" \
        "F4 [$label]: rxMsgRate must be ≥ $F4_RATE_THRESHOLD (got $rx)" || return 1
    assert_json_field "$body" \
        ".data.txMsgRate >= $F4_RATE_THRESHOLD" "true" \
        "F4 [$label]: txMsgRate must be ≥ $F4_RATE_THRESHOLD (got $tx)" || return 1
}

test_ac5_sustained_traffic_state_a() {
    wait_for_f4_settle
    verify_sustained_traffic_state "broker-a" "$BROKER_A_URL"
}
test_ac5_sustained_traffic_state_b() {
    # wait_for_f4_settle is a no-op the second time through.
    wait_for_f4_settle
    verify_sustained_traffic_state "broker-b" "$BROKER_B_URL"
}

# ── AC 8 — F6-spool discards ─────────────────────────────────────────────────

verify_discard_spool_state() {
    local label="$1"
    local broker_url="$2"
    local body
    body=$(semp_monitor_get "$broker_url" "msgVpns/$BROKER_VPN/queues/$F6_SPOOL_QUEUE") || {
        log_fail "F6-spool [$label]: GET queues/$F6_SPOOL_QUEUE failed"
        return 1
    }
    local count
    count=$(echo "$body" | jq -r '.data.maxMsgSpoolUsageExceededDiscardedMsgCount')
    assert_json_field "$body" \
        ".data.maxMsgSpoolUsageExceededDiscardedMsgCount > 0" "true" \
        "F6-spool [$label]: maxMsgSpoolUsageExceededDiscardedMsgCount must be > 0 (got $count)" || return 1
}

test_ac8_discard_spool_a() { verify_discard_spool_state "broker-a" "$BROKER_A_URL"; }
test_ac8_discard_spool_b() { verify_discard_spool_state "broker-b" "$BROKER_B_URL"; }

# ── AC 9 — F6-ttl discards ───────────────────────────────────────────────────

verify_discard_ttl_state() {
    local label="$1"
    local broker_url="$2"
    local body
    body=$(semp_monitor_get "$broker_url" "msgVpns/$BROKER_VPN/queues/$F6_TTL_QUEUE") || {
        log_fail "F6-ttl [$label]: GET queues/$F6_TTL_QUEUE failed"
        return 1
    }
    local count
    count=$(echo "$body" | jq -r '.data.maxTtlExpiredDiscardedMsgCount')
    assert_json_field "$body" \
        ".data.maxTtlExpiredDiscardedMsgCount > 0" "true" \
        "F6-ttl [$label]: maxTtlExpiredDiscardedMsgCount must be > 0 (got $count)" || return 1
}

test_ac9_discard_ttl_a() { verify_discard_ttl_state "broker-a" "$BROKER_A_URL"; }
test_ac9_discard_ttl_b() { verify_discard_ttl_state "broker-b" "$BROKER_B_URL"; }

# ── Run ──────────────────────────────────────────────────────────────────────

run_test "AC 2 — F1 multi-VPN state (broker-a)" test_ac2_multi_vpn_state_a
run_test "AC 2 — F1 multi-VPN state (broker-b)" test_ac2_multi_vpn_state_b
run_test "AC 3 — F2 multi-queue state (broker-a)" test_ac3_multi_queue_state_a
run_test "AC 3 — F2 multi-queue state (broker-b)" test_ac3_multi_queue_state_b
run_test "AC 4 — F3 connected client (broker-a)" test_ac4_connected_client_state_a
run_test "AC 4 — F3 connected client (broker-b)" test_ac4_connected_client_state_b
run_test "AC 5 — F4 sustained traffic (broker-a)" test_ac5_sustained_traffic_state_a
run_test "AC 5 — F4 sustained traffic (broker-b)" test_ac5_sustained_traffic_state_b
run_test "AC 8 — F6-spool discard count (broker-a)" test_ac8_discard_spool_a
run_test "AC 8 — F6-spool discard count (broker-b)" test_ac8_discard_spool_b
run_test "AC 9 — F6-ttl discard count (broker-a)" test_ac9_discard_ttl_a
run_test "AC 9 — F6-ttl discard count (broker-b)" test_ac9_discard_ttl_b

print_summary "Fixture verification"
