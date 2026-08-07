#!/usr/bin/env bash
# Claims-based tool RBAC (SOL-151440) driven over the real MCP JSON-RPC wire,
# with real Keycloak-minted tokens. Proves the full chain end to end: Keycloak
# issues a subject token carrying a groups claim -> the server verifies it ->
# resolveGroupsValue lifts the claim onto TokenInfo.Extra -> policy.Authorize
# decides -> withAuthorization allows or denies the tools/call.
#
# Run in two modes, both driven by run-all.sh, which owns the server lifecycle:
#
#   disabled   against the RBAC-off server the hop-2 suite already uses.
#              Proves RBAC off is a genuine no-op.
#   enabled    against a server restarted with TOOL_AUTHZ_RBAC (helpers.sh).
#
# ── Why the policy grants on `Ops` ───────────────────────────────────────────
# There are two tokens in this system and only one of them feeds tool RBAC:
#
#   subject token    agent -> MCP server    read by tool RBAC
#   exchanged token  MCP server -> broker   read by broker access levels
#
# Both carry a claim named `groups`, both layers read it, but they are
# independent decisions with independent vocabularies. The ordering settles
# which token withAuthorization sees: it runs at tools/call dispatch, while the
# exchanged token is not minted until the handler makes its first SEMP call.
#
# So the policy grants on `Ops`, which no broker has ever heard of. See
# TOOL_AUTHZ_RBAC in helpers.sh and README.md for the full rationale.
#
# ── Why the assertions read the server log ───────────────────────────────────
# A denial is not an HTTP 401 (authentication fails far earlier) and not a
# JSON-RPC error. It is an ordinary 200 carrying a tool-level error result, and
# both deny reasons return a byte-identical message on purpose — a caller must
# not learn whether an admin forgot to grant them or excluded them
# deliberately. The audit event is the only place that separates the two, so
# every decision assertion here goes through assert_authz_decision.

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/helpers.sh"

MODE="${1:-}"
case "$MODE" in
    enabled|disabled) ;;
    *) echo "usage: $(basename "$0") <enabled|disabled>" >&2; exit 2 ;;
esac

# The tool the policy grants, and the arguments every scenario calls it with.
# Read-only, works on both brokers, and already exercised by the hop-2 suite —
# so a failure here is about authorization, not about the tool.
# Names come from helpers.sh so the policy and these assertions cannot drift.
GRANTED_TOOL="$RBAC_GRANTED_TOOL"
UNGRANTED_TOOL="$RBAC_UNGRANTED_TOOL"
GRANT_GROUP="$RBAC_GRANT_GROUP"
TOOL_ARGS=$(jq -nc --arg b "$BROKER_A_ALIAS" '{broker:$b,maxResults:500}')
UNGRANTED_TOOL_ARGS=$(jq -nc --arg b "$BROKER_A_ALIAS" '{broker:$b,msgVpnName:"default",maxResults:100}')

# The caller-facing denial message, identical for both deny reasons.
# Mirrors authzDeniedMessage / authzMissingClaimMessage in
# internal/tools/authorization.go.
DENY_MESSAGE="You are not authorized to use this tool."

# assert_tool_denied <response> <label>
# Asserts the caller-visible shape of a denial: a tool-level error result, not
# a JSON-RPC error. Says nothing about which deny reason produced it — that is
# assert_authz_decision's job, and the whole point is that the wire cannot tell.
assert_tool_denied() {
    local resp="$1" label="$2"
    local jrpc_err is_err msg
    jrpc_err=$(jq -r '.error.message // empty' <<<"$resp")
    if [ -n "$jrpc_err" ]; then
        log_fail "$label: denial surfaced as a JSON-RPC error ($jrpc_err); expected a tool-level error result"
        return 1
    fi
    is_err=$(jq -r '.result.isError // false' <<<"$resp")
    if [ "$is_err" != "true" ]; then
        log_fail "$label: expected result.isError=true, got $is_err"
        return 1
    fi
    msg=$(jq -r '.result.content[0].text // ""' <<<"$resp")
    assert_contains "$msg" "$DENY_MESSAGE" "$label: denial should carry the standard message"
}

# ── Scenario: allow, and the audit trail names the granting group ────────────
# test-admin-user holds solace-admins AND Ops. Only Ops grants list-vpns, so
# matched_groups must be exactly ["Ops"] — proving the grant came from the
# MCP-only group and not from the group the broker also honours.
test_allow_names_granting_group() {
    local token mark resp content matched
    token=$(mint_token "$TEST_ADMIN_USER" "$TEST_USER_PASSWORD") || return 1

    mark=$(log_mark)
    resp=$(mcp_call_tool_as "$token" "$GRANTED_TOOL" "$TOOL_ARGS") \
        || { log_fail "$GRANTED_TOOL call transport failure"; return 1; }

    # Audit first. If the grant has broken, this names the reason; the data
    # assertion below would instead fail on jq choking on the denial string and
    # report an empty "Actual:", hiding the only record that explains why.
    assert_authz_decision "$mark" "$GRANTED_TOOL" "allowed" "" "allow" || {
        log_fail "  response was: $(jq -c '.result' <<<"$resp")"
        return 1
    }

    content=$(extract_content "$resp")
    assert_json_field "$content" '(.vpns.data | length) > 0' "true" \
        "granted caller should get real data back from $BROKER_A_ALIAS" || return 1

    matched=$(authz_records_since "$mark" "$GRANTED_TOOL" | jq -c '.matched_groups')
    if [ "$matched" != "[\"${GRANT_GROUP}\"]" ]; then
        log_fail "matched_groups=$matched, want [\"${GRANT_GROUP}\"]"
        log_fail "  the caller also holds solace-admins; if that appears here the policy is granting on a broker-shared group and the test proves nothing"
        return 1
    fi
    return 0
}

# ── Scenario: the grant is tool-scoped ──────────────────────────────────────
# Without this, a Policy.Authorize that ignored toolName entirely — "caller
# holds any group named anywhere in the config, so allow" — would pass every
# other scenario in this file. The only deny-with-groups caller elsewhere is
# test-readonly-user, whose group appears nowhere in the policy, so nothing
# else exercises the tool dimension of the index.
#
# Same caller as the allow scenario, same group, different tool.
test_grant_is_tool_scoped() {
    local token mark resp
    token=$(mint_token "$TEST_ADMIN_USER" "$TEST_USER_PASSWORD") || return 1

    mark=$(log_mark)
    resp=$(mcp_call_tool_as "$token" "$UNGRANTED_TOOL" "$UNGRANTED_TOOL_ARGS") \
        || { log_fail "$UNGRANTED_TOOL call transport failure"; return 1; }

    assert_tool_denied "$resp" "tool-scoped deny" || return 1
    assert_authz_decision "$mark" "$UNGRANTED_TOOL" "denied" "not_permitted" \
        "caller holds ${GRANT_GROUP}, which grants ${GRANTED_TOOL} but not ${UNGRANTED_TOOL}"
}

# ── Scenario: the broker authorizes independently ───────────────────────────
# Ops must not exist as an access-level group on either broker. Combined with
# the allow scenario above — where the MCP grant came solely from Ops yet the
# SEMP call still succeeded — this shows the broker made its own decision on
# solace-admins, a group the MCP policy never mentions. Two layers, two
# vocabularies, one claim name.
test_broker_authorizes_independently() {
    local port body names
    for port in "$BROKER_A_SEMP_PORT" "$BROKER_B_SEMP_PORT"; do
        body=$(semp_curl -s \
            "http://localhost:${port}/SEMP/v2/config/oauthProfiles/keycloak_profile/accessLevelGroups?select=groupName")
        names=$(jq -r '[.data[]?.groupName] | @json' <<<"$body" 2>/dev/null)

        # Positive control FIRST. Without it this scenario fails open: any SEMP
        # error body (renamed profile, 401, wrong path) has no .data, `.data[]?`
        # swallows it, and "Ops is absent" would be reported from a query that
        # never worked. Requiring a group we know IS configured proves the read
        # actually returned the access-level group list.
        if ! grep -q '"solace-admins"' <<<"$names"; then
            log_fail "broker on port $port: could not read a usable access-level group list (expected solace-admins to be present)"
            log_fail "  SEMP said: ${body:-<empty>}"
            return 1
        fi

        if grep -q "\"${GRANT_GROUP}\"" <<<"$names"; then
            log_fail "broker on port $port has ${GRANT_GROUP} as an access-level group: $names"
            log_fail "  the MCP-only group must not be mapped on any broker, or the isolation this suite depends on is lost"
            return 1
        fi
    done
    return 0
}

# ── Scenario: a denial short-circuits before the broker is ever contacted ───
# Otherwise invisible: a withAuthorization that returned the deny result AND
# still called next(...) would produce an identical response and an identical
# audit record. The observable is hop 2 — a denied call must never trigger a
# token exchange, because the exchange only happens inside the tool handler.
# This is also the empirical half of the ordering argument in README.md.
test_deny_short_circuits_before_broker() {
    local token before after i
    token=$(mint_token "$TEST_READONLY_USER" "$TEST_USER_PASSWORD") || return 1

    before=$(count_token_exchanges)
    mcp_call_tool_as "$token" "$GRANTED_TOOL" "$TOOL_ARGS" >/dev/null \
        || { log_fail "$GRANTED_TOOL call transport failure"; return 1; }

    # Keycloak's TOKEN_EXCHANGE line reaches `docker logs` asynchronously, so
    # a delta of 0 sampled immediately could just be lag. Wait out the window
    # the cache-hit scenario found sufficient, then assert nothing appeared.
    for i in $(seq 1 10); do
        sleep 0.2
    done
    after=$(count_token_exchanges)

    if [ "$after" -ne "$before" ]; then
        log_fail "a denied call triggered $((after - before)) token exchange(s); the deny must short-circuit before the tool handler reaches the broker"
        return 1
    fi
    return 0
}

# ── Scenario: deny, caller has groups but none that grant ───────────────────
test_deny_not_permitted() {
    local token mark resp
    token=$(mint_token "$TEST_READONLY_USER" "$TEST_USER_PASSWORD") || return 1

    mark=$(log_mark)
    resp=$(mcp_call_tool_as "$token" "$GRANTED_TOOL" "$TOOL_ARGS") \
        || { log_fail "$GRANTED_TOOL call transport failure"; return 1; }

    assert_tool_denied "$resp" "not-permitted deny" || return 1
    assert_authz_decision "$mark" "$GRANTED_TOOL" "denied" "not_permitted" "not-permitted deny" || return 1

    # The caller's own groups must never reach the audit record on a deny.
    local record
    record=$(authz_records_since "$mark" "$GRANTED_TOOL")
    if grep -qF "solace-readonly" <<<"$record"; then
        log_fail "deny audit record leaked the caller's groups: $record"
        return 1
    fi
    return 0
}

# ── Scenario: deny, token carries no groups claim at all ────────────────────
# Minted through agentic-app-client-nogroups, which lacks the
# realm-roles-to-groups mapper. Same user, same audience, same authentication
# result — the only difference is the claim set, because which claims a token
# carries depends on which client requested it.
test_deny_missing_claim() {
    local token mark resp expected
    token=$(mint_token "$TEST_ADMIN_USER" "$TEST_USER_PASSWORD" "$HOP1_CLIENT_ID_NOGROUPS") || return 1

    mark=$(log_mark)
    resp=$(mcp_call_tool_as "$token" "$GRANTED_TOOL" "$TOOL_ARGS") \
        || { log_fail "$GRANTED_TOOL call transport failure"; return 1; }

    assert_tool_denied "$resp" "missing-claim deny" || return 1
    assert_authz_decision "$mark" "$GRANTED_TOOL" "denied" "missing_claim" "missing-claim deny" || return 1

    # The diagnostic that tells an admin what the server was looking for.
    expected=$(authz_records_since "$mark" "$GRANTED_TOOL" | jq -r '.expected_claim // "<none>"')
    if [ "$expected" != "groups" ]; then
        log_fail "expected_claim=$expected, want groups"
        return 1
    fi

    # Same user as the allow scenario — so this is the claim set being denied,
    # not the identity.
    return 0
}

# ── Scenario: the two deny reasons are indistinguishable to the caller ──────
# Deliberate: a caller must not be able to tell "the admin has not granted you
# this" from "your token is missing the claim". Asserting the two responses are
# byte-identical pins that property, and is why every other deny assertion in
# this file reads the audit log instead of the wire.
test_deny_reasons_indistinguishable_to_caller() {
    local not_permitted_token missing_claim_token resp_a resp_b a b
    not_permitted_token=$(mint_token "$TEST_READONLY_USER" "$TEST_USER_PASSWORD") || return 1
    missing_claim_token=$(mint_token "$TEST_ADMIN_USER" "$TEST_USER_PASSWORD" "$HOP1_CLIENT_ID_NOGROUPS") || return 1

    resp_a=$(mcp_call_tool_as "$not_permitted_token" "$GRANTED_TOOL" "$TOOL_ARGS") \
        || { log_fail "not_permitted call transport failure"; return 1; }
    resp_b=$(mcp_call_tool_as "$missing_claim_token" "$GRANTED_TOOL" "$TOOL_ARGS") \
        || { log_fail "missing_claim call transport failure"; return 1; }

    # Both must actually BE denials before comparing them. Equality alone is
    # satisfied by two identical successes (RBAC silently off) and by two
    # identical empty strings (both calls failing) — this scenario would then
    # pass while proving the opposite of its name.
    assert_tool_denied "$resp_a" "not_permitted deny" || return 1
    assert_tool_denied "$resp_b" "missing_claim deny" || return 1

    # Drop only correlation_id, which is stamped per request and unique by
    # design. Dropping the whole _meta would blind this assertion to any future
    # field that leaked the deny reason — the exact thing it exists to catch.
    a=$(jq -S '.result | del(._meta.correlation_id)' <<<"$resp_a")
    b=$(jq -S '.result | del(._meta.correlation_id)' <<<"$resp_b")

    if [ "$a" != "$b" ]; then
        log_fail "the two deny reasons produced different caller-visible results; they must be indistinguishable"
        log_fail "  not_permitted: $a"
        log_fail "  missing_claim: $b"
        return 1
    fi
    return 0
}

# ── Scenario: list-brokers is structurally exempt ───────────────────────────
# The policy grants test-readonly-user nothing, and list-brokers is not in the
# policy at all — yet it must still work. Exemption is structural, not a rule:
# RegisterListBrokers takes no policy argument, so the gate is never wrapped
# around it and never runs. Hence the second assertion: no audit record.
test_list_brokers_exempt() {
    local token mark resp content records
    token=$(mint_token "$TEST_READONLY_USER" "$TEST_USER_PASSWORD") || return 1

    mark=$(log_mark)
    resp=$(mcp_call_tool_as "$token" "list-brokers" '{}') \
        || { log_fail "list-brokers call transport failure"; return 1; }

    content=$(extract_content "$resp")
    assert_json_field "$content" '(.brokers | length) > 0' "true" \
        "list-brokers must stay available to any authenticated caller, including one the policy grants nothing" || return 1

    records=$(authz_records_since "$mark" "list-brokers")
    if [ -n "$records" ]; then
        log_fail "list-brokers emitted a tool authorization record; it should never reach the gate: $records"
        return 1
    fi
    return 0
}

# ── Scenario: RBAC disabled is a genuine no-op ──────────────────────────────
# The same call that test_deny_not_permitted asserts is denied must succeed
# here, and the gate must not run at all.
#
# The server for this phase runs TOOL_AUTHZ_RBAC_OFF — the real policy, with
# only the flag flipped. Running it against an empty policy instead would make
# the scenario unable to catch "enabled:false is ignored but the policy map is
# still consulted", since there would be nothing to consult.
test_rbac_disabled_is_noop() {
    local token mark resp content records
    token=$(mint_token "$TEST_READONLY_USER" "$TEST_USER_PASSWORD") || return 1

    mark=$(log_mark)
    resp=$(mcp_call_tool_as "$token" "$GRANTED_TOOL" "$TOOL_ARGS") \
        || { log_fail "$GRANTED_TOOL call transport failure"; return 1; }

    content=$(extract_content "$resp")
    assert_json_field "$content" '(.vpns.data | length) > 0' "true" \
        "with RBAC off, a caller denied under the policy should succeed" || return 1

    records=$(authz_records_since "$mark")
    if [ -n "$records" ]; then
        log_fail "RBAC is off but the gate emitted audit records: $records"
        return 1
    fi
    return 0
}

# ── Run ──────────────────────────────────────────────────────────────────────

# Positive control, before any scenario. Several assertions below turn on the
# ABSENCE of audit records, and absence proves nothing unless we know the
# server is genuinely in the mode this phase expects — a phase pointed at the
# wrong server, or a broken log path, would otherwise report a clean pass.
# Fail the whole phase loudly instead.
if ! assert_server_rbac_mode "$MODE"; then
    log_fail "aborting the $MODE phase: cannot confirm the server's tool_authorization state"
    exit 1
fi

if [ "$MODE" = "disabled" ]; then
    run_test "RBAC disabled is a no-op"                  test_rbac_disabled_is_noop
    print_summary "Tool RBAC tests (disabled)"
else
    run_test "Allow names the granting group"            test_allow_names_granting_group
    run_test "Grant is tool-scoped"                      test_grant_is_tool_scoped
    run_test "Broker authorizes independently"           test_broker_authorizes_independently
    run_test "Deny: groups present, none permit"         test_deny_not_permitted
    run_test "Deny: no groups claim at all"              test_deny_missing_claim
    run_test "Deny reasons indistinguishable to caller"  test_deny_reasons_indistinguishable_to_caller
    run_test "Deny short-circuits before the broker"     test_deny_short_circuits_before_broker
    run_test "list-brokers is structurally exempt"       test_list_brokers_exempt
    print_summary "Tool RBAC tests (enabled)"
fi
