#!/usr/bin/env bash
# Standalone e2e tests for /livez, /health, /ready and /readyz endpoints.
# Manages its own docker containers and MCP server — can be run independently
# of the full e2e suite.
#
# Readiness model (SOL-151285): /ready is a body-identical alias of /readyz and
# reflects the MCP server's OWN readiness only — it makes no broker calls. An
# unreachable broker therefore does NOT make /ready (or /readyz) return 503.

set -euo pipefail
source "$(dirname "$0")/../e2e-basic-mcp/helpers.sh"

STARTED_DOCKER=false
SECONDARY_MCP_PORT=9091
SECONDARY_MCP_PID=""
SECONDARY_CONFIG=""

# ── Helpers ───────────────────────────────────────────────────────────────────

# Wait for a TCP port to accept connections. Used to confirm the brokers are up
# before the MCP server starts; /ready no longer dials brokers itself.
wait_for_tcp() {
    local host="$1" port="$2" label="$3"
    local max=30
    local attempt=0
    log_info "Waiting for TCP $label ($host:$port) ..."
    while [ $attempt -lt $max ]; do
        # -z  zero-I/O mode: open the connection and close it immediately without sending data
        # -w 1  give up after 1 second if the port doesn't respond
        # 2>/dev/null  suppress per-attempt "connection refused" noise; log_fail handles the timeout case
        if nc -z -w 1 "$host" "$port" 2>/dev/null; then
            log_info "TCP $label ready after ${attempt}s"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done
    log_fail "TCP $label not ready after ${max}s"
    return 1
}

# Wait until a /readyz-style endpoint returns HTTP 200 before asserting its body.
# start_server() only waits for /health, which returns 200 as soon as the HTTP
# server is up; /readyz (and its /ready alias) returns 503 {"status":"starting"}
# until SetInitialized() runs at the end of startup. Polling here closes that
# brief "starting" window so the readiness assertions don't flake.
wait_for_ready() {
    local url="$1" label="$2"
    local max=20
    local attempt=0
    local http_code body
    while [ $attempt -lt $max ]; do
        http_code=$(curl -s -o /dev/null -w "%{http_code}" "$url")
        if [ "$http_code" = "200" ]; then
            return 0
        fi
        sleep 0.5
        attempt=$((attempt + 1))
    done
    body=$(curl -s "$url")
    log_fail "$label ($url) not ready after $((max / 2))s (last HTTP $http_code, body: $body)"
    return 1
}

cleanup() {
    log_info "Running cleanup ..."
    stop_server
    if [ -n "$SECONDARY_MCP_PID" ]; then
        kill "$SECONDARY_MCP_PID" 2>/dev/null || true
        wait "$SECONDARY_MCP_PID" 2>/dev/null || true
    fi
    if [ -n "$SECONDARY_CONFIG" ]; then
        rm -f "$SECONDARY_CONFIG"
    fi
    rm -f "$CONFIG_FILE"
    if [ "$STARTED_DOCKER" = "true" ]; then
        log_info "Stopping broker containers ..."
        docker compose -f "$SCRIPT_DIR/docker-compose.yml" down --timeout 10 >/dev/null 2>&1 || true
    fi
}

# ── Setup ─────────────────────────────────────────────────────────────────────
CONFIG_FILE=$(mktemp /tmp/e2e-config-XXXXXX.yaml)
trap cleanup EXIT

log_info "=== Health/Readiness E2E Tests ==="
echo ""

# Start broker containers if not already reachable on the configured ports.
# The health test only needs TCP connectivity — it does not perform SEMP operations.
if ! nc -z -w 1 localhost "$BROKER_A_SEMP_PORT" 2>/dev/null || \
   ! nc -z -w 1 localhost "$BROKER_B_SEMP_PORT" 2>/dev/null; then
    log_info "Starting broker containers ..."
    docker compose -f "$SCRIPT_DIR/docker-compose.yml" up -d >/dev/null 2>&1 || \
        { log_fail "Failed to start broker containers"; exit 1; }
    STARTED_DOCKER=true
fi

wait_for_tcp localhost "$BROKER_A_SEMP_PORT" "broker-a"
wait_for_tcp localhost "$BROKER_B_SEMP_PORT" "broker-b"

build_server
write_config "$CONFIG_FILE"
start_server "$CONFIG_FILE"

# ── Tests ─────────────────────────────────────────────────────────────────────

test_livez() {
    local response
    response=$(curl -sf "$MCP_URL/livez")
    assert_json_field "$response" ".status" "alive" "/livez should return status=alive"
}

test_health() {
    # /health is retained for backward compatibility and preserves its ORIGINAL
    # body (status=healthy). It is NOT a body-identical alias of /livez; /livez
    # is the canonical liveness endpoint and returns status=alive.
    local response
    response=$(curl -sf "$MCP_URL/health")
    assert_json_field "$response" ".status" "healthy" "/health should return status=healthy (legacy back-compat body)"
}

test_readyz() {
    # /readyz reflects MCP-server readiness only. Once the server is up it is
    # ready (it makes no broker calls).
    wait_for_ready "$MCP_URL/readyz" "/readyz" || return 1
    local response
    response=$(curl -sf "$MCP_URL/readyz")
    assert_json_field "$response" ".status" "ready" "/readyz should return status=ready"
}

test_ready_aliases_readyz() {
    # /ready is a body-identical alias of /readyz: same status code and body.
    wait_for_ready "$MCP_URL/readyz" "/readyz" || return 1
    local ready_body readyz_body ready_code readyz_code
    ready_body=$(curl -s "$MCP_URL/ready")
    readyz_body=$(curl -s "$MCP_URL/readyz")
    ready_code=$(curl -s -o /dev/null -w "%{http_code}" "$MCP_URL/ready")
    readyz_code=$(curl -s -o /dev/null -w "%{http_code}" "$MCP_URL/readyz")

    assert_json_field "$ready_body" ".status" "ready" "/ready should return status=ready" || return 1
    if [ "$ready_body" != "$readyz_body" ]; then
        log_fail "/ready body ($ready_body) != /readyz body ($readyz_body)"
        return 1
    fi
    if [ "$ready_code" != "$readyz_code" ]; then
        log_fail "/ready HTTP $ready_code != /readyz HTTP $readyz_code"
        return 1
    fi
    log_info "  /ready and /readyz returned identical body and status ($ready_code)"
}

test_ready_decoupled_from_broker() {
    # An unreachable broker MUST NOT make /ready return 503 — readiness is the
    # MCP server's own state, decoupled from broker reachability (SOL-151285).
    SECONDARY_CONFIG=$(mktemp /tmp/e2e-secondary-XXXXXX)
    cat > "$SECONDARY_CONFIG" <<EOF
mcp_client_auth:
  mode: disabled
port: $SECONDARY_MCP_PORT
brokers:
  broker-dead:
    url: "http://localhost:19999"
    auth:
      mode: basic
      username: "admin"
      password: "admin"
EOF

    local existing
    existing=$(lsof -ti:"$SECONDARY_MCP_PORT" 2>/dev/null || true)
    if [ -n "$existing" ]; then
        kill "$existing" 2>/dev/null || true
        sleep 0.5
    fi

    CONFIG_FILE="$SECONDARY_CONFIG" "$BIN_DIR/mcp-server" 2>/dev/null &
    SECONDARY_MCP_PID=$!

    local attempt=0 ready=0
    # 30 attempts × 0.5s = 15s timeout
    while [ $attempt -lt 30 ]; do
        if curl -sf "http://localhost:$SECONDARY_MCP_PORT/health" >/dev/null 2>&1; then
            ready=1; break
        fi
        sleep 0.5
        attempt=$((attempt + 1))
    done

    local result=0
    if [ "$ready" -eq 1 ]; then
        # /health (polled above) is up before SetInitialized() runs, so wait for
        # /ready to leave the "starting" window before asserting it is 200. The
        # explicit assertion below remains the source of truth for pass/fail.
        wait_for_ready "http://localhost:$SECONDARY_MCP_PORT/ready" "/ready (secondary)" || true
        local http_code
        http_code=$(curl -s -o /dev/null -w "%{http_code}" "http://localhost:$SECONDARY_MCP_PORT/ready")
        if [ "$http_code" = "200" ]; then
            log_info "  /ready correctly returned 200 despite unreachable broker — readiness is broker-decoupled"
        else
            log_fail "/ready returned HTTP $http_code, want 200 (readiness must not depend on broker reachability)"
            result=1
        fi
    else
        log_fail "Secondary MCP server failed to start on port $SECONDARY_MCP_PORT"
        result=1
    fi

    kill "$SECONDARY_MCP_PID" 2>/dev/null || true
    wait "$SECONDARY_MCP_PID" 2>/dev/null || true
    SECONDARY_MCP_PID=""
    rm -f "$SECONDARY_CONFIG"
    SECONDARY_CONFIG=""
    return "$result"
}

# ── Run ───────────────────────────────────────────────────────────────────────

run_test "Livez endpoint"                      test_livez
run_test "Health endpoint (legacy back-compat body)"  test_health
run_test "Readyz endpoint (MCP-server readiness)"     test_readyz
run_test "Ready aliases readyz (identical body)"      test_ready_aliases_readyz
run_test "Ready decoupled from broker reachability"   test_ready_decoupled_from_broker

print_summary "Health/Readiness"
