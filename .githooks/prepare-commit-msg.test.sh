#!/usr/bin/env bash
#
# Self-test for the prepare-commit-msg hook. Builds throwaway git repositories,
# installs the real hook into each, makes real commits through every path a
# contributor uses, and asserts what git ends up recording.
#
# Run manually:  .githooks/prepare-commit-msg.test.sh
# No CI job runs it, so a change to the hook is only verified if you run this. The
# hook rewrites every commit message on a contributor's machine; regressions there
# are silent, because the CI DCO gate accepts a sign-off anywhere in the message
# and so does not notice a trailer that got welded onto the subject line.
# Run it on the oldest git you expect contributors to have: some cases pass on git
# 2.50 and catch a real bug only on 2.39, the version bookworm/22.04/RHEL 9 ship.
#
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
HOOK="${SCRIPT_DIR}/prepare-commit-msg"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

pass_count=0
fail_count=0

# Deterministic identities. Nothing here may read the developer's git config OR
# environment. The env vars matter as much as the config: identity from
# GIT_AUTHOR_*/GIT_COMMITTER_* OUTRANKS config, so in a devcontainer, a corporate
# CI image, or agent tooling that exports them, every assertion comparing against
# $SIGNOFF goes red against a perfectly correct hook. Test 5 sets them per-command
# for the one case that needs them, so unsetting here costs nothing.
unset GIT_AUTHOR_NAME GIT_AUTHOR_EMAIL GIT_COMMITTER_NAME GIT_COMMITTER_EMAIL
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
DEV_NAME="Dev Example"; DEV_EMAIL="dev@example.com"
SIGNOFF="Signed-off-by: $DEV_NAME <$DEV_EMAIL>"

# new_repo <name> [--no-identity] — a repo with the hook installed and one base
# commit. Echoes the repo path.
new_repo() {
  local dir="$WORK/$1"
  mkdir -p "$dir"
  git -C "$dir" init -q -b main
  if [ "${2:-}" != "--no-identity" ]; then
    git -C "$dir" config user.name "$DEV_NAME"
    git -C "$dir" config user.email "$DEV_EMAIL"
  fi
  install -m 755 "$HOOK" "$dir/.git/hooks/prepare-commit-msg"
  # --no-verify so the base commit does not depend on the hook under test.
  GIT_AUTHOR_NAME="$DEV_NAME" GIT_AUTHOR_EMAIL="$DEV_EMAIL" \
    GIT_COMMITTER_NAME="$DEV_NAME" GIT_COMMITTER_EMAIL="$DEV_EMAIL" \
    git -C "$dir" commit -q --allow-empty --no-verify -m "base"
  printf '%s' "$dir"
}

# stage <repo> <name> — stage a unique change so each commit has content.
stage() {
  echo "$2" >"$1/$2.txt"
  git -C "$1" add "$2.txt"
}

# An editor that types a subject onto the buffer's first line, exactly as a human
# does: the newline terminating line 1 stays where it is. This is the path that
# once welded the trailer onto the subject.
make_editor() {
  local path="$1" subject="$2"
  cat >"$path" <<EOF
#!/bin/sh
{ printf '%s\n' "$subject"; tail -n +2 "\$1"; } >"\$1.typed" && mv "\$1.typed" "\$1"
EOF
  chmod 755 "$path"
}

# A template whose message is still empty — line 1 blank, then a hint comment.
printf '\n\n# hint from the commit template\n' >"$WORK/tmpl"

ok()   { printf 'ok  %s\n' "$1"; pass_count=$((pass_count + 1)); }
fail() { printf 'FAIL  %s\n' "$1"; shift; printf '%s\n' "$@" | sed 's/^/    | /'; fail_count=$((fail_count + 1)); }

# expect_equal <case name> <want> <got>
expect_equal() {
  if [ "$2" = "$3" ]; then ok "$1"; else fail "$1" "want: $2" "got:  $3"; fi
}

# --- 1. editor mode: subject typed on line 1 keeps its own line ---------------
# The blocker this suite exists for, across every buffer shape git may hand the
# hook. Whatever git puts below the subject — a comment block under any comment
# char, the un-prefixed diff of `-v`, a scissors line, a template — the subject
# must keep its own line and the trailer must stay a trailer git can parse.
make_editor "$WORK/ed-subject" "real subject"

# editor_case <case name> <repo config args...> -- <git commit args...>
editor_case() {
  local name="$1"; shift
  local dir="$WORK/editor_$(echo "$name" | tr -cd 'a-zA-Z0-9')"
  local -a cfg=() args=()
  while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do cfg+=("$1"); shift; done
  shift || true
  args=("$@")

  dir=$(new_repo "$(basename "$dir")")
  local i
  for ((i = 0; i < ${#cfg[@]}; i += 2)); do
    git -C "$dir" config "${cfg[i]}" "${cfg[i + 1]}"
  done
  stage "$dir" one
  # "${args[@]+...}" — a bare "${args[@]}" on an EMPTY array is an unbound-variable
  # error under `set -u` on bash < 4.4, which is what /usr/bin/env bash resolves to
  # on a stock macOS (3.2.57). That kills the whole script at the first case, so no
  # case runs at all. Same idiom as the `extra` array below; keep them consistent.
  GIT_EDITOR="$WORK/ed-subject" git -C "$dir" commit -q "${args[@]+"${args[@]}"}"
  expect_equal "editor mode, $name: subject keeps its own line" \
    "real subject" "$(git -C "$dir" log -1 --format=%s)"
  # The READ side is pinned as well as the write side: `%(trailers)` re-renders the
  # separator as trailer.separators[0], so with a hostile `trailer.separators` in
  # this repo's config git echoes back `Signed-off-by=` for a message that on disk
  # says `Signed-off-by:`. That is the reader's rendering, not what the hook wrote,
  # and comparing against it would fail a correct hook. Asking with ':' pinned is
  # the actual question: did git parse a `Signed-off-by:` trailer?
  expect_equal "editor mode, $name: trailer is parseable" \
    "$SIGNOFF" \
    "$(git -C "$dir" -c trailer.separators=: log -1 --format='%(trailers:only=true)' | tr -d '\n')"
}

editor_case "plain"
# `-v` appends the raw diff below a scissors line, and those diff lines carry no
# comment prefix — so no amount of comment-stripping tells you the message is
# still empty. commit.verbose is a common dotfile setting, which makes this every
# commit for the people who set it, not an edge case.
editor_case "-v" -- -v
editor_case "commit.verbose=true" commit.verbose true --
editor_case "--cleanup=scissors" -- --cleanup=scissors
editor_case "core.commentChar=';'" core.commentChar ';' --
editor_case "core.commentChar=auto" core.commentChar auto --
editor_case "commit.template" commit.template "$WORK/tmpl" -- -t "$WORK/tmpl"
# `trailer.*` in a developer's own config changes the form interpret-trailers
# EMITS, while the hook's line-2 check compares against the form it ASKED for. One
# config line — `trailer.separators='=:'` writes `Signed-off-by= Name <email>` —
# is enough to defeat the check and bring the weld back, and the CI DCO gate does
# not notice because it matches a sign-off anywhere in the message. These cases
# only pass because the hook pins `trailer.*` on the interpret-trailers call; drop
# the `-c` flags and the first of them welds.
editor_case "trailer.separators='=:'" trailer.separators '=:' --
editor_case "hostile trailer.* config" \
  trailer.separators '=:' trailer.where start trailer.ifmissing doNothing \
  trailer.ifexists doNothing --

# --- 1a. CRLF buffer ----------------------------------------------------------
# A CRLF template makes line 1 a bare carriage return rather than an empty line,
# which is why the check normalises CRs before comparing. Without that, this welds
# exactly like the plain case did.
printf '\r\n# crlf template hint\r\n' >"$WORK/tmpl-crlf"
r=$(new_repo editor_crlf)
stage "$r" one
GIT_EDITOR="$WORK/ed-subject" git -C "$r" commit -q -t "$WORK/tmpl-crlf"
expect_equal "editor mode, CRLF template: subject keeps its own line" \
  "real subject" "$(git -C "$r" log -1 --format=%s)"
expect_equal "editor mode, CRLF template: trailer is parseable" \
  "$SIGNOFF" "$(git -C "$r" log -1 --format='%(trailers:only=true)' | tr -d '\n')"

# --- 1b. the message body matches native `git commit -s`, byte for byte -------
# Two repos, same content and same identity: one signs off through the hook, the
# other through git's own -s with no hook installed. The recorded message bodies
# must be identical, so the hook is not inventing its own layout.
native_repo() {
  local dir="$WORK/$1"
  mkdir -p "$dir"
  git -C "$dir" init -q -b main
  git -C "$dir" config user.name "$DEV_NAME"
  git -C "$dir" config user.email "$DEV_EMAIL"
  git -C "$dir" commit -q --allow-empty -m "base"
  printf '%s' "$dir"
}
for variant in plain -v; do
  hooked=$(new_repo "parity_hooked${variant}")
  native=$(native_repo "parity_native${variant}")
  extra=()
  [ "$variant" = "-v" ] && extra=(-v)
  stage "$hooked" one; stage "$native" one
  GIT_EDITOR="$WORK/ed-subject" git -C "$hooked" commit -q "${extra[@]+"${extra[@]}"}"
  GIT_EDITOR="$WORK/ed-subject" git -C "$native" commit -q -s "${extra[@]+"${extra[@]}"}"
  h=$(git -C "$hooked" cat-file commit HEAD | sed -n '/^$/,$p' | od -An -c)
  n=$(git -C "$native" cat-file commit HEAD | sed -n '/^$/,$p' | od -An -c)
  expect_equal "editor mode, $variant: message body matches native git commit -s" "$n" "$h"
done

# --- 2. -m mode ---------------------------------------------------------------
r=$(new_repo dash_m)
stage "$r" one
git -C "$r" commit -q -m "with dash m"
expect_equal "-m keeps the subject" "with dash m" "$(git -C "$r" log -1 --format=%s)"
expect_equal "-m produces a parseable trailer" \
  "$SIGNOFF" "$(git -C "$r" log -1 --format='%(trailers:only=true)' | tr -d '\n')"

# --- 3. explicit -s does not double up ---------------------------------------
r=$(new_repo dash_s)
stage "$r" one
git -C "$r" commit -q -s -m "explicit dash s"
expect_equal "-s yields exactly one sign-off" \
  "1" "$(git -C "$r" log -1 --format=%B | grep -c '^Signed-off-by:')"

# --- 4. repeated --amend --no-edit does not accumulate -----------------------
r=$(new_repo amend)
stage "$r" one
git -C "$r" commit -q -m "amend me"
git -C "$r" commit -q --amend --no-edit
git -C "$r" commit -q --amend --no-edit
git -C "$r" commit -q --amend --no-edit
expect_equal "three amends yield exactly one sign-off" \
  "1" "$(git -C "$r" log -1 --format=%B | grep -c '^Signed-off-by:')"
expect_equal "amend keeps the subject" "amend me" "$(git -C "$r" log -1 --format=%s)"

# --- 5. identity from the environment, no user.name/user.email in config -----
# How bots, CI, and agent tooling commit. git commits fine in this state, so the
# hook must sign off rather than bail out.
r=$(new_repo env_identity --no-identity)
stage "$r" one
GIT_AUTHOR_NAME="Bot Example" GIT_AUTHOR_EMAIL="bot@example.com" \
  GIT_COMMITTER_NAME="Bot Example" GIT_COMMITTER_EMAIL="bot@example.com" \
  git -C "$r" commit -q -m "env identity"
expect_equal "env-supplied identity is signed off" \
  "Signed-off-by: Bot Example <bot@example.com>" \
  "$(git -C "$r" log -1 --format='%(trailers:only=true)' | tr -d '\n')"

# --- 6. an existing Co-Authored-By trailer survives --------------------------
r=$(new_repo coauthor)
stage "$r" one
git -C "$r" commit -q -m "with a co-author

Co-authored-by: Other Person <other@example.com>"
got=$(git -C "$r" log -1 --format=%B | grep -c -e '^Co-authored-by: Other Person <other@example.com>$' -e "^$SIGNOFF\$")
expect_equal "co-author trailer is preserved alongside the sign-off" "2" "$got"

# --- 7. a third party's sign-off gets ours added next to it ------------------
# The case that justifies addIfDifferent over doNothing: the message carries a
# sign-off belonging to neither the author nor the committer, which CI rejects.
r=$(new_repo third_party)
stage "$r" one
git -C "$r" commit -q -m "quoting someone else's patch

Signed-off-by: Third Party <third@example.com>"
expect_equal "our sign-off is added next to a third party's" \
  "1" "$(git -C "$r" log -1 --format=%B | grep -c "^$SIGNOFF\$")"

# --- 8. hostile content in user.name round-trips verbatim --------------------
# The trailer is data, never shell. Nothing here may be expanded or executed.
r=$(new_repo hostile)
HOSTILE='Ev"il $(touch '"$WORK"'/pwned) `touch '"$WORK"'/pwned2` ünïcøde'
git -C "$r" config user.name "$HOSTILE"
stage "$r" one
git -C "$r" commit -q -m "hostile identity"
expect_equal "hostile user.name round-trips verbatim" \
  "Signed-off-by: $HOSTILE <$DEV_EMAIL>" \
  "$(git -C "$r" log -1 --format=%B | grep '^Signed-off-by:')"
if [ -e "$WORK/pwned" ] || [ -e "$WORK/pwned2" ]; then
  fail "command substitution in user.name was executed"
else
  ok "command substitution in user.name is not executed"
fi

# --- 8b. merge commits -------------------------------------------------------
# Merges have their own DCO rules: the gate exempts a merge that only records its
# parents, but a merge contributing a conflict resolution needs its own sign-off.
# The hook must sign both, and must not damage the generated merge subject.
r=$(new_repo merge_clean)
git -C "$r" checkout -q -b side
stage "$r" side; git -C "$r" commit -q -m "side work"
git -C "$r" checkout -q main
stage "$r" mainline; git -C "$r" commit -q -m "main work"
GIT_EDITOR=true git -C "$r" merge -q --no-ff side
expect_equal "clean merge keeps its generated subject" \
  "Merge branch 'side'" "$(git -C "$r" log -1 --format=%s)"
expect_equal "clean merge is signed off" \
  "$SIGNOFF" "$(git -C "$r" log -1 --format='%(trailers:only=true)' | tr -d '\n')"

r=$(new_repo merge_conflict)
echo "theirs" >"$r/clash.txt"; git -C "$r" add clash.txt
git -C "$r" commit -q -m "base clash"
git -C "$r" checkout -q -b side
echo "side" >"$r/clash.txt"; git -C "$r" commit -q -am "side edit"
git -C "$r" checkout -q main
echo "main" >"$r/clash.txt"; git -C "$r" commit -q -am "main edit"
git -C "$r" merge -q side >/dev/null 2>&1 || true
echo "resolved" >"$r/clash.txt"; git -C "$r" add clash.txt
GIT_EDITOR=true git -C "$r" commit -q --no-edit
expect_equal "conflicted merge is signed off" \
  "$SIGNOFF" "$(git -C "$r" log -1 --format='%(trailers:only=true)' | tr -d '\n')"
expect_equal "conflicted merge yields exactly one sign-off" \
  "1" "$(git -C "$r" log -1 --format=%B | grep -c '^Signed-off-by:')"

# `git merge --signoff` already put the trailer there; do not double it.
r=$(new_repo merge_signoff)
git -C "$r" checkout -q -b side
stage "$r" side; git -C "$r" commit -q -m "side work"
git -C "$r" checkout -q main
stage "$r" mainline; git -C "$r" commit -q -m "main work"
GIT_EDITOR=true git -C "$r" merge -q --no-ff --signoff side
expect_equal "merge --signoff yields exactly one sign-off" \
  "1" "$(git -C "$r" log -1 --format=%B | grep -c '^Signed-off-by:')"

# --- 9. no resolvable identity: no block, no corruption ----------------------
# A non-zero prepare-commit-msg aborts the commit, so with no resolvable identity
# the hook must exit 0 AND leave the message exactly as it found it — a
# `Signed-off-by:` with nothing after it is worse than no trailer.
#
# `user.useConfigOnly` is what makes the state real: without it git derives
# `user@host.local` from the hostname and `git var GIT_COMMITTER_IDENT` succeeds,
# so the `|| exit 0` branch is never entered and this tests nothing.
#
# The hook is invoked directly rather than through `git commit` because git
# resolves the committer ident BEFORE it runs prepare-commit-msg (verified on git
# 2.39.5 and 2.50.1: the hook does not run at all, git just prints "Committer
# identity unknown"). There is therefore no `git commit` that reaches the hook in
# this state; driving one would assert git's behaviour, not the hook's.
#
# Both assertions are reachable: turn the `|| exit 0` into `|| exit 1` and the
# first goes red; delete it and the hook signs off with an empty identity, which
# fails the second.
r=$(new_repo no_identity --no-identity)
git -C "$r" config user.useConfigOnly true
printf 'a subject\n' >"$r/msg"
rc=0
( cd "$r" && env -u GIT_AUTHOR_NAME -u GIT_AUTHOR_EMAIL -u GIT_COMMITTER_NAME \
    -u GIT_COMMITTER_EMAIL EMAIL= GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null \
    "$HOOK" msg ) || rc=$?
expect_equal "no resolvable identity: hook exits 0" "0" "$rc"
expect_equal "no resolvable identity: message left untouched" \
  "a subject" "$(cat "$r/msg")"
# Guard the guard: if git ever resolves an identity here, the two assertions above
# pass without exercising the branch they exist for.
if ( cd "$r" && env -u GIT_AUTHOR_NAME -u GIT_AUTHOR_EMAIL -u GIT_COMMITTER_NAME \
      -u GIT_COMMITTER_EMAIL EMAIL= GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null \
      git var GIT_COMMITTER_IDENT >/dev/null 2>&1 ); then
  fail "no resolvable identity: git var really fails" \
    "git var GIT_COMMITTER_IDENT succeeded, so the two cases above proved nothing"
else
  ok "no resolvable identity: git var really fails"
fi

echo
printf 'prepare-commit-msg self-test: %d passed, %d failed\n' "$pass_count" "$fail_count"
[ "$fail_count" -eq 0 ]
