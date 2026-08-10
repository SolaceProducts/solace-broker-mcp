#!/usr/bin/env bash
# Shared helper library for the E2E test suites (monitoring, management, …).
#
# This is the generic half of the scaffold: broker readiness, MCP server
# lifecycle, config generation, SEMP operations, the shared base broker
# fixtures, the MCP JSON-RPC wire, and assertions/test-runner. Suite-specific
# fixtures (monitoring's F1–F7, management's per-test config objects) live in
# each suite's own helpers.
#
# Note that this lib does NOT locate itself from its own path. The sourcing
# suite sets SUITE_DIR (its own directory) first; the lib derives BIN_DIR /
# ENV_FILE / REPO_ROOT from it and sources the suite's .env. Each suite
# therefore gets its own bin/, .env, ports, and containers off one
# shared library.
#
#   SUITE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
#   source "$SUITE_DIR/../e2e-common/lib.sh"

set -euo pipefail

# ── Paths (SUITE_DIR contract) ───────────────────────────────────────────────
: "${SUITE_DIR:?SUITE_DIR must be set before sourcing e2e-common/lib.sh}"
export SUITE_DIR
REPO_ROOT="${REPO_ROOT:-$(cd "$SUITE_DIR/../.." && pwd)}"
BIN_DIR="$SUITE_DIR/bin"
ENV_FILE="${ENV_FILE:-$SUITE_DIR/.env}"

# ── Broker settings (all values from .env) ──────────────────────────────────
# shellcheck source=/dev/null
source "$ENV_FILE"

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
# Honor a pre-exported MCP_URL so callers (e.g. an llm/ config.env) can target a
# non-default MCP server without losing the value to this assignment. Internal
# callers that don't pre-set it still get the local-docker default.
MCP_URL="${MCP_URL:-http://localhost:$MCP_PORT}"
# Normalize to a base URL: callers append path suffixes (`/mcp`, `/health`,
# etc.), so strip a trailing slash and a trailing `/mcp` to keep
# `MCP_URL=http://x:9090/mcp` overrides from yielding `…/mcp/mcp`.
MCP_URL="${MCP_URL%/}"
MCP_URL="${MCP_URL%/mcp}"
MCP_SERVER_PID=""
# Server stdout/stderr is captured here (under gitignored bin/) so a startup
# or runtime failure is diagnosable — locally and in CI — instead of vanishing
# into /dev/null.
MCP_SERVER_LOG="$BIN_DIR/mcp-server.log"

# Static dev token used to authenticate every e2e curl request to the broker
# MCP server. Defined in .env (single source of truth); exported here so child
# processes see it. write_config() references it as ${MCP_DEV_TOKEN} so the
# server's env substitution resolves it at config load.
export MCP_DEV_TOKEN

# setsid is Linux-only; fall back to a plain exec on macOS where nohup alone
# is sufficient to keep the child alive after the parent shell exits.
if command -v setsid >/dev/null 2>&1; then
    _SESSION_WRAP="setsid"
else
    _SESSION_WRAP=""
fi

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

# curl wrapper that pulls $BROKER_USER / $BROKER_PASS from the environment and
# feeds basic-auth via curl -K - (stdin config) instead of -u on the command
# line. Keeps the password out of the process argv so `ps` on the host can't
# see it. Callers pass any curl args as usual (e.g. -sf, -X DELETE,
# -w '%{http_code}'). Backslashes and double quotes in the values are escaped
# for curl's -K parser; newlines/CRs are rejected outright to prevent config
# injection. Exported so scenario setup.cmd / teardown.cmd / ground_truth.shell
# strings — which run in `bash -c` children — can invoke it. Defined here
# (above wait_for_broker) so every caller in this file resolves it regardless
# of source order. Introduced by SOL-150727 (LLM suite); consolidated by
# SOL-151860.
semp_curl() {
    local u="${BROKER_USER:?BROKER_USER not set}"
    local p="${BROKER_PASS:?BROKER_PASS not set}"
    if [[ "$u" == *[$'\n\r']* || "$p" == *[$'\n\r']* ]]; then
        echo "semp_curl: BROKER_USER/BROKER_PASS must not contain newlines" >&2
        return 2
    fi
    u="${u//\\/\\\\}"; u="${u//\"/\\\"}"
    p="${p//\\/\\\\}"; p="${p//\"/\\\"}"
    printf 'user = "%s:%s"\n' "$u" "$p" | curl -K - "$@"
}
export -f semp_curl

wait_for_broker() {
    local broker_url="${1:-$BROKER_URL}"
    local max_attempts="${2:-60}"
    local semp_config="$broker_url/SEMP/v2/config"
    local queues_url="$semp_config/msgVpns/$BROKER_VPN/queues"
    local probe_url="$queues_url/_e2e_spool_probe_"
    local probe_body='{"queueName":"_e2e_spool_probe_","accessType":"non-exclusive"}'
    # Shared budget across both phases: SEMP API readiness + message-spool
    # readiness must complete within max_attempts seconds combined. The
    # attempt counter is intentionally not reset between phases.
    local attempt=0
    log_info "Waiting for Solace broker at $broker_url (budget: ${max_attempts}s) ..."

    # Phase 1: Wait for SEMP API to respond
    while [ $attempt -lt "$max_attempts" ]; do
        if semp_curl -sf "$semp_config/msgVpns/$BROKER_VPN" >/dev/null 2>&1; then
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
    semp_curl -sf -X DELETE "$probe_url" >/dev/null 2>&1 || true

    local phase2_start=$attempt
    while [ $attempt -lt "$max_attempts" ]; do
        local http_code
        http_code=$(semp_curl -s -o /dev/null -w "%{http_code}" \
            -X POST -H "Content-Type: application/json" \
            -d "$probe_body" "$queues_url")
        if [ "$http_code" -ge 200 ] && [ "$http_code" -lt 300 ]; then
            semp_curl -sf -X DELETE "$probe_url" >/dev/null 2>&1 || true
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
            # Only a C compiler is needed: solace.dev/go/messaging statically
            # links libsolclient (and its OpenSSL dependency) on Linux, so there
            # is no libssl-dev build requirement nor a libssl runtime dependency.
            command -v gcc >/dev/null 2>&1 || missing+=("gcc")
            if [ ${#missing[@]} -gt 0 ]; then
                log_warn "Missing build dependencies: ${missing[*]}"
                log_warn "  Install with: sudo apt-get install build-essential"
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
    mkdir -p "$BIN_DIR"
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
    ENV_FILE="$ENV_FILE" \
        "$BIN_DIR/mcp-server" >"$MCP_SERVER_LOG" 2>&1 &
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
    log_fail "MCP server failed to start; last 50 lines of $MCP_SERVER_LOG:"
    tail -n 50 "$MCP_SERVER_LOG" >&2 2>/dev/null || true
    return 1
}

# Terminates one or more PIDs: sends SIGTERM to each that is still alive,
# waits up to 5s for all of them to exit, then SIGKILLs any straggler. PIDs are
# signalled concurrently so the grace window is shared across them, not paid
# per-PID. Already-dead PIDs (and empty/garbage ones) are skipped. We poll with
# `kill -0` rather than `wait` because these processes are not direct children
# of this shell: the MCP server is a child of start-server.sh and the
# broker-drivers run in their own sessions.
kill_gracefully() {
    local pids=() pid
    for pid in "$@"; do
        if kill -0 "$pid" 2>/dev/null; then
            kill -TERM "$pid" 2>/dev/null || true
            pids+=("$pid")
        fi
    done
    [ ${#pids[@]} -gt 0 ] || return 0

    local elapsed=0
    while [ ${#pids[@]} -gt 0 ] && [ "$elapsed" -lt 5 ]; do
        sleep 1; elapsed=$((elapsed + 1))
        local still=()
        for pid in "${pids[@]}"; do
            kill -0 "$pid" 2>/dev/null && still+=("$pid")
        done
        pids=( ${still[@]+"${still[@]}"} )
    done

    for pid in ${pids[@]+"${pids[@]}"}; do
        log_warn "PID $pid did not exit within 5s; sending SIGKILL"
        kill -KILL "$pid" 2>/dev/null || true
    done
}

stop_server() {
    if [ -z "$MCP_SERVER_PID" ] || ! kill -0 "$MCP_SERVER_PID" 2>/dev/null; then
        return 0
    fi
    log_info "Stopping MCP server (PID=$MCP_SERVER_PID) ..."
    kill_gracefully "$MCP_SERVER_PID"
    MCP_SERVER_PID=""
}

# ── broker-driver (messaging-layer fixtures) ─────────────────────────────────
# The broker-driver is a Go/CGo binary (solace.dev/go/messaging → libsolclient)
# that connects over SMF to produce broker states SEMP/curl cannot: connected
# clients, spooled messages, sustained traffic. Suites that need messaging-layer
# fixtures (monitoring's F3–F7, action's client/spool fixtures) build it and reap
# its long-lived processes via these helpers; pure-SEMP suites (management) don't.
# The sources live in test/e2e-common/broker-driver with their own go.mod.
#
# Suites that use it must `export BROKER_A_SMF_PORT BROKER_B_SMF_PORT` (from their
# .env) so the driver can resolve --broker=a|b to a host SMF port.

build_broker_driver() {
    log_info "Building broker-driver binary (CGo: libsolclient via solace.dev/go/messaging) ..."
    mkdir -p "$BIN_DIR"
    (cd "$REPO_ROOT/test/e2e-common/broker-driver" && go build -o "$BIN_DIR/broker-driver" .)
    log_info "broker-driver binary built: $BIN_DIR/broker-driver"
}

# Path pattern for broker-driver PID files, scoped to this suite's own bin/.
# Every long-lived driver self-writes a "$BIN_DIR/broker-driver-<role>.pid" that
# stop_broker_drivers reaps. Suite-scoped via BIN_DIR, so one suite never reaps
# another's drivers.
BROKER_DRIVER_PIDFILE_GLOB="$BIN_DIR/broker-driver-*.pid"

# Polls for a broker-driver's self-written pidfile — its readiness signal — up to
# 10s (20 * 0.5s). Returns non-zero and logs which driver failed and where to
# look if the file is still absent/empty.
#   $1 pidfile   $2 label   $3 logfile   $4 what (driver description)
wait_for_pidfile() {
    local pidfile="$1"
    local label="$2"
    local logfile="$3"
    local what="$4"
    local max_attempts=20     # 20 * 0.5s = 10s
    local attempt=0
    while [ $attempt -lt $max_attempts ] && [ ! -s "$pidfile" ]; do
        sleep 0.5
        attempt=$((attempt + 1))
    done
    if [ ! -s "$pidfile" ]; then
        log_fail "$what did not create pidfile on $label within 10s; see $logfile"
        return 1
    fi
}

# Stop all long-lived broker-driver processes this suite spawned. Reads each PID
# file under bin/ and hands the PIDs to kill_gracefully (TERM, then KILL after a
# shared 5s grace). SIGCONTs first so a deliberately-SIGSTOP'd driver (monitoring
# F6) can receive the TERM. Safe to call when there are no PID files.
stop_broker_drivers() {
    local pidfiles=( $BROKER_DRIVER_PIDFILE_GLOB )
    [ -e "${pidfiles[0]}" ] || return 0

    local pids=() f
    for f in "${pidfiles[@]}"; do
        pids+=("$(<"$f")")
    done
    local pid
    for pid in "${pids[@]}"; do
        kill -0 "$pid" 2>/dev/null && kill -CONT "$pid" 2>/dev/null || true
    done
    kill_gracefully "${pids[@]}"

    rm -f $BROKER_DRIVER_PIDFILE_GLOB
    # Let the broker finish cleaning up stale SMF sessions before subsequent SEMP
    # config operations (e.g. deleting a queue a driver was bound to) run.
    sleep 3
}

# Generate the MCP server config from .env-derived values so ports stay in sync.
# Credentials use ${VAR_NAME} substitution — resolved by the server via ENV_FILE.
# enable_write_tools is on for every suite: all suites exercise one server with
# both read and write tools registered.
#
# _lib_write_config emits the base two-broker config (broker-a/broker-b). A
# suite's helpers.sh may override the public write_config to call this and
# append suite-local aliases (see e2e-basic-mcp/helpers.sh); suites that need
# no extras just use the default definition below.
#   $1 config_file   path to write the generated YAML to
_lib_write_config() {
    local config_file="$1"
    cat > "$config_file" <<EOF
port: ${MCP_PORT}

mcp_client_auth:
  mode: static
  dev_token: "\${MCP_DEV_TOKEN}"

enable_write_tools: true

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

# Default public entry point. Suites that need suite-local aliases override
# this in their own helpers.sh (later-wins bash function definition) and call
# _lib_write_config to emit the base body.
write_config() { _lib_write_config "$@"; }

# ── SEMP Operations ──────────────────────────────────────────────────────────

semp_post() {
    local semp_config="$1"
    local path="$2"
    local data="$3"
    local response status body
    response=$(semp_curl -s -w $'\n%{http_code}' -X POST \
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
    semp_curl -sf -X DELETE \
        "$semp_config/$path" >/dev/null 2>&1 || true
}

# GET against the broker's private monitor endpoint, returning the JSON body
# on stdout. Returns non-zero on any non-2xx, so callers can short-circuit
# with `body=$(semp_monitor_get ...) || return 1`.
#
# Args:
#   $1 broker_url    e.g. http://localhost:8090
#   $2 path          path under SEMP/v2/__private_monitor__,
#                    e.g. "msgVpns/test-vpn"
semp_monitor_get() {
    local broker_url="$1"
    local path="$2"
    semp_curl -sf \
        "$broker_url/SEMP/v2/__private_monitor__/$path"
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
#   $5 predicate     optional jq bool expr, e.g. ".data.maxSpoolUsage == 10"
verify_monitor_object() {
    local broker_url="$1"
    local label="$2"
    local object_path="$3"
    local max_attempts="${4:-30}"
    local predicate="${5:-}"
    local monitor="$broker_url/SEMP/v2/__private_monitor__"
    local description="${object_path##*/}"
    local attempt=0 body

    while [ $attempt -lt $max_attempts ]; do
        if body=$(semp_curl -sf \
            "$monitor/$object_path" 2>/dev/null) \
           && { [ -z "$predicate" ] || [ "$(jq -r "$predicate" <<<"$body")" = "true" ]; }; then
            log_info "  monitor visible: $description on $label (${attempt}s)"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done
    log_warn "  monitor NOT visible: $description on $label after ${max_attempts}s"
    return 1
}

# ── Base broker fixtures ─────────────────────────────────────────────────────
# The starter set both the basic-mcp and monitoring suites provision on the
# default VPN of each broker: a queue, a REST delivery point with one consumer,
# and a queue binding tying them together. The monitoring suite layers its
# F1–F7 fixtures on top of this set; the management suite provisions nothing here.

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

    # test-rdp-failing: enabled RDP pointed at an unreachable remote so its
    # consumer's connect attempts fail and populate lastFailureReason on the RDP,
    # its restConsumers, and its queueBindings. Gives list-rdps.byLastFailureReason
    # and get-rdp-status.by{Binding,Consumer}LastFailureReason a real value to
    # aggregate. Separate from test-rdp (kept as the admin-disabled case).
    semp_post "$semp_config" "msgVpns/$BROKER_VPN/restDeliveryPoints" \
        '{"restDeliveryPointName":"test-rdp-failing","enabled":true}' >/dev/null

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/restDeliveryPoints/test-rdp-failing/restConsumers" \
        '{"restConsumerName":"test-consumer-failing","remoteHost":"127.0.0.1","remotePort":1,"tlsEnabled":false,"enabled":true}' >/dev/null

    semp_post "$semp_config" "msgVpns/$BROKER_VPN/restDeliveryPoints/test-rdp-failing/queueBindings" \
        '{"queueBindingName":"test-queue","postRequestTarget":"/test"}' >/dev/null

    # The private monitor endpoint can lag the config API, so confirm the
    # objects are visible before dependent fixtures/tests run.
    verify_fixtures "$broker_url" "$label"

    log_info "Fixtures created on $label"
}

# Visibility checks for the base queue and RDPs. The existence probes are
# best-effort (verify_monitor_object warns on timeout so the run continues if
# the monitor endpoint is still catching up), but the lastFailureReason poll
# is required — its value feeds the summary-aggregation assertions and a
# missing value would surface as a bogus count mismatch downstream.
verify_fixtures() {
    local broker_url="$1"
    local label="$2"
    log_info "Verifying base fixtures visible on $label ..."
    verify_monitor_object "$broker_url" "$label" "msgVpns/$BROKER_VPN/queues/test-queue" || true
    verify_monitor_object "$broker_url" "$label" "msgVpns/$BROKER_VPN/restDeliveryPoints/test-rdp" || true
    verify_monitor_object "$broker_url" "$label" "msgVpns/$BROKER_VPN/restDeliveryPoints/test-rdp-failing" || true
    # Poll until the failing consumer's lastFailureReason is populated. Retries
    # to the unreachable endpoint are asynchronous, so the field can be empty
    # for the first few seconds; the aggregation assertions depend on it, so
    # fail fixture creation here rather than let the summary counts drift later.
    verify_monitor_object "$broker_url" "$label" \
        "msgVpns/$BROKER_VPN/restDeliveryPoints/test-rdp-failing/restConsumers/test-consumer-failing" \
        30 '.data.lastFailureReason != ""'
}

# Deletes the base fixtures in reverse dependency order (binding → consumer →
# RDP → queue), ignoring 404s. Idempotent: safe to call before a run and from a
# cleanup trap.
cleanup_fixtures_on() {
    local semp_config="$1"
    local label="$2"
    log_info "Cleaning up fixtures on $label ..."
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/restDeliveryPoints/test-rdp-failing/queueBindings/test-queue"
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/restDeliveryPoints/test-rdp-failing/restConsumers/test-consumer-failing"
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/restDeliveryPoints/test-rdp-failing"
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/restDeliveryPoints/test-rdp/queueBindings/test-queue"
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/restDeliveryPoints/test-rdp/restConsumers/test-consumer"
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/restDeliveryPoints/test-rdp"
    semp_delete "$semp_config" "msgVpns/$BROKER_VPN/queues/test-queue"
    log_info "Fixtures cleaned up on $label"
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

# Unwraps the tool payload from the JSON-RPC envelope returned by mcp_call_tool.
# A tool's real output is escaped JSON nested in .result.content[0].text; this
# returns that inner payload so assertions can run assert_json_field against the
# tool's structured output rather than substring-matching the whole envelope.
#   response=$(mcp_call_tool "get-vpn-status" "$args")
#   content=$(extract_content "$response")
#   assert_json_field "$content" ".enabled" "false"
extract_content() {
    # Surface JSON-RPC errors inline. Without this, an error envelope
    # ({"error":{...}} instead of {"result":...}) makes the jq below emit the
    # literal "null", and the caller's assertion fails with a cryptic
    # "Actual: null" that hides the real tool/broker error. log_fail writes to
    # stderr, so the stdout payload contract is unchanged and callers need no
    # update — the error just appears in the log next to the assertion failure.
    local err
    err=$(jq -r '.error.message // empty' <<<"$1")
    [ -n "$err" ] && log_fail "MCP call returned error: $err"
    jq -r '.result.content[0].text' <<<"$1"
}

# ── Assertions ───────────────────────────────────────────────────────────────

assert_contains() {
    local haystack="$1"
    local needle="$2"
    local msg="${3:-Response should contain '$needle'}"
    # -F: treat needle as a fixed string, not a regex. Without this, metacharacters
    # (. * [ ] etc.) in tool responses or expected values silently change matching.
    # Herestring, not `echo … | grep -qF`: echo emits one write() per line, so on a
    # multi-line or >64KB haystack grep -q can match and exit with writes still
    # pending, and the writer's SIGPIPE (141) becomes the pipeline's status under
    # pipefail — inverting the result. Here that would be a spurious failure; in
    # assert_not_contains below, a silent pass.
    if grep -qF "$needle" <<<"$haystack"; then
        return 0
    else
        log_fail "$msg"
        log_fail "  Expected to find: $needle"
        log_fail "  In: $(head -5 <<<"$haystack")"
        return 1
    fi
}

assert_not_contains() {
    local haystack="$1"
    local needle="$2"
    local msg="${3:-Response should NOT contain '$needle'}"
    # Herestring for the reason given in assert_contains above — load-bearing here,
    # since the inverted result is a silent pass on a guard (e.g. credential leak).
    if grep -qF "$needle" <<<"$haystack"; then
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

# Assert that .summary.<field> in <content> equals the count of rows in
# <well_typed> that match <row_predicate>. Primary building block for the
# SOL-151519 summary-aggregation checks: the handler emits scalar counts over
# well-typed rows matching a predicate; the test recomputes with the same
# well-typed filter and predicate and requires equality.
#
# Args:
#   $1 content         MCP tool response payload (after extract_content)
#   $2 label           prefix for the failure message, e.g. "list-rdps [broker-a]"
#   $3 well_typed      jq expression returning a filtered array of rows the
#                      handler would NOT skip, e.g.
#                      '[.rdps.data[] | select((.up|type)=="boolean" ...)]'
#   $4 field           summary field name, e.g. "disabledCount"
#   $5 row_predicate   jq boolean expression over a single row, e.g.
#                      '.enabled == false'
assert_recompute_count() {
    local content="$1" label="$2" well_typed="$3" field="$4" predicate="$5"
    assert_json_field "$content" \
        "(.summary.${field}) == (${well_typed} | map(select(${predicate})) | length)" "true" \
        "${label}: summary.${field} must equal recomputed count from rows"
}

# Assert that .summary.<field> in <content> — an object shaped
# {key: count} — equals the grouped tally of <well_typed> rows matching
# <row_predicate>, keyed by <group_expr>. Handler-side skip filters (e.g.
# omit empty-string keys) must be encoded in <row_predicate> so both sides
# apply the same filter.
#
# Args mirror assert_recompute_count plus:
#   $6 group_expr      jq expression selecting the group key on a single row,
#                      starting with '.', e.g. '.lastFailureReason'
assert_recompute_group() {
    local content="$1" label="$2" well_typed="$3" field="$4" predicate="$5" group_expr="$6"
    # from_entries over grouped rows gives {key: count}. Empty input → {}
    # which matches the handler's initial empty map when nothing qualifies.
    assert_json_field "$content" \
        "(.summary.${field}) == (${well_typed} | map(select(${predicate})) | group_by(${group_expr}) | map({key: .[0]${group_expr}, value: length}) | from_entries)" "true" \
        "${label}: summary.${field} must equal recomputed grouping from rows"
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

    # Append a machine-readable row when E2E_RESULTS_DIR is set, so a multi-scenario
    # runner (e2e-basic-mcp/run-all.sh) can build an aggregate summary table. No-op
    # for suites that don't set it.
    if [ -n "${E2E_RESULTS_DIR:-}" ]; then
        echo "${label}|${TESTS_RUN}|${TESTS_PASSED}|${TESTS_FAILED}" >> "$E2E_RESULTS_DIR/results.txt"
    fi

    [ "$TESTS_FAILED" -eq 0 ]
}
