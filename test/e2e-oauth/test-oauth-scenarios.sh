#!/usr/bin/env bash
# OAuth token-exchange functional tests, driven over the real MCP JSON-RPC
# wire with tokens minted from a live Keycloak (no mocks, no static dev
# token). Covers Hop 1 (agent -> MCP server JWT validation) and Hop 2 (MCP
# server -> broker RFC 8693 token exchange): admin path, cache hit, cache
# invalidation on 401, insufficient permission, audience isolation, and
# wrong-audience rejection.
#
# Fixture model: one disposable queue on prod-us (broker A), created on entry
# and swept on exit, used by the insufficient-permission scenario's write-tool
# call.

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/helpers.sh"

FIXTURE_QUEUE="e2e-oauth-queue"

sweep_oauth_fixtures() {
    semp_delete "$BROKER_A_SEMP_CONFIG" "msgVpns/$BROKER_VPN/queues/$FIXTURE_QUEUE"
}
trap sweep_oauth_fixtures EXIT
sweep_oauth_fixtures
semp_post "$BROKER_A_SEMP_CONFIG" "msgVpns/$BROKER_VPN/queues" \
    "{\"queueName\":\"$FIXTURE_QUEUE\",\"accessType\":\"non-exclusive\",\"permission\":\"consume\",\"ingressEnabled\":true,\"egressEnabled\":true}" >/dev/null

# ── Scenario 1: Admin path ──────────────────────────────────────────────────
# test-admin-user (solace-admins) calls a read-only tool on prod-us -> success.
test_admin_path() {
    local token args resp content
    token=$(mint_token "$TEST_ADMIN_USER" "$TEST_USER_PASSWORD") || return 1
    args=$(jq -nc --arg b "$BROKER_A_ALIAS" '{broker:$b,maxResults:500}')
    resp=$(mcp_call_tool_as "$token" "list-vpns" "$args") || { log_fail "list-vpns call transport failure"; return 1; }
    content=$(extract_content "$resp")
    assert_json_field "$content" '(.vpns.data | length) > 0' "true" "list-vpns should return at least one VPN on $BROKER_A_ALIAS"
}

# ── Scenario 2: Cache hit ────────────────────────────────────────────────────
# Two consecutive tool calls by the same user -> only 1 IdP token-exchange
# request observable (Keycloak's own log is the observation point — see
# count_token_exchanges).
test_cache_hit() {
    local token args before after delta
    token=$(mint_token "$TEST_ADMIN_USER" "$TEST_USER_PASSWORD") || return 1
    args=$(jq -nc --arg b "$BROKER_A_ALIAS" '{broker:$b,maxResults:500}')

    before=$(count_token_exchanges)
    mcp_call_tool_as "$token" "list-vpns" "$args" >/dev/null || { log_fail "first call failed"; return 1; }
    mcp_call_tool_as "$token" "list-vpns" "$args" >/dev/null || { log_fail "second call failed"; return 1; }
    after=$(count_token_exchanges)
    delta=$((after - before))

    if [ "$delta" -ne 1 ]; then
        log_fail "expected exactly 1 token-exchange request across 2 calls by the same user, saw $delta"
        return 1
    fi
    return 0
}

# ── Scenario 3: Cache invalidation on 401 ───────────────────────────────────
# The poisoning call's own pass/fail is deliberately not asserted (whether the
# auth layer's in-flight retry succeeds silently is an implementation detail).
# What's asserted: the call after restoring the profile succeeds and used a
# fresh token exchange, not a stale cached one.
test_cache_invalidation_on_401() {
    local token args mid_before after
    token=$(mint_token "$TEST_ADMIN_USER" "$TEST_USER_PASSWORD") || return 1
    args=$(jq -nc --arg b "$BROKER_A_ALIAS" '{broker:$b,maxResults:500}')

    mcp_call_tool_as "$token" "list-vpns" "$args" >/dev/null || { log_fail "priming call failed"; return 1; }
    mid_before=$(count_token_exchanges)

    # Poison: require a different audience so the cached Hop-2 token (minted
    # for the real audience) fails broker-side validation on next use.
    upsert_profile "$BROKER_A_SEMP_PORT" "poisoned-audience-temp" || { log_fail "failed to poison profile"; return 1; }

    # Exercise the poisoned state — outcome intentionally not asserted.
    mcp_call_tool_as "$token" "list-vpns" "$args" >/dev/null 2>&1 || true

    # Restore.
    upsert_profile "$BROKER_A_SEMP_PORT" "$BROKER_A_AUDIENCE" || { log_fail "failed to restore profile"; return 1; }

    if ! mcp_call_tool_as "$token" "list-vpns" "$args" >/dev/null; then
        log_fail "call after cache invalidation did not succeed"
        return 1
    fi
    after=$(count_token_exchanges)

    if [ "$after" -le "$mid_before" ]; then
        log_fail "expected a new token exchange after cache invalidation, saw none (before=$mid_before after=$after)"
        return 1
    fi
    return 0
}

# ── Scenario 4: Insufficient permission ─────────────────────────────────────
# test-readonly-user (solace-readonly) calls a write tool -> SEMP code=72 ->
# "Authorization failed on broker "prod-us"." with no other detail leaked.
test_insufficient_permission() {
    local token args resp msg
    token=$(mint_token "$TEST_READONLY_USER" "$TEST_USER_PASSWORD") || return 1
    args=$(jq -nc --arg b "$BROKER_A_ALIAS" --arg q "$FIXTURE_QUEUE" '{broker:$b,msgVpnName:"default",queueName:$q}')
    resp=$(mcp_call_tool_as "$token" "clear-queue-stats" "$args") || { log_fail "clear-queue-stats transport failure"; return 1; }
    msg=$(jq -r '.result.content[0].text // .error.message // ""' <<<"$resp")

    assert_contains "$msg" "Authorization failed on broker \"${BROKER_A_ALIAS}\"." \
        "readonly user's write-tool call should be denied with the alias-tagged message"
}

# ── Scenario 5: Audience isolation ──────────────────────────────────────────
# test-admin-user calls a tool against test-us (audience solace-broker-second)
# -> success, proving the custom-audience mapper selects the right audience
# per target broker.
test_audience_isolation() {
    local token args resp content
    token=$(mint_token "$TEST_ADMIN_USER" "$TEST_USER_PASSWORD") || return 1
    args=$(jq -nc --arg b "$BROKER_B_ALIAS" '{broker:$b,maxResults:500}')
    resp=$(mcp_call_tool_as "$token" "list-vpns" "$args") || { log_fail "list-vpns call transport failure"; return 1; }
    content=$(extract_content "$resp")
    assert_json_field "$content" '(.vpns.data | length) > 0' "true" "list-vpns should return at least one VPN on $BROKER_B_ALIAS"
}

# ── Scenario 6: Wrong audience denied ───────────────────────────────────────
# A tool call against the deliberately-misconfigured test-us-wrong-audience
# alias (same URL as test-us, but configured with prod-us's audience) ->
# broker rejects the exchanged token. Exercises the real Hop-2 exchange code
# path rather than a raw curl bypassing the MCP server.
test_wrong_audience_denied() {
    local token args resp is_err jrpc_err
    token=$(mint_token "$TEST_ADMIN_USER" "$TEST_USER_PASSWORD") || return 1
    args=$(jq -nc '{broker:"test-us-wrong-audience",maxResults:500}')
    resp=$(mcp_call_tool_as "$token" "list-vpns" "$args") || { log_fail "call transport failure"; return 1; }

    jrpc_err=$(jq -r '.error.message // empty' <<<"$resp")
    is_err=$(jq -r '.result.isError // false' <<<"$resp")
    if [ -z "$jrpc_err" ] && [ "$is_err" != "true" ]; then
        log_fail "expected the wrong-audience call to fail, but it succeeded"
        return 1
    fi
    return 0
}

# ── Run ──────────────────────────────────────────────────────────────────────

run_test "Admin path (prod-us)"                    test_admin_path
run_test "Cache hit (1 exchange for 2 calls)"       test_cache_hit
run_test "Cache invalidation on 401"                test_cache_invalidation_on_401
run_test "Insufficient permission (readonly write)" test_insufficient_permission
run_test "Audience isolation (test-us)"             test_audience_isolation
run_test "Wrong audience denied"                    test_wrong_audience_denied

print_summary "OAuth token-exchange tests"
