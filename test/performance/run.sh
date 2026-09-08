#!/usr/bin/env bash
# Single-host smoke run: mock-semp + MCP + fidelity + loadgen + samplers on
# one box. Convenient for local dev / cold fidelity. For the split-host demo
# (Box A: loadgen+mock, Box B: MCP), use run-mcp.sh on B and run-loadgen.sh
# on A instead.
#
# Usage:
#   ./run.sh                       # 32 clients, 60s, default tools
#   CLIENTS=16 DURATION=30s ./run.sh
#
# Env overrides:
#   CLIENTS      loadgen -clients      (default 32)
#   DURATION     loadgen -duration     (default 60s)
#   TOOLS        loadgen -tools        (default get-broker-status,list-queues,list-rdps,get-rdp-status)
#   LATENCY_MS   mock-semp -default-latency-ms (default 0). Set >0 to make every
#                broker response take that long — with max_concurrent_per_broker=10
#                in the MCP config, this is what causes requests to queue up on
#                the per-broker semaphore inside MCP.
#   ERROR_RATE   probability [0,1] a broker request is injected with an error
#                response instead of the canned one. 0 (default) = no injection.
#                Applied per-broker via POST /_mock/config.
#   ERROR_COUNT  cap on total injected errors across the run. 0 (default) =
#                unlimited. Budget is per-port; a run with 50 brokers and
#                ERROR_COUNT=10 emits at most 10 errors per broker.
#   ERROR_STATUSES  weighted status pool as "code:weight,code:weight,...".
#                   Default: "503:70,429:20,500:10" — mirrors realistic broker
#                   overload signals and exercises the MCP retry chain.
#                   Only 429/500/502/503/504 are accepted (retryable codes).
#   CONFIG_FILE  MCP broker config (default ./broker-config.mock.yaml). Point
#                this at a per-run copy to vary request_min_interval; the file
#                used is copied into the run directory so the numbers stay
#                traceable to it.
#   BROKER_ALIAS fidelity -broker  (default broker-01; must exist in broker-config.mock.yaml)
#   VPN          fidelity/loadgen -vpn (default: the VPN recorded in fixtures.manifest
#                at capture time — set this only to override that)
#   RDP          fidelity/loadgen -rdp (default: the RDP recorded in fixtures.manifest;
#                the mock serves get-rdp-status for that RDP only)
#   BROKER_USERNAME / BROKER_PASSWORD  (default perf/perf; mock accepts anything non-empty)
#   BROKERS      number of mock brokers, and loadgen -broker-count (default 50).
#                Above 50 the committed config runs out of aliases, so generate
#                one to match:
#                  ./gen-mock-config.sh -n 120 -o broker-config.gen120.yaml
#                  BROKERS=120 CONFIG_FILE=./broker-config.gen120.yaml ./run.sh
#   PORT_WAIT_SECS  how long to wait for a port held by a previous sweep point
#                to be released before giving up (default 60)
#   NOFILE       descriptor limit to request (default 1048576; falls back to the
#                hard limit, and both are recorded)
#   RIG_NOTE     free-text note about this host, recorded verbatim in the run
#                record

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"
bin="$here/bin"
runs="$bin/runs/$(date +%Y%m%d-%H%M%S)"
mkdir -p "$runs"

# shellcheck source=lib.sh
source "$here/lib.sh"
# Single host, so both halves of the run live in one directory — but still two
# records, one per role, so a consumer reads a single-host run the same way it
# reads the two halves of a split-host one.
mcp_record="$runs/run-record.mcp"
lg_record="$runs/run-record.loadgen"

CLIENTS="${CLIENTS:-32}"
DURATION="${DURATION:-60s}"
TOOLS="${TOOLS:-get-broker-status,list-queues,list-rdps,get-rdp-status}"
LATENCY_MS="${LATENCY_MS:-0}"
ERROR_RATE="${ERROR_RATE:-0}"
ERROR_COUNT="${ERROR_COUNT:-0}"
ERROR_STATUSES="${ERROR_STATUSES:-503:70,429:20,500:10}"
# Fidelity gate targets. BROKER_ALIAS only picks which mock broker MCP dials
# (the mock replays the same canned bytes on every port), so it defaults to a
# mock alias. VPN must match the capture and is resolved from
# fixtures.manifest after the preflight below — hardcoding a default here is
# how it drifted from regen-golden.sh's.
BROKER_ALIAS="${BROKER_ALIAS:-broker-01}"
CONFIG_FILE="${CONFIG_FILE:-$here/broker-config.mock.yaml}"
BROKERS="${BROKERS:-50}"
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
export BROKER_USERNAME="${BROKER_USERNAME:-perf}"
export BROKER_PASSWORD="${BROKER_PASSWORD:-perf}"
# Single-host: MCP reaches the mock over loopback.
export MOCK_HOST="${MOCK_HOST:-localhost}"

required_bins=(mock-semp fidelity memsampler loadgen mcp-server)
for b in "${required_bins[@]}"; do
  if [[ ! -x "$bin/$b" ]]; then
    echo "missing $bin/$b — run ./build.sh first" >&2
    exit 2
  fi
done

# A non-numeric or zero BROKERS would build an empty port list and hand
# mock-semp a -listen-count it rejects several steps later. Catch it here,
# where the message can name the variable.
if ! [[ "$BROKERS" =~ ^[0-9]+$ ]] || (( BROKERS < 1 )); then
  echo "BROKERS must be a positive integer, got: $BROKERS" >&2
  exit 2
fi

mock_start=18081
mock_count="$BROKERS"

# Wait for every port this run needs rather than aborting on a held one. A
# back-to-back sweep point was lost this way: the previous point's server still
# held :9090 when the next one started. An immediate abort turns a two-second
# wait into a missing data point; starting anyway would measure the previous
# process. Bounded, and the failure names the port it waited on.
#
# "Free" means no listener, not no socket: a TIME_WAIT socket does not block a
# bind with SO_REUSEADDR, so waiting for every socket to clear would wait for
# something that never happens. See perf_first_held_port.
#
# The mock's whole port range is covered, not just its first port — a previous
# run with a larger BROKERS leaves the tail bound while 18081 is already free.
mock_ports=()
for (( p = mock_start; p < mock_start + mock_count; p++ )); do mock_ports+=("$p"); done
perf_wait_ports_free "$PORT_WAIT_SECS" 9090 19000 "${mock_ports[@]}" || exit 2

# Raise the descriptor limit before anything is launched, so both the server
# and the mock inherit it. Nothing in this suite, cmd/ or deploy/ raised
# RLIMIT_NOFILE before, which left it varying with whatever the login shell
# handed the operator — a result-moving input that was invisible in the output.
# On a single host this shell's limit covers the server's two idle pools per
# broker AND the load generator's inbound sessions, which is the larger of the
# two by far at high client counts.
perf_raise_nofile "${NOFILE:-1048576}"
echo "== 0. descriptor limit: requested $PERF_NOFILE_REQUESTED, granted $PERF_NOFILE_GRANTED"

# Fixture preflight. The canned responses and goldens are lab captures kept
# out of git, so "absent" is the normal state of a fresh clone — fail here
# with a pointer to regen-golden.sh rather than 404ing mid-run or failing the
# fidelity gate in a way that reads like a regression.
echo "== 0. fixture preflight"
"$here/fixtures-manifest.sh" check

# Resolve the fidelity VPN from the capture's own provenance so the gate can't
# ask for a VPN the goldens were never captured against. An explicit VPN= in
# the environment still wins.
VPN="${VPN:-$("$here/fixtures-manifest.sh" vpn)}"
if [[ -z "$VPN" || "$VPN" == "unknown" ]]; then
  echo "fixtures.manifest records no VPN — recapture with ./regen-golden.sh, or pass VPN=<name> explicitly." >&2
  exit 2
fi

# Same for the pinned RDP. The mock answers get-rdp-status for exactly one RDP
# name — the one in the capture — and misses (404, non-zero exit) for any
# other, so both fidelity and loadgen have to ask for that one.
RDP="${RDP:-$("$here/fixtures-manifest.sh" rdp)}"
if [[ -z "$RDP" || "$RDP" == "unknown" ]]; then
  echo "fixtures.manifest records no RDP — recapture with ./regen-golden.sh, or pass RDP=<name> explicitly." >&2
  exit 2
fi

# Validate TOOLS before anything starts. loadgen enforces this at its own
# startup too, but by then the mock is listening, the fidelity gate has run,
# and (split-host) we may have waited minutes for Box B — a typo in TOOLS
# should cost nothing. The tool list lives in loadgen's own recipe map, so the
# check is delegated rather than duplicated here: a bash copy of the valid
# names would drift the first time a tool is added.
"$bin/loadgen" -validate-only -tools "$TOOLS" -vpn "$VPN" -rdp "$RDP"

# Records first: a run that dies in startup should still say what it was.
perf_record_begin "$mcp_record" mcp "$runs"
perf_record_rig "$mcp_record"
perf_record_code "$mcp_record" "$repo_root" "$bin" mcp-server memsampler
perf_record_fixtures "$mcp_record" "$here"
perf_record_kv "$mcp_record" config_file "$CONFIG_FILE"
perf_record_kv "$mcp_record" nofile_requested "$PERF_NOFILE_REQUESTED"
perf_record_kv "$mcp_record" nofile_granted "$PERF_NOFILE_GRANTED"

perf_record_begin "$lg_record" loadgen "$runs"
perf_record_rig "$lg_record"
perf_record_code "$lg_record" "$repo_root" "$bin" loadgen mock-semp fidelity
perf_record_fixtures "$lg_record" "$here"
perf_record_comment "$lg_record" "workload"
perf_record_kv "$lg_record" clients "$CLIENTS"
perf_record_kv "$lg_record" duration "$DURATION"
perf_record_kv "$lg_record" tools "$TOOLS"
perf_record_kv "$lg_record" broker_count "$BROKERS"
perf_record_kv "$lg_record" vpn "$VPN"
perf_record_kv "$lg_record" rdp "$RDP"
perf_record_kv "$lg_record" latency_ms "$LATENCY_MS"
perf_record_kv "$lg_record" error_rate "$ERROR_RATE"
perf_record_kv "$lg_record" error_count "$ERROR_COUNT"
perf_record_kv "$lg_record" error_statuses "$ERROR_STATUSES"
perf_record_kv "$lg_record" nofile_requested "$PERF_NOFILE_REQUESTED"
perf_record_kv "$lg_record" nofile_granted "$PERF_NOFILE_GRANTED"

mock_pid= mcp_pid= mem_pid= top_pid=
# kill_tree signals a pid (and its process group if reachable) and waits for
# it. It does NOT `wait` inside — callers that need the child's exit code
# (e.g. mock-semp's hard-gate exit-nonzero-on-miss) must wait separately.
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
}
cleanup() {
  local rc=$?
  set +e
  kill_tree "$top_pid";  wait "$top_pid" 2>/dev/null
  kill_tree "$mem_pid";  wait "$mem_pid" 2>/dev/null
  kill_tree "$mcp_pid";  wait "$mcp_pid" 2>/dev/null
  if [[ -n "$mock_pid" ]]; then
    kill_tree "$mock_pid"
    wait "$mock_pid"
    local mock_rc=$?
    # SIGTERM (128+15=143) and SIGKILL (128+9=137) are expected shutdown paths.
    # Anything else means mock-semp's hard gate (unmatched request) fired.
    if (( mock_rc != 0 && mock_rc != 143 && mock_rc != 137 )); then
      echo "!! mock-semp exited $mock_rc — a request 404'd (missing canned response). See $runs/mock.log"
      (( rc == 0 )) && rc=$mock_rc
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

wait_for_tcp() {
  local host=$1 port=$2 name=$3
  for _ in $(seq 1 60); do
    if (exec 3<>/dev/tcp/"$host"/"$port") 2>/dev/null; then
      exec 3<&- 3>&-
      return 0
    fi
    sleep 0.2
  done
  echo "timed out waiting for $name at $host:$port" >&2
  return 1
}

echo "== 1. mock-semp on :$mock_start..$((mock_start + mock_count - 1)) (default-latency-ms=$LATENCY_MS)"
# mock-semp reads canned/ from disk at startup (auto-located next to the
# binary), so it replays whatever the last capture produced — no rebuild
# needed, and a missing or empty canned/ is a startup fatal.
setsid "$bin/mock-semp" -listen-start "$mock_start" -listen-count "$mock_count" -config-port 19000 \
  -default-latency-ms "$LATENCY_MS" \
  >"$runs/mock.log" 2>&1 &
mock_pid=$!
wait_for_tcp localhost 18081 mock-semp

# arm_injection POSTs the error-injection config to every broker port. Deferred
# until after the fidelity gate so a 1% roll doesn't flake the pre-run check.
# No-op when ERROR_RATE is 0 (default), so runs without injection are unchanged.
arm_injection() {
  awk -v r="$ERROR_RATE" 'BEGIN { exit !(r+0 > 0) }' || return 0
  local statuses_json ports_json
  statuses_json=$(awk -v s="$ERROR_STATUSES" 'BEGIN {
    n = split(s, a, ",")
    printf "["
    for (i = 1; i <= n; i++) {
      split(a[i], kv, ":")
      if (i > 1) printf ","
      printf "{\"code\":%d,\"weight\":%d}", kv[1]+0, kv[2]+0
    }
    printf "]"
  }')
  # Preserve LATENCY_MS in the payload — configStore.set replaces the whole
  # portOverride, so omitting it would zero out the seeded per-port latency
  # and defeat the LATENCY_MS + ERROR_RATE combination.
  ports_json=$(awk -v start="$mock_start" -v count="$mock_count" \
                   -v lat="$LATENCY_MS" \
                   -v rate="$ERROR_RATE" -v cnt="$ERROR_COUNT" -v st="$statuses_json" '
    BEGIN {
      printf "{"
      for (i = 0; i < count; i++) {
        p = start + i
        if (i > 0) printf ","
        printf "\"%d\":{\"latency_ms\":%d,\"error_rate\":%s,\"error_count\":%d,\"error_statuses\":%s}", p, lat+0, rate, cnt, st
      }
      printf "}"
    }')
  curl -fsS -X POST -H "Content-Type: application/json" \
    -d "{\"ports\":$ports_json}" \
    http://localhost:19000/_mock/config >/dev/null
}

# snapshot_fanout banks the per-rule SEMP counts accumulated so far and zeroes
# them: POST /_mock/hits reports the window it closes. Called between the
# fidelity gate and the load run, so the gate's requests are recorded as the
# fan-out table they are, and the shutdown summary in mock.log covers only the
# load phase. Never fatal — a missing fan-out table is worth a warning, not a
# dead run, and mock.log still carries the (combined) totals.
snapshot_fanout() {
  local out="$runs/semp-fanout.json"
  if ! curl -fsS -X POST http://localhost:19000/_mock/hits -o "$out"; then
    echo "   WARNING: could not read /_mock/hits; mock.log's shutdown summary will include the gate's requests" >&2
    return 0
  fi
  if command -v jq >/dev/null 2>&1; then
    jq -r '.rules[] | select(.hits > 0) | "   \(.hits) \(.rule)"' "$out"
  else
    echo "   counts in $out (install jq to see them tabulated here)"
  fi
}

echo "== 2. MCP server on :9090 (config: $CONFIG_FILE)"
# Keep the exact config alongside the numbers it produced. Recording only the
# filename is not enough: the per-run copies are local and get overwritten.
cp "$CONFIG_FILE" "$runs/broker-config.used.yaml"
# Exec the prebuilt binary, not `go run`: `go run` runs the compiled program
# as a child process, so $mcp_pid would be the toolchain wrapper and the
# memsampler in step 4 would sample that instead of MCP.
setsid bash -c "cd '$repo_root' && CONFIG_FILE='$CONFIG_FILE' exec '$bin/mcp-server'" \
  >"$runs/mcp.log" 2>&1 &
mcp_pid=$!
wait_for_http "http://localhost:9090/health" mcp-server

# The server has now reported its own effective configuration, so record the
# four settings that move admission behaviour and the limit the process
# actually runs under.
perf_record_kv "$mcp_record" mcp_pid "$mcp_pid"
perf_record_admission "$mcp_record" "$runs/mcp.log" "$runs/broker-config.used.yaml"
perf_record_proc_nofile "$mcp_record" "$mcp_pid"

echo "== 3. fidelity gate (exact mode; broker=$BROKER_ALIAS vpn=$VPN rdp=$RDP; exclusions in fidelity/exclusions.txt)"
# BROKER_ALIAS + VPN must match how the goldens were captured; the mock
# replays canned bytes regardless of the alias in the request path, so
# BROKER_ALIAS just picks which mock broker MCP dials.
if ! "$bin/fidelity" -mcp-url http://localhost:9090 -broker "$BROKER_ALIAS" -vpn "$VPN" -rdp "$RDP" \
      -golden-dir "$here/fidelity/golden" | tee "$runs/fidelity.log"; then
  echo "!! fidelity FAILED — aborting before load run" >&2
  exit 1
fi

# The gate makes exactly one call per check, so the counters now hold the SEMP
# cost of those five calls — the factor that turns loadgen's tool calls/s into
# SEMP requests/s. Bank that table, then zero the counters so the shutdown
# summary in mock.log measures the load phase alone instead of the load phase
# plus these dozen requests.
#
# Read it per rule, not per tool: two of the five checks are list-rdps (default
# args, and maxResults=200 for the paginated one), so "rdps page 1" shows 2 —
# one hit from each — and only "rdps page 2 (cursor)" is unique to the
# paginated call. README's per-call table has the split.
echo "== 3b. SEMP fan-out per tool call (from the gate's one-call-per-check pass)"
snapshot_fanout

if awk -v r="$ERROR_RATE" 'BEGIN { exit !(r+0 > 0) }'; then
  echo "== 3c. arming error injection (rate=$ERROR_RATE count=$ERROR_COUNT/port statuses=$ERROR_STATUSES)"
  arm_injection
fi

echo "== 4. memsampler alongside MCP (pid=$mcp_pid)"
"$bin/memsampler" -pid "$mcp_pid" -interval 1s -duration "$DURATION" \
  -out "$runs/mem.csv" >"$runs/memsampler.log" 2>&1 &
mem_pid=$!

# sampler samples MCP + mock CPU + RSS/PSS/USS via /proc, and box totals, in
# parallel with memsampler. Duration argument is seconds; DURATION here is a
# Go duration string (e.g. "60s", "2m") so convert with a tiny awk.
top_secs=$(awk -v d="$DURATION" 'BEGIN {
  if (match(d, /^([0-9.]+)s$/, m)) { print int(m[1]); exit }
  if (match(d, /^([0-9.]+)m$/, m)) { print int(m[1]*60); exit }
  if (match(d, /^([0-9.]+)h$/, m)) { print int(m[1]*3600); exit }
  print 90  # fallback if duration is unparseable
}')
# Small tail buffer, the same one run-loadgen.sh gives its samplers. The
# samplers start just before loadgen does, so an exactly-DURATION window ends a
# few seconds before the load does and clips the tail off the load-phase
# figures. The extra idle samples land in the whole-run average, which is the
# diluted number this change exists to stop anyone relying on.
top_secs=$(( top_secs + 10 ))
echo "== 4b. sampler alongside MCP+mock (~${top_secs}s at 5s intervals)"
"$here/sampler.sh" "$runs/sampler.csv" 5 "$top_secs" >"$runs/sampler.log" 2>&1 &
top_pid=$!

inject_note="no error injection"
if awk -v r="$ERROR_RATE" 'BEGIN { exit !(r+0 > 0) }'; then
  inject_note="inject rate=$ERROR_RATE count=$ERROR_COUNT/port statuses=$ERROR_STATUSES"
fi
echo "== 5. loadgen ($CLIENTS clients, $DURATION, tools=$TOOLS, broker-count=$BROKERS; $inject_note)"
# Stamp the load window into both records. summary.sh windows the sampler CSV
# on it so the reported CPU is the load phase, not the load phase averaged
# together with the idle stretch the fidelity gate ran in. Stamped by the
# runner that starts the load, never inferred from the samples.
# One clock read for both records: four independent `date +%s` calls could
# leave the two files disagreeing by a second, and summary.sh reads whichever
# record its glob happens to yield first — so the window a run reports would
# depend on directory order.
load_start_epoch=$(date +%s)
perf_stamp_load_start "$mcp_record" "$load_start_epoch"
perf_stamp_load_start "$lg_record" "$load_start_epoch"
lg_rc=0
"$bin/loadgen" -mcp-url http://localhost:9090 -broker-count "$BROKERS" \
  -clients "$CLIENTS" -duration "$DURATION" -tools "$TOOLS" \
  -vpn "$VPN" -rdp "$RDP" \
  | tee "$runs/loadgen.log" || lg_rc=$?
load_end_epoch=$(date +%s)
perf_stamp_load_end "$mcp_record" "$load_end_epoch"
perf_stamp_load_end "$lg_record" "$load_end_epoch"

# Peaks are only knowable once the samplers have stopped.
# `|| true`: a provenance write failing before the load starts is a fair
# abort, but failing after a completed run would discard the report for a
# measurement that is already safely in the CSVs.
wait "$mem_pid" 2>/dev/null || true
wait "$top_pid" 2>/dev/null || true
perf_record_fd_peak "$mcp_record" "$runs/mem.csv" || true

echo "== done"
echo
"$here/summary.sh" "$runs" || true
echo "run records: $mcp_record, $lg_record"
exit "$lg_rc"
