#!/usr/bin/env bash
#
# CI gate: every Go source file carries the Apache-2.0 licence header.
#
# WHY THIS EXISTS
#
# Apache 2.0 works best when the grant travels with the file. A downstream
# consumer who receives one `.go` file should be able to read its licence from
# the file, not infer it from a LICENSE three directories up.
#
# The headers were applied once, by hand, across a codebase that was much
# smaller — and then the codebase grew. By August 2026, 53 of 260 files had no
# header (SOL-152896). That is the whole mechanism, and fixing the 53 files
# without adding a gate just restarts the same clock. The Open Source Solace
# Software Checklist uses this repository as its worked example of that drift.
#
# WHAT IT CHECKS
#
# The first 13 lines of every `.go` file are byte-identical to the canonical
# header below, and line 14 is blank. All three properties are load-bearing:
#
#   - Byte-identical, not "contains the word Apache". A half-copied or reworded
#     header is the thing this gate exists to prevent, and the repository is
#     already 100% consistent (207 of 207 files matched exactly when the gate was
#     written), so there is no legacy variation to accommodate. A looser match
#     would let the next variant in and the file would still read as covered.
#
#   - At the TOP of the file, which a `grep -L` over the whole file does not
#     check. A header pasted below the `package` clause satisfies a naive search
#     while leaving the top of the file — where a reader and every licence
#     scanner look — blank.
#
#   - Followed by a blank line, which is Go semantics, not formatting. A comment
#     block immediately above `package` IS the package doc comment. Without the
#     blank line the licence text becomes the package documentation and shows up
#     in `go doc` and on pkg.go.dev, displacing the real doc comment.
#
# THE COPYRIGHT YEAR, WHICH IS PINNED RATHER THAN VALIDATED
#
# The comparison is byte-exact over all 13 lines, so the year range in line 1 is
# pinned to whatever `CANONICAL_HEADER` says. It is NOT checked against the
# current date: nothing here fails in January because the range ended last year.
# That is deliberate. Rolling the range is a repo-wide edit with no licence
# consequence, and failing every pull request until someone does it would be
# friction without control value.
#
# When the range does roll, edit `CANONICAL_HEADER` below and run `--fix`. A file
# that differs from canonical ONLY in a Solace copyright line is repaired in
# place rather than reported, precisely so that edit stays a one-liner. The
# tolerance is narrow on purpose: the line must still be a Solace copyright
# notice. A third party's copyright line is not ours to rewrite, so a file
# carrying one is reported and left alone.
#
# WHAT IT DELIBERATELY DOES NOT CHECK
#
# Non-Go files. Go is what this repository publishes. Widen the walk here if
# that changes; do not add a second script.
#
# Written for bash 3.2, which is what ships on macOS — no `mapfile`, and no
# expansion of a possibly-empty array under `set -u`. Results accumulate in
# newline-delimited strings for that reason, as in `licenses-check.sh`.
#
# Usage:
#   .github/scripts/license-header-check.sh          # check, exit 1 on any miss
#   .github/scripts/license-header-check.sh --fix    # prepend the header in place
#
# `--fix` only ever touches files with no licence text at all. A file that has
# some other header is reported, never rewritten — prepending to it would
# produce two headers, and a human should read what is already there.

set -euo pipefail

# The canonical header. `internal/tools/register.go` was the reference when this
# gate was written; this constant is now the source of truth for all of them.
read -r -d '' CANONICAL_HEADER <<'EOF' || true
// Copyright 2024-2026 Solace Corporation. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
EOF

HEADER_LINES=$(grep -c '' <<<"$CANONICAL_HEADER")
# The header plus the mandatory blank line, which is what a conforming file
# opens with. Compared as one block so a missing blank line 14 is caught too.
PREFIX_LINES=$((HEADER_LINES + 1))

# A line every conforming header contains, used only to classify a failure for
# reporting and to decide whether `--fix` may touch the file. Never the pass
# condition — that is the byte-exact comparison above.
LICENCE_MARKER='Licensed under the Apache License'

# A Solace copyright line carrying any year or year range. Used only to decide
# that a file diverging from canonical on line 1 alone is OURS to repair. Anyone
# else's copyright line fails this and is left for a human.
SOLACE_COPYRIGHT_RE='^// Copyright [0-9]{4}(-[0-9]{4})? Solace Corporation\. All rights reserved\.$'

FIX=0
case "${1-}" in
    --fix) FIX=1 ;;
    "") ;;
    *)
        echo "::error::Unknown argument '$1'. Usage: $0 [--fix]" >&2
        exit 2
        ;;
esac

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
printf '%s\n\n' "$CANONICAL_HEADER" >"$work/prefix"

# `find` rather than `git ls-files`: the self-test runs this against fixture
# trees that are not git repositories, and a licence obligation attaches to the
# file on disk either way. `vendor/` is excluded because those files carry their
# upstream's licence, not ours; `.git`, `.ua` and `node_modules` hold no
# first-party Go source and only slow the walk down.
find . \
    \( -name .git -o -name .ua -o -name vendor -o -name node_modules \) -prune -o \
    -type f -name '*.go' -print |
    sort >"$work/files"

checked=$(grep -c . <"$work/files" || true)

# A gate that inspects nothing reports success. If the walk above ever breaks —
# run from the wrong directory, a prune pattern that swallows the tree — this is
# the difference between a loud failure and a green check over an empty set.
if [ "$checked" -eq 0 ]; then
    echo "::error::Found no .go files to check. Run from the repository root; a check over zero files is not a pass." >&2
    exit 1
fi

# Everything below line 1 of the conforming prefix. A file matching this but not
# the whole prefix diverges on the copyright line alone.
tail -n +2 "$work/prefix" >"$work/prefix_tail"

missing=""     # no licence text at all      -> --fix prepends
stale_year=""  # only the Solace year differs -> --fix rewrites line 1
malformed=""   # anything else                -> --fix refuses

while IFS= read -r f; do
    [ -n "$f" ] || continue
    if head -n "$PREFIX_LINES" "$f" | diff -q - "$work/prefix" >/dev/null 2>&1; then
        continue
    fi
    if head -n "$PREFIX_LINES" "$f" | tail -n +2 | diff -q - "$work/prefix_tail" >/dev/null 2>&1 &&
        head -n 1 "$f" | grep -qE "$SOLACE_COPYRIGHT_RE"; then
        stale_year="${stale_year}${f}"$'\n'
    elif grep -qF "$LICENCE_MARKER" "$f"; then
        malformed="${malformed}${f}"$'\n'
    else
        missing="${missing}${f}"$'\n'
    fi
done <"$work/files"

n_missing=$(grep -c . <<<"$missing" || true)
n_stale=$(grep -c . <<<"$stale_year" || true)
n_malformed=$(grep -c . <<<"$malformed" || true)

# --- fix mode ---------------------------------------------------------------

# Replace a file's contents without ever leaving it truncated. The staging file
# is created with `cp -p`, which clones the original's mode, and lands with a
# same-directory `mv`, which is atomic. Writing straight back over `$f` would be
# simpler but opens a window — an interrupt or a full disk between truncate and
# write leaves a destroyed source file whose only other copy is in a temp
# directory the EXIT trap deletes.
replace_contents() { # <file> <cat args producing the new content...>
    local target="$1"
    shift
    local staged="${target}.license-header.tmp.$$"
    cp -p "$target" "$staged"
    cat "$@" >"$staged"
    mv "$staged" "$target"
}

if [ "$FIX" -eq 1 ]; then
    while IFS= read -r f; do
        [ -n "$f" ] || continue
        replace_contents "$f" "$work/prefix" "$f"
        echo "  added header: $f"
    done <<<"$missing"

    # Lines 2 to 14 already match canonical, so the whole prefix can be restated
    # and the rest of the file carried over untouched.
    while IFS= read -r f; do
        [ -n "$f" ] || continue
        tail -n +"$((PREFIX_LINES + 1))" "$f" >"$work/body"
        replace_contents "$f" "$work/prefix" "$work/body"
        echo "  updated copyright line: $f"
    done <<<"$stale_year"

    if [ "$n_malformed" -gt 0 ]; then
        echo
        echo "::error::$n_malformed file(s) carry licence text this cannot safely rewrite — reworded, below the package clause, missing the blank line, or under someone else's copyright. Read what is there and fix it by hand:" >&2
        grep . <<<"$malformed" | sed 's/^/  /' >&2
        exit 1
    fi

    echo "✅ Repaired $((n_missing + n_stale)) file(s) ($n_missing header added, $n_stale copyright line updated); $checked Go file(s) now conform."
    exit 0
fi

# --- verdict ----------------------------------------------------------------
failures=$((n_missing + n_stale + n_malformed))

if [ "$failures" -eq 0 ]; then
    echo "✅ All $checked Go file(s) carry the Apache-2.0 licence header."
    exit 0
fi

while IFS= read -r f; do
    [ -n "$f" ] || continue
    echo "::error file=${f#./},line=1::No Apache-2.0 licence header. Every published Go file must carry the licence grant."
done <<<"$missing"

while IFS= read -r f; do
    [ -n "$f" ] || continue
    echo "::error file=${f#./},line=1::Copyright line does not match the canonical header. Run the check with --fix to update it."
done <<<"$stale_year"

while IFS= read -r f; do
    [ -n "$f" ] || continue
    echo "::error file=${f#./},line=1::Licence text is present but the first $HEADER_LINES lines are not the canonical header followed by a blank line. It may be reworded, or placed below the package clause."
done <<<"$malformed"

cat >&2 <<EOF

$failures of $checked Go file(s) do not carry the canonical Apache-2.0 licence header ($n_missing with no licence text, $n_stale with a stale copyright line, $n_malformed with something else).

The grant has to travel with the file: a downstream consumer who receives one
file should read its licence from that file, not from a LICENSE three
directories up. Fix every auto-fixable case with:

    .github/scripts/license-header-check.sh --fix

The header goes above everything else in the file, followed by a blank line.
The blank line is required: without it Go treats the licence text as the
package doc comment and publishes it on pkg.go.dev in place of the real one.
EOF
exit 1
