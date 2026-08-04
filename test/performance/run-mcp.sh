#!/usr/bin/env bash
# Split-host: Box B — runs MCP + memsampler + sampler.sh (MCP-only).
# mock-semp and loadgen live on Box A (see run-loadgen.sh).
#
# Usage:
#   MOCK_HOST=<box-a-ip> ./run-mcp.sh
#   MOCK_HOST=192.168.2.180 DURATION=90s ./run-mcp.sh
#
# Env:
#   MOCK_HOST      required — LAN address of Box A running mock-semp
#   DURATION       how long to hold MCP up for the loadgen run (default 90s;
#                  should exceed the loadgen -duration you use on Box A)
#   BROKER_USERNAME / BROKER_PASSWORD   (default perf/perf; mock accepts anything)

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"
bin="$here/bin"
runs="$bin/runs/$(date +%Y%m%d-%H%M%S)-mcp"
mkdir -p "$runs"

: "${MOCK_HOST:?MOCK_HOST unset — set to the Box A LAN IP, e.g. MOCK_HOST=192.168.2.180}"
export MOCK_HOST
export BROKER_USERNAME="${BROKER_USERNAME:-perf}"
export BROKER_PASSWORD="${BROKER_PASSWORD:-perf}"
DURATION="${DURATION:-90s}"

for b in memsampler; do
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

echo "== 1. MCP server on :9090 (config: broker-config.mock.yaml, MOCK_HOST=$MOCK_HOST)"
setsid bash -c "cd '$repo_root' && CONFIG_FILE='$here/broker-config.mock.yaml' exec go run ./cmd/server" \
  >"$runs/mcp.log" 2>&1 &
mcp_pid=$!
wait_for_http "http://localhost:9090/health" mcp-server

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
echo
"$here/summary.sh" "$runs" || true
