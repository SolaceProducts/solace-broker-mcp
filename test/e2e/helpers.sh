#!/usr/bin/env bash
# Shared helper functions for E2E tests.
# Source this file from test scripts: source "$(dirname "$0")/helpers.sh"

set -euo pipefail

# ── Paths ────────────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
BIN_DIR="$SCRIPT_DIR/bin"

# ── Broker settings (all values from .env) ──────────────────────────────────
# shellcheck source=.env
source "$SCRIPT_DIR/.env"

BROKER_A_URL="http://localhost:${BROKER_A_SEMP_PORT}"
BROKER_B_URL="http://localhost:${BROKER_B_SEMP_PORT}"
BROKER_USER="${BROKER_USERNAME}"
BROKER_PASS="${BROKER_PASSWORD}"
BROKER_VPN="default"
BROKER_A_SEMP_CONFIG="$BROKER_A_URL/SEMP/v2/config"
BROKER_B_SEMP_CONFIG="$BROKER_B_URL/SEMP/v2/config"

# Default BROKER_URL / SEMP_CONFIG point to broker A (used by single-broker helpers)
BROKER_URL="$BROKER_A_URL"
SEMP_CONFIG="$BROKER_A_SEMP_CONFIG"

# ── MCP server settings ─────────────────────────────────────────────────────
MCP_PORT=9090
MCP_URL="http://localhost:$MCP_PORT"
MCP_SERVER_PID=""

# ── Colors ───────────────────────────────────────────────────────────────────
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
CYAN='\033[0;36m'
NC='\033[0m'

# ── Test counters ────────────────────────────────────────────────────────────
TESTS_RUN=0
TESTS_PASSED=0
TESTS_FAILED=0

# ── Logging ──────────────────────────────────────────────────────────────────

log_info()  { echo -e "${CYAN}[INFO]${NC}  $*"; }
log_ok()    { echo -e "${GREEN}[PASS]${NC}  $*"; }
log_fail()  { echo -e "${RED}[FAIL]${NC}  $*"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }

# ── Broker Readiness ─────────────────────────────────────────────────────────

wait_for_broker() {
    local broker_url="${1:-$BROKER_URL}"
    local max_attempts="${2:-60}"
    local semp_config="$broker_url/SEMP/v2/config"
    local attempt=0
    log_info "Waiting for Solace broker at $broker_url ..."

    # Phase 1: Wait for SEMP API to respond
    while [ $attempt -lt "$max_attempts" ]; do
        if curl -sf -u "$BROKER_USER:$BROKER_PASS" \
            "$semp_config/msgVpns/$BROKER_VPN" >/dev/null 2>&1; then
            break
        fi
        sleep 1
        attempt=$((attempt + 1))
    done
    if [ $attempt -ge "$max_attempts" ]; then
        log_fail "Broker SEMP API not ready after ${max_attempts}s ($broker_url)"
        return 1
    fi
    log_info "SEMP API responding after ${attempt}s ($broker_url)"

    # Phase 2: Wait for message spool to accept queue operations.
    # The monitor API may report spool fields before the spool is actually writable,
    # so we probe with a real queue create/delete to confirm readiness.
    while [ $attempt -lt "$max_attempts" ]; do
        local http_code
        http_code=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
            -u "$BROKER_USER:$BROKER_PASS" \
            -H "Content-Type: application/json" \
            "$semp_config/msgVpns/$BROKER_VPN/queues" \
            -d '{"queueName":"_e2e_spool_probe_","accessType":"non-exclusive"}')
        if [ "$http_code" -ge 200 ] && [ "$http_code" -lt 300 ]; then
            # Clean up probe queue
            curl -sf -X DELETE -u "$BROKER_USER:$BROKER_PASS" \
                "$semp_config/msgVpns/$BROKER_VPN/queues/_e2e_spool_probe_" >/dev/null 2>&1 || true
            log_info "Broker fully ready after ${attempt}s (message spool writable) ($broker_url)"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done
    log_fail "Broker message spool not ready after ${max_attempts}s ($broker_url)"
    return 1
}

wait_for_all_brokers() {
    local max_attempts="${1:-90}"
    wait_for_broker "$BROKER_A_URL" "$max_attempts"
    wait_for_broker "$BROKER_B_URL" "$max_attempts"
}

# ── MCP Server ───────────────────────────────────────────────────────────────

build_server() {
    log_info "Building MCP server binary ..."
    (cd "$REPO_ROOT" && go build -o "$BIN_DIR/mcp-server" ./cmd/server)
    log_info "Server binary built: $BIN_DIR/mcp-server"
}

start_server() {
    local config_file="$1"
    log_info "Starting MCP server (config=$config_file, port=$MCP_PORT) ..."

    # Kill any leftover process on the MCP port
    local existing_pid
    existing_pid=$(lsof -ti:"$MCP_PORT" 2>/dev/null || true)
    if [ -n "$existing_pid" ]; then
        log_warn "Killing existing process on port $MCP_PORT (PID=$existing_pid)"
        kill "$existing_pid" 2>/dev/null || true
        sleep 1
    fi

    # Credentials come from .env (already sourced): E2E_A_USERNAME, E2E_A_PASSWORD, etc.
    # ENV_FILE tells the MCP server to also load .env for credential resolution.
    CONFIG_FILE="$config_file" \
    ENV_FILE="$SCRIPT_DIR/.env" \
        "$BIN_DIR/mcp-server" 2>/dev/null &
    MCP_SERVER_PID=$!

    # Wait for server to be ready
    local attempt=0
    while [ $attempt -lt 30 ]; do
        if curl -sf "$MCP_URL/health" >/dev/null 2>&1; then
            log_info "MCP server ready (PID=$MCP_SERVER_PID)"
            return 0
        fi
        sleep 0.5
        attempt=$((attempt + 1))
    done
    log_fail "MCP server failed to start"
    return 1
}

stop_server() {
    if [ -n "$MCP_SERVER_PID" ] && kill -0 "$MCP_SERVER_PID" 2>/dev/null; then
        log_info "Stopping MCP server (PID=$MCP_SERVER_PID) ..."
        kill "$MCP_SERVER_PID" 2>/dev/null || true
        wait "$MCP_SERVER_PID" 2>/dev/null || true
        MCP_SERVER_PID=""
    fi
}

write_config() {
    local config_file="$1"
    # Generate broker config from .env-derived values so ports stay in sync.
    # Credentials use ${VAR_NAME} substitution — resolved by the server via ENV_FILE.
    cat > "$config_file" <<EOF
development_mode: true

brokers:
  broker-a:
    url: "${BROKER_A_URL}"
    auth:
      mode: basic
      username: "\${E2E_A_USERNAME}"
      password: "\${E2E_A_PASSWORD}"
  broker-b:
    url: "${BROKER_B_URL}"
    auth:
      mode: basic
      username: "\${E2E_B_USERNAME}"
      password: "\${E2E_B_PASSWORD}"
EOF
    log_info "Config written to $config_file (broker-a=$BROKER_A_URL, broker-b=$BROKER_B_URL)"
}

# ── Broker Fixtures ──────────────────────────────────────────────────────────

semp_post() {
    local semp_config="$1"
    local path="$2"
    local data="$3"
    local response
    response=$(curl -s -o /dev/null -w "%{http_code}" -X POST -u "$BROKER_USER:$BROKER_PASS" \
        -H "Content-Type: application/json" \
        "$semp_config/$path" -d "$data")
    if [ "$response" -ge 200 ] && [ "$response" -lt 300 ]; then
        return 0
    else
        log_fail "SEMP POST $path returned HTTP $response"
        curl -s -X POST -u "$BROKER_USER:$BROKER_PASS" \
            -H "Content-Type: application/json" \
            "$semp_config/$path" -d "$data" >&2
        return 1
    fi
}

semp_delete() {
    local semp_config="$1"
    local path="$2"
    curl -sf -X DELETE -u "$BROKER_USER:$BROKER_PASS" \
        "$semp_config/$path" >/dev/null 2>&1 || true
}

create_fixtures_on() {
    local semp_config="$1"
    local label="$2"
    local broker_url="$3"
    log_info "Creating fixtures on $label ..."

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues" \
        '{"queueName":"test-queue","accessType":"non-exclusive","permission":"consume","ingressEnabled":true,"egressEnabled":true}' >/dev/null

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/restDeliveryPoints" \
        '{"restDeliveryPointName":"test-rdp","enabled":false}' >/dev/null

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/restDeliveryPoints/test-rdp/restConsumers" \
        '{"restConsumerName":"test-consumer","remoteHost":"localhost","remotePort":8888,"tlsEnabled":false,"enabled":false}' >/dev/null

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/restDeliveryPoints/test-rdp/queueBindings" \
        '{"queueBindingName":"test-queue","postRequestTarget":"/test"}' >/dev/null

    # Verify fixtures are visible via the monitor API before proceeding.
    # The private monitor endpoint can lag behind the config API.
    verify_fixtures "$broker_url" "$label"

    log_info "Fixtures created on $label"
}

verify_fixtures() {
    local broker_url="$1"
    local label="$2"
    local monitor="$broker_url/SEMP/v2/__private_monitor__"
    local max_attempts=30
    local attempt=0

    log_info "Verifying fixtures visible on $label ..."
    while [ $attempt -lt $max_attempts ]; do
        if curl -sf -u "$BROKER_USER:$BROKER_PASS" \
            "$monitor/msgVpns/$BROKER_VPN/queues/test-queue" >/dev/null 2>&1 && \
           curl -sf -u "$BROKER_USER:$BROKER_PASS" \
            "$monitor/msgVpns/$BROKER_VPN/restDeliveryPoints/test-rdp" >/dev/null 2>&1; then
            log_info "Fixtures verified on $label (${attempt}s)"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done
    log_warn "Fixtures not yet visible via monitor API on $label after ${max_attempts}s — proceeding anyway"
}

cleanup_fixtures_on() {
    local semp_config="$1"
    local label="$2"
    log_info "Cleaning up fixtures on $label ..."
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/restDeliveryPoints/test-rdp/queueBindings/test-queue"
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/restDeliveryPoints/test-rdp/restConsumers/test-consumer"
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/restDeliveryPoints/test-rdp"
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/queues/test-queue"
    log_info "Fixtures cleaned up on $label"
}

create_fixtures() {
    cleanup_fixtures
    create_fixtures_on "$BROKER_A_SEMP_CONFIG" "broker-a" "$BROKER_A_URL"
    create_fixtures_on "$BROKER_B_SEMP_CONFIG" "broker-b" "$BROKER_B_URL"
}

cleanup_fixtures() {
    cleanup_fixtures_on "$BROKER_A_SEMP_CONFIG" "broker-a"
    cleanup_fixtures_on "$BROKER_B_SEMP_CONFIG" "broker-b"
}

# ── MCP Protocol Helpers ─────────────────────────────────────────────────────

# Performs the MCP initialize handshake. Returns the Mcp-Session-Id.
mcp_initialize() {
    local response
    response=$(curl -sf -D - -X POST "$MCP_URL/mcp" \
        -H "Content-Type: application/json" \
        -H "Accept: application/json, text/event-stream" \
        -d '{
            "jsonrpc": "2.0",
            "id": 1,
            "method": "initialize",
            "params": {
                "protocolVersion": "2025-03-26",
                "capabilities": {},
                "clientInfo": { "name": "e2e-test", "version": "1.0.0" }
            }
        }')

    local session_id
    session_id=$(echo "$response" | grep -i 'mcp-session-id' | tr -d '\r' | awk -F': ' '{print $2}')

    if [ -z "$session_id" ]; then
        log_fail "No Mcp-Session-Id in initialize response"
        echo "$response" >&2
        return 1
    fi

    # Send initialized notification
    curl -sf -X POST "$MCP_URL/mcp" \
        -H "Content-Type: application/json" \
        -H "Accept: application/json, text/event-stream" \
        -H "Mcp-Session-Id: $session_id" \
        -d '{
            "jsonrpc": "2.0",
            "method": "notifications/initialized"
        }' >/dev/null 2>&1 || true

    echo "$session_id"
}

# Sends an MCP request with the given session ID. Returns the JSON response body.
# The server responds with SSE (text/event-stream), so we extract the data: line.
mcp_request() {
    local session_id="$1"
    local body="$2"

    local raw
    raw=$(curl -s -X POST "$MCP_URL/mcp" \
        -H "Content-Type: application/json" \
        -H "Accept: application/json, text/event-stream" \
        -H "Mcp-Session-Id: $session_id" \
        -d "$body")

    # Extract JSON from SSE "data: {...}" line, or return raw if not SSE
    echo "$raw" | grep '^data: ' | sed 's/^data: //' | head -1
}

# ── Assertions ───────────────────────────────────────────────────────────────

assert_contains() {
    local haystack="$1"
    local needle="$2"
    local msg="${3:-Response should contain '$needle'}"
    if echo "$haystack" | grep -q "$needle"; then
        return 0
    else
        log_fail "$msg"
        log_fail "  Expected to find: $needle"
        log_fail "  In: $(echo "$haystack" | head -5)"
        return 1
    fi
}

assert_not_contains() {
    local haystack="$1"
    local needle="$2"
    local msg="${3:-Response should NOT contain '$needle'}"
    if echo "$haystack" | grep -q "$needle"; then
        log_fail "$msg"
        return 1
    fi
    return 0
}

assert_json_field() {
    local json="$1"
    local field="$2"
    local expected="$3"
    local msg="${4:-JSON field '$field' should equal '$expected'}"
    local actual
    actual=$(echo "$json" | jq -r "$field")
    if [ "$actual" = "$expected" ]; then
        return 0
    else
        log_fail "$msg"
        log_fail "  Expected: $expected"
        log_fail "  Actual:   $actual"
        return 1
    fi
}

# ── Test Runner ──────────────────────────────────────────────────────────────

run_test() {
    local name="$1"
    local func="$2"
    TESTS_RUN=$((TESTS_RUN + 1))
    log_info "Running: $name"
    if "$func"; then
        TESTS_PASSED=$((TESTS_PASSED + 1))
        log_ok "$name"
    else
        TESTS_FAILED=$((TESTS_FAILED + 1))
        log_fail "$name"
    fi
}

print_summary() {
    local label="${1:-Tests}"
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo -e "  ${label}: ${TESTS_RUN} run, ${GREEN}${TESTS_PASSED} passed${NC}, ${RED}${TESTS_FAILED} failed${NC}"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""

    # Write results file if E2E_RESULTS_DIR is set (used by run_all.sh)
    if [ -n "${E2E_RESULTS_DIR:-}" ]; then
        echo "${label}|${TESTS_RUN}|${TESTS_PASSED}|${TESTS_FAILED}" >> "$E2E_RESULTS_DIR/results.txt"
    fi

    [ "$TESTS_FAILED" -eq 0 ]
}
