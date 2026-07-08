#!/usr/bin/env bash
# Bring the test brokers up and wait until they're ready.
# Safe to run multiple times — does nothing if brokers are already up.
#
# Usage: SUITE_DIR=/path/to/suite bash test/e2e-common/setup-brokers.sh
#   or:  bash test/e2e-common/setup-brokers.sh /path/to/suite
#
# SUITE_DIR must point to the suite directory containing docker-compose.yml
# and helpers.sh (which sources lib.sh).

set -euo pipefail

# Accept SUITE_DIR as first arg or from environment
SUITE_DIR="${1:-${SUITE_DIR:-}}"
if [ -z "$SUITE_DIR" ]; then
    echo "Usage: SUITE_DIR=/path/to/suite $0" >&2
    echo "   or: $0 /path/to/suite" >&2
    exit 1
fi
export SUITE_DIR

docker compose -f "$SUITE_DIR/docker-compose.yml" up -d

source "$SUITE_DIR/helpers.sh"
wait_for_all_brokers 120
log_ok "Brokers ready."
