#!/usr/bin/env bash
#
# The blocking half of CodeQL advanced setup (SOL-153411).
#
# `codeql-action/analyze` has no severity threshold input of any kind — its full
# input list carries `output, upload, cleanup-level, ram, add-snippets,
# skip-queries, threads, ref, sha, category, upload-database,
# post-processed-sarif-path, wait-for-processing, token, matrix, expect-error`
# and nothing resembling `fail-on`. So the matrix passes whenever analysis
# *completes*, whatever it found, and the verdict has to live somewhere else.
# This is that somewhere else, run as the `CodeQL gate` job in
# .github/workflows/codeql.yml — the required status check.
#
# TWO FAILURE MODES, NOT ONE
#
# A finding at or above threshold fails the gate. So does a *missing* analysis.
# The second is the one worth writing a script for: default setup's own check
# concluded `neutral` ("N configurations not found") when no analysis arrived,
# and GitHub counts `neutral` as passing, so the gate was absent rather than
# blocking exactly when it was needed. That is the shape SOL-152410 and
# SOL-152412 exist to prevent, and this script refuses to reproduce it.
#
# WHY THE FETCHING IS SOMEWHERE ELSE
#
# Everything here is a pure function of three JSON files. The workflow does the
# `gh api` calls and writes them to disk; this script only judges. That is what
# lets codeql-gate.test.sh run offline against fixtures on every pull request,
# rather than asking a reviewer to trust the logic by reading it.
#
# Env (all required — a gate that defaults its own inputs is not a gate; see the
# classification-script discussion on PR #322 for why a `:-` fallback to the
# most permissive arm is the wrong default):
#   CODEQL_GATE_MODE           blocking | report
#   CODEQL_ANALYZE_RESULT      GitHub `needs.<job>.result` for the analyze matrix
#   CODEQL_EXPECTED_LANGUAGES  space-separated, e.g. "actions go"
#   CODEQL_ANALYZED_SHA        the commit the analyze jobs examined
#   CODEQL_ANALYSES_JSON       flat JSON array: code-scanning analyses for the ref
#   CODEQL_ALERTS_JSON         flat JSON array: open alerts for the ref
#   CODEQL_BASELINE_JSON       flat JSON array: open alerts for refs/heads/main
#
set -euo pipefail

: "${CODEQL_GATE_MODE:?set to blocking or report}"
: "${CODEQL_ANALYZE_RESULT:?the analyze matrix job result}"
: "${CODEQL_EXPECTED_LANGUAGES:?space-separated CodeQL languages}"
: "${CODEQL_ANALYZED_SHA:?the commit SHA the analyze jobs examined}"
: "${CODEQL_ANALYSES_JSON:?path to the analyses JSON}"
: "${CODEQL_ALERTS_JSON:?path to the alerts JSON}"
: "${CODEQL_BASELINE_JSON:?path to the baseline alerts JSON}"

# The threshold, and deliberately not configurable. It reproduces what default
# setup enforced and .github/ADMIN_SETUP.md documents: severity `error`, or
# security severity `critical` or `high`. A `medium` or `low` security-severity
# finding leaves the gate green. There is no scenario in which turning this off
# is the right call, so it is a constant rather than a knob — change it in a
# commit a reviewer can see, not in a workflow input.
SEVERITY_FAIL='error'
SECURITY_SEVERITY_FAIL='critical high'

# Decide the mode once, so every non-OK exit below honours it. Same shape as
# changelog-check.sh's CHANGELOG_GATE_MODE.
#
# `blocking` on `pull_request` and `merge_group` — the two events where a merge
# can still be stopped. `report` on push/schedule, where the analysis being run
# *is* the baseline and there is no merge left to block; a dirty `main` should
# not wedge every open pull request behind a context nobody can turn green.
case "$CODEQL_GATE_MODE" in
  blocking) LEVEL='::error::'; EC=1 ;;
  report)   LEVEL='::warning::'; EC=0 ;;
  *)
    echo "codeql-gate: CODEQL_GATE_MODE must be 'blocking' or 'report', got '${CODEQL_GATE_MODE}'." >&2
    exit 2
    ;;
esac

for f in "$CODEQL_ANALYSES_JSON" "$CODEQL_ALERTS_JSON" "$CODEQL_BASELINE_JSON"; do
  if [ ! -f "$f" ]; then
    echo "${LEVEL}codeql-gate: '$f' does not exist — the fetch step did not produce it, so there is nothing to judge. Refusing to pass on absence."
    exit "$EC"
  fi
done

# --- 1. Did the analysis run at all? -----------------------------------------
#
# `needs.analyze.result` is `success` only when every matrix leg succeeded;
# `failure`, `cancelled` and `skipped` all mean no trustworthy verdict exists.
if [ "$CODEQL_ANALYZE_RESULT" != "success" ]; then
  echo "${LEVEL}CodeQL analysis did not complete: analyze = '${CODEQL_ANALYZE_RESULT}'. No verdict can be derived, so the gate fails rather than passes."
  exit "$EC"
fi

# --- 2. Is there one analysis per expected language, and did any error out? ---
#
# Matched on two things, both necessary:
#
#   `category`   — set explicitly to `/language:<lang>` by the analyze step.
#                  Matching on anything GitHub derives for us instead (analysis
#                  key, environment blob) would make this depend on how a matrix
#                  job name happens to be rendered.
#   `commit_sha` — the commit analyzed. Analyses accumulate on a pull request ref
#                  across pushes, so without this a stale analysis from an
#                  earlier commit would satisfy the check while this commit went
#                  unscanned. That is a vacuous pass with a plausible-looking
#                  audit trail, which is the worst kind.
missing=''
for lang in $CODEQL_EXPECTED_LANGUAGES; do
  count=$(jq \
    --arg cat "/language:${lang}" \
    --arg sha "$CODEQL_ANALYZED_SHA" \
    '[.[] | select(.category == $cat) | select(.commit_sha == $sha)] | length' \
    "$CODEQL_ANALYSES_JSON")
  if [ "$count" -eq 0 ]; then
    missing="${missing} ${lang}"
  fi
done

if [ -n "$missing" ]; then
  echo "${LEVEL}No CodeQL analysis of commit ${CODEQL_ANALYZED_SHA} was uploaded for:${missing}. Expected one per language in '${CODEQL_EXPECTED_LANGUAGES}'. An absent analysis is the vacuous-pass failure mode this gate exists to catch."
  exit "$EC"
fi

# An analysis that uploaded but reported an extraction/query error is
# inconclusive: it may have produced no findings because it examined nothing.
errored=$(jq -r --arg sha "$CODEQL_ANALYZED_SHA" '[.[] | select(.commit_sha == $sha) | select((.category // "") | startswith("/language:")) | select((.error // "") != "")] | .[] | "\(.category): \(.error)"' "$CODEQL_ANALYSES_JSON")
if [ -n "$errored" ]; then
  echo "${LEVEL}CodeQL analysis completed but reported errors, so its result is inconclusive:"
  sed 's/^/  - /' <<<"$errored"
  exit "$EC"
fi

# --- 3. New findings at or above threshold ------------------------------------
#
# "New" means new relative to `main`, preserving default setup's documented
# new-findings-only scope. The comparison is by alert *number*, which is
# repository-scoped and stable across refs — an alert already open on `main`
# carries the same number on a pull request ref, so no rule-name-plus-line
# fingerprinting is needed and none of its fragility is inherited.
new_alerts=$(
  jq -r \
    --slurpfile baseline "$CODEQL_BASELINE_JSON" \
    --arg sev "$SEVERITY_FAIL" \
    --arg secsev "$SECURITY_SEVERITY_FAIL" \
    '
    ([$baseline[0][]?.number]) as $known
    | ($secsev | split(" ")) as $blocking_security
    | [ .[]
        | select(.state == "open")
        | select((.number | IN($known[])) | not)
        | select((.rule.severity == $sev)
                 or (.rule.security_severity_level as $s | $blocking_security | index($s)))
      ]
    | .[]
    | "#\(.number) [\(.rule.security_severity_level // .rule.severity // "?")] \(.rule.id // "?") — \(.most_recent_instance.location.path // "?"):\(.most_recent_instance.location.start_line // 0)"
    ' "$CODEQL_ALERTS_JSON"
)

if [ -n "$new_alerts" ]; then
  echo "${LEVEL}CodeQL found new alerts at or above the gate threshold (severity '${SEVERITY_FAIL}', or security severity '${SECURITY_SEVERITY_FAIL}'):"
  sed 's/^/  - /' <<<"$new_alerts"
  echo
  echo "Fix the finding, or dismiss the alert in the repository's Security tab with a"
  echo "reason if it is a false positive — a dismissed alert is no longer open and stops"
  echo "blocking. Alerts already open on main are pre-existing and are not counted here."
  exit "$EC"
fi

echo "CodeQL gate passed: analyses present for '${CODEQL_EXPECTED_LANGUAGES}', no new alerts at or above threshold."
