#!/usr/bin/env bash
# Run a single scenario N times against the same broker.
#
# Surfaces LLM nondeterminism — useful when a scenario fails once on a full
# suite and you want to know if it's a flake or a hard regression. Reports
# pass rate, per-iteration result, and total/average cost.
#
# Usage:
#   ./run-flake-check.sh scenarios/safety-mcp-down.json
#   ./run-flake-check.sh scenarios/f5-detect.json 5
#   N=20 BROKER=broker-b ./run-flake-check.sh scenarios/f3-subscriptions.json
#
# Args:
#   $1   scenario file (required)
#   $2   iteration count (optional, default $N or 10)
# Env:
#   BROKER   broker-a (default) or broker-b — passed through to run-scenario.sh
#   N        iteration count (overridden by $2)
#
# Exit: 0 if every iteration passed, 1 otherwise.

set -euo pipefail

RUNNER_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HELPERS="$RUNNER_DIR/helpers.sh"

# Mirror run-all.sh / run-scenario.sh: load suite-wide config so
# BROKER_TARGET, MCP_URL, PINNED_CLAUDE_VERSION, etc. are resolved the
# same way here as in a full-suite run. The child run-scenario.sh sources
# it again — values pre-exported here still win (every entry is ${X:-default}).
# shellcheck disable=SC1091
source "$RUNNER_DIR/config.env"

if [ -f "$HELPERS" ]; then
    # shellcheck disable=SC1090
    source "$HELPERS"
else
    RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; CYAN='\033[0;36m'; NC='\033[0m'
fi

SCENARIO="${1:-}"
if [ -z "$SCENARIO" ] || [ ! -f "$SCENARIO" ]; then
    echo "usage: $0 <scenario.json> [N]" >&2
    exit 2
fi
ITERS="${2:-${N:-10}}"
BROKER="${BROKER:-broker-a}"
NAME="$(basename "$SCENARIO" .json)"

# Per-iteration cost goes into this file so we can sum at the end. The
# child run-scenario.sh appends to whatever path is exported here. Trap
# cleans up on any exit path.
SCENARIO_COST_FILE=$(mktemp -t llm-flake-cost.XXXXXXXX)
export SCENARIO_COST_FILE
trap 'rm -f "$SCENARIO_COST_FILE"' EXIT INT TERM HUP

PASS=0; FAIL=0
declare -a RESULTS

for i in $(seq 1 "$ITERS"); do
    echo
    echo "════════ [$i/$ITERS] $NAME [$BROKER] ════════"
    set +e
    BROKER="$BROKER" bash "$RUNNER_DIR/run-scenario.sh" "$SCENARIO"
    rc=$?
    set -e
    if [ "$rc" -eq 0 ]; then
        PASS=$((PASS+1)); RESULTS+=("PASS")
    else
        FAIL=$((FAIL+1)); RESULTS+=("FAIL")
    fi
done

TOTAL_COST=$(awk '{s+=$1} END {printf "%.4f", s+0}' "$SCENARIO_COST_FILE")
AVG_COST=$(awk -v n="$ITERS" '{s+=$1} END {printf "%.4f", (n>0 ? s/n : 0)}' "$SCENARIO_COST_FILE")

echo
printf -- "%.0s═" {1..56}; echo
printf "Flake check: %s [%s]\n" "$NAME" "$BROKER"
printf -- "%.0s─" {1..56}; echo
for i in "${!RESULTS[@]}"; do
    status="${RESULTS[$i]}"
    case "$status" in
        PASS) color="$GREEN" ;;
        FAIL) color="$RED"   ;;
        *)    color="$NC"    ;;
    esac
    printf "  iter %2d  ${color}%s${NC}\n" "$((i+1))" "$status"
done
printf -- "%.0s═" {1..56}; echo
printf "Pass rate: ${GREEN}%d${NC}/%d  (${RED}FAIL=%d${NC})  cost: \$%s total, \$%s avg\n" \
    "$PASS" "$ITERS" "$FAIL" "$TOTAL_COST" "$AVG_COST"

[ "$FAIL" -eq 0 ] && exit 0 || exit 1
