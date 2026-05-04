#!/usr/bin/env bash
# Self-contained helper functions for tool validation tests.
# Source this file from test scripts: source "$(dirname "$0")/helpers.sh"

set -euo pipefail

# ── Paths ────────────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
BIN_DIR="$SCRIPT_DIR/bin"

# ── Settings from .env ───────────────────────────────────────────────────────
# shellcheck source=.env
source "$SCRIPT_DIR/.env"

BROKER_URL="http://localhost:${BROKER_SEMP_PORT}"
BROKER_USER="${BROKER_USERNAME}"
BROKER_PASS="${BROKER_PASSWORD}"
BROKER_VPN="${BROKER_VPN}"

# ── MCP server settings ─────────────────────────────────────────────────────
MCP_PORT=9091
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
    local max_attempts="${2:-90}"
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

    # Phase 2: Wait for message spool to be writable
    while [ $attempt -lt "$max_attempts" ]; do
        local http_code
        http_code=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
            -u "$BROKER_USER:$BROKER_PASS" \
            -H "Content-Type: application/json" \
            "$semp_config/msgVpns/$BROKER_VPN/queues" \
            -d '{"queueName":"_val_spool_probe_","accessType":"non-exclusive"}')
        if [ "$http_code" -ge 200 ] && [ "$http_code" -lt 300 ]; then
            curl -sf -X DELETE -u "$BROKER_USER:$BROKER_PASS" \
                "$semp_config/msgVpns/$BROKER_VPN/queues/_val_spool_probe_" >/dev/null 2>&1 || true
            log_info "Broker fully ready after ${attempt}s (message spool writable)"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done
    log_fail "Broker message spool not ready after ${max_attempts}s ($broker_url)"
    return 1
}

# ── MCP Server ───────────────────────────────────────────────────────────────

build_server() {
    log_info "Building MCP server binary ..."
    mkdir -p "$BIN_DIR"
    (cd "$REPO_ROOT" && go build -o "$BIN_DIR/mcp-server" ./cmd/server)
    log_info "Server binary built: $BIN_DIR/mcp-server"
}

write_config() {
    local config_file="$1"
    cat > "$config_file" <<EOF
port: ${MCP_PORT}
development_mode: true

brokers:
  broker-a:
    url: "${BROKER_URL}"
    auth:
      mode: basic
      username: "${BROKER_USER}"
      password: "${BROKER_PASS}"
EOF
    log_info "Config written to $config_file"
}

start_server() {
    local config_file="$1"
    log_info "Starting MCP server (config=$config_file, port=$MCP_PORT) ..."

    local existing_pid
    existing_pid=$(lsof -ti:"$MCP_PORT" 2>/dev/null || true)
    if [ -n "$existing_pid" ]; then
        log_warn "Killing existing process on port $MCP_PORT (PID=$existing_pid)"
        kill "$existing_pid" 2>/dev/null || true
        sleep 1
    fi

    CONFIG_FILE="$config_file" \
        "$BIN_DIR/mcp-server" 2>/dev/null &
    MCP_SERVER_PID=$!

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

# ── MCP Protocol Helpers ─────────────────────────────────────────────────────

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
                "clientInfo": { "name": "validation-test", "version": "1.0.0" }
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

mcp_call_tool() {
    local session_id="$1"
    local tool_name="$2"
    local arguments="$3"

    local body
    body=$(cat <<EOF
{
    "jsonrpc": "2.0",
    "id": $((RANDOM % 10000 + 100)),
    "method": "tools/call",
    "params": {
        "name": "$tool_name",
        "arguments": $arguments
    }
}
EOF
)

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

assert_json_field_gt() {
    local json="$1"
    local field="$2"
    local threshold="$3"
    local msg="${4:-JSON field '$field' should be > $threshold}"
    local actual
    actual=$(echo "$json" | jq -r "$field")
    if [ "$actual" -gt "$threshold" ] 2>/dev/null; then
        return 0
    else
        log_fail "$msg"
        log_fail "  Expected > $threshold, got: $actual"
        return 1
    fi
}

assert_json_field_gte() {
    local json="$1"
    local field="$2"
    local threshold="$3"
    local msg="${4:-JSON field '$field' should be >= $threshold}"
    local actual
    actual=$(echo "$json" | jq -r "$field")
    if [ "$actual" -ge "$threshold" ] 2>/dev/null; then
        return 0
    else
        log_fail "$msg"
        log_fail "  Expected >= $threshold, got: $actual"
        return 1
    fi
}

assert_json_array_length_gte() {
    local json="$1"
    local field="$2"
    local min_length="$3"
    local msg="${4:-JSON array '$field' should have >= $min_length elements}"
    local actual
    actual=$(echo "$json" | jq "$field | length")
    if [ "$actual" -ge "$min_length" ] 2>/dev/null; then
        return 0
    else
        log_fail "$msg"
        log_fail "  Expected >= $min_length elements, got: $actual"
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
    [ "$TESTS_FAILED" -eq 0 ]
}

# ── Message Publishing ───────────────────────────────────────────────────────

publish_messages() {
    local topic_prefix="$1"
    local count="$2"
    local rest_port="${BROKER_REST_PORT}"
    local user="${VAL_USERNAME}"
    local pass="${VAL_PASSWORD}"

    log_info "Publishing $count messages to ${topic_prefix}/..."
    for i in $(seq 1 "$count"); do
        local http_code
        http_code=$(curl -s -o /dev/null -w "%{http_code}" \
            -X POST "http://localhost:${rest_port}/${topic_prefix}/test/msg${i}" \
            -u "${user}:${pass}" \
            -H "Content-Type: text/plain" \
            -H "Solace-Delivery-Mode: persistent" \
            -d "Message ${i} on ${topic_prefix}")
        if [ "$http_code" -lt 200 ] || [ "$http_code" -ge 300 ]; then
            log_fail "Failed to publish message $i to ${topic_prefix} (HTTP $http_code)"
            return 1
        fi
    done
    log_info "Published $count messages to ${topic_prefix}/"
}

# ── sdkperf Client Management ───────────────────────────────────────────────

SDKPERF_PID_FILE="$SCRIPT_DIR/.sdkperf_pids"

start_sdkperf_subscriber() {
    local name="$1"
    local topics="$2"  # comma-separated topic list
    local smf_port="${BROKER_SMF_PORT}"
    local user="${VAL_USERNAME}"
    local pass="${VAL_PASSWORD}"

    if [ ! -x "$SDKPERF" ]; then
        log_warn "sdkperf_c not found at $SDKPERF — skipping client '$name'"
        return 1
    fi

    log_info "Starting sdkperf subscriber '$name' on topics: $topics"
    "$SDKPERF" -cip="localhost:${smf_port}" \
        -cu="${user}@${BROKER_VPN}" -cp="${pass}" \
        -stl="$topics" -q &
    local pid=$!
    echo "$pid" >> "$SDKPERF_PID_FILE"
    log_info "sdkperf subscriber '$name' started (PID=$pid)"
}

start_sdkperf_queue_consumer() {
    local name="$1"
    local queue="$2"
    local smf_port="${BROKER_SMF_PORT}"
    local user="${VAL_USERNAME}"
    local pass="${VAL_PASSWORD}"

    if [ ! -x "$SDKPERF" ]; then
        log_warn "sdkperf_c not found at $SDKPERF — skipping client '$name'"
        return 1
    fi

    log_info "Starting sdkperf queue consumer '$name' on queue: $queue"
    "$SDKPERF" -cip="localhost:${smf_port}" \
        -cu="${user}@${BROKER_VPN}" -cp="${pass}" \
        -sql="$queue" -q &
    local pid=$!
    echo "$pid" >> "$SDKPERF_PID_FILE"
    log_info "sdkperf queue consumer '$name' started (PID=$pid)"
}

stop_all_sdkperf() {
    if [ -f "$SDKPERF_PID_FILE" ]; then
        log_info "Stopping sdkperf clients ..."
        while IFS= read -r pid; do
            if kill -0 "$pid" 2>/dev/null; then
                kill "$pid" 2>/dev/null || true
            fi
        done < "$SDKPERF_PID_FILE"
        rm -f "$SDKPERF_PID_FILE"
        log_info "sdkperf clients stopped"
    fi
}
