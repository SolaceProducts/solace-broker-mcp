#!/usr/bin/env bash
# Orchestrator for MCP tool validation against a non-trivially configured
# Solace Enterprise broker.
#
# Usage:
#   ./run.sh              # full run: setup -> validate -> cleanup
#   ./run.sh setup        # only provision broker + start clients
#   ./run.sh validate     # only run validation tests (assumes setup done)
#   ./run.sh cleanup      # only tear down fixtures + stop clients
#
# Prerequisites:
#   - Docker running with solace-validation container up
#   - terraform CLI available
#   - ~/pubSubTools/sdkperf_c available (optional, for client tests)
#   - Go toolchain for building MCP server

set -euo pipefail
source "$(dirname "$0")/helpers.sh"

TERRAFORM_DIR="$SCRIPT_DIR/terraform"
CONFIG_FILE="$SCRIPT_DIR/broker-config.yaml"

# ── Setup ────────────────────────────────────────────────────────────────────

do_setup() {
    log_info "═══ Phase 1: Wait for broker ═══"
    wait_for_broker "$BROKER_URL"

    log_info "═══ Phase 2: Terraform apply ═══"
    (cd "$TERRAFORM_DIR" && terraform init -input=false && terraform apply -auto-approve -input=false)

    log_info "═══ Phase 3: Publish messages ═══"
    # Wait a moment for REST messaging service to be ready after VPN config update
    sleep 3

    publish_messages "backlog" 5
    publish_messages "load" 50
    publish_messages "stuck" 10

    log_info "═══ Phase 4: Start sdkperf clients ═══"
    rm -f "$SDKPERF_PID_FILE"

    start_sdkperf_subscriber "topic-sub-1" \
        "sensor/temperature/>,sensor/humidity/>,alerts/critical" || true

    start_sdkperf_subscriber "topic-sub-2" \
        "orders/new,orders/cancel,orders/status" || true

    start_sdkperf_queue_consumer "queue-consumer" \
        "val-q-with-consumer" || true

    # Let clients connect and subscribe
    sleep 3
    log_info "Setup complete"
}

# ── Validate ─────────────────────────────────────────────────────────────────

do_validate() {
    log_info "═══ Phase 5: Build + start MCP server ═══"
    build_server
    write_config "$CONFIG_FILE"
    start_server "$CONFIG_FILE"

    log_info "═══ Phase 6: Run validation tests ═══"
    # Source validate.sh to run all tests in this process (shares helpers)
    source "$SCRIPT_DIR/validate.sh"
}

# ── Cleanup ──────────────────────────────────────────────────────────────────

do_cleanup() {
    log_info "═══ Cleanup ═══"

    stop_server || true
    stop_all_sdkperf || true

    if [ -d "$TERRAFORM_DIR/.terraform" ]; then
        log_info "Running terraform destroy ..."
        (cd "$TERRAFORM_DIR" && terraform destroy -auto-approve -input=false) || true
    fi

    rm -f "$CONFIG_FILE"
    log_info "Cleanup complete"
}

# ── Main ─────────────────────────────────────────────────────────────────────

# Trap to ensure cleanup on exit (unless running a single phase)
MODE="${1:-all}"

case "$MODE" in
    setup)
        do_setup
        ;;
    validate)
        do_validate
        ;;
    cleanup)
        do_cleanup
        ;;
    all)
        trap do_cleanup EXIT
        do_setup
        do_validate
        ;;
    *)
        echo "Usage: $0 [setup|validate|cleanup|all]"
        exit 1
        ;;
esac
