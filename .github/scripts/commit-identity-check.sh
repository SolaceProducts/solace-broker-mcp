#!/usr/bin/env bash
# Fails a pull request whose commits carry a non-routable author or committer
# email — one ending in `.local`, `.sol-local`, `.internal`, or `.lan`, or a
# bare hostname with no dot at all.
#
# Why this exists: git invents an address from the machine's hostname when
# `git config user.email` is unset, so a laptop or build box name ends up in
# commit metadata. This repository is public, and git history is permanent —
# an internal hostname published once cannot be recalled without rewriting
# history everyone has already fetched. Prevention is the only control that
# works. Introduced under SOL-152902.
#
# Split out of dco-check.sh (SOL-153050) when DCO enforcement moved to the CNCF
# `dco2` GitHub App. The App checks sign-off and nothing else, so this half
# needed its own home rather than disappearing with the script that used to
# carry both.
#
# A denylist rather than an allowlist, so an outside contributor with an
# ordinary address is never blocked. A typo like `.locol` slips through; that is
# the accepted trade-off for not gatekeeping every contributor's domain.
#
# Forward-only: it walks the commits this pull request adds, not the history
# already on the base branch. Commits already merged are out of reach.
#
# Environment:
#   HEAD_SHA  head commit of the PR       (defaults to HEAD)
#   BASE_SHA  base commit of the PR       (defaults to origin/main)
#   BASE_REF  base branch name, e.g. main (optional; also excluded when the
#             remote ref is present, so commits landing on the base after
#             BASE_SHA was captured are not attributed to this PR)
#
# Usage: .github/scripts/commit-identity-check.sh
set -euo pipefail

HEAD_REV="${HEAD_SHA:-HEAD}"
BASE_REV="${BASE_SHA:-origin/main}"

for rev in "$HEAD_REV" "$BASE_REV"; do
  if ! git rev-parse --quiet --verify "${rev}^{commit}" >/dev/null; then
    echo "::error::Cannot resolve '${rev}' to a commit. Check the checkout depth." >&2
    exit 1
  fi
done

# Exclude the live base tip as well when it is visible. BASE_SHA is a snapshot
# taken when the run started; anything merged to the base since then is not this
# pull request's contribution and must not be judged as though it were.
EXCLUDE_BASE_REF=""
if [ -n "${BASE_REF:-}" ] && git rev-parse --quiet --verify "refs/remotes/origin/${BASE_REF}^{commit}" >/dev/null; then
  EXCLUDE_BASE_REF="^refs/remotes/origin/${BASE_REF}"
fi

# Merges included, unlike the sign-off check. A merge commit contributes no
# content to sign off for, but its identity fields are still this pull request's
# contribution and publish just the same.
# shellcheck disable=SC2086 # EXCLUDE_BASE_REF is a single rev or deliberately empty
commits=$(git rev-list "$HEAD_REV" "^${BASE_REV}" $EXCLUDE_BASE_REF)

if [ -z "${commits//[[:space:]]/}" ]; then
  echo "This pull request adds no commits — nothing to check."
  exit 0
fi

lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }

# Rejects the shapes a developer machine invents when `git config user.email` is
# unset: a bare hostname (`alice@buildbox`), an mDNS domain (`.local`), or an
# internal-only TLD (`.sol-local`, `.internal`, `.lan`).
identity_domain_ok() {
  local email="$1" domain
  domain="${email##*@}"
  # reject empty-domain and no-`@` inputs
  [ "$domain" != "$email" ] && [ -n "$domain" ] || return 1
  domain=$(lower "$domain")
  case "$domain" in
    *.local|*.sol-local|*.internal|*.lan) return 1 ;;
  esac
  # bare hostname (no dot at all in the domain part)
  case "$domain" in
    *.*) return 0 ;;
    *)   return 1 ;;
  esac
}

failed=""
total=0
for sha in $commits; do
  total=$((total + 1))
  author_email=$(lower "$(git show -s --format='%ae' "$sha")")
  committer_email=$(lower "$(git show -s --format='%ce' "$sha")")

  if ! identity_domain_ok "$author_email" || ! identity_domain_ok "$committer_email"; then
    failed="${failed}${sha}"$'\n'
  fi
done

if [ -z "$failed" ]; then
  echo "All ${total} commit(s) use a routable author and committer address — OK."
  exit 0
fi

failed_count=$(grep -c . <<<"$failed")
echo "::error::${failed_count} of ${total} commit(s) in this pull request use an author or committer email whose domain is not routable (.local, .sol-local, .internal, .lan, or a bare hostname). These publish permanently with the git history." >&2
echo >&2

# Commit metadata is attacker-controlled text going into a CI log that reads
# workflow commands from stdout. Strip carriage returns and control characters
# so a crafted address cannot forge a `::` directive or garble the report.
sanitize() { tr -d '\r' | tr -d '\000-\010\013\014\016-\037'; }

while IFS= read -r sha; do
  [ -n "$sha" ] || continue
  printf '  %s  author=%s  committer=%s\n' \
    "$(git rev-parse --short "$sha")" \
    "$(git show -s --format='%ae' "$sha" | sanitize)" \
    "$(git show -s --format='%ce' "$sha" | sanitize)" >&2
done <<<"$failed"

cat >&2 <<'EOF'

Set your commit identity to a routable address, then rewrite the commits listed
above. The address sits in two places — the author/committer fields and the
`Signed-off-by:` trailer — and both have to change: `--reset-author` alone
leaves the old address published in the trailer, and leaves the `DCO` check
failing, because the trailer no longer matches the author or committer.

  git config user.email "you@your-domain.example"

Substitute the address CI listed above for BAD below.

  # only the most recent commit (works on a merge commit too — amend keeps its
  # parents)
  export BAD="the.address@ci.listed"
  git log -1 --format=%B | grep -vi "signed-off-by:.*<$BAD>" |
    git commit --amend --reset-author -s --file=-
EOF

# `git rebase` replays a merge as an ordinary commit, throwing away the merge and
# any conflict resolution in it. This check walks merges, so a merge can be among
# the offenders — withhold the branch-wide command when one is.
failed_merges=""
while IFS= read -r sha; do
  [ -n "$sha" ] || continue
  [ "$(git rev-list --parents -n1 "$sha" | wc -w)" -le 2 ] || failed_merges=yes
done <<<"$failed"

if [ -n "$failed_merges" ]; then
  cat >&2 <<'EOF'

Do NOT rewrite this branch with `git rebase` — merge commits are among those
listed above, and rebase would replay them as ordinary commits, throwing away
the merge and any conflict resolution in it. Fix a merge that is not at the tip
by re-creating it: reset to before it, then redo the merge with your corrected
identity.
EOF
else
  cat >&2 <<'EOF'

  # every commit on the branch carrying that address, leaving the others alone.
  # The --exec argument must stay on ONE line; git rejects a newline in it.
EOF
  # shellcheck disable=SC2016 # $BAD is for the contributor's shell, not this one
  REBASE_EXEC='git log -1 --format="%ae %ce" | grep -qiF "$BAD" || exit 0; git log -1 --format=%B | grep -vi "signed-off-by:.*<$BAD>" | git commit --amend --reset-author -s --file=-'
  printf "  git rebase --exec '%s' %s\n" "$REBASE_EXEC" "$BASE_REV" >&2
  cat >&2 <<'EOF'

Do NOT drop the leading guard and run `--exec 'git commit --amend
--reset-author'` over the branch. `--exec` runs after every commit it replays,
so that rewrites the author of everyone else's commits on the branch to you.
EOF
fi

printf "\nCheck it took before pushing:\n\n  git log %s..HEAD --format='%%h %%ae %%ce'\n" "$BASE_REV" >&2

cat >&2 <<'EOF'

Then `git push --force-with-lease`.

See .github/CONTRIBUTING.md#author-identity for the requirement and the
reasoning.
EOF

exit 1
