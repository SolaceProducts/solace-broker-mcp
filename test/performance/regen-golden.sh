#!/usr/bin/env bash
# Regenerate the mock's canned SEMP responses and fidelity/golden/*.json in
# a single coordinated capture against the real lab broker.
#
# The two capture paths must hit the same broker within seconds of each
# other, otherwise self-changing fields (uptime, memory %, disk usage) drift
# far enough apart to defeat exact-mode fidelity comparison even when the
# mock is replaying the "same" data. This script drives both back-to-back:
#
#   1. Start MCP against the real broker (broker-config.real.yaml).
#   2. Wait for MCP /health.
#   3. mock-semp/canned/capture.sh — direct SEMP curls, writes canned/*.
#   4. fidelity -capture — through MCP, writes fidelity/golden/*.
#   5. sanitize.sh — scrub lab-identifying values from both sides.
#   6. fixtures-manifest.sh write — record hashes + capture time for both sets.
#
# Neither fixture set is in git (they are lab captures); this script is the
# only supported way to produce them. mock-semp reads canned/ from disk at
# startup, so there is no rebuild step — a fresh capture takes effect on the
# next mock start.
#
# After this, run exact-mode fidelity (no -shape) to see the residual diff:
# that diff is the ground truth for what genuinely needs an exception list.

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"
bin="$here/bin"
config="${CONFIG_FILE:-$repo_root/broker-config.yaml}"
broker_alias="${BROKER_ALIAS:-my-broker}"
vpn="${VPN:-default}"
# The RDP that get-rdp-status is captured and replayed for. Optional, and
# deliberately not defaulted to a literal: capture.sh pins the first RDP the
# VPN reports when this is unset, and step 3 below reads the name back out of
# the captured object — the same source mock-semp resolves it from. So the
# fixture, the mock, the manifest and the run scripts all name the same RDP
# because they all read it from one place, rather than because a default
# happened to match the lab.
rdp_name="${RDP_NAME:-}"
runs="$bin/runs/$(date +%Y%m%d-%H%M%S)-regen"
mkdir -p "$runs"

# Resolve config path: try as-given, then relative to CWD, then performance dir,
# then repo root. Makes `./regen-golden.sh` work from anywhere.
resolve_config() {
  local c=$1
  for candidate in "$c" "$PWD/$c" "$here/$c" "$repo_root/$c"; do
    if [[ -f "$candidate" ]]; then
      readlink -f "$candidate"
      return 0
    fi
  done
  return 1
}
if ! config="$(resolve_config "$config")"; then
  echo "config not found: ${CONFIG_FILE:-$repo_root/broker-config.yaml}" >&2
  echo "tried: as-given, \$PWD/, $here/, $repo_root/" >&2
  exit 2
fi

for b in fidelity mcp-server; do
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

mcp_pid=
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

# broker_url_from_config extracts the `url:` field for $broker_alias from a
# broker-config.*.yaml. Uses awk (rather than yq) to keep the script's
# dependency footprint identical to what it already needs. Fails loudly if
# the alias or url is missing — silently defaulting would send capture.sh
# at the wrong broker and hide a config typo.
#
# If the extracted value is a ${NAME} env-var reference (broker-config.real.yaml
# uses ${BROKER_URL} so no real lab IP lands in the repo), it is resolved from
# the current environment. An unresolved reference returns empty so the caller
# fails loud instead of shipping the literal string to curl.
broker_url_from_config() {
  local cfg=$1 alias=$2
  local raw
  raw="$(awk -v alias="$alias" '
    # Match the alias line, e.g. "  my-broker:"
    match($0, "^[[:space:]]{2}" alias ":[[:space:]]*$") { in_block=1; next }
    # Any subsequent line at the same 2-space indent that is an alias header
    # ends the block. Guards against reading a url from a later broker.
    in_block && /^[[:space:]]{2}[A-Za-z0-9_-]+:[[:space:]]*$/ { in_block=0 }
    in_block && match($0, /^[[:space:]]+url:[[:space:]]*/) {
      v = substr($0, RSTART + RLENGTH)
      gsub(/^["'\'']|["'\'']$/, "", v)
      print v
      exit
    }
  ' "$cfg")"
  # Resolve a lone ${NAME} reference via bash indirect expansion. Only the
  # single-token form is handled; a mixed value like "http://${HOST}:80" would
  # need envsubst — not a shape this repo uses, so we don't pull in the dep.
  if [[ "$raw" =~ ^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$ ]]; then
    printf '%s\n' "${!BASH_REMATCH[1]-}"
    return
  fi
  printf '%s\n' "$raw"
}

# MCP auto-loads .env from the config file's directory. When the config is
# test/performance/broker-config.real.yaml there's no .env alongside it, so pull
# credentials + broker URL from the repo-root .env (or the caller's env) and
# export them. broker-config.real.yaml references ${BROKER_URL} so no lab IP
# lives in the repo — that means BROKER_URL must be present in the env before
# MCP starts, alongside the credentials.
if [[ -f "$repo_root/.env" ]]; then
  set -a; . "$repo_root/.env"; set +a
fi
: "${BROKER_URL:?BROKER_URL unset (missing from $repo_root/.env and shell env)}"
: "${BROKER_USERNAME:?BROKER_USERNAME unset (missing from $repo_root/.env and shell env)}"
: "${BROKER_PASSWORD:?BROKER_PASSWORD unset (missing from $repo_root/.env and shell env)}"
export BROKER_URL BROKER_USERNAME BROKER_PASSWORD

echo "== 1. MCP server on :9090 (config: $config)"
setsid bash -c "cd '$repo_root' && CONFIG_FILE='$config' exec '$bin/mcp-server'" \
  >"$runs/mcp.log" 2>&1 &
mcp_pid=$!
wait_for_http "http://localhost:9090/health" mcp-server

echo "== 2. broker aliases MCP loaded from $config"
grep -E '^[[:space:]]{2}[A-Za-z0-9_-]+:$' "$config" | sed -E 's/^[[:space:]]+/    /; s/:$//'

broker_url="$(broker_url_from_config "$config" "$broker_alias")"
if [[ -z "$broker_url" ]]; then
  echo "could not find brokers.$broker_alias.url in $config" >&2
  exit 2
fi

echo "== 3. canned/*: direct SEMP capture from $broker_url (vpn=$vpn rdp=${rdp_name:-<first in VPN>})"
BROKER_URL="$broker_url" \
  BROKER_USERNAME="$BROKER_USERNAME" \
  BROKER_PASSWORD="$BROKER_PASSWORD" \
  MSG_VPN="$vpn" \
  RDP_NAME="$rdp_name" \
  "$here/mock-semp/canned/capture.sh" 2>&1 | tee "$runs/canned.log"

# Read the pinned name back out of the capture, exactly as mock-semp does at
# startup (pinnedRDPName in mock-semp/handler.go). capture.sh may have derived
# it, so this — not $RDP_NAME — is what the golden capture and the manifest
# must use. Parsing the fixture rather than the capture log keeps the manifest
# describing the bytes on disk.
rdp_object="$here/mock-semp/canned/rdp_object.json"
if command -v jq >/dev/null 2>&1; then
  rdp_name="$(jq -r '.data.restDeliveryPointName // ""' "$rdp_object")"
else
  rdp_name="$(grep -oE '"restDeliveryPointName"[[:space:]]*:[[:space:]]*"[^"]*"' "$rdp_object" |
    head -1 | sed -E 's/.*"([^"]+)"$/\1/')"
fi
if [[ -z "$rdp_name" ]]; then
  echo "capture wrote no restDeliveryPointName into $rdp_object — cannot pin an RDP." >&2
  exit 2
fi
echo "   pinned RDP: $rdp_name"

echo "== 4. fidelity -capture (alias=$broker_alias vpn=$vpn rdp=$rdp_name)"
# A checkout that has never captured fixtures has no fidelity/golden/ yet —
# that's the normal state of a fresh clone (see the fixtures-manifest.sh
# preflight above), so create it rather than letting fidelity -capture fail
# staging its first golden file into a directory that was never made.
mkdir -p "$here/fidelity/golden"
"$bin/fidelity" -mcp-url http://localhost:9090 -broker "$broker_alias" -vpn "$vpn" -rdp "$rdp_name" \
  -golden-dir "$here/fidelity/golden" -capture 2>&1 | tee "$runs/regen.log"

echo "== 5. sanitize canned and golden — strip lab-identifying values"
# Runs before the manifest so the recorded hashes are of the scrubbed bytes —
# otherwise every later check would flag sanitization as a hand-edit.
# Idempotent, so recaptures that happen to be already-clean pass through as
# a no-op.
"$here/mock-semp/canned/sanitize.sh" 2>&1 | tee "$runs/sanitize.log"

# The paginated list-rdps fidelity check is the only thing in the suite that
# walks a cursor chain, and it can only do that if this capture produced more
# than one RDP page. With a single page it still passes — a one-page golden
# against a one-page replay — so the coverage would disappear without anything
# turning red. Warn here, where recapturing from a bigger VPN is still an
# option; mock-semp repeats the warning at every startup.
rdps_pages=$(find "$here/mock-semp/canned" -maxdepth 1 -name 'rdps_page*.json' | wc -l)
if (( rdps_pages < 2 )); then
  echo "!! WARNING: this VPN yielded $rdps_pages RDP page(s) (<=100 RDPs), so the paginated" >&2
  echo "   list-rdps check has no cursor chain to walk and pagination goes uncovered." >&2
  echo "   Capture from a VPN with more than 100 RDPs to keep that coverage." >&2
fi

echo "== 6. record fixtures.manifest (hashes + capture time for both sets)"
# One manifest per capture is what lets run.sh prove canned and golden came
# from the same pass. Written last, after sanitization, so it describes
# exactly the bytes the mock will replay.
BROKER_ALIAS="$broker_alias" VPN="$vpn" RDP_NAME="$rdp_name" "$here/fixtures-manifest.sh" write

echo "== canned regenerated at $here/mock-semp/canned"
echo "== goldens regenerated at $here/fidelity/golden"
echo "== manifest at $here/fixtures.manifest (local only — both sets are gitignored)"
