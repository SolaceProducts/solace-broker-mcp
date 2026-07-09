#!/usr/bin/env bash
# Action-suite helpers. The generic scaffold (broker readiness, MCP server
# lifecycle, config generation, SEMP ops, MCP wire, broker-driver lifecycle,
# assertions, test runner) lives in the shared library; this file adds only the
# action-fixture naming, the messaging-fixture builders (spooled queue,
# connected client) the action tools act on, and the sweep used to guarantee
# clean state between runs.
# Source from test scripts: source "$(dirname "$0")/helpers.sh"

set -euo pipefail

# SUITE_DIR contract (see e2e-common/lib.sh): set our own directory, then source
# the shared library, which derives BIN_DIR/ENV_FILE/REPO_ROOT and .env from it.
SUITE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../e2e-common/lib.sh
source "$SUITE_DIR/../e2e-common/lib.sh"

# Exported so broker-driver can resolve --broker=a|b to a host SMF port from the
# same single source of truth (.env, sourced by the shared library).
export BROKER_A_SMF_PORT BROKER_B_SMF_PORT

# ── Action fixtures ──────────────────────────────────────────────────────────
# Disposable objects owned per-test (create → act → assert → delete). Queue names
# are broker-suffixed so the two brokers never collide, plus shared-name `-iso`
# objects used by the cross-broker isolation tests. All share the e2e-action-
# prefix so the sweep can find and drop every remnant. Connected-client fixtures
# are long-lived broker-driver processes reaped via stop_broker_drivers; the
# queues they bind to are listed here so the sweep drops them too. These never
# touch the monitoring or management fixtures.
ACTION_QUEUE_NAMES=(
    "e2e-action-clearstats-queue-broker-a" "e2e-action-clearstats-queue-broker-b"
    "e2e-action-deletemsgs-broker-a"       "e2e-action-deletemsgs-broker-b"
    "e2e-action-clearstats-cq-broker-a"    "e2e-action-clearstats-cq-broker-b"
    "e2e-action-disc-q-broker-a"           "e2e-action-disc-q-broker-b"
    "e2e-action-deletemsgs-iso"            "e2e-action-disc-q-iso"
)

# Topics the fixtures publish/subscribe on. Distinct per fixture so concurrent or
# repeated runs never cross-deliver.
ACTION_TOPIC_CLEARSTATS_QUEUE="e2e-action/clearstats/queue"
ACTION_TOPIC_DELETEMSGS="e2e-action/deletemsgs"
ACTION_TOPIC_CLEARSTATS_CLIENT="e2e-action/clearstats/client"
ACTION_TOPIC_DISC="e2e-action/disc"

# Drop every action fixture on both brokers, ignoring 404s. Reaps any lingering
# broker-driver clients FIRST (the broker refuses to delete a queue while a
# client is bound to it), then deletes the queues. Idempotent: safe to call on
# entry (pre-clean leftover state) and from a cleanup trap (post-run / on failure).
sweep_action_fixtures() {
    stop_broker_drivers
    local semp_config name
    for semp_config in "$BROKER_A_SEMP_CONFIG" "$BROKER_B_SEMP_CONFIG"; do
        for name in "${ACTION_QUEUE_NAMES[@]}"; do
            semp_delete "$semp_config" "msgVpns/$BROKER_VPN/queues/$name"
        done
    done
}

# Create a queue with a topic subscription and spool exactly $count persistent
# messages into it with NO consumer, so spooledMsgCount (cumulative) and the live
# depth are both $count. publish-batch is one-shot (publishes, then exits), so no
# driver survives this call.
#   $1 semp_config   $2 broker_letter (a|b)   $3 queue   $4 topic   $5 count
create_spooled_queue() {
    local semp_config="$1" broker_letter="$2" queue="$3" topic="$4" count="$5"
    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues" \
        "{\"queueName\":\"$queue\",\"accessType\":\"non-exclusive\",\"permission\":\"consume\",\"ingressEnabled\":true,\"egressEnabled\":true}" >/dev/null
    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues/$queue/subscriptions" \
        "{\"subscriptionTopic\":\"$topic\"}" >/dev/null
    "$BIN_DIR/broker-driver" publish-batch \
        --broker="$broker_letter" --vpn="$BROKER_VPN" \
        --topic="$topic" --count="$count" --size=256 --message-type=persistent \
        >"$BIN_DIR/publish-batch-$queue.log" 2>&1
}

# Provision a durable queue and spawn a long-lived connected-client bound to it
# (persistent receiver on the queue as a bind target + a DIRECT subscription on
# $topic). The queue itself carries NO topic subscription: traffic for the
# clearStats fixture is delivered to the client's direct receiver (fire-and-
# forget, never redelivered), so the client's data counter is stable and no
# unacked guaranteed backlog builds up. The client self-writes a pidfile that
# stop_broker_drivers reaps. Blocks until the broker reports the client by name.
#   $1 semp_config  $2 broker_letter (a|b)  $3 label  $4 broker_url
#   $5 client_name  $6 queue  $7 topic  $8 role (pidfile discriminator)
spawn_action_client() {
    local semp_config="$1" broker_letter="$2" label="$3" broker_url="$4"
    local client_name="$5" queue="$6" topic="$7" role="$8"
    local pidfile="$BIN_DIR/broker-driver-$role-$broker_letter.pid"
    local logfile="$BIN_DIR/broker-driver-$role-$broker_letter.log"

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues" \
        "{\"queueName\":\"$queue\",\"accessType\":\"non-exclusive\",\"permission\":\"consume\",\"ingressEnabled\":true,\"egressEnabled\":true}" >/dev/null

    # nohup + setsid so the driver survives an aborted parent shell; the harness
    # finds it via the pidfile glob (broker-driver-*.pid) under this suite's bin/.
    nohup ${_SESSION_WRAP:+$_SESSION_WRAP} "$BIN_DIR/broker-driver" connected-client \
        --broker="$broker_letter" \
        --vpn="$BROKER_VPN" \
        --client-name="$client_name" \
        --queue="$queue" \
        --subscriptions="$topic" \
        --pidfile="$pidfile" \
        >"$logfile" 2>&1 &

    wait_for_pidfile "$pidfile" "$label" "$logfile" "broker-driver connected-client" || return 1
    verify_monitor_object "$broker_url" "$label" "msgVpns/$BROKER_VPN/clients/$client_name" || return 1
}
