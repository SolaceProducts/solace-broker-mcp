#!/usr/bin/env bash
#
# Self-test for refresh-licenses-inventory.sh (SOL-152956).
#
# Same shape as licenses-check.test.sh: mutations touch only a copy, and the
# tree symlinks the real module so expected values come from a real `go list`
# rather than a fixture that goes stale. The one addition this suite needs
# that licenses-check.test.sh doesn't: refresh-licenses-inventory.sh itself
# calls licenses-check.sh (to find out what to fix, and again to verify the
# fix), and its Go-module-license path shells out to `go run
# github.com/google/go-licenses@v1.6.0` and, for actions, `gh api` — so this
# suite needs network access and the module/build caches warm, unlike the
# check scripts' own self-tests. Not worth mocking: a fake network response
# proves the parser handles a string, not that the automation resolves a real
# licence, which is the actual thing worth being sure of. It's also slower —
# expect this to take noticeably longer than licenses-check.test.sh's own run.
#
# Every case asserts on two things, not one: the refresh script's own exit
# code, and — for the fix/add/drop cases — that the row it produced is
# byte-for-byte identical to the row in the *computed* baseline (the real
# refresh script run once against an unmutated copy of the repo), not the
# committed file. The committed file is stale on every fresh Dependabot PR
# (SOL-153454); treating it as ground truth made this suite fail for the
# exact situation the refresh scripts exist to fix. A refresh that exits 0
# by coincidentally producing *a* row that also happens to satisfy
# licenses-check.sh (e.g. a differently-cased licence name that still
# resolves) would pass on exit code alone and be wrong regardless.
#
# SKIP_NETWORK_TESTS=1 skips the one case that shells out to `go run
# github.com/google/go-licenses@v1.6.0` (network, to fetch the tool itself on
# a cold cache). Unset/0 by default so a local or manual run gets full
# coverage; CI sets it for the fast, offline, cross-platform-matrixed leg of
# this suite, keeping a module-proxy outage from being able to block a merge
# on a required check — the every-case-needs-network shape this file used to
# have coupled a required context's reliability to an external service with
# no bearing on whether the inventory logic itself is correct.
#
# Usage: .github/scripts/refresh-licenses-inventory.test.sh

set -uo pipefail

REPO_ROOT=$(git rev-parse --show-toplevel)
REFRESH="$REPO_ROOT/.github/scripts/refresh-licenses-inventory.sh"
DOC="THIRD_PARTY_LICENSES.md"
NOTICE_FILE="NOTICE"

# REFRESH_NOTICE=1 (SOL-159006) changes what the refresh script may write.
# Every case runs with it off unless the case itself turns it on
# (`REFRESH_NOTICE=1 assert_refresh ...`), so an ambient value in the caller's
# shell cannot silently turn the mode-off cases into mode-on ones.
unset REFRESH_NOTICE

pass=0
fail=0
skip=0

# Suite-level scratch: holds the computed baseline and the ambient-drift
# fixture. Cleaned on EXIT so a Ctrl-C mid-run doesn't leak tmpdirs.
SUITE_TMP=$(mktemp -d)
CONVERGED_DOC="$SUITE_TMP/$DOC"
trap 'rm -rf "$SUITE_TMP"' EXIT

# skip_network <description>
#   Prints a "skip" line and stops the caller from reporting it as pass/fail.
#   Used only when SKIP_NETWORK_TESTS=1.
skip_network() {
    echo "  skip     $1 (SKIP_NETWORK_TESTS=1)"
    skip=$((skip + 1))
}

# --- mutations ---------------------------------------------------------------
# Same idiom as licenses-check.test.sh's own drop_row/change_version, applied
# to the tmp copy the harness below builds.

drop_row() { # <tmp> <component>
    grep -vF "\`$2\`" "$1/$DOC" >"$1/t" && mv "$1/t" "$1/$DOC"
}

change_version() { # <tmp> <component> <wrong-version>
    awk -v comp="\`$2\`" -v ver="$3" -F' \\| ' '
        index($0, comp) && /^\| `/ { $2 = ver; print $1 " | " $2 " | " $3 " | " $4; next }
        { print }
    ' "$1/$DOC" >"$1/t" && mv "$1/t" "$1/$DOC"
}

add_stale_row() { # <tmp> <name-not-in-closure>
    printf '| `%s` | v1.0.0 | MIT | [license](https://example.invalid/LICENSE) |\n' "$2" >>"$1/$DOC"
}

add_unparseable_row() { # <tmp> — an error class this script must refuse, not fix
    printf '| `weird thing` | v0.1.0 (vendored) | Apache-2.0 | [license](x) |\n' >>"$1/$DOC"
}

drop_required_component_row() { # <tmp> <component>
    # licenses-check.sh's REQUIRED_COMPONENTS check ("must keep its own row")
    # is a fourth message shape this script's parser does not recognise (it
    # names no version, add/fix/drop pattern doesn't apply), and must never
    # be treated as an ordinary stale row to reinstate however it sees fit —
    # the point of the check is that a human verified this component's
    # licence differs from its parent's, and only a human re-adding it can
    # attest that again.
    grep -vF "\`$2\`" "$1/$DOC" >"$1/t" && mv "$1/t" "$1/$DOC"
}

drop_notice_block() { # <tmp> <module-path> — delete that block from NOTICE
    # The block runs from its header line to the line before the next
    # header (a non-indented line after a blank one); its trailing blank line
    # goes with it. The last block has no trailing blank line, so the blank
    # line before it goes instead. Fails if no such block exists.
    awk -v hdr="$2" '
        { lines[NR] = $0 }
        END {
            s = 0
            for (i = 2; i <= NR; i++) if (lines[i] == hdr && lines[i - 1] == "") { s = i; break }
            if (!s) exit 1
            e = NR
            for (i = s + 1; i <= NR; i++) {
                if (lines[i] != "" && lines[i] !~ /^ / && lines[i - 1] == "") { e = i - 1; break }
            }
            if (e == NR) s--
            for (i = 1; i <= NR; i++) if (i < s || i > e) print lines[i]
        }
    ' "$1/$NOTICE_FILE" >"$1/t" && mv "$1/t" "$1/$NOTICE_FILE"
}

drop_notice_block_and_change_version() { # <tmp> <module-path> <component> <wrong-version>
    drop_notice_block "$1" "$2" && change_version "$1" "$3" "$4"
}

drop_notice_block_change_version_and_drop_section() { # <tmp> <module-path> <component> <wrong-version>
    # A NOTICE gap the script can resolve, plus an inventory fix, in a NOTICE
    # whose "Required attributions" heading is gone, so insert_notice_block
    # must refuse. That refusal has to come before the inventory is written.
    drop_notice_block_and_change_version "$@" &&
        grep -vx 'Required attributions propagated from bundled dependencies' "$1/$NOTICE_FILE" >"$1/t" &&
        mv "$1/t" "$1/$NOTICE_FILE"
}

drop_notice_block_and_add_unparseable_row() { # <tmp> <module-path>
    drop_notice_block "$1" "$2" && add_unparseable_row "$1"
}

# --- harness -------------------------------------------------------------
#
# INVENTORY_SOURCE (optional): path of the inventory file copied into the
# fixture. Defaults to the committed $REPO_ROOT/$DOC. The ambient-drift case
# points this at a stale copy so the suite exercises SOL-153454 on main.
#
# CONVERGED_DOC: computed once per run by compute_converged_baseline, by
# running the real refresh script against an unmutated copy of the repo.
# Exit-0 cases whole-file-diff and per-row-diff against this, not against
# the committed file.
#
# assert_refresh <description> <expected exit code> <row-to-check-after|-> <mutation fn + args>
#   <row-to-check-after> is the exact backticked component name whose row
#   must, after a successful (exit-0) run, be byte-identical to the row for
#   that name in CONVERGED_DOC — pass "-" to skip that comparison
#   (the refusal cases, where nothing should have changed at all).

setup_refresh_fixture() { # <tmp>
    local tmp="$1"
    ln -s "$REPO_ROOT/go.mod" "$tmp/go.mod"
    ln -s "$REPO_ROOT/go.sum" "$tmp/go.sum"
    ln -s "$REPO_ROOT/cmd" "$tmp/cmd"
    ln -s "$REPO_ROOT/internal" "$tmp/internal"
    ln -s "$REPO_ROOT/test" "$tmp/test"
    ln -s "$REPO_ROOT/.github" "$tmp/.github"
    cp "${INVENTORY_SOURCE:-$REPO_ROOT/$DOC}" "$tmp/$DOC"
    cp "$REPO_ROOT/$NOTICE_FILE" "$tmp/$NOTICE_FILE"
}

# compute_converged_baseline
#   Runs refresh against an unmutated fixture and saves the result as
#   CONVERGED_DOC. Aborts the suite if that run does not exit 0 — every
#   later exit-0 case is meaningless without this oracle. Not reachable
#   today (this always runs before any case sets INVENTORY_SOURCE), but
#   shadowed with a local, unset value regardless: the baseline must stay
#   an unmutated oracle even if a future case sets INVENTORY_SOURCE before
#   calling this.
compute_converged_baseline() {
    local tmp out_file got=0 INVENTORY_SOURCE=
    tmp=$(mktemp -d)
    out_file=$(mktemp "$tmp/refresh-out.XXXXXX")
    setup_refresh_fixture "$tmp"
    echo "  computing converged baseline from an unmutated copy"
    (cd "$tmp" && "$REFRESH" >"$out_file" 2>&1) || got=$?
    if [ "$got" -ne 0 ]; then
        echo "  FATAL    could not compute converged baseline (refresh exited $got)"
        echo "           --- refresh output ---"
        sed 's/^/           /' "$out_file"
        rm -rf "$tmp"
        exit 1
    fi
    cp "$tmp/$DOC" "$CONVERGED_DOC"
    rm -rf "$tmp"
}

assert_refresh() {
    local desc="$1" want="$2" check_row="$3"
    shift 3

    local tmp
    tmp=$(mktemp -d)

    setup_refresh_fixture "$tmp"

    if [ "$#" -gt 0 ]; then
        local fn="$1"
        shift
        if ! "$fn" "$tmp" "$@"; then
            echo "  ERROR    $desc (the test's own mutation '$fn' failed)"
            fail=$((fail + 1))
            rm -rf "$tmp"
            return
        fi
    fi

    # Snapshot the mutated-but-not-yet-refreshed file, for the want=1
    # (refusal) branch below: a refusal case has no computed baseline
    # to converge to (it's supposed to change nothing), so "left alone" can
    # only be checked against what the mutation itself produced.
    local pre_refresh_snapshot pre_refresh_notice
    pre_refresh_snapshot=$(mktemp "$tmp/refresh-pre.XXXXXX")
    cp "$tmp/$DOC" "$pre_refresh_snapshot"
    pre_refresh_notice=$(mktemp "$tmp/refresh-pre-notice.XXXXXX")
    cp "$tmp/$NOTICE_FILE" "$pre_refresh_notice"

    # Scratch files live under $tmp, not a fixed /tmp/...$$ path: colocating
    # with the rest of this case's fixture means the single `rm -rf "$tmp"` at
    # each return already cleans them up (no separate rm of a sibling path to
    # remember), and a real mktemp name rules out any chance of collision
    # across concurrent runs sharing one PID's $$ value.
    local out_file
    out_file=$(mktemp "$tmp/refresh-out.XXXXXX")

    local got=0
    (cd "$tmp" && "$REFRESH" >"$out_file" 2>&1) || got=$?

    if [ "$got" -ne "$want" ]; then
        echo "  NOT OK   $desc (expected exit $want, got $got)"
        echo "           --- refresh output ---"
        sed 's/^/           /' "$out_file"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi

    if [ "$check_row" != "-" ]; then
        local got_row want_row
        got_row=$(grep -F "\`$check_row\`" "$tmp/$DOC" || true)
        want_row=$(grep -F "\`$check_row\`" "$CONVERGED_DOC" || true)
        if [ "$got_row" != "$want_row" ]; then
            echo "  NOT OK   $desc (exit $got matched, but the row for \`$check_row\` did not converge)"
            echo "           got:  $got_row"
            echo "           want: $want_row"
            fail=$((fail + 1))
            rm -rf "$tmp"
            return
        fi
    fi

    # Every exit-0 case here should converge to exactly the computed
    # baseline, not merely make the one row this case names look right —
    # checking only a single row is exactly how a sibling bug slipped past
    # refresh-build-test-inventory.test.sh (a missing fixture input made the
    # real script correctly, but unexpectedly, drop two unrelated rows that no
    # single-row check happened to be watching). No known equivalent gap
    # exists in this fixture today, but the check is cheap and the failure
    # mode it guards against is silent, so it stays on regardless.
    #
    # The "Generated" date line is excluded from both sides before comparing:
    # refresh-licenses-inventory.sh deliberately bumps it as housekeeping on
    # any real rewrite (see its own comment), so it legitimately differs from
    # the baseline whenever "today" isn't the date that file happens to
    # carry — that is the intended behaviour, not drift to catch.
    if [ "$want" -eq 0 ]; then
        local diff_file
        diff_file=$(mktemp "$tmp/refresh-diff.XXXXXX")
        if ! diff -u \
            <(grep -v '^\*\*Generated\*\*' "$CONVERGED_DOC") \
            <(grep -v '^\*\*Generated\*\*' "$tmp/$DOC") \
            >"$diff_file" 2>&1; then
            echo "  NOT OK   $desc (exit 0, but the file didn't fully converge to the computed baseline)"
            sed 's/^/           /' "$diff_file"
            fail=$((fail + 1))
            rm -rf "$tmp"
            return
        fi
        # NOTICE (SOL-159006): every exit-0 case must end with exactly the
        # committed NOTICE. The mode-off cases never touch it; the
        # REFRESH_NOTICE=1 cases delete a committed block and must put it
        # back byte-identical. The blocks they use (coreos/go-oidc/v3, grpc)
        # are upstream's NOTICE files verbatim, so this is the "copied
        # verbatim" check. Not every committed block is: the jsonpointer one
        # and the yaml group are hand-written, so no case drops those.
        if ! diff -u "$REPO_ROOT/$NOTICE_FILE" "$tmp/$NOTICE_FILE" >"$diff_file" 2>&1; then
            echo "  NOT OK   $desc (exit 0, but $NOTICE_FILE differs from the committed one)"
            sed 's/^/           /' "$diff_file"
            fail=$((fail + 1))
            rm -rf "$tmp"
            return
        fi
    else
        # Same contract for NOTICE: a refusal writes nothing to it either.
        if ! cmp -s "$pre_refresh_notice" "$tmp/$NOTICE_FILE"; then
            echo "  NOT OK   $desc (exit $got matched, but $NOTICE_FILE was touched despite the refusal)"
            diff -u "$pre_refresh_notice" "$tmp/$NOTICE_FILE" | sed 's/^/           /'
            fail=$((fail + 1))
            rm -rf "$tmp"
            return
        fi
        # A refusal must leave the file exactly as the mutation left it — not
        # merely "exit nonzero". This is the check that would catch a
        # regression where the script fails loudly but still writes a partial
        # fix first; nothing exercised that until now.
        local refusal_diff_file
        refusal_diff_file=$(mktemp "$tmp/refresh-refusal-diff.XXXXXX")
        if ! diff -u "$pre_refresh_snapshot" "$tmp/$DOC" >"$refusal_diff_file" 2>&1; then
            echo "  NOT OK   $desc (exit $got matched, but the file was touched despite the refusal)"
            sed 's/^/           /' "$refusal_diff_file"
            fail=$((fail + 1))
            rm -rf "$tmp"
            return
        fi
    fi

    echo "  ok       $desc (exit $got)"
    pass=$((pass + 1))
    rm -rf "$tmp"
}

echo "refresh-licenses-inventory.sh self-test"

# --- normalize_pseudo_version, in isolation ---------------------------------
# A direct unit test, not exercised through assert_refresh above: forcing a
# real commit-pinned dependency to bump would need a live upstream commit to
# land during the test run, which isn't something to depend on. This is a
# pure string transform with no side effects, so testing it directly is both
# sufficient and exact — no build/network round-trip needed to know it's
# right. Guards against the bug this function exists to fix: writing a
# version-bump row for a commit-pinned module (github.com/xeipuuv/* today) in
# its raw, un-folded pseudo-version form corrupts both the version cell and,
# via substitute_in_row_field, the licence URL built from it.
# shellcheck source=lib/inventory-refresh-common.sh
source "$REPO_ROOT/.github/scripts/lib/inventory-refresh-common.sh"

assert_normalize() {
    local desc="$1" input="$2" want="$3" got
    got=$(normalize_pseudo_version "$input")
    if [ "$got" = "$want" ]; then
        echo "  ok       $desc (\"$input\" -> \"$got\")"
        pass=$((pass + 1))
    else
        echo "  NOT OK   $desc (\"$input\" -> \"$got\", want \"$want\")"
        fail=$((fail + 1))
    fi
}

assert_normalize "a pseudo-version folds to its trailing commit" \
    "v0.0.0-20180127040603-4e3ac2762d5f" "4e3ac2762d5f"
assert_normalize "an ordinary tag passes through unchanged" \
    "v1.2.0" "v1.2.0"
assert_normalize "a prerelease tag passes through unchanged" \
    "v2.0.0-beta.1" "v2.0.0-beta.1"
assert_normalize "a non-v0 pseudo-version-shaped tag passes through unchanged" \
    "v1.2.3-0.20240101000000-abcdef123456" "v1.2.3-0.20240101000000-abcdef123456"

# --- notice_source_file, in isolation (SOL-159006) ---------------------------
# Picks the one root NOTICE file of a module directory that REFRESH_NOTICE=1
# may copy, and refuses every shape the ticket says must go to a person.
# Direct unit tests: a real module carrying two NOTICE files or a binary one
# is not something the dependency closure can be made to produce on demand.

# assert_notice_source <description> <want: ok|refuse> <setup fn>
#   <setup fn> populates a fresh module dir passed as $1. On "ok" the helper
#   must print the path of the single file the setup created whose name
#   starts with NOTICE (any case).
assert_notice_source() {
    local desc="$1" want="$2" setup="$3" dir got rc=0
    dir=$(mktemp -d "$SUITE_TMP/mod.XXXXXX")
    "$setup" "$dir"
    got=$(notice_source_file "$dir" 2>/dev/null) || rc=$?
    if [ "$want" = ok ]; then
        local expected
        expected=$(find "$dir" -maxdepth 1 -iname 'NOTICE*')
        if [ "$rc" -eq 0 ] && [ "$got" = "$expected" ]; then
            echo "  ok       $desc"
            pass=$((pass + 1))
        else
            echo "  NOT OK   $desc (rc=$rc, got \"$got\", want \"$expected\")"
            fail=$((fail + 1))
        fi
    elif [ "$rc" -eq 1 ] && [ -z "$got" ]; then
        echo "  ok       $desc (refused)"
        pass=$((pass + 1))
    else
        echo "  NOT OK   $desc (expected a refusal, got rc=$rc, \"$got\")"
        fail=$((fail + 1))
    fi
}

mk_notice_plain() { printf 'Example Project\nCopyright 2020 Example\n' >"$1/NOTICE"; }
mk_notice_txt_lower() { printf 'Example Project\n' >"$1/notice.txt"; }
mk_notice_two() { printf 'a\n' >"$1/NOTICE"; printf 'b\n' >"$1/NOTICE.txt"; }
mk_notice_none() { printf 'x\n' >"$1/LICENSE"; }
mk_notice_md() { printf '# Notice\n' >"$1/NOTICE.md"; }
mk_notice_empty() { : >"$1/NOTICE"; }
mk_notice_blank() { printf '  \n\t\n\n' >"$1/NOTICE"; }
mk_notice_nul() { printf 'Example\000Project\n' >"$1/NOTICE"; }
mk_notice_latin1() { printf 'Copyright 2020 Fran\347ois\n' >"$1/NOTICE"; }
mk_notice_utf8() { printf 'Copyright 2020 Fran\303\247ois\n' >"$1/NOTICE"; }
mk_notice_crlf() { printf 'Example Project\r\nCopyright 2020 Example\r\n' >"$1/NOTICE"; }
mk_notice_bom() { printf '\357\273\277Example Project\n' >"$1/NOTICE"; }

assert_notice_source "a single NOTICE file is accepted" ok mk_notice_plain
assert_notice_source "a single lower-case notice.txt is accepted" ok mk_notice_txt_lower
assert_notice_source "non-ASCII UTF-8 text is accepted" ok mk_notice_utf8
assert_notice_source "two NOTICE files refuse" refuse mk_notice_two
assert_notice_source "no NOTICE file refuses" refuse mk_notice_none
assert_notice_source "a NOTICE.md (not plain text by name) refuses" refuse mk_notice_md
assert_notice_source "an empty NOTICE refuses" refuse mk_notice_empty
assert_notice_source "a whitespace-only NOTICE refuses" refuse mk_notice_blank
assert_notice_source "a NOTICE with a NUL byte refuses" refuse mk_notice_nul
assert_notice_source "a NOTICE that is not valid UTF-8 refuses" refuse mk_notice_latin1
# Copying either verbatim would put stray \r or a mid-file BOM into NOTICE.
assert_notice_source "a NOTICE with CRLF line endings refuses" refuse mk_notice_crlf
assert_notice_source "a NOTICE with a UTF-8 byte-order mark refuses" refuse mk_notice_bom

# --- insert_notice_block, in isolation (SOL-159006) --------------------------
# Synthetic NOTICE shaped like the real one: a preamble, a separator, the
# "Required attributions" section with an intro paragraph, then blocks. The
# second block is a hand-merged group whose sort key is its smallest header
# (c.example/y), the same rule that places the real yaml group.

NOTICE_SYNTH_HEAD='Example Product

--------------------------------------------------------------------------------
Required attributions propagated from bundled dependencies

Intro paragraph that names no module
and spans two lines.
'
NOTICE_SYNTH_B='b.example/one
  B notice'
NOTICE_SYNTH_GROUP='d.example/x
c.example/y
  Group notice

  second paragraph'
NOTICE_SYNTH_E='e.example/five
  E notice'
# What the fixed upstream NOTICE below must become: 2-space indent on
# non-blank lines (tabs inside kept), blank lines empty, trailing blank and
# whitespace-only lines dropped.
NOTICE_SYNTH_NEW='new.example/mod
  Line one
  	tab-indented line kept as is

  Line after blank'

# write_synth_notice <path> <block>... — head, then blocks separated by one
# blank line, ending with a single newline, exactly like the committed file.
write_synth_notice() {
    local path="$1" first=1 b
    shift
    { printf '%s\n' "$NOTICE_SYNTH_HEAD"
      for b in "$@"; do
          [ "$first" -eq 1 ] || printf '\n'
          first=0
          printf '%s\n' "$b"
      done; } >"$path"
}

# assert_insert_notice <description> <mod_path> <expected block order...>
#   Inserts a fixed upstream NOTICE for <mod_path> into the synthetic NOTICE
#   and compares the whole result with the synthetic NOTICE rebuilt in the
#   expected order. "NEW" in the order list stands for the inserted block.
assert_insert_notice() {
    local desc="$1" mod="$2" dir src notice want_file b
    shift 2
    dir=$(mktemp -d "$SUITE_TMP/ins.XXXXXX")
    src="$dir/upstream-NOTICE"
    notice="$dir/NOTICE"
    printf 'Line one\n\ttab-indented line kept as is\n\nLine after blank\n\n \n' >"$src"
    write_synth_notice "$notice" "$NOTICE_SYNTH_B" "$NOTICE_SYNTH_GROUP" "$NOTICE_SYNTH_E"
    local new_block="${NOTICE_SYNTH_NEW/new.example\/mod/$mod}" ordered=()
    for b in "$@"; do
        case "$b" in
            B) ordered+=("$NOTICE_SYNTH_B") ;;
            GROUP) ordered+=("$NOTICE_SYNTH_GROUP") ;;
            E) ordered+=("$NOTICE_SYNTH_E") ;;
            NEW) ordered+=("$new_block") ;;
        esac
    done
    want_file="$dir/want"
    write_synth_notice "$want_file" "${ordered[@]}"
    local rc=0
    insert_notice_block "$notice" "$mod" "$src" 2>"$dir/err" || rc=$?
    if [ "$rc" -ne 0 ]; then
        echo "  NOT OK   $desc (insert_notice_block exited $rc: $(cat "$dir/err"))"
        fail=$((fail + 1))
    elif ! diff -u "$want_file" "$notice" >"$dir/diff"; then
        echo "  NOT OK   $desc (NOTICE differs from expected)"
        sed 's/^/           /' "$dir/diff"
        fail=$((fail + 1))
    else
        echo "  ok       $desc"
        pass=$((pass + 1))
    fi
}

assert_insert_notice "a block sorting first goes before every existing block" \
    "a.example/first" NEW B GROUP E
assert_insert_notice "a group sorts by its smallest header, so c.example/a lands before it" \
    "c.example/a" B NEW GROUP E
assert_insert_notice "a path after the group's smallest header lands after the group" \
    "c.example/z" B GROUP NEW E
assert_insert_notice "a block sorting last is appended at the end of the file" \
    "f.example/last" B GROUP E NEW

# assert_insert_notice_refuses <description> <notice content> <mod_path>
assert_insert_notice_refuses() {
    local desc="$1" content="$2" mod="$3" dir rc=0
    dir=$(mktemp -d "$SUITE_TMP/insr.XXXXXX")
    printf 'Notice text\n' >"$dir/src"
    printf '%s\n' "$content" >"$dir/NOTICE"
    cp "$dir/NOTICE" "$dir/before"
    insert_notice_block "$dir/NOTICE" "$mod" "$dir/src" 2>/dev/null || rc=$?
    if [ "$rc" -eq 1 ] && cmp -s "$dir/before" "$dir/NOTICE"; then
        echo "  ok       $desc (refused, NOTICE untouched)"
        pass=$((pass + 1))
    else
        echo "  NOT OK   $desc (rc=$rc; NOTICE must be refused and left untouched)"
        fail=$((fail + 1))
    fi
}

assert_insert_notice_refuses "a NOTICE without the attributions section refuses" \
    "Example Product" "x.example/mod"
assert_insert_notice_refuses "a module that already has a block refuses rather than duplicating" \
    "$NOTICE_SYNTH_HEAD
$NOTICE_SYNTH_B" "b.example/one"

compute_converged_baseline

# The baseline: nothing to do, exit 0, no crash on an already-clean tree. If
# this fails, every case below is meaningless.
assert_refresh "an already-matching file is a clean no-op" 0 -

# SOL-153454: the committed inventory is not ground truth. A Dependabot PR
# leaves this file stale against go.mod; the harness must still pass. This
# case manufactures that shape on main so CI covers it without a live
# Dependabot PR. The stale copy is the starting document only — comparison
# is still against CONVERGED_DOC.
stale_dir="$SUITE_TMP/stale"
mkdir -p "$stale_dir"
cp "$REPO_ROOT/$DOC" "$stale_dir/$DOC"
if ! change_version "$stale_dir" "golang.org/x/sync" "v0.0.1"; then
    echo "  ERROR    ambient inventory drift (could not stale golang.org/x/sync)"
    fail=$((fail + 1))
elif diff -q \
    <(grep -v '^\*\*Generated\*\*' "$stale_dir/$DOC") \
    <(grep -v '^\*\*Generated\*\*' "$CONVERGED_DOC") \
    >/dev/null; then
    echo "  ERROR    ambient inventory drift (stale copy did not diverge from the baseline)"
    fail=$((fail + 1))
else
    INVENTORY_SOURCE="$stale_dir/$DOC"
    assert_refresh "a stale committed inventory still converges to the computed baseline" 0 "golang.org/x/sync"
    assert_refresh "ambient drift plus a further mutation still converges" 0 - \
        add_stale_row "github.com/example/not-a-dependency"
    unset INVENTORY_SOURCE
fi

# Direction 1: a documented row's version drifts from the binary — the
# ordinary Dependabot version-bump case. golang.org/x/sync is pinned in
# go.mod, so a real version is available to converge back to.
assert_refresh "a stale version is fixed to match the binary" 0 "golang.org/x/sync" \
    change_version "golang.org/x/sync" "v0.0.1"

# Direction 2: a component drops out of the row set entirely — the new
# transitive Go module case. Deleting a real row must be repaired to exactly
# what was there before, licence and all, not just a row that also happens to
# pass. The only case in this file that needs network (fetch_go_module_license
# shells out to go-licenses) — see SKIP_NETWORK_TESTS's own comment at the top.
if [ "${SKIP_NETWORK_TESTS:-0}" = "1" ]; then
    skip_network "a missing row is re-added, byte-identical to the original"
else
    assert_refresh "a missing row is re-added, byte-identical to the original" 0 "golang.org/x/sync" \
        drop_row "golang.org/x/sync"
fi

# Direction 3: a documented row for a component that fell out of the closure —
# the reverse-direction drift the ticket calls out explicitly ("the gate fails
# in both directions").
assert_refresh "a stale row for an unused component is dropped" 0 - \
    add_stale_row "github.com/example/not-a-dependency"

# Refusal: an error class this script does not recognise must abort the whole
# run rather than guess. Confirmed by two things — exit 1, and (via
# assert_refresh's own want!=0 branch) that the file is byte-identical to
# what the mutation left it as; the point of this case is specifically that
# nothing gets written at all, not just that the process exits nonzero.
assert_refresh "an unparseable row refuses the whole batch rather than guessing" 1 - \
    add_unparseable_row

# The differing-licence sub-package case (licenses-check.sh's own
# REQUIRED_COMPONENTS list): github.com/go-jose/go-jose/v4/json ships BSD-3-Clause
# while its parent module is Apache-2.0, so it must keep its own row
# independently of the parent's. Dropping it must refuse, not "helpfully"
# reinstate it under some inferred licence — the whole point of the check this
# is exercising is that no automation gets to make that call.
assert_refresh "dropping a required differing-licence row refuses rather than reinstating it under a guess" 1 - \
    drop_required_component_row "github.com/go-jose/go-jose/v4/json"

# --- REFRESH_NOTICE=1 (SOL-159006) ------------------------------------------
# A missing NOTICE block is manufactured by deleting a committed one; the
# harness then requires the refreshed NOTICE to equal the committed file
# byte for byte. Offline: the module cache already holds each module's
# upstream NOTICE once `go mod download` has run.

# coreos/go-oidc/v3 ships a plain NOTICE and its block is early in the file.
REFRESH_NOTICE=1 assert_refresh "REFRESH_NOTICE=1 restores a missing NOTICE block verbatim" 0 - \
    drop_notice_block "github.com/coreos/go-oidc/v3"

# grpc ships NOTICE.txt and its block is the last one in the file, so this
# also covers appending at the end of the section.
REFRESH_NOTICE=1 assert_refresh "REFRESH_NOTICE=1 restores a NOTICE.txt block at the end of the file" 0 - \
    drop_notice_block "google.golang.org/grpc"

REFRESH_NOTICE=1 assert_refresh "REFRESH_NOTICE=1 fixes the inventory and NOTICE in one run" 0 "golang.org/x/sync" \
    drop_notice_block_and_change_version "github.com/coreos/go-oidc/v3" "golang.org/x/sync" "v0.0.1"

# Validation happens before any write: an inventory problem the script
# cannot fix must leave NOTICE untouched too, not half-applied.
REFRESH_NOTICE=1 assert_refresh "REFRESH_NOTICE=1 writes nothing when another diagnostic refuses" 1 - \
    drop_notice_block_and_add_unparseable_row "github.com/coreos/go-oidc/v3"

REFRESH_NOTICE=1 assert_refresh "REFRESH_NOTICE=1 refuses before writing the inventory when NOTICE has no attributions section" 1 - \
    drop_notice_block_change_version_and_drop_section "github.com/coreos/go-oidc/v3" "golang.org/x/sync" "v0.0.1"

# A NOTICE-only fix must not touch THIRD_PARTY_LICENSES.md at all, not even
# its "Generated" date: no row changed, so the inventory was not
# regenerated. assert_refresh ignores that line, so this checks it directly.
notice_only_desc="REFRESH_NOTICE=1 with only a NOTICE gap leaves $DOC byte-identical"
notice_only_tmp=$(mktemp -d "$SUITE_TMP/notice-only.XXXXXX")
INVENTORY_SOURCE="$CONVERGED_DOC" setup_refresh_fixture "$notice_only_tmp"
sed -e 's/^\*\*Generated\*\* [0-9-]*/**Generated** 2000-01-01/' "$notice_only_tmp/$DOC" >"$notice_only_tmp/t" &&
    mv "$notice_only_tmp/t" "$notice_only_tmp/$DOC"
cp "$notice_only_tmp/$DOC" "$notice_only_tmp/doc-before"
notice_only_rc=0
if ! drop_notice_block "$notice_only_tmp" "github.com/coreos/go-oidc/v3"; then
    echo "  ERROR    $notice_only_desc (could not drop the coreos block)"
    fail=$((fail + 1))
else
    (cd "$notice_only_tmp" && REFRESH_NOTICE=1 "$REFRESH" >"$notice_only_tmp/out" 2>&1) || notice_only_rc=$?
    if [ "$notice_only_rc" -ne 0 ]; then
        echo "  NOT OK   $notice_only_desc (expected exit 0, got $notice_only_rc)"
        sed 's/^/           /' "$notice_only_tmp/out"
        fail=$((fail + 1))
    elif ! diff -u "$notice_only_tmp/doc-before" "$notice_only_tmp/$DOC" >"$notice_only_tmp/d"; then
        echo "  NOT OK   $notice_only_desc ($DOC changed)"
        sed 's/^/           /' "$notice_only_tmp/d"
        fail=$((fail + 1))
    else
        echo "  ok       $notice_only_desc"
        pass=$((pass + 1))
    fi
fi

# Mode off is today's behaviour: a NOTICE gap is handed to a person.
assert_refresh "without REFRESH_NOTICE a NOTICE gap still refuses and leaves NOTICE alone" 1 - \
    drop_notice_block "github.com/coreos/go-oidc/v3"

# The #590 shape (go.yaml.in/yaml/v4@v4.0.0, brought in by kin-openapi
# v0.153.0, not yet in this repo's closure): inserted into the committed
# NOTICE, the block must be that version's upstream NOTICE verbatim, under
# its own header between prometheus/procfs and grpc — not merged into the
# yaml v3 group, whose text differs — and nothing else may change. Network:
# the module is downloaded, since main's closure doesn't contain it.
if [ "${SKIP_NETWORK_TESTS:-0}" = "1" ]; then
    skip_network "go.yaml.in/yaml/v4@v4.0.0's NOTICE lands verbatim between procfs and grpc"
else
    v4_desc="go.yaml.in/yaml/v4@v4.0.0's NOTICE lands verbatim between procfs and grpc"
    v4_tmp=$(mktemp -d "$SUITE_TMP/yamlv4.XXXXXX")
    v4_dir=$( (cd "$REPO_ROOT" && go mod download -json go.yaml.in/yaml/v4@v4.0.0) |
        sed -n 's/^[[:space:]]*"Dir": "\(.*\)",$/\1/p')
    cp "$REPO_ROOT/$NOTICE_FILE" "$v4_tmp/$NOTICE_FILE"
    v4_src=""
    if [ -n "$v4_dir" ]; then v4_src=$(notice_source_file "$v4_dir" 2>/dev/null || true); fi
    if [ -z "$v4_src" ] || ! insert_notice_block "$v4_tmp/$NOTICE_FILE" "go.yaml.in/yaml/v4" "$v4_src"; then
        echo "  NOT OK   $v4_desc (could not download the module or insert its NOTICE)"
        fail=$((fail + 1))
    else
        # Expected block built independently of insert_notice_block: header,
        # then upstream with whitespace-only lines emptied, the rest indented,
        # trailing blank lines dropped.
        { echo "go.yaml.in/yaml/v4"
          sed -e 's/^[[:space:]]*$//' -e 's/^./  &/' "$v4_src" |
              awk '{ l[NR] = $0 } END { n = NR; while (n > 0 && l[n] == "") n--; for (i = 1; i <= n; i++) print l[i] }'
        } >"$v4_tmp/want-block"
        n_procfs=$(grep -nx 'github.com/prometheus/procfs' "$v4_tmp/$NOTICE_FILE" | cut -d: -f1)
        n_v4=$(grep -nx 'go.yaml.in/yaml/v4' "$v4_tmp/$NOTICE_FILE" | cut -d: -f1)
        n_grpc=$(grep -nx 'google.golang.org/grpc' "$v4_tmp/$NOTICE_FILE" | cut -d: -f1)
        sed -n "${n_v4},$((n_grpc - 2))p" "$v4_tmp/$NOTICE_FILE" >"$v4_tmp/got-block"
        cp "$v4_tmp/$NOTICE_FILE" "$v4_tmp/with-v4"
        drop_notice_block "$v4_tmp" "go.yaml.in/yaml/v4"
        if ! [ "$n_procfs" -lt "$n_v4" ] || ! [ "$n_v4" -lt "$n_grpc" ]; then
            echo "  NOT OK   $v4_desc (placed at line $n_v4; procfs at $n_procfs, grpc at $n_grpc)"
            fail=$((fail + 1))
        elif ! diff -u "$v4_tmp/want-block" "$v4_tmp/got-block" >"$v4_tmp/d"; then
            echo "  NOT OK   $v4_desc (block is not upstream's NOTICE verbatim)"
            sed 's/^/           /' "$v4_tmp/d"
            fail=$((fail + 1))
        elif ! cmp -s "$REPO_ROOT/$NOTICE_FILE" "$v4_tmp/$NOTICE_FILE"; then
            echo "  NOT OK   $v4_desc (inserting the block changed something else in NOTICE)"
            fail=$((fail + 1))
        else
            echo "  ok       $v4_desc"
            pass=$((pass + 1))
        fi
    fi
fi

echo
echo "$pass passed, $fail failed, $skip skipped"
[ "$fail" -eq 0 ]
