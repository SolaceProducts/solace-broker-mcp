#!/usr/bin/env bash
#
# CI check: a PR that changes production surface should also add an entry under
# the CHANGELOG's [Unreleased] section. Escape hatch: the `no-changelog` label.
#
# This is intentionally ADVISORY per-PR (warns, never blocks the merge) — the
# hard gate lives at release time (extract-release-notes.sh fails the release if
# the version's block is missing). Set CHANGELOG_GATE_MODE=blocking to make this
# fail PRs too.
#
# Env:
#   BASE_SHA             base commit of the PR (defaults to origin/main)
#   HEAD_SHA             head commit of the PR (defaults to HEAD)
#   LABELS               comma-joined PR label names
#   CHANGELOG_GATE_MODE  advisory (default) | blocking
#
set -euo pipefail

BASE="${BASE_SHA:-origin/main}"
HEAD="${HEAD_SHA:-HEAD}"

# Escape hatch — pure refactor/test/docs PRs can opt out with a label.
case ",${LABELS:-}," in
  *,no-changelog,*)
    echo "no-changelog label present — skipping CHANGELOG gate."
    exit 0
    ;;
esac

# advisory (default) warns without blocking; blocking fails the PR. Decide once
# so every non-OK exit below honors the mode.
MODE="${CHANGELOG_GATE_MODE:-advisory}"
if [ "$MODE" = "blocking" ]; then LEVEL="::error::"; EC=1; else LEVEL="::warning::"; EC=0; fi

# Diff from the merge-base, not the base tip: if main advances while the PR is
# open, base.sha moves forward, and a two-dot diff would misattribute unrelated
# commits. The merge-base isolates this PR's own changes.
MERGE_BASE=$(git merge-base "$BASE" "$HEAD") || {
  echo "${LEVEL}Could not compute a merge-base for '$BASE' and '$HEAD' — the branch shares no common ancestor with the base (force-pushed base or shallow clone?). Skipping the CHANGELOG check." >&2
  exit "$EC"
}

# Production surface whose change requires a CHANGELOG entry. Mirrors the
# breaking-surface list in the /changelog skill and SOL-152075. Go test files
# are excluded — a test-only change is not user- or operator-visible.
surface_re='^(internal/config/|internal/tools/|internal/composite/definitions/tools\.yaml)'

changed=$(git diff --name-only "$MERGE_BASE" "$HEAD")
surface_hits=$(grep -E "$surface_re" <<<"$changed" | grep -v '_test\.go$' || true)

if [ -z "$surface_hits" ]; then
  echo "No production surface touched — CHANGELOG entry not required."
  exit 0
fi

# Content of the [Unreleased] section at a given ref (empty if the file/section
# is absent). Stops at the next top-level version header.
extract_unreleased() {
  git show "$1:CHANGELOG.md" 2>/dev/null | awk '
    /^## \[Unreleased\]/ { p = 1; next }
    p && /^## \[/        { exit }
    p                    { print }
  '
}

if [ "$(extract_unreleased "$MERGE_BASE")" = "$(extract_unreleased "$HEAD")" ]; then
  echo "${LEVEL}This PR changes production surface but the CHANGELOG.md [Unreleased] section is unchanged."
  echo "Triggered by:"
  sed 's/^/  - /' <<<"$surface_hits"
  echo
  echo "Add an entry under [Unreleased] — run /changelog to draft one from your diff —"
  echo "or apply the 'no-changelog' label if this change has no user- or operator-visible"
  echo "surface (pure test/refactor/docs)."
  exit "$EC"
fi

echo "CHANGELOG [Unreleased] updated — OK."
