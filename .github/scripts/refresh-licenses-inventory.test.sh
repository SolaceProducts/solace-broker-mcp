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
#   later exit-0 case is meaningless without this oracle.
compute_converged_baseline() {
    local tmp out_file got=0
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
    local pre_refresh_snapshot
    pre_refresh_snapshot=$(mktemp "$tmp/refresh-pre.XXXXXX")
    cp "$tmp/$DOC" "$pre_refresh_snapshot"

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
    else
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

echo
echo "$pass passed, $fail failed, $skip skipped"
[ "$fail" -eq 0 ]
