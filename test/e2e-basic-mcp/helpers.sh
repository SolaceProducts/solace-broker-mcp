#!/usr/bin/env bash
# Basic-MCP suite helpers. The generic scaffold (broker readiness, MCP server
# lifecycle, config generation, SEMP ops, base broker fixtures, MCP wire,
# assertions, test runner) lives in the shared library. The protocol scenarios
# exercise exactly that base set, so this file adds only the two per-broker
# orchestrators below.
# Source from test scripts: source "$(dirname "$0")/helpers.sh"

set -euo pipefail

# SUITE_DIR contract (see e2e-common/lib.sh): set our own directory, then source
# the shared library, which derives BIN_DIR/ENV_FILE/REPO_ROOT and .env from it.
SUITE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../e2e-common/lib.sh
source "$SUITE_DIR/../e2e-common/lib.sh"

create_fixtures() {
    cleanup_fixtures
    create_fixtures_on "$BROKER_A_SEMP_CONFIG" "broker-a" "$BROKER_A_URL"
    create_fixtures_on "$BROKER_B_SEMP_CONFIG" "broker-b" "$BROKER_B_URL"
}

cleanup_fixtures() {
    cleanup_fixtures_on "$BROKER_A_SEMP_CONFIG" "broker-a"
    cleanup_fixtures_on "$BROKER_B_SEMP_CONFIG" "broker-b"
}
