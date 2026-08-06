#!/usr/bin/env bash
# Monitoring-suite helpers. The generic scaffold (broker readiness, MCP server
# lifecycle, config generation, SEMP ops, base broker fixtures, MCP wire,
# assertions, test runner) lives in the shared library; this file adds only the
# monitoring-specific fixtures (F1–F8) and broker-driver orchestration.
# Source from test scripts: source "$(dirname "$0")/helpers.sh"

set -euo pipefail

# SUITE_DIR contract (see e2e-common/lib.sh): set our own directory, then source
# the shared library, which derives BIN_DIR/ENV_FILE/REPO_ROOT and .env from it.
# Honor a pre-set SUITE_DIR so cross-suite sourcing (e.g. e2e-llm/helpers.sh
# reusing our F1–F8 code against the LLM suite's .env / bin / ports) keeps
# the caller's tree instead of being silently rewired to e2e-monitoring.
SUITE_DIR="${SUITE_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}"
# shellcheck source=../e2e-common/lib.sh
source "$SUITE_DIR/../e2e-common/lib.sh"

# Exported so broker-driver (spawned as a child by create_connected_client_on
# and the F4-F7 helpers) can resolve --broker=a|b to a SMF host:port from the
# same single source of truth (.env, sourced by the shared library).
export BROKER_A_SMF_PORT BROKER_B_SMF_PORT

# broker-driver lifecycle helpers (build_broker_driver, wait_for_pidfile,
# stop_broker_drivers, BROKER_DRIVER_PIDFILE_GLOB) now live in the shared
# e2e-common/lib.sh — the monitoring and action suites both use them. The sources
# moved to test/e2e-common/broker-driver too. This file keeps only the
# monitoring-specific F1–F8 fixtures below.

# ── Broker fixtures (F1–F8, layered on the shared base set in lib.sh) ─────────

verify_multi_vpn_on() {
    local broker_url="$1"
    local label="$2"
    log_info "Verifying multi-VPN fixture visible on $label ..."
    verify_monitor_object "$broker_url" "$label" "msgVpns/test-vpn" || true
}

verify_multi_queue_on() {
    local broker_url="$1"
    local label="$2"
    log_info "Verifying multi-queue fixtures visible on $label ..."
    verify_monitor_object "$broker_url" "$label" "msgVpns/$BROKER_VPN/queues/test-queue-2" || true
    verify_monitor_object "$broker_url" "$label" "msgVpns/$BROKER_VPN/queues/test-queue-3" || true
}

# Provisions a second, non-default VPN ("test-vpn") on a broker with enabled=false.
# Intentionally no client-user / ACL / queue provisioning — this VPN exists only
# to exercise multi-VPN discovery and listing, not messaging.
create_multi_vpn_on() {
    local semp_config="$1"
    local label="$2"
    local broker_url="$3"
    log_info "Creating multi-VPN fixture on $label ..."
    semp_post "$semp_config" "msgVpns" \
        '{"msgVpnName":"test-vpn","enabled":false}' >/dev/null
    verify_multi_vpn_on "$broker_url" "$label"
    log_info "Multi-VPN fixture created on $label (test-vpn, enabled=false)"
}

cleanup_multi_vpn_on() {
    local semp_config="$1"
    local label="$2"
    log_info "Cleaning up multi-VPN fixture on $label ..."
    semp_delete "$semp_config" "msgVpns/test-vpn"
    log_info "Multi-VPN fixture cleaned up on $label"
}

# Provisions a bare enabled VPN ("test-vpn-empty") with no client-user / ACL /
# queue setup — its only job is to satisfy list-vpns.zeroConnectionCount. The
# handler probes each enabled+up VPN with getMsgVpnClients filtered by
# `clientUsername != #*`; a VPN with no real client hits that empty-result
# path and lands in the count. Deliberately distinct from `test-vpn`
# (enabled=false, feeds disabledCount).
create_empty_enabled_vpn_on() {
    local semp_config="$1"
    local label="$2"
    local broker_url="$3"
    log_info "Creating empty-enabled-VPN fixture on $label ..."
    semp_post "$semp_config" "msgVpns" \
        '{"msgVpnName":"test-vpn-empty","enabled":true}' >/dev/null
    verify_monitor_object "$broker_url" "$label" "msgVpns/test-vpn-empty" \
        15 '.data.enabled == true and .data.state == "up"'
    log_info "Empty-enabled-VPN fixture created on $label (test-vpn-empty, enabled=true)"
}

cleanup_empty_enabled_vpn_on() {
    local semp_config="$1"
    local label="$2"
    log_info "Cleaning up empty-enabled-VPN fixture on $label ..."
    semp_delete "$semp_config" "msgVpns/test-vpn-empty"
    log_info "Empty-enabled-VPN fixture cleaned up on $label"
}

# Provisions two additional queues on the default VPN to exercise multi-queue
# discovery and bound-vs-unbound state:
#   - test-queue-2: non-exclusive, bound to the existing test-rdp
#   - test-queue-3: non-exclusive, unbound
# Depends on create_fixtures_on having run first (reuses test-rdp).
create_multi_queue_on() {
    local semp_config="$1"
    local label="$2"
    local broker_url="$3"
    log_info "Creating multi-queue fixtures on $label ..."

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues" \
        '{"queueName":"test-queue-2","accessType":"non-exclusive","permission":"consume","ingressEnabled":true,"egressEnabled":true}' >/dev/null

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues" \
        '{"queueName":"test-queue-3","accessType":"non-exclusive","permission":"consume","ingressEnabled":true,"egressEnabled":true}' >/dev/null

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/restDeliveryPoints/test-rdp/queueBindings" \
        '{"queueBindingName":"test-queue-2","postRequestTarget":"/test"}' >/dev/null

    verify_multi_queue_on "$broker_url" "$label"
    log_info "Multi-queue fixtures created on $label (test-queue-2 bound to test-rdp, test-queue-3 unbound)"
}

cleanup_multi_queue_on() {
    local semp_config="$1"
    local label="$2"
    log_info "Cleaning up multi-queue fixtures on $label ..."
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/restDeliveryPoints/test-rdp/queueBindings/test-queue-2"
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/queues/test-queue-2"
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/queues/test-queue-3"
    log_info "Multi-queue fixtures cleaned up on $label"
}

# F8 bridges — SOL-152231. Distinct from every other fixture in this file:
# F1-F7 apply independently per broker, but a bridge is inherently
# cross-broker, so each side's fixture points at the OTHER broker's internal
# container hostname (fixed by docker-compose.yml's `hostname:` field, not
# .env-configurable like the host-mapped ports — reachable from the sibling
# container over the compose network's default bridge on 55555, the SMF
# plaintext listener's *internal* container port).
# Overridable via each suite's .env (same SUITE_DIR/.env indirection lib.sh
# uses for ports/creds) so the LLM suite — which sources this file for its
# F1–F8 fixture code — can point bridges at solace-e2e-llm-* instead of
# monitoring's containers. Defaults preserve monitoring's original wiring.
F8_REMOTE_HOST_FROM_A="${F8_REMOTE_HOST_FROM_A:-solace-e2e-mon-b}"
F8_REMOTE_HOST_FROM_B="${F8_REMOTE_HOST_FROM_B:-solace-e2e-mon-a}"
F8_SMF_PORT=55555

# Bridge health-state constants and predicate, mirroring bridgeInboundUpStates
# / bridgeOutboundUpStates in list_bridges.go. Single source of truth for the
# assertions in this file's verify_bridges_on plus verify-fixtures.sh and
# test-monitoring-tools.sh (both source this file) — without it, three
# hand-copied predicates could silently drift out of sync with a future change
# to the server's classification and still pass.
BRIDGE_HEALTHY_INBOUND_STATE="ready-in-sync"
BRIDGE_HEALTHY_OUTBOUND_STATE="ready"

# jq boolean fragment: true when the inboundState value at jq path $1 (e.g.
# ".data.inboundState" or ".bridgeStatus.data.inboundState" — callers differ
# on whether they're reading a raw SEMP body or an MCP tool response) is NOT
# one of this server's healthy inbound states.
bridge_inbound_unhealthy_jq() {
    local path="$1"
    printf '%s != "%s" and %s != "ready-subscribing" and %s != "not-applicable"' \
        "$path" "$BRIDGE_HEALTHY_INBOUND_STATE" "$path" "$path"
}

# jq boolean fragment: true when the inboundState value at jq path $1 IS one
# of this server's healthy inbound states — the logical complement of
# bridge_inbound_unhealthy_jq above, kept as its own function (not `not (...)`)
# so callers read naturally either way. "ready-subscribing" is a real healthy
# state (still adding configured subscriptions), not just "not yet broken" —
# a bridge with no remote subscriptions configured, like this suite's
# fixtures, likely never lingers there, but asserting the exact steady-state
# value alone was stricter than the server's own classification and a
# plausible flake source on slower CI runners.
bridge_inbound_healthy_jq() {
    local path="$1"
    printf '%s == "%s" or %s == "ready-subscribing"' \
        "$path" "$BRIDGE_HEALTHY_INBOUND_STATE" "$path"
}

# Creates three bridges on one broker, pointed at $remote_host (the sibling
# broker's container hostname). Does NOT verify convergence — call
# verify_bridges_on for both brokers only after create_bridges_on has run for
# both, since a lone one-sided bridge's outboundState reports
# "not-applicable" until the peer's reciprocal bridge also exists
# (lab-verified against SEMP 2.46; see verify_bridges_on).
#
# LAB-VERIFIED: deleting a bridge cascades its remoteMsgVpns sub-resource
# automatically (unlike RDPs, which require deleting queueBindings/
# restConsumers before the RDP itself) — cleanup_bridges_on below only
# deletes the three top-level bridge objects.
create_bridges_on() {
    local semp_config="$1"
    local label="$2"
    local remote_host="$3"
    log_info "Creating bridge fixtures on $label (remote: $remote_host) ..."

    # test-bridge: healthy once both sides exist. remoteAuthenticationScheme
    # basic + clientUsername "default" with an empty password reuses the same
    # pre-provisioned `default` client-username the broker-driver F3/F4/etc.
    # fixtures already authenticate as (connected_client.go) — no new
    # client-username fixture needed.
    semp_post "$semp_config" "msgVpns/$BROKER_VPN/bridges" \
        '{"bridgeName":"test-bridge","bridgeVirtualRouter":"auto","enabled":true,"remoteAuthenticationScheme":"basic","remoteAuthenticationBasicClientUsername":"default","remoteAuthenticationBasicPassword":""}' >/dev/null
    semp_post "$semp_config" "msgVpns/$BROKER_VPN/bridges/test-bridge,auto/remoteMsgVpns" \
        "$(jq -nc --arg loc "$remote_host:$F8_SMF_PORT" \
            '{remoteMsgVpnName:"default",remoteMsgVpnLocation:$loc,remoteMsgVpnInterface:"",enabled:true,tlsEnabled:false,clientUsername:"default",password:""}')" >/dev/null

    # test-bridge-failing: enabled, pointed at an address nothing listens on
    # (immediate TCP refusal) so inboundState settles into a down/retry state.
    # LAB-VERIFIED (SEMP 2.46): unlike RDPs' lastFailureReason, a bridge's
    # inboundFailureReason and rxConnectionFailureCategory stay empty /
    # "no-failure" for connection-level failures — tested against an
    # unreachable host, a nonexistent remote VPN name, and wrong credentials,
    # all three left inboundFailureReason == "" indefinitely. Only
    # inboundState changes (settles at "not-ready-wait-next"). Do not assert
    # byInboundFailureReason has an entry from this fixture — assert
    # downCount instead (see test-monitoring-tools.sh).
    semp_post "$semp_config" "msgVpns/$BROKER_VPN/bridges" \
        '{"bridgeName":"test-bridge-failing","bridgeVirtualRouter":"auto","enabled":true,"remoteAuthenticationScheme":"basic","remoteAuthenticationBasicClientUsername":"default","remoteAuthenticationBasicPassword":""}' >/dev/null
    semp_post "$semp_config" "msgVpns/$BROKER_VPN/bridges/test-bridge-failing,auto/remoteMsgVpns" \
        '{"remoteMsgVpnName":"default","remoteMsgVpnLocation":"127.0.0.1:1","remoteMsgVpnInterface":"","enabled":true,"tlsEnabled":false,"clientUsername":"default","password":""}' >/dev/null

    # test-bridge-disabled: enabled=false — feeds disabledCount. LAB-VERIFIED:
    # inboundFailureReason == "Bridge disabled" here (unlike the failing case
    # above, which never populates a reason) — this is exactly the case the
    # postprocess handler's admin-disabled exclusion from
    # byInboundFailureReason must catch, mirroring list-rdps' "RDP Shutdown"
    # exclusion.
    semp_post "$semp_config" "msgVpns/$BROKER_VPN/bridges" \
        '{"bridgeName":"test-bridge-disabled","bridgeVirtualRouter":"auto","enabled":false,"remoteAuthenticationScheme":"basic","remoteAuthenticationBasicClientUsername":"default","remoteAuthenticationBasicPassword":""}' >/dev/null

    log_info "Bridge fixtures created on $label"
}

# Polls all three bridges to their converged terminal state. Call only after
# create_bridges_on has run for BOTH brokers — test-bridge's outboundState
# depends on the peer's reciprocal bridge existing (lab-verified: a one-sided
# bridge reports outboundState "not-applicable" until both sides are up).
#
# These three calls are bare (no `|| true`): a timeout here aborts
# create_fixtures rather than degrading to a downstream test failure. That's
# deliberate, not an oversight — it matches the same required-convergence
# pattern already used by create_empty_enabled_vpn_on above (also bare) and
# the RDP-failing-reason poll in e2e-common/lib.sh: the tool-level summary/
# downCount assertions in test-monitoring-tools.sh depend on this exact state,
# so failing loudly here (with a clear "which bridge, which broker" message)
# beats a vaguer downstream assertion mismatch. The tradeoff is real —
# bridges are the one cross-broker, two-sided handshake in this suite and so
# the most timeout-prone fixture — but softening only this one call would
# make it inconsistent with its same-file sibling rather than actually safer.
verify_bridges_on() {
    local broker_url="$1"
    local label="$2"
    log_info "Verifying bridge fixtures visible on $label ..."
    verify_monitor_object "$broker_url" "$label" "msgVpns/$BROKER_VPN/bridges/test-bridge,auto" \
        30 "($(bridge_inbound_healthy_jq '.data.inboundState')) and .data.outboundState == \"$BRIDGE_HEALTHY_OUTBOUND_STATE\""
    # No inboundFailureReason predicate here (it never populates for this
    # fixture — see create_bridges_on) — poll on the classification this
    # server's own down logic uses instead (matches
    # bridgeInboundUpStates/bridgeOutboundUpStates in list_bridges.go).
    verify_monitor_object "$broker_url" "$label" "msgVpns/$BROKER_VPN/bridges/test-bridge-failing,auto" \
        30 "$(bridge_inbound_unhealthy_jq '.data.inboundState')"
    verify_monitor_object "$broker_url" "$label" "msgVpns/$BROKER_VPN/bridges/test-bridge-disabled,auto" \
        15 '.data.enabled == false'
}

cleanup_bridges_on() {
    local semp_config="$1"
    local label="$2"
    log_info "Cleaning up bridge fixtures on $label ..."
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/bridges/test-bridge-disabled,auto"
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/bridges/test-bridge-failing,auto"
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/bridges/test-bridge,auto"
    log_info "Bridge fixtures cleaned up on $label"
}

# F9/F10 Kafka Receivers/Senders — SOL-152370. Both brokers bridge to the
# same real external broker: the "kafka" service in docker-compose.yml
# (apache/kafka:3.7.0, KRaft mode, container hostname kafka-e2e-mon),
# reachable from either Solace container the same way F8's sibling-broker
# hostname is — over the compose network's default bridge.
#
# LAB-VERIFIED (SEMP 2.46, solace/solace-pubsub-standard:latest): creating a
# Kafka Receiver/Sender at all is gated by two enum-restricted scaling
# settings that default to 0 on this image — not a license restriction, as
# SOL-152370 originally concluded before this was investigated further. The
# confd env-backend keys are /system/scaling/maxkafkabridgecount (valid
# values: 0, 10, 50, 200) and .../maxkafkabrokerconnectioncount (valid
# values: 0, 300, 2000, 10000) — both enums, not free integers; other values
# fail confd's config check and the container exits at boot. Set via
# SYSTEM_SCALING_MAXKAFKABRIDGECOUNT/SYSTEM_SCALING_MAXKAFKABROKERCONNECTIONCOUNT
# in docker-compose.yml (note the SYSTEM_ prefix, not SOLACE_ — an earlier,
# wrong guess at the variable name silently no-ops instead of erroring, since
# confd's env backend just falls through to the "0" default for an unknown
# key rather than failing).
#
# Also LAB-VERIFIED: unlike bridges' inboundFailureReason (which never
# populates for connection-level failures — see F8 above), Kafka Receiver/
# Sender's failureReason DOES populate reliably: "Shutdown" for an
# admin-disabled object, "No remote-broker in UP state" for an unreachable
# bootstrap address — both stable across 45s of polling, not transient.
# Deleting a Kafka Receiver/Sender does NOT cascade its topicBindings/
# queueBindings sub-resource — unlike bridges, but like RDPs — so cleanup
# below deletes bindings before the parent object.
F9_KAFKA_TOPIC="e2e-monitoring-kafka-topic"
F9_KAFKA_LOCAL_TOPIC="e2e/kafka/receiver"
F10_KAFKA_QUEUE="test-queue-kafka-sender"
F10_KAFKA_REMOTE_TOPIC="e2e-monitoring-kafka-sender-topic"

# True when the current suite's compose stack declares a Kafka service at
# all. Intent, not runtime state: this suite's docker-compose.yml always
# declares "kafka", so this is always true here, and a declared-but-not-ready
# Kafka is a real failure that should abort loudly (see wait_for_kafka).
# e2e-llm/helpers.sh reuses this file's F1-F8 fixture code wholesale (see its
# own header comment) against its own brokers, whose docker-compose.yml
# declares no "kafka" service — SUITE_DIR there is the LLM suite's own
# directory (the established contract, see this file's header comment), so
# this correctly reads *its* compose file and returns false, a legitimate
# skip rather than a failure.
#
# A prior version of this predicate checked `docker ps` for a running
# kafka-e2e-mon container instead of declared intent — that conflated "no
# Kafka by design" with "Kafka failed to start" for any ordinary reason
# (image pull failure, a port clash, a healthcheck that never passes): in
# this suite specifically, both cases produced a silent skip here, followed
# by the Tool 16-19 tests failing on "test-kafka-receiver must be present"
# with the real cause sitting in a log_warn far up the log. Gating on the
# compose declaration instead means a real Kafka failure now aborts loudly at
# wait_for_kafka, naming Kafka, rather than surfacing as two dozen downstream
# assertion failures. Cleanup functions need no equivalent guard: they only
# issue semp_delete against the Solace broker (safe/idempotent on a 404
# regardless of whether kafka-e2e-mon ever existed), never docker exec
# against Kafka itself.
#
# LAB-VERIFIED: `docker compose config --services` can itself fail
# transiently under load (observed directly once during local testing, amid
# the concurrent docker activity of brokers just having come up) — silently
# treating that failure the same as "kafka legitimately not declared" would
# reintroduce the exact silent-misclassification bug this predicate exists to
# fix, just from a different cause. So the compose command's own exit status
# is checked explicitly, with a couple of retries for the transient case,
# rather than folding it into the same pipeline as the grep.
kafka_expected() {
    local services attempt
    for attempt in 1 2 3; do
        if services=$(docker compose -f "$SUITE_DIR/docker-compose.yml" config --services 2>&1); then
            printf '%s\n' "$services" | grep -qx kafka
            return $?
        fi
        sleep 1
    done
    log_warn "docker compose config failed after 3 attempts while checking Kafka expectations: $services"
    return 1
}

# Blocks until the Kafka broker's inter-broker protocol responds, so F9/F10
# fixture creation doesn't race a still-starting KRaft node. Docker Compose's
# healthcheck on the "kafka" service already gates this in practice (the
# Solace containers wait on their own healthchecks the same way via
# wait_for_all_brokers), but this is a direct, suite-owned check since Kafka
# is unique to this suite (not part of the shared e2e-common scaffold).
wait_for_kafka() {
    local max_attempts="${1:-60}"
    local attempt=0
    log_info "Waiting for Kafka broker (kafka-e2e-mon) ..."
    while [ $attempt -lt "$max_attempts" ]; do
        if docker exec kafka-e2e-mon /opt/kafka/bin/kafka-broker-api-versions.sh \
            --bootstrap-server localhost:9092 >/dev/null 2>&1; then
            log_ok "Kafka broker ready after ${attempt}s"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done
    # Called unguarded from create_fixtures under this file's `set -euo
    # pipefail` (line 8) — returning non-zero here aborts the whole fixture
    # set immediately, it does not "proceed anyway". Now that the caller
    # gates on kafka_expected (declared intent, not runtime state), reaching
    # here means Kafka was supposed to come up and didn't, so the abort is
    # the correct behavior and the message says so.
    log_warn "Kafka broker not ready after ${max_attempts}s — aborting F9/F10 setup"
    return 1
}

# Creates the two topics F9/F10 fixtures bind to. Idempotent (--if-not-exists)
# and safe to call on every run-all.sh invocation, including repeated runs
# against an already-up broker during development (see README.md Quickstart).
create_kafka_topics() {
    log_info "Creating Kafka topics on kafka-e2e-mon ..."
    docker exec kafka-e2e-mon /opt/kafka/bin/kafka-topics.sh --create --if-not-exists \
        --topic "$F9_KAFKA_TOPIC" --bootstrap-server localhost:9092 \
        --partitions 1 --replication-factor 1 >/dev/null
    docker exec kafka-e2e-mon /opt/kafka/bin/kafka-topics.sh --create --if-not-exists \
        --topic "$F10_KAFKA_REMOTE_TOPIC" --bootstrap-server localhost:9092 \
        --partitions 1 --replication-factor 1 >/dev/null
    log_info "Kafka topics ready"
}

# Creates three Kafka Receivers per broker, mirroring F8 bridges' three-state
# pattern: healthy (bound to the real kafka-e2e-mon broker and topic),
# failing (enabled, unreachable bootstrap address), disabled.
create_kafka_receivers_on() {
    local semp_config="$1"
    local label="$2"
    log_info "Creating Kafka Receiver fixtures on $label ..."

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/kafkaReceivers" \
        '{"kafkaReceiverName":"test-kafka-receiver","enabled":true,"authenticationScheme":"none","bootstrapAddressList":"kafka-e2e-mon:9092"}' >/dev/null
    semp_post "$semp_config" "msgVpns/$BROKER_VPN/kafkaReceivers/test-kafka-receiver/topicBindings" \
        "$(jq -nc --arg t "$F9_KAFKA_TOPIC" --arg lt "$F9_KAFKA_LOCAL_TOPIC" \
            '{topicName:$t,localTopic:$lt,enabled:true}')" >/dev/null

    # test-kafka-receiver-failing: enabled, pointed at an address nothing
    # listens on (immediate TCP refusal) so it settles into a down/retry
    # state with failureReason populated (unlike bridges' equivalent case).
    semp_post "$semp_config" "msgVpns/$BROKER_VPN/kafkaReceivers" \
        '{"kafkaReceiverName":"test-kafka-receiver-failing","enabled":true,"authenticationScheme":"none","bootstrapAddressList":"127.0.0.1:1"}' >/dev/null

    # test-kafka-receiver-disabled: enabled=false — feeds disabledCount and
    # gives byFailureReason's admin-disabled exclusion something real to
    # exclude (failureReason populates "Shutdown" here).
    semp_post "$semp_config" "msgVpns/$BROKER_VPN/kafkaReceivers" \
        '{"kafkaReceiverName":"test-kafka-receiver-disabled","enabled":false,"authenticationScheme":"none","bootstrapAddressList":"127.0.0.1:1"}' >/dev/null

    log_info "Kafka Receiver fixtures created on $label"
}

verify_kafka_receivers_on() {
    local broker_url="$1"
    local label="$2"
    log_info "Verifying Kafka Receiver fixtures visible on $label ..."
    verify_monitor_object "$broker_url" "$label" "msgVpns/$BROKER_VPN/kafkaReceivers/test-kafka-receiver" \
        60 '.data.up == true'
    verify_monitor_object "$broker_url" "$label" "msgVpns/$BROKER_VPN/kafkaReceivers/test-kafka-receiver-failing" \
        30 '.data.failureReason != ""'
    verify_monitor_object "$broker_url" "$label" "msgVpns/$BROKER_VPN/kafkaReceivers/test-kafka-receiver-disabled" \
        15 '.data.enabled == false'
}

# Bindings must be deleted before their parent Kafka Receiver — lab-verified:
# unlike bridges' remoteMsgVpns, this object does NOT cascade-delete.
cleanup_kafka_receivers_on() {
    local semp_config="$1"
    local label="$2"
    log_info "Cleaning up Kafka Receiver fixtures on $label ..."
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/kafkaReceivers/test-kafka-receiver/topicBindings/$F9_KAFKA_TOPIC"
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/kafkaReceivers/test-kafka-receiver"
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/kafkaReceivers/test-kafka-receiver-failing"
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/kafkaReceivers/test-kafka-receiver-disabled"
    log_info "Kafka Receiver fixtures cleaned up on $label"
}

# Creates three Kafka Senders per broker, same three-state pattern as
# receivers above. The healthy sender needs a local queue to bind from (feeds
# messages published to that queue out to the real kafka-e2e-mon broker's
# topic) — separate from every other fixture's queue in this suite.
create_kafka_senders_on() {
    local semp_config="$1"
    local label="$2"
    log_info "Creating Kafka Sender fixtures on $label ..."

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues" \
        "$(jq -nc --arg q "$F10_KAFKA_QUEUE" \
            '{queueName:$q,accessType:"non-exclusive",permission:"consume",ingressEnabled:true,egressEnabled:true}')" >/dev/null

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/kafkaSenders" \
        '{"kafkaSenderName":"test-kafka-sender","enabled":true,"authenticationScheme":"none","bootstrapAddressList":"kafka-e2e-mon:9092"}' >/dev/null
    # queueBindings default to enabled=false regardless of the request body
    # order — must be set explicitly, lab-verified (a binding created without
    # it silently never carries traffic and the sender never reports up).
    semp_post "$semp_config" "msgVpns/$BROKER_VPN/kafkaSenders/test-kafka-sender/queueBindings" \
        "$(jq -nc --arg q "$F10_KAFKA_QUEUE" --arg t "$F10_KAFKA_REMOTE_TOPIC" \
            '{queueName:$q,remoteTopic:$t,enabled:true}')" >/dev/null

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/kafkaSenders" \
        '{"kafkaSenderName":"test-kafka-sender-failing","enabled":true,"authenticationScheme":"none","bootstrapAddressList":"127.0.0.1:1"}' >/dev/null

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/kafkaSenders" \
        '{"kafkaSenderName":"test-kafka-sender-disabled","enabled":false,"authenticationScheme":"none","bootstrapAddressList":"127.0.0.1:1"}' >/dev/null

    log_info "Kafka Sender fixtures created on $label"
}

verify_kafka_senders_on() {
    local broker_url="$1"
    local label="$2"
    log_info "Verifying Kafka Sender fixtures visible on $label ..."
    verify_monitor_object "$broker_url" "$label" "msgVpns/$BROKER_VPN/kafkaSenders/test-kafka-sender" \
        60 '.data.up == true'
    verify_monitor_object "$broker_url" "$label" "msgVpns/$BROKER_VPN/kafkaSenders/test-kafka-sender-failing" \
        30 '.data.failureReason != ""'
    verify_monitor_object "$broker_url" "$label" "msgVpns/$BROKER_VPN/kafkaSenders/test-kafka-sender-disabled" \
        15 '.data.enabled == false'
}

cleanup_kafka_senders_on() {
    local semp_config="$1"
    local label="$2"
    log_info "Cleaning up Kafka Sender fixtures on $label ..."
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/kafkaSenders/test-kafka-sender/queueBindings/$F10_KAFKA_QUEUE"
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/kafkaSenders/test-kafka-sender"
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/kafkaSenders/test-kafka-sender-failing"
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/kafkaSenders/test-kafka-sender-disabled"
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/queues/$F10_KAFKA_QUEUE"
    log_info "Kafka Sender fixtures cleaned up on $label"
}

# F3 connected client — single source of truth for the fixture's identifiers,
# referenced by both create_connected_client_on and verify-fixtures.sh.
F3_CLIENT_NAME_A="e2e-monitoring-connected-a"
F3_CLIENT_NAME_B="e2e-monitoring-connected-b"
F3_SUBSCRIPTIONS="e2e-monitoring/connected/t1,e2e-monitoring/connected/t2"

# Spawn a long-lived broker-driver process that binds a persistent receiver
# to test-queue and holds direct topic subscriptions, satisfying F3. The
# process self-writes a PID file that stop_broker_drivers later reaps.
# Depends on create_fixtures_on having created test-queue first.
create_connected_client_on() {
    local label="$1"
    local broker_url="$2"
    local broker_letter="$3"     # "a" or "b" — resolves SMF host:port in broker-driver
    local client_name="$4"
    local pidfile="$BIN_DIR/broker-driver-f3-$broker_letter.pid"
    local logfile="$BIN_DIR/broker-driver-f3-$broker_letter.log"
    log_info "Creating connected-client fixture on $label (clientName=$client_name) ..."

    # nohup + setsid so the driver survives an aborted parent shell and is in
    # its own session; the bash harness still finds it via the pidfile glob.
    nohup ${_SESSION_WRAP:+$_SESSION_WRAP} "$BIN_DIR/broker-driver" connected-client \
        --broker="$broker_letter" \
        --vpn="$BROKER_VPN" \
        --client-name="$client_name" \
        --queue=test-queue \
        --subscriptions="$F3_SUBSCRIPTIONS" \
        --pidfile="$pidfile" \
        >"$logfile" 2>&1 &

    # Wait for the driver to self-write its pidfile (signals readiness).
    wait_for_pidfile "$pidfile" "$label" "$logfile" "broker-driver" || return 1

    # Then wait until the broker actually reports the client by name.
    verify_monitor_object "$broker_url" "$label" \
        "msgVpns/$BROKER_VPN/clients/$client_name"
    log_info "Connected-client fixture created on $label (PID=$(<"$pidfile"))"
}

# stop_broker_drivers handles termination via the PID file, so this is a
# label-only nop kept for symmetry with the F1/F2 create/cleanup pairs.
cleanup_connected_client_on() {
    local label="$1"
    log_info "Connected-client cleanup on $label deferred to stop_broker_drivers"
}

# F4 sustained-traffic constants. The topic must be one of F3_SUBSCRIPTIONS
# so the F3 direct receiver drains the persistent publish — that's how
# AC 5's txMsgRate threshold becomes reachable.
F4_TOPIC="e2e-monitoring/connected/t1"
F4_RATE=100      # msg/s, matches the ticket-spec target
F4_SIZE=256      # bytes per message

# Spawn a long-lived broker-driver publisher that hits F4_RATE messages per
# second on F4_TOPIC. The F3 connected-client receiver subscribes to that
# topic, so the broker observes both rxMsgRate (from publisher) and
# txMsgRate (delivered to F3 receiver).
create_sustained_traffic_on() {
    local label="$1"
    local broker_url="$2"
    local broker_letter="$3"
    local pidfile="$BIN_DIR/broker-driver-f4-$broker_letter.pid"
    local logfile="$BIN_DIR/broker-driver-f4-$broker_letter.log"
    log_info "Creating sustained-traffic fixture on $label (rate=$F4_RATE/s topic=$F4_TOPIC) ..."

    nohup ${_SESSION_WRAP:+$_SESSION_WRAP} "$BIN_DIR/broker-driver" publisher \
        --broker="$broker_letter" \
        --vpn="$BROKER_VPN" \
        --topic="$F4_TOPIC" \
        --rate="$F4_RATE" \
        --size="$F4_SIZE" \
        --message-type=persistent \
        --pidfile="$pidfile" \
        >"$logfile" 2>&1 &

    wait_for_pidfile "$pidfile" "$label" "$logfile" "broker-driver publisher" || return 1
    log_info "Sustained-traffic fixture started on $label (PID=$(<"$pidfile"))"
}

cleanup_sustained_traffic_on() {
    local label="$1"
    log_info "Sustained-traffic cleanup on $label deferred to stop_broker_drivers"
}

# F5 slow-consumer constants. A dedicated queue subscribes to F5_TOPIC; the
# broker-driver slow-consumer process publishes into that topic fast while a
# queue-bound receiver ACKs only every F5_ACK_DELAY. F5_MAX_UNACKED caps the
# queue's per-flow delivery window low so txUnackedMsgCount pins near the
# ceiling and spooledMsgCount grows — the queue-level signals SOL-150344
# asserts, replacing the per-client slowSubscriber flag (SOL-150328).
F5_QUEUE="test-queue-slow-consumer"
F5_TOPIC="e2e-monitoring/slow-consumer/topic"
F5_PUBLISH_RATE=100   # msg/s into the queue's topic
F5_PUBLISH_SIZE=256   # bytes per message
F5_ACK_DELAY="2s"     # delay before ACKing each message (the throttle)
F5_MAX_UNACKED=10     # maxDeliveredUnackedMsgsPerFlow on the F5 queue
# txUnackedMsgCount oscillates by one as the slow consumer ACKs, so the
# "pinned near the ceiling" assertion uses 80% of the per-flow cap rather than
# exact equality. Shared by verify-fixtures.sh (SEMP-direct) and
# test-monitoring-tools.sh (MCP-tool) so both layers assert against the same threshold.
F5_NEAR_UNACKED=$(( F5_MAX_UNACKED * 8 / 10 ))

# Provisions F5_QUEUE with a low per-flow unacked window and a subscription to
# F5_TOPIC, then spawns a long-lived broker-driver slow-consumer that floods the
# topic and ACKs slowly. The process self-writes a PID file that
# stop_broker_drivers reaps; the queue is dropped in cleanup (F5 owns it, unlike
# F3/F4 which reuse test-queue).
create_slow_consumer_on() {
    local semp_config="$1"
    local label="$2"
    local broker_url="$3"
    local broker_letter="$4"     # "a" or "b" — resolves SMF host:port in broker-driver
    local pidfile="$BIN_DIR/broker-driver-f5-$broker_letter.pid"
    local logfile="$BIN_DIR/broker-driver-f5-$broker_letter.log"
    log_info "Creating slow-consumer fixture on $label (queue=$F5_QUEUE maxUnacked=$F5_MAX_UNACKED ackDelay=$F5_ACK_DELAY) ..."

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues" \
        "{\"queueName\":\"$F5_QUEUE\",\"accessType\":\"non-exclusive\",\"permission\":\"consume\",\"ingressEnabled\":true,\"egressEnabled\":true,\"maxDeliveredUnackedMsgsPerFlow\":$F5_MAX_UNACKED}" >/dev/null
    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues/$F5_QUEUE/subscriptions" \
        "{\"subscriptionTopic\":\"$F5_TOPIC\"}" >/dev/null

    # nohup + setsid so the driver survives an aborted parent shell; the harness
    # still finds it via the pidfile glob (broker-driver-f*.pid).
    nohup ${_SESSION_WRAP:+$_SESSION_WRAP} "$BIN_DIR/broker-driver" slow-consumer \
        --broker="$broker_letter" \
        --vpn="$BROKER_VPN" \
        --queue="$F5_QUEUE" \
        --topic="$F5_TOPIC" \
        --rate="$F5_PUBLISH_RATE" \
        --size="$F5_PUBLISH_SIZE" \
        --ack-delay="$F5_ACK_DELAY" \
        --pidfile="$pidfile" \
        >"$logfile" 2>&1 &

    wait_for_pidfile "$pidfile" "$label" "$logfile" "broker-driver slow-consumer" || return 1

    verify_monitor_object "$broker_url" "$label" "msgVpns/$BROKER_VPN/queues/$F5_QUEUE"
    log_info "Slow-consumer fixture started on $label (PID=$(<"$pidfile"))"
}

# Drops the F5 queue (cascades its topic subscription). The driver is reaped by
# stop_broker_drivers first, so the bind is gone before the queue delete.
cleanup_slow_consumer_on() {
    local semp_config="$1"
    local label="$2"
    log_info "Cleaning up slow-consumer fixture on $label ..."
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/queues/$F5_QUEUE"
}

# F6 slow-DIRECT-subscriber constants. Distinct from F5 (queue slow-consumer):
# F6 flips the per-client `slowSubscriber` flag that list-slow-subscribers
# filters on. The flag tracks TCP egress back-pressure, which a slow-ACK
# guaranteed consumer never triggers (SOL-150328) — so we close the client's
# receive window at the OS level by SIGSTOPping the subscriber process while a
# separate publisher floods its subscribed topic with large payloads. Direct
# messaging (no queue/spool) so the broker has nowhere to spool the overflow and
# egress congestion is forced onto the stalled client.
F6_SUB_CLIENT_NAME_A="e2e-monitoring-slow-subscriber-a"
F6_SUB_CLIENT_NAME_B="e2e-monitoring-slow-subscriber-b"
F6_TOPIC="e2e-monitoring/slow-subscriber/topic"
F6_FLOOD_RATE=3000     # msg/s — high enough to keep the egress window pinned
F6_FLOOD_SIZE=50000    # bytes — large payloads fill the window fast
F6_FLAG_TIMEOUT=60     # max seconds to wait for slowSubscriber to flip true

# Polls the broker until a client's slowSubscriber flag reads true, or fails
# after F6_FLAG_TIMEOUT. The flag is computed over a rolling ~1 min window, so
# a single read just after SIGSTOP flakes — poll until it settles true.
#
# Self-healing: if the broker reports the client absent (HTTP 400 NOT_FOUND),
# the SIGSTOPped subscriber was reaped by the broker (keepalive timeout or
# egress threshold) — respawn it once and keep polling. The `recreated` flag
# allows exactly one respawn per call to prevent an endless respawn loop; a
# second reap falls back to normal once-per-second polling and ultimately
# times out via the existing log_fail. Respawning resets `attempt` so the new
# subscriber gets its own full F6_FLAG_TIMEOUT budget to flip the flag.
#   $1 broker_url   $2 label   $3 client_name   $4 broker_letter ("a"|"b")
wait_for_slow_subscriber() {
    local broker_url="$1"
    local label="$2"
    local client_name="$3"
    local broker_letter="$4"
    local attempt=0 recreated=0 flag body http_status tx_rate tx_discards prev_observation=""
    local url="$broker_url/SEMP/v2/__private_monitor__/msgVpns/$BROKER_VPN/clients/$client_name?select=slowSubscriber,txByteRate,txDiscardedMsgCount"
    while [ $attempt -lt "$F6_FLAG_TIMEOUT" ]; do
        # http_status drives the self-heal branch below: 400 NOT_FOUND means the
        # broker reaped the client and we must respawn, vs 200/flag=false where
        # the client is present and the flag just decayed.
        # txByteRate + txDiscardedMsgCount disambiguate flag=false further:
        # rate=0 + stable discards = broker drained the egress (real decay);
        # rate>0 or rising discards = broker still delivering/dropping
        # (flag wrong, or just a momentary dip).
        body=$(semp_curl -s -o - -w '\n__HTTP_STATUS__%{http_code}' \
            "$url" 2>/dev/null) || true
        http_status="${body##*__HTTP_STATUS__}"
        body="${body%__HTTP_STATUS__*}"
        # `|| flag=""` so a transient non-JSON body (jq exits non-zero) just
        # retries on the next poll rather than aborting the run under `set -e`.
        flag=$(echo "$body" | jq -r '.data.slowSubscriber // empty' 2>/dev/null) || flag=""
        tx_rate=$(echo "$body" | jq -r '.data.txByteRate // empty' 2>/dev/null) || tx_rate=""
        tx_discards=$(echo "$body" | jq -r '.data.txDiscardedMsgCount // empty' 2>/dev/null) || tx_discards=""
        local observation="http=$http_status flag=${flag:-<empty>} txByteRate=${tx_rate:-<empty>} txDiscardedMsgCount=${tx_discards:-<empty>}"
        if [ "$attempt" = "0" ] || [ "$observation" != "$prev_observation" ]; then
            log_info "  poll [$label/$client_name] t=${attempt}s $observation"
            prev_observation="$observation"
        fi
        if [ "$flag" = "true" ]; then
            log_info "  slowSubscriber=true for $client_name on $label (${attempt}s)"
            return 0
        fi
        # Broker returns 400 NOT_FOUND when the client is gone. The flood
        # publisher is still running, so respawning the SIGSTOPped subscriber
        # alone is enough to re-arm the fixture. Reset the attempt counter so
        # the new subscriber gets its own full timeout budget to flip the flag.
        if [ "$http_status" = "400" ] && [ "$recreated" = "0" ]; then
            recreated=1
            respawn_slow_subscriber_on "$label" "$broker_letter" "$client_name" || return 1
            attempt=0
            prev_observation=""
            continue
        fi
        sleep 1
        attempt=$((attempt + 1))
    done
    log_fail "F6 [$label]: slowSubscriber did not flip true for $client_name within ${F6_FLAG_TIMEOUT}s (last observation: $prev_observation)"
    return 1
}

# Spawns one F6 slow-direct-subscriber driver and waits for its pidfile.
# Shared between the initial fixture create and the post-reap respawn.
#   $1 broker_letter   $2 client_name   $3 pidfile   $4 logfile   $5 label   $6 desc
_spawn_slow_direct_subscriber() {
    local broker_letter="$1" client_name="$2" pidfile="$3" logfile="$4" label="$5" desc="$6"
    nohup ${_SESSION_WRAP:+$_SESSION_WRAP} "$BIN_DIR/broker-driver" slow-direct-subscriber \
        --broker="$broker_letter" \
        --vpn="$BROKER_VPN" \
        --client-name="$client_name" \
        --topic="$F6_TOPIC" \
        --pidfile="$pidfile" \
        >"$logfile" 2>&1 &
    wait_for_pidfile "$pidfile" "$label" "$logfile" "$desc"
}

# Respawns just the SIGSTOPped subscriber half of the F6 fixture after the
# broker has reaped it. The flood publisher is unaffected, so we don't touch
# it (a second publisher with the same client-name would let the broker evict
# the live one, breaking egress pressure). Returns once the new subscriber is
# spawned and SIGSTOPped; the caller (wait_for_slow_subscriber) keeps polling
# for the flag to flip.
respawn_slow_subscriber_on() {
    local label="$1" broker_letter="$2" client_name="$3"
    local sub_pidfile="$BIN_DIR/broker-driver-f6-sub-$broker_letter.pid"
    local sub_logfile="$BIN_DIR/broker-driver-f6-sub-$broker_letter.log"
    log_info "  client $client_name absent on $label (http=400) — broker reaped it; respawning subscriber"
    # The old subscriber is still alive locally (SIGSTOPped, just disconnected
    # from the broker). Reap it BEFORE reusing the pidfile path: the driver
    # has `defer os.Remove(*pidfile)` (see broker-driver/slow_subscriber.go),
    # so a late-exiting old process would otherwise delete the new pidfile we
    # write to the canonical path.
    if [ -f "$sub_pidfile" ]; then
        local old_pid
        old_pid=$(<"$sub_pidfile")
        # Best-effort identity check before signalling: if the PID has been
        # recycled to an unrelated process, /proc/$pid/exe won't resolve to
        # our broker-driver binary. Readlink fails silently for processes we
        # don't own; on mismatch we skip signalling rather than risk killing
        # something else.
        if kill -0 "$old_pid" 2>/dev/null; then
            local exe
            exe=$(readlink "/proc/$old_pid/exe" 2>/dev/null || true)
            if [ -z "$exe" ] || [ "$exe" = "$BIN_DIR/broker-driver" ]; then
                # SIGCONT first so a SIGSTOPped process can actually receive
                # the SIGTERM kill_gracefully sends; otherwise TERM is held
                # pending until CONT and the 5s grace window burns to SIGKILL.
                kill -CONT "$old_pid" 2>/dev/null || true
                kill_gracefully "$old_pid"
            else
                log_warn "  PID $old_pid in $sub_pidfile is not broker-driver (exe=$exe); skipping signal"
            fi
        fi
        rm -f "$sub_pidfile"
    fi
    # Old process is fully exited and the canonical pidfile is gone, so the
    # replacement can write directly onto it — no temp-path + rename dance.
    _spawn_slow_direct_subscriber "$broker_letter" "$client_name" "$sub_pidfile" "$sub_logfile" \
        "$label" "broker-driver slow-direct-subscriber (respawn)" || return 1
    kill -STOP "$(<"$sub_pidfile")"
    log_info "  SIGSTOP sent to respawned slow-subscriber (PID=$(<"$sub_pidfile")) on $label"
}

# Provisions F6: a long-lived direct subscriber on F6_TOPIC plus a separate
# flood publisher into that topic, then SIGSTOPs the subscriber so its TCP
# receive window stalls and the broker flags it slowSubscriber=true. Two PIDs
# (sub + pub) so SIGSTOP halts only the subscriber, never the flood. Both
# pidfiles match the broker-driver-f*.pid glob and are reaped by
# stop_broker_drivers (which SIGCONTs first). No queue — direct messaging only.
create_slow_subscriber_on() {
    local label="$1"
    local broker_url="$2"
    local broker_letter="$3"     # "a" or "b" — resolves SMF host:port in broker-driver
    local client_name="$4"
    local sub_pidfile="$BIN_DIR/broker-driver-f6-sub-$broker_letter.pid"
    local sub_logfile="$BIN_DIR/broker-driver-f6-sub-$broker_letter.log"
    local pub_pidfile="$BIN_DIR/broker-driver-f6-pub-$broker_letter.pid"
    local pub_logfile="$BIN_DIR/broker-driver-f6-pub-$broker_letter.log"
    log_info "Creating slow-subscriber fixture on $label (clientName=$client_name topic=$F6_TOPIC) ..."

    # Direct subscriber (this is the process we SIGSTOP).
    _spawn_slow_direct_subscriber "$broker_letter" "$client_name" "$sub_pidfile" "$sub_logfile" \
        "$label" "broker-driver slow-direct-subscriber" || return 1

    # Separate large-payload flood into the subscribed topic — must NOT be
    # stopped, so it keeps egress pressure on the stalled subscriber.
    nohup ${_SESSION_WRAP:+$_SESSION_WRAP} "$BIN_DIR/broker-driver" publisher \
        --broker="$broker_letter" \
        --vpn="$BROKER_VPN" \
        --client-name="e2e-monitoring-slow-sub-flood-$broker_letter" \
        --topic="$F6_TOPIC" \
        --rate="$F6_FLOOD_RATE" \
        --size="$F6_FLOOD_SIZE" \
        --pidfile="$pub_pidfile" \
        >"$pub_logfile" 2>&1 &
    wait_for_pidfile "$pub_pidfile" "$label" "$pub_logfile" "broker-driver publisher (F6 flood)" || return 1

    # Close the subscriber's TCP receive window at the OS level. A slow app
    # callback is not enough (the client C lib drains the socket regardless);
    # SIGSTOP halts the whole process so the window stays shut.
    kill -STOP "$(<"$sub_pidfile")"
    log_info "  SIGSTOP sent to slow-subscriber (PID=$(<"$sub_pidfile")) on $label"

    # Wait for the broker to observe the stall and flip the flag.
    wait_for_slow_subscriber "$broker_url" "$label" "$client_name" "$broker_letter" || return 1
    log_info "Slow-subscriber fixture created on $label"
}

# Both F6 processes are reaped by stop_broker_drivers via the pidfile glob, and
# it SIGCONTs the stopped subscriber before SIGTERM, so teardown needs nothing
# here. No queue to delete (direct messaging). Kept as a label-only nop for
# symmetry with the other create/cleanup pairs.
cleanup_slow_subscriber_on() {
    local label="$1"
    log_info "Slow-subscriber cleanup on $label deferred to stop_broker_drivers"
}

# F-lowprio constants. A queue with rejectLowPriorityMsgEnabled + a low limit
# and egressEnabled=false latches lowPriorityMsgCongestionState="congested"
# once ≥ F_LOWPRIO_LIMIT priority-0 messages accumulate: no consumer drains
# it, so congestion is sticky. Feeds list-queues.congestedCount. Fresh fixture
# rather than reusing F7's spool queue — single-responsibility per fixture.
F_LOWPRIO_QUEUE="test-queue-lowprio-congestion"
F_LOWPRIO_TOPIC="e2e-monitoring/lowprio/topic"
F_LOWPRIO_LIMIT=5     # rejectLowPriorityMsgLimit; must be exceeded to flip state
F_LOWPRIO_RATE=50     # msg/s; 50/s × 2s = 100 msgs (well past the limit)
F_LOWPRIO_DURATION="2s"
# Counter jq expression used by e2e-llm/read-list-queue-discards to poll the
# fixture's readiness. Kept next to F_LOWPRIO_QUEUE so a broker-side field
# rename or a fixture-name change only requires editing this one block.
F_LOWPRIO_DISCARD_JQ='.data.lowPriorityMsgCongestionDiscardedMsgCount // 0'

# Provisions F_LOWPRIO_QUEUE and runs a one-shot broker-driver publisher with
# --priority=0 --duration=2s. The queue's rejectLowPriorityMsgLimit gates the
# congestion signal; egressEnabled=false ensures messages sit until deleted.
# Waits for the state to flip before returning so downstream assertions can
# read the aggregation without their own poll.
create_lowprio_congestion_on() {
    local semp_config="$1"
    local label="$2"
    local broker_url="$3"
    local broker_letter="$4"
    log_info "Creating lowprio-congestion fixture on $label (queue=$F_LOWPRIO_QUEUE limit=$F_LOWPRIO_LIMIT) ..."

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues" \
        "{\"queueName\":\"$F_LOWPRIO_QUEUE\",\"accessType\":\"non-exclusive\",\"permission\":\"consume\",\"ingressEnabled\":true,\"egressEnabled\":false,\"rejectLowPriorityMsgEnabled\":true,\"rejectLowPriorityMsgLimit\":$F_LOWPRIO_LIMIT}" >/dev/null
    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues/$F_LOWPRIO_QUEUE/subscriptions" \
        "{\"subscriptionTopic\":\"$F_LOWPRIO_TOPIC\"}" >/dev/null

    local pidfile="$BIN_DIR/broker-driver-lowprio-$broker_letter.pid"
    "$BIN_DIR/broker-driver" publisher \
        --broker="$broker_letter" \
        --vpn="$BROKER_VPN" \
        --client-name="e2e-monitoring-lowprio-$broker_letter" \
        --topic="$F_LOWPRIO_TOPIC" \
        --rate="$F_LOWPRIO_RATE" \
        --duration="$F_LOWPRIO_DURATION" \
        --priority=0 \
        --message-type=persistent \
        --pidfile="$pidfile"

    # Predicate poll: the field is "live state", so verify it flipped rather
    # than assuming the publish alone is enough. The downstream summary
    # assertion depends on this — fail the fixture here rather than surface it
    # as a mysterious count mismatch later.
    verify_monitor_object "$broker_url" "$label" \
        "msgVpns/$BROKER_VPN/queues/$F_LOWPRIO_QUEUE" \
        15 '.data.lowPriorityMsgCongestionState == "congested"'

    log_info "Lowprio-congestion fixture created on $label"
}

cleanup_lowprio_congestion_on() {
    local semp_config="$1"
    local label="$2"
    log_info "Cleaning up lowprio-congestion fixture on $label ..."
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/queues/$F_LOWPRIO_QUEUE"
}

# F7-spool constants.
F7_SPOOL_QUEUE="test-queue-discards-spool"
F7_SPOOL_TOPIC="e2e-monitoring/discards/spool"
F7_SPOOL_COUNT=8000   # × 256 B ≈ 2 MB; overflows the 1 MB spool quota
F7_SPOOL_SIZE=256
F7_SPOOL_MAX_MB=1     # maxMsgSpoolUsage in MB
# Counter jq expression shared by verify-fixtures.sh (AC 8 assertion) and
# e2e-llm/read-list-queue-discards (readiness poll). Single source of truth
# for the SEMP field name.
F7_SPOOL_DISCARD_JQ='.data.maxMsgSpoolUsageExceededDiscardedMsgCount // 0'

# Provisions test-queue-discards-spool with a 1 MB spool cap and
# egressEnabled=false, then runs a one-shot publish-batch that fills ~2 MB
# worth of messages. The broker discards the overflow and increments
# maxMsgSpoolUsageExceededDiscardedMsgCount — a cumulative counter, so no
# sustained traffic is needed after the one-shot publish.
create_discard_spool_on() {
    local semp_config="$1"
    local label="$2"
    local broker_url="$3"
    local broker_letter="$4"
    log_info "Creating discard-spool fixture on $label (queue=$F7_SPOOL_QUEUE maxSpool=${F7_SPOOL_MAX_MB}MB) ..."

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues" \
        "{\"queueName\":\"$F7_SPOOL_QUEUE\",\"accessType\":\"non-exclusive\",\"permission\":\"consume\",\"ingressEnabled\":true,\"egressEnabled\":false,\"maxMsgSpoolUsage\":$F7_SPOOL_MAX_MB}" >/dev/null
    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues/$F7_SPOOL_QUEUE/subscriptions" \
        "{\"subscriptionTopic\":\"$F7_SPOOL_TOPIC\"}" >/dev/null

    "$BIN_DIR/broker-driver" publish-batch \
        --broker="$broker_letter" \
        --topic="$F7_SPOOL_TOPIC" \
        --count="$F7_SPOOL_COUNT" \
        --size="$F7_SPOOL_SIZE" \
        --message-type=persistent

    log_info "Discard-spool fixture created on $label"
}

# Drops the F7-spool queue (cascades to its topic subscription).
cleanup_discard_spool_on() {
    local semp_config="$1"
    local label="$2"
    log_info "Cleaning up discard-spool fixture on $label ..."
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/queues/$F7_SPOOL_QUEUE"
}

# F7-ttl constants.
F7_TTL_QUEUE="test-queue-discards-ttl"
F7_TTL_TOPIC="e2e-monitoring/discards/ttl"
F7_TTL_COUNT=200     # small batch; messages expire by TTL, not spool
F7_TTL_SIZE=256
F7_TTL_MAX_TTL_S=1   # maxTtl in seconds
F7_TTL_WAIT_S=2      # sleep after publish to let the 1 s TTL expire
# TTL discards land on one of three counters depending on DMQ resolution;
# sum all three so "expiry happened" is what we assert on. The per-field
# constants are also consumed individually by verify-fixtures.sh's AC 9
# diagnostic so a broker-side rename flips assertion and diagnostic together.
F7_TTL_DISCARDED_JQ='.data.maxTtlExpiredDiscardedMsgCount // 0'
F7_TTL_TO_DMQ_JQ='.data.maxTtlExpiredToDmqMsgCount // 0'
F7_TTL_TO_DMQ_FAILED_JQ='.data.maxTtlExpiredToDmqFailedMsgCount // 0'
F7_TTL_DISCARD_JQ="($F7_TTL_DISCARDED_JQ) + ($F7_TTL_TO_DMQ_JQ) + ($F7_TTL_TO_DMQ_FAILED_JQ)"

# Provisions test-queue-discards-ttl with a 1 s TTL and no consumer, publishes
# a one-shot batch with --dmq-eligible=false so the broker increments
# maxTtlExpiredDiscardedMsgCount rather than moving expired messages to the DMQ.
# Sleeps F7_TTL_WAIT_S after publishing so the TTL window closes before
# verify-fixtures.sh runs the AC 9 assertion.
create_discard_ttl_on() {
    local semp_config="$1"
    local label="$2"
    local broker_letter="$3"
    log_info "Creating discard-ttl fixture on $label (queue=$F7_TTL_QUEUE maxTtl=${F7_TTL_MAX_TTL_S}s) ..."

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues" \
        "{\"queueName\":\"$F7_TTL_QUEUE\",\"accessType\":\"non-exclusive\",\"permission\":\"consume\",\"ingressEnabled\":true,\"egressEnabled\":true,\"maxTtl\":$F7_TTL_MAX_TTL_S,\"respectTtlEnabled\":true}" >/dev/null
    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues/$F7_TTL_QUEUE/subscriptions" \
        "{\"subscriptionTopic\":\"$F7_TTL_TOPIC\"}" >/dev/null

    "$BIN_DIR/broker-driver" publish-batch \
        --broker="$broker_letter" \
        --topic="$F7_TTL_TOPIC" \
        --count="$F7_TTL_COUNT" \
        --size="$F7_TTL_SIZE" \
        --message-type=persistent \
        --dmq-eligible=false

    log_info "Waiting ${F7_TTL_WAIT_S}s for TTL to expire ..."
    sleep "$F7_TTL_WAIT_S"
    log_info "Discard-ttl fixture created on $label"
}

# Drops the F7-ttl queue (cascades to its topic subscription).
cleanup_discard_ttl_on() {
    local semp_config="$1"
    local label="$2"
    log_info "Cleaning up discard-ttl fixture on $label ..."
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/queues/$F7_TTL_QUEUE"
}

# Epoch (seconds since Unix) at which the last F4 publisher finished
# starting. verify-fixtures.sh reads this to wait out the AC 5 settle
# window (≥ 25 s) before sampling rxMsgRate / txMsgRate. Exported so the
# child verifier process inherits it. Use := so sourcing helpers.sh in the
# child verifier does not clobber the value exported by the parent runner.
: "${F4_READY_EPOCH:=}"
export F4_READY_EPOCH

# Epoch at which the F5 slow-consumer drivers finished starting.
# verify-fixtures.sh reads this to wait out the F5 settle window before
# sampling the queue-level signals. Same := guard as F4_READY_EPOCH so a
# child verifier sourcing helpers.sh does not clobber the parent's value.
: "${F5_READY_EPOCH:=}"
export F5_READY_EPOCH

create_fixtures() {
    cleanup_fixtures
    # NFR-4: one-shot SEMP (F1, F2) before client-bearing (F3+) so the
    # queues a receiver binds to are already provisioned.
    create_fixtures_on "$BROKER_A_SEMP_CONFIG" "broker-a" "$BROKER_A_URL"
    create_fixtures_on "$BROKER_B_SEMP_CONFIG" "broker-b" "$BROKER_B_URL"
    create_multi_queue_on "$BROKER_A_SEMP_CONFIG" "broker-a" "$BROKER_A_URL"
    create_multi_queue_on "$BROKER_B_SEMP_CONFIG" "broker-b" "$BROKER_B_URL"
    # Both sides' bridge objects must exist before either side is verified —
    # test-bridge's outboundState depends on the peer's reciprocal bridge.
    create_bridges_on "$BROKER_A_SEMP_CONFIG" "broker-a" "$F8_REMOTE_HOST_FROM_A"
    create_bridges_on "$BROKER_B_SEMP_CONFIG" "broker-b" "$F8_REMOTE_HOST_FROM_B"
    verify_bridges_on "$BROKER_A_URL" "broker-a"
    verify_bridges_on "$BROKER_B_URL" "broker-b"
    create_multi_vpn_on "$BROKER_A_SEMP_CONFIG" "broker-a" "$BROKER_A_URL"
    create_multi_vpn_on "$BROKER_B_SEMP_CONFIG" "broker-b" "$BROKER_B_URL"
    create_empty_enabled_vpn_on "$BROKER_A_SEMP_CONFIG" "broker-a" "$BROKER_A_URL"
    create_empty_enabled_vpn_on "$BROKER_B_SEMP_CONFIG" "broker-b" "$BROKER_B_URL"
    create_connected_client_on "broker-a" "$BROKER_A_URL" a "$F3_CLIENT_NAME_A"
    create_connected_client_on "broker-b" "$BROKER_B_URL" b "$F3_CLIENT_NAME_B"
    create_sustained_traffic_on "broker-a" "$BROKER_A_URL" a
    create_sustained_traffic_on "broker-b" "$BROKER_B_URL" b
    F4_READY_EPOCH=$(date +%s)
    export F4_READY_EPOCH
    # F5 owns its queue and is independent of F3/F4; its consumer binds to that
    # queue so it must be reaped before the queue delete (cleanup handles order).
    create_slow_consumer_on "$BROKER_A_SEMP_CONFIG" "broker-a" "$BROKER_A_URL" a
    create_slow_consumer_on "$BROKER_B_SEMP_CONFIG" "broker-b" "$BROKER_B_URL" b
    F5_READY_EPOCH=$(date +%s)
    export F5_READY_EPOCH
    # F6 is independent of F5; it owns no queue (direct messaging). create_*
    # blocks until the slowSubscriber flag has flipped, so no settle epoch is
    # needed — the tool test can read the flag immediately afterwards.
    create_slow_subscriber_on "broker-a" "$BROKER_A_URL" a "$F6_SUB_CLIENT_NAME_A"
    create_slow_subscriber_on "broker-b" "$BROKER_B_URL" b "$F6_SUB_CLIENT_NAME_B"
    # F7 is independent of F3/F4 — run after them but the queues have no
    # client dependency so order within F7 does not matter.
    create_discard_spool_on "$BROKER_A_SEMP_CONFIG" "broker-a" "$BROKER_A_URL" a
    create_discard_spool_on "$BROKER_B_SEMP_CONFIG" "broker-b" "$BROKER_B_URL" b
    create_discard_ttl_on "$BROKER_A_SEMP_CONFIG" "broker-a" a
    create_discard_ttl_on "$BROKER_B_SEMP_CONFIG" "broker-b" b
    # Lowprio-congestion is independent of F5/F6/F7 and needs no long-lived
    # driver — the one-shot publish fills the queue and congestion holds.
    create_lowprio_congestion_on "$BROKER_A_SEMP_CONFIG" "broker-a" "$BROKER_A_URL" a
    create_lowprio_congestion_on "$BROKER_B_SEMP_CONFIG" "broker-b" "$BROKER_B_URL" b
    # F9/F10 are independent of every fixture above — both wait on the
    # Kafka broker rather than each other or any Solace-side state. Skipped
    # only when this suite's own compose stack declares no "kafka" service
    # (see kafka_expected's comment) — a declared Kafka that fails to come up
    # aborts loudly via wait_for_kafka instead of being silently skipped.
    if kafka_expected; then
        wait_for_kafka
        create_kafka_topics
        create_kafka_receivers_on "$BROKER_A_SEMP_CONFIG" "broker-a"
        create_kafka_receivers_on "$BROKER_B_SEMP_CONFIG" "broker-b"
        verify_kafka_receivers_on "$BROKER_A_URL" "broker-a"
        verify_kafka_receivers_on "$BROKER_B_URL" "broker-b"
        create_kafka_senders_on "$BROKER_A_SEMP_CONFIG" "broker-a"
        create_kafka_senders_on "$BROKER_B_SEMP_CONFIG" "broker-b"
        verify_kafka_senders_on "$BROKER_A_URL" "broker-a"
        verify_kafka_senders_on "$BROKER_B_URL" "broker-b"
    else
        log_warn "Kafka service not declared in this compose stack — skipping F9/F10 Kafka Receiver/Sender fixtures"
    fi
}

cleanup_fixtures() {
    # Reap client-bearing fixtures first — broker refuses to delete a queue
    # while a client is bound, so stop_broker_drivers must run before any
    # SEMP queue/RDP deletes downstream.
    stop_broker_drivers
    cleanup_discard_ttl_on "$BROKER_A_SEMP_CONFIG" "broker-a"
    cleanup_discard_ttl_on "$BROKER_B_SEMP_CONFIG" "broker-b"
    cleanup_discard_spool_on "$BROKER_A_SEMP_CONFIG" "broker-a"
    cleanup_discard_spool_on "$BROKER_B_SEMP_CONFIG" "broker-b"
    cleanup_slow_consumer_on "$BROKER_A_SEMP_CONFIG" "broker-a"
    cleanup_slow_consumer_on "$BROKER_B_SEMP_CONFIG" "broker-b"
    cleanup_slow_subscriber_on "broker-a"
    cleanup_slow_subscriber_on "broker-b"
    cleanup_sustained_traffic_on "broker-a"
    cleanup_sustained_traffic_on "broker-b"
    cleanup_connected_client_on "broker-a"
    cleanup_connected_client_on "broker-b"
    cleanup_lowprio_congestion_on "$BROKER_A_SEMP_CONFIG" "broker-a"
    cleanup_lowprio_congestion_on "$BROKER_B_SEMP_CONFIG" "broker-b"
    cleanup_empty_enabled_vpn_on "$BROKER_A_SEMP_CONFIG" "broker-a"
    cleanup_empty_enabled_vpn_on "$BROKER_B_SEMP_CONFIG" "broker-b"
    cleanup_multi_vpn_on "$BROKER_A_SEMP_CONFIG" "broker-a"
    cleanup_multi_vpn_on "$BROKER_B_SEMP_CONFIG" "broker-b"
    cleanup_bridges_on "$BROKER_A_SEMP_CONFIG" "broker-a"
    cleanup_bridges_on "$BROKER_B_SEMP_CONFIG" "broker-b"
    cleanup_kafka_senders_on "$BROKER_A_SEMP_CONFIG" "broker-a"
    cleanup_kafka_senders_on "$BROKER_B_SEMP_CONFIG" "broker-b"
    cleanup_kafka_receivers_on "$BROKER_A_SEMP_CONFIG" "broker-a"
    cleanup_kafka_receivers_on "$BROKER_B_SEMP_CONFIG" "broker-b"
    cleanup_multi_queue_on "$BROKER_A_SEMP_CONFIG" "broker-a"
    cleanup_multi_queue_on "$BROKER_B_SEMP_CONFIG" "broker-b"
    cleanup_fixtures_on "$BROKER_A_SEMP_CONFIG" "broker-a"
    cleanup_fixtures_on "$BROKER_B_SEMP_CONFIG" "broker-b"
}
