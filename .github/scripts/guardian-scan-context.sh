#!/usr/bin/env bash
# Classifies a guardian-scan.yaml run into its mode and derived settings and
# writes them to $GITHUB_OUTPUT. Extracted from the workflow's `setup` job so
# the classification is testable (guardian-scan-context.test.sh): this tuple
# decides whether a run uploads to Guardian, runs the DB sync + Jira report,
# and blocks on Guardian's verdict — and the `queue` arm cannot be exercised
# end-to-end until a merge queue exists (SOL-152974).
#
# Modes:
#   pr     pull_request event        report/diff only, no upload
#   queue  merge_group event         scan jobs skip entirely (see the workflow)
#   trunk  everything else           upload + enforce (unless FAIL_ON_BLOCKED=false)
#
# Environment (workflow maps event context in):
#   EVENT_NAME, HEAD_REF, GITHUB_REF, GIT_REF_INPUT, FULL_VERSION_INPUT,
#   FAIL_ON_BLOCKED, IS_FORK, GITHUB_OUTPUT
set -euo pipefail

# FOSSA, Prisma, and db-sync must share one version prefix (DATAGO-147232).
if [ -n "${FULL_VERSION_INPUT:-}" ]; then
  FULL_VERSION="$FULL_VERSION_INPUT"
else
  FULL_VERSION="$(git describe --tags --always)"
fi

if [ "${EVENT_NAME:-}" = "pull_request" ]; then
  MODE="pr"; IS_PR="true"; IS_TRUNK="false"; IS_QUEUE="false"
  FOSSA_BRANCH="PR"; FOSSA_REVISION="${HEAD_REF:-}"
elif [ "${EVENT_NAME:-}" = "merge_group" ]; then
  # Its own mode, and must not fall through to the `else`: a queue entry is a
  # throwaway ref that may never land, so classifying it as trunk would upload
  # scan results, run the Guardian DB sync and its Jira report, and block on
  # Guardian's verdict, once per entry (SOL-152974). The scan jobs skip on
  # is_queue — the workflow says why. FOSSA_BRANCH is still set, defensively:
  # if a queue scan is ever re-enabled, it must not masquerade as main.
  MODE="queue"; IS_PR="false"; IS_TRUNK="false"; IS_QUEUE="true"
  FOSSA_BRANCH="merge-queue"; FOSSA_REVISION="$FULL_VERSION"
else
  MODE="trunk"; IS_PR="false"; IS_TRUNK="true"; IS_QUEUE="false"
  FOSSA_BRANCH="main"; FOSSA_REVISION="$FULL_VERSION"
fi

# Enforce (block) only on trunk and only when not bypassed.
ENFORCE="false"
if [ "$IS_TRUNK" = "true" ] && [ "${FAIL_ON_BLOCKED:-}" != "false" ]; then ENFORCE="true"; fi
# FOSSA licensing guard: BLOCK when enforcing, else REPORT.
if [ "$ENFORCE" = "true" ]; then FOSSA_MODE="BLOCK"; else FOSSA_MODE="REPORT"; fi

# The line an operator needs when asking "which mode did this run take?" —
# $GITHUB_OUTPUT is never displayed in the run log.
echo "Resolved: mode=$MODE event=${EVENT_NAME:-} is_fork=${IS_FORK:-false} enforce=$ENFORCE fossa_branch=$FOSSA_BRANCH"

{
  echo "mode=$MODE"
  echo "ref=${GIT_REF_INPUT:-${GITHUB_REF:-}}"
  echo "fossa_branch=$FOSSA_BRANCH"
  echo "fossa_revision=$FOSSA_REVISION"
  echo "full_version=$FULL_VERSION"
  echo "is_fork=${IS_FORK:-false}"
  echo "is_pr=$IS_PR"
  echo "is_trunk=$IS_TRUNK"
  echo "is_queue=$IS_QUEUE"
  echo "enforce=$ENFORCE"
  echo "fossa_mode=$FOSSA_MODE"
} >> "$GITHUB_OUTPUT"
