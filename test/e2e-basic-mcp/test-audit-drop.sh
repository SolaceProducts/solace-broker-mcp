#!/usr/bin/env bash
# Scenario 5: Audit drop visibility (SOL-154569) — mcp_audit_events_dropped_total
# rises alongside the audit_drop record, and keeps rising when the log stream
# itself fails and no record can be written at all.
#
# Two phases, one server each:
#
#   Phase 1 — level filter (both signals observable). `log_level: error` with
#   the audit log on is a configuration the server accepts and the runbook
#   tells operators to fix: every INFO-level audit record (operation,
#   auth_success) is dropped through the same EmitDrop path a refusing handler
#   takes, while the ERROR-level audit_drop notice still lands in the server
#   log. Asserts the counter is 0 before any drop, that a destructive call
#   leaves audit_drop records (at least one naming an operation record) and no
#   operation record — the positive control that the filter is in force — and
#   that the counter rose by exactly the number of audit_drop records written.
#
#   Phase 2 — dead log stream (the counter is the only signal). Linux only:
#   stderr is redirected to /dev/full, which fails every write with ENOSPC at
#   once, so at `log_level: info` neither the operation record nor its drop
#   notice lands anywhere. Asserts the tool call still completes and the counter
#   still moves. This is the ticket's headline case. Discovery called it "stderr
#   backpressure", but a FULL pipe blocks the writer rather than failing the
#   write (see the runbook's "Log-shipper or stderr backpressure"), so nothing
#   would drop; /dev/full is the deterministic stand-in for a stream that fails.
#   Skipped where /dev/full does not exist (macOS).
#
# The handler-panic drop path is pinned by unit tests in
# internal/observability/audit. Starts and stops its own MCP server (the shared
# one is already gone after Scenario 4), so it runs after test-throttling.sh in
# run-all.sh.

set -euo pipefail
source "$(dirname "$0")/helpers.sh"

MCP_METRICS_PORT="${MCP_METRICS_PORT:-9091}"
METRICS_URL="http://localhost:${MCP_METRICS_PORT}/metrics"
LEVEL_FILTER_CONFIG="$BIN_DIR/mcp-config-audit-drop.yaml"
DEAD_SINK_CONFIG="$BIN_DIR/mcp-config-audit-drop-deadsink.yaml"

# This script owns the servers it starts; run-all.sh's own cleanup only knows
# the shared server's PID. INT/TERM/HUP as well as EXIT, because bash skips the
# EXIT trap when the shell dies from an untrapped signal (test-throttling.sh
# and start-server.sh use the same set).
trap stop_server EXIT INT TERM HUP

# ── Config ───────────────────────────────────────────────────────────────────

# write_audit_drop_config <file> <log_level>
# The suite's write_config body (four brokers, test retry budget) plus the two
# settings this scenario is about. log_level is a top-level key; the
# observability block carries only the metrics listener address, because the
# on/off switches are OBS_* environment variables (docs/configuration.md,
# "Observability Settings"). Lives under bin/ so a CI-only failure still shows
# what the server ran with (the workflow's evidence step globs
# bin/mcp-config-*.yaml); it holds only ${VAR} placeholders.
write_audit_drop_config() {
    local config_file="$1" level="$2"
    write_config "$config_file"
    cat >> "$config_file" <<YAML

# Scenario 5 (SOL-154569). At "error" every INFO-level audit record is filtered
# out and reported as an audit_drop; at "info" nothing is filtered, so a drop
# here can only come from the log write itself failing.
log_level: ${level}

observability:
  metrics_bind_address: ":${MCP_METRICS_PORT}"
YAML
    log_info "Appended log_level=${level} and metrics_bind_address=:${MCP_METRICS_PORT} to $config_file"
}

# ── Helpers ──────────────────────────────────────────────────────────────────

# Prints the current value of mcp_audit_events_dropped_total, or nothing when
# the series is absent from the scrape.
scrape_drop_counter() {
    curl -sf "$METRICS_URL" | awk '$1 == "mcp_audit_events_dropped_total" { print $2 }'
}

# Current line count of the server log; pair with audit_records_since so an
# assertion only considers records appended by the call under test.
log_mark() {
    local n
    n=$(wc -l < "$MCP_SERVER_LOG" 2>/dev/null) || n=0
    echo "${n// /}"
}

# audit_records_since <mark> <audit_event_type>
# Emits, one JSON object per line, the audit records of that type appended
# after <mark>. fromjson? skips any non-JSON line rather than aborting.
audit_records_since() {
    local mark="$1" type="$2"
    tail -n +"$((mark + 1))" "$MCP_SERVER_LOG" 2>/dev/null \
        | jq -c -R --arg t "$type" 'fromjson? | select(.event == "audit" and .audit_event_type == $t)'
}

# Counts non-empty stdin lines; 0 rather than grep's exit-1 on no match.
count_lines() { grep -c . || true; }

require_int() {
    local value="$1" what="$2"
    if ! [[ "$value" =~ ^[0-9]+$ ]]; then
        log_fail "$what is not an integer: '${value:-<empty>}' (is the series on the scrape?)"
        return 1
    fi
}

# Frees a port a previous run may have left a server on. TERM with a grace
# period, then KILL: the server holds its listeners through its shutdown drain
# delay, so a bare kill followed by a one-second sleep would leave the port
# taken and the next bind failing (same pattern as start_semp_tap).
sweep_port() {
    local port="$1" what="$2" existing_pid
    existing_pid=$(lsof -ti:"$port" 2>/dev/null || true)
    if [ -n "$existing_pid" ]; then
        log_warn "Killing existing process on $what port $port (PID=$existing_pid)"
        # shellcheck disable=SC2086 # one PID per line; kill_gracefully takes a list
        kill_gracefully $existing_pid
    fi
}

wait_for_metrics() {
    local attempt=0
    while [ $attempt -lt 20 ]; do
        if curl -sf "$METRICS_URL" >/dev/null 2>&1; then
            return 0
        fi
        sleep 0.5
        attempt=$((attempt + 1))
    done
    log_fail "/metrics on :${MCP_METRICS_PORT} did not come up; last 30 lines of $MCP_SERVER_LOG:"
    tail -n 30 "$MCP_SERVER_LOG" >&2 2>/dev/null || true
    return 1
}

# Mirrors e2e-common/lib.sh's start_server, differing only in where stderr
# goes: /dev/full, which fails every write with ENOSPC immediately, standing in
# for a log stream that has failed outright. stdout still goes to
# $MCP_SERVER_LOG; the server writes nothing there in normal operation, so the
# file stays empty of audit records by construction and the counter is the
# only place a drop can show.
start_server_with_dead_stderr() {
    local config_file="$1"
    log_info "Starting MCP server with stderr on /dev/full (config=$config_file, port=$MCP_PORT) ..."
    sweep_port "$MCP_PORT" "MCP"

    CONFIG_FILE="$config_file" \
    ENV_FILE="$ENV_FILE" \
        "$BIN_DIR/mcp-server" >"$MCP_SERVER_LOG" 2>/dev/full &
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
    log_fail "MCP server with stderr on /dev/full failed to start; its log went to /dev/full, so there is nothing to show"
    return 1
}

# The one destructive call both phases make. disconnect-client carries
# destructiveHint, so the manager emits an operation record at completion
# whatever the outcome. The client does not exist: the broker answers
# not-found, the tool reports isError, and nothing on the broker changes.
call_destructive_tool() {
    mcp_call_tool "disconnect-client" \
        '{"broker":"broker-a","msgVpnName":"default","clientName":"e2e-audit-drop-no-such-client"}'
}

# ── Phase 1 tests: level filter ──────────────────────────────────────────────

# The series must exist at 0 from process start. Without the seed, increase()
# could not fire on a process's first drop, and an absent series would be
# indistinguishable from "metrics off".
test_counter_seeded_at_zero() {
    local before
    before=$(scrape_drop_counter)
    if [ "$before" != "0" ]; then
        log_fail "mcp_audit_events_dropped_total before any drop = '${before:-<absent>}', want 0"
        return 1
    fi
}

test_counter_tracks_drop_records() {
    local before after mark response drops n_drops n_operation_drops n_operations
    before=$(scrape_drop_counter)
    require_int "$before" "counter before the call" || return 1
    mark=$(log_mark)

    response=$(call_destructive_tool) || return 1
    assert_json_field "$response" ".error" "null" \
        "disconnect-client: tool must execute (no JSON-RPC error envelope)" || return 1

    drops=$(audit_records_since "$mark" audit_drop)
    n_drops=$(count_lines <<<"$drops")
    n_operation_drops=$(jq -c 'select(.dropped_audit_event_type == "operation")' <<<"$drops" | count_lines)
    n_operations=$(audit_records_since "$mark" operation | count_lines)
    after=$(scrape_drop_counter)
    require_int "$after" "counter after the call" || return 1

    # Positive control: the level filter is in force, so no operation record
    # reached the log. Without this, a server quietly running at INFO would
    # pass the next two checks with zero drops on both sides.
    if [ "$n_operations" -ne 0 ]; then
        log_fail "found $n_operations operation record(s) at log_level=error; the level filter is not in force, so nothing here is a drop"
        return 1
    fi
    if [ "$n_operation_drops" -lt 1 ]; then
        log_fail "no audit_drop record names dropped_audit_event_type=operation ($n_drops audit_drop record(s) since the mark)"
        [ -n "$drops" ] && log_fail "  records: $drops"
        return 1
    fi
    # The two signals agree: every audit_drop written moved the counter by one,
    # and nothing else did. auth_success records (INFO) drop here too — one per
    # authenticated request the call made — which is why this compares against
    # the record count rather than a literal.
    if [ $((after - before)) -ne "$n_drops" ]; then
        log_fail "mcp_audit_events_dropped_total rose by $((after - before)) ($before → $after), but $n_drops audit_drop record(s) were written since the mark"
        log_fail "  records: $drops"
        return 1
    fi
    log_info "  counter $before → $after; $n_drops audit_drop record(s), $n_operation_drops for an operation record"
}

# ── Phase 2 test: dead log stream ────────────────────────────────────────────

# At log_level=info nothing is level-filtered, so any increment can only come
# from the write itself failing. The call must still complete: this package
# promises audit emission never fails the operation it describes, and a dead
# stderr is the sharpest test of that promise.
test_dead_sink_counter_is_the_only_signal() {
    local before after response n_records
    before=$(scrape_drop_counter)
    require_int "$before" "counter before the call" || return 1
    if [ "$before" != "0" ]; then
        log_fail "mcp_audit_events_dropped_total = $before on a fresh process, want 0"
        return 1
    fi

    response=$(call_destructive_tool) || return 1
    assert_json_field "$response" ".error" "null" \
        "disconnect-client with a dead log stream: the tool must still execute" || return 1

    after=$(scrape_drop_counter)
    require_int "$after" "counter after the call" || return 1
    n_records=$(audit_records_since 0 audit_drop | count_lines)
    if [ "$n_records" -ne 0 ]; then
        log_fail "found $n_records audit_drop record(s) in $MCP_SERVER_LOG with stderr on /dev/full; this phase is not testing a dead stream"
        return 1
    fi
    if [ "$after" -lt 1 ]; then
        log_fail "counter is $after after a destructive call through a dead log stream; the counter must move when the record cannot be written"
        return 1
    fi
    log_info "  counter 0 → $after with no audit_drop record observable anywhere: the scrape surface is the surviving signal"
}

# ── Main ─────────────────────────────────────────────────────────────────────

log_info "=== Scenario 5: Audit drop counter (SOL-154569) ==="

export OBS_AUDIT_LOG_ENABLED=true
export OBS_METRICS_ENABLED=true

# Phase 1: level filter. start_server sweeps the MCP port itself; the metrics
# port is this scenario's own.
sweep_port "$MCP_METRICS_PORT" "metrics"
write_audit_drop_config "$LEVEL_FILTER_CONFIG" error
start_server "$LEVEL_FILTER_CONFIG" || exit 1
wait_for_metrics || exit 1

run_test "counter is seeded at 0 before any drop" test_counter_seeded_at_zero
run_test "level filter: counter rises by exactly the audit_drop records written" test_counter_tracks_drop_records

# Phase 2: dead log stream.
if [ -w /dev/full ]; then
    stop_server
    sweep_port "$MCP_METRICS_PORT" "metrics"
    write_audit_drop_config "$DEAD_SINK_CONFIG" info
    start_server_with_dead_stderr "$DEAD_SINK_CONFIG" || exit 1
    wait_for_metrics || exit 1

    run_test "dead log stream: counter moves when no record can be written" test_dead_sink_counter_is_the_only_signal
else
    log_warn "SKIP dead-log-stream phase: /dev/full is not available on this host (Linux only; CI runs it)"
fi

print_summary "Audit-drop tests"
