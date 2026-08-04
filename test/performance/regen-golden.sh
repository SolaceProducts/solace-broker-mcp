#!/usr/bin/env bash
# Regenerate fidelity/golden/*.json from the real lab broker.
# Starts MCP with broker-config.yaml (the local dev config that reads
# credentials from .env via ${BROKER_USERNAME}/${BROKER_PASSWORD}), invokes
# each tool via `fidelity -capture`, tears down.

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

# MCP auto-loads .env from the config file's directory. When the config is
# test/performance/broker-config.real.yaml there's no .env alongside it, so pull
# credentials from the repo-root .env (or the caller's env) and export them.
if [[ -f "$repo_root/.env" ]]; then
  set -a; . "$repo_root/.env"; set +a
fi
: "${BROKER_USERNAME:?BROKER_USERNAME unset (missing from $repo_root/.env and shell env)}"
: "${BROKER_PASSWORD:?BROKER_PASSWORD unset (missing from $repo_root/.env and shell env)}"
export BROKER_USERNAME BROKER_PASSWORD

echo "== 1. MCP server on :9090 (config: $config)"
setsid bash -c "cd '$repo_root' && CONFIG_FILE='$config' exec go run ./cmd/server" \
  >"$runs/mcp.log" 2>&1 &
mcp_pid=$!
wait_for_http "http://localhost:9090/health" mcp-server

echo "== 2. broker aliases MCP loaded from $config"
grep -E '^[[:space:]]{2}[A-Za-z0-9_-]+:$' "$config" | sed -E 's/^[[:space:]]+/    /; s/:$//'

echo "== 3. fidelity -capture (alias=$broker_alias vpn=$vpn)"
"$bin/fidelity" -mcp-url http://localhost:9090 -broker "$broker_alias" -vpn "$vpn" \
  -golden-dir "$here/fidelity/golden" -capture 2>&1 | tee "$runs/regen.log"

echo "== goldens regenerated at $here/fidelity/golden"
