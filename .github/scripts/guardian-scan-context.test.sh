#!/usr/bin/env bash
#
# Self-test for guardian-scan-context.sh. Runs the real script under each event
# shape and asserts the full output tuple, so the pr/queue/trunk classification
# is verified on every run rather than trusted — same pattern as the other
# gates. The dangerous regression this pins: a queue entry falling through to
# trunk, which would upload scan results, run the Guardian DB sync + Jira
# report, and block on Guardian's verdict for a throwaway ref (SOL-152974).
#
# Run manually:  .github/scripts/guardian-scan-context.test.sh
# Runs in CI as a step of the `setup` job in .github/workflows/guardian-scan.yaml.
#
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
RESOLVE="${SCRIPT_DIR}/guardian-scan-context.sh"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

pass_count=0
fail_count=0
pass() { pass_count=$((pass_count + 1)); printf 'ok   - %s\n' "$1"; }
fail() { fail_count=$((fail_count + 1)); printf 'FAIL - %s\n' "$1"; [ -n "${2:-}" ] && printf '       %s\n' "$2"; return 0; }

# run — execute the script with the env given as KEY=VALUE args; set globals
# STDOUT and OUT (path to the captured $GITHUB_OUTPUT file). The script must
# exit 0 on every shape tested here; a crash is a failure in its own right,
# not just empty outputs downstream.
run() {
  OUT="$WORK/output.$RANDOM"
  : > "$OUT"
  local rc=0
  STDOUT=$(env -i PATH="$PATH" HOME="${HOME:-}" GITHUB_OUTPUT="$OUT" "$@" bash "$RESOLVE" 2>&1) || rc=$?
  if [ "$rc" -ne 0 ]; then fail "script exited $rc" "env: $* / stdout=[$STDOUT]"; fi
  return 0
}

# get <key> — value of <key> from the captured output file.
get() { sed -n "s/^$1=//p" "$OUT" | tail -n 1; }

# expect <case> <key> <want> — assert one key of the tuple.
expect() {
  local case="$1" key="$2" want="$3" got
  got=$(get "$key")
  if [ "$got" = "$want" ]; then pass "$case: $key=$want"; else fail "$case: $key" "want [$want] got [$got]"; fi
}

# --- Case 1: pull_request -> pr mode, nothing enforced ------------------------
run EVENT_NAME=pull_request HEAD_REF=feature/x FULL_VERSION_INPUT=v1.2.3 \
    GITHUB_REF=refs/pull/7/merge IS_FORK=false FAIL_ON_BLOCKED=""
expect "pr" mode pr
expect "pr" is_pr true
expect "pr" is_trunk false
expect "pr" is_queue false
expect "pr" enforce false
expect "pr" fossa_mode REPORT
expect "pr" fossa_branch PR
expect "pr" fossa_revision feature/x

# --- Case 2: merge_group -> queue mode, NOT trunk ------------------------------
run EVENT_NAME=merge_group FULL_VERSION_INPUT=v1.2.3-4-gabc \
    GITHUB_REF=refs/heads/gh-readonly-queue/main/pr-9 IS_FORK=false FAIL_ON_BLOCKED=""
expect "queue" mode queue
expect "queue" is_pr false
expect "queue" is_trunk false
expect "queue" is_queue true
expect "queue" enforce false
expect "queue" fossa_mode REPORT
expect "queue" fossa_branch merge-queue
expect "queue" fossa_revision v1.2.3-4-gabc
if grep -q 'Resolved: mode=queue' <<<"$STDOUT"; then
  pass "queue: resolved mode is logged to stdout"
else
  fail "queue: resolved mode is logged to stdout" "stdout=[$STDOUT]"
fi

# --- Case 3: push -> trunk mode, enforcing -------------------------------------
run EVENT_NAME=push FULL_VERSION_INPUT=v1.2.3 \
    GITHUB_REF=refs/heads/main IS_FORK=false FAIL_ON_BLOCKED=""
expect "push" mode trunk
expect "push" is_trunk true
expect "push" is_queue false
expect "push" enforce true
expect "push" fossa_mode BLOCK
expect "push" fossa_branch main

# --- Case 4: workflow_dispatch falls through to trunk --------------------------
run EVENT_NAME=workflow_dispatch FULL_VERSION_INPUT=v1.2.3 \
    GITHUB_REF=refs/heads/main IS_FORK=false FAIL_ON_BLOCKED=""
expect "dispatch" mode trunk
expect "dispatch" enforce true

# --- Case 5: FAIL_ON_BLOCKED=false bypasses enforcement on trunk ---------------
# The release-readiness path: workflow_call runs under the caller's event.
run EVENT_NAME=push FULL_VERSION_INPUT=v1.2.3 \
    GITHUB_REF=refs/heads/main IS_FORK=false FAIL_ON_BLOCKED=false
expect "bypass" mode trunk
expect "bypass" enforce false
expect "bypass" fossa_mode REPORT

# --- Case 6: ref precedence — explicit input wins, else github.ref -------------
run EVENT_NAME=push FULL_VERSION_INPUT=v1 GITHUB_REF=refs/heads/main \
    GIT_REF_INPUT=refs/tags/v9.9.9 IS_FORK=false FAIL_ON_BLOCKED=""
expect "ref-input" ref refs/tags/v9.9.9
run EVENT_NAME=push FULL_VERSION_INPUT=v1 GITHUB_REF=refs/heads/main \
    GIT_REF_INPUT="" IS_FORK=false FAIL_ON_BLOCKED=""
expect "ref-fallback" ref refs/heads/main

# --- Case 7: FULL_VERSION_INPUT is used verbatim; empty falls back to describe -
run EVENT_NAME=push FULL_VERSION_INPUT=7.7.7-custom \
    GITHUB_REF=refs/heads/main IS_FORK=false FAIL_ON_BLOCKED=""
expect "version-input" full_version 7.7.7-custom
expect "version-input" fossa_revision 7.7.7-custom
# Runs `git describe` for real, so this case needs the repo checkout (CI uses
# fetch-depth: 0). Assert non-empty rather than a value that moves per commit.
run EVENT_NAME=push FULL_VERSION_INPUT="" \
    GITHUB_REF=refs/heads/main IS_FORK=false FAIL_ON_BLOCKED=""
if [ -n "$(get full_version)" ]; then
  pass "version-describe: git describe fallback is non-empty"
else
  fail "version-describe: git describe fallback is non-empty" "stdout=[$STDOUT]"
fi

# --- Case 8: is_fork passes through untouched -----------------------------------
run EVENT_NAME=pull_request HEAD_REF=f FULL_VERSION_INPUT=v1 \
    GITHUB_REF=refs/pull/7/merge IS_FORK=true FAIL_ON_BLOCKED=""
expect "fork" is_fork true

printf '\n%d passed, %d failed\n' "$pass_count" "$fail_count"
[ "$fail_count" -eq 0 ]
