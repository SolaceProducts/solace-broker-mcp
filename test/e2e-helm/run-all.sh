#!/usr/bin/env bash
# e2e-helm suite runner (SOL-156328): installs the Helm chart from this checkout
# into an ephemeral kind cluster, against two real brokers, and drives live MCP
# tool calls through it. Owns the whole lifecycle — cluster, brokers, install,
# tests, teardown — so `make e2e-helm-all` and helm-chart-e2e.yml run the same
# thing. Tears down on every exit, after dumping diagnostics on a failure.

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/helpers.sh"

# Before the EXIT trap: a run refused here must not tear down the other run's
# cluster and brokers.
acquire_run_lock || exit 1

# A random token per run, used for both the install and every client call
# (lib.sh reads MCP_DEV_TOKEN at call time).
MCP_DEV_TOKEN="e2e-helm-$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
export MCP_DEV_TOKEN

on_exit() {
    local rc=$?
    if [ "$rc" -ne 0 ]; then
        dump_diagnostics || true
    fi
    log_info "Tearing down ..."
    teardown
    exit "$rc"
}
trap on_exit EXIT
# Without these a run killed by a signal reaches on_exit with $? == 0: no
# diagnostics, and a zero exit status for an interrupted run.
trap 'exit 130' INT
trap 'exit 143' TERM

# How long the negative control waits for the failing pod to log its error.
# Covers the dev image pull on a fresh node plus one container start.
NEGATIVE_CONTROL_TIMEOUT_S=180

# ── Tests ────────────────────────────────────────────────────────────────────

test_decoy_present_before_install() {
    # Asserted, not assumed: without the decoy the install below proves nothing
    # about the collision bug.
    kc get service "$DECOY_SERVICE" >/dev/null
}

test_service_links_disabled() {
    local pods
    pods=$(kc get pods -l "app.kubernetes.io/instance=$HELM_RELEASE" -o json) || return 1
    assert_json_field "$pods" '(.items | length > 0) and all(.items[]; .spec.enableServiceLinks == false)' "true" \
        "every chart pod must run with enableServiceLinks: false"
}

test_list_brokers() {
    local response content
    response=$(mcp_call_tool "list-brokers" '{}') || return 1
    content=$(extract_content "$response")
    assert_json_field "$content" '.brokers | sort | join(",")' "broker-a,broker-b" \
        "list-brokers must return exactly broker-a and broker-b"
}

# A SEMPv1 call per broker: succeeds only if the pod reached the broker over
# kind's network and authenticated with that broker's credentials.
test_broker_status() {
    local broker="$1"
    local response content
    response=$(mcp_call_tool "get-broker-status" \
        "$(jq -nc --arg b "$broker" '{broker:$b}')") || return 1
    assert_json_field "$response" '.result.isError // false' "false" \
        "get-broker-status [$broker] must not return a tool error" || return 1
    content=$(extract_content "$response")
    assert_json_field "$content" '(.version.description | type) == "string" and (.version.description | contains("Solace"))' "true" \
        "get-broker-status [$broker]: .version.description must be a Solace string" || return 1
    assert_json_field "$content" '.version.uptime.totalSecs > 0' "true" \
        "get-broker-status [$broker]: .version.uptime.totalSecs must be > 0"
}
# Broker A: chart-managed credentials. Broker B: existingSecret.
test_broker_status_a() { test_broker_status "broker-a"; }
test_broker_status_b() { test_broker_status "broker-b"; }

# Negative control: the same install with service links re-enabled must fail
# the way the original bug did. Proves the decoy setup can detect a regression.
test_negative_control() {
    # run_test calls this in an `if`, where set -e is off: check every step.
    enable_service_links || return 1
    log_info "Patched enableServiceLinks: true; waiting up to ${NEGATIVE_CONTROL_TIMEOUT_S}s for the new pod's error"
    local deadline=$((SECONDS + NEGATIVE_CONTROL_TIMEOUT_S)) pod logs
    while [ "$SECONDS" -lt "$deadline" ]; do
        for pod in $(service_link_pods); do
            logs=$(pod_logs "$pod")
            if grep -qF 'invalid MCP_SERVER_PORT' <<<"$logs" && grep -qF 'must be a number' <<<"$logs"; then
                log_info "Pod $pod failed as expected: $(grep -F 'invalid MCP_SERVER_PORT' <<<"$logs" | head -1)"
                return 0
            fi
        done
        sleep 3
    done
    log_fail "No pod with enableServiceLinks: true logged 'invalid MCP_SERVER_PORT ... must be a number' within ${NEGATIVE_CONTROL_TIMEOUT_S}s"
    return 1
}

# ── Main ─────────────────────────────────────────────────────────────────────

log_info "=== Solace Broker MCP — Helm chart E2E (kind) ==="
mkdir -p "$BIN_DIR"
check_helm_deps

# 1. Cluster, with the dev image loaded into it
kind_cluster_up
load_mcp_image

# 2. Brokers, then onto kind's network
bash "$SUITE_DIR/../e2e-common/setup-brokers.sh" "$SUITE_DIR" 9>&-
attach_brokers_to_kind

# 3. Decoy Service, before anything that creates pods
create_namespace_and_decoy

# 4. Broker B's existing Secret, then the chart
create_broker_b_secret
install_chart

# 5. Live tool calls through a port-forward
start_port_forward
run_test "Decoy Service '$DECOY_SERVICE' exists in the namespace" test_decoy_present_before_install
run_test "Chart pods run with enableServiceLinks: false" test_service_links_disabled
run_test "list-brokers returns both brokers" test_list_brokers
run_test "get-broker-status on broker-a (chart-managed credentials)" test_broker_status_a
run_test "get-broker-status on broker-b (existingSecret)" test_broker_status_b

# 6. Negative control — last, since it leaves the Deployment mid-rollout
run_test "Negative control: enableServiceLinks: true fails with invalid MCP_SERVER_PORT" test_negative_control

# 7. Teardown runs from the EXIT trap
print_summary "Helm chart E2E"
