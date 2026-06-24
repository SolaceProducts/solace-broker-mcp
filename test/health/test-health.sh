#!/usr/bin/env bash
# Standalone e2e tests for /health and /ready endpoints.
# Manages its own docker containers and MCP server — can be run independently
# of the full e2e suite.

set -euo pipefail
source "$(dirname "$0")/../e2e-basic-mcp/helpers.sh"

STARTED_DOCKER=false
SECONDARY_MCP_PORT=9091
SECONDARY_MCP_PID=""
SECONDARY_CONFIG=""

# ── Helpers ───────────────────────────────────────────────────────────────────

# Wait for a TCP port to accept connections — mirrors exactly what /ready uses internally.
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

test_health() {
    local response
    response=$(curl -sf "$MCP_URL/health")
    assert_json_field "$response" ".status" "healthy" "/health should return status=healthy"
}

test_ready_both_reachable() {
    local response
    response=$(curl -sf "$MCP_URL/ready")
    assert_json_field "$response" ".ready" "true" "/ready should return ready=true" || return 1
    assert_contains "$response" "broker-a" "/ready should list broker-a" || return 1
    assert_contains "$response" "broker-b" "/ready should list broker-b" || return 1
}

test_ready_unreachable_broker() {
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
        local http_code
        http_code=$(curl -s -o /dev/null -w "%{http_code}" "http://localhost:$SECONDARY_MCP_PORT/ready")
        if [ "$http_code" = "503" ]; then
            log_info "  /ready correctly returned 503 — TCP dial to localhost:19999 failed as expected"
        else
            log_fail "/ready returned HTTP $http_code, want 503 for unreachable broker"
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

run_test "Health endpoint"                     test_health
run_test "Ready endpoint (brokers reachable)"  test_ready_both_reachable
run_test "Ready endpoint (broker unreachable)" test_ready_unreachable_broker

print_summary "Health/Readiness"
