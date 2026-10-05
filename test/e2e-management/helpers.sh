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
CONFIG_QUEUE_NAMES=("e2e-config-queue-broker-a" "e2e-config-queue-broker-b" "e2e-config-iso" "e2e-config-queue-sub-broker-a" "e2e-config-queue-sub-broker-b" "e2e-config-queue-sub-htmlent-broker-a" "e2e-config-queue-sub-htmlent-broker-b" "e2e-config-queue-sub-htmlent-del-broker-a" "e2e-config-queue-sub-htmlent-del-broker-b" "e2e-config-queue-owner-ok-broker-a" "e2e-config-queue-owner-ok-broker-b" "e2e-config-queue-owner-ghost-broker-a" "e2e-config-queue-owner-ghost-broker-b")
CONFIG_TE_NAMES=("e2e-config-te-broker-a" "e2e-config-te-broker-b" "e2e-config-te-owner-ghost-broker-a" "e2e-config-te-owner-ghost-broker-b")
CONFIG_RDP_NAMES=("e2e-config-rdp-broker-a" "e2e-config-rdp-broker-b" "e2e-config-rdp-iso")
# SOL-153080: a real, provisioned client username used as the "owner" fixture
# for the owner-validation tests below. Never connected to — its only purpose
# is to exist, so create-queue/create-topic-endpoint have a real username to
# accept.
CONFIG_CLIENT_USERNAME_NAMES=("e2e-config-owner-broker-a" "e2e-config-owner-broker-b")

# Delete every config fixture on both brokers via the SEMP config API, ignoring
# 404s. Idempotent: safe to call before a run (pre-clean leftover state) and from
# a cleanup trap (post-run / on failure). Queues, topic endpoints, and client
# usernames live in the default VPN; the VPN fixtures are standalone VPNs,
# dropped last. Client usernames are swept after queues/topic endpoints so a
# (bug-induced) queue binding never blocks the username's own deletion.
sweep_config_fixtures() {
    local semp_config name
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
