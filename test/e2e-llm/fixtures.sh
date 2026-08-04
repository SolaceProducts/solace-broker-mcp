#!/usr/bin/env bash
# LLM suite standing fixtures for Mode-2 write/action/config scenarios
# (SOL-150727). Split from setup-fixtures.sh so run-scenario.sh can source
# the same definitions and re-export refill_e2e_llm_action_queue via
# `export -f` — a scenario's setup.cmd runs in a `bash -c` child that
# only sees exported functions.
#
# Depends on the monitoring suite's helpers being sourced first: this file
# uses BROKER_A_SEMP_CONFIG / BROKER_B_SEMP_CONFIG, BROKER_A_URL /
# BROKER_B_URL, BIN_DIR, BROKER_VPN, semp_post, semp_delete,
# wait_for_pidfile, verify_monitor_object. Cross-suite sourcing of
# e2e-action/helpers.sh was ruled out because it resets SUITE_DIR/BIN_DIR
# via its own lib.sh source, which would break the monitoring suite's
# BIN_DIR pidfile paths mid-setup. The queue/client provisioning below
# intentionally mirrors e2e-action/helpers.sh's `create_spooled_queue`
# and `spawn_action_client` shapes so the fixture surface stays familiar.

# semp_curl (basic-auth via curl -K - to keep the password out of argv) lives
# in test/e2e-common/lib.sh and is inherited transitively via helpers.sh →
# lib.sh. See SOL-151860 for the consolidation.

# ── Fixture names ────────────────────────────────────────────────────────────
# All share the e2e-llm- prefix so monitoring/management sweeps never touch
# them. Broker-suffixed to keep two-broker parallelism explicit; run-scenario.sh
# aliases them to unsuffixed E2E_LLM_ACTION_QUEUE / E2E_LLM_KICK_TARGET
# based on $BROKER so scenarios can reference the unsuffixed names.
E2E_LLM_ACTION_QUEUE_A="e2e-llm-action-queue-broker-a"
E2E_LLM_ACTION_QUEUE_B="e2e-llm-action-queue-broker-b"
E2E_LLM_ACTION_TOPIC="e2e-llm/action/msgs"
E2E_LLM_ACTION_BURST=50
E2E_LLM_KICK_TARGET_A="e2e-llm-kick-target-a"
E2E_LLM_KICK_TARGET_B="e2e-llm-kick-target-b"
E2E_LLM_KICK_QUEUE_A="e2e-llm-kick-target-queue-broker-a"
E2E_LLM_KICK_QUEUE_B="e2e-llm-kick-target-queue-broker-b"
E2E_LLM_KICK_TOPIC="e2e-llm/kick/msgs"
# Standing topic endpoint for the B5-style delete-topic-endpoint scenario —
# no equivalent standing TE exists in the monitoring layer (unlike test-vpn /
# test-rdp), so the LLM suite owns it. Turn 2 "no" preserves it; the shell
# ground truth GETs it and asserts topicEndpointName echoes back.
E2E_LLM_STANDING_TE_A="e2e-llm-standing-te-broker-a"
E2E_LLM_STANDING_TE_B="e2e-llm-standing-te-broker-b"

# Create the standing spooled queue (no consumer, ingress+egress enabled)
# on one broker and subscribe it to E2E_LLM_ACTION_TOPIC. Idempotent —
# semp_post is tolerant of 400-duplicate.
_create_e2e_llm_action_queue_on() {
    local semp_config="$1" broker_letter="$2" queue="$3"
    log_info "Provisioning LLM action queue on broker-$broker_letter: $queue"
    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues" \
        "{\"queueName\":\"$queue\",\"accessType\":\"non-exclusive\",\"permission\":\"consume\",\"ingressEnabled\":true,\"egressEnabled\":true}" >/dev/null || true
    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues/$queue/subscriptions" \
        "{\"subscriptionTopic\":\"$E2E_LLM_ACTION_TOPIC\"}" >/dev/null || true
}

# One-shot publish of E2E_LLM_ACTION_BURST persistent messages to the
# action topic on the current $BROKER's SMF port. Exported so a scenario's
# setup.cmd / teardown.cmd can call it by name; those run in a `bash -c`
# child that inherits exported functions.
#
# BROKER, BIN_DIR, BROKER_VPN, and the E2E_LLM_ACTION_* constants must be
# exported by the parent (run-scenario.sh's `set -a; source helpers.sh;
# source fixtures.sh; set +a` block handles this).
refill_e2e_llm_action_queue() {
    local broker_letter
    case "${BROKER:-broker-a}" in
        broker-a) broker_letter=a ;;
        broker-b) broker_letter=b ;;
        *) echo "refill_e2e_llm_action_queue: unknown BROKER=$BROKER" >&2; return 1 ;;
    esac
    "$BIN_DIR/broker-driver" publish-batch \
        --broker="$broker_letter" --vpn="$BROKER_VPN" \
        --topic="$E2E_LLM_ACTION_TOPIC" \
        --count="$E2E_LLM_ACTION_BURST" \
        --size=256 --message-type=persistent \
        >"$BIN_DIR/publish-batch-llm-action-$broker_letter.log" 2>&1
}
export -f refill_e2e_llm_action_queue

# Delete a queue on the current $BROKER's SEMP config endpoint. Used by
# scenarios that create their own scenario-scoped queue (e.g. C1's
# e2e-llm-c1-queue) — per-scenario teardown keeps re-runs idempotent
# without waiting for the suite-level teardown-fixtures.sh. Uses env vars
# for creds so the scenario JSON stays credential-free (the runner
# echoes teardown.cmd into the log).
delete_queue_on_current_broker() {
    local queue="$1"
    if [ -z "$queue" ]; then
        echo "delete_queue_on_current_broker: missing queue name" >&2
        return 1
    fi
    # Capture HTTP status so a botched teardown (auth, wrong broker URL) is
    # visible in the log instead of silently leaving residue for the next
    # scenario. 404 is expected when the create failed before the object
    # existed — that's fine. Any other non-2xx is a warning worth surfacing.
    local code
    code=$(semp_curl -s -o /dev/null -w '%{http_code}' -X DELETE \
        "$SEMP_CONFIG/msgVpns/$BROKER_VPN/queues/$queue" 2>/dev/null || echo "000")
    case "$code" in
        2*|404) ;;
        *) echo "delete_queue_on_current_broker: DELETE $queue returned HTTP $code (queue may leak)" >&2 ;;
    esac
}
export -f delete_queue_on_current_broker

# Poll the three F7/lowprio discard counters on the current $BROKER_URL
# until each is non-zero, up to a wall-clock timeout. Bridges the boundary
# between the monitoring suite (which owns the fixture identities — queue
# names and SEMP field paths) and the read-list-queue-discards scenario,
# whose topOffenderQueues[] assertion otherwise races the fixture warm-up.
#
# Reads F7_SPOOL_QUEUE / F7_TTL_QUEUE / F_LOWPRIO_QUEUE and the matching
# *_DISCARD_JQ constants from monitoring/helpers.sh, so a rename there
# propagates here.
#
# Args: [timeout_s]   wall-clock ceiling, default 30
# Env:  BROKER_URL, BROKER_VPN, semp_curl (all exported by run-scenario.sh)
# Exit: 0 on all-non-zero; 1 on timeout with a message naming which counters
#       are still zero and the BROKER_URL for unreachable-broker diagnosis.
wait_for_discard_fixtures() {
    local timeout_s="${1:-30}"
    local deadline=$((SECONDS + timeout_s))
    local spool=0 ttl=0 lowprio=0

    # nested so it does not leak into the surrounding namespace when exported.
    _wfd_nonzero() {
        local queue="$1" jq_expr="$2" v
        v=$(semp_curl --connect-timeout 3 --max-time 5 -sf \
            "$BROKER_URL/SEMP/v2/__private_monitor__/msgVpns/$BROKER_VPN/queues/$queue" 2>/dev/null \
            | jq -r "$jq_expr" 2>/dev/null)
        [ -n "$v" ] && [ "$v" != "null" ] && [ "$v" -gt 0 ] 2>/dev/null
    }

    while [ "$SECONDS" -lt "$deadline" ]; do
        _wfd_nonzero "$F7_SPOOL_QUEUE"   "$F7_SPOOL_DISCARD_JQ"   && spool=1
        _wfd_nonzero "$F7_TTL_QUEUE"     "$F7_TTL_DISCARD_JQ"     && ttl=1
        _wfd_nonzero "$F_LOWPRIO_QUEUE"  "$F_LOWPRIO_DISCARD_JQ"  && lowprio=1
        [ "$spool" = 1 ] && [ "$ttl" = 1 ] && [ "$lowprio" = 1 ] && return 0
        sleep 1
    done
    echo "wait_for_discard_fixtures: F7 discard counters did not accumulate within ${timeout_s}s wall-clock (spool=$spool ttl=$ttl lowprio=$lowprio; 1=non-zero, 0=still-zero-or-unreachable) — verify broker reachable at $BROKER_URL and setup-fixtures.sh ran" >&2
    return 1
}
export -f wait_for_discard_fixtures

# Long-lived connected-client bound to a dedicated queue on one broker.
# Modelled on e2e-action/helpers.sh:spawn_action_client but uses the
# monitoring suite's BIN_DIR so the driver co-locates with F3/F4/F5 and
# stop_broker_drivers (called by cleanup_fixtures) reaps it via the
# broker-driver-*.pid glob.
_spawn_llm_kick_client_on() {
    local semp_config="$1" broker_letter="$2" broker_url="$3"
    local client_name="$4" queue="$5"
    local pidfile="$BIN_DIR/broker-driver-llmkick-$broker_letter.pid"
    local logfile="$BIN_DIR/broker-driver-llmkick-$broker_letter.log"
    log_info "Provisioning LLM kick-target on broker-$broker_letter: $client_name → $queue"
    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues" \
        "{\"queueName\":\"$queue\",\"accessType\":\"non-exclusive\",\"permission\":\"consume\",\"ingressEnabled\":true,\"egressEnabled\":true}" >/dev/null || true

    nohup ${_SESSION_WRAP:+$_SESSION_WRAP} "$BIN_DIR/broker-driver" connected-client \
        --broker="$broker_letter" \
        --vpn="$BROKER_VPN" \
        --client-name="$client_name" \
        --queue="$queue" \
        --subscriptions="$E2E_LLM_KICK_TOPIC" \
        --pidfile="$pidfile" \
        >"$logfile" 2>&1 &
    wait_for_pidfile "$pidfile" "broker-$broker_letter" "$logfile" "broker-driver llmkick" || return 1
    verify_monitor_object "$broker_url" "broker-$broker_letter" "msgVpns/$BROKER_VPN/clients/$client_name" || return 1
}

# Provision both brokers' LLM fixtures + prime the action queues.
# Called from setup-fixtures.sh AFTER create_fixtures (so BIN_DIR/broker-driver
# is built and monitoring fixtures are up first).
create_llm_standing_fixtures() {
    _create_e2e_llm_action_queue_on "$BROKER_A_SEMP_CONFIG" a "$E2E_LLM_ACTION_QUEUE_A"
    _create_e2e_llm_action_queue_on "$BROKER_B_SEMP_CONFIG" b "$E2E_LLM_ACTION_QUEUE_B"
    # Prime both brokers so A1/A2/B1 pass regardless of which run-scenario.sh
    # targets. Uses BROKER= override to hit each broker in turn.
    BROKER=broker-a refill_e2e_llm_action_queue
    BROKER=broker-b refill_e2e_llm_action_queue
    _spawn_llm_kick_client_on "$BROKER_A_SEMP_CONFIG" a "$BROKER_A_URL" \
        "$E2E_LLM_KICK_TARGET_A" "$E2E_LLM_KICK_QUEUE_A"
    _spawn_llm_kick_client_on "$BROKER_B_SEMP_CONFIG" b "$BROKER_B_URL" \
        "$E2E_LLM_KICK_TARGET_B" "$E2E_LLM_KICK_QUEUE_B"
    _create_e2e_llm_standing_te_on "$BROKER_A_SEMP_CONFIG" a "$E2E_LLM_STANDING_TE_A"
    _create_e2e_llm_standing_te_on "$BROKER_B_SEMP_CONFIG" b "$E2E_LLM_STANDING_TE_B"
}

# Create the standing topic endpoint. Disabled so it holds no messages and
# has no delivery-side effects — the scenario only cares that the object
# exists so "no" can preserve it. Idempotent; the already-exists status
# (400) is tolerated, any other non-2xx fails fast so auth/URL/schema
# breakage surfaces here instead of as a confusing scenario failure later.
_create_e2e_llm_standing_te_on() {
    local semp_config="$1" broker_letter="$2" te="$3"
    log_info "Provisioning LLM standing topic endpoint on broker-$broker_letter: $te"
    local code
    code=$(semp_curl -s -o /dev/null -w '%{http_code}' -X POST \
        -H "Content-Type: application/json" \
        "$semp_config/msgVpns/$BROKER_VPN/topicEndpoints" \
        -d "{\"topicEndpointName\":\"$te\",\"accessType\":\"non-exclusive\",\"permission\":\"consume\",\"ingressEnabled\":false,\"egressEnabled\":false}" 2>/dev/null || echo "000")
    case "$code" in
        2*|400) ;;
        *) log_fail "provisioning standing TE $te on broker-$broker_letter returned HTTP $code"; return 1 ;;
    esac
}

# Drop the LLM fixtures on both brokers. The kick-target broker-driver
# processes are already reaped by stop_broker_drivers (called by
# cleanup_fixtures in the monitoring helpers), so this is queue-only.
# Idempotent — semp_delete returns 0 on 404.
cleanup_llm_standing_fixtures() {
    semp_delete "$BROKER_A_SEMP_CONFIG" "msgVpns/$BROKER_VPN/queues/$E2E_LLM_ACTION_QUEUE_A"
    semp_delete "$BROKER_B_SEMP_CONFIG" "msgVpns/$BROKER_VPN/queues/$E2E_LLM_ACTION_QUEUE_B"
    semp_delete "$BROKER_A_SEMP_CONFIG" "msgVpns/$BROKER_VPN/queues/$E2E_LLM_KICK_QUEUE_A"
    semp_delete "$BROKER_B_SEMP_CONFIG" "msgVpns/$BROKER_VPN/queues/$E2E_LLM_KICK_QUEUE_B"
    semp_delete "$BROKER_A_SEMP_CONFIG" "msgVpns/$BROKER_VPN/topicEndpoints/$E2E_LLM_STANDING_TE_A"
    semp_delete "$BROKER_B_SEMP_CONFIG" "msgVpns/$BROKER_VPN/topicEndpoints/$E2E_LLM_STANDING_TE_B"
}
