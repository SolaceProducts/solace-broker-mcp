#!/usr/bin/env bash
# Self-test for the record-analysis arithmetic in test-throttling.sh.
#
# The throttling scenario's verdict is only as good as the four helpers that
# turn a request record into numbers. Those helpers need neither a broker nor
# Docker to exercise, so they are checked here against synthetic records with
# known answers. test-throttling.sh runs this first, before spending four
# server restarts on a measurement it has not verified; it also runs standalone
# in about a second:
#
#   bash test/e2e-basic-mcp/test-throttling-analysis.sh
#
# Sourcing test-throttling.sh returns at its own guard before any phase runs,
# so this only picks up the helpers.

set -uo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./test-throttling.sh
source "$SCRIPT_DIR/test-throttling.sh"

# test-throttling.sh sets -e for its own run. This file deliberately keeps
# going after a failed check so one broken helper does not hide the other
# fifteen results, and it reports the tally at the end instead.
set +e

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

FAILURES=0
check() { # label expected actual
    if [ "$2" = "$3" ]; then
        log_ok "  $1 => $3"
    else
        log_fail "  $1 => got '$3', want '$2'"
        FAILURES=$((FAILURES + 1))
    fi
}

MS=1000000
row() { # seq start_ms end_ms status
    printf '%d,%d,%d,%d,GET,/SEMP/v2/monitor/msgVpns/default\n' "$1" $(( $2 * MS )) $(( $3 * MS )) "$4"
}

log_info "Record-analysis self-test"

# ── A paced record, shaped exactly like phase A's ────────────────────────────
# Arrival 1 is the warm-up. Arrival 2 spends the ticker's buffered tick and so
# sits only 80ms after it. Arrivals 3+ are paced at 200ms. This is the shape the
# skip-2 in phase A exists for, and the skip-0 case below is what it prevents.
paced="$TMP/paced.csv"
: > "$paced"
row 1 0 5 200 >> "$paced"
row 2 700 705 200 >> "$paced"
row 3 780 785 200 >> "$paced"
t=780
for i in 4 5 6 7 8 9 10 11 12 13; do
    t=$((t + 200))
    row "$i" "$t" $((t + 5)) 200 >> "$paced"
done

check "paced: count"                13        "$(record_count "$paced")"
check "paced: bad statuses"         0         "$(record_bad_status "$paced")"
check "paced: min gap, skip 2"      200       "$(record_min_gap_ms "$paced" 2)"
check "paced: min gap, skip 0"      80        "$(record_min_gap_ms "$paced" 0)"
check "paced: span, skip 2"         "2000 10" "$(record_span_ms "$paced" 2)"
check "paced: span, skip 0"         "2780 12" "$(record_span_ms "$paced" 0)"
check "paced: peak in-flight"       1         "$(record_max_inflight "$paced")"

# ── An unpaced record, shaped like a pacer-control run ───────────────────────
unpaced="$TMP/unpaced.csv"
: > "$unpaced"
row 1 0 150 200 >> "$unpaced"
t=500
for i in 2 3 4 5 6 7 8 9 10 11 12 13; do
    row "$i" "$t" $((t + 150)) 200 >> "$unpaced"
    t=$((t + 2))
done
check "unpaced: min gap, skip 1"    2         "$(record_min_gap_ms "$unpaced" 1)"
check "unpaced: peak in-flight"     12        "$(record_max_inflight "$unpaced")"

# ── A capped record: strictly two at a time ──────────────────────────────────
capped="$TMP/capped.csv"
: > "$capped"
row 1 0 150 200 >> "$capped"
n=1
for wave in 0 1 2 3 4 5; do
    s=$((500 + wave * 150))
    n=$((n + 1)); row "$n" "$s" $((s + 150)) 200 >> "$capped"
    n=$((n + 1)); row "$n" "$s" $((s + 150)) 200 >> "$capped"
done
check "capped: peak in-flight"      2         "$(record_max_inflight "$capped")"

# ── Touching intervals are not an overlap ────────────────────────────────────
# The sweep line must apply the -1 before the +1 on a tie, or a correct cap of
# N reads as N+1 whenever one request ends exactly as the next begins.
tie="$TMP/tie.csv"
{ row 1 0 100 200; row 2 100 200 200; } > "$tie"
check "touching intervals: peak"    1         "$(record_max_inflight "$tie")"

triple="$TMP/triple.csv"
{ row 1 0 100 200; row 2 10 90 200; row 3 20 80 200; } > "$triple"
check "nested intervals: peak"      3         "$(record_max_inflight "$triple")"

# ── Completion order is not arrival order ────────────────────────────────────
# The tap appends as requests finish, so a slow early request lands after a fast
# later one. Everything positional must sort by arrival first.
ooo="$TMP/out-of-order.csv"
{ row 1 300 310 200; row 2 0 900 200; row 3 600 610 200; } > "$ooo"
check "out-of-order: min gap"       300       "$(record_min_gap_ms "$ooo" 0)"
check "out-of-order: peak"          2         "$(record_max_inflight "$ooo")"

# ── Degenerate inputs must not read as success ───────────────────────────────
empty="$TMP/empty.csv"; : > "$empty"
check "empty: count"                0         "$(record_count "$empty")"
check "empty: min gap"              -1        "$(record_min_gap_ms "$empty" 2)"
check "empty: span"                 "-1 0"    "$(record_span_ms "$empty" 2)"
check "empty: peak"                 0         "$(record_max_inflight "$empty")"

single="$TMP/single.csv"
{ row 1 0 1 200; row 2 5 6 200; } > "$single"
check "one arrival past skip: span" "-1 0"    "$(record_span_ms "$single" 1)"
check "one arrival past skip: gap"  -1        "$(record_min_gap_ms "$single" 1)"

errs="$TMP/errors.csv"
{ row 1 0 1 502; row 2 1 2 200; row 3 2 3 0; } > "$errs"
check "errors counted"              2         "$(record_bad_status "$errs")"

# ── require_int rejects everything a broken helper could emit ────────────────
# This is the guard that stops `[ "" -lt 13 ]` (which exits 2, not 1) from
# falling through an `if` and returning success from an assertion function.
int_guard_errors=0
for bad in "" "abc" "1 2" "-" "1.5"; do
    if require_int "$bad" "self-test" 2>/dev/null; then
        int_guard_errors=$((int_guard_errors + 1))
    fi
done
for good in 0 7 -1 12345; do
    if ! require_int "$good" "self-test" 2>/dev/null; then
        int_guard_errors=$((int_guard_errors + 1))
    fi
done
check "require_int accept/reject"   0         "$int_guard_errors"

if [ "$FAILURES" -ne 0 ]; then
    log_fail "Record-analysis self-test: $FAILURES check(s) failed"
    exit 1
fi
log_ok "Record-analysis self-test: all checks passed"
