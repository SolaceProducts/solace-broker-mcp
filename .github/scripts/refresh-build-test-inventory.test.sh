#!/usr/bin/env bash
#
# Self-test for refresh-build-test-inventory.sh (SOL-152956).
#
# Same shape as build-test-licenses-check.test.sh: mutations touch only a
# copy, the tree symlinks the real submodules and workflows so expected values
# come from real `go list`/`gh api` calls rather than a fixture that goes
# stale, and every fix/add case asserts the produced row is byte-identical to
# the *computed* baseline (the real refresh script run once against an
# unmutated copy of the repo), not the committed file. The committed file is
# stale on every fresh Dependabot PR (SOL-153454); treating it as ground
# truth made this suite fail for the exact situation the refresh scripts
# exist to fix. Exit code 0 alone would also pass a coincidentally-also-valid
# row, which is why the per-row check stays.
#
# This suite needs network access (`gh api` for action licences, `go run
# github.com/google/go-licenses` for new Go-module rows) and a warm module
# cache, unlike build-test-licenses-check.test.sh's own self-test — expect it
# to run noticeably slower.
#
# One case per kind the gate covers (Go module, GitHub Action — both tables,
# container image, npm), in both directions where automation exists, plus the
# two deliberate refusals (a brand-new container image, a brand-new npm
# package) and the "don't guess when something else is also wrong" case.
#
# The Re-pinned prose below the Solace-internal table (SOL-156741) is covered
# with and without REFRESH_PR_NUMBER, plus the cases it must refuse: a
# malformed number, prose it cannot find, and rows left on two pins.
#
# SKIP_NETWORK_TESTS=1 skips the three cases that need `go run
# github.com/google/go-licenses` or `gh api` for a genuinely new row's
# licence — everything else here (fixes, drops, and every refusal) resolves
# from local files and the already-warm module cache. Unset/0 by default so a
# local or manual run gets full coverage; CI sets it for the fast, offline,
# cross-platform-matrixed leg of this suite, so a module-proxy or GitHub API
# outage can't block a merge on a required check that has no real bearing on
# whether the inventory logic itself is correct.
#
# Usage: .github/scripts/refresh-build-test-inventory.test.sh

set -uo pipefail

# The suite controls REFRESH_PR_NUMBER per case; an inherited value would
# change what every other case expects.
unset REFRESH_PR_NUMBER

REPO_ROOT=$(git rev-parse --show-toplevel)
REFRESH="$REPO_ROOT/.github/scripts/refresh-build-test-inventory.sh"
DOC="THIRD_PARTY_BUILD_TEST.md"

# shellcheck source=lib/inventory-refresh-common.sh
source "$REPO_ROOT/.github/scripts/lib/inventory-refresh-common.sh"

# current_field <name> <field-num>
#   Reads a value straight from the committed document rather than hardcoding
#   a snapshot of it in a case's mutation arguments — the exact coupling this
#   ticket's own build-test-licenses-check.test.sh fix (deriving
#   set_solace_action_ref's expected values from the live workflow ref) exists
#   to eliminate elsewhere. A hardcoded "current" version/SHA/tag here goes
#   stale the moment the real row changes, the same way the sibling file's
#   fixtures used to. Backticks are stripped so the result is ready to use as
#   change_field's <old-regex> argument, which matches against the row's raw
#   text, not the cell's own markdown formatting.
current_field() {
    # Escape literal `.` for safe use as an ERE pattern: a version like
    # v0.41.0 must match itself, not "v0<any><41><any>0" — the same
    # precaution the literals this replaces already took by hand
    # (`'v0\.41\.0'`).
    get_row_field "$REPO_ROOT/$DOC" "$1" "$2" | tr -d '`' | sed -E 's/\./\\./g'
}

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

drop_row() { # <tmp> <name>
    grep -vF "\`$2\`" "$1/$DOC" >"$1/t" && mv "$1/t" "$1/$DOC"
}

# change_field <tmp> <name> <old-regex> <new-literal>
#   Exact-string substitution on the row named <name>, rather than a full-field
#   rebuild, so it can target one piece of a multi-part field (e.g. just the
#   SHA inside a backticked `3d3c42e` cell) without needing to know the row's
#   other columns. `\#pattern#` (not `/pattern/`) for the address, and `#` as
#   the s/// delimiter too: several of the names this is called with contain a
#   literal `/` (`actions/checkout`, `golang.org/x/sys`), which would
#   prematurely close a `/`-delimited address or substitution.
change_field() {
    sed -E "\\#\`$2\`# s#$3#$4#" "$1/$DOC" >"$1/t" && mv "$1/t" "$1/$DOC"
    grep -qF "$4" "$1/$DOC"
}

add_unparseable_row() { # <tmp> — an error class this script must refuse
    printf '| `weird thing` | v0.1.0 (vendored) | Apache-2.0 | [license](x) |\n' >>"$1/$DOC"
}

add_new_container_image() { # <tmp> — Dependabot cannot cause this (no docker
    # ecosystem in dependabot.yml), but the refusal must hold regardless.
    printf '      image: example.invalid/never-pulled-image:v1\n' >>"$1/.github/workflows/ci-pr.yaml"
}

add_unpinned_action() { # <tmp> — a brand-new action written without an
    # @sha at all. build-test-licenses-check.sh reports its actual value as
    # the literal string "(unpinned)", which nests a `)` inside the outer
    # `(...)` the ADD message wraps every actual value in — the regex shape
    # that broke add_re's capture group (see add_re's own comment).
    printf '        - uses: example/never-pinned-action\n' >>"$1/.github/workflows/ci-pr.yaml"
}

add_shared_new_go_module() { # <tmp> — a real dependency that both test
    # submodules newly import. build-test-licenses-check.sh's Go-module check
    # loops per test/*/go.mod submodule, so this produces two byte-identical
    # "not listed" diagnostics for one component — the exact shape that
    # inserted a duplicate row before doc_errors was deduplicated and
    # insert_row_in_table gained its own already-exists guard. Uses a real,
    # buildable dependency (not a fixture) so `go list -deps -test` actually
    # resolves it, the same way the real bug was found and reproduced.
    cat >"$1/test/e2e-basic-mcp/agent/dup_bug_regression_shim.go" << 'EOF'
package main

import _ "github.com/google/uuid"
EOF
    cat >"$1/test/e2e-common/broker-driver/dup_bug_regression_shim.go" << 'EOF'
package main

import _ "github.com/google/uuid"
EOF
    (cd "$1/test/e2e-basic-mcp/agent" && go get github.com/google/uuid@v1.6.0 >/dev/null 2>&1 && go mod tidy >/dev/null 2>&1)
    (cd "$1/test/e2e-common/broker-driver" && go get github.com/google/uuid@v1.6.0 >/dev/null 2>&1 && go mod tidy >/dev/null 2>&1)
    # Confirm both submodules actually picked it up — a go get/tidy failure
    # would make this case pass for the wrong reason (no shared dependency
    # actually added, so no duplicate diagnostic to guard against).
    grep -q "github.com/google/uuid" "$1/test/e2e-basic-mcp/agent/go.mod" &&
        grep -q "github.com/google/uuid" "$1/test/e2e-common/broker-driver/go.mod"
}

add_new_npm_package() { # <tmp> — same posture as images, see script header.
    local lockfile="$1/test/e2e-llm/package-lock.json"
    python3 - "$lockfile" << 'PYEOF'
import json, sys
path = sys.argv[1]
with open(path) as f:
    data = json.load(f)
data["packages"]["node_modules/@example/never-installed"] = {"version": "1.2.3"}
with open(path, "w") as f:
    json.dump(data, f, indent=2)
PYEOF
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
# assert_refresh <description> <expected exit code> <row-name-to-check|-> <mutation fn + args>
#   Mirrors refresh-licenses-inventory.test.sh's own assert_refresh. Workflows
#   and both test submodules must be real copies, not symlinks: the mutation
#   functions edit files inside them (a re-pinned action, a new package-lock.json
#   entry), and mutating through a symlink would corrupt the actual repository.
#
#   .github/scripts is deliberately NOT copied into $tmp: $REFRESH below is
#   invoked by its real, absolute path in this repo (not a path under $tmp),
#   so its own path resolution (SCRIPT_DIR/LIB_DIR/CHECK, all derived from
#   ${BASH_SOURCE[0]}) always reads the real, currently-checked-out scripts —
#   exactly like build-test-licenses-check.test.sh's own $CHECK variable,
#   which points at the real script for the same reason. A copy under $tmp
#   would never be read by anything and was previously dead weight.
#
#   <row-name-to-check> is the exact backticked component name whose row
#   must, after a successful (exit-0) run, be byte-identical to the row for
#   that name in CONVERGED_DOC — pass "-" to skip that comparison.

setup_refresh_fixture() { # <tmp>
    local tmp="$1"
    ln -s "$REPO_ROOT/go.mod" "$tmp/go.mod"
    ln -s "$REPO_ROOT/go.sum" "$tmp/go.sum"
    ln -s "$REPO_ROOT/cmd" "$tmp/cmd"
    ln -s "$REPO_ROOT/internal" "$tmp/internal"
    ln -s "$REPO_ROOT/Dockerfile" "$tmp/Dockerfile"
    mkdir -p "$tmp/.github"
    cp -R "$REPO_ROOT/.github/workflows" "$tmp/.github/workflows"
    cp -R "$REPO_ROOT/test" "$tmp/test"
    cp "${INVENTORY_SOURCE:-$REPO_ROOT/$DOC}" "$tmp/$DOC"
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
    local out_file
    out_file=$(mktemp "$tmp/refresh-out.XXXXXX")

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

    # Every exit-0 case in this suite fixes (or starts, and stays) in a state
    # that should converge to exactly the computed baseline — not merely
    # "the one row this case names looks right". Checking only that one row
    # is how the missing-Dockerfile fixture bug got past every other case
    # here: refresh-build-test-inventory.sh correctly saw `golang` and
    # `gcr.io/distroless/static-debian12` as unused (no Dockerfile existed in
    # $tmp to reference them) and dropped both, and every case still reported
    # "ok" because none of them happened to be checking those two rows.
    # Comparing the whole file catches collateral damage to rows the case
    # under test never mentions.
    #
    # The "Generated" date line is excluded from both sides before comparing:
    # refresh-build-test-inventory.sh now bumps it as housekeeping on any real
    # rewrite (mirroring the sibling script), so it legitimately differs from
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
        # merely "exit nonzero". Nothing asserted this until now: a
        # regression that failed loudly but still wrote a partial fix first
        # would have passed every existing refusal case.
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

echo "refresh-build-test-inventory.sh self-test"

compute_converged_baseline

assert_refresh "an already-matching file is a clean no-op" 0 -

# SOL-153454: the committed inventory is not ground truth. A Dependabot
# github-actions bump leaves this file stale against the workflow SHAs; the
# harness must still pass. This case manufactures that shape on main so CI
# covers it without a live Dependabot PR.
stale_dir="$SUITE_TMP/stale"
mkdir -p "$stale_dir"
cp "$REPO_ROOT/$DOC" "$stale_dir/$DOC"
if ! change_field "$stale_dir" "actions/checkout" "$(current_field "actions/checkout" 2)" '0000000'; then
    echo "  ERROR    ambient inventory drift (could not stale actions/checkout)"
    fail=$((fail + 1))
elif diff -q \
    <(grep -v '^\*\*Generated\*\*' "$stale_dir/$DOC") \
    <(grep -v '^\*\*Generated\*\*' "$CONVERGED_DOC") \
    >/dev/null; then
    echo "  ERROR    ambient inventory drift (stale copy did not diverge from the baseline)"
    fail=$((fail + 1))
else
    INVENTORY_SOURCE="$stale_dir/$DOC"
    assert_refresh "a stale committed inventory still converges to the computed baseline" 0 "actions/checkout"
    assert_refresh "ambient drift plus a further mutation still converges" 0 "golang.org/x/sys" \
        change_field "golang.org/x/sys" "$(current_field "golang.org/x/sys" 2)" 'v0.1.0'
    unset INVENTORY_SOURCE
fi

# --- Go modules ---------------------------------------------------------
assert_refresh "a stale Go module version is fixed" 0 "golang.org/x/sys" \
    change_field "golang.org/x/sys" "$(current_field "golang.org/x/sys" 2)" 'v0.1.0'
# jsonschema-go, not a golang.org/x/* module: THIRD_PARTY_BUILD_TEST.md's own
# golang.org/x/* rows link to `+/master:LICENSE` rather than a pinned tag (an
# existing, hand-written inconsistency with how THIRD_PARTY_LICENSES.md and
# go-licenses itself both do it — pinned), which the gate script doesn't check
# either way (it validates versions, never licence URLs) and this script
# doesn't try to reproduce; picking a non-x/* module here keeps this case
# about convergence, not about that unrelated, harmless URL-style difference.
if [ "${SKIP_NETWORK_TESTS:-0}" = "1" ]; then
    skip_network "a missing Go module row is re-added, byte-identical"
else
    assert_refresh "a missing Go module row is re-added, byte-identical" 0 "github.com/google/jsonschema-go" \
        drop_row "github.com/google/jsonschema-go"
fi

# A row whose exact backticked name also appears in unrelated prose elsewhere
# in the document (THIRD_PARTY_BUILD_TEST.md's own note: "**`golang.org/x/oauth2`
# is pinned here at v0.35.0..."). delete_row/set_row_field/etc. must match only
# the table row's first column, not any line that merely contains the
# substring — a plain `grep -vF`/substring match would silently take the prose
# paragraph with it when the row is dropped, or corrupt it if the row's
# version were 'fixed' by a naive whole-line rewrite. Not exercised through
# assert_refresh: this needs to inspect the tmp copy after the run, which that
# helper doesn't expose.
test_prose_collision() {
    local desc="a row is dropped without touching unrelated prose naming it"
    local tmp
    tmp=$(mktemp -d)
    local out_file
    out_file=$(mktemp "$tmp/refresh-out.XXXXXX")
    ln -s "$REPO_ROOT/go.mod" "$tmp/go.mod"
    ln -s "$REPO_ROOT/go.sum" "$tmp/go.sum"
    ln -s "$REPO_ROOT/cmd" "$tmp/cmd"
    ln -s "$REPO_ROOT/internal" "$tmp/internal"
    ln -s "$REPO_ROOT/Dockerfile" "$tmp/Dockerfile"
    mkdir -p "$tmp/.github"
    cp -R "$REPO_ROOT/.github/workflows" "$tmp/.github/workflows"
    cp -R "$REPO_ROOT/test" "$tmp/test"
    cp "$REPO_ROOT/$DOC" "$tmp/$DOC"

    if ! grep -qF '`golang.org/x/oauth2` is pinned here' "$tmp/$DOC"; then
        echo "  ERROR    $desc (fixture assumption broke: the prose note is gone or reworded)"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi
    # Not this file's own drop_row(): that helper is the same naive substring
    # grep the production delete_row() used to be, and would remove the prose
    # paragraph right here at the mutation step — before the refresh script
    # ever runs — which would make this case pass for the wrong reason (an
    # already-corrupted fixture, not a correct refresh). Target only the table
    # row, matching how a real human edit or tool would remove just the row.
    grep -vE '^\| `golang\.org/x/oauth2`' "$tmp/$DOC" >"$tmp/t" && mv "$tmp/t" "$tmp/$DOC"

    local got=0
    (cd "$tmp" && "$REFRESH" >"$out_file" 2>&1) || got=$?

    if [ "$got" -ne 0 ]; then
        echo "  NOT OK   $desc (expected exit 0, got $got)"
        sed 's/^/           /' "$out_file"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi
    if ! grep -qF '`golang.org/x/oauth2` is pinned here' "$tmp/$DOC"; then
        echo "  NOT OK   $desc (the unrelated prose paragraph was deleted along with the row)"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi
    if ! grep -qE '^\| `golang\.org/x/oauth2`' "$tmp/$DOC"; then
        echo "  NOT OK   $desc (the row was supposed to be re-added by the refresh, not left dropped)"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi

    echo "  ok       $desc (exit $got)"
    pass=$((pass + 1))
    rm -rf "$tmp"
}
test_prose_collision

# --- GitHub Actions: third-party (5-column) table ------------------------
assert_refresh "a re-pinned third-party action's row is fixed" 0 "actions/checkout" \
    change_field "actions/checkout" "$(current_field "actions/checkout" 2)" '0000000'
if [ "${SKIP_NETWORK_TESTS:-0}" = "1" ]; then
    skip_network "a missing third-party action row is re-added, byte-identical"
else
    assert_refresh "a missing third-party action row is re-added, byte-identical" 0 "actions/setup-node" \
        drop_row "actions/setup-node"
fi

# --- GitHub Actions: Solace-internal (3-column) table --------------------
# The backtick-wrapped name in this table is the whole path
# (`SolaceDev/solace-public-workflows/guardian-db-sync`), not the bare
# "guardian-db-sync" tail — change_field's address must match that exactly.
assert_refresh "a re-pinned Solace-internal action's row is fixed" 0 \
    "SolaceDev/solace-public-workflows/guardian-db-sync" \
    change_field "SolaceDev/solace-public-workflows/guardian-db-sync" \
        "$(current_field "SolaceDev/solace-public-workflows/guardian-db-sync" 2)" '0000000'
assert_refresh "a missing Solace-internal action row is re-added, byte-identical" 0 \
    "SolaceDev/solace-public-workflows/.github/actions/fossa-guard" \
    drop_row "SolaceDev/solace-public-workflows/.github/actions/fossa-guard"

# --- Solace-internal re-pin prose (SOL-156741) ---------------------------
# A real re-pin moves every SolaceDev/solace-public-workflows ref at once, so
# the fixture rewrites the workflows, not the document: the check then reports
# all six rows as drifted, exactly as it does on a Dependabot
# `internal-reusable-workflows` PR. The synthetic SHA needs no network — the
# Solace-internal rows carry no licence or tag to look up.
REPIN_SHA="1234567890abcdef1234567890abcdef12345678"
REPIN_SHORT="${REPIN_SHA:0:7}"

# repin_solace_workflows <tmp> <new-full-sha> [<name-regex>]
#   Re-pins every solace-public-workflows `uses:` matching <name-regex>
#   (default: all of them) to <new-full-sha>. Written via a temp file and mv,
#   not `sed -i`, so it runs unchanged on the macOS leg.
repin_solace_workflows() {
    local tmp="$1" sha="$2" name_re="${3:-[^@[:space:]]+}" f
    for f in "$tmp"/.github/workflows/*.yaml "$tmp"/.github/workflows/*.yml; do
        [ -f "$f" ] || continue
        sed -E "s#(SolaceDev/solace-public-workflows/${name_re})@[0-9a-f]{40}#\\1@${sha}#g" "$f" >"$f.t" && mv "$f.t" "$f"
    done
    grep -rqE "SolaceDev/solace-public-workflows/[^@[:space:]]+@${sha}" "$tmp/.github/workflows"
}

reword_repin_prose() { # <tmp> — the sentence the rewrite anchors on, reworded
    sed 's/Re-pinned /Pinned again /' "$1/$DOC" >"$1/t" && mv "$1/t" "$1/$DOC"
    grep -qF 'Pinned again ' "$1/$DOC"
}

reword_and_repin() { # <tmp>
    reword_repin_prose "$1" && repin_solace_workflows "$1" "$REPIN_SHA"
}

# solace_section <file> / outside_solace_section <file>
#   The "Solace-internal composite actions" section (heading to the next
#   heading), and everything else minus the **Generated** line.
solace_section() {
    awk '/^#/ { s = ($0 == "### Solace-internal composite actions") } s' "$1"
}
outside_solace_section() {
    awk '/^#/ { s = ($0 == "### Solace-internal composite actions") } !s' "$1" | grep -v '^\*\*Generated\*\*'
}

split_pins_by_hand() { # <tmp> — one workflow ref and its row on a second pin
    repin_solace_workflows "$1" "$REPIN_SHA" 'guardian-db-sync' &&
        change_field "$1" "SolaceDev/solace-public-workflows/guardian-db-sync" \
            "$(get_row_field "$1/$DOC" "SolaceDev/solace-public-workflows/guardian-db-sync" 2 | tr -d '`')" \
            "$REPIN_SHORT"
}

# test_repin_rewrites_prose <desc> <pr-number-or-empty> <expected-reference> [<how>]
#   Starts from the converged document, where table and prose agree, re-pins
#   the workflows, and checks the result field by field: every row on the new
#   pin, the Re-pinned sentence carrying today's date, the PR reference and the
#   old pin as the prior one, no other mention of the old pin left, the line
#   breaks kept, and nothing outside the section touched. <how>:
#     (empty)  one refresh run, with <pr-number-or-empty>
#     rerun    a first run without a PR number, then the run under test
test_repin_rewrites_prose() {
    local desc="$1" pr="$2" want_ref="$3" how="${4:-}"
    local tmp out_file
    tmp=$(mktemp -d)
    out_file=$(mktemp "$tmp/refresh-out.XXXXXX")
    INVENTORY_SOURCE="$CONVERGED_DOC" setup_refresh_fixture "$tmp"

    local old_pin
    old_pin=$(solace_section "$CONVERGED_DOC" | sed -nE 's/^`([0-9a-f]{7,40})` is a branch commit.*/\1/p')
    if [ -z "$old_pin" ] || ! repin_solace_workflows "$tmp" "$REPIN_SHA"; then
        echo "  ERROR    $desc (fixture assumption broke: no prose pin, or the re-pin mutation failed)"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi

    local day_before day_after got=0
    day_before=$(date -u +%Y-%m-%d)
    if [ "$how" = "rerun" ]; then
        (cd "$tmp" && "$REFRESH" >/dev/null 2>&1) || true
        if ! grep -qF '(#N)' "$tmp/$DOC"; then
            echo "  ERROR    $desc (fixture assumption broke: the first run left no (#N))"
            fail=$((fail + 1))
            rm -rf "$tmp"
            return
        fi
    fi
    if [ -n "$pr" ]; then
        (cd "$tmp" && REFRESH_PR_NUMBER="$pr" "$REFRESH" >"$out_file" 2>&1) || got=$?
    else
        (cd "$tmp" && "$REFRESH" >"$out_file" 2>&1) || got=$?
    fi
    day_after=$(date -u +%Y-%m-%d)

    local problems=() section flat
    if [ "$got" -ne 0 ]; then
        problems+=("expected exit 0, got $got")
    fi
    section=$(solace_section "$tmp/$DOC")
    flat=$(tr '\n' ' ' <<<"$section" | tr -s ' ')

    local rows on_new
    rows=$(grep -cE '^\| `SolaceDev/solace-public-workflows/' <<<"$section" || true)
    on_new=$(grep -cE "^\\| \`SolaceDev/solace-public-workflows/[^\`]+\` \\| \`${REPIN_SHORT}\` \\|" <<<"$section" || true)
    if [ "$rows" -eq 0 ] || [ "$rows" -ne "$on_new" ]; then
        problems+=("$on_new of $rows Solace-internal rows on \`$REPIN_SHORT\`")
    fi
    grep -qF "\`${REPIN_SHORT}\` is a branch commit, not a tag" <<<"$flat" ||
        problems+=("the branch-commit sentence does not name \`$REPIN_SHORT\`")
    local sentence_ok=0 day
    for day in "$day_before" "$day_after"; do
        if grep -qF "Re-pinned ${day} by the Dependabot \`internal-reusable-workflows\` group update (${want_ref}); the prior pin was \`${old_pin}\`." <<<"$flat"; then
            sentence_ok=1
        fi
    done
    [ "$sentence_ok" -eq 1 ] ||
        problems+=("no 'Re-pinned ${day_after} … (${want_ref}); the prior pin was \`${old_pin}\`.' sentence")
    local old_count
    old_count=$(grep -oF "\`${old_pin}\`" <<<"$section" | wc -l | tr -d ' ')
    [ "$old_count" -eq 1 ] ||
        problems+=("\`$old_pin\` appears $old_count time(s) in the section; only the prior-pin mention should remain")
    [ "$(solace_section "$CONVERGED_DOC" | wc -l)" -eq "$(solace_section "$tmp/$DOC" | wc -l)" ] ||
        problems+=("the section's line count changed; the rewrite must keep the existing line breaks")
    diff -q <(outside_solace_section "$CONVERGED_DOC") <(outside_solace_section "$tmp/$DOC") >/dev/null ||
        problems+=("lines outside the Solace-internal section changed")
    if [ "$want_ref" = "#N" ] && ! grep -q '^::warning::.*REFRESH_PR_NUMBER' "$out_file"; then
        problems+=("no ::warning:: naming REFRESH_PR_NUMBER while (#N) remains")
    fi

    if [ "${#problems[@]}" -gt 0 ]; then
        echo "  NOT OK   $desc"
        printf '           - %s\n' "${problems[@]}"
        echo "           --- section after refresh ---"
        solace_section "$tmp/$DOC" | sed 's/^/           /'
        echo "           --- refresh output ---"
        sed 's/^/           /' "$out_file"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi

    echo "  ok       $desc (exit $got)"
    pass=$((pass + 1))
    rm -rf "$tmp"
}
test_repin_rewrites_prose "a re-pin rewrites the prose, with the PR number" 4242 "#4242"
test_repin_rewrites_prose "a re-pin rewrites the prose, (#N) left without the PR number" "" "#N"

# Only the Re-pinned paragraph is the re-pin's to rewrite. A later paragraph
# in the same section that names the old pin records something else (which
# update did what), so the new pin must not be written into it.
test_repin_leaves_other_paragraphs() {
    local desc="a re-pin leaves a later paragraph naming the old pin as it was" tmp got=0 old_pin note
    tmp=$(mktemp -d)
    INVENTORY_SOURCE="$CONVERGED_DOC" setup_refresh_fixture "$tmp"
    old_pin=$(solace_section "$tmp/$DOC" | sed -nE 's/^`([0-9a-f]{7})` is a branch commit.*/\1/p')
    note="History: \`${old_pin}\` was the pin this note was written against."
    # The note goes in as its own paragraph just before "Two entries have
    # been dropped", still inside the Solace-internal section.
    if [ -z "$old_pin" ] || ! grep -q '^Two entries have been dropped' "$tmp/$DOC" ||
        ! awk -v note="$note" '/^Two entries have been dropped/ && !done { print note; print ""; done = 1 } { print }' \
            "$tmp/$DOC" >"$tmp/t" || ! mv "$tmp/t" "$tmp/$DOC" || ! repin_solace_workflows "$tmp" "$REPIN_SHA"; then
        echo "  ERROR    $desc (fixture assumption broke)"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi
    (cd "$tmp" && REFRESH_PR_NUMBER=4242 "$REFRESH" >"$tmp/out" 2>&1) || got=$?
    if [ "$got" -ne 0 ] || ! grep -qxF "$note" "$tmp/$DOC" ||
        ! grep -q "^\`${REPIN_SHORT}\` is a branch commit" "$tmp/$DOC"; then
        echo "  NOT OK   $desc (exit $got; the note must be unchanged and the branch-commit sentence re-pinned)"
        solace_section "$tmp/$DOC" | sed 's/^/           /'
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi
    echo "  ok       $desc (exit $got)"
    pass=$((pass + 1))
    rm -rf "$tmp"
}
test_repin_leaves_other_paragraphs
# The number is used only by the run that re-pins; a later run is not one,
# so a "(#N)" an earlier run left stays for a person (or a fresh re-pin run).
test_repin_rewrites_prose "a re-run with the PR number does not fill the (#N), and warns" 4242 "#N" rerun
test_repin_rewrites_prose "a re-run without the PR number keeps (#N) and warns again" "" "#N" rerun

# A "(#N)" already in the prose belongs to an earlier re-pin: a run that does
# not re-pin never writes its PR number into it, so an unrelated PR cannot be
# credited with someone else's re-pin. The accept side is the re-pin cases
# above, which do write the number.
stale_placeholder() { # <tmp> — the Re-pinned sentence says (#N), dated in the past
    sed -E 's/(Re-pinned )[0-9]{4}-[0-9]{2}-[0-9]{2}/\12020-01-02/; s/group update \(#[0-9]+\)/group update (#N)/' \
        "$1/$DOC" >"$1/t" && mv "$1/t" "$1/$DOC"
    grep -qF 'Re-pinned 2020-01-02' "$1/$DOC" && grep -qF '(#N)' "$1/$DOC"
}

# assert_noop <desc> <mutation fn + args>
#   Runs refresh on the converged document after <mutation>, and requires
#   exit 0 with the file byte-for-byte as the mutation left it.
assert_noop() {
    local desc="$1"
    shift
    local tmp got=0
    tmp=$(mktemp -d)
    INVENTORY_SOURCE="$CONVERGED_DOC" setup_refresh_fixture "$tmp"
    if ! "$@" "$tmp"; then
        echo "  ERROR    $desc (the test's own mutation '$1' failed)"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi
    cp "$tmp/$DOC" "$tmp/pre"
    (cd "$tmp" && "$REFRESH" >"$tmp/out" 2>&1) || got=$?
    if [ "$got" -ne 0 ] || ! diff -u "$tmp/pre" "$tmp/$DOC" >"$tmp/diff" 2>&1; then
        echo "  NOT OK   $desc (want exit 0 and the file untouched; got exit $got)"
        sed 's/^/           /' "$tmp/diff" "$tmp/out"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi
    echo "  ok       $desc (exit $got)"
    pass=$((pass + 1))
    rm -rf "$tmp"
}

# Accept side of the case above: rows already on two pins (both matching
# the workflows) have no one pin to name, so a run with nothing to fix leaves
# the prose alone rather than refusing.
assert_noop "rows already on two pins with nothing to fix stay a clean no-op" split_pins_by_hand
REFRESH_PR_NUMBER=4242 assert_noop "a PR number on a run that does not re-pin leaves an existing (#N)" stale_placeholder

# Nothing to fix, rows on one pin the prose does not name, and the prose not
# found: still a no-op (exit 0), but not a silent one.
hand_repin() { # <tmp> — workflows and rows re-pinned by hand, prose left behind
    local old
    old=$(get_row_field "$1/$DOC" "SolaceDev/solace-public-workflows/guardian-db-sync" 2 | tr -d '`')
    repin_solace_workflows "$1" "$REPIN_SHA" &&
        sed -E "/^\| \`SolaceDev\/solace-public-workflows\//s/\| \`${old}\` \|/| \`${REPIN_SHORT}\` |/" \
            "$1/$DOC" >"$1/t" && mv "$1/t" "$1/$DOC"
}
hand_repin_reworded() { # <tmp>
    hand_repin "$1" && reword_repin_prose "$1"
}
reword_branch_commit() { # <tmp> — the sentence the prose pin is read from, reworded
    sed 's/is a branch commit/is a commit on a branch/' "$1/$DOC" >"$1/t" && mv "$1/t" "$1/$DOC"
    grep -qF 'is a commit on a branch' "$1/$DOC"
}
hand_repin_reworded_branch() { # <tmp>
    hand_repin_reworded "$1" || return 1
    # Undo hand_repin_reworded's Re-pinned rewording; reword the other sentence.
    sed 's/Pinned again /Re-pinned /' "$1/$DOC" >"$1/t" && mv "$1/t" "$1/$DOC"
    reword_branch_commit "$1"
}
hand_repin_reworded_then_drop() { # <tmp> — an unrelated drop over prose that already lagged
    hand_repin_reworded "$1" || return 1
    local f
    for f in "$1"/.github/workflows/*.yaml "$1"/.github/workflows/*.yml; do
        [ -f "$f" ] || continue
        grep -v 'SolaceDev/solace-public-workflows/prisma-cloud-scan@' "$f" >"$f.t" || true
        mv "$f.t" "$f"
    done
}
# test_unreconciled_prose_warns <desc> <mutation> <want-file: same|changed>
#   Only a re-pin rewrites the prose. Any other run that leaves the rows on a
#   pin the prose does not name exits 0, leaves the prose as it was, and warns.
test_unreconciled_prose_warns() {
    local desc="$1" mutation="$2" want_file="$3" tmp got=0
    tmp=$(mktemp -d)
    INVENTORY_SOURCE="$CONVERGED_DOC" setup_refresh_fixture "$tmp"
    if ! "$mutation" "$tmp"; then
        echo "  ERROR    $desc (the test's own mutation failed)"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi
    cp "$tmp/$DOC" "$tmp/pre"
    (cd "$tmp" && "$REFRESH" >"$tmp/out" 2>&1) || got=$?
    local same=1 prose_same=1
    diff -q "$tmp/pre" "$tmp/$DOC" >/dev/null || same=0
    diff -q <(solace_section "$tmp/pre" | grep -v '^|') <(solace_section "$tmp/$DOC" | grep -v '^|') >/dev/null ||
        prose_same=0
    if [ "$got" -ne 0 ] || [ "$prose_same" -ne 1 ] || { [ "$want_file" = "same" ] && [ "$same" -ne 1 ]; } ||
        { [ "$want_file" = "changed" ] && [ "$same" -ne 0 ]; } ||
        ! grep -q "^::warning::.*\`${REPIN_SHORT}\`" "$tmp/out"; then
        echo "  NOT OK   $desc (want exit 0, prose untouched, file $want_file and a ::warning:: naming \`$REPIN_SHORT\`; got exit $got)"
        sed 's/^/           /' "$tmp/out"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi
    echo "  ok       $desc (exit $got)"
    pass=$((pass + 1))
    rm -rf "$tmp"
}
test_unreconciled_prose_warns "prose lagging a table re-pinned by hand is left alone, with a warning" \
    hand_repin same

# The warning is quoted verbatim to a person by the skill, so its text is
# checked, not just its presence: the prose's pin once printed twice.
test_hand_repin_warning_text() {
    local desc="the hand re-pin warning names the prose's pin once" tmp prose_pin
    tmp=$(mktemp -d)
    INVENTORY_SOURCE="$CONVERGED_DOC" setup_refresh_fixture "$tmp"
    prose_pin=$(solace_section "$tmp/$DOC" | sed -nE 's/^`([0-9a-f]{7})` is a branch commit.*/\1/p')
    if [ -z "$prose_pin" ] || ! hand_repin "$tmp"; then
        echo "  ERROR    $desc (fixture assumption broke)"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi
    (cd "$tmp" && "$REFRESH" >"$tmp/out" 2>&1) || true
    if ! grep -qF "names \`${prose_pin}\`. The prose is rewritten only on a re-pin" "$tmp/out"; then
        echo "  NOT OK   $desc"
        grep '^::warning::' "$tmp/out" | sed 's/^/           /'
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi
    echo "  ok       $desc"
    pass=$((pass + 1))
    rm -rf "$tmp"
}
test_hand_repin_warning_text
test_unreconciled_prose_warns "a hand re-pin whose prose cannot be found warns, exit 0" \
    hand_repin_reworded same
test_unreconciled_prose_warns "a hand re-pin whose branch-commit sentence cannot be found warns, exit 0" \
    hand_repin_reworded_branch same
test_unreconciled_prose_warns "a drop over prose that already lagged is not refused, and warns" \
    hand_repin_reworded_then_drop changed

# Rows on two pins contradict prose that says they share one, so that state
# warns too, with every pin on the one line, and the prose is left alone.
test_unreconciled_prose_warns "rows already on two pins warn that the prose names one pin" \
    split_pins_by_hand same

# Rows joining a pin the table already uses write no new pin, so this is not
# a re-pin: the rows are fixed, the prose is left alone, and the run warns.
split_then_join() { # <tmp>
    split_pins_by_hand "$1" && repin_solace_workflows "$1" "$REPIN_SHA"
}
split_reword_then_join() { # <tmp>
    split_pins_by_hand "$1" && reword_repin_prose "$1" && repin_solace_workflows "$1" "$REPIN_SHA"
}
split_reword_branch_then_join() { # <tmp>
    split_pins_by_hand "$1" && reword_branch_commit "$1" && repin_solace_workflows "$1" "$REPIN_SHA"
}
test_unreconciled_prose_warns "rows joining a pin the table already uses leave the prose, with a warning" \
    split_then_join changed
test_unreconciled_prose_warns "rows joining a pin, with the Re-pinned sentence reworded, warn" \
    split_reword_then_join changed
test_unreconciled_prose_warns "rows joining a pin, with the branch-commit sentence reworded, warn" \
    split_reword_branch_then_join changed

# The prose names the same commit as the rows, written at 8 characters. Read
# as a different pin, a run with nothing to fix would replace the real re-pin
# history with today's date and "(#N)". Only a 7-character pin is read.
long_prose_pin() { # <tmp>
    local pin
    pin=$(solace_section "$1/$DOC" | sed -nE 's/^`([0-9a-f]{7})` is a branch commit.*/\1/p')
    [ -n "$pin" ] || return 1
    sed "s/^\`${pin}\` is a branch commit/\`${pin}0\` is a branch commit/" "$1/$DOC" >"$1/t" && mv "$1/t" "$1/$DOC"
    grep -q "^\`${pin}0\` is a branch commit" "$1/$DOC"
}
assert_noop "a prose pin not written at 7 characters is not read as a different pin" long_prose_pin

# Rows that were already on two pins are a state a run accepts (above), so a
# later change to them that moves no pin must not be refused for it. The
# accept side is "a re-pin that leaves the rows on two different pins is
# refused", which starts from one shared pin.
split_then_drop() { # <tmp>
    split_pins_by_hand "$1" || return 1
    local f
    for f in "$1"/.github/workflows/*.yaml "$1"/.github/workflows/*.yml; do
        [ -f "$f" ] || continue
        grep -v 'SolaceDev/solace-public-workflows/prisma-cloud-scan@' "$f" >"$f.t" || true
        mv "$f.t" "$f"
    done
}
split_then_typo() { # <tmp>
    split_pins_by_hand "$1" &&
        change_field "$1" "SolaceDev/solace-public-workflows/prisma-cloud-scan" \
            "$(get_row_field "$1/$DOC" "SolaceDev/solace-public-workflows/prisma-cloud-scan" 2 | tr -d '`')" '0000000'
}
# assert_split_change <desc> <want-prisma-row: absent|present> <mutation>
assert_split_change() {
    local desc="$1" want="$2" mutation="$3" tmp got=0 problems=()
    tmp=$(mktemp -d)
    INVENTORY_SOURCE="$CONVERGED_DOC" setup_refresh_fixture "$tmp"
    if ! "$mutation" "$tmp"; then
        echo "  ERROR    $desc (the test's own mutation '$mutation' failed)"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi
    (cd "$tmp" && "$REFRESH" >"$tmp/out" 2>&1) || got=$?
    [ "$got" -eq 0 ] || problems+=("expected exit 0, got $got")
    local row
    row=$(grep -cE '^\| `SolaceDev/solace-public-workflows/prisma-cloud-scan`' "$tmp/$DOC" || true)
    if [ "$want" = "absent" ] && [ "$row" -ne 0 ]; then problems+=("the dropped row is still listed"); fi
    if [ "$want" = "present" ] && ! grep -qE '^\| `SolaceDev/solace-public-workflows/prisma-cloud-scan` \| `[0-9a-f]{7}` \|' "$tmp/$DOC"; then
        problems+=("the mistyped row was not fixed back")
    fi
    grep -q '0000000' "$tmp/$DOC" && problems+=("the mistyped ref is still in the file")
    diff -q <(solace_section "$CONVERGED_DOC" | grep -v '^|') <(solace_section "$tmp/$DOC" | grep -v '^|') >/dev/null ||
        problems+=("the prose below the table changed")
    if [ "${#problems[@]}" -gt 0 ]; then
        echo "  NOT OK   $desc"
        printf '           - %s\n' "${problems[@]}"
        sed 's/^/           /' "$tmp/out"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi
    echo "  ok       $desc (exit $got)"
    pass=$((pass + 1))
    rm -rf "$tmp"
}
assert_split_change "a row dropped from rows already on two pins is not refused" absent split_then_drop
assert_split_change "a mistyped row among rows already on two pins is fixed, not refused" present split_then_typo

# The refuse side of the case above: rows already on two pins do not excuse
# a fix that brings in a pin they did not carry.
split_then_tag() { # <tmp>
    split_pins_by_hand "$1" && repin_solace_workflows "$1" "v9.9.9"
}
split_then_third_pin() { # <tmp> — five of six moved to a third SHA
    split_pins_by_hand "$1" &&
        repin_solace_workflows "$1" "fedcba9876543210fedcba9876543210fedcba98" \
            '[^@[:space:]]*(fossa-guard|sca-scan|update-manifest|prisma-cloud-scan|vulnerability-gate)'
}
split_then_remove_all() { # <tmp>
    split_pins_by_hand "$1" && remove_solace_workflows "$1"
}
INVENTORY_SOURCE="$CONVERGED_DOC"
assert_refresh "a re-pin to a tag from rows already on two pins is refused" 1 - split_then_tag
assert_refresh "a re-pin to a third SHA from rows already on two pins is refused" 1 - split_then_third_pin
unset INVENTORY_SOURCE

# Every Solace-internal reference removed: the rows go, as before, and the
# prose is left for a person with a warning — there is no pin to name.
remove_solace_workflows() { # <tmp>
    local f
    for f in "$1"/.github/workflows/*.yaml "$1"/.github/workflows/*.yml; do
        [ -f "$f" ] || continue
        grep -v 'SolaceDev/solace-public-workflows/' "$f" >"$f.t" || true
        mv "$f.t" "$f"
    done
    ! grep -rq 'SolaceDev/solace-public-workflows/' "$1/.github/workflows"
}
# test_drop_all_solace_rows <desc> <mutation>
test_drop_all_solace_rows() {
    local desc="$1" mutation="$2"
    local tmp got=0 problems=()
    tmp=$(mktemp -d)
    INVENTORY_SOURCE="$CONVERGED_DOC" setup_refresh_fixture "$tmp"
    if ! "$mutation" "$tmp"; then
        echo "  ERROR    $desc (the test's own mutation failed)"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi
    (cd "$tmp" && "$REFRESH" >"$tmp/out" 2>&1) || got=$?
    [ "$got" -eq 0 ] || problems+=("expected exit 0, got $got")
    ! grep -qE '^\| `SolaceDev/solace-public-workflows/' "$tmp/$DOC" ||
        problems+=("Solace-internal rows are still listed")
    diff -q <(solace_section "$CONVERGED_DOC" | grep -v '^|') <(solace_section "$tmp/$DOC" | grep -v '^|') >/dev/null ||
        problems+=("the prose below the table changed")
    grep -q '^::warning::Every SolaceDev/solace-public-workflows row was dropped' "$tmp/out" ||
        problems+=("no ::warning:: that the prose needs a person")
    if [ "${#problems[@]}" -gt 0 ]; then
        echo "  NOT OK   $desc"
        printf '           - %s\n' "${problems[@]}"
        sed 's/^/           /' "$tmp/out"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi
    echo "  ok       $desc (exit $got)"
    pass=$((pass + 1))
    rm -rf "$tmp"
}
test_drop_all_solace_rows "dropping every Solace-internal row leaves the prose, with a warning" \
    remove_solace_workflows
test_drop_all_solace_rows "dropping every row from rows already on two pins still warns" \
    split_then_remove_all

# Accept side of REFRESH_PR_NUMBER: a valid number with nothing re-pinned
# changes nothing. Starts from the converged rows with a concrete PR
# reference: on an unrefreshed re-pin PR the converged document itself says
# "(#N)" dated today, which a run with a number would rightly fill.
concrete_ref() { # <tmp>
    sed -E 's/group update \(#(N|[0-9]+)\)/group update (#540)/' "$1/$DOC" >"$1/t" && mv "$1/t" "$1/$DOC"
    grep -qF 'group update (#540)' "$1/$DOC"
}
REFRESH_PR_NUMBER=4242 assert_noop "a PR number with no re-pin is a clean no-op" concrete_ref

# A consistent table and prose produce no warning at all: the warnings below
# are worth reading only if they never fire on a clean run.
test_clean_run_is_quiet() {
    local desc="a run over consistent rows and prose prints no warning" tmp got=0 n
    tmp=$(mktemp -d)
    INVENTORY_SOURCE="$CONVERGED_DOC" setup_refresh_fixture "$tmp"
    if ! concrete_ref "$tmp"; then
        echo "  ERROR    $desc (the test's own mutation failed)"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi
    (cd "$tmp" && REFRESH_PR_NUMBER=4242 "$REFRESH" >"$tmp/out" 2>&1) || got=$?
    n=$(grep -c '^::warning::' "$tmp/out" || true)
    if [ "$got" -ne 0 ] || [ "$n" -ne 0 ]; then
        echo "  NOT OK   $desc (exit $got, $n warning(s))"
        sed 's/^/           /' "$tmp/out"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi
    echo "  ok       $desc (exit $got)"
    pass=$((pass + 1))
    rm -rf "$tmp"
}
test_clean_run_is_quiet

# Refusals: each leaves the file exactly as the mutation left it (asserted by
# assert_refresh's want=1 branch), so no half-rewritten table is left behind.
INVENTORY_SOURCE="$CONVERGED_DOC"
REFRESH_PR_NUMBER="540x" assert_refresh "a malformed REFRESH_PR_NUMBER is refused before anything is written" 1 - \
    repin_solace_workflows "$REPIN_SHA"
REFRESH_PR_NUMBER="0" assert_refresh "REFRESH_PR_NUMBER=0 is refused" 1 - \
    repin_solace_workflows "$REPIN_SHA"
# Under a UTF-8 locale bash's [0-9] also matches other scripts' digits; the
# number lands in a Legal document and the next re-pin must be able to read
# it back, so only ASCII digits pass. Skipped, not passed, without a UTF-8
# locale, where the case could not tell the two apart. en_US first: C.UTF-8
# collates by codepoint, so its [0-9] is already ASCII-only and would let the
# case pass without the fix.
utf8_locale=$(locale -a 2>/dev/null | grep -iE '^en_us\.utf-?8$' | head -n 1 || true)
if [ -n "$utf8_locale" ]; then
    LC_ALL="$utf8_locale" REFRESH_PR_NUMBER="５４０" assert_refresh \
        "full-width digits in REFRESH_PR_NUMBER are refused under a UTF-8 locale" 1 - \
        repin_solace_workflows "$REPIN_SHA"
else
    echo "  skip     full-width digits in REFRESH_PR_NUMBER are refused under a UTF-8 locale (no en_US UTF-8 locale)"
    skip=$((skip + 1))
fi
assert_refresh "a re-pin with the Re-pinned sentence reworded is refused, not half-applied" 1 - \
    reword_and_repin
# The prose already named a pin no row carried before the re-pin: the
# "prior pin" and the other mentions it would rewrite are not the rows' own,
# so the re-pin is refused rather than written as if they were.
stale_prose_then_repin() { # <tmp>
    local pin
    pin=$(solace_section "$1/$DOC" | sed -nE 's/^`([0-9a-f]{7})` is a branch commit.*/\1/p')
    [ -n "$pin" ] || return 1
    sed "s/^\`${pin}\` is a branch commit/\`0000000\` is a branch commit/" "$1/$DOC" >"$1/t" && mv "$1/t" "$1/$DOC" &&
        grep -q '^`0000000` is a branch commit' "$1/$DOC" && repin_solace_workflows "$1" "$REPIN_SHA"
}
assert_refresh "a re-pin over prose naming a pin no row carried is refused" 1 - \
    stale_prose_then_repin
reword_branch_and_repin() { # <tmp>
    reword_branch_commit "$1" && repin_solace_workflows "$1" "$REPIN_SHA"
}
assert_refresh "a re-pin with the branch-commit sentence not found is refused" 1 - \
    reword_branch_and_repin
assert_refresh "a re-pin that leaves the rows on two different pins is refused" 1 - \
    repin_solace_workflows "$REPIN_SHA" 'guardian-db-sync'
# The prose calls the pin a branch commit and the next re-pin looks for a hex
# SHA there; the accept side is every 40-hex re-pin case above.
assert_refresh "a re-pin to a tag is refused, not written in as a branch commit" 1 - \
    repin_solace_workflows "v1.0.0"
# Refs whose first seven characters are hex: only the full ref says it is not
# a commit SHA, so the check must not run on the 7-character cell value.
assert_refresh "a re-pin to a branch with a hex-looking prefix is refused" 1 - \
    repin_solace_workflows "deadbeef-fix"
assert_refresh "a re-pin to an 8-character abbreviation is refused" 1 - \
    repin_solace_workflows "cafe1234"
# Exactly 7 hex characters looks like the cell value, but a `uses:` ref that
# short is a branch or tag name, never a commit SHA.
assert_refresh "a re-pin to a 7-character hex ref is refused" 1 - \
    repin_solace_workflows "cafe123"
unset INVENTORY_SOURCE

# A brand-new UNPINNED action: exit code alone can't tell "refused via the
# specific not-SHA-pinned message" apart from "refused via the generic
# unhandled bucket because add_re failed to match at all" — both currently
# exit 1, but only the first is the intended path (see add_re's own comment on
# why the "(unpinned)" shape used to break it). Assert on output content, not
# just exit code, to actually catch a regression of that regex bug rather than
# passing coincidentally because refusal is refusal either way.
test_unpinned_action_refusal() {
    local desc="a brand-new unpinned action is refused via its own specific message"
    local tmp
    tmp=$(mktemp -d)
    local out_file
    out_file=$(mktemp "$tmp/refresh-out.XXXXXX")
    ln -s "$REPO_ROOT/go.mod" "$tmp/go.mod"
    ln -s "$REPO_ROOT/go.sum" "$tmp/go.sum"
    ln -s "$REPO_ROOT/cmd" "$tmp/cmd"
    ln -s "$REPO_ROOT/internal" "$tmp/internal"
    ln -s "$REPO_ROOT/Dockerfile" "$tmp/Dockerfile"
    mkdir -p "$tmp/.github"
    cp -R "$REPO_ROOT/.github/workflows" "$tmp/.github/workflows"
    cp -R "$REPO_ROOT/test" "$tmp/test"
    cp "$REPO_ROOT/$DOC" "$tmp/$DOC"
    add_unpinned_action "$tmp"

    local got=0
    (cd "$tmp" && "$REFRESH" >"$out_file" 2>&1) || got=$?

    if [ "$got" -ne 1 ]; then
        echo "  NOT OK   $desc (expected exit 1, got $got)"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi
    if ! grep -qF 'refused: action is not SHA-pinned' "$out_file"; then
        echo "  NOT OK   $desc (exit 1, but not via the specific not-SHA-pinned message — regex regression?)"
        sed 's/^/           /' "$out_file"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi

    echo "  ok       $desc (exit $got)"
    pass=$((pass + 1))
    rm -rf "$tmp"
}
test_unpinned_action_refusal

# A new Go module shared by both test submodules at once must be inserted
# exactly once, not once per submodule's identical diagnostic. Not exercised
# through assert_refresh: this needs go get/mod tidy against real submodule
# copies (which the standard harness already provides) and a bespoke
# row-count assertion rather than a single committed-file comparison, since
# this row is genuinely new and has no "original" to converge back to.
test_shared_new_dependency_inserted_once() {
    local desc="a Go module newly shared by two test submodules is added once, not twice"
    local tmp
    tmp=$(mktemp -d)
    local out_file
    out_file=$(mktemp "$tmp/refresh-out.XXXXXX")
    ln -s "$REPO_ROOT/go.mod" "$tmp/go.mod"
    ln -s "$REPO_ROOT/go.sum" "$tmp/go.sum"
    ln -s "$REPO_ROOT/cmd" "$tmp/cmd"
    ln -s "$REPO_ROOT/internal" "$tmp/internal"
    ln -s "$REPO_ROOT/Dockerfile" "$tmp/Dockerfile"
    mkdir -p "$tmp/.github"
    cp -R "$REPO_ROOT/.github/workflows" "$tmp/.github/workflows"
    cp -R "$REPO_ROOT/test" "$tmp/test"
    cp "$REPO_ROOT/$DOC" "$tmp/$DOC"

    if ! add_shared_new_go_module "$tmp"; then
        echo "  ERROR    $desc (the test's own mutation failed — network or toolchain issue?)"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi

    local got=0
    (cd "$tmp" && "$REFRESH" >"$out_file" 2>&1) || got=$?

    if [ "$got" -ne 0 ]; then
        echo "  NOT OK   $desc (expected exit 0, got $got)"
        sed 's/^/           /' "$out_file"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi

    local count
    count=$(grep -cE '^\| `github\.com/google/uuid`' "$tmp/$DOC")
    if [ "$count" -ne 1 ]; then
        echo "  NOT OK   $desc (expected exactly 1 row, found $count)"
        grep -E '^\| `github\.com/google/uuid`' "$tmp/$DOC" | sed 's/^/           /'
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi

    echo "  ok       $desc (exit $got, 1 row)"
    pass=$((pass + 1))
    rm -rf "$tmp"
}
if [ "${SKIP_NETWORK_TESTS:-0}" = "1" ]; then
    skip_network "a Go module newly shared by two test submodules is added once, not twice"
else
    test_shared_new_dependency_inserted_once
fi

# The case above only half-locks the fix: `sort -u` on the checker's
# diagnostics already collapses the two byte-identical "not listed" lines
# before insert_row_in_table is ever called twice, so that case alone would
# keep passing even if insert_row_in_table's own "refuse if this name already
# exists" guard (lib/inventory-refresh-common.sh) were reverted — nothing
# would call it twice in that scenario to notice. This is a direct unit test
# of that guard specifically, needing no network: call it twice with the
# identical row and require the second call to fail and the file to still
# hold exactly one copy.
test_insert_row_in_table_refuses_duplicate() {
    local desc="insert_row_in_table refuses a second insert of a name that already exists"
    local tmp
    tmp=$(mktemp -d)
    cp "$REPO_ROOT/$DOC" "$tmp/$DOC"

    local row="| \`github.com/example/direct-unit-test-dup\` | v1.0.0 | MIT | [license](https://example.invalid/LICENSE) |"
    if ! (cd "$tmp" && insert_row_in_table "$DOC" "test/e2e-basic-mcp/agent" "$row"); then
        echo "  ERROR    $desc (the test's own first insert failed)"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi

    local got=0
    (cd "$tmp" && insert_row_in_table "$DOC" "test/e2e-basic-mcp/agent" "$row") || got=$?
    if [ "$got" -eq 0 ]; then
        echo "  NOT OK   $desc (a second insert of the same row succeeded — the duplicate-refusal guard is gone)"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi

    local count
    count=$(grep -cF 'github.com/example/direct-unit-test-dup' "$tmp/$DOC")
    if [ "$count" -ne 1 ]; then
        echo "  NOT OK   $desc (expected exactly 1 row after the refused second insert, found $count)"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi

    echo "  ok       $desc"
    pass=$((pass + 1))
    rm -rf "$tmp"
}
test_insert_row_in_table_refuses_duplicate

# --- container images -----------------------------------------------------
assert_refresh "a bumped container image tag is fixed" 0 "apache/kafka" \
    change_field "apache/kafka" "$(current_field "apache/kafka" 2)" '9.9.9'
assert_refresh "a brand-new container image is refused, not guessed" 1 - \
    add_new_container_image

# --- npm packages ----------------------------------------------------------
assert_refresh "a bumped npm package version is fixed" 0 "@anthropic-ai/claude-code" \
    change_field "@anthropic-ai/claude-code" "$(current_field "@anthropic-ai/claude-code" 2)" '9.9.9'
assert_refresh "a brand-new npm package is refused, not guessed" 1 - \
    add_new_npm_package

# --- refuse-to-guess, kind-agnostic ---------------------------------------
assert_refresh "an unparseable row refuses the whole batch rather than guessing" 1 - \
    add_unparseable_row

echo
echo "$pass passed, $fail failed, $skip skipped"
[ "$fail" -eq 0 ]
