#!/usr/bin/env bash
# LLM-suite helpers. Thin wrapper — the LLM suite has its own broker
# containers (./docker-compose.yml, ./.env, ports 8102/8104 SEMP + 55661/55662
# SMF, MCP on 9094) but reuses the monitoring suite's F1–F7 fixture code
# so the scenarios that reference F3 clients, test-vpn, test-queue-3 etc.
# find those objects on our brokers.
#
# The trick: pre-set SUITE_DIR to THIS suite's directory before sourcing
# monitoring/helpers.sh. That file has been made SUITE_DIR-conditional
# (see its comment), so the shared lib (e2e-common/lib.sh) derives
# BIN_DIR / ENV_FILE / broker URLs from our tree, not monitoring's.
#
# fixtures.sh adds the LLM-specific standing objects (e2e-llm-action-queue,
# e2e-llm-kick-target) sourced on top.

set -euo pipefail

SUITE_DIR="${SUITE_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}"
export SUITE_DIR

# Fail loud if the monitoring suite has moved or been renamed. Without this
# check, a missing file would surface as `UNSET_VARS: F3_CLIENT_NAME_A …` in
# every scenario — technically correct but pointing at the wrong root cause.
MONITORING_HELPERS="$SUITE_DIR/../e2e-monitoring/helpers.sh"
if [ ! -f "$MONITORING_HELPERS" ]; then
    echo "[ERROR] monitoring suite helpers not found at $MONITORING_HELPERS" >&2
    echo "[HINT]  the LLM suite reuses F1–F7 fixture code from test/e2e-monitoring/;" >&2
    echo "[HINT]  if that directory was moved/renamed, update SUITE_DIR path here." >&2
    return 1 2>/dev/null || exit 1
fi

# shellcheck source=../e2e-monitoring/helpers.sh
source "$MONITORING_HELPERS"
# shellcheck source=fixtures.sh
source "$SUITE_DIR/fixtures.sh"
