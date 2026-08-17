#!/usr/bin/env bash
# LLM e2e eval suite: run every scenario under scenarios/.
#
# Usage:
#   ./run-all.sh                          # run every scenario
#   ./run-all.sh --filter f5              # only scenarios whose filename matches
#   ./run-all.sh --no-precheck            # skip MCP/broker reachability check
#   ./run-all.sh --fail-fast              # stop after the first failing scenario
#
# Exit: 0 if every scenario passed, 1 otherwise.

set -euo pipefail

RUNNER_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCENARIO_DIR="$RUNNER_DIR/scenarios"
HELPERS="$RUNNER_DIR/helpers.sh"

# Suite-wide config — MCP_URL, MCP_BEARER_TOKEN, PRECHECK_BROKERS,
# PINNED_CLAUDE_VERSION, etc. Anything exported beforehand wins.
# shellcheck disable=SC1091
source "$RUNNER_DIR/config.env"

# Source helpers.sh for shared colours and log_* functions. helpers.sh
# also sets MCP_URL to the server's base URL (no /mcp suffix) — our
# config.env defaults to the same shape, so the two harmonize cleanly.
# Fallback covers the edge case where helpers.sh isn't on disk (CI image
# without the e2e-monitoring tree).
if [ -f "$HELPERS" ]; then
    # shellcheck disable=SC1090
    source "$HELPERS"
else
    RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; CYAN='\033[0;36m'; NC='\033[0m'
fi

FILTER=""
SKIP_PRECHECK=0
FAIL_FAST=0

while [ $# -gt 0 ]; do
    case "$1" in
        --filter)        FILTER="$2";  shift 2 ;;
        --no-precheck)  SKIP_PRECHECK=1; shift ;;
        --fail-fast)     FAIL_FAST=1; shift ;;
        -h|--help)       sed -n '2,10p' "$0"; exit 0 ;;
        *) echo "unknown arg: $1" >&2; exit 2 ;;
    esac
done

# ── Precheck ─────────────────────────────────────────────────────────────────
# Verify MCP server + every PRECHECK_BROKERS entry is reachable BEFORE
# burning API credits on the suite. Without this, brokers-down looks like
# N weird scenario fails + 1 passing safety-mcp-down — confusing and
# expensive. Skippable via --no-precheck for niche subsets.
MCP_HEADERS=(
    -H 'Content-Type: application/json'
    -H 'Accept: application/json, text/event-stream'
    -H "Authorization: Bearer $MCP_BEARER_TOKEN"
)

# POST a JSON-RPC body to the MCP endpoint. First arg is the optional session
# id (empty for initialize); second is the JSON-RPC body. `-i` is added when
# the caller needs response headers (for the Mcp-Session-Id on initialize).
mcp_call() {
    local sid="$1" body="$2" curl_args=()
    [ -n "$sid" ] && curl_args+=(-H "Mcp-Session-Id: $sid")
    curl -s --max-time 5 "${curl_args[@]}" "${MCP_HEADERS[@]}" \
        -X POST "$MCP_URL/mcp" -d "$body" 2>/dev/null
}

# protocolVersion pins 2025-11-25, the newest revision this server negotiates;
# see mcp_initialize in ../e2e-common/lib.sh for why it is not go-sdk's latest.
precheck() {
    local sid
    sid=$(curl -s --max-time 5 -i -X POST "$MCP_URL/mcp" "${MCP_HEADERS[@]}" \
        -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"precheck","version":"0"}}}' \
        2>/dev/null | grep -i '^Mcp-Session-Id:' | awk '{print $2}' | tr -d '\r\n')
    if [ -z "$sid" ]; then
        echo -e "${RED}[PRECHECK FAIL]${NC} MCP server not reachable on $MCP_URL/mcp" >&2
        echo -e "${YELLOW}[HINT]${NC}  run ./setup-fixtures.sh to bring up brokers + MCP server" >&2
        return 1
    fi
    # Complete the MCP handshake before issuing tool calls. The spec requires
    # `notifications/initialized` after `initialize`; this server tolerates
    # skipping it today, but a stricter server (or a future tightening) would
    # reject the tools/call below and surface as a confusing precheck fail.
    curl -s --max-time 5 "${MCP_HEADERS[@]}" -H "Mcp-Session-Id: $sid" \
        -X POST "$MCP_URL/mcp" \
        -d '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
        >/dev/null 2>&1 || true
    local req_id=2
    for broker in $PRECHECK_BROKERS; do
        local ver raw
        raw=$(mcp_call "$sid" "{\"jsonrpc\":\"2.0\",\"id\":${req_id},\"method\":\"tools/call\",\"params\":{\"name\":\"get-broker-status\",\"arguments\":{\"broker\":\"${broker}\"}}}")
        # Accept either SSE-framed ("data: {…}") or plain JSON-RPC body —
        # the MCP server can legitimately return either depending on
        # transport config, and a parser that only handles SSE would
        # silently look like a broker-unreachable failure.
        ver=$(printf '%s\n' "$raw" \
            | { grep -oE 'data: \{.*' | sed 's/^data: //' || true; } \
            | jq -r '.result.structuredContent.version.description // empty' 2>/dev/null)
        if [ -z "$ver" ]; then
            ver=$(printf '%s\n' "$raw" \
                | jq -r '.result.structuredContent.version.description // empty' 2>/dev/null)
        fi
        if [ -z "$ver" ]; then
            echo -e "${RED}[PRECHECK FAIL]${NC} MCP up but ${broker} get-broker-status returned no data" >&2
            echo -e "${YELLOW}[HINT]${NC}  check container state: docker ps | grep solace" >&2
            echo -e "${YELLOW}[HINT]${NC}  re-run ./setup-fixtures.sh" >&2
            return 1
        fi
        echo -e "${GREEN}[PRECHECK PASS]${NC} ${MCP_URL} reachable, ${broker} get-broker-status returned $ver"
        req_id=$((req_id + 1))
    done
}

if [ "$SKIP_PRECHECK" -ne 1 ]; then
    precheck || exit 2
fi

# Echo the claude CLI version so logs are immediately greppable by the
# build under test. Model is intentionally not probed — it lives in the
# user's settings.json and we don't want the suite poking around there.
CLAUDE_VERSION=$(claude --version 2>/dev/null | awk '{print $1}')
echo -e "${CYAN}[INFO]${NC}  Claude CLI Version: ${CLAUDE_VERSION:-?}"
echo -e "${CYAN}[INFO]${NC}  Broker target: $BROKER_TARGET ($MCP_URL)"

mapfile -t SCENARIOS < <(find "$SCENARIO_DIR" -maxdepth 1 -name '*.json' | sort)
if [ -n "$FILTER" ]; then
    FILTERED=()
    for s in "${SCENARIOS[@]}"; do
        [[ "$(basename "$s")" == *"$FILTER"* ]] && FILTERED+=("$s")
    done
    SCENARIOS=("${FILTERED[@]}")
fi

if [ "${#SCENARIOS[@]}" -eq 0 ]; then
    echo "no scenarios found (dir=$SCENARIO_DIR filter='$FILTER')" >&2
    exit 2
fi

PASS=0; FAIL=0
declare -a FAIL_NAMES
declare -A SCENARIO_RESULT

# Per-scenario USD cost (one number per line) so the wrapper can sum and
# print a total at the end. EXIT trap cleans up unconditionally.
SCENARIO_COST_FILE=$(mktemp -t llm-suite-cost.XXXXXXXX)
export SCENARIO_COST_FILE
trap 'rm -f "$SCENARIO_COST_FILE"' EXIT INT TERM HUP

# Each scenario declares which brokers it runs on via a top-level `brokers`
# array. Default is ["broker-a"] when the field is missing — most scenarios
# don't gain coverage from broker-b (same fixture, same expected answer);
# only the ones whose entity name differs per broker (F3/F6 client names)
# opt in to dual-broker runs. See README "Per-scenario broker selection".
#
# Non-local-docker targets ignore the scenario's `brokers` field — those
# names refer to the local fixtures, not whatever aliases the lab MCP
# server exposes. Each scenario runs once against the first PRECHECK_BROKERS
# entry. To run against a different lab broker, export BROKER explicitly
# or call ./run-scenario.sh directly.
brokers_for() {
    local file="$1"
    if [ "$BROKER_TARGET" = "local-docker" ]; then
        jq -r '(.brokers // ["broker-a"]) | .[]' "$file"
    else
        echo "${PRECHECK_BROKERS%% *}"
    fi
}

# Pre-compute (scenario, broker) plan so the [idx/total] counter is honest.
declare -a PLAN_SCENARIOS PLAN_BROKERS
for scenario in "${SCENARIOS[@]}"; do
    while IFS= read -r broker; do
        [ -z "$broker" ] && continue
        PLAN_SCENARIOS+=("$scenario")
        PLAN_BROKERS+=("$broker")
    done < <(brokers_for "$scenario")
done

total=${#PLAN_SCENARIOS[@]}
for i in "${!PLAN_SCENARIOS[@]}"; do
    scenario="${PLAN_SCENARIOS[$i]}"
    broker="${PLAN_BROKERS[$i]}"
    idx=$((i+1))
    name="$(basename "$scenario" .json) [$broker]"
    echo
    echo "════════ [$idx/$total] $name ════════"
    # set -e would abort the loop on the first failing scenario; we want to
    # run every scenario and tally results at the end.
    set +e
    BROKER="$broker" bash "$RUNNER_DIR/run-scenario.sh" "$scenario"
    rc=$?
    set -e
    if [ "$rc" -eq 0 ]; then
        SCENARIO_RESULT[$name]="PASS"
        PASS=$((PASS+1))
    else
        SCENARIO_RESULT[$name]="FAIL"
        FAIL=$((FAIL+1)); FAIL_NAMES+=("$name")
        if [ "$FAIL_FAST" -eq 1 ]; then
            echo
            echo -e "${YELLOW}[fail-fast]${NC} stopping suite after first failure"
            break
        fi
    fi
done

TOTAL_COST=$(awk '{s+=$1} END {printf "%.4f", s+0}' "$SCENARIO_COST_FILE")

# ── Result table ──────────────────────────────────────────────────────────────
echo
printf -- "%.0s═" {1..56}; echo
printf "%-44s %s\n" "scenario" "result"
printf -- "%.0s─" {1..56}; echo
for i in "${!PLAN_SCENARIOS[@]}"; do
    name="$(basename "${PLAN_SCENARIOS[$i]}" .json) [${PLAN_BROKERS[$i]}]"
    status="${SCENARIO_RESULT[$name]:-SKIP}"  # SKIP if fail-fast aborted before this row
    case "$status" in
        PASS) color="$GREEN" ;;
        FAIL) color="$RED"   ;;
        SKIP) color="$YELLOW" ;;
        *)    color="$NC"    ;;
    esac
    printf "%-44s ${color}%s${NC}\n" "$name" "$status"
done
printf -- "%.0s═" {1..56}; echo
printf "Summary: ${GREEN}PASS=%d${NC}  ${RED}FAIL=%d${NC}  (of %d)  total cost: \$%s\n" "$PASS" "$FAIL" "$total" "$TOTAL_COST"
[ "$FAIL" -gt 0 ] && echo -e "${RED}Failed:${NC} ${FAIL_NAMES[*]}"

[ "$FAIL" -eq 0 ] && exit 0 || exit 1
