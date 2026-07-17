#!/usr/bin/env bash
# Scenario 3: Negative-path smoke — confirm three tool-execution failures
# reach the caller as clean MCP structured errors (SOL-150767 / SOL-147161
# §3.7). Behavior coverage lives in internal/semp/resilience/sender_test.go
# and cookiejar_test.go; this script only proves the envelope contract
# survives the full MCP-server + real-broker stack.
#
# Scenarios:
#   - Bad credentials → SEMPv2 401, retryable=false, no credential leak
#   - Broker unreachable → RetriesExhaustedError, retryable=true, no status
#   - 404 not-found → SEMPv2 404, retryable=false
#
# The two negative-path broker aliases (broker-bad-creds, broker-dead) are
# declared in the shared write_config() so a single MCP server sees all four
# brokers, matching production shape. Requires: MCP server running on
# $MCP_URL, broker fixtures created on broker-a (test-queue etc.).

set -euo pipefail
source "$(dirname "$0")/helpers.sh"

# The literal password in broker-bad-creds (kept in sync with write_config()).
# Asserted absent from the entire tool envelope as a credential-leak guard.
NEG_BAD_PASSWORD="wrong-password-for-e2e-negative-path"

# ── Tests ────────────────────────────────────────────────────────────────────

# Bad credentials → SEMPv2 401 surfaces as isError with status=401 and
# retryable=false. The password literal must not appear anywhere in the
# JSON-RPC envelope (defense in depth against future credential leaks in
# error messages or structured content).
test_bad_credentials() {
    local response
    response=$(mcp_call_tool "get-vpn-status" \
        '{"broker":"broker-bad-creds","msgVpnName":"default"}') || return 1

    # Tool execution errors are returned as .result with isError=true, NOT as
    # a JSON-RPC .error — verify the tool actually executed rather than
    # short-circuiting at the protocol layer.
    assert_json_field "$response" ".error" "null" \
        "bad-creds: tool must execute (no JSON-RPC error envelope)" || return 1
    assert_json_field "$response" ".result.isError" "true" \
        "bad-creds: result.isError must be true" || return 1
    assert_json_field "$response" ".result.structuredContent.status" "401" \
        "bad-creds: structuredContent.status must be 401" || return 1
    assert_json_field "$response" ".result.structuredContent.retryable" "false" \
        "bad-creds: 401 must be non-retryable" || return 1
    assert_json_field "$response" \
        "(.result.structuredContent.error | length) > 0" "true" \
        "bad-creds: structuredContent.error must be non-empty" || return 1

    # Credential-leak guard: the literal password must not appear anywhere
    # in the envelope. Covers the tool error message, structured content,
    # and any incidental echo (e.g. from URL rendering).
    assert_not_contains "$response" "$NEG_BAD_PASSWORD" \
        "bad-creds: password must not leak into the tool envelope" || return 1
}

# Broker unreachable → resilience layer raises RetriesExhaustedError with
# StatusCode=0 (no response was received), which the tool layer maps to
# retryable=true and omits the status field. Empty status is the signal
# that the failure was a connection error, not an HTTP error.
test_dead_broker() {
    local response
    response=$(mcp_call_tool "get-vpn-status" \
        '{"broker":"broker-dead","msgVpnName":"default"}') || return 1

    assert_json_field "$response" ".error" "null" \
        "dead-broker: tool must execute (no JSON-RPC error envelope)" || return 1
    assert_json_field "$response" ".result.isError" "true" \
        "dead-broker: result.isError must be true" || return 1
    assert_json_field "$response" ".result.structuredContent.retryable" "true" \
        "dead-broker: connection error must be retryable" || return 1
    assert_json_field "$response" \
        "(.result.structuredContent.error | length) > 0" "true" \
        "dead-broker: structuredContent.error must be non-empty" || return 1
    # No HTTP response was received, so the status field must be absent.
    # A present status would mean the resilience layer captured a response
    # from some other endpoint — a real defect.
    assert_json_field "$response" \
        '.result.structuredContent | has("status")' "false" \
        "dead-broker: structuredContent.status must be absent" || return 1
}

# Nonexistent queue → SEMPv2 signals "not found" as HTTP 400 with sempCode 6
# (NOT_FOUND), not HTTP 404. Assert on the sempCode (which encodes the actual
# semantics) as well as the status, and confirm the failure is non-retryable.
# Uses a queue name mangled with $$ so parallel runs and stray fixtures cannot
# collide.
test_nonexistent_queue() {
    local queue_name="does-not-exist-e2e-negative-$$"
    local args
    args=$(jq -nc --arg q "$queue_name" \
        '{broker:"broker-a",msgVpnName:"default",queueName:$q}')

    local response
    response=$(mcp_call_tool "get-queue-metrics" "$args") || return 1

    assert_json_field "$response" ".error" "null" \
        "not-found-queue: tool must execute (no JSON-RPC error envelope)" || return 1
    assert_json_field "$response" ".result.isError" "true" \
        "not-found-queue: result.isError must be true" || return 1
    assert_json_field "$response" ".result.structuredContent.status" "400" \
        "not-found-queue: structuredContent.status must be 400 (SEMPv2 uses 400+sempCode)" || return 1
    assert_json_field "$response" ".result.structuredContent.sempCode" "6" \
        "not-found-queue: structuredContent.sempCode must be 6 (NOT_FOUND)" || return 1
    assert_json_field "$response" ".result.structuredContent.retryable" "false" \
        "not-found-queue: not-found must be non-retryable" || return 1
}

# ── Run ──────────────────────────────────────────────────────────────────────

run_test "Bad credentials → 401 error envelope"       test_bad_credentials
run_test "Unreachable broker → retryable error"       test_dead_broker
run_test "Nonexistent queue → 400+NOT_FOUND envelope" test_nonexistent_queue

print_summary "Negative-path tests"
