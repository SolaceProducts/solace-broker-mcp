#!/usr/bin/env bash
# LLM e2e eval suite: single-scenario runner.
#
# Drives one NL prompt through Claude Code CLI and runs assertions against
# the resulting stream-json output. The scenario file declares both the
# prompt and the assertions, so this script is generic across scenarios.
#
# Usage:   ./run-scenario.sh scenarios/<name>.json
# Prereqs: brokers + MCP server up; `claude`, `jq`, `envsubst`, `uuidgen`
#          on PATH.
# Exit:    0 pass, 1 assertion failed, 2 setup/invocation error.
#
# Scenario fields (all optional except `prompt`):
#   prompt                    NL question sent to Claude (turn 1)
#   brokers                   array of brokers run-all.sh iterates this scenario
#                             over (default: ["broker-a"]). Single-scenario runs
#                             here ignore this field — use $BROKER env var.
#   mcp_config                path to MCP config JSON (default: mcp-config.json)
#   setup.cmd                 bash string run before turn 1; non-zero exit
#                             fails the scenario with rc 2 (test-infra error,
#                             not an assertion failure). Runs in a `bash -c`
#                             child with fixture functions (semp_curl,
#                             refill_e2e_llm_action_queue, etc.) exported via
#                             `export -f` visible.
#   teardown.cmd              bash string run in the EXIT trap (always runs,
#                             even on assertion failure). Same environment
#                             as setup.cmd. Use to restore fixture state.
#   expected_tool             single tool name that MUST be called
#   expected_tool_any_of      array; at least one MUST be called
#   expected_tools_all_of     array; all MUST be called
#   expected_no_mutating_tools  true → assert no tool call matches
#                             $MUTATING_TOOL_REGEX (config.env). Read-only MCP
#                             calls and the CLI's own ToolSearch are permitted.
#                             This is the assertion a confirmation-gated turn 1
#                             wants: "changed nothing", not "called nothing".
#                             Replaces `expected_tools_none`, which is retired —
#                             a scenario still carrying that key hard-fails.
#   ground_truth.jq           jq path applied to the tool_result to extract the
#                             must-appear entity set (every name it emits MUST
#                             be named in the answer).
#   ground_truth.answer_regex regex applied to the answer to extract entity names.
#                             Each match MUST appear as a substring of the raw
#                             tool_result content (else it is a fabrication).
#                             Fabrication check runs against the raw tool_result,
#                             NOT against the ground_truth.jq set — so a name
#                             the model uses for context (an adjacent seeded
#                             entity the tool returned) is not flagged.
#   ground_truth.shell        bash pipeline; usually `semp_curl … | jq …`
#   ground_truth.expect_stdout_regex
#                             stdout of ground_truth.shell MUST match this regex.
#                             ground_truth.{jq,answer_regex} and
#                             ground_truth.{shell,expect_stdout_regex} are
#                             mutually exclusive per scope.
#   required_substrings       each MUST appear in the answer (case-insensitive)
#   required_substrings_any_of  at least ONE MUST appear (case-insensitive) —
#                             use when paraphrases are equally valid (e.g.
#                             healthy / operational / up)
#   forbidden_substrings      each MUST NOT appear in the answer
#   numeric_match.regex       extracts a number from the answer
#   numeric_match.min/max     extracted number MUST fall in [min, max]
#   followup                  optional second turn. Object with the same
#                             assertion field names as the top level, plus a
#                             required `prompt`. Turn 1 uses `--session-id
#                             <uuid>`; turn 2 uses `--resume <uuid>` so the
#                             agent sees turn-1 context.
#
# Env-var substitution: ./helpers.sh is auto-sourced (if present), which in
# turn pulls in the monitoring suite's F1–F7 helpers and this suite's
# fixtures.sh — so scenarios can reference $F3_CLIENT_NAME_A,
# $E2E_LLM_ACTION_QUEUE, etc. as literals.

set -euo pipefail

RUNNER_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HELPERS="$RUNNER_DIR/helpers.sh"

# Suite-wide config. Anything exported beforehand wins (each value is
# `${X:-default}` in config.env). The runner has no hardcoded MCP URL,
# bearer token, or CLI version pin — they all live there.
# shellcheck disable=SC1091
source "$RUNNER_DIR/config.env"

DEFAULT_MCP_CONFIG="$RUNNER_DIR/mcp-config.json.tmpl"

# Stream-json events go straight to the user's terminal AND per-turn temp
# files we parse for assertions. Rendered MCP config (from a .tmpl) lives
# next to them, also temp. Trap-cleanup deletes them unconditionally — no
# persistent transcripts — and also runs teardown.cmd, if the scenario
# declared one, before deleting the run files.
RUN_FILE=$(mktemp -t llm-scenario.XXXXXXXX.jsonl)
RUN_FILE2=$(mktemp -t llm-scenario.XXXXXXXX.jsonl)
RENDERED_MCP_CONFIG=""
TEARDOWN_CMD=""
cleanup() {
    # Teardown FIRST — its whole job is to restore fixture state (re-prime a
    # drained queue, drop a created queue, etc.). Running before rm keeps
    # $RUN_FILE around in case teardown wants to inspect it (nothing does
    # today, but the ordering is the safer default).
    if [ -n "$TEARDOWN_CMD" ]; then
        bash -c "set -euo pipefail; $TEARDOWN_CMD" >&2 || log_warn "teardown.cmd exited non-zero (fixture may need manual reset)"
    fi
    rm -f "$RUN_FILE" "$RUN_FILE2"
    [ -n "$RENDERED_MCP_CONFIG" ] && rm -f "$RENDERED_MCP_CONFIG"
}
trap cleanup EXIT INT TERM HUP

# Source helpers.sh for shared colours (RED/GREEN/YELLOW/CYAN/NC), the
# log_info/log_ok/log_fail family, and fixture name vars (F1_*,
# F3_CLIENT_NAME_A, F5_QUEUE, …) that envsubst needs to resolve $-refs in
# scenario JSON. LLM helpers.sh transitively sources the monitoring suite's
# F1–F7 helpers (with SUITE_DIR pinned to this suite so ports/.env resolve
# here, not in monitoring/) and this suite's fixtures.sh — but we re-source
# fixtures.sh explicitly so a future refactor of helpers.sh can't silently
# drop the E2E_LLM_ACTION_QUEUE_A/B / E2E_LLM_KICK_TARGET_A/B alias inputs.
# The `set -a` wrapping exports the fixture names. Fallback defs cover the
# case where helpers.sh isn't on disk (e.g. CI image without the e2e-llm
# tree) so the version-pin check below can still log.
if [ -f "$HELPERS" ]; then
    set -a
    # shellcheck disable=SC1090
    source "$HELPERS"
    # shellcheck disable=SC1091
    source "$RUNNER_DIR/fixtures.sh"
    set +a
else
    RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; CYAN='\033[0;36m'; NC='\033[0m'
    log_info() { echo -e "${CYAN}[INFO]${NC}  $*" >&2; }
    log_ok()   { echo -e "${GREEN}[PASS]${NC}  $*" >&2; }
    log_fail() { echo -e "${RED}[FAIL]${NC}  $*" >&2; }
    log_warn() { echo -e "${YELLOW}[WARN]${NC}  $*" >&2; }
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
    for v in F3_CLIENT_NAME F6_SUB_CLIENT_NAME E2E_LLM_ACTION_QUEUE E2E_LLM_KICK_TARGET E2E_LLM_STANDING_TE; do
        src="${v}_${BROKER_SUFFIX}"
        export "$v"="${!src:-}"
    done
    # Point BROKER_URL / SEMP_CONFIG at the current broker so Mode-2 scenarios'
    # setup.cmd / teardown.cmd / ground_truth.shell strings (which reach for
    # $BROKER_URL/SEMP/v2/__private_monitor__/… via semp_curl) hit the right
    # broker on both broker-a and broker-b runs. lib.sh defaults BROKER_URL to
    # BROKER_A_URL; without this alias, broker-b scenarios would silently
    # verify against broker-a state.
    _url_src="BROKER_${BROKER_SUFFIX}_URL"
    _semp_src="BROKER_${BROKER_SUFFIX}_SEMP_CONFIG"
    export BROKER_URL="${!_url_src:-${BROKER_URL:-}}"
    export SEMP_CONFIG="${!_semp_src:-${SEMP_CONFIG:-}}"
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
    log_hint "source test/e2e-llm/helpers.sh, or run after ./setup-fixtures.sh"
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

SCENARIO_NAME="$(basename "$SCENARIO_FILE" .json) [$BROKER]"

# ── Setup / teardown hooks ────────────────────────────────────────────────────
# setup.cmd runs BEFORE turn 1 and its non-zero exit kills the scenario with
# rc 2 (test infra error, not an assertion failure). teardown.cmd runs in the
# EXIT trap regardless of outcome — its whole job is to restore fixture state
# so the next run finds things where it expects them. Both `bash -c` children
# inherit exported functions from fixtures.sh (semp_curl,
# refill_e2e_llm_action_queue, delete_queue_on_current_broker) plus the
# per-broker BROKER_URL / SEMP_CONFIG / *_QUEUE / *_TARGET aliases set above.
TEARDOWN_CMD=$(jq -r '.teardown.cmd // empty' <<<"$EXPANDED")
SETUP_CMD=$(jq -r '.setup.cmd // empty' <<<"$EXPANDED")
if [ -n "$SETUP_CMD" ]; then
    bash -c "set -euo pipefail; $SETUP_CMD" >&2 || { log_err "setup.cmd failed — aborting scenario"; exit 2; }
fi

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

# `tee` keeps the full raw JSONL in a per-turn run file so jq can parse it
# for assertions; the compact formatter below summarizes each event into
# one short line for the user (raw stream-json is unreadable at suite scale).
#
# The model field joins every key of `modelUsage`, not just the first. A turn
# can legitimately bill more than one model — a ToolSearch schema lookup runs
# its retrieval on a small fast model while the main model does the reasoning —
# and this line used to print `keys[0]`, which jq sorts, so such a turn reported
# "claude-haiku-4-5-…" no matter which model actually answered. That is a
# misreport in exactly the field the daily run's artifacts exist to record.
# Sorted order is kept so the string is stable across runs and diffable.
JQ_PRETTY='
def info(msg): "[0;36m[INFO][0m  " + msg;
if .type == "assistant" then
  .message.content[] | (
    if .type == "tool_use" then
      info("tool call: \(.name | sub("mcp__solace-broker__"; "")) \(.input | tostring)")
    else empty end
  )
elif .type == "result" then
  info("answer: " + ((.result // "(empty)") | gsub("\n"; " ") | gsub(" +"; " ")
        # 1200, not 300. A truncated answer is unreadable exactly when it
        # matters: an entity-set or substring failure is a claim about the
        # answer text, and diagnosing one from the first 300 chars is
        # guesswork. Two rounds of failures in this suite were misdiagnosed
        # that way. Still capped, so a runaway answer cannot flood the log.
        | if length > 1200 then .[:1200] + "…" else . end)
    + "  ($\((.total_cost_usd * 10000 + 0.5 | floor) / 10000), \(.modelUsage // {} | keys | join("+") | if . == "" then "?" else . end))")
else empty end
'

# invoke_claude <run_file> <prompt> [<extra claude args...>]
# Streams claude's stream-json output through JQ_PRETTY for user-facing
# summary lines AND tees the raw JSONL into <run_file> for downstream
# assertions. Appends the turn's total_cost_usd to $SCENARIO_COST_FILE so
# run-all.sh's totals include multi-turn scenarios' followup cost.
# Non-zero claude exit dumps the last 20 events (the pretty filter only
# surfaces tool_use/result, so an auth/overload failure would otherwise
# show nothing diagnostic). Returns 2 on claude failure, 0 otherwise.
invoke_claude() {
    local run_file="$1" prompt="$2"; shift 2
    local claude_args=(
        --mcp-config "$MCP_CONFIG"
        --strict-mcp-config
        # `--allowed-tools` IS load-bearing in --print mode — it's the
        # auto-approve list. Without it, every MCP tool call gets denied with
        # "I need permission to run X — please approve and I'll retry." Wildcard
        # auto-approves every tool from our MCP server (the only one loaded,
        # thanks to --strict-mcp-config), so the list stays maintenance-free
        # as the server adds tools. Assertion logic catches wrong tool choices.
        --allowed-tools "mcp__solace-broker__*"
        # `--tools` scopes the BUILT-IN set only; MCP tools are unaffected.
        # Naming ToolSearch alone yields an allow-list of one: the agent keeps
        # MCP discovery and loses Bash, Read, Write, Edit, WebFetch and
        # WebSearch. An allow-list is deliberate over `--disallowed-tools`,
        # which would go quietly permissive each time the CLI ships a new
        # built-in. ToolSearch stays because the CLI defers tool schemas once
        # enough tools are registered, and the agent cannot name an MCP tool
        # without looking it up first; it only loads schemas, so it reaches
        # neither the broker nor the filesystem.
        #
        # Do NOT rely on --allowed-tools for this. It is the auto-approve list,
        # not a registry filter: built-ins absent from it were still handed to
        # the agent AND still ran, with an empty `permission_denials`. Measured
        # on 2.1.224 via the init event's tool registry — 72 tools with
        # --allowed-tools alone (Bash, Write and Edit among them), 41 with
        # `--tools "ToolSearch"`, MCP's 40 intact in both. Between SOL-152862
        # and SOL-153285 the suite ran unconfined on that false premise, and D1
        # spent its turns shelling out to reach the broker another way.
        #
        # `--tools ""` also works now and is one tool tighter. It is avoided
        # because it is the flag SOL-152862 removed: CLI 2.1.181 read it as
        # "disable ALL tools including MCP" and every scenario answered "I
        # don't have the Solace MCP tools available". That is fixed on 2.1.224,
        # but dropping ToolSearch would break discovery again the moment
        # deferral changes, and this suite has broken on a CLI bump twice.
        --tools "ToolSearch"
        --output-format stream-json
        --verbose
        --max-turns 5
        "${CLAUDE_MODEL_ARGS[@]}"
        "$@"
        -p "$prompt"
    )
    set +e
    claude "${claude_args[@]}" | tee "$run_file" | jq -r --unbuffered "$JQ_PRETTY"
    local rc=${PIPESTATUS[0]}
    set -e
    # Bank the cost before the failure check. A non-zero exit does not mean
    # nothing was spent — max-turns, overload and auth failures all bill for
    # the turns they got through, and those are exactly the runs whose spend
    # you want in the total. The jq selects on the result event, so a run that
    # died before emitting one contributes nothing.
    if [ -n "${SCENARIO_COST_FILE:-}" ]; then
        jq -r 'select(.type=="result") | .total_cost_usd' "$run_file" >> "$SCENARIO_COST_FILE" 2>/dev/null || true
    fi
    if [ "$rc" -ne 0 ]; then
        log_err "claude exited $rc"
        log_err "last 20 stream events:"
        tail -20 "$run_file" >&2 || true
        return 2
    fi
    return 0
}

PASS=1
fail() { log_fail "$*"; PASS=0; }

# run_assertions <run_file> <scoped_json> <turn_label>
# Applies all assertion fields (tool-choice / substring / numeric /
# ground_truth.{jq,answer_regex} / ground_truth.{shell,expect_stdout_regex})
# against the scoped_json blob. Same field names work at scenario top level
# and inside `followup`, so this function serves both turns.
run_assertions() {
    local run_file="$1" scoped="$2" label="$3"

    local expected_tool expected_any expected_all expected_no_mutating removed_none
    local required required_any forbidden
    local num_regex num_min num_max
    local gt_jq gt_regex gt_shell gt_expect

    expected_tool=$(jq -r '.expected_tool // empty' <<<"$scoped")
    expected_any=$(jq -r '.expected_tool_any_of[]? // empty' <<<"$scoped")
    expected_all=$(jq -r '.expected_tools_all_of[]? // empty' <<<"$scoped")
    expected_no_mutating=$(jq -r '.expected_no_mutating_tools // false' <<<"$scoped")
    removed_none=$(jq -r 'has("expected_tools_none")' <<<"$scoped")
    required=$(jq -r '.required_substrings[]? // empty' <<<"$scoped")
    required_any=$(jq -r '.required_substrings_any_of[]? // empty' <<<"$scoped")
    forbidden=$(jq -r '.forbidden_substrings[]? // empty' <<<"$scoped")
    num_regex=$(jq -r '.numeric_match.regex // empty' <<<"$scoped")
    num_min=$(jq -r '.numeric_match.min // empty' <<<"$scoped")
    num_max=$(jq -r '.numeric_match.max // empty' <<<"$scoped")
    gt_jq=$(jq -r '.ground_truth.jq // empty' <<<"$scoped")
    gt_regex=$(jq -r '.ground_truth.answer_regex // empty' <<<"$scoped")
    gt_shell=$(jq -r '.ground_truth.shell // empty' <<<"$scoped")
    gt_expect=$(jq -r '.ground_truth.expect_stdout_regex // empty' <<<"$scoped")

    # ground_truth has two flavors; they are mutually exclusive per scope.
    # jq+answer_regex diffs an entity set derived from a tool_result against
    # one derived from the answer text (Mode-1 read-tool coverage).
    # shell+expect_stdout_regex runs an out-of-band SEMPv2 check to verify
    # that the broker really is in the state the answer implies (Mode-2
    # write-tool coverage). Allowing both would be ambiguous and mask misuse.
    if [ -n "$gt_jq$gt_regex" ] && [ -n "$gt_shell$gt_expect" ]; then
        fail "$label: ground_truth.jq/answer_regex and ground_truth.shell/expect_stdout_regex are mutually exclusive"
        return
    fi
    # Fail fast on a partially-specified pair — otherwise the check below skips
    # silently and the scenario passes on other assertions, hiding the misconfig.
    if { [ -n "$gt_jq" ] && [ -z "$gt_regex" ]; } || { [ -z "$gt_jq" ] && [ -n "$gt_regex" ]; }; then
        fail "$label: ground_truth.jq and ground_truth.answer_regex must both be set"
        return
    fi
    if { [ -n "$gt_shell" ] && [ -z "$gt_expect" ]; } || { [ -z "$gt_shell" ] && [ -n "$gt_expect" ]; }; then
        fail "$label: ground_truth.shell and ground_truth.expect_stdout_regex must both be set"
        return
    fi

    local tool_calls answer
    tool_calls=$(jq -r 'select(.type=="assistant") | .message.content[]? | select(.type=="tool_use") | .name' "$run_file" | sort -u)
    answer=$(jq -r 'select(.type=="result") | .result' "$run_file")

    # ── Tool-choice ──
    if [ -n "$expected_tool" ]; then
        grep -Fqx "$expected_tool" <<<"$tool_calls" \
            || fail "$label: expected tool '$expected_tool' was not called"
    fi
    if [ -n "$expected_any" ]; then
        local matched=""
        while IFS= read -r t; do
            [ -z "$t" ] && continue
            if grep -Fqx "$t" <<<"$tool_calls"; then matched="$t"; break; fi
        done <<<"$expected_any"
        [ -z "$matched" ] && fail "$label: no tool from expected_tool_any_of was called: $(echo "$expected_any" | tr '\n' ' ')"
    fi
    if [ -n "$expected_all" ]; then
        while IFS= read -r t; do
            [ -z "$t" ] && continue
            grep -Fqx "$t" <<<"$tool_calls" || fail "$label: required tool '$t' was not called"
        done <<<"$expected_all"
    fi
    # The invariant a gated turn 1 actually has to hold is "changed nothing",
    # not "called nothing". The predecessor assertion (`expected_tools_none`)
    # conflated the two, so it failed on a read-only get-queue-metrics that
    # makes the confirmation *better* — "100 messages (25,600 bytes) will be
    # deleted" beats a bare "are you sure?" — and on the ToolSearch schema
    # lookup the CLI needs before it can name an MCP tool at all, which no
    # scenario can avoid once the server's tool count crosses the CLI's
    # deferral threshold. It was also weaker where it counted: it would have
    # passed a turn-1 `delete-queue` no more loudly than a `list-queues`,
    # because it only ever checked whether the list was empty.
    if [ "$expected_no_mutating" = "true" ]; then
        local mutating
        mutating=$(grep -E "$MUTATING_TOOL_REGEX" <<<"$tool_calls" || true)
        if [ -n "$mutating" ]; then
            fail "$label: expected no state-changing tool call but agent called: $(echo "$mutating" | tr '\n' ' ')"
        fi
    fi

    # Fail loud on the removed key. A scenario still carrying
    # `expected_tools_none` would otherwise assert nothing at all and pass
    # vacuously — silently retiring a safety assertion is the worst outcome
    # available here.
    if [ "$removed_none" = "true" ]; then
        fail "$label: 'expected_tools_none' was replaced by 'expected_no_mutating_tools' — update this scenario"
    fi

    # ── Substrings ──
    while IFS= read -r needle; do
        [ -z "$needle" ] && continue
        grep -qi -- "$needle" <<<"$answer" || fail "$label: required substring '$needle' missing"
    done <<<"$required"

    if [ -n "$required_any" ]; then
        local matched_any=""
        while IFS= read -r needle; do
            [ -z "$needle" ] && continue
            if grep -qi -- "$needle" <<<"$answer"; then matched_any="$needle"; break; fi
        done <<<"$required_any"
        [ -z "$matched_any" ] && fail "$label: no substring from required_substrings_any_of present: $(echo "$required_any" | tr '\n' ' ')"
    fi

    while IFS= read -r needle; do
        [ -z "$needle" ] && continue
        if grep -qi -- "$needle" <<<"$answer"; then
            # Intentional: any single forbidden hit is a hard fail, so stop
            # at the first one rather than spamming N fail lines for the
            # same logical violation.
            fail "$label: forbidden substring '$needle' present"
            break
        fi
    done <<<"$forbidden"

    # ── Numeric ──
    if [ -n "$num_regex" ]; then
        # Strip thousands-separator commas BEFORE the inner number regex —
        # without this, "1,311 msg/sec" splits into "1" and "311" and awk
        # gets two args.
        local num
        num=$(grep -oE "$num_regex" <<<"$answer" | head -1 | tr -d ',' | grep -oE '[0-9]+(\.[0-9]+)?' | head -1 || true)
        if [ -z "$num" ]; then
            fail "$label: numeric_match.regex '$num_regex' found no number in answer"
        else
            if [ -n "$num_min" ] && awk "BEGIN{exit !($num < $num_min)}"; then
                fail "$label: extracted number $num below min $num_min"
            fi
            if [ -n "$num_max" ] && awk "BEGIN{exit !($num > $num_max)}"; then
                fail "$label: extracted number $num above max $num_max"
            fi
            log_info "$label numeric: $num (in [$num_min, $num_max])"
        fi
    fi

    # ── ground_truth.jq / answer_regex (entity-set diff) ──
    #
    # Two-directional check with two different universes:
    #   missing  = expected_set (from gt_jq) \ answer_set  — answer must
    #              name every entity gt_jq extracted from the tool_result.
    #   fabricated = answer_set entries not present anywhere in the raw
    #                tool_result content — answer must not invent entities
    #                the broker never returned.
    #
    # The fabrication universe is the set of tokens extracted from the RAW
    # tool_result content by the same gt_regex used on the answer, NOT the
    # narrow expected_set. gt_jq is typically a narrow projection (e.g. only
    # discarding queues, only unhealthy RDPs); an entity the model names for
    # legitimate context (an adjacent test-queue-*, a healthy sibling RDP)
    # exists on the broker and was returned by the tool, but is not in
    # expected_set — diffing extras against expected_set flagged those as
    # fabrications. Diffing against gt_regex-extracted tokens from the raw
    # tool_result correctly doesn't. Whole-token match (not substring): a
    # fabricated `test-queue-discards` must not pass because it is a prefix
    # of the real `test-queue-discards-spool`, and a fabricated `test-rdp`
    # must not pass because it is a prefix of `test-rdp-failing`.
    #
    # Applies uniformly to every scenario with ground_truth.jq set — today:
    # f1-list-vpns, f3-subscriptions, read-list-brokers, read-list-rdps,
    # read-list-queue-discards. Each supplies its own answer_regex, so the
    # fabrication universe is scenario-scoped.
    if [ -n "$gt_jq" ] && [ -n "$gt_regex" ]; then
        local expected_ids expected_set answer_set haystack haystack_set missing extra
        expected_ids=$(jq -r --arg t "$expected_tool" '
            select(.type=="assistant") | .message.content[]?
            | select(.type=="tool_use")
            | select($t == "" or .name == $t) | .id
        ' "$run_file")
        # Raw tool_result contents (concatenated) — gt_regex-extracted below
        # to form the fabrication universe. gt_jq is applied per-ID against
        # each raw tool_result to produce the must-appear expected_set (a
        # concatenated haystack is not valid JSON, so per-ID here).
        haystack=""
        expected_set=""
        while IFS= read -r id; do
            [ -z "$id" ] && continue
            local raw
            raw=$(jq -r --arg id "$id" '
                select(.type=="user") | .message.content[]?
                | select(.type=="tool_result" and .tool_use_id==$id)
                | .content
            ' "$run_file")
            haystack+="$raw"$'\n'
            expected_set+=$(jq -r "$gt_jq" <<<"$raw" 2>/dev/null || true)$'\n'
        done <<<"$expected_ids"
        expected_set=$(echo "$expected_set" | grep -v '^$' | sort -u || true)
        answer_set=$(grep -oE "$gt_regex" <<<"$answer" | sort -u || true)
        # Fabrication universe: same regex applied to the raw tool_result,
        # producing a whole-token set. Prefix-of-real names (e.g. answer
        # says `test-queue-discards`, real is `test-queue-discards-spool`)
        # are correctly flagged; a substring haystack check wouldn't.
        haystack_set=$(grep -oE "$gt_regex" <<<"$haystack" | sort -u || true)
        log_info "$label expected set:  $(echo "$expected_set"  | tr '\n' ' ')"
        log_info "$label answer set:    $(echo "$answer_set"    | tr '\n' ' ')"
        log_info "$label haystack set:  $(echo "$haystack_set"  | tr '\n' ' ')"
        missing=$(comm -23 <(echo "$expected_set") <(echo "$answer_set"))
        [ -n "$missing" ] && fail "$label: answer omitted entities: $(echo "$missing" | tr '\n' ' ')"
        extra=$(comm -23 <(echo "$answer_set") <(echo "$haystack_set"))
        [ -n "$extra" ] && fail "$label: answer fabricated entities: $(echo "$extra" | tr '\n' ' ') (present in tool_result: $(echo "$haystack_set" | tr '\n' ' '))"
    fi

    # ── ground_truth.shell / expect_stdout_regex (broker-state verification) ──
    # Run the shell string in a `bash -c` child (inherits BROKER_URL,
    # semp_curl, BROKER_USER/PASS/VPN via export / export -f) and match
    # stdout. Used by Mode-2 scenarios to independently verify that the
    # answer's claim ("queue drained", "vpn still there", "queue created")
    # matches the broker's actual state — otherwise a confidently-worded
    # confabulation would pass every text-level assertion.
    if [ -n "$gt_shell" ] && [ -n "$gt_expect" ]; then
        local gt_out gt_rc
        set +e
        gt_out=$(bash -c "set -euo pipefail; $gt_shell" 2>&1)
        gt_rc=$?
        set -e
        log_info "$label ground_truth.shell → $(echo "$gt_out" | tr '\n' ' ' | head -c 120)"
        if [ "$gt_rc" -ne 0 ]; then
            fail "$label: ground_truth.shell exited $gt_rc (stdout: $gt_out)"
        elif ! grep -Eq -- "$gt_expect" <<<"$gt_out"; then
            fail "$label: ground_truth stdout did not match /$gt_expect/ (got: $gt_out)"
        fi
    fi
}

# ── Turn 1 ────────────────────────────────────────────────────────────────────
# --session-id pins the conversation id up front so a followup can resume
# it deterministically. `--continue` would work for a single-scenario shell
# but breaks the moment two scenarios run in parallel (each would resume
# whichever "most recent" happened to land last). uuidgen is standard on
# every distro we support; /proc/sys/kernel/random/uuid is the Linux
# fallback if uuidgen isn't installed.
SESSION_ID=$(uuidgen 2>/dev/null || cat /proc/sys/kernel/random/uuid)
invoke_claude "$RUN_FILE" "$PROMPT" --session-id "$SESSION_ID" || exit 2

run_assertions "$RUN_FILE" "$EXPANDED" "turn-1"

# ── Turn 2 (optional followup) ────────────────────────────────────────────────
FOLLOWUP=$(jq -c '.followup // empty' <<<"$EXPANDED")
if [ -n "$FOLLOWUP" ]; then
    FOLLOWUP_PROMPT=$(jq -r '.prompt // empty' <<<"$FOLLOWUP")
    if [ -z "$FOLLOWUP_PROMPT" ]; then
        log_err "followup is set but followup.prompt is empty"
        exit 2
    fi
    log_info "followup prompt: $FOLLOWUP_PROMPT"
    invoke_claude "$RUN_FILE2" "$FOLLOWUP_PROMPT" --resume "$SESSION_ID" || exit 2
    run_assertions "$RUN_FILE2" "$FOLLOWUP" "turn-2"
fi

if [ "$PASS" -eq 1 ]; then
    log_ok "$SCENARIO_NAME"
    exit 0
fi
exit 1
