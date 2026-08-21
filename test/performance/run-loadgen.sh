#!/usr/bin/env bash
# Split-host: Box A — runs mock-semp + loadgen + samplers.
# MCP lives on Box B (see run-mcp.sh) and dials this host on 18081..18130.
#
# Usage:
#   ./run-loadgen.sh <mcp-url>
#   CLIENTS=2000 DURATION=60s ./run-loadgen.sh http://198.51.100.31:9090
#
# Env overrides (same names as run.sh so the two scripts share vocabulary):
#   CLIENTS      loadgen -clients                (default 200)
#   DURATION     loadgen -duration               (default 60s)
#   TOOLS        loadgen -tools                  (default get-broker-status,list-queues,list-rdps,get-rdp-status)
#   BROKERS      loadgen -broker-count           (default 50)
#   LATENCY_MS   mock-semp -default-latency-ms   (default 0). Set >0 to make every
#                broker response take that long — combined with the MCP config's
#                max_concurrent_per_broker, requests pile up on the per-broker
#                semaphore inside MCP on Box B.
#   TOTAL_RPS    loadgen -total-rps              (default 0 = unlimited). Paces
#                aggregate req/s across all clients; use to break the release-
#                barrier convoy that otherwise correlates all clients.
#   RUN_TAG      tag appended to the runs dir    (default $CLIENTS-clients)
#   NO_MOCK=1    skip starting mock-semp here (it's already running elsewhere).
#                Note: error injection is only auto-armed when this script
#                owns the mock. With NO_MOCK=1, POST /_mock/config yourself.
#   ERROR_RATE     probability [0,1] a broker response is injected as an error
#                  (default 0 = no injection). See run.sh header for the full
#                  contract; same knobs, same defaults.
#   ERROR_COUNT    cap on injected errors per broker port (default 0 = unlimited)
#   ERROR_STATUSES weighted status pool "code:w,code:w,..."
#                  (default "503:70,429:20,500:10")
#   BROKER_ALIAS   fidelity -broker  (default broker-01; must exist in broker-config.mock.yaml)
#   VPN            fidelity/loadgen -vpn (default: the VPN recorded in
#                  fixtures.manifest at capture time — set only to override)
#   RDP            fidelity/loadgen -rdp (default: the RDP recorded in
#                  fixtures.manifest; the mock serves get-rdp-status for it only)

set -euo pipefail

mcp_url="${1:?usage: $0 <mcp-url>   e.g.  $0 http://198.51.100.31:9090}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bin="$here/bin"

CLIENTS="${CLIENTS:-200}"
DURATION="${DURATION:-60s}"
TOOLS="${TOOLS:-get-broker-status,list-queues,list-rdps,get-rdp-status}"
BROKERS="${BROKERS:-50}"
LATENCY_MS="${LATENCY_MS:-0}"
TOTAL_RPS="${TOTAL_RPS:-0}"
RUN_TAG="${RUN_TAG:-${CLIENTS}c}"
NO_MOCK="${NO_MOCK:-0}"
ERROR_RATE="${ERROR_RATE:-0}"
ERROR_COUNT="${ERROR_COUNT:-0}"
ERROR_STATUSES="${ERROR_STATUSES:-503:70,429:20,500:10}"
# Fidelity gate targets. BROKER_ALIAS only picks which mock broker MCP dials
# (the mock replays the same canned bytes on every port), so it defaults to a
# mock alias. VPN must match the capture and is resolved from
# fixtures.manifest after the preflight below — hardcoding a default here is
# how it drifted from regen-golden.sh's.
BROKER_ALIAS="${BROKER_ALIAS:-broker-01}"
runs="$bin/runs/$(date +%Y%m%d-%H%M%S)-loadgen-$RUN_TAG"
mkdir -p "$runs"

required_bins=(loadgen fidelity)
[[ "$NO_MOCK" != "1" ]] && required_bins+=(mock-semp)
for b in "${required_bins[@]}"; do
  if [[ ! -x "$bin/$b" ]]; then
    echo "missing $bin/$b — run ./build.sh first" >&2
    exit 2
  fi
done

if [[ "$NO_MOCK" != "1" ]] && ss -tln 2>/dev/null | grep -q ":18081 "; then
  echo "port 18081 already in use — mock-semp may already be running:" >&2
  ss -tlnp 2>/dev/null | grep ":18081 " >&2
  echo "kill it, or re-run with NO_MOCK=1 to reuse the existing mock." >&2
  exit 2
fi

# Fixture preflight. The canned responses and goldens are lab captures kept
# out of git, so "absent" is the normal state of a fresh clone — fail here
# with a pointer to regen-golden.sh rather than 404ing mid-run or failing the
# fidelity gate in a way that reads like a regression. Under NO_MOCK=1 the
# mock (and its canned/) belongs to another host, so only the goldens this
# box feeds to the fidelity gate are checked.
echo "== 0. fixture preflight"
if [[ "$NO_MOCK" == "1" ]]; then
  "$here/fixtures-manifest.sh" check --no-canned
else
  "$here/fixtures-manifest.sh" check
fi

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

# Convert Go duration to seconds for the sampler's -duration arg.
sample_secs=$(awk -v d="$DURATION" 'BEGIN {
  if (match(d, /^([0-9.]+)s$/, m)) { print int(m[1]); exit }
  if (match(d, /^([0-9.]+)m$/, m)) { print int(m[1]*60); exit }
  if (match(d, /^([0-9.]+)h$/, m)) { print int(m[1]*3600); exit }
  print 90
}')
# Give the sampler a small buffer so it captures loadgen's teardown too.
sample_secs=$(( sample_secs + 10 ))

mock_pid= lg_pid= sampler_pid= mock_top_pid=
# kill_tree signals a pid (and its process group if reachable) and returns.
# It does NOT `wait` inside — callers that need the child's exit code
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
  kill_tree "$sampler_pid";  wait "$sampler_pid" 2>/dev/null
  kill_tree "$mock_top_pid"; wait "$mock_top_pid" 2>/dev/null
  kill_tree "$lg_pid";       wait "$lg_pid" 2>/dev/null
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

# arm_injection POSTs the error-injection config to every broker port on the
# local mock (localhost:19000). No-op when ERROR_RATE is 0. Skipped under
# NO_MOCK=1 — the mock isn't ours to configure in that mode.
arm_injection() {
  awk -v r="$ERROR_RATE" 'BEGIN { exit !(r+0 > 0) }' || return 0
  [[ "$NO_MOCK" == "1" ]] && { echo "   (NO_MOCK=1: arm the external mock yourself)"; return 0; }
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
  ports_json=$(awk -v start=18081 -v count="$BROKERS" \
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
# load phase. Skipped under NO_MOCK=1 for the same reason arm_injection is: the
# mock isn't ours. Never fatal.
snapshot_fanout() {
  [[ "$NO_MOCK" == "1" ]] && { echo "   (NO_MOCK=1: read /_mock/hits on the host that owns the mock)"; return 0; }
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

wait_for_tcp() {
  local host=$1 port=$2 name=$3 timeout_s=${4:-12}
  local end=$((SECONDS + timeout_s))
  while (( SECONDS < end )); do
    if (exec 3<>/dev/tcp/"$host"/"$port") 2>/dev/null; then
      exec 3<&- 3>&-
      return 0
    fi
    sleep 0.5
  done
  echo "timed out after ${timeout_s}s waiting for $name at $host:$port" >&2
  return 1
}

wait_for_http() {
  local url=$1 name=$2 timeout_s=${3:-12}
  local end=$((SECONDS + timeout_s))
  # -fs (not -fsS): silence per-retry connection errors during the poll.
  # Box B often takes tens of seconds to bring MCP up; a chatty poll would
  # spew hundreds of "Failed to connect" lines before success. A real
  # network problem still surfaces as the timeout below.
  while (( SECONDS < end )); do
    if curl -fs -o /dev/null "$url"; then return 0; fi
    sleep 0.5
  done
  echo "timed out after ${timeout_s}s waiting for $name at $url" >&2
  return 1
}

# Parse host:port out of the MCP URL so we can wait for Box B to come up
# before firing loadgen. Otherwise mock-semp starts, loadgen fires
# immediately, and Box B's run-mcp.sh (which itself waits for our mock)
# hasn't spun MCP yet — connection refused.
mcp_hostport="${mcp_url#*://}"
mcp_hostport="${mcp_hostport%%/*}"
mcp_host="${mcp_hostport%:*}"
mcp_port="${mcp_hostport##*:}"
[[ "$mcp_port" == "$mcp_hostport" ]] && mcp_port=80

if [[ "$NO_MOCK" != "1" ]]; then
  echo "== 1. mock-semp on 0.0.0.0:18081..$((18081 + BROKERS - 1)) (config: :19000, default-latency-ms=$LATENCY_MS)"
  # Bind all interfaces so Box B can reach us over the LAN. mock-semp reads
  # canned/ from disk at startup (auto-located next to the binary), so it
  # replays whatever the last capture produced — no rebuild needed, and a
  # missing or empty canned/ is a startup fatal.
  setsid "$bin/mock-semp" -listen-addr 0.0.0.0 -listen-start 18081 -listen-count "$BROKERS" -config-port 19000 \
    -default-latency-ms "$LATENCY_MS" \
    >"$runs/mock.log" 2>&1 &
  mock_pid=$!
  wait_for_tcp localhost 18081 mock-semp
fi

echo "== 2. sampler for mock (~${sample_secs}s at 5s intervals, /proc-based)"
MOCK_ONLY=1 "$here/sampler.sh" "$runs/mock-sampler.csv" 5 "$sample_secs" \
  >"$runs/mock-sampler.log" 2>&1 &
mock_top_pid=$!

echo "== 3. waiting for MCP at $mcp_url/health (start run-mcp.sh on Box B now if you haven't — up to 5 min)"
# HTTP-readiness (not TCP): a TCP-listening port is not the same as a
# serving MCP — the socket accepts before `go run` finishes compiling and
# streamable-HTTP is registered, so a TCP-only wait can let loadgen fire
# into 400s. Poll /health until it returns 200.
if ! wait_for_http "$mcp_url/health" mcp-server 300; then
  my_ip="$(hostname -I 2>/dev/null | awk '{print $1}')"
  echo "!! MCP never came up — is Box B running:  MOCK_HOST=${my_ip:-<box-a-ip>} ./run-mcp.sh ?" >&2
  exit 1
fi

# Fidelity gate: exact-mode diff of MCP tool output vs golden captures.
# Runs BEFORE arm_injection so the check isn't flaked by a 1% error roll.
# BROKER_ALIAS + VPN must match how the goldens were captured; see run.sh.
echo "== 3b. fidelity gate (exact mode; broker=$BROKER_ALIAS vpn=$VPN rdp=$RDP; exclusions in fidelity/exclusions.txt)"
if ! "$bin/fidelity" -mcp-url "$mcp_url" -broker "$BROKER_ALIAS" -vpn "$VPN" -rdp "$RDP" \
      -golden-dir "$here/fidelity/golden" | tee "$runs/fidelity.log"; then
  echo "!! fidelity FAILED — aborting before load run" >&2
  exit 1
fi

# The gate makes exactly one call per check, so the counters now hold each
# tool's SEMP fan-out. Bank that table and zero the counters so mock.log's
# shutdown summary measures the load phase alone.
echo "== 3c. SEMP fan-out per tool call (from the gate's one-call-per-check pass)"
snapshot_fanout

extra_args=()
[[ "$TOTAL_RPS" != "0" ]] && extra_args+=(-total-rps "$TOTAL_RPS")

if awk -v r="$ERROR_RATE" 'BEGIN { exit !(r+0 > 0) }'; then
  echo "== 3d. arming error injection (rate=$ERROR_RATE count=$ERROR_COUNT/port statuses=$ERROR_STATUSES)"
  arm_injection
  inject_note="inject rate=$ERROR_RATE count=$ERROR_COUNT/port statuses=$ERROR_STATUSES"
else
  inject_note="no error injection"
fi

echo "== 4. loadgen against $mcp_url ($CLIENTS clients, $DURATION, tools=$TOOLS${TOTAL_RPS:+, total-rps=$TOTAL_RPS}; $inject_note)"
"$bin/loadgen" -mcp-url "$mcp_url" -broker-count "$BROKERS" \
  -clients "$CLIENTS" -duration "$DURATION" -tools "$TOOLS" \
  -vpn "$VPN" -rdp "$RDP" \
  "${extra_args[@]}" \
  > >(tee "$runs/loadgen.log") 2>&1 &
lg_pid=$!

# Wait for loadgen to actually be running (dial phase can take a second at
# high client counts). Sampler needs a PID before it starts.
for _ in $(seq 1 40); do
  if kill -0 "$lg_pid" 2>/dev/null; then break; fi
  sleep 0.1
done
sleep 1  # let dialAll get past its initial burst

echo "== 5. loadgen-sampler (~${sample_secs}s at 5s intervals)"
"$here/loadgen-sampler.sh" "$runs/loadgen-metrics.csv" 5 "$sample_secs" \
  > "$runs/loadgen-sampler.log" 2>&1 &
sampler_pid=$!

wait "$lg_pid"
lg_rc=$?

# Let the samplers flush; they exit on their own via kill -0 / duration.
wait "$sampler_pid" 2>/dev/null || true
wait "$mock_top_pid" 2>/dev/null || true

echo "== done (loadgen rc=$lg_rc)"
echo
"$here/summary.sh" "$runs" || true
exit "$lg_rc"
