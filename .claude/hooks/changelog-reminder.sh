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
surface_hits=$(grep -E '^(internal/config/|internal/tools/|internal/composite/definitions/tools\.yaml)' <<<"$changed" | grep -v '_test\.go$' || true)
[ -n "$surface_hits" ] || exit 0

extract_unreleased() {
  git show "$1:CHANGELOG.md" 2>/dev/null | awk '
    /^## \[Unreleased\]/ { p = 1; next }
    p && /^## \[/        { exit }
    p                    { print }
  '
}

if [ "$(extract_unreleased "$base")" = "$(extract_unreleased HEAD)" ]; then
  cat <<'EOF'
{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"This branch changes production surface (internal/config, internal/tools, or tools.yaml) but CHANGELOG.md [Unreleased] has no new entry. Run /changelog to draft one before opening/updating the PR, or plan to apply the 'no-changelog' label. The CI changelog gate will otherwise fail this PR."}}
EOF
fi

exit 0
