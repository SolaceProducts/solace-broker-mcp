#!/usr/bin/env bash
# Config-tool (management) functional tests, driven over the MCP JSON-RPC wire
# (Mode 1, no LLM). Each tool family (VPN, queue, topic-endpoint, RDP) is
# exercised through a full create → verify → update → verify → delete →
# verify-absent round-trip on both brokers, plus cross-broker isolation,
# annotation, and error-translation checks.
#
# Fixture model: per-test ownership. Each test creates its own e2e-config-*
# object, acts on it, asserts, and deletes it. A suite-level sweep runs on entry
# (pre-clean) and on exit (safety net) so a mid-run failure never leaks state.
#
# Verification mixes two layers, per the tool under test:
#   - presence/absence → monitoring tools (list-vpns / list-queues); this is the
#     read-after-write / cache-invalidation assertion (reads hit the broker live).
#   - updated attribute → SEMP-direct monitor GET (the monitoring tools don't
#     surface arbitrary config attributes).
#   - topic-endpoints have no monitoring tool, so all of their verification is
#     SEMP-direct.

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/helpers.sh"

# Sweep on exit (standalone runs / mid-run failure) and pre-clean on entry.
trap sweep_config_fixtures EXIT
sweep_config_fixtures

# ── Per-test helpers ─────────────────────────────────────────────────────────

broker_url_for() {
    case "$1" in
        broker-a) echo "$BROKER_A_URL" ;;
        broker-b) echo "$BROKER_B_URL" ;;
    esac
}

# Call a config tool and assert it succeeded: no JSON-RPC error and the tool
# result is not flagged isError. On failure, logs the broker's message.
#   $1 tool   $2 args_json   $3 description
call_tool_ok() {
    local tool="$1" args="$2" desc="$3" resp jrpc_err is_err
    resp=$(mcp_call_tool "$tool" "$args") || { log_fail "$desc: MCP transport failure"; return 1; }
    jrpc_err=$(jq -r '.error.message // empty' <<<"$resp")
    if [ -n "$jrpc_err" ]; then
        log_fail "$desc: JSON-RPC error: $jrpc_err"
        return 1
    fi
    is_err=$(jq -r '.result.isError // false' <<<"$resp")
    if [ "$is_err" = "true" ]; then
        log_fail "$desc: tool returned isError=true: $(jq -r '.result.content[0].text // ""' <<<"$resp")"
        return 1
    fi
    return 0
}

# Assert an object name is (or is not) present in a list tool's output.
#   $1 broker   $2 list_tool (list-vpns|list-queues|list-rdps)   $3 name   $4 present(true|false)
assert_listed() {
    local broker="$1" list_tool="$2" name="$3" present="$4"
    local args resp content data_path key expr
    case "$list_tool" in
        list-vpns)
            args=$(jq -nc --arg b "$broker" '{broker:$b,maxResults:500}')
            data_path=".vpns.data"; key="msgVpnName" ;;
        list-queues)
            args=$(jq -nc --arg b "$broker" '{broker:$b,msgVpnName:"default",maxResults:500}')
            data_path=".queues.data | map(select(.msgVpnName==\"default\"))"; key="queueName" ;;
        list-rdps)
            args=$(jq -nc --arg b "$broker" '{broker:$b,msgVpnName:"default",maxResults:500}')
            data_path=".rdps.data"; key="restDeliveryPointName" ;;
    esac
    resp=$(mcp_call_tool "$list_tool" "$args") || return 1
    content=$(extract_content "$resp")
    if [ "$present" = "true" ]; then
        expr="($data_path | map(.$key) | index(\"$name\")) != null"
    else
        expr="($data_path | map(.$key) | index(\"$name\")) == null"
    fi
    assert_json_field "$content" "$expr" "true" \
        "$list_tool [$broker]: $name present=$present"
}

# ── VPN round-trip ───────────────────────────────────────────────────────────

test_vpn_roundtrip() {
    local broker="$1"
    local name="e2e-config-vpn-$broker"
    local burl
    burl=$(broker_url_for "$broker")

    call_tool_ok "create-message-vpn" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:$n,msgVpnConfig:{enabled:false}}')" \
        "create-message-vpn [$broker]" || return 1
    assert_listed "$broker" "list-vpns" "$name" "true" || return 1

    call_tool_ok "update-message-vpn" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:$n,msgVpnConfig:{maxConnectionCount:50}}')" \
        "update-message-vpn [$broker]" || return 1
    verify_monitor_object "$burl" "$broker" "msgVpns/$name" 30 '.data.maxConnectionCount == 50' \
        || { log_fail "update-message-vpn [$broker]: maxConnectionCount not reflected"; return 1; }

    call_tool_ok "delete-message-vpn" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:$n}')" \
        "delete-message-vpn [$broker]" || return 1
    assert_listed "$broker" "list-vpns" "$name" "false" || return 1
}

# ── Queue round-trip ─────────────────────────────────────────────────────────

test_queue_roundtrip() {
    local broker="$1"
    local name="e2e-config-queue-$broker"
    local burl
    burl=$(broker_url_for "$broker")

    call_tool_ok "create-queue" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:"default",queueName:$n,queueConfig:{accessType:"non-exclusive"}}')" \
        "create-queue [$broker]" || return 1
    assert_listed "$broker" "list-queues" "$name" "true" || return 1

    call_tool_ok "update-queue" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:"default",queueName:$n,queueConfig:{maxMsgSpoolUsage:10}}')" \
        "update-queue [$broker]" || return 1
    verify_monitor_object "$burl" "$broker" "msgVpns/$BROKER_VPN/queues/$name" 30 '.data.maxMsgSpoolUsage == 10' \
        || { log_fail "update-queue [$broker]: maxMsgSpoolUsage not reflected"; return 1; }

    call_tool_ok "delete-queue" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:"default",queueName:$n}')" \
        "delete-queue [$broker]" || return 1
    assert_listed "$broker" "list-queues" "$name" "false" || return 1
}

# ── Topic-endpoint round-trip (SEMP-direct verify: no monitoring tool) ────────

test_te_roundtrip() {
    local broker="$1"
    local name="e2e-config-te-$broker"
    local burl
    burl=$(broker_url_for "$broker")

    call_tool_ok "create-topic-endpoint" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:"default",topicEndpointName:$n,topicEndpointConfig:{accessType:"non-exclusive"}}')" \
        "create-topic-endpoint [$broker]" || return 1
    verify_monitor_object "$burl" "$broker" "msgVpns/$BROKER_VPN/topicEndpoints/$name" \
        || { log_fail "create-topic-endpoint [$broker]: $name not visible"; return 1; }

    # Topic endpoints use maxSpoolUsage (queues use maxMsgSpoolUsage).
    call_tool_ok "update-topic-endpoint" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:"default",topicEndpointName:$n,topicEndpointConfig:{maxSpoolUsage:10}}')" \
        "update-topic-endpoint [$broker]" || return 1
    verify_monitor_object "$burl" "$broker" "msgVpns/$BROKER_VPN/topicEndpoints/$name" 30 '.data.maxSpoolUsage == 10' \
        || { log_fail "update-topic-endpoint [$broker]: maxSpoolUsage not reflected"; return 1; }

    call_tool_ok "delete-topic-endpoint" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:"default",topicEndpointName:$n}')" \
        "delete-topic-endpoint [$broker]" || return 1
    if semp_monitor_get "$burl" "msgVpns/$BROKER_VPN/topicEndpoints/$name" >/dev/null 2>&1; then
        log_fail "delete-topic-endpoint [$broker]: $name still visible after delete"
        return 1
    fi
}

# ── RDP round-trip ───────────────────────────────────────────────────────────
# RDPs live in the default VPN and have monitoring tools (list-rdps,
# get-rdp-status), so presence/absence and the enabled read-back go through the
# MCP monitoring path (the read-after-write / cache-invalidation assertion). A
# newly created RDP is disabled by default and carries no consumers or queue
# bindings — no MCP tool creates those, so they are out of scope here.
test_rdp_roundtrip() {
    local broker="$1"
    local name="e2e-config-rdp-$broker"

    # create — disabled by default
    local resp content
    call_tool_ok "create-rdp" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:"default",restDeliveryPointName:$n}')" \
        "create-rdp [$broker]" || return 1
    assert_listed "$broker" "list-rdps" "$name" "true" || return 1

    # Baseline: a freshly created RDP is disabled by default. Asserting this makes
    # the enabled=true check after the update meaningful — it proves the update
    # changed state rather than reading a value that was already set.
    resp=$(mcp_call_tool "list-rdps" "$(jq -nc --arg b "$broker" '{broker:$b,msgVpnName:"default",maxResults:500}')") || return 1
    content=$(extract_content "$resp")
    assert_json_field "$content" \
        "(.rdps.data[] | select(.restDeliveryPointName==\"$name\") | .enabled)" "false" \
        "create-rdp [$broker]: $name is disabled by default" || return 1

    # get-rdp-status resolves the specific RDP (DoD: visible via get-rdp-status).
    # Assert the RDP name is in the payload, not just the always-present rdpStatus
    # step key — otherwise the check passes for any/empty response.
    resp=$(mcp_call_tool "get-rdp-status" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:"default",restDeliveryPointName:$n}')") || return 1
    content=$(extract_content "$resp")
    assert_contains "$content" "rdpStatus" \
        "get-rdp-status [$broker]: returns an rdpStatus section" || return 1
    assert_contains "$content" "$name" \
        "get-rdp-status [$broker]: resolves the specific RDP $name" || return 1

    # update: enable it — list-rdps must read back enabled=true (not stale)
    call_tool_ok "update-rdp" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:"default",restDeliveryPointName:$n,rdpConfig:{enabled:true}}')" \
        "update-rdp [$broker]" || return 1
    resp=$(mcp_call_tool "list-rdps" "$(jq -nc --arg b "$broker" '{broker:$b,msgVpnName:"default",maxResults:500}')") || return 1
    content=$(extract_content "$resp")
    assert_json_field "$content" \
        "(.rdps.data[] | select(.restDeliveryPointName==\"$name\") | .enabled)" "true" \
        "update-rdp [$broker]: enabled=true reflected" || return 1

    # delete
    call_tool_ok "delete-rdp" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:"default",restDeliveryPointName:$n}')" \
        "delete-rdp [$broker]" || return 1
    assert_listed "$broker" "list-rdps" "$name" "false" || return 1
}

# ── Queue-subscription round-trip (SOL-153868) ───────────────────────────────
# create-queue-subscription / list-queue-subscriptions / delete-queue-subscription
# on a queue owned by this test. Covers two wildcard topics (multi-level '>'
# and single-level '*') since queue-subscription is the first tool in this
# codebase to put an arbitrary topic string — potentially containing '/' — into
# a SEMP path segment (delete) or request body (create). Absence after delete
# is asserted positively (a second list call), not inferred from the delete
# call returning success: removing a subscription raises no broker error and
# the failure mode is silent (messages simply stop arriving).
test_queue_subscription_roundtrip() {
    local broker="$1"
    local name="e2e-config-queue-sub-$broker"
    local resp content

    call_tool_ok "create-queue" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:"default",queueName:$n,queueConfig:{accessType:"non-exclusive"}}')" \
        "queue-subscription: create-queue [$broker]" || return 1

    call_tool_ok "create-queue-subscription" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:"default",queueName:$n,subscriptionTopic:"ABC/>"}')" \
        "create-queue-subscription [$broker]: ABC/>" || return 1
    call_tool_ok "create-queue-subscription" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:"default",queueName:$n,subscriptionTopic:"foo/*/bar"}')" \
        "create-queue-subscription [$broker]: foo/*/bar" || return 1

    resp=$(mcp_call_tool "list-queue-subscriptions" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:"default",queueName:$n,maxResults:500}')") || return 1
    content=$(extract_content "$resp")
    assert_json_field "$content" \
        '(.subscriptions.data | map(.subscriptionTopic) | index("ABC/>")) != null' "true" \
        "list-queue-subscriptions [$broker]: ABC/> present after create" || return 1
    assert_json_field "$content" \
        '(.subscriptions.data | map(.subscriptionTopic) | index("foo/*/bar")) != null' "true" \
        "list-queue-subscriptions [$broker]: foo/*/bar present after create" || return 1

    call_tool_ok "delete-queue-subscription" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:"default",queueName:$n,subscriptionTopic:"ABC/>"}')" \
        "delete-queue-subscription [$broker]: ABC/>" || return 1

    resp=$(mcp_call_tool "list-queue-subscriptions" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:"default",queueName:$n,maxResults:500}')") || return 1
    content=$(extract_content "$resp")
    assert_json_field "$content" \
        '(.subscriptions.data | map(.subscriptionTopic) | index("ABC/>")) == null' "true" \
        "list-queue-subscriptions [$broker]: ABC/> absent after delete (asserted positively)" || return 1
    assert_json_field "$content" \
        '(.subscriptions.data | map(.subscriptionTopic) | index("foo/*/bar")) != null' "true" \
        "list-queue-subscriptions [$broker]: foo/*/bar still present (untouched by the other topic's delete)" || return 1

    # AC4: list-queue-subscriptions must stay permissive for a real, existing
    # queue that simply has no subscriptions left (isError absent, empty
    # array) — the preflight exists to distinguish "queue missing" from
    # "queue has no subscriptions", not to reject the latter. Delete the
    # remaining topic first so this is a real empty-list case, not just the
    # nonexistent-queue path already covered elsewhere.
    call_tool_ok "delete-queue-subscription" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:"default",queueName:$n,subscriptionTopic:"foo/*/bar"}')" \
        "delete-queue-subscription [$broker]: foo/*/bar" || return 1

    resp=$(mcp_call_tool "list-queue-subscriptions" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:"default",queueName:$n,maxResults:500}')") || return 1
    assert_json_field "$resp" ".result.isError // false" "false" \
        "list-queue-subscriptions [$broker]: real queue with zero subscriptions is not an error" || return 1
    content=$(extract_content "$resp")
    assert_json_field "$content" '(.subscriptions.data | length)' "0" \
        "list-queue-subscriptions [$broker]: real queue with zero subscriptions returns an empty list" || return 1

    call_tool_ok "delete-queue" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:"default",queueName:$n}')" \
        "queue-subscription: delete-queue [$broker]" || return 1
}

# list-queue-subscriptions on a queue that was never created must fail, not
# report an empty list — the exact confusion SOL-153868 exists to prevent.
# Wording of the broker's own error (verified live: "Could not find match for
# queue <name>") is deliberately not asserted here — that string is this
# suite's own broker's prose, not part of the tool's contract, and pinning it
# would make this test brittle to a SEMP version bump for no behavioral
# benefit. Only the isError contract, which the tool does control, is pinned.
test_list_queue_subscriptions_nonexistent_queue() {
    local broker="$1"
    local name="e2e-config-queue-sub-missing-$broker"
    local resp

    resp=$(mcp_call_tool "list-queue-subscriptions" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:"default",queueName:$n}')") \
        || { log_fail "list-queue-subscriptions [$broker]: transport failure on nonexistent queue"; return 1; }
    assert_json_field "$resp" ".result.isError" "true" \
        "list-queue-subscriptions [$broker]: nonexistent queue reports isError=true, not an empty list" || return 1
}

# ── Cross-cutting ────────────────────────────────────────────────────────────

# A create/delete on broker-a's fixture must leave broker-b untouched. Uses one
# shared queue name created only on broker-a.
test_cross_broker_isolation() {
    local name="e2e-config-iso"
    call_tool_ok "create-queue" \
        "$(jq -nc --arg n "$name" '{broker:"broker-a",msgVpnName:"default",queueName:$n,queueConfig:{accessType:"non-exclusive"}}')" \
        "isolation: create-queue [broker-a]" || return 1
    assert_listed "broker-a" "list-queues" "$name" "true" || return 1
    assert_listed "broker-b" "list-queues" "$name" "false" || return 1
    call_tool_ok "delete-queue" \
        "$(jq -nc --arg n "$name" '{broker:"broker-a",msgVpnName:"default",queueName:$n}')" \
        "isolation: delete-queue [broker-a]" || return 1
}

# RDP cross-broker isolation: an identically-named RDP exists on BOTH brokers;
# create+delete on broker-a must leave broker-b's copy untouched (the ticket's
# isolation DoD). Kept as its own test so a failure names the RDP path and never
# hides behind the queue isolation above.
test_rdp_cross_broker_isolation() {
    local rdp="e2e-config-rdp-iso"
    call_tool_ok "create-rdp" \
        "$(jq -nc --arg n "$rdp" '{broker:"broker-a",msgVpnName:"default",restDeliveryPointName:$n}')" \
        "isolation: create-rdp [broker-a]" || return 1
    call_tool_ok "create-rdp" \
        "$(jq -nc --arg n "$rdp" '{broker:"broker-b",msgVpnName:"default",restDeliveryPointName:$n}')" \
        "isolation: create-rdp [broker-b]" || return 1
    assert_listed "broker-a" "list-rdps" "$rdp" "true" || return 1
    assert_listed "broker-b" "list-rdps" "$rdp" "true" || return 1

    call_tool_ok "delete-rdp" \
        "$(jq -nc --arg n "$rdp" '{broker:"broker-a",msgVpnName:"default",restDeliveryPointName:$n}')" \
        "isolation: delete-rdp [broker-a]" || return 1
    assert_listed "broker-a" "list-rdps" "$rdp" "false" || return 1
    # broker-b's identically-named RDP must survive broker-a's delete.
    assert_listed "broker-b" "list-rdps" "$rdp" "true"  || return 1

    # Clean up broker-b's copy (the sweep also covers it).
    call_tool_ok "delete-rdp" \
        "$(jq -nc --arg n "$rdp" '{broker:"broker-b",msgVpnName:"default",restDeliveryPointName:$n}')" \
        "isolation: delete-rdp [broker-b] cleanup" || return 1
}

# tools/list advertises every config tool with its declared annotations. All are
# write tools (readOnlyHint=false); create-* are non-destructive, update-*/
# delete-* are destructive. readOnlyHint/destructiveHint are omitempty on the
# wire, so `// false` normalizes an absent hint to false.
test_annotations() {
    local sid resp t
    sid=$(mcp_initialize) || return 1
    resp=$(mcp_request "$sid" '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}') || return 1

    for t in create-message-vpn create-queue create-topic-endpoint create-rdp \
             create-queue-subscription \
             update-message-vpn delete-message-vpn update-queue delete-queue \
             update-topic-endpoint delete-topic-endpoint update-rdp delete-rdp \
             delete-queue-subscription; do
        assert_json_field "$resp" "([.result.tools[].name] | index(\"$t\")) != null" "true" \
            "annotations: $t advertised in tools/list" || return 1
        assert_json_field "$resp" "(.result.tools[] | select(.name==\"$t\") | .annotations.readOnlyHint) // false" "false" \
            "annotations: $t readOnlyHint=false" || return 1
    done

    for t in create-message-vpn create-queue create-topic-endpoint create-rdp \
             create-queue-subscription; do
        assert_json_field "$resp" "(.result.tools[] | select(.name==\"$t\") | .annotations.destructiveHint) // false" "false" \
            "annotations: $t destructiveHint=false" || return 1
    done
    for t in update-message-vpn delete-message-vpn update-queue delete-queue \
             update-topic-endpoint delete-topic-endpoint update-rdp delete-rdp \
             delete-queue-subscription; do
        assert_json_field "$resp" "(.result.tools[] | select(.name==\"$t\") | .annotations.destructiveHint) // false" "true" \
            "annotations: $t destructiveHint=true" || return 1
    done
}

# Creating an object that already exists surfaces the broker's translated error
# through the wire: isError=true with the parsed SEMP HTTP status, not a raw
# transport failure.
# SOL-153341, AC1/AC5/AC6: a duplicate create is a non-failure, distinguishable
# from a real error — isError=false, with an outcome/changed pair an agent can
# read without reconciling against the broker. Before SOL-153341 this asserted
# the opposite (isError=true, HTTP 400) — that was the exact case the ticket
# reclassifies, so this test's own assertions had to flip, not just its fixture.
test_error_translation() {
    local broker="broker-a"
    local name="e2e-config-vpn-$broker"
    local resp text

    call_tool_ok "create-message-vpn" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:$n,msgVpnConfig:{enabled:false}}')" \
        "error-xlate: initial create [$broker]" || return 1

    resp=$(mcp_call_tool "create-message-vpn" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:$n,msgVpnConfig:{enabled:false}}')") \
        || { log_fail "error-xlate: transport failure on duplicate create"; return 1; }
    assert_json_field "$resp" ".result.isError // false" "false" \
        "error-xlate: duplicate create returns isError=false (non-failure, SOL-153341)" || return 1
    assert_json_field "$resp" ".result.structuredContent.outcome" "exists_unchanged" \
        "error-xlate: duplicate create reports outcome=exists_unchanged" || return 1
    assert_json_field "$resp" ".result.structuredContent.changed" "false" \
        "error-xlate: duplicate create reports changed=false" || return 1
    text=$(jq -r '.result.content[0].text // ""' <<<"$resp" | tr '[:upper:]' '[:lower:]')
    assert_contains "$text" "exist" \
        "error-xlate: message reports the object already exists" || return 1

    call_tool_ok "delete-message-vpn" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:$n}')" \
        "error-xlate: cleanup delete [$broker]" || return 1
}

# SOL-153341, AC2: deleting an object that does not exist is a non-failure too.
test_delete_nonexistent_is_noop() {
    local broker="broker-a"
    local name="e2e-config-vpn-missing-$broker"
    local resp

    resp=$(mcp_call_tool "delete-message-vpn" \
        "$(jq -nc --arg b "$broker" --arg n "$name" '{broker:$b,msgVpnName:$n}')") \
        || { log_fail "error-xlate: transport failure on delete of nonexistent VPN"; return 1; }
    assert_json_field "$resp" ".result.isError // false" "false" \
        "error-xlate: deleting a nonexistent VPN returns isError=false (non-failure, SOL-153341)" || return 1
    assert_json_field "$resp" ".result.structuredContent.outcome" "already_absent" \
        "error-xlate: delete of nonexistent VPN reports outcome=already_absent" || return 1
}

# SOL-153341, AC3: NOT_FOUND on a create still IS a real error (isError=true)
# when what's missing is a parent, not the object being created — and the
# message names the parent's type in plain language rather than surfacing
# "Cannot enter <mode>: not found" CLI jargon. Also proves the non-failure
# reclassification above didn't accidentally swallow a genuine error case.
# Uses create-rdp against a nonexistent VPN specifically: it's live-confirmed
# to use the "bare" message shape (no instance name — "Cannot enter
# message-vpn mode: not found."), the harder of the two shapes to translate
# correctly, and a different one than test_error_translation's create-message-vpn
# case exercises above.
test_create_missing_parent_is_real_error() {
    local vpn="e2e-config-missing-parent-vpn"
    local resp text

    resp=$(mcp_call_tool "create-rdp" \
        "$(jq -nc --arg v "$vpn" '{broker:"broker-a",msgVpnName:$v,restDeliveryPointName:"whatever"}')") \
        || { log_fail "error-xlate: transport failure on create against a missing parent VPN"; return 1; }
    assert_json_field "$resp" ".result.isError" "true" \
        "error-xlate: create against a missing parent VPN returns isError=true (a real error, not a noop)" || return 1
    text=$(jq -r '.result.content[0].text // ""' <<<"$resp" | tr '[:upper:]' '[:lower:]')
    assert_contains "$text" "message vpn" \
        "error-xlate: message names the missing parent's type" || return 1
    if [[ "$text" == *"cannot enter"* ]]; then
        log_fail "error-xlate: message still shows raw CLI-mode jargon: $text"
        return 1
    fi
}

# ── Per-broker wrappers (run_test calls no-arg functions) ─────────────────────
test_vpn_roundtrip_a()   { test_vpn_roundtrip broker-a; }
test_vpn_roundtrip_b()   { test_vpn_roundtrip broker-b; }
test_queue_roundtrip_a() { test_queue_roundtrip broker-a; }
test_queue_roundtrip_b() { test_queue_roundtrip broker-b; }
test_te_roundtrip_a()    { test_te_roundtrip broker-a; }
test_te_roundtrip_b()    { test_te_roundtrip broker-b; }
test_rdp_roundtrip_a()   { test_rdp_roundtrip broker-a; }
test_rdp_roundtrip_b()   { test_rdp_roundtrip broker-b; }
test_queue_subscription_roundtrip_a() { test_queue_subscription_roundtrip broker-a; }
test_queue_subscription_roundtrip_b() { test_queue_subscription_roundtrip broker-b; }
test_list_queue_subscriptions_nonexistent_queue_a() { test_list_queue_subscriptions_nonexistent_queue broker-a; }
test_list_queue_subscriptions_nonexistent_queue_b() { test_list_queue_subscriptions_nonexistent_queue broker-b; }

# ── Main ─────────────────────────────────────────────────────────────────────

log_info "=== Config-tool (management) E2E tests ==="

run_test "VPN round-trip (broker-a)"            test_vpn_roundtrip_a
run_test "VPN round-trip (broker-b)"            test_vpn_roundtrip_b
run_test "Queue round-trip (broker-a)"          test_queue_roundtrip_a
run_test "Queue round-trip (broker-b)"          test_queue_roundtrip_b
run_test "Topic-endpoint round-trip (broker-a)" test_te_roundtrip_a
run_test "Topic-endpoint round-trip (broker-b)" test_te_roundtrip_b
run_test "RDP round-trip (broker-a)"            test_rdp_roundtrip_a
run_test "RDP round-trip (broker-b)"            test_rdp_roundtrip_b
run_test "Queue-subscription round-trip (broker-a)" test_queue_subscription_roundtrip_a
run_test "Queue-subscription round-trip (broker-b)" test_queue_subscription_roundtrip_b
run_test "list-queue-subscriptions on nonexistent queue (broker-a)" test_list_queue_subscriptions_nonexistent_queue_a
run_test "list-queue-subscriptions on nonexistent queue (broker-b)" test_list_queue_subscriptions_nonexistent_queue_b
run_test "Cross-broker isolation (queue)"       test_cross_broker_isolation
run_test "Cross-broker isolation (RDP)"         test_rdp_cross_broker_isolation
run_test "Annotations (tools/list)"             test_annotations
run_test "Error translation (duplicate create)" test_error_translation
run_test "Error translation (delete nonexistent is a noop)" test_delete_nonexistent_is_noop
run_test "Error translation (missing parent is a real error)" test_create_missing_parent_is_real_error

print_summary "Config-tool tests"
