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
MCP_PORT="${MCP_PORT:-9090}"
MCP_URL="http://localhost:$MCP_PORT"
MCP_SERVER_PID=""

# Static dev token used to authenticate every e2e curl/mcp-tester request to
# the broker MCP server. Defined in .env (single source of truth); exported
# here so child processes (mcp-tester) see it. write_config() and
# broker-config.yaml reference it as ${MCP_DEV_TOKEN} so the server's env
# substitution resolves it at config load.
export MCP_DEV_TOKEN

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

log_info()  { echo -e "${CYAN}[INFO]${NC}  $*" >&2; }
log_ok()    { echo -e "${GREEN}[PASS]${NC}  $*" >&2; }
log_fail()  { echo -e "${RED}[FAIL]${NC}  $*" >&2; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC}  $*" >&2; }

# ── Broker Readiness ─────────────────────────────────────────────────────────

wait_for_broker() {
    local broker_url="${1:-$BROKER_URL}"
    local max_attempts="${2:-60}"
    local semp_config="$broker_url/SEMP/v2/config"
    # Shared budget across both phases: SEMP API readiness + message-spool
    # readiness must complete within max_attempts seconds combined. The
    # attempt counter is intentionally not reset between phases.
    local attempt=0
    log_info "Waiting for Solace broker at $broker_url (budget: ${max_attempts}s) ..."

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
        log_fail "Broker SEMP API not ready within ${max_attempts}s budget ($broker_url)"
        return 1
    fi
    log_info "SEMP API responding after ${attempt}s ($broker_url)"

    # Phase 2: Wait for message spool to accept queue operations.
    # The monitor API may report spool fields before the spool is actually writable,
    # so we probe with a real queue create/delete to confirm readiness.
    # If a previous run was killed in the middle, the probe queue may still
    # exist on the broker. Delete it before we try to create a fresh one,
    # otherwise the create returns "already exists" and we time out blaming
    # the broker.
    curl -sf -X DELETE -u "$BROKER_USER:$BROKER_PASS" \
        "$semp_config/msgVpns/$BROKER_VPN/queues/_e2e_spool_probe_" >/dev/null 2>&1 || true

    local phase2_start=$attempt
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
    log_fail "Broker message spool not ready after $((attempt - phase2_start))s in phase 2 (${max_attempts}s total budget exhausted) ($broker_url)"
    return 1
}

wait_for_all_brokers() {
    local max_attempts="${1:-90}"
    wait_for_broker "$BROKER_A_URL" "$max_attempts"
    wait_for_broker "$BROKER_B_URL" "$max_attempts"
}

# ── MCP Server ───────────────────────────────────────────────────────────────

check_build_deps() {
    local os missing=()
    os="$(uname -s)"

    case "$os" in
        Linux)
            command -v gcc >/dev/null 2>&1 || missing+=("gcc")
            if ! ldconfig -p 2>/dev/null | grep -q 'libssl\.so'; then
                missing+=("libssl-dev")
            fi
            if [ ${#missing[@]} -gt 0 ]; then
                log_warn "Missing build dependencies: ${missing[*]}"
                log_warn "  Install with: sudo apt-get install ${missing[*]}"
            fi
            ;;
        Darwin)
            if ! command -v brew >/dev/null 2>&1; then
                log_warn "Homebrew not found — cannot verify openssl@3 is installed"
                log_warn "  Install Homebrew from https://brew.sh, then: brew install openssl@3"
            elif ! brew list --versions openssl@3 >/dev/null 2>&1; then
                log_warn "Missing build dependency: openssl@3"
                log_warn "  Install with: brew install openssl@3"
            fi
            ;;
    esac
}

build_server() {
    log_info "Building MCP server binary ..."
    (cd "$REPO_ROOT" && go build -o "$BIN_DIR/mcp-server" ./cmd/server)
    log_info "Server binary built: $BIN_DIR/mcp-server"
}

build_mcp_tester() {
    log_info "Building mcp-tester binary ..."
    (cd "$SCRIPT_DIR/mcp-tester" && go build -o "$BIN_DIR/mcp-tester" .)
    log_info "mcp-tester binary built: $BIN_DIR/mcp-tester"
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
    # `wait` only works on direct children of the current shell. When this
    # helper is sourced into test-monitoring-tools.sh and MCP_SERVER_PID was
    # read from a pidfile (server is a child of start-server.sh, not us), we
    # poll with `kill -0` instead. Escalate to SIGKILL if the server hasn't
    # exited within the grace window — mirrors stop_broker_drivers.
    if [ -z "$MCP_SERVER_PID" ] || ! kill -0 "$MCP_SERVER_PID" 2>/dev/null; then
        return 0
    fi
    log_info "Stopping MCP server (PID=$MCP_SERVER_PID) ..."
    kill -TERM "$MCP_SERVER_PID" 2>/dev/null || true

    local elapsed=0
    while kill -0 "$MCP_SERVER_PID" 2>/dev/null && [ "$elapsed" -lt 5 ]; do
        sleep 1
        elapsed=$((elapsed + 1))
    done

    if kill -0 "$MCP_SERVER_PID" 2>/dev/null; then
        log_warn "MCP server did not exit within 5s; sending SIGKILL"
        kill -KILL "$MCP_SERVER_PID" 2>/dev/null || true
    fi
    MCP_SERVER_PID=""
}

# Path pattern for broker-driver PID files. Defined once here so the
# stop helper and any future code use the same convention.
BROKER_DRIVER_PIDFILE_GLOB="$BIN_DIR/broker-driver-f*.pid"

# Stop any long-lived broker-driver processes that fixtures F3-F6 spawn.
# Reads each PID file under bin/, sends a polite termination signal (TERM),
# waits up to 5 seconds for them to exit cleanly, then forces (KILL).
# Safe to call when there are no PID files (no-op until F3 lands).
stop_broker_drivers() {
    local pidfiles=( $BROKER_DRIVER_PIDFILE_GLOB )
    [ -e "${pidfiles[0]}" ] || return 0

    local pids=()
    for f in "${pidfiles[@]}"; do
        local pid; pid=$(<"$f")
        if kill -0 "$pid" 2>/dev/null; then
            kill -TERM "$pid"
            pids+=("$pid")
        fi
    done

    local elapsed=0
    while [ ${#pids[@]} -gt 0 ] && [ "$elapsed" -lt 5 ]; do
        sleep 1; elapsed=$((elapsed+1))
        local still=(); for p in "${pids[@]}"; do
            kill -0 "$p" 2>/dev/null && still+=("$p")
        done
        pids=( "${still[@]}" )
    done

    for p in "${pids[@]}"; do kill -KILL "$p" 2>/dev/null; done
    rm -f $BROKER_DRIVER_PIDFILE_GLOB
}

write_config() {
    local config_file="$1"
    # Generate broker config from .env-derived values so ports stay in sync.
    # Credentials use ${VAR_NAME} substitution — resolved by the server via ENV_FILE.
    cat > "$config_file" <<EOF
client_auth:
  mode: static
  dev_token: "\${MCP_DEV_TOKEN}"

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
    local response status body
    response=$(curl -s -w $'\n%{http_code}' -X POST -u "$BROKER_USER:$BROKER_PASS" \
        -H "Content-Type: application/json" \
        "$semp_config/$path" -d "$data")
    status="${response##*$'\n'}"
    body="${response%$'\n'*}"
    if [ "$status" -ge 200 ] && [ "$status" -lt 300 ]; then
        return 0
    else
        log_fail "SEMP POST $path returned HTTP $status"
        [ -n "$body" ] && printf '%s\n' "$body" >&2
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

# Polls the monitor API until the given object is visible (HTTP 2xx).
# Returns 0 on success, 1 on timeout. Logs a warning on timeout but does
# not fail — callers proceed-anyway so a transient monitor lag doesn't
# block the scaffold. The leaf segment of object_path is used as the
# human description in logs.
#
# Args:
#   $1 broker_url    e.g. http://localhost:8090
#   $2 label         human label for logs, e.g. "broker-a"
#   $3 object_path   path under SEMP/v2/__private_monitor__,
#                    e.g. "msgVpns/default/queues/test-queue-2"
#   $4 max_attempts  optional, default 30
verify_monitor_object() {
    local broker_url="$1"
    local label="$2"
    local object_path="$3"
    local max_attempts="${4:-30}"
    local monitor="$broker_url/SEMP/v2/__private_monitor__"
    local description="${object_path##*/}"
    local attempt=0

    while [ $attempt -lt $max_attempts ]; do
        if curl -sf -u "$BROKER_USER:$BROKER_PASS" \
            "$monitor/$object_path" >/dev/null 2>&1; then
            log_info "  monitor visible: $description on $label (${attempt}s)"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done
    log_warn "  monitor NOT visible: $description on $label after ${max_attempts}s — proceeding anyway"
    return 1
}

verify_fixtures() {
    local broker_url="$1"
    local label="$2"
    log_info "Verifying base fixtures visible on $label ..."
    verify_monitor_object "$broker_url" "$label" "msgVpns/$BROKER_VPN/queues/test-queue"
    verify_monitor_object "$broker_url" "$label" "msgVpns/$BROKER_VPN/restDeliveryPoints/test-rdp"
}

verify_multi_vpn_on() {
    local broker_url="$1"
    local label="$2"
    log_info "Verifying multi-VPN fixture visible on $label ..."
    verify_monitor_object "$broker_url" "$label" "msgVpns/test-vpn"
}

verify_multi_queue_on() {
    local broker_url="$1"
    local label="$2"
    log_info "Verifying multi-queue fixtures visible on $label ..."
    verify_monitor_object "$broker_url" "$label" "msgVpns/$BROKER_VPN/queues/test-queue-2"
    verify_monitor_object "$broker_url" "$label" "msgVpns/$BROKER_VPN/queues/test-queue-3"
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

# Provisions a second, non-default VPN ("test-vpn") on a broker with enabled=false.
# Intentionally no client-user / ACL / queue provisioning — this VPN exists only
# to exercise multi-VPN discovery and listing, not messaging.
create_multi_vpn_on() {
    local semp_config="$1"
    local label="$2"
    local broker_url="$3"
    log_info "Creating multi-VPN fixture on $label ..."
    semp_post "$semp_config" "msgVpns" \
        '{"msgVpnName":"test-vpn","enabled":false}' >/dev/null
    verify_multi_vpn_on "$broker_url" "$label"
    log_info "Multi-VPN fixture created on $label (test-vpn, enabled=false)"
}

cleanup_multi_vpn_on() {
    local semp_config="$1"
    local label="$2"
    log_info "Cleaning up multi-VPN fixture on $label ..."
    semp_delete "$semp_config" "msgVpns/test-vpn"
    log_info "Multi-VPN fixture cleaned up on $label"
}

# Provisions two additional queues on the default VPN to exercise multi-queue
# discovery and bound-vs-unbound state:
#   - test-queue-2: non-exclusive, bound to the existing test-rdp
#   - test-queue-3: non-exclusive, unbound
# Depends on create_fixtures_on having run first (reuses test-rdp).
create_multi_queue_on() {
    local semp_config="$1"
    local label="$2"
    local broker_url="$3"
    log_info "Creating multi-queue fixtures on $label ..."

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues" \
        '{"queueName":"test-queue-2","accessType":"non-exclusive","permission":"consume","ingressEnabled":true,"egressEnabled":true}' >/dev/null

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/queues" \
        '{"queueName":"test-queue-3","accessType":"non-exclusive","permission":"consume","ingressEnabled":true,"egressEnabled":true}' >/dev/null

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/restDeliveryPoints/test-rdp/queueBindings" \
        '{"queueBindingName":"test-queue-2","postRequestTarget":"/test"}' >/dev/null

    verify_multi_queue_on "$broker_url" "$label"
    log_info "Multi-queue fixtures created on $label (test-queue-2 bound to test-rdp, test-queue-3 unbound)"
}

cleanup_multi_queue_on() {
    local semp_config="$1"
    local label="$2"
    log_info "Cleaning up multi-queue fixtures on $label ..."
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/restDeliveryPoints/test-rdp/queueBindings/test-queue-2"
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/queues/test-queue-2"
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/queues/test-queue-3"
    log_info "Multi-queue fixtures cleaned up on $label"
}

create_fixtures() {
    cleanup_fixtures
    create_fixtures_on "$BROKER_A_SEMP_CONFIG" "broker-a" "$BROKER_A_URL"
    create_fixtures_on "$BROKER_B_SEMP_CONFIG" "broker-b" "$BROKER_B_URL"
    create_multi_queue_on "$BROKER_A_SEMP_CONFIG" "broker-a" "$BROKER_A_URL"
    create_multi_queue_on "$BROKER_B_SEMP_CONFIG" "broker-b" "$BROKER_B_URL"
    create_multi_vpn_on "$BROKER_A_SEMP_CONFIG" "broker-a" "$BROKER_A_URL"
    create_multi_vpn_on "$BROKER_B_SEMP_CONFIG" "broker-b" "$BROKER_B_URL"
}

cleanup_fixtures() {
    cleanup_multi_vpn_on "$BROKER_A_SEMP_CONFIG" "broker-a"
    cleanup_multi_vpn_on "$BROKER_B_SEMP_CONFIG" "broker-b"
    cleanup_multi_queue_on "$BROKER_A_SEMP_CONFIG" "broker-a"
    cleanup_multi_queue_on "$BROKER_B_SEMP_CONFIG" "broker-b"
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
        -H "Authorization: Bearer $MCP_DEV_TOKEN" \
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
        -H "Authorization: Bearer $MCP_DEV_TOKEN" \
        -H "Mcp-Session-Id: $session_id" \
        -d '{
            "jsonrpc": "2.0",
            "method": "notifications/initialized"
        }' >/dev/null 2>&1 || true

    echo "$session_id"
}

# Initialize a session and call a single tool. Returns the SSE-extracted JSON response.
# Args:
#   $1 = tool name (e.g. "get-rdp-status")
#   $2 = tool arguments as a JSON object literal (e.g. '{"broker":"broker-a"}')
# Use jq for the args to keep escaping sane:
#   args=$(jq -nc --arg b "$broker" '{broker:$b,msgVpnName:"default"}')
#   mcp_call_tool "list-queues" "$args"
mcp_call_tool() {
    local tool="$1"
    local args_json="$2"
    local sid
    sid=$(mcp_initialize) || return 1
    mcp_request "$sid" "$(jq -nc --arg t "$tool" --argjson a "$args_json" \
        '{jsonrpc:"2.0",id:1,method:"tools/call",params:{name:$t,arguments:$a}}')"
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
        -H "Authorization: Bearer $MCP_DEV_TOKEN" \
        -H "Mcp-Session-Id: $session_id" \
        -d "$body")

    # Extract JSON from SSE "data: {...}" lines. Keep all data: lines — a single
    # logical response can span multiple lines or events per the SSE spec.
    echo "$raw" | grep '^data: ' | sed 's/^data: //'
}

# ── Assertions ───────────────────────────────────────────────────────────────

assert_contains() {
    local haystack="$1"
    local needle="$2"
    local msg="${3:-Response should contain '$needle'}"
    # -F: treat needle as a fixed string, not a regex. Without this, metacharacters
    # (. * [ ] etc.) in tool responses or expected values silently change matching.
    if echo "$haystack" | grep -qF "$needle"; then
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
    if echo "$haystack" | grep -qF "$needle"; then
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

    [ "$TESTS_FAILED" -eq 0 ]
}
