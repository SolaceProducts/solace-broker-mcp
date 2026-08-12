#!/usr/bin/env bash
# Self-test for commit-identity-check.sh. Builds throwaway git repositories,
# runs the real script against them, and asserts both the pass and the fail
# path. Every failure says why, so a red run is actionable without reading this
# file.
#
# Run manually:  .github/scripts/commit-identity-check.test.sh
# Runs in CI as a step of the `identity` job in .github/workflows/ci-pr.yaml,
# so a change that guts the check goes red rather than silently passing.
#
# The failure mode this guards is a vacuous pass: a check that exits 0 on
# everything looks identical to a check that finds nothing wrong.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK="${SCRIPT_DIR}/commit-identity-check.sh"

if [ ! -x "$CHECK" ]; then
  printf '::error::SELF-TEST FAILED: %s is missing or not executable.\n' "$CHECK" >&2
  exit 1
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# Deterministic identities. Nothing here reads the developer's git config.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
ALICE_NAME="Alice Example"; ALICE_EMAIL="alice@example.com"
BOB_NAME="Bob Example";     BOB_EMAIL="bob@example.com"

pass_count=0
fail_count=0

# new_repo <name> — a repo with one base commit on main, plus an origin/main
# remote-tracking ref pointing at it. Echoes the repo path.
new_repo() {
  local dir="$WORK/$1"
  mkdir -p "$dir"
  git -C "$dir" init -q -b main
  git -C "$dir" config user.name "$ALICE_NAME"
  git -C "$dir" config user.email "$ALICE_EMAIL"
  git -C "$dir" commit -q --allow-empty -m "base"
  git -C "$dir" update-ref refs/remotes/origin/main "$(git -C "$dir" rev-parse HEAD)"
  printf '%s' "$dir"
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
    "$CHECK" 2>&1
  ) || rc=$?

  if [ "$rc" -ne "$want" ]; then
    printf '::error::SELF-TEST FAILED: %s — expected exit %s, got %s. This is a defect in the identity check itself, not a problem with your commits.\n' "$name" "$want" "$rc" >&2
    printf '%s\n' "$out" >&2
    fail_count=$((fail_count + 1))
    return
  fi

  if [ -n "$needle" ] && ! printf '%s' "$out" | grep -qF -- "$needle"; then
    printf '::error::SELF-TEST FAILED: %s — exit code was right (%s) but the output never mentioned %s, so the check may be passing or failing for the wrong reason.\n' "$name" "$want" "$needle" >&2
    printf '%s\n' "$out" >&2
    fail_count=$((fail_count + 1))
    return
  fi

  printf '  ok       %s (exit %s)\n' "$name" "$want"
  pass_count=$((pass_count + 1))
}

# --- the denylist ------------------------------------------------------------
# Each shape below is one git invents from the machine when `user.email` is
# unset. `.sol-local` is the exact shape SOL-152902 found in this repository's
# own history.

r=$(new_repo sol_local)
GIT_AUTHOR_NAME="$ALICE_NAME" GIT_AUTHOR_EMAIL="alice@buildbox.sol-local" \
  git -C "$r" commit -q --allow-empty -m "work"
expect 1 ".sol-local author is rejected" "$r" "not routable"

r=$(new_repo dot_local)
GIT_AUTHOR_NAME="$ALICE_NAME" GIT_AUTHOR_EMAIL="alice@alices-laptop.local" \
  git -C "$r" commit -q --allow-empty -m "work"
expect 1 ".local author is rejected" "$r" "not routable"

r=$(new_repo internal)
GIT_AUTHOR_NAME="$ALICE_NAME" GIT_AUTHOR_EMAIL="alice@host.internal" \
  git -C "$r" commit -q --allow-empty -m "work"
expect 1 ".internal author is rejected" "$r" "not routable"

r=$(new_repo lan)
GIT_AUTHOR_NAME="$ALICE_NAME" GIT_AUTHOR_EMAIL="alice@host.lan" \
  git -C "$r" commit -q --allow-empty -m "work"
expect 1 ".lan author is rejected" "$r" "not routable"

r=$(new_repo bare_hostname)
GIT_AUTHOR_NAME="$ALICE_NAME" GIT_AUTHOR_EMAIL="alice@buildbox" \
  git -C "$r" commit -q --allow-empty -m "work"
expect 1 "bare hostname with no dot is rejected" "$r" "not routable"

# Case must not be an escape hatch.
r=$(new_repo uppercase)
GIT_AUTHOR_NAME="$ALICE_NAME" GIT_AUTHOR_EMAIL="alice@BuildBox.SOL-LOCAL" \
  git -C "$r" commit -q --allow-empty -m "work"
expect 1 "uppercase .SOL-LOCAL is still rejected" "$r" "not routable"

# The committer is this pull request's contribution too, so a clean author does
# not excuse a machine-generated committer.
r=$(new_repo committer)
GIT_COMMITTER_NAME="$BOB_NAME" GIT_COMMITTER_EMAIL="bob@laptop.local" \
  git -C "$r" commit -q --allow-empty -m "work"
expect 1 "bad committer is rejected even with a clean author" "$r" "not routable"

# --- the pass path -----------------------------------------------------------
# A denylist exists so outside contributors are not gatekept. If these fail, the
# check has become an allowlist and will block the community.

r=$(new_repo external_ok)
GIT_AUTHOR_NAME="$BOB_NAME" GIT_AUTHOR_EMAIL="bob@example.com" \
  git -C "$r" commit -q --allow-empty -m "work"
expect 0 "an ordinary external domain passes" "$r" "OK"

r=$(new_repo gh_noreply_ok)
GIT_AUTHOR_NAME="$BOB_NAME" GIT_AUTHOR_EMAIL="12345+bob@users.noreply.github.com" \
  git -C "$r" commit -q --allow-empty -m "work"
expect 0 "a GitHub noreply address passes" "$r" "OK"

# `.localdomain` is not `.local`. Suffix matching that is too loose would reject
# a legitimate address.
r=$(new_repo localdomain_ok)
GIT_AUTHOR_NAME="$BOB_NAME" GIT_AUTHOR_EMAIL="bob@example.localdomain" \
  git -C "$r" commit -q --allow-empty -m "work"
expect 0 "a domain merely containing 'local' is not rejected" "$r" "OK"

# --- scope -------------------------------------------------------------------
# Forward-only. A bad address already on the base branch is out of reach, and
# judging it would block every pull request until history was rewritten.

r=$(new_repo base_history_ignored)
GIT_AUTHOR_NAME="$ALICE_NAME" GIT_AUTHOR_EMAIL="alice@oldbox.local" \
  git -C "$r" commit -q --allow-empty -m "already on main"
git -C "$r" update-ref refs/remotes/origin/main "$(git -C "$r" rev-parse HEAD)"
GIT_AUTHOR_NAME="$BOB_NAME" GIT_AUTHOR_EMAIL="bob@example.com" \
  git -C "$r" commit -q --allow-empty -m "this PR"
expect 0 "a bad address already on the base branch is not this PR's problem" "$r" "OK"

# Merges carry identity fields even though they carry no content to sign off.
r=$(new_repo merge_checked)
git -C "$r" checkout -q -b side
GIT_AUTHOR_NAME="$BOB_NAME" GIT_AUTHOR_EMAIL="bob@example.com" \
  git -C "$r" commit -q --allow-empty -m "side work"
git -C "$r" checkout -q main
GIT_COMMITTER_NAME="$ALICE_NAME" GIT_COMMITTER_EMAIL="alice@laptop.local" \
  GIT_AUTHOR_NAME="$ALICE_NAME" GIT_AUTHOR_EMAIL="alice@laptop.local" \
  git -C "$r" merge -q --no-ff side -m "merge side"
expect 1 "a merge commit's identity is checked too" "$r" "not routable"

# --- the remediation text ----------------------------------------------------
# The printed fix instructions are part of the control, not commentary. Advice
# that rewrites the wrong commits, or that leaves the bad address in the
# `Signed-off-by:` trailer while the check goes green, defeats the check by
# being followed. These cases pin the two guards lifted from the retired
# dco-check.sh: no branch-wide rebase when a merge is among the offenders, and a
# per-commit guard on `--exec` when it is not.

# capture <repo> — the check's combined output, exit code ignored.
capture() {
  ( cd "$1" &&
    BASE_SHA="$(git rev-parse refs/remotes/origin/main)" \
    BASE_REF="main" \
    HEAD_SHA="$(git rev-parse HEAD)" \
    "$CHECK" 2>&1 ) || true
}

# assert_has / assert_lacks <case name> <output> <needle>
assert_has() {
  if printf '%s' "$2" | grep -qF -- "$3"; then
    printf '  ok       %s\n' "$1"
    pass_count=$((pass_count + 1))
  else
    printf '::error::SELF-TEST FAILED: %s — the failure output never mentioned %s.\n' "$1" "$3" >&2
    printf '%s\n' "$2" >&2
    fail_count=$((fail_count + 1))
  fi
}
assert_lacks() {
  if printf '%s' "$2" | grep -qF -- "$3"; then
    printf '::error::SELF-TEST FAILED: %s — the failure output contained %s, which it must not.\n' "$1" "$3" >&2
    printf '%s\n' "$2" >&2
    fail_count=$((fail_count + 1))
  else
    printf '  ok       %s\n' "$1"
    pass_count=$((pass_count + 1))
  fi
}

# A plain commit: the bulk fix is offered, and it carries the per-commit guard
# that stops `--exec` rewriting a colleague's commits during the replay.
r=$(new_repo advice_plain)
GIT_AUTHOR_NAME="$ALICE_NAME" GIT_AUTHOR_EMAIL="alice@laptop.local" \
  git -C "$r" commit -q --allow-empty -m "work"
out=$(capture "$r")
# Assert on the command line itself, not the whole output — the cautionary prose
# below it deliberately quotes the unguarded form as the thing not to run.
rebase_line=$(printf '%s\n' "$out" | grep -F 'git rebase --exec' || true)
assert_has "the bulk fix is offered for a plain commit" "$rebase_line" 'git rebase --exec'
assert_has "the bulk fix guards --exec per commit" "$rebase_line" '|| exit 0'
assert_has "the hazard of dropping the guard is spelled out" "$out" 'runs after every commit it replays'
assert_has "the trailer is stripped, not just the author reset" "$out" 'signed-off-by:.*<$BAD>'
assert_has "the base of the rebase is the PR's own base" "$out" "$(git -C "$r" rev-parse refs/remotes/origin/main)"
assert_has "the requirement is cross-referenced" "$out" '.github/CONTRIBUTING.md#author-identity'

# A merge among the offenders: rebase would flatten it and discard the conflict
# resolution, so the branch-wide command must be withheld entirely.
r=$(new_repo advice_merge)
git -C "$r" checkout -q -b side
GIT_AUTHOR_NAME="$BOB_NAME" GIT_AUTHOR_EMAIL="bob@example.com" \
  git -C "$r" commit -q --allow-empty -m "side work"
git -C "$r" checkout -q main
GIT_COMMITTER_NAME="$ALICE_NAME" GIT_COMMITTER_EMAIL="alice@laptop.local" \
  GIT_AUTHOR_NAME="$ALICE_NAME" GIT_AUTHOR_EMAIL="alice@laptop.local" \
  git -C "$r" merge -q --no-ff side -m "merge side"
out=$(capture "$r")
assert_has "a failing merge withholds the rebase" "$out" 'Do NOT rewrite this branch with `git rebase`'
assert_lacks "no rebase command is printed when a merge fails" "$out" 'git rebase --exec'

# --- no commits --------------------------------------------------------------

r=$(new_repo empty_range)
expect 0 "a pull request that adds no commits passes" "$r" "nothing to check"

# --- mutation ----------------------------------------------------------------
# A vacuous pass is the failure mode that matters: gut the denylist and every
# case above still exits 0. This asserts the suite would notice.

mutant="$WORK/mutant.sh"
sed 's/\*\.local|\*\.sol-local|\*\.internal|\*\.lan) return 1 ;;/*.nomatch) return 1 ;;/' "$CHECK" >"$mutant"
chmod +x "$mutant"
if ! grep -q '\*\.nomatch' "$mutant"; then
  printf '::error::SELF-TEST FAILED: the mutation did not apply — the denylist line in commit-identity-check.sh changed shape, so this guard is no longer testing anything. Update the sed pattern here.\n' >&2
  fail_count=$((fail_count + 1))
else
  r=$(new_repo mutation_probe)
  GIT_AUTHOR_NAME="$ALICE_NAME" GIT_AUTHOR_EMAIL="alice@buildbox.sol-local" \
    git -C "$r" commit -q --allow-empty -m "work"
  mrc=0
  ( cd "$r" && BASE_SHA="$(git rev-parse refs/remotes/origin/main)" BASE_REF="main" \
      HEAD_SHA="$(git rev-parse HEAD)" "$mutant" >/dev/null 2>&1 ) || mrc=$?
  if [ "$mrc" -eq 0 ]; then
    printf '  ok       a gutted denylist would be caught (mutant passes where the real check fails)\n'
    pass_count=$((pass_count + 1))
  else
    printf '::error::SELF-TEST FAILED: the mutant still rejected a .sol-local address, so this suite cannot tell a working denylist from a broken one.\n' >&2
    fail_count=$((fail_count + 1))
  fi
fi

printf 'commit-identity-check self-test: %d passed, %d failed\n' "$pass_count" "$fail_count"
[ "$fail_count" -eq 0 ]
