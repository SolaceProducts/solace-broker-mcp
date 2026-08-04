#!/usr/bin/env bash
#
# Self-test for licenses-check.sh, in the same spirit as dco-check.test.sh.
#
# A compliance gate that cannot fail is not a gate. The dangerous failure mode
# here is not a false alarm, it is a *silent pass*, and three real ones were
# found by review after the first version of this file claimed to cover them:
#
#   - a fully unparseable table aborted the script with no output at all, because
#     `grep -oE` exiting 1 under `pipefail` killed it before it could report;
#   - a single row in a form the parser skipped vanished from the inventory and
#     the check went green;
#   - deleting the row for a sub-package with its own licence went green, because
#     the parent module was still covered.
#
# Every case below asserts on an exit code, so each one must be a shape where the
# correct implementation and a plausibly broken one differ. Cases that fail "for
# some other reason" prove nothing; see the near-miss case for why that matters.
#
# Mutations only ever touch a copy. The tree symlinks the real module so the
# expected inventory comes from the real `go list -deps` rather than a fixture
# that can go stale.
#
# Usage: .github/scripts/licenses-check.test.sh

set -euo pipefail

REPO_ROOT=$(git rev-parse --show-toplevel)
CHECK="$REPO_ROOT/.github/scripts/licenses-check.sh"
DOC="THIRD_PARTY_LICENSES.md"
NOTICE_FILE="NOTICE"

pass=0
fail=0

# --- mutations -------------------------------------------------------------
# Functions rather than inline sed: the table rows are full of '|', which makes
# sed delimiters a source of bugs in the test itself. Each takes the tmp dir.

drop_row() { # <tmp> <component>
    grep -vF "\`$2\`" "$1/$DOC" >"$1/t" && mv "$1/t" "$1/$DOC"
}

add_row() { # <tmp> <component> <version>
    printf '| `%s` | %s | MIT | [license](https://example.invalid/LICENSE) |\n' "$2" "$3" >>"$1/$DOC"
}

add_unparseable_row() { # <tmp> <component> — a row the strict parser skips
    printf '| `%s` | v0.1.0 (vendored) | Apache-2.0 | [license](x) |\n' "$2" >>"$1/$DOC"
}

change_version() { # <tmp> <component> <new version>
    awk -v comp="\`$2\`" -v ver="$3" -F' \\| ' '
        index($0, comp) && /^\| `/ { $2 = ver; print $1 " | " $2 " | " $3 " | " $4; next }
        { print }
    ' "$1/$DOC" >"$1/t" && mv "$1/t" "$1/$DOC"
}

rename_component() { # <tmp> <from> <to>
    awk -v from="\`$2\`" -v to="\`$3\`" '
        { if (index($0, from)) { sub(from, to) } print }
    ' "$1/$DOC" >"$1/t" && mv "$1/t" "$1/$DOC"
}

break_table_format() { # <tmp> — make every row unparseable
    sed 's/^| `/X `/' "$1/$DOC" >"$1/t" && mv "$1/t" "$1/$DOC"
}

unname_in_notice() { # <tmp> <module path>
    grep -vF "$2" "$1/$NOTICE_FILE" >"$1/t" && mv "$1/t" "$1/$NOTICE_FILE"
}

# --- harness ---------------------------------------------------------------

# assert_check <description> <expected exit code> [mutation function + args]
assert_check() {
    local desc="$1" want="$2"
    shift 2

    local tmp
    tmp=$(mktemp -d)

    ln -s "$REPO_ROOT/go.mod" "$tmp/go.mod"
    ln -s "$REPO_ROOT/go.sum" "$tmp/go.sum"
    ln -s "$REPO_ROOT/cmd" "$tmp/cmd"
    ln -s "$REPO_ROOT/internal" "$tmp/internal"
    ln -s "$REPO_ROOT/test" "$tmp/test"
    cp "$REPO_ROOT/$DOC" "$tmp/$DOC"
    cp "$REPO_ROOT/$NOTICE_FILE" "$tmp/$NOTICE_FILE"

    # A mutation that silently fails would make the case vacuous, so surface its
    # failure loudly rather than swallowing it.
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

    local got=0
    (cd "$tmp" && "$CHECK" >/dev/null 2>&1) || got=$?

    if [ "$got" -eq "$want" ]; then
        echo "  ok       $desc (exit $got)"
        pass=$((pass + 1))
    else
        echo "  NOT OK   $desc (expected exit $want, got $got)"
        fail=$((fail + 1))
    fi

    rm -rf "$tmp"
}

echo "licenses-check.sh self-test"

# The committed artifacts must actually pass. If this fails, every case below is
# meaningless, and it is also what catches the symlink set above going stale.
assert_check "the committed artifacts pass" 0

# Drift direction 1: a shipped component goes missing from the document. The
# sony/gobreaker/v2 and maypok86/otter/v2 case from SOL-152414.
assert_check "a deleted row fails" 1 \
    drop_row github.com/sony/gobreaker/v2

# Drift direction 2: the document lists something no longer in the binary. The
# dolthub/maphash and gammazero/deque case.
assert_check "a row for a removed component fails" 1 \
    add_row github.com/dolthub/maphash v0.1.0

# Drift direction 3: right component, stale version. The otter v1.2.4 -> v2.3.0
# case.
assert_check "a stale version fails" 1 \
    change_version github.com/sony/gobreaker/v2 v2.0.0

# Silent pass 1: every row stops parsing. Asserting the exit code alone is not
# enough here — the original bug also exited 1, just with no output — so the
# script's own "parsed no component rows" branch must be reachable, which the
# direction-2 errors below it now demonstrate.
assert_check "a wholly unparseable table fails" 1 \
    break_table_format

# Silent pass 2: ONE row stops parsing. This used to pass green: the row vanished
# from the parsed set, so the stale component it named became invisible.
assert_check "a single unparseable row fails rather than vanishing" 1 \
    add_unparseable_row github.com/dolthub/maphash

# Silent pass 3: a sub-package whose licence differs from its module's loses its
# row. go-jose/v4/json is BSD-3-Clause where its parent is Apache-2.0, so the
# parent's row does not cover it. Used to pass green.
assert_check "dropping a distinct-licence sub-package row fails" 1 \
    drop_row github.com/go-jose/go-jose/v4/json

# NOTICE propagation: a dependency that ships a NOTICE must stay named in ours.
# This is the oasdiff/yaml3 omission the ticket asked about, now automated.
assert_check "a dependency's un-propagated NOTICE fails" 1 \
    unname_in_notice github.com/oasdiff/yaml3

# The '/' boundary in module resolution. This case is built so that a
# boundary-less prefix match would PASS: `golang.org/x/sync-extra` is not in the
# closure, but it does prefix-match module `golang.org/x/sync`, and the version
# given is x/sync's real one, so a broken matcher resolves it happily and reports
# nothing. Getting the version right is what makes the case discriminating —
# an arbitrary version would fail for the wrong reason and prove nothing.
assert_check "a near-miss module path is not accepted as coverage" 1 \
    add_row golang.org/x/sync-extra v0.20.0

# The positive half of prefix resolution: a row deeper than its module path must
# still resolve. The real document depends on this, since rows like
# `github.com/coreos/go-oidc/v3/oidc` and `golang.org/x/sys/cpu` are packages,
# not modules. Note this proves prefix resolution happens at all; it does not
# exercise *longest*-prefix, because no module in the current closure nests
# inside another.
assert_check "a row deeper than the module path still resolves" 0 \
    rename_component github.com/coreos/go-oidc/v3/oidc github.com/coreos/go-oidc/v3/oidc/internal/deeper

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
