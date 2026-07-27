#!/usr/bin/env bash
#
# CI gate: every commit a PR contributes must carry a Developer Certificate of
# Origin sign-off (https://developercertificate.org/). This is the control the
# project relies on instead of a contributor licence agreement, so it BLOCKS —
# there is deliberately no label, flag, or env var that skips it.
#
# A commit passes when its message contains a `Signed-off-by:` line whose email
# matches the commit's author or committer, case-insensitively.
#
#   Why email, not name: emails are stable identifiers. Names differ in casing,
#   accents, and ordering, which produces noisy failures without adding control.
#
#   Why the committer counts too: DCO is a chain. Whoever forwards a change signs
#   off on it. `git rebase --signoff` and `git cherry-pick -s` attribute the
#   sign-off to the person running them, who becomes the committer while the
#   original author is preserved. Accepting either end keeps that flow working.
#
#   Why not "any sign-off line at all": a cherry-picked or copied commit carries
#   the *original* author's sign-off, so nobody in this PR asserted anything.
#   That is the common accidental hole, and matching author-or-committer closes it.
#
# Which commits are checked: the ones this PR contributes, computed as everything
# reachable from the PR head but not from the base. Commits already on the base
# branch are somebody else's contribution and are not re-litigated here.
#
# Merge commits are exempt. Keeping a branch current with `git merge main`
# produces a commit git does not sign off, and its content comes from commits
# that are checked in their own right. Caveat, stated so it is not mistaken for
# coverage: edits made while resolving a merge conflict live only in the merge
# commit and are therefore not covered. Every mainstream DCO implementation,
# GitHub's DCO app included, shares this exemption.
#
# Squash-merge safety: the repo's squash-message setting is COMMIT_MESSAGES, so
# the individual commits' sign-off lines survive into the squashed commit on
# main. Merge-commit merges preserve them directly.
#
# Bots are not exempt. An author-name exemption would be trivially forgeable
# (`git commit --author='renovate[bot] <...>'`), which is exactly the bypass this
# gate must not have. Renovate signs off its own commits instead, via the
# `commitBody` setting in .github/renovate.json — keep that address in step with
# the identity Renovate actually commits under.
#
# Fork PRs: needs only a checkout and read-only `contents: read`. No secrets, no
# API token. A fork *can* edit this file or drop the job in its own PR, because
# `pull_request` runs workflows from the PR's own ref. Branch protection is the
# defence: this check must stay in the required-status-checks list (see
# .github/ADMIN_SETUP.md), because a required check that never reports blocks the
# merge rather than passing it.
#
# Env:
#   HEAD_SHA  head commit of the PR       (defaults to HEAD)
#   BASE_SHA  base commit of the PR       (defaults to origin/main)
#   BASE_REF  base branch name, e.g. main (optional; also excluded when the
#             matching origin/<ref> exists, so base-branch commits merged in
#             after BASE_SHA was captured are not attributed to this PR)
#
set -euo pipefail

HEAD_REV="${HEAD_SHA:-HEAD}"
BASE_REV="${BASE_SHA:-origin/main}"

for rev in "$HEAD_REV" "$BASE_REV"; do
  if ! git rev-parse --quiet --verify "${rev}^{commit}" >/dev/null; then
    echo "::error::DCO check cannot resolve '${rev}'. The checkout needs full history (actions/checkout with fetch-depth: 0)." >&2
    exit 1
  fi
done

# Exclude the live base tip as well when it is visible. BASE_SHA is a snapshot
# taken when the workflow event fired; anything the author merged in from the
# base branch is an ancestor of that tip, so excluding both keeps other people's
# commits out of this PR's range.
EXCLUDE_BASE_REF=""
if [ -n "${BASE_REF:-}" ] && git rev-parse --quiet --verify "refs/remotes/origin/${BASE_REF}^{commit}" >/dev/null; then
  EXCLUDE_BASE_REF="^refs/remotes/origin/${BASE_REF}"
fi

# shellcheck disable=SC2086 # EXCLUDE_BASE_REF is a single rev or deliberately empty
commits=$(git rev-list --no-merges "$HEAD_REV" "^${BASE_REV}" $EXCLUDE_BASE_REF)

if [ -z "$commits" ]; then
  echo "No non-merge commits contributed by this PR — nothing to check."
  exit 0
fi

# Sign-off lines on a commit, verbatim. Anchored on the `Signed-off-by:` key so
# sibling trailers (`Co-Authored-By:`, `Reviewed-by:`) can never satisfy the
# check, and matched over the whole message rather than only git's trailer block
# so a sign-off followed by prose still counts.
signoff_lines() {
  git show -s --format='%B' "$1" |
    awk 'tolower($0) ~ /^[ \t]*signed-off-by:/ { print }'
}

# Lowercased email from each sign-off line — the first <...> on the line, which
# is the signer's own address.
signoff_emails() {
  signoff_lines "$1" |
    awk 'match($0, /<[^>]+>/) { print tolower(substr($0, RSTART + 1, RLENGTH - 2)) }'
}

lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }

failed=""
total=0
for sha in $commits; do
  total=$((total + 1))
  author_email=$(lower "$(git show -s --format='%ae' "$sha")")
  committer_email=$(lower "$(git show -s --format='%ce' "$sha")")

  matched=no
  while IFS= read -r email; do
    [ -n "$email" ] || continue
    if [ "$email" = "$author_email" ] || [ "$email" = "$committer_email" ]; then
      matched=yes
      break
    fi
  done < <(signoff_emails "$sha")

  if [ "$matched" = no ]; then
    failed="${failed}${sha}"$'\n'
  fi
done

if [ -z "$failed" ]; then
  echo "DCO: all ${total} commit(s) contributed by this PR are signed off — OK."
  exit 0
fi

failed_count=$(grep -c . <<<"$failed")
echo "::error::${failed_count} of ${total} commit(s) in this pull request are missing a Developer Certificate of Origin sign-off."
echo
while IFS= read -r sha; do
  [ -n "$sha" ] || continue
  echo "  $(git show -s --format='%h %s' "$sha")"
  echo "    author:    $(git show -s --format='%an <%ae>' "$sha")"
  echo "    committer: $(git show -s --format='%cn <%ce>' "$sha")"
  found=$(signoff_lines "$sha")
  if [ -n "$found" ]; then
    echo "    sign-off present but no email matches the author or committer:"
    sed 's/^[[:space:]]*/      /' <<<"$found"
  else
    echo "    no Signed-off-by line found"
  fi
  echo
done <<<"$failed"

cat <<'EOF'
Every commit needs a sign-off line carrying its own author (or committer) email:

  Signed-off-by: Your Name <your.email@example.com>

`git commit -s` adds it for you. To fix commits you have already made:

  # only the most recent commit
  git commit --amend -s --no-edit && git push --force-with-lease

  # every commit this PR adds (N = the number of commits listed above)
  git rebase --signoff HEAD~N && git push --force-with-lease

If the email above is not the one you meant to sign off with, set
`git config user.email` first, then re-run. Adding the line certifies the
Developer Certificate of Origin: https://developercertificate.org/ — see
.github/CONTRIBUTING.md#developer-certificate-of-origin.
EOF
exit 1
