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
#                  should exceed the loadgen -duration you use on Box A)
#   CONFIG_FILE    MCP broker config (default ./broker-config.mock.yaml). Point
#                  this at a per-run copy to vary request_min_interval; the file
#                  used is copied into the run directory so the numbers stay
#                  traceable to it.
#   BROKER_USERNAME / BROKER_PASSWORD   (default perf/perf; mock accepts anything)
#   PORT_WAIT_SECS how long to wait for :9090 to be released by a previous
#                  sweep point before giving up (default 60)
#   NOFILE         descriptor limit to request for the server (default 1048576;
#                  falls back to the hard limit, and both are recorded)
#   RIG_NOTE       free-text note about this host, recorded verbatim in the run
#                  record. For a box that cannot describe itself.

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
cleanup() {
  local rc=$?
  set +e
  kill_tree "$top_pid"
  kill_tree "$mem_pid"
  kill_tree "$mcp_pid"
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

# This box drives no load, so it cannot stamp the load window: the load runs on
# the other box and the two share no channel by design. Say so in the record
# rather than inferring a boundary — a guessed window would poison every later
# comparison, and both run directories are archived together anyway.
perf_no_load_window "$record" split-host-load-on-other-box

# Convert Go duration to seconds for the samplers.
top_secs=$(awk -v d="$DURATION" 'BEGIN {
  if (match(d, /^([0-9.]+)s$/, m)) { print int(m[1]); exit }
  if (match(d, /^([0-9.]+)m$/, m)) { print int(m[1]*60); exit }
  if (match(d, /^([0-9.]+)h$/, m)) { print int(m[1]*3600); exit }
  print 90
}')

echo "== 2. memsampler alongside MCP (pid=$mcp_pid)"
"$bin/memsampler" -pid "$mcp_pid" -interval 1s -duration "$DURATION" \
  -out "$runs/mem.csv" >"$runs/memsampler.log" 2>&1 &
mem_pid=$!

echo "== 3. sampler alongside MCP (~${top_secs}s at 5s intervals, /proc-based)"
MCP_ONLY=1 "$here/sampler.sh" "$runs/sampler.csv" 5 "$top_secs" >"$runs/sampler.log" 2>&1 &
top_pid=$!

echo "== 4. holding MCP+samplers for ${top_secs}s — drive loadgen from Box A now"
echo "     ss check (before load):"
ss -tn state established 2>/dev/null | awk -v h="$MOCK_HOST" '$0 ~ h {n++} END {print "     established conns to " h ": " (n+0)}'

wait "$mem_pid" 2>/dev/null || true
wait "$top_pid" 2>/dev/null || true

echo "== done"

# Peaks are only knowable once the samplers have stopped.
# `|| true`: a provenance write failing before the load starts is a fair
# abort, but failing after a completed run would discard the report for a
# measurement that is already safely in the CSVs.
perf_record_fd_peak "$record" "$runs/mem.csv" || true

echo
"$here/summary.sh" "$runs" || true
echo "run record: $record"
