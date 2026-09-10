#!/usr/bin/env bash
# Split-host: Box B — runs MCP + memsampler + sampler.sh (MCP-only).
# mock-semp and loadgen live on Box A (see run-loadgen.sh).
#
# Usage:
#   MOCK_HOST=<box-a-ip> ./run-mcp.sh
#   MOCK_HOST=198.51.100.30 DURATION=90s ./run-mcp.sh
#
# Env:
#   MOCK_HOST      required — LAN address of Box A running mock-semp
#   DURATION       how long to hold MCP up for the loadgen run (default 90s;
#                  should exceed the loadgen -duration you use on Box A). A
#                  number of s, m or h — "1m30s" and "500ms" are refused, since
#                  the samplers count in whole seconds.
#   WARMUP         set this to the WARMUP Box A is using (default unset). The
#                  two boxes share no channel, so this box cannot learn it:
#                  under a warmup the load lasts WARMUP + DURATION, and without
#                  it this box's samplers stop before the load does and the
#                  server-side CPU window misses exactly the tail the stats
#                  describe.
#   CONFIG_FILE    MCP broker config (default ./broker-config.mock.yaml). Point
#                  this at a per-run copy to vary request_min_interval; the file
#                  used is copied into the run directory so the numbers stay
#                  traceable to it.
#   BROKER_USERNAME / BROKER_PASSWORD   (default perf/perf; mock accepts anything)
#   PORT_WAIT_SECS how long to wait for :9090 to be released by a previous
#                  sweep point before giving up (default 60)
#   NOFILE         descriptor limit to request for the server (default 1048576;
#                  falls back to the hard limit, and both are recorded)
#   RIG_NOTE       free-text note about this host. Control characters are
#                  flattened, `=` becomes `:` and the value is capped at 200
#                  characters so the record stays parseable — the substitutions
#                  are reported on stderr.

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"
bin="$here/bin"
runs="$bin/runs/$(date +%Y%m%d-%H%M%S)-mcp"
mkdir -p "$runs"

# shellcheck source=lib.sh
source "$here/lib.sh"
record="$runs/run-record.mcp"

: "${MOCK_HOST:?MOCK_HOST unset — set to the Box A LAN IP, e.g. MOCK_HOST=198.51.100.30}"
export MOCK_HOST
export BROKER_USERNAME="${BROKER_USERNAME:-perf}"
export BROKER_PASSWORD="${BROKER_PASSWORD:-perf}"
DURATION="${DURATION:-90s}"
CONFIG_FILE="${CONFIG_FILE:-$here/broker-config.mock.yaml}"
PORT_WAIT_SECS="${PORT_WAIT_SECS:-60}"
WARMUP="${WARMUP:-}"

# Both size this box's sampler windows, so both are resolved once, here, and a
# value that cannot be resolved stops the run before the server is started.
# Stricter than Go's parser on purpose — see run.sh for why the old two-parser
# arrangement sampled the wrong span in silence.
duration_secs=$(perf_duration_secs "$DURATION") || exit 2
# Zero is a legitimate WARMUP and never a legitimate DURATION: `loadgen`
# rejects a non-positive -duration, and `memsampler -duration 0s` means "run
# until the process disappears", so a zero would sail through this preflight
# and hang the run at its final wait. Refuse it here, where refusing is free.
if (( duration_secs == 0 )); then
  echo "DURATION must be greater than zero, got: '$DURATION'" >&2
  exit 2
fi
warmup_secs=0
if [[ -n "$WARMUP" ]]; then
  warmup_secs=$(perf_duration_secs "$WARMUP") || exit 2
  # A zero warmup is the same run as no warmup; normalise so the record does
  # not claim one.
  (( warmup_secs > 0 )) || WARMUP=""
fi
# The load on the other box lasts WARMUP + DURATION; this box has to hold, and
# sample, for at least that long.
load_secs=$(( duration_secs + warmup_secs ))

# Fail loud on a bad path. Silently falling back to the committed config would
# make every run measure the interval that file happens to ship, and a sweep
# meant to vary the pacer would report one setting under five names.
if [[ ! -f "$CONFIG_FILE" ]]; then
  echo "CONFIG_FILE not found: $CONFIG_FILE" >&2
  exit 2
fi
# MCP is launched with cwd=$repo_root, so a relative path would resolve against
# the repo root rather than against the caller. Pin it here instead.
CONFIG_FILE="$(realpath "$CONFIG_FILE")"

for b in memsampler mcp-server; do
  if [[ ! -x "$bin/$b" ]]; then
    echo "missing $bin/$b — run ./build.sh first" >&2
    exit 2
  fi
done

# Wait for the port rather than aborting on it. A back-to-back sweep point was
# lost this way: the previous point's MCP still held :9090 when the next one
# started, and an immediate abort turns a two-second wait into a missing data
# point in the middle of a sweep. Bounded — a port held past the timeout is a
# stuck process, not a slow one — and starting anyway is not an option, because
# the run would then measure the previous server.
perf_wait_ports_free "$PORT_WAIT_SECS" 9090 || exit 2

# Raise the descriptor limit before anything is launched, so the server
# inherits it. Nothing in this suite, cmd/ or deploy/ raised RLIMIT_NOFILE
# before, which means it silently varied with whatever the login shell handed
# the operator — a result-moving input that was invisible in the output.
#
# Two idle connection pools per broker, each holding up to
# max_concurrent_per_broker for IdleConnTimeout, put the server's own
# descriptor use at up to twice the cap per broker under a protocol-alternating
# workload. The load generator's inbound sessions are one socket each and
# dominate a 2000-caller run, but those land on the load box, not this one.
perf_raise_nofile "${NOFILE:-1048576}"
echo "== 0. descriptor limit: requested $PERF_NOFILE_REQUESTED, granted $PERF_NOFILE_GRANTED"

# Confirm Box A's mock is reachable before spinning MCP; a silent
# unreachable-mock reads as a broken MCP in the load run.
if ! (exec 3<>/dev/tcp/"$MOCK_HOST"/18081) 2>/dev/null; then
  echo "cannot reach mock at $MOCK_HOST:18081 — is run-loadgen.sh already up on Box A?" >&2
  echo "(mock-semp must be listening before MCP dials it)" >&2
  exit 2
fi
exec 3<&- 3>&-

mcp_pid= mem_pid= top_pid=
kill_tree() {
  local pid=$1
  [[ -z "$pid" ]] && return
  local pgid
  pgid=$(ps -o pgid= "$pid" 2>/dev/null | tr -d ' ') || true
  if [[ -n "$pgid" ]]; then
    kill -TERM -"$pgid" 2>/dev/null || true
    sleep 0.5
    kill -KILL -"$pgid" 2>/dev/null || true
  else
    kill -TERM "$pid" 2>/dev/null || true
  fi
  wait "$pid" 2>/dev/null || true
}
# Set once the peaks have been written, so a run interrupted with Ctrl-C does
# not append them twice: cleanup is the handler for INT and TERM as well as
# EXIT, and its `exit` re-enters it through the EXIT trap.
peaks_recorded=0
# Set at the end of the main flow. cleanup() is reached on every path, so this
# is what tells it whether the peaks it is about to record cover a whole run.
run_finished=0
cleanup() {
  local rc=$?
  set +e
  # kill_tree waits for each pid, so mem.csv is final and complete by the time
  # the peaks are read below.
  kill_tree "$top_pid"
  kill_tree "$mem_pid"
  kill_tree "$mcp_pid"
  # The peaks are recorded HERE, not in the main flow, because the runs that
  # lose them are exactly the runs that never reach the main flow's end. A
  # terminated run used to produce a record short by two fields — fd_peak and
  # threads_peak — and Run H of the last campaign, whose whole purpose was
  # measuring fd_peak under a raised connection cap, came back without it.
  #
  # This covers EXIT, INT and TERM. A SIGKILL, or an instance stopped out from
  # under the run, still loses them: nothing shell-side can survive that, and
  # the record simply stays short.
  if (( ! peaks_recorded )); then
    peaks_recorded=1
    perf_record_fd_peak "$record" "$runs/mem.csv"
    # A peak over a run that was cut short is not the peak the run would have
    # reached, and on this box nothing else marks a short record: it never
    # stamps a load window by design, so an interrupted record was otherwise
    # field-for-field identical to a complete one. fd_peak is the number Run H
    # exists to quote, so it says which kind it is.
    if (( run_finished )); then
      perf_record_kv "$record" fd_peak_source complete
    else
      perf_record_kv "$record" run_terminated true
      perf_record_kv "$record" fd_peak_source partial
    fi
    # Assert only where there were samples to take a peak from: -s is true for
    # a header-only CSV, which is what a run terminated inside the first
    # sampling interval leaves, so count rows rather than bytes. A run that
    # aborts in preflight — a held port, a failed fidelity gate — has no
    # mem.csv and no peaks to miss, and warning there would bury the real
    # error under a provenance complaint.
    if [[ -r "$runs/mem.csv" ]] && (( $(wc -l <"$runs/mem.csv") > 1 )); then
      perf_record_assert_fields "$record" fd_peak threads_peak
    fi
  fi
  echo "artifacts: $runs"
  exit "$rc"
}
trap cleanup EXIT INT TERM

wait_for_http() {
  local url=$1 name=$2
  for _ in $(seq 1 60); do
    if curl -fsS -o /dev/null "$url"; then return 0; fi
    sleep 0.5
  done
  echo "timed out waiting for $name at $url" >&2
  return 1
}

# The run record is what makes this run comparable with the next one. Written
# before MCP starts so a run that dies during startup still says what it was.
perf_record_begin "$record" mcp "$runs"
perf_record_rig "$record"
perf_record_code "$record" "$repo_root" "$bin" mcp-server memsampler
perf_record_fixtures "$record" "$here"
perf_record_comment "$record" "workload (driven from the load box; this box only holds MCP up)"
perf_record_kv "$record" hold_duration "$DURATION"
# What Box A told us it is discarding from its stats. This box drives no load
# and cannot verify it; it is recorded so the two halves can be read together.
perf_record_kv "$record" stats_warmup "${WARMUP:-none}"
perf_record_kv "$record" mock_host "$MOCK_HOST"
perf_record_kv "$record" config_file "$CONFIG_FILE"
perf_record_kv "$record" nofile_requested "$PERF_NOFILE_REQUESTED"
perf_record_kv "$record" nofile_granted "$PERF_NOFILE_GRANTED"

echo "== 1. MCP server on :9090 (config: $CONFIG_FILE, MOCK_HOST=$MOCK_HOST)"
# Keep the exact config alongside the numbers it produced. Recording only the
# filename is not enough: the per-run copies are local and get overwritten.
cp "$CONFIG_FILE" "$runs/broker-config.used.yaml"
# Exec the prebuilt binary, not `go run`: `go run` runs the compiled program
# as a child process, so $mcp_pid would be the toolchain wrapper and the
# memsampler in step 2 would sample that instead of MCP.
setsid bash -c "cd '$repo_root' && CONFIG_FILE='$CONFIG_FILE' exec '$bin/mcp-server'" \
  >"$runs/mcp.log" 2>&1 &
mcp_pid=$!
wait_for_http "http://localhost:9090/health" mcp-server

# Now that the server has reported its own effective configuration, record the
# four settings that move admission behaviour, and the descriptor limit the
# process actually runs under (its own /proc view, not this shell's — the two
# diverge across a re-exec).
perf_record_kv "$record" mcp_pid "$mcp_pid"
perf_record_admission "$record" "$runs/mcp.log" "$runs/broker-config.used.yaml"
perf_record_proc_nofile "$record" "$mcp_pid"
# How much processor the Go runtime was entitled to. Without this, two runs on
# one box with different GOMAXPROCS write records identical in every field.
perf_record_runtime_cpu "$record" "$mcp_pid"

# This box drives no load, so it cannot stamp the load window: the load runs on
# the other box and the two share no channel by design. Say so in the record
# rather than inferring a boundary — a guessed window would poison every later
# comparison, and both run directories are archived together anyway.
perf_no_load_window "$record" split-host-load-on-other-box

# Resolved once at the top, and the same number both samplers use. They used to
# size themselves from two different parsers, and this one fell back to a
# hardcoded 90 on anything it could not read — including on a mawk host, where
# its match() form does not exist at all.
top_secs=$load_secs

echo "== 2. memsampler alongside MCP (pid=$mcp_pid)"
# Plus the tail buffer sampler.sh gets. On this box the case is stronger still:
# the load is driven from the other box, which dials its sessions before its
# own clock starts, so a window of exactly load_secs reliably ends early — and
# what it clips is fd_peak, the number Run H exists to measure.
"$bin/memsampler" -pid "$mcp_pid" -interval 1s -duration "$(( load_secs + 10 ))s" \
  -out "$runs/mem.csv" >"$runs/memsampler.log" 2>&1 &
mem_pid=$!

echo "== 3. sampler alongside MCP (~${top_secs}s at 5s intervals, /proc-based)"
MCP_ONLY=1 "$here/sampler.sh" "$runs/sampler.csv" 5 "$top_secs" >"$runs/sampler.log" 2>&1 &
top_pid=$!

echo "== 4. holding MCP+samplers for ${top_secs}s — drive loadgen from Box A now"
echo "     ss check (before load):"
# `|| true`: this is a diagnostic, not a check. Under `pipefail` a missing ss
# would fail the pipeline and abort the run — which would make the documented
# `PORT_WAIT_SECS=0` escape hatch for a box without iproute2 not actually work
# on this runner.
{ ss -tn state established 2>/dev/null || true; } \
  | awk -v h="$MOCK_HOST" '$0 ~ h {n++} END {print "     established conns to " h ": " (n+0)}'

wait "$mem_pid" 2>/dev/null || true
wait "$top_pid" 2>/dev/null || true

run_finished=1

echo "== done"

# The peaks are written by cleanup(), which runs on the way out of every path
# including a terminated one — see the trap.

echo
"$here/summary.sh" "$runs" || true
echo "run record: $record"
