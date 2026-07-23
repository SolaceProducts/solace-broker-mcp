#!/usr/bin/env bash
#
# PreToolUse hook (Bash): when about to run `gh pr create` / `gh pr edit`, remind
# to update CHANGELOG.md [Unreleased] — but only if this branch actually changed
# production surface and hasn't touched the section yet. Never blocks; it only
# injects a note. The CI gate (.github/scripts/changelog-check.sh) is the hard
# backstop and also covers PRs opened by hand in the web UI.
#
set -euo pipefail

input=$(cat)

# Anchor to the concrete command, not an inferred intent. Substring match on the
# raw tool payload avoids a JSON parser dependency.
case "$input" in
  *"gh pr create"*|*"gh pr edit"*) ;;
  *) exit 0 ;;
esac

# Prefer origin/main to match the CI gate's base; fall back to local main.
base=$(git merge-base origin/main HEAD 2>/dev/null || git merge-base main HEAD 2>/dev/null) || exit 0
changed=$(git diff --name-only "$base"...HEAD 2>/dev/null) || exit 0

# Production surface, excluding Go test files (a test-only change needs no entry).
# Pattern is the single source of truth shared with the CI gate and /cut-release.
source "$(dirname "${BASH_SOURCE[0]}")/../../.github/scripts/production-surface.sh"
surface_hits=$(grep -E "$SURFACE_RE" <<<"$changed" | grep -v "$SURFACE_TEST_EXCLUDE" || true)
[ -n "$surface_hits" ] || exit 0

extract_unreleased() {
  # || true so a missing CHANGELOG.md at this ref yields empty output rather than
  # tripping `set -o pipefail` (keeps the hook non-blocking).
  { git show "$1:CHANGELOG.md" 2>/dev/null || true; } | awk '
    /^## \[Unreleased\]/ { p = 1; next }
    p && /^## \[/        { exit }
    p                    { print }
  '
}

if [ "$(extract_unreleased "$base")" = "$(extract_unreleased HEAD)" ]; then
  cat <<'EOF'
{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"This branch changes production surface (internal/config, internal/tools, or tools.yaml) but CHANGELOG.md [Unreleased] has no new entry. Run /changelog to draft one before opening/updating the PR, or apply the 'no-changelog' label. CI will post an advisory warning (non-blocking); the release will fail at tag time if the entry is still missing."}}
EOF
fi

exit 0
