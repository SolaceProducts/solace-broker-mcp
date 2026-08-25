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
#   BROKER_ALIAS fidelity -broker  (default broker-01; must exist in broker-config.mock.yaml)
#   VPN          fidelity/loadgen -vpn (default: the VPN recorded in fixtures.manifest
#                at capture time — set this only to override that)
#   RDP          fidelity/loadgen -rdp (default: the RDP recorded in fixtures.manifest;
#                the mock serves get-rdp-status for that RDP only)
#   BROKER_USERNAME / BROKER_PASSWORD  (default perf/perf; mock accepts anything non-empty)

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"
bin="$here/bin"
runs="$bin/runs/$(date +%Y%m%d-%H%M%S)"
mkdir -p "$runs"

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

if ss -tln 2>/dev/null | grep -q ":9090 "; then
  echo "port 9090 already in use — another MCP is running:" >&2
  ss -tlnp 2>/dev/null | grep ":9090 " >&2
  echo "kill it before running this script." >&2
  exit 2
fi

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

mock_start=18081
mock_count=50
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

echo "== 2. MCP server on :9090 (config: broker-config.mock.yaml)"
# Exec the prebuilt binary, not `go run`: `go run` runs the compiled program
# as a child process, so $mcp_pid would be the toolchain wrapper and the
# memsampler in step 4 would sample that instead of MCP.
setsid bash -c "cd '$repo_root' && CONFIG_FILE='$here/broker-config.mock.yaml' exec '$bin/mcp-server'" \
  >"$runs/mcp.log" 2>&1 &
mcp_pid=$!
wait_for_http "http://localhost:9090/health" mcp-server

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
echo "== 4b. sampler alongside MCP+mock (~${top_secs}s at 5s intervals)"
"$here/sampler.sh" "$runs/sampler.csv" 5 "$top_secs" >"$runs/sampler.log" 2>&1 &
top_pid=$!

inject_note="no error injection"
if awk -v r="$ERROR_RATE" 'BEGIN { exit !(r+0 > 0) }'; then
  inject_note="inject rate=$ERROR_RATE count=$ERROR_COUNT/port statuses=$ERROR_STATUSES"
fi
echo "== 5. loadgen ($CLIENTS clients, $DURATION, tools=$TOOLS; $inject_note)"
"$bin/loadgen" -mcp-url http://localhost:9090 -broker-count 50 \
  -clients "$CLIENTS" -duration "$DURATION" -tools "$TOOLS" \
  -vpn "$VPN" -rdp "$RDP" \
  | tee "$runs/loadgen.log"

echo "== done"
