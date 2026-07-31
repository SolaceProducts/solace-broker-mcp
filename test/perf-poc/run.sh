#!/usr/bin/env bash
# Single-host smoke run: mock-semp + MCP + fidelity + loadgen + samplers on
# one box. Convenient for local dev / cold fidelity. For the split-host demo
# (Box A: loadgen+mock, Box B: MCP), use run-mcp.sh on B and run-loadgen.sh
# on A instead. See docs/plans/2026-07-22-semp-mock-perf-poc.md.
#
# Usage:
#   ./run.sh                       # 32 clients, 60s, default tools
#   CLIENTS=16 DURATION=30s ./run.sh
#
# Env overrides:
#   CLIENTS      loadgen -clients      (default 32)
#   DURATION     loadgen -duration     (default 60s)
#   TOOLS        loadgen -tools        (default get-broker-status,list-queues)
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
#   BROKER_USERNAME / BROKER_PASSWORD  (default poc/poc; mock accepts anything non-empty)

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"
bin="$here/bin"
runs="$bin/runs/$(date +%Y%m%d-%H%M%S)"
mkdir -p "$runs"

CLIENTS="${CLIENTS:-32}"
DURATION="${DURATION:-60s}"
TOOLS="${TOOLS:-get-broker-status,list-queues}"
LATENCY_MS="${LATENCY_MS:-0}"
ERROR_RATE="${ERROR_RATE:-0}"
ERROR_COUNT="${ERROR_COUNT:-0}"
ERROR_STATUSES="${ERROR_STATUSES:-503:70,429:20,500:10}"
export BROKER_USERNAME="${BROKER_USERNAME:-poc}"
export BROKER_PASSWORD="${BROKER_PASSWORD:-poc}"
# Single-host: MCP reaches the mock over loopback.
export MOCK_HOST="${MOCK_HOST:-localhost}"

required_bins=(mock-semp fidelity memsampler loadgen)
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

mock_pid= mcp_pid= mem_pid= top_pid=
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
  if [[ -n "$mock_pid" ]]; then
    kill_tree "$mock_pid"
    local mock_rc=$?
    if (( mock_rc != 0 )); then
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

echo "== 1. mock-semp on :18081..18130 (default-latency-ms=$LATENCY_MS)"
setsid "$bin/mock-semp" -listen-start 18081 -listen-count 50 -config-port 19000 \
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
  ports_json=$(awk -v start=18081 -v count=50 \
                   -v rate="$ERROR_RATE" -v cnt="$ERROR_COUNT" -v st="$statuses_json" '
    BEGIN {
      printf "{"
      for (i = 0; i < count; i++) {
        p = start + i
        if (i > 0) printf ","
        printf "\"%d\":{\"latency_ms\":0,\"error_rate\":%s,\"error_count\":%d,\"error_statuses\":%s}", p, rate, cnt, st
      }
      printf "}"
    }')
  curl -fsS -X POST -H "Content-Type: application/json" \
    -d "{\"ports\":$ports_json}" \
    http://localhost:19000/_mock/config >/dev/null
}

echo "== 2. MCP server on :9090 (config: broker-config.mock.yaml)"
setsid bash -c "cd '$repo_root' && CONFIG_FILE='$here/broker-config.mock.yaml' exec go run ./cmd/server" \
  >"$runs/mcp.log" 2>&1 &
mcp_pid=$!
wait_for_http "http://localhost:9090/health" mcp-server

echo "== 3. fidelity gate"
if ! "$bin/fidelity" -mcp-url http://localhost:9090 -broker broker-01 -vpn default \
      -golden-dir "$here/fidelity/golden" -shape | tee "$runs/fidelity.log"; then
  echo "!! fidelity FAILED — aborting before load run" >&2
  exit 1
fi

if awk -v r="$ERROR_RATE" 'BEGIN { exit !(r+0 > 0) }'; then
  echo "== 3b. arming error injection (rate=$ERROR_RATE count=$ERROR_COUNT/port statuses=$ERROR_STATUSES)"
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
  | tee "$runs/loadgen.log"

echo "== done"
