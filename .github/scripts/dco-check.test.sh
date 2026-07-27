#!/usr/bin/env bash
#
# Self-test for dco-check.sh. Builds throwaway git repositories, runs the real
# check against them, and asserts the exit code and output.
#
# Run manually:  .github/scripts/dco-check.test.sh
# Runs in CI as the first step of the `dco` job in .github/workflows/ci-pr.yaml,
# so the gate's logic is verified on every PR rather than trusted.
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
expect 1 "a merge commit cannot smuggle in unsigned content" "$r" "merge commit that adds content"

r=$(evil_merge_repo evil_merge_beside_signed)
content_commit "$r" g.txt "honest signed work

Signed-off-by: $ALICE_NAME <$ALICE_EMAIL>"
git -C "$r" merge -q --no-commit --no-ff main >/dev/null 2>&1 || true
echo BACKDOOR >"$r/backdoor.txt"; git -C "$r" add backdoor.txt
git -C "$r" commit -q -m "Merge branch 'main'"
expect 1 "an evil merge beside a signed commit still fails" "$r" "merge commit that adds content"

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
expect 1 "failure output names the rebase fix" "$r" "git rebase --signoff HEAD~N"

echo
printf 'dco-check self-test: %d passed, %d failed\n' "$pass_count" "$fail_count"
[ "$fail_count" -eq 0 ]
