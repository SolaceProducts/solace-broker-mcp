#!/usr/bin/env bash
# LLM e2e eval suite: single-scenario runner.
#
# Drives one NL prompt through Claude Code CLI and runs assertions against
# the resulting stream-json output. The scenario file declares both the
# prompt and the assertions, so this script is generic across scenarios.
#
# Usage:   ./run-scenario.sh scenarios/<name>.json
# Prereqs: brokers + MCP server up; `claude`, `jq`, `envsubst` on PATH.
# Exit:    0 pass, 1 assertion failed, 2 setup/invocation error.
#
# Scenario fields (all optional except `prompt`):
#   prompt                    NL question sent to Claude
#   brokers                   array of brokers run-all.sh iterates this scenario
#                             over (default: ["broker-a"]). Single-scenario runs
#                             here ignore this field — use $BROKER env var.
#   mcp_config                path to MCP config JSON (default: mcp-config.json)
#   expected_tool             single tool name that MUST be called
#   expected_tool_any_of      array; at least one MUST be called
#   expected_tools_all_of     array; all MUST be called
#   expected_tools_none       true → assert ZERO tool calls (MCP-down test)
#   ground_truth.jq           jq path applied to the tool_result to extract entity set
#   ground_truth.answer_regex regex applied to the answer to extract entity set
#   required_substrings       each MUST appear in the answer (case-insensitive)
#   required_substrings_any_of  at least ONE MUST appear (case-insensitive) —
#                             use when paraphrases are equally valid (e.g.
#                             healthy / operational / up)
#   forbidden_substrings      each MUST NOT appear in the answer
#   numeric_match.regex       extracts a number from the answer
#   numeric_match.min/max     extracted number MUST fall in [min, max]
#
# Env-var substitution: ../helpers.sh is auto-sourced (if present) so
# scenarios can reference $F3_CLIENT_NAME_A etc. as literals.

set -euo pipefail

RUNNER_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HELPERS="$RUNNER_DIR/../helpers.sh"

# Suite-wide config. Anything exported beforehand wins (each value is
# `${X:-default}` in config.env). The runner has no hardcoded MCP URL,
# bearer token, or CLI version pin — they all live there.
# shellcheck disable=SC1091
source "$RUNNER_DIR/config.env"

DEFAULT_MCP_CONFIG="$RUNNER_DIR/mcp-config.json.tmpl"

# Stream-json events go straight to the user's terminal AND a temp file we
# parse for assertions. Rendered MCP config (from a .tmpl) lives next to it,
# also temp. Trap-cleanup deletes both unconditionally — no persistent
# transcripts.
RUN_FILE=$(mktemp -t llm-scenario.XXXXXXXX.jsonl)
RENDERED_MCP_CONFIG=""
# shellcheck disable=SC2317  # invoked indirectly via trap
cleanup() {
    rm -f "$RUN_FILE"
    [ -n "$RENDERED_MCP_CONFIG" ] && rm -f "$RENDERED_MCP_CONFIG"
}
trap cleanup EXIT INT TERM HUP

# Source helpers.sh for shared colours (RED/GREEN/YELLOW/CYAN/NC), the
# log_info/log_ok/log_fail family, and fixture name vars (F1_*,
# F3_CLIENT_NAME_A, F5_QUEUE, …) that envsubst needs to resolve $-refs in
# scenario JSON. The `set -a` wrapping exports the fixture names. Fallback
# defs cover the case where helpers.sh isn't on disk (e.g. CI image without
# the e2e-monitoring tree) so the version-pin check below can still log.
if [ -f "$HELPERS" ]; then
    set -a
    # shellcheck disable=SC1090
    source "$HELPERS"
    set +a
else
    RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; CYAN='\033[0;36m'; NC='\033[0m'
    log_info() { echo -e "${CYAN}[INFO]${NC}  $*" >&2; }
    log_ok()   { echo -e "${GREEN}[PASS]${NC}  $*" >&2; }
    log_fail() { echo -e "${RED}[FAIL]${NC}  $*" >&2; }
fi
# helpers.sh has log_warn but not these two — define them either way.
log_err()  { echo -e "${RED}[ERROR]${NC} $*" >&2; }
log_hint() { echo -e "${YELLOW}[HINT]${NC}  $*" >&2; }

# Scenarios reference $BROKER and unsuffixed per-broker vars (e.g. $F3_CLIENT_NAME)
# so a single scenario file covers both local brokers. The runner picks the
# broker via $BROKER (default first PRECHECK_BROKERS entry) and, on
# local-docker, aliases the matching _A / _B vars from helpers.sh into the
# unsuffixed names that scenarios reference. Non-local targets don't have
# our per-broker fixture aliases — the aliasing is skipped, and scenarios
# that reference $F3_CLIENT_NAME etc. will trip the UNSET_VARS check below
# with a clear error.
BROKER="${BROKER:-${PRECHECK_BROKERS%% *}}"
case "$BROKER" in
    broker-a) BROKER_SUFFIX=A ;;
    broker-b) BROKER_SUFFIX=B ;;
    *)        BROKER_SUFFIX="" ;;
esac
export BROKER
if [ -n "$BROKER_SUFFIX" ]; then
    for v in F3_CLIENT_NAME F6_SUB_CLIENT_NAME; do
        src="${v}_${BROKER_SUFFIX}"
        export "$v"="${!src:-}"
    done
fi

SCENARIO_FILE="${1:-}"
if [ -z "$SCENARIO_FILE" ] || [ ! -f "$SCENARIO_FILE" ]; then
    echo "usage: $0 <scenario.json>" >&2
    exit 2
fi

# ── Claude Code version pin ───────────────────────────────────────────────────
# PINNED_CLAUDE_VERSION comes from config.env. The suite's pass/fail
# behavior is calibrated against it — a newer build with different
# tool-call wrapping or refusal phrasing can flip scenarios silently.
# DISABLE_AUTOUPDATER stops the background updater from drifting the
# version mid-suite. CLAUDE_VERSION_PIN_OVERRIDE=1 skips the check for
# local experimentation.
export DISABLE_AUTOUPDATER=1

if [ "${CLAUDE_VERSION_PIN_OVERRIDE:-0}" != "1" ]; then
    ACTUAL_VERSION=$(claude --version 2>/dev/null | awk '{print $1}')
    if [ -z "$ACTUAL_VERSION" ]; then
        log_err "could not determine claude version (is the CLI installed?)"
        exit 2
    fi
    if [ "$ACTUAL_VERSION" != "$PINNED_CLAUDE_VERSION" ]; then
        log_err "claude version mismatch: have $ACTUAL_VERSION, pinned $PINNED_CLAUDE_VERSION"
        log_hint "reinstall with: npm install -g @anthropic-ai/claude-code@$PINNED_CLAUDE_VERSION"
        log_hint "or set CLAUDE_VERSION_PIN_OVERRIDE=1 to skip this check"
        exit 2
    fi
fi

# Detect $-references against unset OR empty vars before envsubst silently
# expands them to "" and produces a quietly-wrong scenario. Empty matters:
# `F5_QUEUE=""` would pass a `${!v+x}` set-check but still envsubst-out to ""
# and yield a wrong prompt. `grep` exits 1 when the scenario has no $-refs
# at all — guard the pipeline with `|| true` so set -e + pipefail don't abort.
UNSET_VARS=$(
    { grep -oE '\$\{?[A-Za-z_][A-Za-z0-9_]*\}?' "$SCENARIO_FILE" || true; } \
        | sed -E 's/[${}]//g' | sort -u \
        | { while read -r v; do [ -z "${!v:-}" ] && echo "$v"; done; true; }
)
if [ -n "$UNSET_VARS" ]; then
    log_err "scenario references unset env var(s):"
    echo "$UNSET_VARS" >&2
    log_hint "source test/e2e-monitoring/helpers.sh, or run after setup-brokers.sh"
    exit 2
fi
EXPANDED=$(envsubst < "$SCENARIO_FILE")

PROMPT=$(jq -r '.prompt' <<<"$EXPANDED")
MCP_CONFIG=$(jq -r --arg d "$DEFAULT_MCP_CONFIG" '.mcp_config // $d' <<<"$EXPANDED")
[[ "$MCP_CONFIG" = /* ]] || MCP_CONFIG="$RUNNER_DIR/$MCP_CONFIG"

# .tmpl → render with envsubst to a temp file so MCP_URL, MCP_BEARER_TOKEN,
# and MCP_URL_DOWN can be swapped per target without editing JSON. Plain
# .json is still accepted (back-compat for ad-hoc test configs).
if [[ "$MCP_CONFIG" == *.tmpl ]]; then
    if [ ! -f "$MCP_CONFIG" ]; then
        log_err "mcp_config template not found: $MCP_CONFIG"
        exit 2
    fi
    RENDERED_MCP_CONFIG=$(mktemp -t llm-mcp-config.XXXXXXXX.json)
    MCP_URL="$MCP_URL" MCP_URL_DOWN="$MCP_URL_DOWN" MCP_BEARER_TOKEN="$MCP_BEARER_TOKEN" \
        envsubst < "$MCP_CONFIG" > "$RENDERED_MCP_CONFIG"
    MCP_CONFIG="$RENDERED_MCP_CONFIG"
fi

EXPECTED_TOOL=$(jq -r '.expected_tool // empty' <<<"$EXPANDED")
EXPECTED_ANY=$(jq -r '.expected_tool_any_of[]? // empty' <<<"$EXPANDED")
EXPECTED_ALL=$(jq -r '.expected_tools_all_of[]? // empty' <<<"$EXPANDED")
EXPECTED_NONE=$(jq -r '.expected_tools_none // false' <<<"$EXPANDED")

GT_JQ=$(jq -r '.ground_truth.jq // empty' <<<"$EXPANDED")
GT_REGEX=$(jq -r '.ground_truth.answer_regex // empty' <<<"$EXPANDED")

REQUIRED=$(jq -r '.required_substrings[]? // empty' <<<"$EXPANDED")
REQUIRED_ANY=$(jq -r '.required_substrings_any_of[]? // empty' <<<"$EXPANDED")
FORBIDDEN=$(jq -r '.forbidden_substrings[]? // empty' <<<"$EXPANDED")

NUM_REGEX=$(jq -r '.numeric_match.regex // empty' <<<"$EXPANDED")
NUM_MIN=$(jq -r '.numeric_match.min // empty' <<<"$EXPANDED")
NUM_MAX=$(jq -r '.numeric_match.max // empty' <<<"$EXPANDED")

SCENARIO_NAME="$(basename "$SCENARIO_FILE" .json) [$BROKER]"

log_info "prompt: $PROMPT"

# ── LLM service auth ──────────────────────────────────────────────────────────
# Three modes, picked from config.env:
#   1. LLM_SERVICE_ENDPOINT set → route Claude Code through that endpoint
#      (LiteLLM proxy or other Anthropic-compatible gateway). API key
#      becomes ANTHROPIC_AUTH_TOKEN; LLM_SERVICE_MODEL_NAME, when set,
#      is passed via --model.
#   2. LLM_SERVICE_ENDPOINT empty but LLM_SERVICE_API_KEY set → talk to
#      api.anthropic.com directly with that key.
#   3. Both empty → fall back to the CLI's ambient login (~/.claude).
#      Friendly for local dev; not what CI uses.
CLAUDE_MODEL_ARGS=()
if [ -n "${LLM_SERVICE_ENDPOINT:-}" ]; then
    if [ -z "${LLM_SERVICE_API_KEY:-}" ]; then
        log_err "LLM_SERVICE_ENDPOINT is set but LLM_SERVICE_API_KEY is not"
        log_hint "export LLM_SERVICE_API_KEY in your shell (CI: pull from vault)"
        exit 2
    fi
    export ANTHROPIC_BASE_URL="$LLM_SERVICE_ENDPOINT"
    export ANTHROPIC_AUTH_TOKEN="$LLM_SERVICE_API_KEY"
    if [ -n "${LLM_SERVICE_MODEL_NAME:-}" ]; then
        CLAUDE_MODEL_ARGS=(--model "$LLM_SERVICE_MODEL_NAME")
    fi
elif [ -n "${LLM_SERVICE_API_KEY:-}" ]; then
    export ANTHROPIC_API_KEY="$LLM_SERVICE_API_KEY"
    if [ -n "${LLM_SERVICE_MODEL_NAME:-}" ]; then
        CLAUDE_MODEL_ARGS=(--model "$LLM_SERVICE_MODEL_NAME")
    fi
fi

CLAUDE_ARGS=(
    --mcp-config "$MCP_CONFIG"
    --strict-mcp-config
    # `--tools ""` disables Claude's built-in tools (Bash, Read, WebSearch, …)
    # so the agent can only reach for MCP tools.
    --tools ""
    # `--allowed-tools` IS load-bearing in --print mode — it's the
    # auto-approve list. Without it, every MCP tool call gets denied with
    # "I need permission to run X — please approve and I'll retry." Wildcard
    # auto-approves every tool from our MCP server (the only one loaded,
    # thanks to --strict-mcp-config), so the list stays maintenance-free
    # as the server adds tools. Assertion logic catches wrong tool choices.
    --allowed-tools "mcp__solace-broker__*"
    --output-format stream-json
    --verbose
    --max-turns 5
    "${CLAUDE_MODEL_ARGS[@]}"
    -p "$PROMPT"
)

# `tee` keeps the full raw JSONL in $RUN_FILE so jq can parse it for
# assertions; the compact formatter below summarizes each event into one
# short line for the user (raw stream-json is unreadable for 11 scenarios).
JQ_PRETTY='
def info(msg): "\u001b[0;36m[INFO]\u001b[0m  " + msg;
if .type == "assistant" then
  .message.content[] | (
    if .type == "tool_use" then
      info("tool call: \(.name | sub("mcp__solace-broker__"; "")) \(.input | tostring)")
    else empty end
  )
elif .type == "result" then
  info("answer: " + ((.result // "(empty)") | gsub("\n"; " ") | gsub(" +"; " ")
        | if length > 300 then .[:300] + "…" else . end)
    + "  ($\((.total_cost_usd * 10000 + 0.5 | floor) / 10000), \(.modelUsage | keys[0] // "?"))")
else empty end
'
set +e
claude "${CLAUDE_ARGS[@]}" | tee "$RUN_FILE" | jq -r --unbuffered "$JQ_PRETTY"
# Only the `claude` exit code matters for the run; jq is just a display
# prettifier and $RUN_FILE is the source of truth for all downstream
# assertions, so a malformed event slipping past jq is harmless here.
CLAUDE_EXIT=${PIPESTATUS[0]}
set -e

if [ "$CLAUDE_EXIT" -ne 0 ]; then
    log_err "claude exited $CLAUDE_EXIT"
    # The pretty filter above only surfaces tool_use/result events, so a
    # CLI-level failure (auth, network, model overload) shows nothing
    # diagnostic on its own. Dump the tail of the raw stream-json so the
    # actual error is visible before the EXIT trap removes $RUN_FILE.
    log_err "last 20 stream events:"
    tail -20 "$RUN_FILE" >&2 || true
    exit 2
fi

# ── Parse ─────────────────────────────────────────────────────────────────────
# TOOL_CALLS and ANSWER feed the assertion logic below. We don't echo them
# back to the user — the live `→` and `✓` stream lines already show every
# tool call and the final answer.
TOOL_CALLS=$(jq -r 'select(.type=="assistant") | .message.content[]? | select(.type=="tool_use") | .name' "$RUN_FILE" | sort -u)
ANSWER=$(jq -r 'select(.type=="result") | .result' "$RUN_FILE")

# Append this scenario's cost to the wrapper's tally file, if the wrapper
# set one. Single source of truth for cost is the result event itself.
if [ -n "${SCENARIO_COST_FILE:-}" ]; then
    jq -r 'select(.type=="result") | .total_cost_usd' "$RUN_FILE" >> "$SCENARIO_COST_FILE" 2>/dev/null || true
fi

PASS=1
fail() { log_fail "$*"; PASS=0; }

# ── Tool-choice assertions ────────────────────────────────────────────────────
if [ -n "$EXPECTED_TOOL" ]; then
    grep -Fqx "$EXPECTED_TOOL" <<<"$TOOL_CALLS" \
        || fail "expected tool '$EXPECTED_TOOL' was not called"
fi

if [ -n "$EXPECTED_ANY" ]; then
    MATCHED=""
    while IFS= read -r t; do
        [ -z "$t" ] && continue
        if grep -Fqx "$t" <<<"$TOOL_CALLS"; then MATCHED="$t"; break; fi
    done <<<"$EXPECTED_ANY"
    [ -z "$MATCHED" ] && fail "no tool from expected_tool_any_of was called: $(echo "$EXPECTED_ANY" | tr '\n' ' ')"
fi

if [ -n "$EXPECTED_ALL" ]; then
    while IFS= read -r t; do
        [ -z "$t" ] && continue
        grep -Fqx "$t" <<<"$TOOL_CALLS" || fail "required tool '$t' was not called"
    done <<<"$EXPECTED_ALL"
fi

if [ "$EXPECTED_NONE" = "true" ] && [ -n "$TOOL_CALLS" ]; then
    fail "expected zero tool calls but agent called: $(echo "$TOOL_CALLS" | tr '\n' ' ')"
fi

# ── Substring assertions ──────────────────────────────────────────────────────
while IFS= read -r needle; do
    [ -z "$needle" ] && continue
    grep -qi -- "$needle" <<<"$ANSWER" || fail "required substring '$needle' missing"
done <<<"$REQUIRED"

if [ -n "$REQUIRED_ANY" ]; then
    MATCHED_ANY=""
    while IFS= read -r needle; do
        [ -z "$needle" ] && continue
        if grep -qi -- "$needle" <<<"$ANSWER"; then MATCHED_ANY="$needle"; break; fi
    done <<<"$REQUIRED_ANY"
    [ -z "$MATCHED_ANY" ] && fail "no substring from required_substrings_any_of present: $(echo "$REQUIRED_ANY" | tr '\n' ' ')"
fi

while IFS= read -r needle; do
    [ -z "$needle" ] && continue
    if grep -qi -- "$needle" <<<"$ANSWER"; then
        fail "forbidden substring '$needle' present"
        # Intentional: any single forbidden hit is a hard fail, so stop
        # at the first one rather than spamming N fail lines for the
        # same logical violation.
        break
    fi
done <<<"$FORBIDDEN"

# ── Numeric assertion ─────────────────────────────────────────────────────────
if [ -n "$NUM_REGEX" ]; then
    # Strip thousands-separator commas BEFORE the inner number regex — without
    # this, "1,311 msg/sec" splits into "1" and "311" and awk gets two args.
    NUM=$(grep -oE "$NUM_REGEX" <<<"$ANSWER" | head -1 | tr -d ',' | grep -oE '[0-9]+(\.[0-9]+)?' | head -1 || true)
    if [ -z "$NUM" ]; then
        fail "numeric_match.regex '$NUM_REGEX' found no number in answer"
    else
        if [ -n "$NUM_MIN" ] && awk "BEGIN{exit !($NUM < $NUM_MIN)}"; then
            fail "extracted number $NUM below min $NUM_MIN"
        fi
        if [ -n "$NUM_MAX" ] && awk "BEGIN{exit !($NUM > $NUM_MAX)}"; then
            fail "extracted number $NUM above max $NUM_MAX"
        fi
        log_info "numeric:         $NUM (in [$NUM_MIN, $NUM_MAX])"
    fi
fi

# ── Entity-set assertion (ground-truth diff) ──────────────────────────────────
if [ -n "$GT_JQ" ] && [ -n "$GT_REGEX" ]; then
    # All tool_use_ids whose name matches any expected_* declaration. If only
    # expected_tool is set we filter on that; otherwise use whatever was called.
    EXPECTED_IDS=$(jq -r --arg t "$EXPECTED_TOOL" '
        select(.type=="assistant") | .message.content[]?
        | select(.type=="tool_use")
        | select($t == "" or .name == $t) | .id
    ' "$RUN_FILE")

    EXPECTED_SET=$(
        while IFS= read -r id; do
            [ -z "$id" ] && continue
            jq -r --arg id "$id" '
                select(.type=="user") | .message.content[]?
                | select(.type=="tool_result" and .tool_use_id==$id)
                | .content | (try fromjson catch empty)
            ' "$RUN_FILE" | jq -r "$GT_JQ" 2>/dev/null
        done <<<"$EXPECTED_IDS" | sort -u
    )

    ANSWER_SET=$(grep -oE "$GT_REGEX" <<<"$ANSWER" | sort -u || true)

    log_info "expected set:    $(echo "$EXPECTED_SET" | tr '\n' ' ')"
    log_info "answer set:      $(echo "$ANSWER_SET" | tr '\n' ' ')"

    MISSING=$(comm -23 <(echo "$EXPECTED_SET") <(echo "$ANSWER_SET"))
    EXTRA=$(comm -13 <(echo "$EXPECTED_SET") <(echo "$ANSWER_SET"))
    [ -n "$MISSING" ] && fail "answer omitted entities: $(echo "$MISSING" | tr '\n' ' ')"
    [ -n "$EXTRA" ]   && fail "answer fabricated entities: $(echo "$EXTRA" | tr '\n' ' ')"
fi

if [ "$PASS" -eq 1 ]; then
    log_ok "$SCENARIO_NAME"
    exit 0
fi
exit 1
