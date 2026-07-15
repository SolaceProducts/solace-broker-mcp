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

# curl wrapper that pulls $BROKER_USER / $BROKER_PASS from the environment
# and feeds basic-auth via curl -K - (stdin config) instead of -u on the
# command line. Keeps the password out of the process argv so `ps` on the
# host can't see it. Callers pass any curl args as usual (e.g. -sf, -X DELETE,
# -w '%{http_code}'). Backslashes and double quotes in the values are
# escaped for curl's -K parser. Exported so scenario setup.cmd /
# teardown.cmd / ground_truth.shell strings — which run in `bash -c`
# children — can invoke it.
semp_curl() {
    local u="${BROKER_USER:?BROKER_USER not set}"
    local p="${BROKER_PASS:?BROKER_PASS not set}"
    u="${u//\\/\\\\}"; u="${u//\"/\\\"}"
    p="${p//\\/\\\\}"; p="${p//\"/\\\"}"
    printf 'user = "%s:%s"\n' "$u" "$p" | curl -K - "$@"
}
export -f semp_curl

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
        "$BROKER_SEMP_CONFIG/msgVpns/$BROKER_VPN/queues/$queue" 2>/dev/null || echo "000")
    case "$code" in
        2*|404) ;;
        *) echo "delete_queue_on_current_broker: DELETE $queue returned HTTP $code (queue may leak)" >&2 ;;
    esac
}
export -f delete_queue_on_current_broker

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
}
