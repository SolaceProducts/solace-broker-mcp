#!/usr/bin/env bash
#
# Self-test for codeql-gate.sh. Builds throwaway analyses/alerts fixtures, runs
# the real script against them, and asserts the exit code and the message.
# Entirely offline — the gate's fetching lives in the workflow, so every
# decision it makes is reachable from a file on disk.
#
# The cases that matter most are the ones asserting a *refusal*: a gate that
# passes when the analysis is missing looks exactly like a gate that passed
# because the code is clean, and that is the failure mode SOL-152410 and
# SOL-152412 exist to prevent.
#
# Run manually:  .github/scripts/codeql-gate.test.sh
# Runs in CI as the first step of the `CodeQL gate` job in
# .github/workflows/codeql.yml, before the gate it guards.
#
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
GATE="${SCRIPT_DIR}/codeql-gate.sh"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

pass_count=0
fail_count=0
pass() { pass_count=$((pass_count + 1)); printf 'ok   - %s\n' "$1"; }
fail() { fail_count=$((fail_count + 1)); printf 'FAIL - %s\n' "$1"; [ -n "${2:-}" ] && printf '       %s\n' "$2"; return 0; }

# --- fixtures ----------------------------------------------------------------

# The commit under analysis, and an earlier one whose analyses are stale.
SHA='1111111111111111111111111111111111111111'
OLD_SHA='2222222222222222222222222222222222222222'

BOTH_ANALYSES="$WORK/analyses-both.json"
cat > "$BOTH_ANALYSES" <<EOF
[
  {"category": "/language:go", "commit_sha": "$SHA", "error": "", "results_count": 0},
  {"category": "/language:actions", "commit_sha": "$SHA", "error": "", "results_count": 0}
]
EOF

GO_ONLY_ANALYSES="$WORK/analyses-go-only.json"
printf '[{"category": "/language:go", "commit_sha": "%s", "error": "", "results_count": 0}]\n' "$SHA" > "$GO_ONLY_ANALYSES"

ERRORED_ANALYSES="$WORK/analyses-errored.json"
cat > "$ERRORED_ANALYSES" <<EOF
[
  {"category": "/language:go", "commit_sha": "$SHA", "error": "1 error(s) during extraction", "results_count": 0},
  {"category": "/language:actions", "commit_sha": "$SHA", "error": "", "results_count": 0}
]
EOF

# Every analysis belongs to an earlier commit — this commit was never scanned,
# even though the ref carries a full, error-free set.
STALE_ANALYSES="$WORK/analyses-stale.json"
cat > "$STALE_ANALYSES" <<EOF
[
  {"category": "/language:go", "commit_sha": "$OLD_SHA", "error": "", "results_count": 0},
  {"category": "/language:actions", "commit_sha": "$OLD_SHA", "error": "", "results_count": 0}
]
EOF

# The realistic shape: a stale pair plus a fresh pair, as a re-pushed pull
# request ref actually looks. This must pass.
ACCUMULATED_ANALYSES="$WORK/analyses-accumulated.json"
cat > "$ACCUMULATED_ANALYSES" <<EOF
[
  {"category": "/language:go", "commit_sha": "$OLD_SHA", "error": "", "results_count": 0},
  {"category": "/language:actions", "commit_sha": "$OLD_SHA", "error": "", "results_count": 0},
  {"category": "/language:go", "commit_sha": "$SHA", "error": "", "results_count": 0},
  {"category": "/language:actions", "commit_sha": "$SHA", "error": "", "results_count": 0}
]
EOF

# An error on the *stale* analysis must not condemn a clean fresh one.
STALE_ERROR_ANALYSES="$WORK/analyses-stale-error.json"
cat > "$STALE_ERROR_ANALYSES" <<EOF
[
  {"category": "/language:go", "commit_sha": "$OLD_SHA", "error": "1 error(s) during extraction", "results_count": 0},
  {"category": "/language:actions", "commit_sha": "$OLD_SHA", "error": "", "results_count": 0},
  {"category": "/language:go", "commit_sha": "$SHA", "error": "", "results_count": 0},
  {"category": "/language:actions", "commit_sha": "$SHA", "error": "", "results_count": 0}
]
EOF

EMPTY="$WORK/empty.json"
printf '[]\n' > "$EMPTY"

# alert <file> <number> <state> <severity> <security_severity>
alert() {
  cat > "$1" <<EOF
[{"number": $2,
  "state": "$3",
  "rule": {"id": "go/example", "severity": "$4", "security_severity_level": "$5"},
  "most_recent_instance": {"location": {"path": "internal/example.go", "start_line": 42}}}]
EOF
}

HIGH_ALERT="$WORK/alert-high.json";     alert "$HIGH_ALERT" 7 open warning high
MEDIUM_ALERT="$WORK/alert-medium.json"; alert "$MEDIUM_ALERT" 8 open warning medium
DISMISSED="$WORK/alert-dismissed.json"; alert "$DISMISSED" 9 dismissed warning critical

# severity `error` with no security severity at all — the non-security arm of
# the threshold, which a `security_severity_level`-only check would miss.
ERROR_SEV_ALERT="$WORK/alert-error-sev.json"
cat > "$ERROR_SEV_ALERT" <<'EOF'
[{"number": 11,
  "state": "open",
  "rule": {"id": "actions/example", "severity": "error", "security_severity_level": null},
  "most_recent_instance": {"location": {"path": ".github/workflows/x.yml", "start_line": 3}}}]
EOF

# --- runner ------------------------------------------------------------------

# run <mode> <analyze-result> <analyses> <alerts> <baseline>
# Sets globals RC and OUT (stdout+stderr merged).
run() {
  RC=0
  OUT=$(
    CODEQL_GATE_MODE="$1" \
    CODEQL_ANALYZE_RESULT="$2" \
    CODEQL_EXPECTED_LANGUAGES="actions go" \
    CODEQL_ANALYZED_SHA="$SHA" \
    CODEQL_ANALYSES_JSON="$3" \
    CODEQL_ALERTS_JSON="$4" \
    CODEQL_BASELINE_JSON="$5" \
    bash "$GATE" 2>&1
  ) || RC=$?
  return 0
}

has() { grep -q "$2" <<<"$1"; }

# --- Case 1: clean run -> pass ------------------------------------------------
run blocking success "$BOTH_ANALYSES" "$EMPTY" "$EMPTY"
if [ "$RC" -eq 0 ] && has "$OUT" 'gate passed'; then
  pass "clean analysis with no alerts passes"
else
  fail "clean analysis with no alerts passes" "rc=$RC out=[$OUT]"
fi

# --- Case 2: analysis did not complete -> refuse ------------------------------
run blocking failure "$BOTH_ANALYSES" "$EMPTY" "$EMPTY"
if [ "$RC" -ne 0 ] && has "$OUT" 'did not complete'; then
  pass "failed analyze matrix fails the gate"
else
  fail "failed analyze matrix fails the gate" "rc=$RC out=[$OUT]"
fi

# A cancelled or skipped matrix is the same absence, and `skipped` is the one
# most likely to arrive by accident (a job-level `if` someone adds later).
for result in cancelled skipped; do
  run blocking "$result" "$BOTH_ANALYSES" "$EMPTY" "$EMPTY"
  if [ "$RC" -ne 0 ]; then
    pass "analyze = $result fails the gate"
  else
    fail "analyze = $result fails the gate" "rc=$RC out=[$OUT]"
  fi
done

# --- Case 3: one language never uploaded an analysis -> refuse ----------------
run blocking success "$GO_ONLY_ANALYSES" "$EMPTY" "$EMPTY"
if [ "$RC" -ne 0 ] && has "$OUT" 'actions'; then
  pass "missing analysis for one language fails, naming it"
else
  fail "missing analysis for one language fails, naming it" "rc=$RC out=[$OUT]"
fi

# --- Case 4: no analyses at all -> refuse (the vacuous-pass regression) -------
run blocking success "$EMPTY" "$EMPTY" "$EMPTY"
if [ "$RC" -ne 0 ]; then
  pass "no analyses at all fails rather than passes on absence"
else
  fail "no analyses at all fails rather than passes on absence" "rc=$RC out=[$OUT]"
fi

# --- Case 4b: analyses exist, but all for an earlier commit -> refuse ---------
# The dangerous one: the ref looks fully scanned, and this commit was not.
run blocking success "$STALE_ANALYSES" "$EMPTY" "$EMPTY"
if [ "$RC" -ne 0 ] && has "$OUT" "$SHA"; then
  pass "analyses for an earlier commit only fails, naming the unscanned commit"
else
  fail "analyses for an earlier commit only fails, naming the unscanned commit" "rc=$RC out=[$OUT]"
fi

# ...and the same ref once this commit *is* scanned must pass, or every re-push
# would be permanently red.
run blocking success "$ACCUMULATED_ANALYSES" "$EMPTY" "$EMPTY"
if [ "$RC" -eq 0 ]; then
  pass "stale analyses alongside fresh ones for this commit pass"
else
  fail "stale analyses alongside fresh ones for this commit pass" "rc=$RC out=[$OUT]"
fi

# --- Case 5: analysis uploaded but errored -> inconclusive, refuse ------------
run blocking success "$ERRORED_ANALYSES" "$EMPTY" "$EMPTY"
if [ "$RC" -ne 0 ] && has "$OUT" 'inconclusive'; then
  pass "errored analysis is inconclusive and fails"
else
  fail "errored analysis is inconclusive and fails" "rc=$RC out=[$OUT]"
fi

# An error on a superseded analysis says nothing about this commit.
run blocking success "$STALE_ERROR_ANALYSES" "$EMPTY" "$EMPTY"
if [ "$RC" -eq 0 ]; then
  pass "an error on a superseded analysis does not condemn this commit"
else
  fail "an error on a superseded analysis does not condemn this commit" "rc=$RC out=[$OUT]"
fi

# --- Case 6: new high security-severity alert -> refuse -----------------------
run blocking success "$BOTH_ANALYSES" "$HIGH_ALERT" "$EMPTY"
if [ "$RC" -ne 0 ] && has "$OUT" '#7'; then
  pass "new high security-severity alert fails, naming the alert"
else
  fail "new high security-severity alert fails, naming the alert" "rc=$RC out=[$OUT]"
fi

# --- Case 7: severity `error` with no security severity -> refuse -------------
run blocking success "$BOTH_ANALYSES" "$ERROR_SEV_ALERT" "$EMPTY"
if [ "$RC" -ne 0 ] && has "$OUT" '#11'; then
  pass "severity 'error' alert fails even with no security severity"
else
  fail "severity 'error' alert fails even with no security severity" "rc=$RC out=[$OUT]"
fi

# --- Case 8: medium security severity -> below threshold, pass ----------------
run blocking success "$BOTH_ANALYSES" "$MEDIUM_ALERT" "$EMPTY"
if [ "$RC" -eq 0 ]; then
  pass "medium security-severity alert is below threshold and passes"
else
  fail "medium security-severity alert is below threshold and passes" "rc=$RC out=[$OUT]"
fi

# --- Case 9: alert already open on main -> pre-existing, pass -----------------
# Same alert number on both refs. This is what preserves the documented
# new-findings-only scope.
run blocking success "$BOTH_ANALYSES" "$HIGH_ALERT" "$HIGH_ALERT"
if [ "$RC" -eq 0 ]; then
  pass "alert already open on main is pre-existing and passes"
else
  fail "alert already open on main is pre-existing and passes" "rc=$RC out=[$OUT]"
fi

# ...but a *different* alert alongside a pre-existing one must still fail, or a
# single baseline entry would whitewash the whole ref.
MIXED="$WORK/alert-mixed.json"
cat > "$MIXED" <<'EOF'
[{"number": 7,  "state": "open", "rule": {"id": "go/old", "severity": "warning", "security_severity_level": "high"},
  "most_recent_instance": {"location": {"path": "a.go", "start_line": 1}}},
 {"number": 12, "state": "open", "rule": {"id": "go/new", "severity": "warning", "security_severity_level": "critical"},
  "most_recent_instance": {"location": {"path": "b.go", "start_line": 2}}}]
EOF
run blocking success "$BOTH_ANALYSES" "$MIXED" "$HIGH_ALERT"
if [ "$RC" -ne 0 ] && has "$OUT" '#12'; then
  pass "a new alert beside a pre-existing one still fails"
else
  fail "a new alert beside a pre-existing one still fails" "rc=$RC out=[$OUT]"
fi

# --- Case 10: dismissed alert -> not open, pass -------------------------------
# The reason the gate reads the alerts API rather than the raw SARIF: a false
# positive has to be dismissable, or the gate becomes unopenable.
run blocking success "$BOTH_ANALYSES" "$DISMISSED" "$EMPTY"
if [ "$RC" -eq 0 ]; then
  pass "dismissed alert does not block"
else
  fail "dismissed alert does not block" "rc=$RC out=[$OUT]"
fi

# --- Case 11: report mode warns without blocking ------------------------------
run report success "$BOTH_ANALYSES" "$HIGH_ALERT" "$EMPTY"
if [ "$RC" -eq 0 ] && has "$OUT" '::warning::'; then
  pass "report mode warns without failing"
else
  fail "report mode warns without failing" "rc=$RC out=[$OUT]"
fi

run report success "$EMPTY" "$EMPTY" "$EMPTY"
if [ "$RC" -eq 0 ] && has "$OUT" '::warning::'; then
  pass "report mode warns on a missing analysis without failing"
else
  fail "report mode warns on a missing analysis without failing" "rc=$RC out=[$OUT]"
fi

# --- Case 12: a missing input file is a refusal, not a pass -------------------
run blocking success "$BOTH_ANALYSES" "$WORK/does-not-exist.json" "$EMPTY"
if [ "$RC" -ne 0 ] && has "$OUT" 'does not exist'; then
  pass "missing input file fails rather than passing on absence"
else
  fail "missing input file fails rather than passing on absence" "rc=$RC out=[$OUT]"
fi

# --- Case 13: an unrecognised mode is a hard error, not a silent default ------
run enforcing success "$BOTH_ANALYSES" "$EMPTY" "$EMPTY"
if [ "$RC" -eq 2 ]; then
  pass "unrecognised CODEQL_GATE_MODE exits 2 instead of guessing"
else
  fail "unrecognised CODEQL_GATE_MODE exits 2 instead of guessing" "rc=$RC out=[$OUT]"
fi

# --- Case 14: every required variable is actually required -------------------
# The `: "${VAR:?}"` guards, asserted one at a time. Without this, dropping an
# `env:` line from the workflow would silently change what the gate enforces.
for var in CODEQL_GATE_MODE CODEQL_ANALYZE_RESULT CODEQL_EXPECTED_LANGUAGES \
           CODEQL_ANALYZED_SHA CODEQL_ANALYSES_JSON CODEQL_ALERTS_JSON \
           CODEQL_BASELINE_JSON; do
  # Export first, then unset one — `env -u X X=v` would re-set it and the case
  # would pass for the wrong reason.
  rc=0
  (
    export CODEQL_GATE_MODE=blocking
    export CODEQL_ANALYZE_RESULT=success
    export CODEQL_EXPECTED_LANGUAGES="actions go"
    export CODEQL_ANALYZED_SHA="$SHA"
    export CODEQL_ANALYSES_JSON="$BOTH_ANALYSES"
    export CODEQL_ALERTS_JSON="$EMPTY"
    export CODEQL_BASELINE_JSON="$EMPTY"
    unset "$var"
    bash "$GATE"
  ) >/dev/null 2>&1 || rc=$?
  if [ "$rc" -ne 0 ]; then
    pass "unset $var fails the gate"
  else
    fail "unset $var fails the gate" "rc=$rc"
  fi
done

printf '\n%d passed, %d failed\n' "$pass_count" "$fail_count"
[ "$fail_count" -eq 0 ]
