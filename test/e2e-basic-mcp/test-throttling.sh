#!/usr/bin/env bash
# Scenario 4: SEMP throttling held end to end against a real broker (SOL-153444).
#
# Rate limiting is one of the concrete protections we tell customers about, and
# until now it was proven only by unit tests against an httptest server. This
# scenario proves it against the Dockerized broker the rest of the suite uses.
#
# ── How it measures ──────────────────────────────────────────────────────────
#
# Client-side timing of tool calls cannot answer the question: one tool call is
# not one SEMP request. So semp-tap, a recording reverse proxy, is placed
# between the MCP server and broker-a, and a dedicated `broker-throttle` alias
# is pointed at it. The broker is real and does the real work; the tap forwards
# everything and records one line per request. Because the rate limiter and the
# in-flight semaphore are keyed by broker alias, the record holds exactly this
# scenario's traffic and nothing else.
#
# The measurement window is [request received -> response headers returned],
# not full body-proxy completion, because Sender.Do drops its semaphore slot at
# header time with the body still open. The tap's package comment explains this
# at length; read it before touching an assertion here.
#
# ── Why four phases ──────────────────────────────────────────────────────────
#
# `semp:` is a single global block, and the two limits mask each other: with the
# pacer at 200ms and a local broker answering in tens of milliseconds, in-flight
# count never reaches 2, so a cap of 2 could never be shown to bind. Each limit
# therefore gets a phase in which it is the only constraint, and its own control
# that differs from it in exactly one variable:
#
#   label           interval  cap  delay   asserts
#   A pacer          200ms     10    0     min gap >= floor, and span matches interval
#   B cap            0s         2  150ms   max in-flight == 2
#   C pacer-control  0s        10    0     min gap < floor AND span < floor   (A can fail)
#   D cap-control    0s        10  150ms   max in-flight > 2                  (B can fail)
#
# C and D are the answer to "sanity-check that the assertion can actually fail".
# They are real, always-on CI assertions rather than commented-out steps, so if
# the tap ever stops being able to see a violation, the control goes red and
# names the assertion that has gone blind.
#
# One shared control would have been cheaper, and wrong: it would differ from
# the pacer phase in two variables at once (the interval AND the tap delay).
# The pacing metrics happen to read only the arrival column, which the delay
# never touches, but that is an argument rather than a guarantee and it would
# quietly stop holding the first time LOAD_CALLS or HOLD_DELAY is tuned. The
# split also has an empirical justification: with no tap delay the same load
# peaks at ~5 in flight, not 10, so a delay-free run is a poor control for the
# cap even though it is the right control for the pacer.
#
# ── Determinism ──────────────────────────────────────────────────────────────
#
# Every assertion is count-based or a generous lower bound. Nothing races a
# wall clock:
#
#   * The pacer floor is half the interval, and the span tolerance is 10% of the
#     span. Both are far wider than any jitter measured, and cost nothing —
#     see the reasoning at GAP_FLOOR_MS below.
#   * A CI runner under load makes gaps longer and requests overlap more. Both
#     are the safe direction for every assertion here.
#   * B and D hold each response an identical extra 150ms at the tap, so overlap
#     is arithmetic instead of depending on how fast the broker answers. Both
#     limits are still enforced by the server against a real broker; only the
#     measurement window is widened, and identically in the phase and in its
#     control.

set -euo pipefail
# BASH_SOURCE rather than $0 (the convention the executed-only scenario scripts
# use), so the path still resolves when this file is sourced for the helper
# tests described at the Main section below.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/helpers.sh"

# ── Knobs ────────────────────────────────────────────────────────────────────

# The pacer interval under test, and the floor assertions are made against.
# 200ms is tight enough to be unmistakable in a 12-request sample and loose
# enough that no plausible scheduling delay manufactures a violation.
THROTTLE_INTERVAL="200ms"
THROTTLE_INTERVAL_MS=200
# Pacing is asserted two ways, for the reason the unit test in
# internal/semp/rate_limiter_shared_test.go asserts it two ways.
#
# Per-pair (GAP_FLOOR_MS) catches a single unpaced admission. It is the jittery
# one: request k's transit from the limiter to the tap can be slower than
# k+1's, which shortens an observed gap even when pacing is perfect. The floor
# is therefore set at half the interval — a deliberately huge budget, because
# widening it costs nothing. Every defect this guards against (limiter
# disabled, limiter per protocol client instead of per broker, limiter not
# shared) produces gaps near ZERO, not gaps a few milliseconds short, so a
# 100ms floor is exactly as damning as a 190ms one. The measured spread on an
# idle machine was 187-199ms; the target is a 2-vCPU CI runner sharing cores
# with two broker containers, which is the case that cannot be measured here,
# so the budget is sized for it rather than for the machine it was written on.
#
# Aggregate (SPAN_TOLERANCE_MS) is the precise one: per-pair jitter cancels out
# over a run, so the total span is stable and a tight tolerance is safe there.
# It is what actually catches systematic under-pacing that the generous
# per-pair floor waves through — an interval quietly applied at half its
# configured value shows up as ~1000ms against a ~1900ms floor.
GAP_FLOOR_MS=$((THROTTLE_INTERVAL_MS / 2))
SPAN_TOLERANCE_MS=200

# The cap under test, and the slack value used where the cap must provably not
# be the constraint (mirrors the reasoning in rate_limiter_shared_test.go).
THROTTLE_CAP=2
SLACK_CAP=10

# Concurrent tool calls per phase. Comfortably above THROTTLE_CAP so the cap
# binds in phase B, and large enough that phase A yields ten asserted gaps.
LOAD_CALLS=12

# Per-response hold at the tap in phases B and C. See "Determinism" above.
HOLD_DELAY="150ms"

# get-vpn-status is a single-step composite with no pagination and no fan-out:
# exactly one SEMP request per tool call. That keeps the record trivially
# interpretable and needs no fixture beyond the default VPN.
LOAD_TOOL="get-vpn-status"

# ── State ────────────────────────────────────────────────────────────────────

# Installed at the bottom of the file, after the sourcing guard — see there.
cleanup() {
    stop_server
    stop_semp_tap
    return 0
}

# ── Record analysis ──────────────────────────────────────────────────────────
# Record lines are appended in completion order, so anything positional must
# sort by start timestamp first. Format: seq,start_ns,end_ns,status,method,path

# Every numeric comparison below goes through this first.
#
# `[ "" -lt 13 ]` exits 2, not 1, and an `if` whose condition exits non-zero
# takes the else branch — so an assertion function that only ever tests
# `if [ "$n" -lt X ]; then fail; fi` falls off the end and RETURNS SUCCESS when
# its input is empty. Arithmetic is worse: $(( )) silently reads an empty
# string as 0, which turned a missing span into a negative floor that every
# value cleared. A scenario whose whole thesis is "no assertion may pass
# without evidence" cannot have guards that pass on no evidence.
#   $1 value   $2 label for the failure message
require_int() {
    if [[ ! "$1" =~ ^-?[0-9]+$ ]]; then
        log_fail "$2: expected a number from the record analysis, got '${1}' — treating as a failure"
        return 1
    fi
    return 0
}

record_count() { wc -l < "$1" | tr -d ' '; }

# Count of requests the broker did not answer with 200. A phase where every call
# errored would otherwise show a beautifully paced record of nothing, and every
# "gap >= floor" assertion would pass vacuously.
record_bad_status() { awk -F, '$4 != 200 { n++ } END { print n + 0 }' "$1"; }

# Smallest gap between consecutive arrivals, in milliseconds, after ignoring the
# first $2 arrivals. -1 when there are too few arrivals left to form a gap.
#
# The skip exists because RateLimiter is a bare time.Ticker, not a token bucket.
# Its channel buffers one tick, so idle time is credit and the first request
# after an idle period is admitted with no wait at all. Phase A skips 2: the
# warm-up call, and the one load request that spends the buffered tick. Phases
# with the pacer off skip only the warm-up.
record_min_gap_ms() {
    sort -t, -k2,2n "$1" | awk -F, -v skip="$2" '
        { n++; s[n] = $2 }
        END {
            min = -1
            for (i = skip + 2; i <= n; i++) {
                gap = s[i] - s[i-1]
                if (min < 0 || gap < min) min = gap
            }
            if (min < 0) { print -1 } else { printf "%d\n", min / 1000000 }
        }'
}

# Elapsed milliseconds from the first asserted arrival to the last, and the
# number of gaps that span covers, as "span_ms gaps". Same $2 skip semantics as
# record_min_gap_ms. Prints "-1 0" when there are too few arrivals to span.
#
# Aggregate pacing is checked against this rather than against the sum of the
# individual gaps so that transit jitter cancels: one gap arriving 13ms early is
# necessarily followed by one arriving 13ms late, and the span is unmoved.
record_span_ms() {
    sort -t, -k2,2n "$1" | awk -F, -v skip="$2" '
        { n++; s[n] = $2 }
        END {
            first = skip + 1
            if (n - first < 1) { print "-1 0"; exit }
            printf "%d %d\n", (s[n] - s[first]) / 1000000, n - first
        }'
}

# Peak number of requests in flight at the broker, by sweep line over the
# [start, end) intervals. On a tie the -1 sorts before the +1, so a request that
# ends exactly as another starts is not counted as an overlap.
record_max_inflight() {
    awk -F, '{ print $2 ",1"; print $3 ",-1" }' "$1" \
        | sort -t, -k1,1n -k2,2n \
        | awk -F, '{ cur += $2; if (cur > max) max = cur } END { print max + 0 }'
}

# ── Load driver ──────────────────────────────────────────────────────────────

# One warm-up call, then $LOAD_CALLS concurrent tools/call on a single session.
#
# A single session is deliberate: go-sdk calls jsonrpc2.Async before dispatching
# anything but `initialize`, so calls on one session genuinely overlap
# server-side, and one session avoids the per-handshake stagger that N sessions
# would add. If that ever regressed to serialised dispatch, phase C's
# "max in-flight > 2" fails loudly rather than any phase passing quietly.
#
# The warm-up is what makes the ticker's buffered-tick behaviour deterministic
# instead of accidental. It creates the BrokerClient (and starts the ticker),
# and the sleep that follows guarantees exactly one buffered tick is waiting
# when the load begins — the case record_min_gap_ms's skip accounts for.
drive_load() {
    local args
    args=$(jq -nc '{broker:"broker-throttle",msgVpnName:"default"}')

    log_info "  warm-up call (creates the broker client and starts the ticker) ..."
    mcp_call_tool "$LOAD_TOOL" "$args" >/dev/null || {
        log_fail "  warm-up call failed"
        return 1
    }
    # Longer than the pacer interval, so the ticker has produced a tick and the
    # channel holds exactly one (a Ticker drops ticks rather than queueing them).
    sleep 0.5

    local sid
    sid=$(mcp_initialize) || return 1

    log_info "  firing $LOAD_CALLS concurrent $LOAD_TOOL calls ..."
    local pids=() i
    for i in $(seq 1 "$LOAD_CALLS"); do
        mcp_request "$sid" "$(jq -nc --argjson id "$i" --arg t "$LOAD_TOOL" --argjson a "$args" \
            '{jsonrpc:"2.0",id:$id,method:"tools/call",params:{name:$t,arguments:$a}}')" \
            >/dev/null 2>&1 &
        pids+=("$!")
    done
    local p
    for p in "${pids[@]}"; do
        wait "$p" || true
    done
}

# ── Phase runner ─────────────────────────────────────────────────────────────

# Bring up a tap and a server configured for one phase, drive the load, then
# tear both down so the record is complete before it is read.
#   $1 phase label   $2 request_min_interval   $3 max_concurrent   $4 tap delay
#   $5 record file
run_phase() {
    local label="$1" interval="$2" cap="$3" delay="$4" record="$5"

    log_info "── Phase $label: request_min_interval=$interval max_concurrent_per_broker=$cap (tap delay=$delay)"

    stop_server
    stop_semp_tap

    start_semp_tap "$BROKER_A_URL" "$record" "$delay" || return 1

    # Configs live under bin/ rather than $TMPDIR, for the reason e2e-oauth
    # keeps its own there: on a CI-only failure they are the only record of
    # which limits were actually in force, and the workflow collects them.
    local config="$BIN_DIR/mcp-config-throttle-$label.yaml"
    write_throttle_config "$config" "$interval" "$cap"
    start_server "$config" || return 1

    drive_load || return 1

    # Stop the server before the tap so no late request lands after the record
    # has been read, and stop the tap before analysing so its last line is out.
    stop_server
    stop_semp_tap

    log_info "  recorded $(record_count "$record") requests → $record"
}

# Shared guards: the load actually reached the broker, and the broker answered.
# Both must hold before any pacing or concurrency number means anything.
assert_record_sane() {
    local record="$1" label="$2"
    local n bad
    n=$(record_count "$record")
    bad=$(record_bad_status "$record")
    require_int "$n" "$label (record count)" || return 1
    require_int "$bad" "$label (bad-status count)" || return 1

    # Warm-up plus the load. Greater-or-equal rather than equality so the check
    # survives a tool gaining a step, while still catching a load that silently
    # did not run.
    if [ "$n" -lt $((LOAD_CALLS + 1)) ]; then
        log_fail "$label: broker saw $n requests, want at least $((LOAD_CALLS + 1)) — the load did not reach the broker"
        return 1
    fi
    if [ "$bad" -ne 0 ]; then
        log_fail "$label: $bad of $n requests did not return 200 — a failed load cannot prove anything about pacing"
        return 1
    fi
}

# ── Phase A: the pacer ───────────────────────────────────────────────────────

RECORD_A="$BIN_DIR/throttle-record-pacer.csv"

test_pacer_spaces_requests() {
    assert_record_sane "$RECORD_A" "pacer" || return 1

    local min_gap
    min_gap=$(record_min_gap_ms "$RECORD_A" 2)
    require_int "$min_gap" "pacer (min gap)" || return 1
    if [ "$min_gap" -lt 0 ]; then
        log_fail "pacer: too few arrivals to form a gap"
        return 1
    fi
    if [ "$min_gap" -lt "$GAP_FLOOR_MS" ]; then
        log_fail "pacer: smallest gap between consecutive SEMP requests was ${min_gap}ms, want >= ${GAP_FLOOR_MS}ms"
        log_fail "  semp.request_min_interval=$THROTTLE_INTERVAL is not being honored end to end against the broker"
        return 1
    fi

    local span gaps span_floor
    read -r span gaps < <(record_span_ms "$RECORD_A" 2)
    require_int "$span" "pacer (span)" || return 1
    require_int "$gaps" "pacer (gap count)" || return 1
    # Without this, zero gaps would make the floor negative and the comparison
    # below would pass on an empty sample — the exact vacuous pass this whole
    # scenario exists to rule out.
    if [ "$gaps" -lt 1 ]; then
        log_fail "pacer: no paced gaps in the record to aggregate over"
        return 1
    fi
    span_floor=$((gaps * THROTTLE_INTERVAL_MS - SPAN_TOLERANCE_MS))
    if [ "$span" -lt "$span_floor" ]; then
        log_fail "pacer: $gaps paced requests spanned ${span}ms, want >= ${span_floor}ms"
        log_fail "  individual gaps cleared the floor but the aggregate rate is faster than semp.request_min_interval=$THROTTLE_INTERVAL"
        return 1
    fi

    log_info "  smallest gap ${min_gap}ms >= floor ${GAP_FLOOR_MS}ms; $gaps gaps spanned ${span}ms >= ${span_floor}ms (interval $THROTTLE_INTERVAL)"
}

# ── Phase B: the in-flight cap ───────────────────────────────────────────────

RECORD_B="$BIN_DIR/throttle-record-cap.csv"

test_cap_bounds_inflight() {
    assert_record_sane "$RECORD_B" "cap" || return 1

    local peak
    peak=$(record_max_inflight "$RECORD_B")
    require_int "$peak" "cap (peak in-flight)" || return 1

    if [ "$peak" -gt "$THROTTLE_CAP" ]; then
        log_fail "cap: broker saw $peak concurrent SEMP requests, want at most $THROTTLE_CAP"
        log_fail "  semp.max_concurrent_per_broker=$THROTTLE_CAP is not being honored end to end against the broker"
        return 1
    fi
    # Equality, not just the bound: a peak below the cap would mean the load
    # never pressed against it, and the upper bound above would have passed for
    # the wrong reason.
    if [ "$peak" -lt "$THROTTLE_CAP" ]; then
        log_fail "cap: broker saw only $peak concurrent SEMP requests with $LOAD_CALLS calls in flight"
        log_fail "  the cap of $THROTTLE_CAP was never pressed, so this phase proves nothing — check the tap delay and the load driver"
        return 1
    fi
    log_info "  peak in-flight $peak == cap $THROTTLE_CAP"
}

# ── Phases C and D: the controls ─────────────────────────────────────────────
#
# Each control differs from the phase it validates in exactly ONE variable.
# That matters more than it might look. A single shared control would differ
# from the pacer phase in two variables at once (the interval AND the tap
# delay), and while the pacing metrics happen to read only the arrival column
# — which the delay never touches — that invariance is an argument, not a
# guarantee, and it would quietly stop holding the first time LOAD_CALLS or
# HOLD_DELAY is tuned. Two controls cost one extra server restart and remove
# the argument entirely.
#
#   C  vs A: interval 200ms -> 0s     (cap 10 and delay 0 held constant)
#   D  vs B: cap 2 -> 10              (interval 0s and delay 150ms held constant)

RECORD_C="$BIN_DIR/throttle-record-pacer-control.csv"
RECORD_D="$BIN_DIR/throttle-record-cap-control.csv"

# Proves phase A's two pacing assertions can actually fail. If this passes while
# A also passes, the pacer is doing real work. If this fails, the measurement has
# gone blind and A's green is worthless — which is exactly what a self-checking
# test should tell you.
test_pacer_control_detects_unpaced() {
    assert_record_sane "$RECORD_C" "pacer-control" || return 1

    local min_gap
    min_gap=$(record_min_gap_ms "$RECORD_C" 1)
    require_int "$min_gap" "pacer-control (min gap)" || return 1
    if [ "$min_gap" -lt 0 ]; then
        log_fail "pacer-control: too few arrivals to form a gap"
        return 1
    fi
    if [ "$min_gap" -ge "$GAP_FLOOR_MS" ]; then
        log_fail "pacer-control: with request_min_interval=0s the smallest gap was still ${min_gap}ms (>= floor ${GAP_FLOOR_MS}ms)"
        log_fail "  the per-pair assertion in phase A cannot tell a paced run from an unpaced one, so it is not testing anything"
        return 1
    fi

    # Phase A asserts pacing twice, so its control has to trip both. Without
    # this, the aggregate check in phase A would have no proof it can fail.
    local span gaps span_floor
    read -r span gaps < <(record_span_ms "$RECORD_C" 1)
    require_int "$span" "pacer-control (span)" || return 1
    require_int "$gaps" "pacer-control (gap count)" || return 1
    if [ "$gaps" -lt 1 ]; then
        log_fail "pacer-control: no gaps in the record to aggregate over"
        return 1
    fi
    span_floor=$((gaps * THROTTLE_INTERVAL_MS - SPAN_TOLERANCE_MS))
    if [ "$span" -ge "$span_floor" ]; then
        log_fail "pacer-control: with request_min_interval=0s $gaps requests still spanned ${span}ms (>= ${span_floor}ms)"
        log_fail "  the aggregate assertion in phase A cannot tell a paced run from an unpaced one"
        return 1
    fi

    log_info "  unpaced run shows gap ${min_gap}ms < floor ${GAP_FLOOR_MS}ms and span ${span}ms < ${span_floor}ms"
}

# Proves phase B's cap assertion can actually fail. Identical to B in every
# respect except the cap itself.
test_cap_control_detects_uncapped() {
    assert_record_sane "$RECORD_D" "cap-control" || return 1

    local peak
    peak=$(record_max_inflight "$RECORD_D")
    require_int "$peak" "cap-control (peak in-flight)" || return 1

    if [ "$peak" -le "$THROTTLE_CAP" ]; then
        log_fail "cap-control: with max_concurrent_per_broker=$SLACK_CAP the peak in-flight was only $peak (<= $THROTTLE_CAP)"
        log_fail "  the assertion in phase B cannot tell a capped run from an uncapped one, so it is not testing anything"
        return 1
    fi
    log_info "  uncapped run shows peak in-flight $peak > cap $THROTTLE_CAP"
}

# ── Main ─────────────────────────────────────────────────────────────────────

# Sourcing this file yields the record-analysis helpers above without running
# any phase. test-throttling-analysis.sh is the consumer: it exercises the gap
# and sweep-line arithmetic on synthetic records, which is the one part of this
# scenario that needs neither a broker nor Docker. run-all.sh executes this file,
# so the suite is unaffected.
#
# The guard sits above the trap deliberately. Installing an EXIT trap in a
# sourcing shell would silently replace whatever trap that shell already had.
if [ "${BASH_SOURCE[0]}" != "$0" ]; then
    return 0
fi

trap cleanup EXIT INT TERM HUP

log_info "=== Scenario 4: SEMP throttling (SOL-153444) ==="
log_info ""

# Check the measurement before spending three server restarts on it. If the gap
# or sweep-line arithmetic is wrong, every verdict below is meaningless, and
# this says so in under a second instead of after a minute of phases.
if ! bash "$SCRIPT_DIR/test-throttling-analysis.sh"; then
    log_fail "Record-analysis self-test failed; the throttling assertions cannot be trusted"
    exit 1
fi

check_build_deps
wait_for_all_brokers 90
build_server
build_semp_tap

# Scenarios 1-3 left a server on $MCP_PORT. Take it over deliberately rather
# than letting start_server's port sweep do it silently, so a stale process is
# never mistaken for one of this scenario's own phases. This scenario runs last
# in run-all.sh precisely so it is free to do this.
if [ -f "$BIN_DIR/mcp-server.pid" ]; then
    log_info "Stopping the shared suite server before taking over port $MCP_PORT ..."
    kill_gracefully "$(cat "$BIN_DIR/mcp-server.pid")"
    rm -f "$BIN_DIR/mcp-server.pid"
fi

# Each phase runs even if an earlier one failed, so a pacer regression does not
# hide the cap result and neither hides either control's verdict.
#
# Every control holds all but one variable constant against the phase it
# validates: C changes only the interval against A, D changes only the cap
# against B.
PHASE_A_OK=0
PHASE_B_OK=0
PHASE_C_OK=0
PHASE_D_OK=0

#                label           interval             cap              delay          record
if run_phase "pacer"         "$THROTTLE_INTERVAL" "$SLACK_CAP"    "0"           "$RECORD_A"; then
    PHASE_A_OK=1
fi
if run_phase "cap"           "0s"                 "$THROTTLE_CAP" "$HOLD_DELAY" "$RECORD_B"; then
    PHASE_B_OK=1
fi
if run_phase "pacer-control" "0s"                 "$SLACK_CAP"    "0"           "$RECORD_C"; then
    PHASE_C_OK=1
fi
if run_phase "cap-control"   "0s"                 "$SLACK_CAP"    "$HOLD_DELAY" "$RECORD_D"; then
    PHASE_D_OK=1
fi

log_info ""

# A phase that could not even run (broker gone, port taken, tap failed to bind)
# is a failure, not a skip. Counting it as a failure keeps the summary table's
# arithmetic honest and stops a broken harness reading as a green suite.
report_phase() {
    local ok="$1" name="$2" func="$3"
    if [ "$ok" -eq 1 ]; then
        run_test "$name" "$func"
        return
    fi
    TESTS_RUN=$((TESTS_RUN + 1))
    TESTS_FAILED=$((TESTS_FAILED + 1))
    log_fail "$name (phase did not complete — see the log above)"
}

report_phase "$PHASE_A_OK" "Pacer spaces SEMP requests by request_min_interval" test_pacer_spaces_requests
report_phase "$PHASE_B_OK" "In-flight cap bounds concurrent SEMP requests"      test_cap_bounds_inflight
report_phase "$PHASE_C_OK" "Control: pacer off trips both pacing assertions"    test_pacer_control_detects_unpaced
report_phase "$PHASE_D_OK" "Control: cap slack trips the in-flight assertion"   test_cap_control_detects_uncapped

print_summary "Throttling tests"
