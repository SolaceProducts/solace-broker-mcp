#!/usr/bin/env bash
# e2e-helm suite helpers (SOL-156328). The broker readiness, MCP JSON-RPC wire,
# assertions and test runner come unchanged from the shared library; this file
# adds only what differs from the other suites: the server runs from the Helm
# chart in a kind cluster instead of as a local process.
# Source from test scripts: source "$(dirname "$0")/helpers.sh"

set -euo pipefail

# SUITE_DIR contract (see e2e-common/lib.sh): set our own directory, then source
# the shared library, which derives BIN_DIR/ENV_FILE/REPO_ROOT and .env from it.
SUITE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../e2e-common/lib.sh
source "$SUITE_DIR/../e2e-common/lib.sh"

# lib.sh keeps an MCP_URL the caller exported (for suites that target a
# server elsewhere). This suite's server is only ever its own port-forward, so
# a stray export must not point the probe and the tool calls somewhere else.
MCP_URL="http://127.0.0.1:$MCP_PORT"

CHART_DIR="$REPO_ROOT/deploy/charts/solace-broker-mcp"
COMPOSE_FILE="$SUITE_DIR/docker-compose.yml"
BROKER_A_CONTAINER="solace-e2e-helm-a"
BROKER_B_CONTAINER="solace-e2e-helm-b"
# The Service the decoy reproduces: its name yields MCP_SERVER_PORT, the
# server's own port override, when service links are enabled.
DECOY_SERVICE="mcp-server"
BROKER_B_SECRET="broker-b-credentials"
PORT_FORWARD_PID_FILE="$BIN_DIR/port-forward.pid"
PORT_FORWARD_LOG="$BIN_DIR/port-forward.log"
KUBE_CONTEXT="kind-$KIND_CLUSTER"

# A kubeconfig of the suite's own. kind create/delete otherwise rewrite the
# caller's ~/.kube/config, switching and then unsetting its current-context.
# kind, kubectl and helm all read KUBECONFIG.
mkdir -p "$BIN_DIR"
export KUBECONFIG="$BIN_DIR/kubeconfig"

# Every kubectl and helm call also names the context and namespace, so a run
# can never act on any other cluster.
kc() { kubectl --context "$KUBE_CONTEXT" -n "$HELM_NAMESPACE" "$@"; }
hc() { helm --kube-context "$KUBE_CONTEXT" -n "$HELM_NAMESPACE" "$@"; }

# Host-wide, not per checkout: every worktree and clone shares the cluster
# name, container names and ports, and kind_cluster_up deletes a "leftover"
# cluster. The lock is held on fd 9 until the process exits, so a run killed
# outright releases it too, provided no child outlives it holding fd 9: every
# long-running child (kind create, setup-brokers.sh, helm install, the
# port-forward) is started with 9>&-. The teardown's own children (kind delete,
# docker compose down) are not, so a run killed during its teardown leaves
# that teardown finishing in the background, holding the lock: hence the
# optional wait, which e2e-helm-down uses. Without flock (e.g. macOS) the
# check is skipped.
# Usage: acquire_run_lock [WAIT_SECONDS]  (default: refuse at once)
E2E_HELM_LOCK=/tmp/sol-e2e-helm.lock
acquire_run_lock() {
    local wait_s=${1:-0}
    if ! command -v flock >/dev/null 2>&1; then
        log_warn "flock not found: a concurrent e2e-helm run on this host would not be detected"
        return 0
    fi
    if ! exec 9>>"$E2E_HELM_LOCK"; then
        log_fail "Cannot open $E2E_HELM_LOCK (owned by another user?)"
        return 1
    fi
    if [ "$wait_s" -gt 0 ] && ! flock -n 9; then
        log_info "Waiting up to ${wait_s}s for $E2E_HELM_LOCK"
        flock -w "$wait_s" 9 && return 0
    fi
    if ! flock -n 9; then
        log_fail "Another e2e-helm run holds $E2E_HELM_LOCK (one run per host); not touching its cluster or brokers"
        return 1
    fi
}

check_helm_deps() {
    local absent=0 cmd
    for cmd in docker kind kubectl helm curl jq; do
        if ! command -v "$cmd" >/dev/null 2>&1; then
            log_fail "Required command not found: $cmd"
            absent=1
        fi
    done
    [ "$absent" -eq 0 ] || return 1
    # The dev image is linux/amd64 only (dev-build.yaml).
    if [ "$(uname -m)" != "x86_64" ]; then
        log_warn "Host is $(uname -m): the dev image is linux/amd64 only and is not expected to run here"
    fi
    local kind_version
    kind_version=$(kind version | awk '{print $2}')
    if [ "$kind_version" != "$KIND_VERSION" ]; then
        log_warn "kind is $kind_version, the suite pins $KIND_VERSION (.env); the node image may not match"
    fi
}

# Recreates the cluster from scratch. The name belongs to this suite, so a
# leftover from an interrupted run is deleted rather than reused: a stale
# release or decoy in it would change what the run proves.
kind_cluster_up() {
    if kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER"; then
        log_info "Deleting leftover kind cluster $KIND_CLUSTER"
        kind delete cluster --name "$KIND_CLUSTER"
    fi
    log_info "Creating kind cluster $KIND_CLUSTER ($KIND_NODE_IMAGE)"
    kind create cluster --name "$KIND_CLUSTER" --image "$KIND_NODE_IMAGE" --wait 120s 9>&-
}

# Pulls the dev image on the host, with retries, and loads it into the kind
# node, so the chart's pods start from a local image (pullPolicy IfNotPresent)
# rather than pulling from ghcr inside the node. A pull inside the node was
# seen to time out (read: connection timed out) while the same pull on the
# host took seconds, and kubelet's backoff then ran out the 5-minute install
# budget. Here a failed pull is retried and shows in the step log.
load_mcp_image() {
    local image="$MCP_IMAGE_REPOSITORY:$MCP_IMAGE_TAG" attempt
    for attempt in 1 2 3; do
        if docker pull "$image" 9>&-; then
            break
        fi
        if [ "$attempt" -eq 3 ]; then
            log_fail "Could not pull $image after 3 attempts"
            return 1
        fi
        log_warn "Pull of $image failed (attempt $attempt of 3); retrying in 10s"
        sleep 10 9>&-
    done
    kind load docker-image "$image" --name "$KIND_CLUSTER" 9>&-
}

# Puts both broker containers on kind's Docker network. Idempotent.
attach_brokers_to_kind() {
    local c
    for c in "$BROKER_A_CONTAINER" "$BROKER_B_CONTAINER"; do
        if docker inspect -f '{{json .NetworkSettings.Networks}}' "$c" | jq -e 'has("kind")' >/dev/null; then
            continue
        fi
        docker network connect kind "$c"
        log_info "Attached $c to the kind network"
    done
}

# Prints a broker container's IPv4 address on kind's network. Pods reach the
# brokers by IP: a pod's lookup of a broker container name returned NXDOMAIN on
# kind v0.33.0 (verified; cause not established — kind routes pod DNS towards
# Docker's embedded DNS, yet the lookup failed).
broker_kind_ip() {
    local ip
    ip=$(docker inspect -f '{{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}' "$1")
    if [ -z "$ip" ]; then
        log_fail "$1 has no IPv4 address on the kind network"
        return 1
    fi
    echo "$ip"
}

# The pre-install state the collision bug needs: Kubernetes injects
# <SVC>_PORT only for Services that exist when a pod is created, so the
# decoy must exist before the chart's pods do.
create_namespace_and_decoy() {
    kubectl --context "$KUBE_CONTEXT" create namespace "$HELM_NAMESPACE"
    kc create service clusterip "$DECOY_SERVICE" --tcp=9090:9090
}

# Broker B's credentials come from a Secret the chart does not manage, under
# the keys the chart documents for basic auth: broker-b's own chart user, which
# broker-a does not have. The password reaches jq through its environment and
# kubectl on stdin, never through argv.
create_broker_b_secret() {
    CHART_B_PASSWORD="$CHART_B_PASSWORD" \
        jq -n --arg n "$BROKER_B_SECRET" --arg ns "$HELM_NAMESPACE" --arg u "$CHART_B_USERNAME" \
        '{apiVersion:"v1",kind:"Secret",type:"Opaque",
          metadata:{name:$n,namespace:$ns},
          stringData:{username:$u,password:env.CHART_B_PASSWORD}}' \
        | kc apply -f - >/dev/null
}

# Installs the chart from the checked-out tree. broker-a's credentials are
# chart-managed, broker-b's come from the existing Secret. Each is a user only
# its own broker has (docker-compose.yml), so credentials routed to the wrong
# broker fail authentication instead of passing on a shared admin login.
# Values go through a 0600 file rather than --set, and the password and token
# reach jq through its environment, so neither appears in argv. The semp
# retry budget is cut to fail fast, as e2e-basic-mcp does: at the default (10
# retries, 3s to 30s backoff) one unreachable broker holds a tool call for
# minutes, long enough to push the run into the job timeout, which kills it
# before the diagnostics dump.
install_chart() {
    local values ip_a ip_b
    ip_a=$(broker_kind_ip "$BROKER_A_CONTAINER") || return 1
    ip_b=$(broker_kind_ip "$BROKER_B_CONTAINER") || return 1
    values=$(mktemp "$BIN_DIR/values-XXXXXX.yaml")
    chmod 600 "$values"
    CHART_A_PASSWORD="$CHART_A_PASSWORD" MCP_DEV_TOKEN="$MCP_DEV_TOKEN" jq -n \
        --arg repo "$MCP_IMAGE_REPOSITORY" --arg tag "$MCP_IMAGE_TAG" \
        --arg urlA "http://$ip_a:8080" --arg urlB "http://$ip_b:8080" \
        --arg user "$CHART_A_USERNAME" --arg secretB "$BROKER_B_SECRET" \
        '{image:{repository:$repo,tag:$tag,pullPolicy:"IfNotPresent"},
          brokers:{
            "broker-a":{url:$urlA,auth:{mode:"basic",username:$user,password:env.CHART_A_PASSWORD}},
            "broker-b":{url:$urlB,auth:{mode:"basic",existingSecret:$secretB}}},
          mcpClientAuth:{mode:"static",devToken:env.MCP_DEV_TOKEN},
          semp:{retries:2,retryMinInterval:"500ms",retryMaxInterval:"1s"}}' >"$values"
    # JSON is valid YAML, so helm reads the file as is.
    local rc=0
    hc install "$HELM_RELEASE" "$CHART_DIR" -f "$values" --wait --timeout 5m 9>&- || rc=$?
    rm -f "$values"
    return "$rc"
}

deployment_name() {
    kc get deployment -l "app.kubernetes.io/instance=$HELM_RELEASE" -o jsonpath='{.items[0].metadata.name}'
}

service_name() {
    kc get service -l "app.kubernetes.io/instance=$HELM_RELEASE" -o jsonpath='{.items[0].metadata.name}'
}

# Forwards MCP_PORT on the host to the chart's Service and waits for /readyz.
# A Service port-forward pins one pod, which is what MCP's in-memory sessions
# need. kubectl is started directly, not through kc, so $! is kubectl's own PID
# rather than a subshell's. It binds 127.0.0.1 only, the address MCP_URL uses,
# and readiness requires the process to be alive and to have logged its bind
# line for exactly that address: if something else already holds the port,
# kubectl exits (or, without --address, would fall back to [::1] alone) and the
# other listener must not pass for the chart. 9>&-: see acquire_run_lock.
start_port_forward() {
    local svc pid
    svc=$(service_name)
    kubectl --context "$KUBE_CONTEXT" -n "$HELM_NAMESPACE" \
        port-forward --address 127.0.0.1 "service/$svc" "$MCP_PORT:http" \
        >"$PORT_FORWARD_LOG" 2>&1 9>&- &
    pid=$!
    echo "$pid" >"$PORT_FORWARD_PID_FILE"
    local _
    for _ in $(seq 1 30); do
        if ! kill -0 "$pid" 2>/dev/null; then
            log_fail "kubectl port-forward exited"
            cat "$PORT_FORWARD_LOG" >&2 || true
            return 1
        fi
        if grep -qF "Forwarding from 127.0.0.1:$MCP_PORT " "$PORT_FORWARD_LOG" \
            && curl -sf "$MCP_URL/readyz" >/dev/null 2>&1; then
            log_info "Port-forward to service/$svc ready on $MCP_URL"
            return 0
        fi
        sleep 1
    done
    log_fail "Port-forward to service/$svc not ready within 30s"
    cat "$PORT_FORWARD_LOG" >&2 || true
    return 1
}

# Kills the recorded port-forward only if that PID is still this suite's
# kubectl: after a run killed outright (kill -9, reboot) the PID file survives,
# and the PID may since belong to an unrelated process.
stop_port_forward() {
    if [ -f "$PORT_FORWARD_PID_FILE" ]; then
        local pid args
        pid=$(cat "$PORT_FORWARD_PID_FILE")
        args=$(ps -p "$pid" -o args= 2>/dev/null || true)
        if [[ "$args" == *port-forward* && "$args" == *"$KUBE_CONTEXT"* ]]; then
            kill "$pid" 2>/dev/null || true
        fi
        rm -f "$PORT_FORWARD_PID_FILE"
    fi
}

# Negative control: re-enables service links on the Deployment. Done with
# kubectl patch, not a chart value, so users cannot re-enable the bug through
# values. maxUnavailable: 0 keeps the old pods serving while the new one fails.
enable_service_links() {
    kc patch deployment "$(deployment_name)" --type=json \
        -p '[{"op":"replace","path":"/spec/template/spec/enableServiceLinks","value":true}]' >/dev/null
}

# Prints the names of the release's pods whose spec has service links enabled,
# i.e. the pods the negative control created.
service_link_pods() {
    kc get pods -l "app.kubernetes.io/instance=$HELM_RELEASE" -o json \
        | jq -r '.items[] | select(.spec.enableServiceLinks == true) | .metadata.name'
}

# Prints the current and previous container logs of the given pod. Either may
# be absent depending on where in its crash loop the pod is.
pod_logs() {
    local pod="$1"
    kc logs "$pod" 2>/dev/null || true
    kc logs "$pod" --previous 2>/dev/null || true
}

dump_diagnostics() {
    log_warn "Dumping diagnostics"
    kc get all,secrets,configmaps -o wide 2>&1 || true
    kc describe deployment -l "app.kubernetes.io/instance=$HELM_RELEASE" 2>&1 || true
    kc describe pods -l "app.kubernetes.io/instance=$HELM_RELEASE" 2>&1 || true
    local pod
    for pod in $(kc get pods -l "app.kubernetes.io/instance=$HELM_RELEASE" -o name 2>/dev/null); do
        echo "── logs: $pod"
        pod_logs "${pod#pod/}" | tail -100
    done
    kc get events --sort-by=.lastTimestamp 2>&1 | tail -50 || true
    echo "── port-forward log"
    cat "$PORT_FORWARD_LOG" 2>/dev/null || true
    local c
    for c in "$BROKER_A_CONTAINER" "$BROKER_B_CONTAINER"; do
        echo "── logs: $c"
        docker logs "$c" 2>&1 | tail -100 || true
    done
}

# Removes everything the suite creates. Safe to run at any point, including
# when nothing exists yet.
teardown() {
    stop_port_forward
    # install_chart removes its values file on return; a signal during the
    # install skips that.
    rm -f "$BIN_DIR"/values-*.yaml
    if kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER"; then
        kind delete cluster --name "$KIND_CLUSTER" || log_warn "kind delete cluster failed"
    fi
    docker compose -f "$COMPOSE_FILE" down -v || log_warn "docker compose down failed"
}
