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
# SKIP_NETWORK_TESTS=1 skips the three cases (of fifteen) that need `go run
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
