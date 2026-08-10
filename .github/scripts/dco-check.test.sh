#!/usr/bin/env bash
#
# Self-test for dco-check.sh. Builds throwaway git repositories, runs the real
# check against them, and asserts the exit code and output.
#
# Run manually:  .github/scripts/dco-check.test.sh
# Runs in CI as a step of the `dco` job in .github/workflows/dco.yaml, so the
# gate's logic is verified on every PR rather than trusted.
#
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
DCO_CHECK="${SCRIPT_DIR}/dco-check.sh"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

pass_count=0
fail_count=0

# Deterministic identities. Nothing here reads the developer's git config.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
ALICE_NAME="Alice Example"; ALICE_EMAIL="alice@example.com"
BOB_NAME="Bob Example";     BOB_EMAIL="bob@example.com"

# new_repo <name> — a repo with one signed base commit on main, plus an
# origin/main remote-tracking ref pointing at it. Echoes the repo path.
new_repo() {
  local dir="$WORK/$1"
  mkdir -p "$dir"
  git -C "$dir" init -q -b main
  git -C "$dir" config user.name "$ALICE_NAME"
  git -C "$dir" config user.email "$ALICE_EMAIL"
  git -C "$dir" commit -q --allow-empty -m "base

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
  git -C "$dir" update-ref refs/remotes/origin/main "$(git -C "$dir" rev-parse HEAD)"
  printf '%s' "$dir"
}

# commit <repo> <message> [author_name] [author_email]
commit() {
  local dir="$1" msg="$2" name="${3:-$ALICE_NAME}" email="${4:-$ALICE_EMAIL}"
  GIT_AUTHOR_NAME="$name" GIT_AUTHOR_EMAIL="$email" \
    git -C "$dir" commit -q --allow-empty -m "$msg"
}

# content_commit <repo> <file> <message> — a commit that actually changes a file,
# so merge behaviour can be exercised with real content rather than empty trees.
content_commit() {
  local dir="$1" file="$2" msg="$3"
  echo "$msg" >>"$dir/$file"
  git -C "$dir" add "$file"
  git -C "$dir" commit -q -m "$msg"
}

# expect <want_exit> <case name> <repo> [output_substring]
expect() {
  local want="$1" name="$2" dir="$3" needle="${4:-}"
  local out rc=0
  out=$(
    cd "$dir" &&
    BASE_SHA="$(git rev-parse refs/remotes/origin/main)" \
    BASE_REF="main" \
    HEAD_SHA="$(git rev-parse HEAD)" \
    "$DCO_CHECK" 2>&1
  ) || rc=$?

  if [ "$rc" -ne "$want" ]; then
    printf '::error::SELF-TEST FAILED: %s — expected exit %s, got %s. This is a defect in the DCO check itself, not a problem with your commits.\n' "$name" "$want" "$rc"
    printf '%s\n' "$out" | sed 's/^/    | /'
    fail_count=$((fail_count + 1))
    return
  fi
  if [ -n "$needle" ] && ! grep -qF -- "$needle" <<<"$out"; then
    printf '::error::SELF-TEST FAILED: %s — output missing %q. This is a defect in the DCO check itself, not a problem with your commits.\n' "$name" "$needle"
    printf '%s\n' "$out" | sed 's/^/    | /'
    fail_count=$((fail_count + 1))
    return
  fi
  printf 'ok  %s\n' "$name"
  pass_count=$((pass_count + 1))
}

# --- 1. signed commit passes -------------------------------------------------
r=$(new_repo signed)
commit "$r" "add a thing

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
expect 0 "signed commit passes" "$r" "signed off — OK"

# --- 2. unsigned commit fails ------------------------------------------------
r=$(new_repo unsigned)
commit "$r" "add a thing"
expect 1 "unsigned commit fails" "$r" "no Signed-off-by line found"
# A plain commit is not a merge, so none of the merge diagnostics may appear.
# Weakening the not-a-merge guard in merge_content_reason otherwise tells someone
# with an ordinary unsigned commit that re-merging its parents conflicts.
out=$(cd "$r" && BASE_SHA="$(git rev-parse refs/remotes/origin/main)" BASE_REF=main \
  HEAD_SHA="$(git rev-parse HEAD)" "$DCO_CHECK" 2>&1) || true
if grep -qE "re-merging|octopus|merge --signoff" <<<"$out"; then
  printf '::error::SELF-TEST FAILED: a plain commit was described with merge wording.\n'
  printf '%s\n' "$out" | sed 's/^/    | /'
  fail_count=$((fail_count + 1))
else
  printf 'ok  %s\n' "a plain commit is never described as a merge"
  pass_count=$((pass_count + 1))
fi

# --- 3. one unsigned commit among signed ones fails --------------------------
r=$(new_repo mixed)
commit "$r" "first

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
commit "$r" "second, forgot to sign"
commit "$r" "third

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
expect 1 "one unsigned among signed fails" "$r" "1 of 3 commit(s)"

# --- 4. sign-off belonging to someone else fails (cherry-pick hole) ----------
r=$(new_repo wrong_signer)
commit "$r" "copied from upstream

Signed-off-by: $BOB_NAME <$BOB_EMAIL>"
expect 1 "sign-off not matching the author fails" "$r" "no email matches the author or committer"

# --- 5. Co-Authored-By alone does not satisfy the check ----------------------
r=$(new_repo coauthor_only)
commit "$r" "pair work

Co-Authored-By: $ALICE_NAME <$ALICE_EMAIL>"
expect 1 "Co-Authored-By alone fails" "$r" "no Signed-off-by line found"

# --- 6. Signed-off-by alongside Co-Authored-By passes -----------------------
r=$(new_repo coauthor_plus_signoff)
commit "$r" "pair work

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>
Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
expect 0 "Signed-off-by with Co-Authored-By passes" "$r" "signed off — OK"

# --- 7. malformed sign-off (no email) fails ---------------------------------
r=$(new_repo malformed_no_email)
commit "$r" "add a thing

Signed-off-by: $ALICE_NAME"
expect 1 "sign-off without an email fails" "$r"

# --- 8. malformed sign-off (no colon) fails ---------------------------------
r=$(new_repo malformed_no_colon)
commit "$r" "add a thing

Signed-off-by $ALICE_NAME <$ALICE_EMAIL>"
expect 1 "sign-off without a colon fails" "$r" "no Signed-off-by line found"

# --- 9. email case differences still match ----------------------------------
r=$(new_repo case_insensitive)
commit "$r" "add a thing

signed-off-by: $ALICE_NAME <ALICE@EXAMPLE.COM>"
expect 0 "case-insensitive key and email match" "$r" "signed off — OK"

# --- 10. empty commit still requires a sign-off ------------------------------
r=$(new_repo empty_commit)
commit "$r" "trigger ci"
expect 1 "empty commit still needs a sign-off" "$r" "1 of 1 commit(s)"

# --- 11. sign-off matching the committer (rebase --signoff) passes ----------
r=$(new_repo committer_match)
GIT_AUTHOR_NAME="$BOB_NAME" GIT_AUTHOR_EMAIL="$BOB_EMAIL" \
  git -C "$r" commit -q --allow-empty -m "forwarded work

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
expect 0 "sign-off matching the committer passes" "$r" "signed off — OK"

# --- 12. an unsigned merge commit is exempt, its branch commits are not -----
r=$(new_repo merge_exempt)
git -C "$r" checkout -q -b feature
commit "$r" "feature work

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
git -C "$r" checkout -q main
commit "$r" "main work

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
git -C "$r" merge -q --no-ff -m "Merge branch 'feature'" feature
expect 0 "unsigned merge commit is exempt" "$r" "signed off — OK"

r=$(new_repo merge_does_not_shield)
git -C "$r" checkout -q -b feature
commit "$r" "feature work, unsigned"
git -C "$r" checkout -q main
commit "$r" "main work

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
git -C "$r" merge -q --no-ff -m "Merge branch 'feature'" feature
expect 1 "a merge does not shield unsigned commits it brings in" "$r" "feature work, unsigned"

# --- 13. base-branch commits merged into the PR are not attributed to it ----
# origin/main advances past BASE_SHA with an unsigned commit, the contributor
# merges it in. That commit is not this PR's contribution, so it must not fail.
r=$(new_repo merged_in_base)
base=$(git -C "$r" rev-parse HEAD)
git -C "$r" checkout -q -b feature
commit "$r" "my signed work

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
git -C "$r" checkout -q main
commit "$r" "someone else's unsigned commit on main"
git -C "$r" update-ref refs/remotes/origin/main "$(git -C "$r" rev-parse HEAD)"
git -C "$r" checkout -q feature
git -C "$r" merge -q --no-ff -m "Merge branch 'main' into feature" main
out=$(
  cd "$r" &&
  BASE_SHA="$base" BASE_REF="main" HEAD_SHA="$(git rev-parse HEAD)" "$DCO_CHECK" 2>&1
) && rc=0 || rc=$?
if [ "$rc" -eq 0 ] && grep -qF "all 1 commit(s)" <<<"$out"; then
  printf 'ok  %s\n' "base-branch commits merged in are not attributed to the PR"
  pass_count=$((pass_count + 1))
else
  printf 'SELF-TEST FAILED: base-branch commits merged in are not attributed to the PR (exit %s)\n' "$rc"
  printf '%s\n' "$out" | sed 's/^/    | /'
  fail_count=$((fail_count + 1))
fi

# --- 13b. an "evil merge" cannot smuggle content past the gate ---------------
# A merge commit can carry content no parent has. If merges were exempt
# unconditionally, a PR consisting of one such merge would report "nothing to
# check" and pass with no sign-off anywhere in it.
evil_merge_repo() {
  local r; r=$(new_repo "$1")
  local base; base=$(git -C "$r" rev-parse HEAD)
  content_commit "$r" f.txt "main moves

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
  git -C "$r" update-ref refs/remotes/origin/main "$(git -C "$r" rev-parse HEAD)"
  git -C "$r" checkout -q -b feature "$base"
  printf '%s' "$r"
}

r=$(evil_merge_repo evil_merge_alone)
git -C "$r" merge -q --no-commit --no-ff main >/dev/null 2>&1 || true
echo BACKDOOR >"$r/backdoor.txt"; git -C "$r" add backdoor.txt
git -C "$r" commit -q -m "Merge branch 'main'"
expect 1 "a merge commit cannot smuggle in unsigned content" "$r" "re-merging its parents does not"

r=$(evil_merge_repo evil_merge_beside_signed)
content_commit "$r" g.txt "honest signed work

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
git -C "$r" merge -q --no-commit --no-ff main >/dev/null 2>&1 || true
echo BACKDOOR >"$r/backdoor.txt"; git -C "$r" add backdoor.txt
git -C "$r" commit -q -m "Merge branch 'main'"
expect 1 "an evil merge beside a signed commit still fails" "$r" "re-merging its parents does not"

r=$(evil_merge_repo evil_merge_signed)
git -C "$r" merge -q --no-commit --no-ff main >/dev/null 2>&1 || true
echo CONTENT >"$r/resolved.txt"; git -C "$r" add resolved.txt
git -C "$r" commit -q -m "Merge branch 'main'

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
expect 0 "a signed content-bearing merge passes" "$r" "signed off — OK"

r=$(evil_merge_repo clean_merge_with_content)
content_commit "$r" g.txt "honest signed work

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
git -C "$r" merge -q --no-ff -m "Merge branch 'main' into feature" main
expect 0 "an ordinary clean merge of the base branch stays exempt" "$r" "signed off — OK"

# --- 13c. bots get no exemption ----------------------------------------------
# An author-name exemption would be forgeable with `git commit --author`, so
# there must not be one. Renovate signs off via renovate.json commitBody.
r=$(new_repo bot_no_exemption)
commit "$r" "chore(deps): bump the pinned CLI" \
  "renovate[bot]" "29139614+renovate[bot]@users.noreply.github.com"
expect 1 "an unsigned bot commit is not exempt" "$r" "no Signed-off-by line found"

r=$(new_repo bot_signed)
commit "$r" "chore(deps): bump the pinned CLI

Signed-off-by: renovate[bot] <29139614+renovate[bot]@users.noreply.github.com>" \
  "renovate[bot]" "29139614+renovate[bot]@users.noreply.github.com"
expect 0 "a signed bot commit passes" "$r" "signed off — OK"

# --- 13d. the email comparison is equality, not substring --------------------
r=$(new_repo substring_email)
commit "$r" "add a thing

Signed-off-by: $ALICE_NAME <example.com>"
expect 1 "a sign-off email that is only a substring fails" "$r" "no email matches"

# --- 13e. any one matching sign-off is enough --------------------------------
r=$(new_repo second_signoff_matches)
commit "$r" "forwarded work

Signed-off-by: $BOB_NAME <$BOB_EMAIL>
Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
expect 0 "a later matching sign-off is accepted" "$r" "signed off — OK"

# --- 13f. a sign-off must start its own line, not appear inside prose --------
r=$(new_repo signoff_inside_prose)
commit "$r" "add a thing

I keep forgetting the Signed-off-by: $ALICE_NAME <$ALICE_EMAIL> line."
expect 1 "a sign-off mentioned mid-sentence does not count" "$r" "no Signed-off-by line found"

# --- 13g. `git merge -s ours` is a contribution, not a free pass -------------
# -s ours records the current branch's tree and discards the other parent's
# changes. `git diff-tree --cc` reports nothing for it, which is why this check
# recomputes the merge instead.
r=$(evil_merge_repo ours_merge_alone)
git -C "$r" merge -q -s ours --no-ff -m "Merge branch 'main' into feature" main
expect 1 "a -s ours merge needs its own sign-off" "$r" "re-merging its parents does not"

# --- 13h. a genuine octopus merge always needs a sign-off --------------------
# merge-tree takes exactly two parents, so the check cannot recompute an octopus
# and refuses to guess. The fixture asserts three parents: merging from `main`
# would fast-forward to the first branch and quietly produce a 2-parent commit,
# which would make this a duplicate of 13b rather than octopus coverage.
octopus_repo() {
  local r; r=$(new_repo "$1")
  local base; base=$(git -C "$r" rev-parse HEAD)
  local b
  for b in oct1 oct2; do
    git -C "$r" checkout -q -b "$b" "$base"
    content_commit "$r" "$b.txt" "$b

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
  done
  git -C "$r" checkout -q -b feature "$base"
  content_commit "$r" feature.txt "feature work

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
  printf '%s' "$r"
}

assert_parents() {
  local r="$1" want="$2" got
  got=$(git -C "$r" rev-list --parents -n1 HEAD | wc -w | tr -d ' ')
  got=$((got - 1))
  [ "$got" -eq "$want" ] && return 0
  printf '::error::SELF-TEST FAILED: fixture built %s parents, expected %s — the case no longer tests what it claims.\n' "$got" "$want"
  fail_count=$((fail_count + 1))
  return 1
}

r=$(octopus_repo octopus_clean)
git -C "$r" merge -q --no-ff -m "Octopus merge" oct1 oct2 >/dev/null 2>&1
if assert_parents "$r" 3; then
  expect 1 "an unsigned octopus merge fails even when clean" "$r" "octopus merge"
fi

r=$(octopus_repo octopus_signed)
git -C "$r" merge -q --no-ff -m "Octopus merge

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>" oct1 oct2 >/dev/null 2>&1
if assert_parents "$r" 3; then
  expect 0 "a signed octopus merge passes" "$r" "signed off — OK"
fi

# --- 13h2. a conflict resolution is a contribution ---------------------------
# The most-travelled path in practice: merge main, hit a conflict, resolve it.
# Re-merging the parents conflicts, so the check cannot see what was resolved by
# hand and must require a sign-off.
conflict_merge_repo() {
  local r; r=$(new_repo "$1")
  local base; base=$(git -C "$r" rev-parse HEAD)
  printf 'l1\nl2\nl3\n' >"$r/c.txt"
  git -C "$r" add c.txt
  git -C "$r" commit -q -m "seed c.txt

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
  git -C "$r" update-ref refs/remotes/origin/main "$(git -C "$r" rev-parse HEAD)"
  git -C "$r" checkout -q -b feature
  printf 'l1\nFEATURE\nl3\n' >"$r/c.txt"
  git -C "$r" commit -q -am "feature edit

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
  git -C "$r" checkout -q main
  printf 'l1\nMAIN\nl3\n' >"$r/c.txt"
  git -C "$r" commit -q -am "main edit

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
  git -C "$r" update-ref refs/remotes/origin/main "$(git -C "$r" rev-parse HEAD)"
  git -C "$r" checkout -q feature
  git -C "$r" merge --no-ff main >/dev/null 2>&1 || true
  printf 'l1\nRESOLVED-BY-HAND\nl3\n' >"$r/c.txt"
  git -C "$r" add c.txt
  printf '%s' "$r"
}

r=$(conflict_merge_repo conflict_unsigned)
git -C "$r" commit -q -m "Merge branch 'main' into feature"
expect 1 "an unsigned conflict-resolution merge fails" "$r" "conflicts, so the check cannot tell"

# The rebase advice must NOT be printed when a merge is among the offenders:
# `git rebase --signoff` replays a merge's parents linearly and discards both the
# merge and any conflict resolution in it.
out=$(cd "$r" && BASE_SHA="$(git rev-parse refs/remotes/origin/main)" BASE_REF=main \
  HEAD_SHA="$(git rev-parse HEAD)" "$DCO_CHECK" 2>&1) || true
# Match the command block (indented, at line start), not the word "rebase" in the
# warning prose that must also be present.
if grep -qE '^[[:space:]]+git rebase --signoff' <<<"$out"; then
  printf '::error::SELF-TEST FAILED: destructive rebase advice printed for a merge offender.\n'
  printf '%s\n' "$out" | sed 's/^/    | /'
  fail_count=$((fail_count + 1))
elif ! grep -qF "Do NOT run" <<<"$out"; then
  printf '::error::SELF-TEST FAILED: no warning against rebasing a branch containing merges.\n'
  printf '%s\n' "$out" | sed 's/^/    | /'
  fail_count=$((fail_count + 1))
else
  printf 'ok  %s\n' "no rebase advice when an offending commit is a merge"
  pass_count=$((pass_count + 1))
fi

# ...and it must be printed, against the base rather than a HEAD~N the
# contributor has to count, when every offender is an ordinary commit.
r2=$(new_repo linear_rebase_advice)
commit "$r2" "unsigned work"
out=$(cd "$r2" && BASE_SHA="$(git rev-parse refs/remotes/origin/main)" BASE_REF=main \
  HEAD_SHA="$(git rev-parse HEAD)" "$DCO_CHECK" 2>&1) || true
if grep -qF "rebase --signoff $(git -C "$r2" rev-parse refs/remotes/origin/main)" <<<"$out"; then
  printf 'ok  %s\n' "linear branches get rebase advice naming the base commit"
  pass_count=$((pass_count + 1))
else
  printf '::error::SELF-TEST FAILED: linear branches get rebase advice naming the base commit.\n'
  printf '%s\n' "$out" | sed 's/^/    | /'
  fail_count=$((fail_count + 1))
fi

r=$(conflict_merge_repo conflict_signed)
git -C "$r" commit -q -m "Merge branch 'main' into feature

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
expect 0 "a signed conflict-resolution merge passes" "$r" "signed off — OK"

# --- 13h3. base-branch merges are not attributed to the PR -------------------
# main carries content-bearing merges of its own (this repo's main has 14 that
# nobody signed off). BASE_SHA is a snapshot from when the event fired, so a
# merge that landed on main afterwards is excluded only by origin/<base>. Drop
# that exclusion from the merge re-add loop and every PR refreshing from main
# fails, citing somebody else's merge commit. The base snapshot below is
# deliberately older than the merge so origin/main is the only thing excluding it.
r=$(conflict_merge_repo base_merge_not_attributed)
git -C "$r" commit -q -m "Merge branch 'main' into feature"   # unsigned, on purpose
merge_on_main=$(git -C "$r" rev-parse HEAD)
snapshot=$(git -C "$r" rev-parse HEAD~1)                      # base.sha as captured earlier
git -C "$r" checkout -q -B main "$merge_on_main"
git -C "$r" update-ref refs/remotes/origin/main "$merge_on_main"
git -C "$r" checkout -q -b later "$snapshot"
content_commit "$r" mine.txt "my own signed work

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
git -C "$r" merge -q --no-ff -m "Merge branch 'main' into later

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>" main >/dev/null 2>&1
out=$(cd "$r" && BASE_SHA="$snapshot" BASE_REF="main" HEAD_SHA="$(git rev-parse HEAD)" \
  "$DCO_CHECK" 2>&1) && rc=0 || rc=$?
if [ "$rc" -eq 0 ]; then
  printf 'ok  %s\n' "an unsigned content-bearing merge already on the base branch is not attributed to the PR"
  pass_count=$((pass_count + 1))
else
  printf '::error::SELF-TEST FAILED: an unsigned content-bearing merge already on the base branch is not attributed to the PR (exit %s)\n' "$rc"
  printf '%s\n' "$out" | sed 's/^/    | /'
  fail_count=$((fail_count + 1))
fi

# --- 13i. a head already contained in the base branch passes -----------------
# Re-running the check after the PR merged must not turn red. The head is left
# strictly BEHIND origin/main so the trees differ: with the head equal to the
# base tip the diff would be empty and the backstop below would be satisfied
# either way, which would stop this case from pinning the ancestry direction.
r=$(new_repo already_merged)
old=$(git -C "$r" rev-parse HEAD)
content_commit "$r" f.txt "landed work

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
newer=$(git -C "$r" rev-parse HEAD)
git -C "$r" update-ref refs/remotes/origin/main "$newer"
out=$(cd "$r" && BASE_SHA="$newer" BASE_REF="main" HEAD_SHA="$old" "$DCO_CHECK" 2>&1) && rc=0 || rc=$?
if [ "$rc" -eq 0 ] && grep -qF "already contained in the base branch" <<<"$out"; then
  printf 'ok  %s\n' "a head already contained in the base branch passes"
  pass_count=$((pass_count + 1))
else
  printf '::error::SELF-TEST FAILED: a head already contained in the base branch passes (exit %s)\n' "$rc"
  printf '%s\n' "$out" | sed 's/^/    | /'
  fail_count=$((fail_count + 1))
fi

# --- 13j. an unresolvable BASE_SHA errors ------------------------------------
r=$(new_repo bad_base)
out=$(cd "$r" && BASE_SHA="0000000000000000000000000000000000000000" BASE_REF="main" \
  HEAD_SHA="$(git rev-parse HEAD)" "$DCO_CHECK" 2>&1) && rc=0 || rc=$?
if [ "$rc" -eq 1 ] && grep -qF "cannot resolve" <<<"$out"; then
  printf 'ok  %s\n' "an unresolvable base errors instead of passing"
  pass_count=$((pass_count + 1))
else
  printf '::error::SELF-TEST FAILED: an unresolvable base errors instead of passing (exit %s)\n' "$rc"
  printf '%s\n' "$out" | sed 's/^/    | /'
  fail_count=$((fail_count + 1))
fi

# --- 13k. the empty-range backstop fails closed ------------------------------
# Range empty, no origin/<base> to fall back on, yet the head's tree differs from
# the base. Contrived, because with merge-tree in place the earlier layers catch
# the realistic cases — that is exactly why this last line of defence needs a
# test of its own.
r=$(new_repo empty_range_backstop)
old=$(git -C "$r" rev-parse HEAD)
content_commit "$r" f.txt "newer base commit

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
newer=$(git -C "$r" rev-parse HEAD)
out=$(cd "$r" && BASE_SHA="$newer" BASE_REF="no-such-branch" HEAD_SHA="$old" \
  "$DCO_CHECK" 2>&1) && rc=0 || rc=$?
if [ "$rc" -eq 1 ] && grep -qF "carry no content of their own" <<<"$out"; then
  printf 'ok  %s\n' "the empty-range backstop fails closed"
  pass_count=$((pass_count + 1))
else
  printf '::error::SELF-TEST FAILED: the empty-range backstop fails closed (exit %s)\n' "$rc"
  printf '%s\n' "$out" | sed 's/^/    | /'
  fail_count=$((fail_count + 1))
fi

# --- 14. an empty range passes -----------------------------------------------
r=$(new_repo empty_range)
expect 0 "empty commit range passes" "$r" "nothing to check"

# --- 15. an unresolvable head errors rather than passing silently ------------
r=$(new_repo bad_head)
out=$(cd "$r" && BASE_SHA="$(git rev-parse HEAD)" BASE_REF="main" \
  HEAD_SHA="0000000000000000000000000000000000000000" "$DCO_CHECK" 2>&1) && rc=0 || rc=$?
if [ "$rc" -eq 1 ] && grep -qF "cannot resolve" <<<"$out"; then
  printf 'ok  %s\n' "unresolvable head errors instead of passing"
  pass_count=$((pass_count + 1))
else
  printf 'SELF-TEST FAILED: unresolvable head errors instead of passing (exit %s)\n' "$rc"
  printf '%s\n' "$out" | sed 's/^/    | /'
  fail_count=$((fail_count + 1))
fi

# --- 16. the failure message tells a contributor how to fix it --------------
r=$(new_repo actionable_message)
commit "$r" "add a thing"
expect 1 "failure output names the fix commands" "$r" "git commit --amend -s --no-edit"
expect 1 "failure output names the rebase fix" "$r" "git rebase --signoff"

# --- 17. contributor-controlled commit text cannot fake the log -------------
# The subject, author name, and email all come from the contributor on a fork
# pull request. A CR makes everything before it vanish in any viewer that
# honours it, so `real subject<CR>::notice::DCO check passed` renders as a
# passing annotation on a commit that just failed. Backspace does the same by
# erasing. Assert neither reaches the output.
#
# Display deception, not workflow-command injection: the Actions log parser
# splits on newline, so a CR does not start a line as far as it is concerned,
# and git forbids newlines in ident fields. The reader is the control here.
r=$(new_repo crlf_injection)
GIT_AUTHOR_NAME="Mallory$(printf '\010\010\010')Alice" \
GIT_AUTHOR_EMAIL="mallory@example.com" \
  git -C "$r" commit -q --allow-empty \
    -m "$(printf 'subject\r::notice::DCO check passed')"

out=$(
  cd "$r" &&
  BASE_SHA="$(git rev-parse refs/remotes/origin/main)" \
  BASE_REF="main" \
  HEAD_SHA="$(git rev-parse HEAD)" \
  "$DCO_CHECK" 2>&1
) || true
if printf '%s' "$out" | grep -q "$(printf '\r')"; then
  printf 'FAIL  carriage return survives into the log output\n'
  fail_count=$((fail_count + 1))
else
  printf 'ok  carriage returns are stripped from commit text\n'
  pass_count=$((pass_count + 1))
fi
if printf '%s' "$out" | grep -q "$(printf '\010')"; then
  printf 'FAIL  backspace survives into the log output\n'
  fail_count=$((fail_count + 1))
else
  printf 'ok  other C0 control characters are stripped\n'
  pass_count=$((pass_count + 1))
fi
# The commit must still be reported as failing — sanitizing must not swallow it.
if printf '%s' "$out" | grep -q 'missing a Developer Certificate of Origin sign-off'; then
  printf 'ok  the offending commit is still reported\n'
  pass_count=$((pass_count + 1))
else
  printf 'FAIL  sanitizing lost the failure report\n'
  fail_count=$((fail_count + 1))
fi
unset GIT_AUTHOR_NAME GIT_AUTHOR_EMAIL

# --- 18. author-identity denylist --------------------------------------------
# Second control on the same walk: reject the non-routable email shapes a
# developer machine invents when `git config user.email` is unset — bare
# hostnames, `.local`, `.sol-local`, `.internal`, `.lan`. A denylist rather than
# an allowlist so ordinary external addresses can still contribute. See the
# script header for the reasoning.

# .sol-local author fails — the exact shape SOL-152902 caught in this repo
r=$(new_repo identity_sol_local_fails)
GIT_AUTHOR_NAME="$ALICE_NAME" GIT_AUTHOR_EMAIL="alice@buildbox.sol-local" \
  git -C "$r" commit -q --allow-empty -m "add a thing

Signed-off-by: $ALICE_NAME <alice@buildbox.sol-local>"
expect 1 "sol-local author identity fails" "$r" "not routable"

# .local author fails — matches the machine-name pattern that leaked a person's name
r=$(new_repo identity_dotlocal_fails)
GIT_AUTHOR_NAME="$ALICE_NAME" GIT_AUTHOR_EMAIL="alice@alices-laptop.local" \
  git -C "$r" commit -q --allow-empty -m "add a thing

Signed-off-by: $ALICE_NAME <alice@alices-laptop.local>"
expect 1 "local author identity fails" "$r" "not routable"

# .internal and .lan also fail
r=$(new_repo identity_internal_fails)
GIT_AUTHOR_NAME="$ALICE_NAME" GIT_AUTHOR_EMAIL="alice@host.internal" \
  git -C "$r" commit -q --allow-empty -m "add a thing

Signed-off-by: $ALICE_NAME <alice@host.internal>"
expect 1 "internal author identity fails" "$r" "not routable"

r=$(new_repo identity_lan_fails)
GIT_AUTHOR_NAME="$ALICE_NAME" GIT_AUTHOR_EMAIL="alice@host.lan" \
  git -C "$r" commit -q --allow-empty -m "add a thing

Signed-off-by: $ALICE_NAME <alice@host.lan>"
expect 1 "lan author identity fails" "$r" "not routable"

# no-dot domain (bare hostname) fails
r=$(new_repo identity_no_dot_fails)
GIT_AUTHOR_NAME="$ALICE_NAME" GIT_AUTHOR_EMAIL="alice@buildbox" \
  git -C "$r" commit -q --allow-empty -m "add a thing

Signed-off-by: $ALICE_NAME <alice@buildbox>"
expect 1 "bare-hostname author identity fails" "$r" "not routable"

# outside domain passes — this is what denylist buys us over allowlist
r=$(new_repo identity_outside_ok)
commit "$r" "add a thing

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
expect 0 "external routable domain (example.com) passes" "$r" "signed off — OK"

# users.noreply.github.com passes — the shape outside contributors and bots use
r=$(new_repo identity_ghnoreply_ok)
GIT_AUTHOR_NAME="$ALICE_NAME" GIT_AUTHOR_EMAIL="12345+alice@users.noreply.github.com" \
  git -C "$r" commit -q --allow-empty -m "add a thing

Signed-off-by: $ALICE_NAME <12345+alice@users.noreply.github.com>"
expect 0 "users.noreply.github.com passes" "$r" "signed off — OK"

# committer domain also checked: author is routable, committer is .local
r=$(new_repo identity_committer_checked)
GIT_COMMITTER_NAME="$BOB_NAME" GIT_COMMITTER_EMAIL="bob@laptop.local" \
GIT_AUTHOR_NAME="$ALICE_NAME" GIT_AUTHOR_EMAIL="$ALICE_EMAIL" \
  git -C "$r" commit -q --allow-empty -m "forwarded work

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
expect 1 "bad committer identity fails even with a clean author" "$r" "not routable"
unset GIT_COMMITTER_NAME GIT_COMMITTER_EMAIL

# fix advice appears in the failure output
r=$(new_repo identity_fix_advice)
GIT_AUTHOR_NAME="$ALICE_NAME" GIT_AUTHOR_EMAIL="alice@buildbox.sol-local" \
  git -C "$r" commit -q --allow-empty -m "add a thing

Signed-off-by: $ALICE_NAME <alice@buildbox.sol-local>"
expect 1 "identity failure output names the fix commands" "$r" 'git config user.email "you@your-domain.example"'
expect 1 "identity failure output names --reset-author" "$r" "git commit --amend --reset-author"

# an identity-only failure (sign-off is fine) still fails, and does NOT print
# the DCO header (which would mislead the contributor about what is wrong).
r=$(new_repo identity_only_no_dco_header)
GIT_AUTHOR_NAME="$ALICE_NAME" GIT_AUTHOR_EMAIL="alice@buildbox.sol-local" \
  git -C "$r" commit -q --allow-empty -m "add a thing

Signed-off-by: $ALICE_NAME <alice@buildbox.sol-local>"
out=$(cd "$r" && BASE_SHA="$(git rev-parse refs/remotes/origin/main)" BASE_REF=main \
  HEAD_SHA="$(git rev-parse HEAD)" "$DCO_CHECK" 2>&1) || true
if grep -qF "missing a Developer Certificate of Origin sign-off" <<<"$out"; then
  printf '::error::SELF-TEST FAILED: identity-only failure printed the DCO-missing header.\n'
  printf '%s\n' "$out" | sed 's/^/    | /'
  fail_count=$((fail_count + 1))
else
  printf 'ok  identity-only failure does not print the DCO header\n'
  pass_count=$((pass_count + 1))
fi

# --- 18b. the DCO merge exemption does not extend to the identity check ------
# A content-free merge is exempt from needing a sign-off, but it still carries
# author and committer fields and still publishes them. `git merge main` is the
# one merge shape CONTRIBUTING.md tells contributors to use, so a leak riding in
# on it would slip through the gate entirely. Pins the separate `identity_commits`
# range in dco-check.sh: recouple it to `$commits` and this case fails.
r=$(new_repo identity_merge_not_exempt)
git -C "$r" checkout -q -b feature
content_commit "$r" feat.txt "feature work

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
git -C "$r" checkout -q main
content_commit "$r" main.txt "main work

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
git -C "$r" update-ref refs/remotes/origin/main "$(git -C "$r" rev-parse HEAD)"
git -C "$r" checkout -q feature
GIT_AUTHOR_NAME="$ALICE_NAME"    GIT_AUTHOR_EMAIL="alice@laptop.local" \
GIT_COMMITTER_NAME="$ALICE_NAME" GIT_COMMITTER_EMAIL="alice@laptop.local" \
  git -C "$r" merge -q --no-ff -m "Merge branch 'main' into feature" main
if assert_parents "$r" 2; then
  expect 1 "a content-free merge's identity is still checked" "$r" "not routable"
fi

# --- 19. the denylist and the message it prints cannot drift apart -----------
# The suffix set is written twice in dco-check.sh: once as the `case` pattern
# that decides, once as prose in the ::error:: line a blocked contributor reads.
# Rather than collapse them into a shared variable (bash `case` cannot take
# alternation from one), read the deciding pattern out of the script and assert
# the message keeps up. Adding a suffix to the `case` and forgetting the message
# fails here instead of in a contributor's CI log, where it would list the
# suffixes they were NOT rejected on.
denylist_line=$(grep -m1 -E '^[[:space:]]*\*\.[a-z-]+(\|\*\.[a-z-]+)*\)[[:space:]]*return 1' "$DCO_CHECK" || true)
denylist_line=${denylist_line%%)*}
IFS='|' read -r -a denylist_pats <<<"$(tr -d '[:space:]' <<<"$denylist_line")"

if [ -z "$denylist_line" ] || [ "${#denylist_pats[@]}" -eq 0 ]; then
  printf '::error::SELF-TEST FAILED: could not read the suffix denylist out of %s. If the `case` pattern was reformatted, update this test to match.\n' "$DCO_CHECK"
  fail_count=$((fail_count + 1))
fi

for pat in "${denylist_pats[@]}"; do
  suffix=${pat#\*}   # `*.sol-local` -> `.sol-local`
  r=$(new_repo "identity_denylist${suffix//./_}")
  GIT_AUTHOR_NAME="$ALICE_NAME" GIT_AUTHOR_EMAIL="alice@buildbox${suffix}" \
    git -C "$r" commit -q --allow-empty -m "add a thing

Signed-off-by: $ALICE_NAME <alice@buildbox${suffix}>"
  expect 1 "denylisted suffix ${suffix} is rejected" "$r" "not routable"

  # Match on a boundary, not a substring: a plain search for `.local` would be
  # satisfied by `.sol-local` alone and miss its removal from the message.
  out=$(cd "$r" && BASE_SHA="$(git rev-parse refs/remotes/origin/main)" BASE_REF=main \
    HEAD_SHA="$(git rev-parse HEAD)" "$DCO_CHECK" 2>&1) || true
  if grep -qE "(^|[^a-z.-])${suffix//./\\.}([^a-z.-]|$)" <<<"$out"; then
    printf 'ok  failure output names the %s suffix\n' "$suffix"
    pass_count=$((pass_count + 1))
  else
    printf '::error::SELF-TEST FAILED: `%s` is in the denylist `case` but the failure output never names it.\n' "$suffix"
    printf '%s\n' "$out" | sed 's/^/    | /'
    fail_count=$((fail_count + 1))
  fi
done

echo
printf 'dco-check self-test: %d passed, %d failed\n' "$pass_count" "$fail_count"
[ "$fail_count" -eq 0 ]
