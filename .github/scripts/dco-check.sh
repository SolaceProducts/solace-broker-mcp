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
# Merge commits are exempt only when they contribute nothing of their own.
# Keeping a branch current with `git merge main` produces a commit git does not
# sign off, and all of its content comes from commits checked in their own
# right, so requiring a sign-off there would be friction with no control value.
# A merge commit *can* carry content no parent has — a conflict resolution, or an
# "evil merge" that edits or adds files while merging. Those must be signed off
# (`git merge --signoff`). Most DCO implementations, GitHub's DCO app included,
# exempt merges unconditionally and are bypassable this way.
#
# The test is `git merge-tree`: recompute the merge of the two parents and
# compare the resulting tree with the tree the merge commit actually recorded.
# Equal means the committer added nothing the merge itself did not produce.
#
#   Rejected alternative: `git diff-tree --cc`. It is hunk-level with three lines
#   of context, so two conflict-free changes landing two or three lines apart
#   share a combined hunk and an ordinary `git merge main` gets flagged as
#   merge-unique content. It also misses `git merge -s ours`, which discards the
#   other parent's changes wholesale.
#
# Conservative in every uncertain case, because the cost of a false positive is
# one `git merge --signoff` and the cost of a false negative is an uncovered
# contribution: octopus merges (merge-tree takes exactly two parents) and a
# recomputation that conflicts both mean "sign it off". Git older than 2.38
# cannot recompute a merge at all, and rather than silently weaken the test the
# check refuses to run — see the version gate below.
#
# Explicit non-goal: a merge that only *removes* content another branch added
# (`git merge -s ours` used as a revert) is a deletion, not a contribution. DCO
# governs the right to contribute code, not the right to delete it. merge-tree
# flags this case anyway, but do not read that as the control's purpose.
#
# Squash-merge safety: the repo's squash-message setting is COMMIT_MESSAGES, so
# the individual commits' sign-off lines survive into the squashed commit on
# main. Merge-commit merges preserve them directly.
#
# Bots are not exempt *by this script*. An author-name exemption in this logic
# would be trivially forgeable (`git commit --author='renovate[bot] <...>'`),
# which is exactly the bypass this gate must not have. Renovate signs off its
# own commits instead, via the `commitBody` setting in .github/renovate.json —
# keep that address in step with the identity Renovate actually commits under.
#
# Dependabot is the one deliberate exception, and it is not implemented here.
# Its commit-message config has no field for a trailer, so it cannot sign off
# no matter what — there is no "correct address" fix like Renovate's. Rather
# than add an author-name check to this script (the forgeable shape the
# paragraph above rejects), .github/workflows/dco.yaml skips the whole job when
# `github.event.pull_request.user.login == 'dependabot[bot]'`: a field GitHub
# itself asserts about who opened the PR, which a PR's contents cannot forge,
# and which only a reviewed change on main (not a PR) can add or widen. See
# that file for the full reasoning. Decided under SOL-152808, after confirming
# Renovate cannot be enrolled for this repo once it is public.
#
# Fork PRs: needs only a checkout and read-only `contents: read`. No secrets, no
# API token. A PR cannot switch the gate off, because .github/workflows/dco.yaml
# is triggered by `pull_request_target` and therefore runs the base ref's copy of
# both the workflow and this script. Under a plain `pull_request` trigger a fork
# could add `if: false` to the job, which GitHub counts as a *successful*
# required check, or swap the script out for `exit 0`. The job must also stay in
# the required-status-checks list (.github/ADMIN_SETUP.md), so that deleting it
# leaves a required check that never reports and blocks the merge.
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

# `git merge-tree --write-tree` landed in git 2.38. Refuse to run on anything
# older rather than silently falling back to a weaker merge check: a hard, honest
# failure beats a gate whose strictness depends on the runner image.
GIT_MAJOR=$(git --version | sed -E 's/[^0-9]*([0-9]+).*/\1/')
GIT_MINOR=$(git --version | sed -E 's/[^0-9]*[0-9]+\.([0-9]+).*/\1/')
if [ "$GIT_MAJOR" -lt 2 ] || { [ "$GIT_MAJOR" -eq 2 ] && [ "$GIT_MINOR" -lt 38 ]; }; then
  echo "::error::DCO check needs git >= 2.38 for 'git merge-tree --write-tree' (found: $(git --version)). Refusing to run rather than weaken the merge check." >&2
  exit 1
fi

# Echoes why a merge commit counts as a contribution, or nothing when it is
# exempt. The reason is surfaced to the contributor, so it must be the real one.
#   octopus  — more than two parents; merge-tree takes exactly two, so the check
#              cannot recompute it and will not guess.
#   conflict — recomputation conflicts, which says nothing about what the
#              committer resolved by hand.
#   differs  — recomputed cleanly, and the merge recorded something else.
merge_content_reason() {
  local m="$1" parents recomputed
  parents=$(git rev-list --parents -n1 "$m")
  parents=${parents#* } # drop the commit's own sha, leaving its parents
  [ "$(wc -w <<<"$parents")" -gt 1 ] || return 0 # not a merge

  if [ "$(wc -w <<<"$parents")" -gt 2 ]; then
    echo octopus
    return 0
  fi
  if ! recomputed=$(git merge-tree --write-tree "${m}^1" "${m}^2" 2>/dev/null); then
    echo conflict
    return 0
  fi
  if [ "$recomputed" != "$(git rev-parse "${m}^{tree}")" ]; then
    echo differs
  fi
}

# Add back any merge commit that introduces content no parent has. Without this,
# a PR consisting of a single merge commit that adds a file reports "nothing to
# check" and passes with no sign-off anywhere in it.
# shellcheck disable=SC2086 # as above
for merge in $(git rev-list --merges "$HEAD_REV" "^${BASE_REV}" $EXCLUDE_BASE_REF); do
  if [ -n "$(merge_content_reason "$merge")" ]; then
    commits="${commits}"$'\n'"${merge}"
  fi
done

if [ -z "${commits//[[:space:]]/}" ]; then
  # Nothing to check. Confirm that really means "contributes nothing" rather
  # than "the range computation missed something", and fail closed if not.
  if [ -n "$EXCLUDE_BASE_REF" ] && git merge-base --is-ancestor "$HEAD_REV" "${EXCLUDE_BASE_REF#^}"; then
    echo "The PR head is already contained in the base branch — nothing to check."
    exit 0
  fi
  if ! git diff --quiet "$BASE_REV" "$HEAD_REV"; then
    echo "::error::This pull request changes files against '${BASE_REV}', but the only commits it adds are merge commits that carry no content of their own. Nothing in it is covered by a sign-off." >&2
    exit 1
  fi
  echo "No commits contributed by this PR — nothing to check."
  exit 0
fi

# Sign-off lines on a commit, verbatim. Anchored on the `Signed-off-by:` key so
# sibling trailers (`Co-Authored-By:`, `Reviewed-by:`) can never satisfy the
# check, and matched over the whole message rather than only git's trailer block
# so a sign-off followed by prose still counts.
signoff_lines() {
  git show -s --format='%B' "$1" |
    tr -d '\r' |
    awk 'tolower($0) ~ /^[ \t]*signed-off-by:/ { print }'
}

# Strip carriage returns and other C0 control characters (tab kept) from git
# output before it is echoed.
#
# Commit subjects, author names, and emails are contributor-controlled on a fork
# pull request, and this script's output is read by a human deciding whether a
# gate really failed. A subject like `real subject<CR>::notice::DCO check passed`
# renders in any viewer that honours CR as just the tail — the text after the CR
# overwrites what came before — so a failing commit can be made to look like a
# passing annotation. Backspace does the same by erasing.
#
# This is display deception, not workflow-command injection: the Actions log
# parser splits on newline, so a CR does not begin a new line as far as it is
# concerned, and git forbids newlines in ident fields. Worth fixing anyway,
# because the person reading the log is the control. None of these characters
# belongs in a trailer or an identity, so dropping them loses nothing.
sanitize() { tr -d '\r' | tr -d '\000-\010\013\014\016-\037'; }

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
failed_merges=""
echo "::error::${failed_count} of ${total} commit(s) in this pull request are missing a Developer Certificate of Origin sign-off."
echo
while IFS= read -r sha; do
  [ -n "$sha" ] || continue
  echo "  $(git show -s --format='%h %s' "$sha" | sanitize)"
  echo "    author:    $(git show -s --format='%an <%ae>' "$sha" | sanitize)"
  echo "    committer: $(git show -s --format='%cn <%ce>' "$sha" | sanitize)"
  reason=$(merge_content_reason "$sha")
  [ -z "$reason" ] || failed_merges=yes # decides which bulk fix is safe to print
  case "$reason" in
    octopus)
      echo "    this is an octopus merge; the check cannot recompute a merge of more"
      echo "    than two parents, so it needs a sign-off of its own — redo it with"
      echo "    \`git merge --signoff\`"
      ;;
    conflict)
      echo "    re-merging this commit's parents conflicts, so the check cannot tell"
      echo "    what you resolved by hand. A resolution is a contribution — redo the"
      echo "    merge with \`git merge --signoff\`"
      ;;
    differs)
      echo "    this merge records a result that re-merging its parents does not"
      echo "    reproduce, so it contributes something of its own and needs a"
      echo "    sign-off — redo it with \`git merge --signoff\`"
      ;;
  esac
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
EOF

# The bulk fix is `git rebase --signoff`, but rebase FLATTENS merge commits: it
# replays their parents' commits linearly and throws the merge away, taking any
# conflict resolution with it. Measured on a branch with one conflict-resolution
# merge: 5 commits and the resolution before, 2 commits and conflict markers
# after, worktree left mid-rebase. So the moment a merge is among the offenders,
# that advice is destructive and must not be printed. Note this is true of any
# upstream argument — `HEAD~N` and the base sha are equally unsafe here.
if [ -n "$failed_merges" ]; then
  cat <<EOF

Do NOT run \`git rebase --signoff\` on this branch. Merge commits are among the
commits listed above, and rebase would replay them as ordinary commits, throwing
away the merge and any conflict resolution in it.

  # a merge commit at the tip of your branch
  git commit --amend -s --no-edit && git push --force-with-lease

  # otherwise, re-create the merge so git signs it
  git merge --signoff <the branch you merged>
EOF
else
  cat <<EOF

  # every commit this PR adds
  git rebase --signoff ${BASE_REV} && git push --force-with-lease
EOF
fi

cat <<'EOF'

If the email above is not the one you meant to sign off with, set
`git config user.email` first, then re-run. Adding the line certifies the
Developer Certificate of Origin: https://developercertificate.org/ — see
.github/CONTRIBUTING.md#developer-certificate-of-origin.
EOF
exit 1
