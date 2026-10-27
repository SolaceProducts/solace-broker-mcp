#!/usr/bin/env bash
#
# Rewrites THIRD_PARTY_BUILD_TEST.md's table rows to match what the repository
# actually builds and tests with, driven entirely by
# build-test-licenses-check.sh's own diagnostics (SOL-152956). Covers all four
# kinds of change the gate checks: Go module version bumps, new transitive Go
# modules, GitHub Action SHA re-pins, and container image tag changes.
#
# WHY IT PARSES THE CHECK SCRIPT'S OUTPUT INSTEAD OF RECOMPUTING DISCOVERY
#
# build-test-licenses-check.sh is out of scope to change for this ticket, and
# it already computes the one thing that matters: the exact difference between
# what four unrelated sources (go.mod closures, package-lock.json, `uses:`
# refs, image tags) say is in use and what THIRD_PARTY_BUILD_TEST.md documents,
# emitted as one `::error file=THIRD_PARTY_BUILD_TEST.md::...` line per
# discrepancy. Recomputing discover()/expect() a second way here would be a
# second, independently-drifting copy of exactly the logic this ticket exists
# to stop drifting. This script treats that output as the single source of
# truth for *what* changed, which is also why re-running it unmodified is the
# correct way to verify the fix rather than a weaker stand-in for one.
#
# WHAT IT REFUSES TO DO
#
# Every new row this script writes carries a licence read fresh — from
# go-licenses for a Go module, or `gh api repos/OWNER/REPO/license?ref=<tag>`
# for an action, per the rule already stated in
# THIRD_PARTY_BUILD_TEST.md#rebuilding-this-file: licences are read, never
# inferred, never copied from a neighbouring row. A brand-new container image
# or npm package is refused outright rather than guessed at, because there is
# no equally reliable, automatable source for either (a registry tag bump
# doesn't change who publishes the image, so that case IS handled; a novel
# image reference has no LICENSE-file or GitHub-API equivalent this script can
# check). In practice this refusal is close to theoretical for images and npm:
# .github/dependabot.yml has neither a `docker` nor an `npm` ecosystem entry,
# so a Dependabot PR cannot structurally add or remove either kind today.
#
# If build-test-licenses-check.sh reports anything else this script does not
# recognise (a duplicate row, an unparseable row, `go list` failing, a missing
# `jq`), it stops and asks for a human rather than guessing at a subset of the
# fix. A partially-applied fix is worse than none.
#
# THE RE-PINNED PROSE (SOL-156741)
#
# The "Solace-internal composite actions" table is followed by prose that names
# the shared pin, the date and Dependabot PR of the last re-pin, and the prior
# pin. On a re-pin, a fix that moves every SolaceDev/solace-public-workflows
# row to one new commit, this script rewrites that paragraph too: every
# backticked old pin in it becomes the new one, and the "Re-pinned <date> ...
# (#<PR>); the prior pin was `<old>`" sentence gets today's date, the PR
# number and the old pin, which is the pin the prose named. Later paragraphs
# in the section are left alone. The PR number comes from REFRESH_PR_NUMBER
# and is used only by a re-pin run, so a caller may pass it on every PR;
# without it the sentence carries a literal "(#N)" for a person to replace.
#
# The check never reads this prose, so it is checked here, before anything is
# written. A Solace-internal ref that is not a full commit SHA is refused. A
# re-pin is refused when it would leave the rows on more than one pin, or when
# this script cannot find the prose to update. Nothing else ever rewrites the
# prose: a run that only drops or fixes rows, or finds rows re-pinned by hand,
# leaves it alone and prints a warning when the rows no longer share the pin it
# names. Dropping every row leaves the prose with a warning too.
#
# Every ::warning:: this script prints about the prose means a person must
# edit it before the change is committed; the check never reads the prose, so
# a run can exit 0 with it wrong. An unattended caller (SOL-153374) must treat
# any ::warning:: line from this script as a failure.
#
# Usage: [REFRESH_PR_NUMBER=<n>] .github/scripts/refresh-build-test-inventory.sh
#   REFRESH_PR_NUMBER  number of the Dependabot PR that re-pinned
#                      SolaceDev/solace-public-workflows; optional.
# Exits 0 if the file already matched, or now matches after a rewrite.
# Exits 1 if any diagnostic could not be safely automated, or the rewrite did
# not converge.

set -euo pipefail

# Run-from-repo-root contract, matching build-test-licenses-check.sh exactly
# (no `git rev-parse` — the check script has no such dependency either, and
# this script's own self-test symlinks individual paths into a plain tmpdir
# with no `.git` at all, the same fixture shape
# build-test-licenses-check.test.sh already uses).
DOC="THIRD_PARTY_BUILD_TEST.md"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK="$SCRIPT_DIR/build-test-licenses-check.sh"
LIB_DIR="$SCRIPT_DIR/lib"

if [ ! -f "$DOC" ]; then
    echo "::error::$DOC not found. Run from the repository root." >&2
    exit 1
fi

# Checked before anything runs: the value lands in a Legal document, so a typo
# must stop the run rather than be written in. Spelled-out ASCII sets, here
# and in the SHA checks below: under a UTF-8 locale bash's [0-9] also matches
# other scripts' digits, which the next re-pin could not read back.
ASCII_DIGIT='[0123456789]'
ASCII_HEX='[0123456789abcdef]'
pr_number="${REFRESH_PR_NUMBER:-}"
if [ -n "$pr_number" ] && ! [[ "$pr_number" =~ ^[123456789]${ASCII_DIGIT}*$ ]]; then
    echo "::error::REFRESH_PR_NUMBER='$pr_number' is not a pull request number. Pass the bare" \
        "number (for example 540), or leave it unset." >&2
    exit 1
fi

# The row-editing primitives below write via "$DOC.tmp" then `mv` it over
# "$DOC" — if a failure lands between those two (an awk error, a full disk),
# the intended "leave the file exactly as it was" contract silently gets an
# exception: the stray .tmp survives in the repo root, where a later
# `git add -A` could commit a half-written copy of a Legal document sitting
# right next to the real one. Removed on every exit, success or failure.
trap 'rm -f "$DOC.tmp"' EXIT

# shellcheck source=lib/inventory-refresh-common.sh
source "$LIB_DIR/inventory-refresh-common.sh"

# Not third-party: five actions from one Solace-owned repository, listed so
# the inventory accounts for every `uses:` without a licence column that
# doesn't apply to them. Placement-only heuristic, exactly as documented in
# THIRD_PARTY_BUILD_TEST.md's "Solace-internal composite actions" section —
# unknown to (and unneeded by) the out-of-scope check script, which doesn't
# care which of this file's tables a row lives in.
SOLACE_INTERNAL_OWNER_REPO="SolaceDev/solace-public-workflows"

SOLACE_INTERNAL_HEADING="### Solace-internal composite actions"

run_check() {
    "$CHECK" 2>&1
}

# repin_prose <mode> <doc> [<old-pin> <new-pin> <date> <pr-ref>]
#   Works on the prose between the end of the Solace-internal table and the
#   next heading. Modes:
#     pin      print the pin the prose names ("`<sha>` is a branch commit",
#              exactly 7 hex characters)
#     ref      print the Re-pinned sentence's PR reference ("#540", or "#N")
#     check    exit 0 if the Re-pinned sentence occurs exactly once, after the
#              branch-commit sentence
#     rewrite  print <doc> with, in the Re-pinned paragraph only (branch-commit
#              sentence to the end of the Re-pinned sentence), every
#              backticked <old-pin> set to <new-pin>, then the date, (#...)
#              and prior pin set to <date>, (<pr-ref>) and <old-pin>
#   Exits 3 when the section, its table or the expected text is missing.
#   The sentence may wrap anywhere, so it is matched across the joined lines
#   with [[:space:]]+, and only the three values are spliced in: the
#   whitespace between words, line breaks included, is kept as it was.
#   POSIX awk only (no {n} intervals, no gensub), for the macOS leg.
repin_prose() {
    awk -v mode="$1" -v heading="$SOLACE_INTERNAL_HEADING" \
        -v old="${3:-}" -v new="${4:-}" -v day="${5:-}" -v ref="${6:-}" '
        { line[NR] = $0 }
        END {
            start = 0
            for (i = 1; i <= NR; i++) if (line[i] == heading) { start = i; break }
            if (!start) exit 3
            # First table line after the heading, then the first line past it.
            t = 0
            for (i = start + 1; i <= NR && line[i] !~ /^#/; i++) if (line[i] ~ /^\|/) { t = i; break }
            if (!t) exit 3
            for (from = t; from <= NR && line[from] ~ /^\|/; from++) ;
            for (to = from; to <= NR && line[to] !~ /^#/; to++) ;
            to--
            if (to < from) exit 3

            text = ""
            for (i = from; i <= to; i++) text = text (i > from ? "\n" : "") line[i]

            sentence = "Re-pinned[[:space:]]+[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9][[:space:]]+by[[:space:]]+the[[:space:]]+Dependabot[[:space:]]+`internal-reusable-workflows`[[:space:]]+group[[:space:]]+update[[:space:]]+\\(#([0-9]+|N)\\);[[:space:]]+the[[:space:]]+prior[[:space:]]+pin[[:space:]]+was[[:space:]]+`[0-9a-f]+`"

            # Exactly 7 characters, as in the table rows: the same commit
            # written longer must not read as a different pin.
            h = "[0123456789abcdef]"
            branch = "`" h h h h h h h "`[[:space:]]+is[[:space:]]+a[[:space:]]+branch[[:space:]]+commit"

            if (mode == "pin") {
                if (!match(text, branch)) exit 3
                s = substr(text, RSTART + 1)
                print substr(s, 1, index(s, "`") - 1)
                exit 0
            }

            n = 0; rest = text
            while (match(rest, sentence)) { n++; rest = substr(rest, RSTART + RLENGTH) }
            if (n != 1) exit 3
            match(text, sentence)
            ss = RSTART; se = RSTART + RLENGTH

            if (mode == "ref") {
                m = substr(text, ss, se - ss)
                match(m, /\(#([0-9]+|N)\)/)
                print substr(m, RSTART + 1, RLENGTH - 2)
                exit 0
            }

            # The Re-pinned paragraph runs from the branch-commit sentence to
            # the end of the Re-pinned sentence; it is all a re-pin rewrites.
            # Later paragraphs that name a pin record other events.
            if (!match(text, branch) || RSTART > ss) exit 3
            bs = RSTART
            if (mode == "check") exit 0

            pre = substr(text, 1, bs - 1); mid = substr(text, bs, se - bs); post = substr(text, se)
            # Old pin to new first, so the prior pin written next survives.
            out = ""; rest = mid; o = "`" old "`"
            while ((k = index(rest, o)) > 0) {
                out = out substr(rest, 1, k - 1) "`" new "`"
                rest = substr(rest, k + length(o))
            }
            mid = out rest
            match(mid, sentence)
            mpre = substr(mid, 1, RSTART - 1); m = substr(mid, RSTART, RLENGTH)
            match(m, /[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]/)
            m = substr(m, 1, RSTART - 1) day substr(m, RSTART + RLENGTH)
            match(m, /\(#([0-9]+|N)\)/)
            m = substr(m, 1, RSTART - 1) "(" ref ")" substr(m, RSTART + RLENGTH)
            match(m, /`[0-9a-f]+`$/)
            m = substr(m, 1, RSTART - 1) "`" old "`"
            text = pre mpre m post

            for (i = 1; i < from; i++) print line[i]
            print text
            for (i = to + 1; i <= NR; i++) print line[i]
        }
    ' "$2"
}

# solace_rows
#   "<name>\t<ref>" for every Solace-internal row in $DOC, backticks stripped.
solace_rows() {
    awk -v p="| \`${SOLACE_INTERNAL_OWNER_REPO}/" -F' \\| ' '
        index($1, p) == 1 { n = substr($1, 4); gsub(/`/, "", n); r = $2; gsub(/`/, "", r); print n "\t" r }
    ' "$DOC"
}

# plan_prose <repins> <rows>
#   Decides the Re-pinned prose rewrite for the Solace-internal rows as they
#   will stand (<rows>, "<name>\t<ref>" lines). The prose is rewritten only on
#   a re-pin, <repins>=1: this run writes a pin the rows did not already
#   carry. Then every row must end on that one pin, and the prose's pin (one
#   the rows carried before the run, in pins_before) and its Re-pinned
#   sentence must be found, or the run is refused (exit 1)
#   before any row is written; when they are, prose_old/prose_new are set.
#   Any other run never touches the prose. When it leaves the rows on more
#   than one pin, or on one pin the prose does not name (rows re-pinned by
#   hand, rows joining a pin the table already used, prose this script
#   cannot read), it says so in a warning and carries on.
prose_old=""
prose_new=""
prose_rows_gone=0
plan_prose() {
    local repins="$1" pins prose_pin prose_rc=0 named
    pins=$(awk -F'\t' 'NF == 2 && $2 != "" { print $2 }' <<<"$2" | sort -u)
    [ -n "$pins" ] || return 0
    prose_pin=$(repin_prose pin "$DOC") || prose_rc=$?
    [ "$prose_rc" -eq 0 ] || prose_pin=""

    if [ "$repins" -ne 1 ]; then
        if [ "$(grep -c . <<<"$pins")" -ne 1 ]; then
            echo "::warning::The ${SOLACE_INTERNAL_OWNER_REPO} rows are on more than one pin" \
                "($(sed 's/.*/`&`/' <<<"$pins" | paste -sd ' ' -)), but the prose below their table says" \
                "they share one. The prose is rewritten only on a re-pin to a new commit; update it by hand."
        elif [ "$pins" != "$prose_pin" ]; then
            if [ -n "$prose_pin" ]; then
                named="\`$prose_pin\`"
            else
                named="no pin this script can read"
            fi
            echo "::warning::The ${SOLACE_INTERNAL_OWNER_REPO} rows are pinned to \`$pins\`, but the prose" \
                "below their table names ${named}. The prose is rewritten only on a re-pin to a new" \
                "commit; update it by hand."
        fi
        return 0
    fi

    if [ "$(grep -c . <<<"$pins")" -ne 1 ]; then
        echo "::error::After this re-pin the ${SOLACE_INTERNAL_OWNER_REPO} rows would not share one pin" \
            "($(tr '\n' ' ' <<<"$pins")), and the prose below their table says they do. Refusing" \
            "to write either; update the table and its prose by hand." >&2
        exit 1
    fi
    if [ -z "$prose_pin" ]; then
        echo "::error::Could not find the \"\`<sha>\` is a branch commit\" sentence below the" \
            "\"${SOLACE_INTERNAL_HEADING#\#\#\# }\" table, so the prose cannot be kept in step with" \
            "the re-pinned rows. Refusing to write either; update both by hand." >&2
        exit 1
    fi
    [ "$pins" != "$prose_pin" ] || return 0
    # The prose's pin becomes the "prior pin" and every mention of it is
    # rewritten, so it must be one the rows actually carried before this run.
    if ! grep -qxF "$prose_pin" <<<"$pins_before"; then
        echo "::error::The prose below the ${SOLACE_INTERNAL_OWNER_REPO} table names \`$prose_pin\`, which" \
            "no row carried before this re-pin ($(tr '\n' ' ' <<<"$pins_before")). Refusing to write either;" \
            "update the table and its prose by hand." >&2
        exit 1
    fi
    if ! repin_prose check "$DOC"; then
        echo "::error::The ${SOLACE_INTERNAL_OWNER_REPO} pin moves from \`$prose_pin\` to" \
            "\`$pins\`, but the \"Re-pinned YYYY-MM-DD by the Dependabot" \
            "\`internal-reusable-workflows\` group update (#N); the prior pin was \`<sha>\`\"" \
            "sentence below its table was not found exactly once. Refusing to write either;" \
            "update the table and its prose by hand." >&2
        exit 1
    fi
    prose_old="$prose_pin"
    prose_new="$pins"
}

# apply_prose <date>
#   Writes what plan_prose decided, citing REFRESH_PR_NUMBER. The number is
#   used only here, by the run that re-pins; any other run ignores it, so a
#   caller can pass it on every PR. A "(#N)" that stays in the Re-pinned
#   sentence is reported on every run until a person replaces it.
prose_changed=0
apply_prose() {
    local day="$1" pr_ref="#N"
    [ -z "$pr_number" ] || pr_ref="#${pr_number}"
    if [ -n "$prose_new" ]; then
        echo "  prose: ${SOLACE_INTERNAL_OWNER_REPO} re-pinned \`$prose_old\` -> \`$prose_new\` on $day ($pr_ref)"
        repin_prose rewrite "$DOC" "$prose_old" "$prose_new" "$day" "$pr_ref" >"$DOC.tmp"
        mv "$DOC.tmp" "$DOC"
        prose_changed=1
    fi
    if [ "$(repin_prose ref "$DOC" 2>/dev/null || true)" = "#N" ]; then
        echo "::warning::$DOC says \"(#N)\" where the Dependabot PR number belongs. Replace it by" \
            "hand, or restore the file and re-run the refresh with REFRESH_PR_NUMBER=<n> on the re-pin PR."
    fi
    if [ "$prose_rows_gone" -eq 1 ]; then
        echo "::warning::Every ${SOLACE_INTERNAL_OWNER_REPO} row was dropped. The prose below their" \
            "table still describes a shared pin; update it by hand."
    fi
}

# find_go_submodule_for <mod_path>
#   Which test/*/go.mod submodule's dependency closure contains <mod_path>,
#   for new-row table placement only (see the file header). Mirrors
#   build-test-licenses-check.sh's own discover()/`go list -deps -test` calls
#   exactly, rather than reinventing the traversal.
find_go_submodule_for() {
    local mod_path="$1" gomod sub
    while read -r gomod; do
        [ -n "$gomod" ] || continue
        sub=$(dirname "$gomod")
        # awk, not `grep -qxF`: grep -q exits at the first match and closes its
        # read end, and once go list's output exceeds the pipe buffer it gets
        # SIGPIPE — under `pipefail` that makes the whole pipeline's exit
        # status 141 even though the module WAS found, so a correct add fails
        # intermittently here and gets misreported as "should not happen" by
        # this function's caller. awk reads its input to EOF regardless of
        # when the match is found, so go list is never killed mid-write.
        #
        # GOOS pinned to linux to match build-test-licenses-check.sh's own
        # closure exactly — the refresh must resolve a module to the same
        # submodule the check will look for it in.
        if (cd "$sub" && GOOS=linux go list -deps -test -f '{{with .Module}}{{.Path}}{{end}}' ./... 2>/dev/null | awk -v m="$mod_path" '$0 == m { f = 1 } END { exit !f }'); then
            printf '%s' "$sub"
            return 0
        fi
    done < <(
        find -L test \( -name .git -o -name .claude -o -name .worktrees -o -name node_modules \) -prune \
            -o -name go.mod -type f -print 2>/dev/null | sort
    )
    return 1
}

echo "--- Running build-test-licenses-check.sh to find what changed ---"
# `|| rc=$?`, not a bare `rc=$?` after: with -e now active, an unguarded
# `output=$(run_check)` would abort the whole script the instant
# build-test-licenses-check.sh exits nonzero — exactly the normal, expected
# case this line exists to detect, not a bug. Same idiom that script itself
# uses for `go list` for the identical reason (see its own comment).
rc=0
output=$(run_check) || rc=$?
echo "$output"

if [ "$rc" -eq 0 ]; then
    # The rows match, but the prose may still lag them: a "(#N)" left by an
    # earlier run, or a table re-pinned by hand. Neither is refused here.
    plan_prose 0 "$(solace_rows)"
    apply_prose "$(date -u +%Y-%m-%d)"
    if [ "$prose_changed" -eq 1 ]; then
        echo "✅ $DOC rows already matched; the Re-pinned prose was brought in line with them."
    else
        echo "✅ $DOC already matches the build and test inputs. Nothing to refresh."
    fi
    exit 0
fi

# sort -u: build-test-licenses-check.sh's Go-module check (check 1) loops per
# test/*/go.mod submodule and reports once per submodule, so one new
# transitive module needed by two submodules at once emits two byte-identical
# "::error::" lines. Without deduplicating, both parse into separate to_add
# entries and get inserted twice — a real, reproduced bug: the check's own
# duplicate-row detector can't catch it either, because it dedupes documented
# rows by (name, version) *before* scanning for dupes, so two identical rows
# collapse to one before that scan ever runs. `sort -u` here is safe because
# only byte-identical diagnostics collapse; two genuinely different messages
# (e.g. the same module at two different versions in two submodules — a real,
# distinct problem) differ in text and are never merged.
doc_errors=$(grep -E "^::error file=${DOC}::" <<<"$output" | sort -u || true)
if [ -z "$doc_errors" ]; then
    echo "::error::build-test-licenses-check.sh failed, but reported no ::error file=${DOC}:: line." \
        "See the full output above." >&2
    exit 1
fi

# Each entry: "kind\tname\tactual" (add), "kind\tname\tdoc_version\tactual" (fix),
# or "name" (drop, kind-agnostic — see build-test-licenses-check.sh's reverse
# direction check, which never learns what kind a stale row was).
to_add=()
to_fix=()
to_drop=()
unhandled=()

# Kept as variables rather than inline in the [[ =~ ]] tests below: an unquoted
# pattern containing a literal, backslash-escaped parenthesis (the "(${actual})"
# in the add-case message) is a bash parse error when written inline, because
# bash's own parser sees the backslash-paren before the regex engine does.
kind_re='(Go module|npm package|Action|Container image)'
drop_re='^`([^`]+)` is listed but nothing in the repository uses it any more'
# The actual-value group is `(.+)`, not `[^)]+`: build-test-licenses-check.sh
# writes an unpinned action's actual value as the literal string "(unpinned)"
# (see the `actual="(unpinned)"` assignment in its Action discovery), so the
# full message reads "...`name` ((unpinned)) is used...". `[^)]+` cannot
# capture past the first `)` inside that value, so it fails to match this case
# at all — the line falls into the generic `unhandled` bucket, which still
# refuses correctly but loses the specific "not SHA-pinned" diagnostic below.
# `.+` finds the right split because " is used but is not listed" appears
# exactly once, immediately after the value's own closing paren.
add_re="^${kind_re}"' `([^`]+)` \((.+)\) is used but is not listed'
# Trailing-period caveat: versions legitimately contain periods, so this is
# anchored on the message's own literal markers, not on \S+.
fix_re="^${kind_re}"' `([^`]+)` is listed at (.+) but the repository uses (.+)\.$'

while IFS= read -r line; do
    [ -n "$line" ] || continue
    msg="${line#::error file=${DOC}::}"

    if [[ "$msg" =~ $drop_re ]]; then
        to_drop+=("${BASH_REMATCH[1]}")
        continue
    fi
    if [[ "$msg" =~ $add_re ]]; then
        kind="${BASH_REMATCH[1]}"
        if [ "$kind" = "Container image" ] || [ "$kind" = "npm package" ]; then
            unhandled+=("$line  [refused: no reliable automated licence source for a new $kind — see this script's header]")
            continue
        fi
        actual="${BASH_REMATCH[3]}"
        if [ "$kind" = "Action" ] && [ "$actual" = "(unpinned)" ]; then
            unhandled+=("$line  [refused: action is not SHA-pinned, nothing to resolve a licence against]")
            continue
        fi
        to_add+=("$kind"$'\t'"${BASH_REMATCH[2]}"$'\t'"$actual")
        continue
    fi
    if [[ "$msg" =~ $fix_re ]]; then
        kind="${BASH_REMATCH[1]}"
        actual="${BASH_REMATCH[4]}"
        if [ "$kind" = "Action" ] && [ "$actual" = "(unpinned)" ]; then
            unhandled+=("$line  [refused: action is not SHA-pinned, nothing to resolve a licence against]")
            continue
        fi
        to_fix+=("$kind"$'\t'"${BASH_REMATCH[2]}"$'\t'"${BASH_REMATCH[3]}"$'\t'"$actual")
        continue
    fi
    unhandled+=("$line")
done <<<"$doc_errors"

if [ "${#unhandled[@]}" -gt 0 ]; then
    echo "::error::build-test-licenses-check.sh reported ${#unhandled[@]} problem(s) this script" \
        "does not fix automatically. Refusing to write a partial fix. A human needs to look at:" >&2
    printf '%s\n' "${unhandled[@]}" >&2
    exit 1
fi

# The Re-pinned prose, decided before any row is written (see the header). The
# Solace-internal rows' refs after the fix are their current refs with each
# to_fix/to_add actual applied; a drop removes that row.
solace_touched=0
solace_after="$(solace_rows)"$'\n'
pins_before=$(solace_rows | awk -F'\t' 'NF == 2 && $2 != "" { print $2 }' | sort -u)
repins=0
for entry in ${to_fix[@]+"${to_fix[@]}"} ${to_add[@]+"${to_add[@]}"}; do
    kind="${entry%%$'\t'*}"
    rest="${entry#*$'\t'}"
    name="${rest%%$'\t'*}"
    actual="${rest##*$'\t'}"
    [ "$kind" = "Action" ] || continue
    [ "$(cut -d/ -f1,2 <<<"$name")" = "$SOLACE_INTERNAL_OWNER_REPO" ] || continue
    solace_touched=1
    # The row keeps 7 characters of a full SHA. Anything else is refused on
    # the full ref, never on its first 7 characters: "deadbeef-fix" must not
    # pass as "deadbee", nor a branch or tag named "cafe123" as a commit. The
    # prose calls the pin a branch commit, and the next re-pin can only find
    # a hex SHA there.
    if [[ ! "$actual" =~ ^${ASCII_HEX}{40}$ ]]; then
        echo "::error::\`$name\` would be pinned to \`$actual\`, which is not a full commit SHA, and" \
            "the prose below the ${SOLACE_INTERNAL_OWNER_REPO} table describes the pin as a branch" \
            "commit. Refusing to write either; pin the workflows to a full commit SHA, or update" \
            "the table and its prose by hand." >&2
        exit 1
    fi
    pin="${actual:0:7}"
    grep -qxF "$pin" <<<"$pins_before" || repins=1
    solace_after=$(awk -F'\t' -v n="$name" '$1 != n' <<<"$solace_after")$'\n'"${name}"$'\t'"${pin}"$'\n'
done
for name in ${to_drop[@]+"${to_drop[@]}"}; do
    [ "$(cut -d/ -f1,2 <<<"$name")" = "$SOLACE_INTERNAL_OWNER_REPO" ] || continue
    solace_touched=1
    solace_after=$(awk -F'\t' -v n="$name" '$1 != n' <<<"$solace_after")$'\n'
done
# A re-pin is a pin the rows did not already carry; only a re-pin rewrites
# the prose (see plan_prose).
plan_prose "$repins" "$solace_after"
if [ "$solace_touched" -eq 1 ] && [ -z "$(awk -F'\t' 'NF == 2 && $2 != ""' <<<"$solace_after")" ]; then
    prose_rows_gone=1
fi

echo "--- Applying ${#to_fix[@]} version fix(es), ${#to_add[@]} new row(s), ${#to_drop[@]} removal(s) ---"

for entry in ${to_fix[@]+"${to_fix[@]}"}; do
    IFS=$'\t' read -r kind name doc_version actual <<<"$entry"
    echo "  fix: $kind \`$name\` $doc_version -> $actual"
    case "$kind" in
        "Go module" | "npm package")
            set_row_field "$DOC" "$name" 2 "$actual"
            substitute_in_row_field "$DOC" "$name" 4 "$doc_version" "$actual"
            ;;
        "Container image")
            # Every row in this table backticks its tag column (see
            # THIRD_PARTY_BUILD_TEST.md's "Container images" section) — unlike
            # the Go module/npm tables just above, which don't.
            set_row_field "$DOC" "$name" 2 "\`${actual}\`"
            ;;
        "Action")
            owner_repo=$(cut -d/ -f1,2 <<<"$name")
            short_sha="${actual:0:7}"
            if [ "$owner_repo" = "$SOLACE_INTERNAL_OWNER_REPO" ]; then
                # 3-column table, no licence/tag columns to keep in sync.
                set_row_field "$DOC" "$name" 2 "\`${short_sha}\`"
            else
                old_tag=$(get_row_field "$DOC" "$name" 3)
                new_tag=$(resolve_action_tag "$owner_repo" "$name" "$actual")
                if [ -z "$new_tag" ]; then
                    echo "::error::Could not resolve a release tag for \`$name\`@$actual (checked the" \
                        "workflow file's trailing comment and the repository's tag list). Update its" \
                        "row by hand." >&2
                    exit 1
                fi
                set_row_field "$DOC" "$name" 2 "\`${short_sha}\`"
                set_row_field "$DOC" "$name" 3 "$new_tag"
                if [ -n "$old_tag" ]; then
                    substitute_in_row_field "$DOC" "$name" 5 "$old_tag" "$new_tag"
                fi
            fi
            ;;
    esac
done

for entry in ${to_add[@]+"${to_add[@]}"}; do
    IFS=$'\t' read -r kind name actual <<<"$entry"
    echo "  add: $kind \`$name\` @ $actual"
    case "$kind" in
        "Go module")
            sub=$(find_go_submodule_for "$name") || {
                echo "::error::\`$name\` is a new dependency but no test/*/go.mod submodule's closure" \
                    "contains it. This should not happen — the check that reported it and this" \
                    "placement lookup should agree; treating as a bug rather than guessing a table." >&2
                exit 1
            }
            # Run from inside the owning submodule, not the repo root: each
            # test/*/go.mod pins its own dependency graph independently (the
            # THIRD_PARTY_BUILD_TEST.md note on golang.org/x/oauth2 records two
            # different pinned versions of one module for exactly this reason),
            # so resolving the licence from the wrong go.sum can silently name
            # the wrong tag in the generated URL even when the version number
            # printed alongside it happens to be right.
            license_line=$(cd "$sub" && fetch_go_module_license "$name")
            if [ -z "$license_line" ]; then
                echo "::error::Could not resolve a single, unambiguous licence for \`$name\` via" \
                    "go-licenses. Add its row by hand, reading the licence from its own LICENSE file." >&2
                exit 1
            fi
            spdx="${license_line%%$'\t'*}"
            url="${license_line#*$'\t'}"
            # This file's own Verdict section states every Go module here is
            # permissive (MIT, BSD-3-Clause, Apache-2.0) — same reasoning as
            # refresh-licenses-inventory.sh's identical check: a real
            # strong-copyleft dependency, or an unresolved value like
            # NOASSERTION/Unknown, must never be written in as if it were
            # confirmed permissive.
            if ! is_recognized_license "$spdx"; then
                echo "::error::\`$name\` resolved to licence '$spdx', which this script does not" \
                    "recognize. Verify by hand and add its row, reading the licence from its own" \
                    "LICENSE file." >&2
                exit 1
            fi
            # The anchor is the full heading ("### `test/e2e-basic-mcp/agent`"),
            # not the bare path: insert_row_in_table matches its anchor as a
            # literal substring anywhere in the file, and the bare submodule
            # path is exactly the kind of string that can legitimately also
            # appear in prose before the real heading (THIRD_PARTY_BUILD_TEST.md
            # already has this shape for a different path — the npm section's
            # "`test/e2e-llm/package.json` pins the Claude Code CLI..." sentence
            # comes before that submodule's own table). The gate never checks
            # which table a row lives in, so a match against the wrong
            # occurrence would insert into the wrong section silently.
            insert_row_in_table "$DOC" "### \`${sub}\`" \
                "| \`${name}\` | ${actual} | ${spdx} | [license](${url}) |"
            ;;
        "Action")
            owner_repo=$(cut -d/ -f1,2 <<<"$name")
            short_sha="${actual:0:7}"
            if [ "$owner_repo" = "$SOLACE_INTERNAL_OWNER_REPO" ]; then
                insert_row_in_table "$DOC" "Solace-internal composite actions" \
                    "| \`${name}\` | \`${short_sha}\` | Solace |"
            else
                tag=$(resolve_action_tag "$owner_repo" "$name" "$actual")
                if [ -z "$tag" ]; then
                    echo "::error::Could not resolve a release tag for new action \`$name\`@$actual." \
                        "Add its row by hand." >&2
                    exit 1
                fi
                # fetch_action_license already refuses (returns empty) for an
                # unrecognized licence or a malformed/failed API response, not
                # only a genuinely empty result — this `-z` check catches both
                # without needing its own allow-list here too.
                license_line=$(fetch_action_license "$owner_repo" "$tag")
                if [ -z "$license_line" ]; then
                    echo "::error::Could not resolve a licence for \`$name\` at ref $tag via" \
                        "'gh api repos/${owner_repo}/license?ref=${tag}'. Add its row by hand." >&2
                    exit 1
                fi
                spdx="${license_line%%$'\t'*}"
                url="${license_line#*$'\t'}"
                insert_row_in_table "$DOC" "## GitHub Actions" \
                    "| \`${name}\` | \`${short_sha}\` | ${tag} | ${spdx} | [license](${url}) |"
            fi
            ;;
    esac
done

for name in ${to_drop[@]+"${to_drop[@]}"}; do
    echo "  drop: \`$name\`"
    delete_row "$DOC" "$name"
done

# Housekeeping only — never load-bearing for the gate — and, like the sibling
# script's own version of this, touches only the LEADING "**Generated** DATE"
# token, not the narrative clauses after it (this file's own "; the GitHub
# Actions section was refreshed ... and again ..." sentence records specific
# past events by their own specific dates, which stay true regardless of when
# this run happens). Without this, a real rewrite could leave the file
# asserting a "Generated" date older than the row it just changed.
today=$(date -u +%Y-%m-%d 2>/dev/null || true)
if [ -n "$today" ]; then
    sed -i.bak -E "s/^\*\*Generated\*\* [0-9]{4}-[0-9]{2}-[0-9]{2}/\*\*Generated\*\* ${today}/" "$DOC" 2>/dev/null || true
    rm -f "$DOC.bak"
fi

apply_prose "${today:-$(date -u +%Y-%m-%d)}"

echo "--- Re-running build-test-licenses-check.sh to verify the rewrite ---"
verify_rc=0
verify_output=$(run_check) || verify_rc=$?
echo "$verify_output"

if [ "$verify_rc" -ne 0 ]; then
    echo "::error::$DOC still does not match after the automated rewrite. Refusing to leave this" \
        "half-fixed silently — see the errors above for what's left, and fix the rest by hand." >&2
    exit 1
fi

echo "✅ $DOC refreshed and verified."
