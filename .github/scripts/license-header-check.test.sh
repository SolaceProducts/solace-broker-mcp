#!/usr/bin/env bash
#
# Self-test for license-header-check.sh, in the same spirit as
# licenses-check.test.sh and dco-check.test.sh.
#
# A gate that cannot fail is not a gate. The dangerous failure mode for a licence
# header check is not a false alarm — it is a *silent pass*, and there are four
# plausible ones, each of which a reasonable implementation gets wrong:
#
#   - `grep -L "Licensed under the Apache License"` over whole files passes a
#     header pasted BELOW the package clause, which leaves the top of the file
#     blank for every reader and licence scanner;
#   - a substring match passes a reworded or half-copied header;
#   - a check for the 13 header lines alone passes a file with no blank line
#     after them, where Go then treats the licence as the package doc comment;
#   - a walk that finds nothing reports success over an empty set.
#
# Every case below asserts on an exit code, so each must be a shape where the
# correct implementation and a plausibly broken one differ. A case that fails
# "for some other reason" proves nothing, so the positive cases matter as much as
# the negative ones: without them a script that failed everything would score
# perfectly on the negatives alone.
#
# Fixtures are built in a temp directory and never touch the working tree. The
# one exception is the first case, which runs against the real repository — that
# is the anchor tying the script's canonical constant to what is committed.
#
# Usage: .github/scripts/license-header-check.test.sh

set -euo pipefail

REPO_ROOT=$(git rev-parse --show-toplevel)
CHECK="$REPO_ROOT/.github/scripts/license-header-check.sh"

# Taken from a real file rather than restated here. A fixture built from a
# hand-copy of the header would drift from the script's constant, and every case
# below would then be testing the drift instead of the behaviour.
HEADER=$(head -n 13 "$REPO_ROOT/internal/tools/register.go")

pass=0
fail=0

# --- fixture helpers --------------------------------------------------------
# Each takes the fixture root as $1. Every fixture starts from a conforming tree
# so a negative case fails for the reason it names and nothing else.

conforming_file() { # <dir> <path>
    mkdir -p "$(dirname "$1/$2")"
    { printf '%s\n\n' "$HEADER"; printf 'package fixture\n'; } >"$1/$2"
}

base_fixture() { # <dir>
    conforming_file "$1" "good.go"
}

no_header() { # <dir>
    printf 'package fixture\n' >"$1/bad.go"
}

nested_no_header() { # <dir> — the same defect, but below the top level
    mkdir -p "$1/internal/deep/pkg"
    printf 'package pkg\n' >"$1/internal/deep/pkg/bad.go"
}

stale_copyright_year() { # <dir> — ours, only the year range differs
    printf '%s\n\n' "$HEADER" | sed 's/2024-2026/2024-2027/' >"$1/bad.go"
    printf 'package fixture\n' >>"$1/bad.go"
}

foreign_copyright() { # <dir> — someone else's notice above our licence text
    printf '%s\n\n' "$HEADER" |
        sed 's|^// Copyright .*|// Copyright 2019 Other Corp. All rights reserved.|' \
            >"$1/bad.go"
    printf 'package fixture\n' >>"$1/bad.go"
}

header_below_package() { # <dir>
    { printf 'package fixture\n\n'; printf '%s\n' "$HEADER"; } >"$1/bad.go"
}

reworded_header() { # <dir>
    printf '%s\n\n' "$HEADER" |
        sed 's|http://www.apache.org/licenses/LICENSE-2.0|https://apache.org/licenses/LICENSE-2.0|' \
            >"$1/bad.go"
    printf 'package fixture\n' >>"$1/bad.go"
}

no_blank_line_after_header() { # <dir>
    { printf '%s\n' "$HEADER"; printf 'package fixture\n'; } >"$1/bad.go"
}

header_then_doc_comment() { # <dir> — the most common shape in this repository
    {
        printf '%s\n\n' "$HEADER"
        printf '// Package fixture does a thing.\n'
        printf 'package fixture\n'
    } >"$1/documented.go"
}

unheadered_vendor_file() { # <dir>
    mkdir -p "$1/vendor/example.com/dep"
    printf 'package dep\n' >"$1/vendor/example.com/dep/dep.go"
}

unheadered_non_go_file() { # <dir>
    printf '#!/bin/sh\necho hi\n' >"$1/script.sh"
}

empty_tree() { # <dir> — remove the only Go file, leaving nothing to check
    rm -f "$1/good.go"
}

# --- harness ----------------------------------------------------------------

# assert_check <description> <expected exit code> [fixture function...]
assert_check() {
    local desc="$1" want="$2"
    shift 2

    local tmp
    tmp=$(mktemp -d)
    base_fixture "$tmp"

    if [ "$#" -gt 0 ]; then
        # A fixture helper that silently fails makes the case vacuous. Surface it.
        if ! "$1" "$tmp"; then
            echo "  ERROR    $desc (the test's own fixture '$1' failed to build)"
            fail=$((fail + 1))
            rm -rf "$tmp"
            return
        fi
    fi

    local got=0 output
    output=$(cd "$tmp" && "$CHECK" 2>&1) || got=$?

    if [ "$got" -eq "$want" ]; then
        echo "  ok       $desc (exit $got)"
        pass=$((pass + 1))
    else
        echo "  NOT OK   $desc (expected exit $want, got $got)"
        echo "$output" | sed 's/^/             > /'
        fail=$((fail + 1))
    fi

    rm -rf "$tmp"
}

# assert_reports <description> <fixture function> <expected substring>
# Exit codes alone cannot tell "failed for the right reason" from "failed at
# all". These cases pin the message so a misclassified file is caught.
assert_reports() {
    local desc="$1" fixture="$2" want="$3"

    local tmp
    tmp=$(mktemp -d)
    base_fixture "$tmp"
    "$fixture" "$tmp"

    local output
    output=$(cd "$tmp" && "$CHECK" 2>&1) || true

    if grep -qF "$want" <<<"$output"; then
        echo "  ok       $desc"
        pass=$((pass + 1))
    else
        echo "  NOT OK   $desc (output did not contain: $want)"
        echo "$output" | sed 's/^/             > /'
        fail=$((fail + 1))
    fi

    rm -rf "$tmp"
}

echo "license-header-check.sh self-test"

# The anchor. If the committed tree does not pass, every fixture case below is
# testing a script that disagrees with the repository, and this is also what
# catches the canonical constant drifting from what the files actually carry.
echo "  -- the committed tree --"
committed=0
(cd "$REPO_ROOT" && "$CHECK" >/dev/null 2>&1) || committed=$?
if [ "$committed" -eq 0 ]; then
    echo "  ok       every committed Go file carries the header (exit 0)"
    pass=$((pass + 1))
else
    echo "  NOT OK   the committed tree does not pass its own gate (exit $committed)"
    (cd "$REPO_ROOT" && "$CHECK" 2>&1 | tail -20 | sed 's/^/             > /') || true
    fail=$((fail + 1))
fi

# --- positive cases ---------------------------------------------------------
# Without these, a script that failed unconditionally would pass every negative
# case below.
echo "  -- shapes that must pass --"

assert_check "a conforming file passes" 0

# The dominant shape in this repository: licence header, blank line, then the
# package doc comment. A check that demanded `package` on line 15 would break it.
assert_check "header followed by a package doc comment passes" 0 \
    header_then_doc_comment

# vendor/ files carry their upstream's licence, not ours.
assert_check "an unheadered file under vendor/ is excluded" 0 \
    unheadered_vendor_file

assert_check "a non-Go file needs no header" 0 \
    unheadered_non_go_file

# --- negative cases ---------------------------------------------------------
echo "  -- shapes that must fail --"

# The 53 files from SOL-152896.
assert_check "a file with no header fails" 1 \
    no_header

# Every other negative fixture writes to the fixture ROOT, so without this case
# nothing proves the walk descends at all. Adding `-o -name internal` to the
# prune list in the check silently stops a whole subtree being examined, and the
# committed-tree anchor above cannot notice: pruning a passing subtree still
# passes. This case is the only thing standing between that edit and a green run.
assert_check "a file with no header in a subdirectory fails" 1 \
    nested_no_header

# The copyright line is header line 1, and no other fixture varies it — the
# reworded-header case below changes the licence URL on line 7. Relaxing the
# comparison to lines 2-14, which is the obvious way to "fix" a stale year, must
# not go unnoticed.
assert_check "a stale copyright year fails the check" 1 \
    stale_copyright_year

assert_check "a foreign copyright line fails the check" 1 \
    foreign_copyright

# Silent pass 1: `grep -L` over the whole file passes this. The licence text is
# present, so a substring search finds it — but the top of the file, which is
# where a reader and every licence scanner look, is bare.
assert_check "a header below the package clause fails" 1 \
    header_below_package

# Silent pass 2: one character changed in the licence URL. Any substring or
# fuzzy match passes this, and the next variant after it.
assert_check "a reworded header fails" 1 \
    reworded_header

# Silent pass 3: the 13 header lines are byte-perfect and the file still fails,
# because Go binds a comment block touching `package` as the package doc
# comment. Checking the header lines alone would pass this.
assert_check "a header with no blank line before package fails" 1 \
    no_blank_line_after_header

# Silent pass 4: a walk that matches nothing has nothing to complain about.
assert_check "a tree with no Go files fails rather than reporting success" 1 \
    empty_tree

# --- classification ---------------------------------------------------------
# --fix rewrites one category and refuses the other, so misclassification is a
# real defect: it would either skip a fixable file or prepend a second header.
echo "  -- failures are classified correctly --"

assert_reports "a headerless file is reported as having no licence text" \
    no_header "No Apache-2.0 licence header"

assert_reports "a misplaced header is reported as licence text in the wrong form" \
    header_below_package "Licence text is present"

# --- --fix round trip -------------------------------------------------------
echo "  -- --fix --"

fix_tmp=$(mktemp -d)
base_fixture "$fix_tmp"
no_header "$fix_tmp"
header_then_doc_comment "$fix_tmp"

fix_exit=0
(cd "$fix_tmp" && "$CHECK" --fix >/dev/null 2>&1) || fix_exit=$?

if [ "$fix_exit" -eq 0 ] && (cd "$fix_tmp" && "$CHECK" >/dev/null 2>&1); then
    echo "  ok       --fix makes a headerless tree pass the check"
    pass=$((pass + 1))
else
    echo "  NOT OK   --fix did not produce a conforming tree (fix exited $fix_exit)"
    fail=$((fail + 1))
fi

# The point of --fix is that nobody hand-copies the header again, so what it
# writes must be exactly what the gate demands, and it must not disturb the file
# it prepends to.
if diff -q <(head -n 13 "$fix_tmp/bad.go") <(printf '%s\n' "$HEADER") >/dev/null &&
    [ -z "$(sed -n '14p' "$fix_tmp/bad.go")" ] &&
    [ "$(sed -n '15p' "$fix_tmp/bad.go")" = "package fixture" ]; then
    echo "  ok       --fix writes the canonical header above the original content"
    pass=$((pass + 1))
else
    echo "  NOT OK   --fix produced an unexpected file"
    head -16 "$fix_tmp/bad.go" | sed 's/^/             > /'
    fail=$((fail + 1))
fi

# An already-conforming file must come out byte-identical, or a repo-wide --fix
# would churn every file it touches.
if diff -q "$fix_tmp/documented.go" <(
    printf '%s\n\n// Package fixture does a thing.\npackage fixture\n' "$HEADER"
) >/dev/null; then
    echo "  ok       --fix leaves an already-conforming file untouched"
    pass=$((pass + 1))
else
    echo "  NOT OK   --fix modified a file that already conformed"
    fail=$((fail + 1))
fi
rm -rf "$fix_tmp"

# The documented remedy for rolling the copyright range is "edit CANONICAL_HEADER
# and run --fix". That instruction was false in the first version of this script:
# a year-differing file was classified alongside reworded headers and refused, so
# --fix repaired zero of 260 files and exited 1. Assert the remedy works.
year_tmp=$(mktemp -d)
base_fixture "$year_tmp"
stale_copyright_year "$year_tmp"
year_exit=0
(cd "$year_tmp" && "$CHECK" --fix >/dev/null 2>&1) || year_exit=$?

if [ "$year_exit" -eq 0 ] && (cd "$year_tmp" && "$CHECK" >/dev/null 2>&1) &&
    [ "$(sed -n '15p' "$year_tmp/bad.go")" = "package fixture" ] &&
    [ "$(grep -c . "$year_tmp/bad.go")" -eq 14 ]; then
    echo "  ok       --fix updates a stale copyright line and keeps the rest of the file"
    pass=$((pass + 1))
else
    echo "  NOT OK   --fix did not repair a stale copyright year (exit $year_exit)"
    head -16 "$year_tmp/bad.go" | sed 's/^/             > /'
    fail=$((fail + 1))
fi
rm -rf "$year_tmp"

# The tolerance above must stay narrow. Someone else's copyright notice is not
# ours to rewrite, so it is reported and left exactly as found.
foreign_tmp=$(mktemp -d)
base_fixture "$foreign_tmp"
foreign_copyright "$foreign_tmp"
foreign_before=$(cat "$foreign_tmp/bad.go")
foreign_exit=0
(cd "$foreign_tmp" && "$CHECK" --fix >/dev/null 2>&1) || foreign_exit=$?

if [ "$foreign_exit" -eq 1 ] && [ "$(cat "$foreign_tmp/bad.go")" = "$foreign_before" ]; then
    echo "  ok       --fix refuses a third party's copyright line and leaves it alone"
    pass=$((pass + 1))
else
    echo "  NOT OK   --fix should have refused the foreign copyright unchanged (exit $foreign_exit)"
    fail=$((fail + 1))
fi
rm -rf "$foreign_tmp"

# The script claims to preserve file modes. An untested claim in a comment is
# the kind of prose that goes stale silently, and a --fix that reset modes to the
# umask default would be found by whoever next runs it on a checked-in script.
mode_tmp=$(mktemp -d)
base_fixture "$mode_tmp"
no_header "$mode_tmp"
chmod 0640 "$mode_tmp/bad.go"
(cd "$mode_tmp" && "$CHECK" --fix >/dev/null 2>&1) || true
mode_after=$(ls -l "$mode_tmp/bad.go" | cut -c1-10)
if [ "$mode_after" = "-rw-r-----" ]; then
    echo "  ok       --fix preserves the file mode ($mode_after)"
    pass=$((pass + 1))
else
    echo "  NOT OK   --fix changed the file mode (expected -rw-r-----, got $mode_after)"
    fail=$((fail + 1))
fi
rm -rf "$mode_tmp"

# --fix must refuse a file it cannot safely rewrite. Prepending to a file that
# already has licence text somewhere would leave it with two headers.
refuse_tmp=$(mktemp -d)
base_fixture "$refuse_tmp"
header_below_package "$refuse_tmp"
before=$(cat "$refuse_tmp/bad.go")

refuse_exit=0
(cd "$refuse_tmp" && "$CHECK" --fix >/dev/null 2>&1) || refuse_exit=$?

if [ "$refuse_exit" -eq 1 ] && [ "$(cat "$refuse_tmp/bad.go")" = "$before" ]; then
    echo "  ok       --fix refuses a file that already has licence text, and leaves it alone"
    pass=$((pass + 1))
else
    echo "  NOT OK   --fix should have refused the file unchanged (exit $refuse_exit)"
    fail=$((fail + 1))
fi
rm -rf "$refuse_tmp"

# --- argument handling ------------------------------------------------------
# A typo'd flag must not be read as "check mode" and report a misleading pass.
echo "  -- arguments --"
arg_tmp=$(mktemp -d)
base_fixture "$arg_tmp"
arg_exit=0
(cd "$arg_tmp" && "$CHECK" --fixx >/dev/null 2>&1) || arg_exit=$?
if [ "$arg_exit" -eq 2 ]; then
    echo "  ok       an unknown flag exits 2 rather than silently checking (exit $arg_exit)"
    pass=$((pass + 1))
else
    echo "  NOT OK   an unknown flag should exit 2, got $arg_exit"
    fail=$((fail + 1))
fi
rm -rf "$arg_tmp"

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
