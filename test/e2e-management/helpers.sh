#!/usr/bin/env bash
# Management-suite helpers. The generic scaffold (broker readiness, MCP server
# lifecycle, config generation, SEMP ops, MCP wire, assertions, test runner)
# lives in the shared library; this file adds only the config-fixture naming and
# the sweep used to guarantee clean state between runs.
# Source from test scripts: source "$(dirname "$0")/helpers.sh"

set -euo pipefail

# SUITE_DIR contract (see e2e-common/lib.sh): set our own directory, then source
# the shared library, which derives BIN_DIR/ENV_FILE/REPO_ROOT and .env from it.
SUITE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../e2e-common/lib.sh
source "$SUITE_DIR/../e2e-common/lib.sh"

# ── Config fixtures ──────────────────────────────────────────────────────────
# Disposable objects owned per-test (create → act → assert → delete). Names are
# broker-suffixed so the two brokers never collide, plus one shared-name queue
# (e2e-config-iso) used by the cross-broker isolation test. All share the
# e2e-config- prefix so the sweep can find and drop every remnant. These never
# touch the shared monitoring fixtures.
CONFIG_VPN_NAMES=("e2e-config-vpn-broker-a" "e2e-config-vpn-broker-b")
CONFIG_QUEUE_NAMES=("e2e-config-queue-broker-a" "e2e-config-queue-broker-b" "e2e-config-iso" "e2e-config-queue-sub-broker-a" "e2e-config-queue-sub-broker-b" "e2e-config-queue-sub-htmlent-broker-a" "e2e-config-queue-sub-htmlent-broker-b" "e2e-config-queue-sub-htmlent-del-broker-a" "e2e-config-queue-sub-htmlent-del-broker-b" "e2e-config-queue-owner-ok-broker-a" "e2e-config-queue-owner-ok-broker-b" "e2e-config-queue-owner-ghost-broker-a" "e2e-config-queue-owner-ghost-broker-b" "e2e-config-kafka-q-in-broker-a" "e2e-config-kafka-q-in-broker-b" "e2e-config-kafka-q-out-broker-a" "e2e-config-kafka-q-out-broker-b" "e2e-config-kafka-q-late-broker-a" "e2e-config-kafka-q-late-broker-b")
CONFIG_TE_NAMES=("e2e-config-te-broker-a" "e2e-config-te-broker-b" "e2e-config-te-owner-ghost-broker-a" "e2e-config-te-owner-ghost-broker-b")
CONFIG_RDP_NAMES=("e2e-config-rdp-broker-a" "e2e-config-rdp-broker-b" "e2e-config-rdp-iso")
# SOL-153080: a real, provisioned client username used as the "owner" fixture
# for the owner-validation tests below. Never connected to — its only purpose
# is to exist, so create-queue/create-topic-endpoint have a real username to
# accept.
CONFIG_CLIENT_USERNAME_NAMES=("e2e-config-owner-broker-a" "e2e-config-owner-broker-b")

# ── Kafka bridge fixtures (SOL-156214) ───────────────────────────────────────
# The real Kafka in docker-compose.yml. KAFKA_BOOTSTRAP is PLAINTEXT
# (authenticationScheme none); KAFKA_BOOTSTRAP_SASL is SASL_PLAINTEXT/PLAIN
# (authenticationScheme basic). The SASL credentials are throwaway test values
# defined in docker-compose.yml; tests set them on the bridge out of band
# (direct SEMP), never through the MCP server.
KAFKA_CONTAINER="kafka-e2e-mgmt"
KAFKA_BOOTSTRAP="kafka-e2e-mgmt:9092"
KAFKA_BOOTSTRAP_SASL="kafka-e2e-mgmt:9094"
KAFKA_SASL_USER="kuser"
KAFKA_SASL_PASSWORD="kpass"
# Every Kafka bridge object the tests create is named e2e-config-kafka-*, per
# broker. The sweep finds bridges by that prefix, so a new test needs no entry here.
KAFKA_FIXTURE_PREFIX="e2e-config-kafka-"

# Delete every config fixture on both brokers via the SEMP config API, ignoring
# 404s. Idempotent: safe to call before a run (pre-clean leftover state) and from
# a cleanup trap (post-run / on failure). Queues, topic endpoints, and client
# usernames live in the default VPN; the VPN fixtures are standalone VPNs,
# dropped last. Client usernames are swept after queues/topic endpoints so a
# (bug-induced) queue binding never blocks the username's own deletion.
sweep_config_fixtures() {
    local semp_config name
    # Kafka bridges first: their bindings point at queues, and the broker does not
    # stop a queue being deleted out from under a binding.
    sweep_kafka_fixtures
    for semp_config in "$BROKER_A_SEMP_CONFIG" "$BROKER_B_SEMP_CONFIG"; do
        for name in "${CONFIG_QUEUE_NAMES[@]}"; do
            semp_delete "$semp_config" "msgVpns/$BROKER_VPN/queues/$name"
        done
        for name in "${CONFIG_TE_NAMES[@]}"; do
            semp_delete "$semp_config" "msgVpns/$BROKER_VPN/topicEndpoints/$name"
        done
        for name in "${CONFIG_RDP_NAMES[@]}"; do
            semp_delete "$semp_config" "msgVpns/$BROKER_VPN/restDeliveryPoints/$name"
        done
        for name in "${CONFIG_CLIENT_USERNAME_NAMES[@]}"; do
            semp_delete "$semp_config" "msgVpns/$BROKER_VPN/clientUsernames/$name"
        done
        for name in "${CONFIG_VPN_NAMES[@]}"; do
            semp_delete "$semp_config" "msgVpns/$name"
        done
    done
}

# ── Kafka bridge helpers (SOL-156214) ────────────────────────────────────────

# PATCH a config object directly through SEMP. This is how the tests play the
# user's "set the secret out of band" step: the secret goes straight to the
# broker and never touches the MCP server.
#   $1 semp_config   $2 path   $3 json body
semp_patch() {
    local semp_config="$1" path="$2" data="$3"
    semp_curl -sf -X PATCH -H "Content-Type: application/json" "$semp_config/$path" -d "$data" >/dev/null
}

# Block until Kafka answers, so the tests do not race a still-starting KRaft node.
wait_for_kafka() {
    local max_attempts="${1:-90}" attempt=0
    log_info "Waiting for Kafka ($KAFKA_CONTAINER) ..."
    while [ $attempt -lt "$max_attempts" ]; do
        if docker exec "$KAFKA_CONTAINER" /opt/kafka/bin/kafka-broker-api-versions.sh \
            --bootstrap-server localhost:9092 >/dev/null 2>&1; then
            log_ok "Kafka ready after ${attempt}s"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done
    log_fail "Kafka not ready after ${max_attempts}s"
    return 1
}

# Create a Kafka topic (idempotent). Auto-creation is off, so a topic exists only
# because a test made it.
kafka_create_topic() {
    docker exec "$KAFKA_CONTAINER" /opt/kafka/bin/kafka-topics.sh --create --if-not-exists \
        --topic "$1" --bootstrap-server localhost:9092 --partitions 1 --replication-factor 1 >/dev/null
}

# Produce records to a topic: one "key:value" per argument.
kafka_produce() {
    local topic="$1"; shift
    printf '%s\n' "$@" | docker exec -i "$KAFKA_CONTAINER" /opt/kafka/bin/kafka-console-producer.sh \
        --bootstrap-server localhost:9092 --topic "$topic" \
        --property parse.key=true --property key.separator=: >/dev/null
}

# Delete every Kafka bridge fixture on both brokers: bindings first (the broker
# refuses to delete a bridge that still has bindings and does not cascade), then
# the bridge. Idempotent; safe before a run and from a cleanup trap.
sweep_kafka_fixtures() {
    local semp_config kind binding_kind key name binding
    for semp_config in "$BROKER_A_SEMP_CONFIG" "$BROKER_B_SEMP_CONFIG"; do
        for kind in kafkaReceivers:topicBindings:topicName kafkaSenders:queueBindings:queueName; do
            IFS=: read -r kind binding_kind key <<<"$kind"
            for name in $(semp_curl -sf "$semp_config/msgVpns/$BROKER_VPN/$kind?count=100" 2>/dev/null \
                    | jq -r --arg p "$KAFKA_FIXTURE_PREFIX" '.data[]? | (.kafkaReceiverName // .kafkaSenderName) | select(startswith($p))'); do
                for binding in $(semp_curl -sf "$semp_config/msgVpns/$BROKER_VPN/$kind/$name/$binding_kind?count=100" 2>/dev/null \
                        | jq -r --arg k "$key" '.data[]? | .[$k] | @uri'); do
                    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/$kind/$name/$binding_kind/$binding"
                done
                semp_delete "$semp_config" "msgVpns/$BROKER_VPN/$kind/$name"
            done
        done
    done
}
