#!/usr/bin/env bash
# Bring the test brokers up and wait until they're ready.
# Safe to run multiple times — does nothing if brokers are already up.
set -euo pipefail
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)

docker compose -f "$SCRIPT_DIR/docker-compose.yml" up -d

source "$SCRIPT_DIR/helpers.sh"
wait_for_all_brokers 120
log_ok "Brokers ready."