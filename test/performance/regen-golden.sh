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
#   6. Rebuild mock-semp so go:embed picks up the fresh canned files.
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

if [[ ! -x "$bin/fidelity" ]]; then
  echo "missing $bin/fidelity — run ./build.sh first" >&2
  exit 2
fi

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
setsid bash -c "cd '$repo_root' && CONFIG_FILE='$config' exec go run ./cmd/server" \
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

echo "== 3. canned/*: direct SEMP capture from $broker_url (vpn=$vpn)"
BROKER_URL="$broker_url" \
  BROKER_USERNAME="$BROKER_USERNAME" \
  BROKER_PASSWORD="$BROKER_PASSWORD" \
  MSG_VPN="$vpn" \
  "$here/mock-semp/canned/capture.sh" 2>&1 | tee "$runs/canned.log"

echo "== 4. fidelity -capture (alias=$broker_alias vpn=$vpn)"
"$bin/fidelity" -mcp-url http://localhost:9090 -broker "$broker_alias" -vpn "$vpn" \
  -golden-dir "$here/fidelity/golden" -capture 2>&1 | tee "$runs/regen.log"

echo "== 5. sanitize canned and golden — strip lab-identifying values"
# Runs before the rebuild so go:embed picks up the scrubbed bytes. Idempotent,
# so recaptures that happen to be already-clean pass through as a no-op.
"$here/mock-semp/canned/sanitize.sh" 2>&1 | tee "$runs/sanitize.log"

echo "== 6. rebuild mock-semp so go:embed picks up the new canned files"
( cd "$here/mock-semp" && go build -o "$bin/mock-semp" . )

echo "== canned regenerated at $here/mock-semp/canned"
echo "== goldens regenerated at $here/fidelity/golden"
echo "== mock rebuilt at $bin/mock-semp"
