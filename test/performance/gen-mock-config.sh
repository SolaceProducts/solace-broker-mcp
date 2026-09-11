#!/usr/bin/env bash
# Copyright 2024-2026 Solace Corporation. All rights reserved.
#
# gen-mock-config.sh — write an MCP broker config for N mock brokers.
#
# broker-config.mock.yaml hand-lists 50 aliases. mock-semp -listen-count and
# loadgen -broker-count both already scale past 50, so this generator is the
# only thing standing between the harness and the ">50 brokers, find the
# maximum" run.
#
# Usage:
#   ./gen-mock-config.sh -n 200 -o broker-config.gen200.yaml
#   ./gen-mock-config.sh -n 120 -prefix broker -port-start 18081 -o ./broker-config.gen120.yaml
#
#   -n <count>        number of brokers (required)
#   -prefix <name>    alias prefix (default broker) — must match loadgen's
#                     -broker-prefix
#   -port-start <p>   first mock port (default 18081) — must match mock-semp's
#                     -listen-start. NOTE: run.sh, run-loadgen.sh and
#                     run-mcp.sh all assume 18081 (port waits, error injection
#                     and Box B's reachability check), so a config generated
#                     with a different value cannot be driven by them — start
#                     mock-semp by hand with a matching -listen-start. The
#                     failure is loud and early: the fidelity gate cannot
#                     reach a mock that is not there.
#                     -listen-start
#   -o <path>         output path (required)
#   -f                overwrite an existing output file
#
# Then run the three sides with matching numbers:
#
#   Box A:  BROKERS=200 ./run-loadgen.sh http://<box-b>:9090
#   Box B:  CONFIG_FILE=./broker-config.gen200.yaml MOCK_HOST=<box-a> ./run-mcp.sh
#
# --- end usage ---
#
# Three constraints govern the output, and each has a failure mode that is not
# loud:
#
#   1. The aliases must be byte-identical to the ones loadgen generates. loadgen
#      builds "<prefix>-%02d" (see resolveBrokers in loadgen/main.go); a
#      mismatch does not fail at startup — every tool call 404s and the run
#      reads like a server fault.
#   2. The three ${...} placeholders must survive verbatim. MCP hard-fails on an
#      unset one, so a generator that expanded or dropped them would emit a
#      config that cannot start.
#   3. It must refuse to write over the committed broker-config.mock.yaml.
#      Nobody should commit a 200-broker config; the generated ones are
#      gitignored (broker-config.gen*.yaml).

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# perf_usage prints the usage block from this file's own header.
#
# Anchored on content rather than a line range: a hardcoded `sed -n '4,32p'`
# printed the options plus the first half of the rationale paragraph below
# them, so `--help` ended mid-clause — and it silently drifted every time the
# header gained a line. The markers below bound exactly the usage text.
perf_usage() {
  sed -n '/^# Usage:/,/^# --- end usage ---/p' "${BASH_SOURCE[0]}" \
    | sed -e '/^# --- end usage ---/d' -e 's/^# \{0,1\}//'
}

count="" prefix="broker" port_start=18081 out="" force=0

while (( $# )); do
  case "$1" in
    -n)          count="${2:?-n needs a value}"; shift 2 ;;
    -prefix)     prefix="${2:?-prefix needs a value}"; shift 2 ;;
    -port-start) port_start="${2:?-port-start needs a value}"; shift 2 ;;
    -o)          out="${2:?-o needs a value}"; shift 2 ;;
    -f)          force=1; shift ;;
    -h|--help)   perf_usage; exit 0 ;;
    *)           echo "unknown argument: $1 (see -h)" >&2; exit 2 ;;
  esac
done

[[ -n "$count" ]] || { echo "-n <count> is required (see -h)" >&2; exit 2; }
[[ -n "$out" ]]   || { echo "-o <path> is required (see -h)" >&2; exit 2; }

if ! [[ "$count" =~ ^[0-9]+$ ]] || (( count < 1 )); then
  echo "-n must be a positive integer, got: $count" >&2
  exit 2
fi
if ! [[ "$port_start" =~ ^[0-9]+$ ]]; then
  echo "-port-start must be an integer, got: $port_start" >&2
  exit 2
fi
# The last port has to be a port. Catching it here beats a partial config and a
# bind failure N brokers into startup.
last_port=$(( port_start + count - 1 ))
if (( port_start < 1 || last_port > 65535 )); then
  echo "port range $port_start..$last_port is outside 1..65535 — lower -n or -port-start" >&2
  exit 2
fi
# Two fixed ports must stay outside the broker range, and a collision with
# either is not loud: the broker on that port answers something other than
# SEMP, so the run 404s on one broker and reads as a broker fault — the same
# non-loud class this file's header enumerates.
#
#   19000  the mock's control endpoint, inside the range from -n 920
#   9090   the MCP server itself, reachable with a low -port-start
for reserved in 19000 9090; do
  if (( port_start <= reserved && last_port >= reserved )); then
    case "$reserved" in
      19000) what="the mock's control port" ;;
      9090)  what="the MCP server's port" ;;
    esac
    echo "port range $port_start..$last_port spans $what ($reserved)" >&2
    echo "lower -n, or move the range with -port-start" >&2
    exit 2
  fi
done

if ! [[ "$prefix" =~ ^[A-Za-z0-9][A-Za-z0-9_-]*$ ]]; then
  # Broker aliases end up as YAML mapping keys and in URLs; keep them to the
  # shape loadgen's generated names take.
  echo "-prefix must match [A-Za-z0-9][A-Za-z0-9_-]* , got: $prefix" >&2
  exit 2
fi

# Check loadgen's alias format string here, every time the generator runs, and
# not only from the self-test. The self-test has no runner in CI, so it fires
# when someone remembers it; this fires when it matters. A mismatch is the
# failure mode this whole script exists to avoid and the one that is not loud:
# every tool call 404s at the mock and the run reads like a server fault.
loadgen_src="$here/loadgen/main.go"
if [[ -r "$loadgen_src" ]] && ! grep -q '"%s-%02d"' "$loadgen_src"; then
  echo "loadgen's alias format string is no longer \"%s-%02d\" — this generator would" >&2
  echo "produce names loadgen does not ask for, and every tool call would 404 at the mock." >&2
  echo "Check resolveBrokers in $loadgen_src and update the printf below to match." >&2
  exit 2
fi

# Refuse the committed config by identity, not by name: -o ./broker-config.mock.yaml,
# an absolute path to it and a path through a symlink are all the same file.
protected="$here/broker-config.mock.yaml"
if [[ -e "$out" ]] && [[ "$(realpath "$out")" == "$(realpath "$protected")" ]]; then
  echo "refusing to overwrite the committed $protected" >&2
  echo "write to a gitignored path instead, e.g. -o broker-config.gen${count}.yaml" >&2
  exit 2
fi
if [[ -e "$out" ]] && (( ! force )); then
  echo "$out exists — pass -f to overwrite" >&2
  exit 2
fi

# Single-quoted heredoc: the ${MOCK_HOST} / ${BROKER_USERNAME} /
# ${BROKER_PASSWORD} placeholders below must reach the file unexpanded, because
# MCP is what expands them. Anything this script does need to interpolate is
# passed through the environment and printf'd by awk instead.
# mktemp beside the target rather than "$out.tmp.$$": the latter is a
# predictable name derived from the caller's own path, so with -o pointing at a
# shared directory a local user could pre-create the name as a symlink and have
# this script truncate whatever it points at. The content is non-secret, but
# the fix costs one line.
tmp="$(mktemp "$(dirname "$out")/.$(basename "$out").XXXXXX")"
trap 'rm -f "$tmp"' EXIT

{
  cat <<'HEADER_START'
# This is to be used in LAB ONLY.
#
# Performance — MCP config pointing at mock-semp.
#
# GENERATED by gen-mock-config.sh. Do not edit and do not commit: regenerate it.
HEADER_START
  printf '# Generated %s for %d brokers on ports %d..%d.\n' \
    "$(date -Iseconds)" "$count" "$port_start" "$last_port"
  printf '#\n'
  printf '# Reproduce with:\n'
  printf '#   ./gen-mock-config.sh -n %d -prefix %s -port-start %d -o <path>\n' \
    "$count" "$prefix" "$port_start"
  printf '#\n'
  printf '# Aliases %s-%02d..%s-%02d, matching what loadgen generates for\n' \
    "$prefix" 1 "$prefix" "$count"
  printf '#   -broker-count %d -broker-prefix %s\n' "$count" "$prefix"
  printf '# and the ports mock-semp binds for\n'
  printf '#   -listen-start %d -listen-count %d\n' "$port_start" "$count"
  cat <<'HEADER_END'
#
# MOCK_HOST is required (MCP hard-fails on unset ${VAR}).
#   - Single-host smoke: MOCK_HOST=localhost.
#   - Split-host run:    MOCK_HOST=<Box A LAN IP>, MCP runs on Box B.
#
# The mock accepts any credentials (it doesn't validate auth), so
# BROKER_USERNAME / BROKER_PASSWORD can be set to any non-empty value —
# MCP still requires them to build the basic-auth header.

listen_address: 0.0.0.0
allow_remote_unauthenticated: true
port: 9090
log_level: info

tls_cert_file: ""
tls_key_file: ""

mcp_client_auth:
  mode: disabled

semp:
  max_concurrent_per_broker: 10
  request_timeout_duration: 1m
  # Pacer off. Only 0s turns it off: any positive value builds a real ticker
  # per broker. See broker-config.mock.yaml for the long version.
  request_min_interval: 0s
  retries: 10
  retry_min_interval: 3s
  retry_max_interval: 30s

brokers:
HEADER_END

  # %02d, not %d and not a width computed from $count: it is literally
  # loadgen's format string, so 9 -> broker-09 and 100 -> broker-100 come out
  # the same on both sides. A "sensible" %03d for a 200-broker run would
  # rename every one of the first 99 brokers and 404 the lot.
  for (( i = 1; i <= count; i++ )); do
    printf '  %s-%02d: { url: "http://${MOCK_HOST}:%d", insecure_skip_verify: true, auth: { mode: basic, username: "${BROKER_USERNAME}", password: "${BROKER_PASSWORD}" } }\n' \
      "$prefix" "$i" "$(( port_start + i - 1 ))"
  done
} >"$tmp"

mv "$tmp" "$out"
trap - EXIT

echo "wrote $out: $count brokers, ${prefix}-$(printf '%02d' 1)..${prefix}-$(printf '%02d' "$count"), ports $port_start..$last_port"
