#!/usr/bin/env bash
#
# Self-test for licenses-check.sh, in the same spirit as dco-check.test.sh.
#
# A compliance gate that cannot fail is not a gate. The dangerous failure mode
# here is not a false alarm, it is a *vacuous pass*: someone reformats the tables
# in THIRD_PARTY_LICENSES.md, the row parser stops matching anything, and the
# check goes green forever while the inventory rots. These cases prove the check
# still notices each kind of drift it exists to catch.
#
# Each case copies the document into a temporary tree that symlinks the real
# module, so the expected inventory comes from the real `go list -deps` rather
# than a fixture that can go stale. Mutations only ever touch the copy.
#
# Usage: .github/scripts/licenses-check.test.sh

set -euo pipefail

REPO_ROOT=$(git rev-parse --show-toplevel)
CHECK="$REPO_ROOT/.github/scripts/licenses-check.sh"
DOC="THIRD_PARTY_LICENSES.md"

pass=0
fail=0

# --- mutations -------------------------------------------------------------
# Each takes the document path and edits it in place. Kept as functions rather
# than inline sed because the table rows are full of '|', which makes sed
# delimiters and quoting a source of bugs in the test itself.

drop_row() { # drop_row <doc> <component>
    grep -vF "\`$2\`" "$1" >"$1.new" && mv "$1.new" "$1"
}

add_row() { # add_row <doc> <component> — a component not in the closure
    printf '| `%s` | v9.9.9 | MIT | [license](https://example.invalid/LICENSE) |\n' "$2" >>"$1"
}

change_version() { # change_version <doc> <component> <new version>
    awk -v comp="\`$2\`" -v ver="$3" -F' \\| ' '
        index($0, comp) && /^\| `/ { $2 = ver; print $1 " | " $2 " | " $3 " | " $4; next }
        { print }
    ' "$1" >"$1.new" && mv "$1.new" "$1"
}

rename_component() { # rename_component <doc> <from> <to>
    awk -v from="\`$2\`" -v to="\`$3\`" '
        { if (index($0, from)) { sub(from, to) } print }
    ' "$1" >"$1.new" && mv "$1.new" "$1"
}

break_table_format() { # make every row unparseable
    sed 's/^| `/X `/' "$1" >"$1.new" && mv "$1.new" "$1"
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

    # A mutation that silently fails would make the case vacuous, so let its
    # failure surface loudly rather than be swallowed.
    if [ "$#" -gt 0 ]; then
        if ! "$1" "$tmp/$DOC" "${@:2}"; then
            echo "  ERROR    $desc (the test's own mutation '$1' failed)"
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

# The committed document must actually pass. If this fails, every case below is
# meaningless.
assert_check "the committed document passes" 0

# Drift direction 1: a shipped component goes missing from the document. This is
# the sony/gobreaker/v2 and maypok86/otter/v2 case from SOL-152414.
assert_check "a deleted row fails" 1 \
    drop_row github.com/sony/gobreaker/v2

# Drift direction 2: the document lists something no longer in the binary. This
# is the dolthub/maphash and gammazero/deque case.
assert_check "a row for a removed component fails" 1 \
    add_row github.com/dolthub/maphash

# Drift direction 3: right component, stale version. This is the
# otter v1.2.4 -> v2.3.0 case.
assert_check "a stale version fails" 1 \
    change_version github.com/sony/gobreaker/v2 v2.0.0

# The vacuous-pass guard: if the tables are reformatted so that no rows parse,
# the check must complain rather than report success.
assert_check "an unparseable table fails instead of passing" 1 \
    break_table_format

# A path that merely shares a prefix must not satisfy a module. Without
# longest-prefix matching, a row for `github.com/sony/gobreaker-notreal` could be
# resolved against module `github.com/sony/gobreaker/v2`.
assert_check "a near-miss module path does not count as coverage" 1 \
    rename_component github.com/sony/gobreaker/v2 github.com/sony/gobreaker-notreal

# The positive half of prefix resolution: a row deeper than the module path must
# still resolve to that module. The real document depends on this — rows like
# `github.com/coreos/go-oidc/v3/oidc` and `golang.org/x/sys/cpu` are packages, not
# modules — so a change that only matched exact module paths must fail here.
assert_check "a row deeper than the module path still resolves" 0 \
    rename_component github.com/coreos/go-oidc/v3/oidc github.com/coreos/go-oidc/v3/oidc/internal/deeper

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
