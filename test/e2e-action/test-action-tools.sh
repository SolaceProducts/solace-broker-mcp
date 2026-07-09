#!/usr/bin/env bash
# Action-tool functional tests, driven over the MCP JSON-RPC wire (no LLM). Each
# tool mutates broker state that only exists with real messaging, so every test
# sets up a disposable fixture (spooled queue / connected client) via the
# broker-driver, invokes the action, and verifies the broker state actually
# changed — on both brokers — plus cross-broker isolation and annotations.
#
# Fixture model: per-test ownership. Each test creates its own e2e-action-*
# fixture, acts, asserts, and tears it down. A suite-level sweep runs on entry
# (pre-clean) and on exit (safety net) so a mid-run failure never leaks a queue
# or a bound broker-driver client.
#
# Assertion fields are lab-verified (see the suite README): clearStats resets
# spooledMsgCount / dataRxMsgCount to 0; deleteMsgs drains the LIVE depth
# (liveDepth.currentMsgCount) while cumulative spooledMsgCount stays; disconnect
# yields a new broker-assigned clientId (the SDK auto-reconnects, so "absent"
# would flake). Post-action reads POLL, never read once — the private monitor
# endpoint can lag a state change.

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/helpers.sh"

# Sweep on exit (standalone runs / mid-run failure) and pre-clean on entry.
trap sweep_action_fixtures EXIT
sweep_action_fixtures

# ── Per-test helpers ─────────────────────────────────────────────────────────

broker_url_for()    { case "$1" in broker-a) echo "$BROKER_A_URL" ;; broker-b) echo "$BROKER_B_URL" ;; esac; }
semp_config_for()   { case "$1" in broker-a) echo "$BROKER_A_SEMP_CONFIG" ;; broker-b) echo "$BROKER_B_SEMP_CONFIG" ;; esac; }
broker_letter_for() { case "$1" in broker-a) echo a ;; broker-b) echo b ;; esac; }

# Call an action tool and assert it succeeded: no JSON-RPC error and the tool
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

# Echo a queue's authoritative live depth (liveDepth.currentMsgCount, from
# get-queue-metrics — SEMPv1 num-messages-spooled under the hood). Empty on error.
queue_current_depth() {
    local broker="$1" queue="$2" resp content
    resp=$(mcp_call_tool "get-queue-metrics" \
        "$(jq -nc --arg b "$broker" --arg q "$queue" '{broker:$b,msgVpnName:"default",queueName:$q}')") || return 1
    content=$(extract_content "$resp")
    jq -r '.liveDepth.currentMsgCount // empty' <<<"$content"
}

# Poll get-queue-metrics until a queue's live depth equals $expected, or timeout.
#   $1 broker   $2 queue   $3 expected   $4 max_attempts (default 15)
poll_queue_depth() {
    local broker="$1" queue="$2" expected="$3" max_attempts="${4:-15}"
    local attempt=0 cur=""
    while [ $attempt -lt "$max_attempts" ]; do
        cur=$(queue_current_depth "$broker" "$queue") || cur=""
        [ "$cur" = "$expected" ] && return 0
        sleep 1; attempt=$((attempt + 1))
    done
    log_fail "poll_queue_depth [$broker/$queue]: expected currentMsgCount=$expected, last saw '${cur:-<none>}'"
    return 1
}

# Echo a client's broker-assigned per-connection clientId. Empty if absent.
read_client_id() {
    local broker_url="$1" client="$2"
    semp_monitor_get "$broker_url" "msgVpns/$BROKER_VPN/clients/$client" 2>/dev/null \
        | jq -r '.data.clientId // empty' 2>/dev/null || true
}

# Poll until a client's session has been terminated after a disconnect: either a
# NEW clientId (SDK reconnected under the same name) or transient absence. Both
# prove the original session ($baseline) ended. Times out → non-zero.
#   $1 broker_url   $2 client   $3 baseline_client_id   $4 max_attempts (default 20)
poll_client_reconnected() {
    local broker_url="$1" client="$2" baseline="$3" max_attempts="${4:-20}"
    local attempt=0 cur
    while [ $attempt -lt "$max_attempts" ]; do
        cur=$(read_client_id "$broker_url" "$client")
        if [ -z "$cur" ]; then
            return 0                      # was present before disconnect, now gone
        elif [ "$cur" != "$baseline" ]; then
            return 0                      # reconnected with a fresh clientId
        fi
        sleep 1; attempt=$((attempt + 1))
    done
    return 1
}

# ── 1. clear-queue-stats (non-destructive) ───────────────────────────────────
# Spool N with no consumer so spooledMsgCount=N, then clearStats. Lab-verified:
# clearStats resets spooledMsgCount (and spooledByteCount) to 0; the messages
# themselves physically remain (msgSpoolUsage is unchanged).
test_clear_queue_stats() {
    local broker="$1" letter sc burl q="e2e-action-clearstats-queue-$1"
    letter=$(broker_letter_for "$broker"); sc=$(semp_config_for "$broker"); burl=$(broker_url_for "$broker")

    create_spooled_queue "$sc" "$letter" "$q" "$ACTION_TOPIC_CLEARSTATS_QUEUE" 20 \
        || { log_fail "clear-queue-stats [$broker]: fixture spool failed"; return 1; }
    verify_monitor_object "$burl" "$broker" "msgVpns/$BROKER_VPN/queues/$q" 15 '.data.spooledMsgCount == 20' \
        || { log_fail "clear-queue-stats [$broker]: fixture did not spool 20"; semp_delete "$sc" "msgVpns/$BROKER_VPN/queues/$q"; return 1; }

    call_tool_ok "clear-queue-stats" \
        "$(jq -nc --arg b "$broker" --arg q "$q" '{broker:$b,msgVpnName:"default",queueName:$q}')" \
        "clear-queue-stats [$broker]" \
        || { semp_delete "$sc" "msgVpns/$BROKER_VPN/queues/$q"; return 1; }

    verify_monitor_object "$burl" "$broker" "msgVpns/$BROKER_VPN/queues/$q" 15 '.data.spooledMsgCount == 0' \
        || { log_fail "clear-queue-stats [$broker]: spooledMsgCount not reset to 0"; semp_delete "$sc" "msgVpns/$BROKER_VPN/queues/$q"; return 1; }

    semp_delete "$sc" "msgVpns/$BROKER_VPN/queues/$q"
}

# ── 2. clear-client-stats (non-destructive) ──────────────────────────────────
# Connect a receiving client, deliver direct traffic to it so its data counter
# rises, then clearStats. Note the SEMP client counters are BROKER-centric: a
# client that RECEIVES shows dataTxMsgCount rising (broker transmits TO client) —
# dataRxMsgCount is client→broker (a pure receiver never bumps it). Lab-verified:
# clearStats resets the data-plane counters to 0; the aggregate rxMsgCount/
# txMsgCount never fully zero (control-plane chatter), so we assert the data-plane
# counter (dataTxMsgCount).
test_clear_client_stats() {
    local broker="$1" letter sc burl
    local c="e2e-action-clearstats-client-$1" q="e2e-action-clearstats-cq-$1"
    letter=$(broker_letter_for "$broker"); sc=$(semp_config_for "$broker"); burl=$(broker_url_for "$broker")

    spawn_action_client "$sc" "$letter" "$broker" "$burl" "$c" "$q" "$ACTION_TOPIC_CLEARSTATS_CLIENT" "act-cs" \
        || { log_fail "clear-client-stats [$broker]: client fixture failed"; return 1; }

    # Drive data-plane traffic: publish to the client's direct-subscription topic;
    # the broker delivers each message to the client → dataTxMsgCount grows.
    "$BIN_DIR/broker-driver" publish-batch --broker="$letter" --vpn="$BROKER_VPN" \
        --topic="$ACTION_TOPIC_CLEARSTATS_CLIENT" --count=20 --size=256 --message-type=persistent \
        >"$BIN_DIR/publish-batch-$c.log" 2>&1 || true

    verify_monitor_object "$burl" "$broker" "msgVpns/$BROKER_VPN/clients/$c" 15 '.data.dataTxMsgCount > 0' \
        || { log_fail "clear-client-stats [$broker]: dataTxMsgCount did not rise"; stop_broker_drivers; semp_delete "$sc" "msgVpns/$BROKER_VPN/queues/$q"; return 1; }
    sleep 2   # let all deliveries settle so no straggler bumps the counter after clearStats

    call_tool_ok "clear-client-stats" \
        "$(jq -nc --arg b "$broker" --arg c "$c" '{broker:$b,msgVpnName:"default",clientName:$c}')" \
        "clear-client-stats [$broker]" \
        || { stop_broker_drivers; semp_delete "$sc" "msgVpns/$BROKER_VPN/queues/$q"; return 1; }

    verify_monitor_object "$burl" "$broker" "msgVpns/$BROKER_VPN/clients/$c" 15 '.data.dataTxMsgCount == 0' \
        || { log_fail "clear-client-stats [$broker]: dataTxMsgCount not reset to 0"; stop_broker_drivers; semp_delete "$sc" "msgVpns/$BROKER_VPN/queues/$q"; return 1; }

    stop_broker_drivers
    semp_delete "$sc" "msgVpns/$BROKER_VPN/queues/$q"
}

# ── 3. delete-queue-messages (destructive) ───────────────────────────────────
# Spool N with no consumer, then deleteMsgs. Lab-verified: deleteMsgs drains the
# LIVE depth (liveDepth.currentMsgCount → 0) while the CUMULATIVE spooledMsgCount
# stays at N (SOL-150260). We assert the live depth via get-queue-metrics, and
# additionally confirm spooledMsgCount is unchanged to document the distinction.
test_delete_queue_messages() {
    local broker="$1" letter sc burl q="e2e-action-deletemsgs-$1"
    letter=$(broker_letter_for "$broker"); sc=$(semp_config_for "$broker"); burl=$(broker_url_for "$broker")

    create_spooled_queue "$sc" "$letter" "$q" "$ACTION_TOPIC_DELETEMSGS" 20 \
        || { log_fail "delete-queue-messages [$broker]: fixture spool failed"; return 1; }
    poll_queue_depth "$broker" "$q" "20" \
        || { semp_delete "$sc" "msgVpns/$BROKER_VPN/queues/$q"; return 1; }

    call_tool_ok "delete-queue-messages" \
        "$(jq -nc --arg b "$broker" --arg q "$q" '{broker:$b,msgVpnName:"default",queueName:$q}')" \
        "delete-queue-messages [$broker]" \
        || { semp_delete "$sc" "msgVpns/$BROKER_VPN/queues/$q"; return 1; }

    poll_queue_depth "$broker" "$q" "0" \
        || { log_fail "delete-queue-messages [$broker]: live depth not drained to 0"; semp_delete "$sc" "msgVpns/$BROKER_VPN/queues/$q"; return 1; }

    # Document the SOL-150260 distinction: cumulative spooledMsgCount is NOT reset
    # by a delete (it only counts lifetime arrivals), so it must still read 20.
    verify_monitor_object "$burl" "$broker" "msgVpns/$BROKER_VPN/queues/$q" 5 '.data.spooledMsgCount == 20' \
        || log_warn "delete-queue-messages [$broker]: spooledMsgCount not 20 post-delete (cumulative-counter assumption drifted?)"

    semp_delete "$sc" "msgVpns/$BROKER_VPN/queues/$q"
}

# ── 4. disconnect-client (destructive) ───────────────────────────────────────
# Connect a named client, capture its clientId, disconnect. Lab-verified: the SDK
# auto-reconnects under the same name within ~1s, so "absent from list-clients"
# flakes; the reliable signal is a NEW broker-assigned clientId (or transient
# absence). Both prove the original session was terminated.
test_disconnect_client() {
    local broker="$1" letter sc burl before
    local c="e2e-action-disc-$1" q="e2e-action-disc-q-$1"
    letter=$(broker_letter_for "$broker"); sc=$(semp_config_for "$broker"); burl=$(broker_url_for "$broker")

    spawn_action_client "$sc" "$letter" "$broker" "$burl" "$c" "$q" "$ACTION_TOPIC_DISC" "act-disc" \
        || { log_fail "disconnect-client [$broker]: client fixture failed"; return 1; }
    before=$(read_client_id "$burl" "$c")
    [ -n "$before" ] || { log_fail "disconnect-client [$broker]: could not read clientId before"; stop_broker_drivers; semp_delete "$sc" "msgVpns/$BROKER_VPN/queues/$q"; return 1; }

    call_tool_ok "disconnect-client" \
        "$(jq -nc --arg b "$broker" --arg c "$c" '{broker:$b,msgVpnName:"default",clientName:$c}')" \
        "disconnect-client [$broker]" \
        || { stop_broker_drivers; semp_delete "$sc" "msgVpns/$BROKER_VPN/queues/$q"; return 1; }

    poll_client_reconnected "$burl" "$c" "$before" \
        || { log_fail "disconnect-client [$broker]: clientId never changed from $before (session not terminated)"; stop_broker_drivers; semp_delete "$sc" "msgVpns/$BROKER_VPN/queues/$q"; return 1; }

    stop_broker_drivers
    semp_delete "$sc" "msgVpns/$BROKER_VPN/queues/$q"
}

# ── 5. read-after-write consistency (FR-3, reframed) ─────────────────────────
# The ticket's FR-3 asks for a "cache-invalidation" test, but the server has NO
# response cache — get-queue-metrics reads the broker live on every call (grep
# confirmed; see README). So this is a read-after-write consistency check: read
# the depth through the monitoring tool (N), delete via the action tool, read
# again through the SAME tool, and confirm it reflects 0 — i.e. the write's
# effect is visible through the monitoring path, never a stale N.
test_read_after_write() {
    local broker="$1" letter sc q="e2e-action-deletemsgs-$1"
    letter=$(broker_letter_for "$broker"); sc=$(semp_config_for "$broker")

    create_spooled_queue "$sc" "$letter" "$q" "$ACTION_TOPIC_DELETEMSGS" 20 \
        || { log_fail "read-after-write [$broker]: fixture spool failed"; return 1; }
    # First read through the monitoring tool populates whatever read path exists.
    poll_queue_depth "$broker" "$q" "20" \
        || { semp_delete "$sc" "msgVpns/$BROKER_VPN/queues/$q"; return 1; }

    call_tool_ok "delete-queue-messages" \
        "$(jq -nc --arg b "$broker" --arg q "$q" '{broker:$b,msgVpnName:"default",queueName:$q}')" \
        "read-after-write [$broker]: delete" \
        || { semp_delete "$sc" "msgVpns/$BROKER_VPN/queues/$q"; return 1; }

    # Second read through the SAME tool must reflect 0, not a stale 20.
    poll_queue_depth "$broker" "$q" "0" \
        || { log_fail "read-after-write [$broker]: get-queue-metrics served a stale depth after delete"; semp_delete "$sc" "msgVpns/$BROKER_VPN/queues/$q"; return 1; }

    semp_delete "$sc" "msgVpns/$BROKER_VPN/queues/$q"
}

# ── 6. Cross-broker isolation — deleteMsgs ───────────────────────────────────
# Identically-named spooled queue on BOTH brokers; deleteMsgs on broker-a must
# leave broker-b's queue intact (catches broker-routing bugs in the action path).
test_deletemsgs_cross_broker_isolation() {
    local q="e2e-action-deletemsgs-iso"
    create_spooled_queue "$BROKER_A_SEMP_CONFIG" a "$q" "$ACTION_TOPIC_DELETEMSGS" 20 \
        || { log_fail "isolation(deleteMsgs): spool on broker-a failed"; return 1; }
    create_spooled_queue "$BROKER_B_SEMP_CONFIG" b "$q" "$ACTION_TOPIC_DELETEMSGS" 20 \
        || { log_fail "isolation(deleteMsgs): spool on broker-b failed"; semp_delete "$BROKER_A_SEMP_CONFIG" "msgVpns/$BROKER_VPN/queues/$q"; return 1; }
    poll_queue_depth "broker-a" "$q" "20" && poll_queue_depth "broker-b" "$q" "20" \
        || { semp_delete "$BROKER_A_SEMP_CONFIG" "msgVpns/$BROKER_VPN/queues/$q"; semp_delete "$BROKER_B_SEMP_CONFIG" "msgVpns/$BROKER_VPN/queues/$q"; return 1; }

    call_tool_ok "delete-queue-messages" \
        "$(jq -nc --arg q "$q" '{broker:"broker-a",msgVpnName:"default",queueName:$q}')" \
        "isolation(deleteMsgs): delete on broker-a" \
        || { semp_delete "$BROKER_A_SEMP_CONFIG" "msgVpns/$BROKER_VPN/queues/$q"; semp_delete "$BROKER_B_SEMP_CONFIG" "msgVpns/$BROKER_VPN/queues/$q"; return 1; }

    local rc=0
    poll_queue_depth "broker-a" "$q" "0" || { log_fail "isolation(deleteMsgs): broker-a not drained"; rc=1; }
    # broker-b untouched — its depth must still be 20.
    local b_depth; b_depth=$(queue_current_depth "broker-b" "$q")
    [ "$b_depth" = "20" ] || { log_fail "isolation(deleteMsgs): broker-b depth changed to '$b_depth' (expected 20)"; rc=1; }

    semp_delete "$BROKER_A_SEMP_CONFIG" "msgVpns/$BROKER_VPN/queues/$q"
    semp_delete "$BROKER_B_SEMP_CONFIG" "msgVpns/$BROKER_VPN/queues/$q"
    return $rc
}

# ── 7. Cross-broker isolation — disconnect ───────────────────────────────────
# Identically-named connected client on BOTH brokers; disconnect on broker-a must
# leave broker-b's client's session untouched (same clientId afterward).
test_disconnect_cross_broker_isolation() {
    local c="e2e-action-disc-iso" q="e2e-action-disc-q-iso" ca cb rc=0
    spawn_action_client "$BROKER_A_SEMP_CONFIG" a "broker-a" "$BROKER_A_URL" "$c" "$q" "$ACTION_TOPIC_DISC" "act-disc-iso" \
        || { log_fail "isolation(disconnect): client on broker-a failed"; return 1; }
    spawn_action_client "$BROKER_B_SEMP_CONFIG" b "broker-b" "$BROKER_B_URL" "$c" "$q" "$ACTION_TOPIC_DISC" "act-disc-iso" \
        || { log_fail "isolation(disconnect): client on broker-b failed"; stop_broker_drivers; semp_delete "$BROKER_A_SEMP_CONFIG" "msgVpns/$BROKER_VPN/queues/$q"; semp_delete "$BROKER_B_SEMP_CONFIG" "msgVpns/$BROKER_VPN/queues/$q"; return 1; }

    ca=$(read_client_id "$BROKER_A_URL" "$c"); cb=$(read_client_id "$BROKER_B_URL" "$c")
    if [ -z "$ca" ] || [ -z "$cb" ]; then
        log_fail "isolation(disconnect): could not read both clientIds (a='$ca' b='$cb')"; rc=1
    else
        call_tool_ok "disconnect-client" \
            "$(jq -nc --arg c "$c" '{broker:"broker-a",msgVpnName:"default",clientName:$c}')" \
            "isolation(disconnect): disconnect on broker-a" \
            && {
                poll_client_reconnected "$BROKER_A_URL" "$c" "$ca" || { log_fail "isolation(disconnect): broker-a session not terminated"; rc=1; }
                # broker-b's client must be untouched — same session (clientId).
                local cb_after; cb_after=$(read_client_id "$BROKER_B_URL" "$c")
                [ "$cb_after" = "$cb" ] || { log_fail "isolation(disconnect): broker-b clientId changed ($cb -> '$cb_after')"; rc=1; }
            } || rc=1
    fi

    stop_broker_drivers
    semp_delete "$BROKER_A_SEMP_CONFIG" "msgVpns/$BROKER_VPN/queues/$q"
    semp_delete "$BROKER_B_SEMP_CONFIG" "msgVpns/$BROKER_VPN/queues/$q"
    return $rc
}

# ── 8. Annotations (tools/list) ──────────────────────────────────────────────
# All four action tools are write tools (readOnlyHint=false). delete-queue-
# messages and disconnect-client are destructive; the clear-*-stats pair is not.
# There is no requiresConfirmation annotation (confirmation lives in the tool
# description prose). readOnlyHint/destructiveHint are omitempty on the wire, so
# `// false` normalizes an absent hint to false.
test_annotations() {
    local sid resp t
    sid=$(mcp_initialize) || return 1
    resp=$(mcp_request "$sid" '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}') || return 1

    for t in delete-queue-messages clear-queue-stats disconnect-client clear-client-stats; do
        assert_json_field "$resp" "([.result.tools[].name] | index(\"$t\")) != null" "true" \
            "annotations: $t advertised in tools/list" || return 1
        assert_json_field "$resp" "(.result.tools[] | select(.name==\"$t\") | .annotations.readOnlyHint) // false" "false" \
            "annotations: $t readOnlyHint=false" || return 1
    done
    for t in delete-queue-messages disconnect-client; do
        assert_json_field "$resp" "(.result.tools[] | select(.name==\"$t\") | .annotations.destructiveHint) // false" "true" \
            "annotations: $t destructiveHint=true" || return 1
    done
    for t in clear-queue-stats clear-client-stats; do
        assert_json_field "$resp" "(.result.tools[] | select(.name==\"$t\") | .annotations.destructiveHint) // false" "false" \
            "annotations: $t destructiveHint=false" || return 1
    done
}

# ── Per-broker wrappers (run_test calls no-arg functions) ─────────────────────
test_clear_queue_stats_a()    { test_clear_queue_stats broker-a; }
test_clear_queue_stats_b()    { test_clear_queue_stats broker-b; }
test_clear_client_stats_a()   { test_clear_client_stats broker-a; }
test_clear_client_stats_b()   { test_clear_client_stats broker-b; }
test_delete_queue_messages_a() { test_delete_queue_messages broker-a; }
test_delete_queue_messages_b() { test_delete_queue_messages broker-b; }
test_disconnect_client_a()    { test_disconnect_client broker-a; }
test_disconnect_client_b()    { test_disconnect_client broker-b; }
test_read_after_write_a()     { test_read_after_write broker-a; }
test_read_after_write_b()     { test_read_after_write broker-b; }

# ── Main ─────────────────────────────────────────────────────────────────────

log_info "=== Action-tool E2E tests ==="

run_test "clear-queue-stats (broker-a)"          test_clear_queue_stats_a
run_test "clear-queue-stats (broker-b)"          test_clear_queue_stats_b
run_test "clear-client-stats (broker-a)"         test_clear_client_stats_a
run_test "clear-client-stats (broker-b)"         test_clear_client_stats_b
run_test "delete-queue-messages (broker-a)"      test_delete_queue_messages_a
run_test "delete-queue-messages (broker-b)"      test_delete_queue_messages_b
run_test "disconnect-client (broker-a)"          test_disconnect_client_a
run_test "disconnect-client (broker-b)"          test_disconnect_client_b
run_test "read-after-write consistency (broker-a)" test_read_after_write_a
run_test "read-after-write consistency (broker-b)" test_read_after_write_b
run_test "cross-broker isolation (deleteMsgs)"   test_deletemsgs_cross_broker_isolation
run_test "cross-broker isolation (disconnect)"   test_disconnect_cross_broker_isolation
run_test "annotations (tools/list)"              test_annotations

print_summary "Action-tool tests"
