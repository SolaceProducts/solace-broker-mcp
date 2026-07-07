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
CONFIG_QUEUE_NAMES=("e2e-config-queue-broker-a" "e2e-config-queue-broker-b" "e2e-config-iso")
CONFIG_TE_NAMES=("e2e-config-te-broker-a" "e2e-config-te-broker-b")

# Delete every config fixture on both brokers via the SEMP config API, ignoring
# 404s. Idempotent: safe to call before a run (pre-clean leftover state) and from
# a cleanup trap (post-run / on failure). Queues and topic endpoints live in the
# default VPN; the VPN fixtures are standalone VPNs, dropped last.
sweep_config_fixtures() {
    local semp_config name
    for semp_config in "$BROKER_A_SEMP_CONFIG" "$BROKER_B_SEMP_CONFIG"; do
        for name in "${CONFIG_QUEUE_NAMES[@]}"; do
            semp_delete "$semp_config" "msgVpns/$BROKER_VPN/queues/$name"
        done
        for name in "${CONFIG_TE_NAMES[@]}"; do
            semp_delete "$semp_config" "msgVpns/$BROKER_VPN/topicEndpoints/$name"
        done
        for name in "${CONFIG_VPN_NAMES[@]}"; do
            semp_delete "$semp_config" "msgVpns/$name"
        done
    done
}
