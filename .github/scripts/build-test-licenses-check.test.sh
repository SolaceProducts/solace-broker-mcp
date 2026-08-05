#!/usr/bin/env bash
#
# Self-test for build-test-licenses-check.sh, in the same spirit as
# licenses-check.test.sh and dco-check.test.sh.
#
# A compliance gate that cannot fail is not a gate. The dangerous failure mode is
# not a false alarm, it is a *silent pass*: the inventory rots, CI stays green,
# and nobody learns until an auditor asks. licenses-check.test.sh found three real
# silent passes in its own subject after review, so each shape it caught is
# asserted here from the start rather than after the same lesson is relearned:
#
#   - a fully unparseable table aborting with no output, because `grep -oE`
#     exiting 1 under `pipefail` kills the script before it can report;
#   - a single row in a form the strict parser skips vanishing from the checks;
#   - drift in the reverse direction, where a row outlives the thing it describes.
#
# This subject reasons about three unrelated input sources, so each gets its own
# case in both directions. A case that only proves "Go modules are checked" would
# leave the actions and images paths untested, and those are the ones a reader is
# most likely to forget to update.
#
# Every case asserts on an exit code, so each must be a shape where the correct
# implementation and a plausibly broken one differ.
#
# Mutations only ever touch a copy. The tree symlinks the real submodules and
# workflows so the expected sets come from real `go list` and real files rather
# than fixtures that go stale.
#
# Usage: .github/scripts/build-test-licenses-check.test.sh

set -euo pipefail

REPO_ROOT=$(git rev-parse --show-toplevel)
CHECK="$REPO_ROOT/.github/scripts/build-test-licenses-check.sh"
DOC="THIRD_PARTY_BUILD_TEST.md"

pass=0
fail=0

# --- mutations -------------------------------------------------------------
# Functions rather than inline sed: the table rows are full of '|', which makes
# sed delimiters a source of bugs in the test itself.

drop_row() { # <tmp> <name>
    grep -vF "\`$2\`" "$1/$DOC" >"$1/t" && mv "$1/t" "$1/$DOC"
}

add_row() { # <tmp> <name> <version>
    printf '| `%s` | %s | MIT | [license](https://example.invalid/LICENSE) |\n' "$2" "$3" >>"$1/$DOC"
}

add_unparseable_row() { # <tmp> <name> — a row the strict parser skips
    printf '| `%s` | v0.1.0 (vendored) | Apache-2.0 | [license](x) |\n' "$2" >>"$1/$DOC"
}

change_version() { # <tmp> <name> <new version>
    awk -v comp="\`$2\`" -v ver="$3" -F' \\| ' '
        index($0, comp) && /^\| `/ { $2 = ver; print $1 " | " $2 " | " $3 " | " $4; next }
        { print }
    ' "$1/$DOC" >"$1/t" && mv "$1/t" "$1/$DOC"
}

break_table_format() { # <tmp> — make every row unparseable
    sed 's/^| `/X `/' "$1/$DOC" >"$1/t" && mv "$1/t" "$1/$DOC"
}

# --- harness ---------------------------------------------------------------

# assert_check <description> <expected exit code> [mutation function + args]
assert_check() {
    local desc="$1" want="$2"
    shift 2

    local tmp
    tmp=$(mktemp -d)

    # Symlink every input the check reads, so the expected sets are the real
    # ones. Only the documents are copied, because only they get mutated.
    ln -s "$REPO_ROOT/test" "$tmp/test"
    ln -s "$REPO_ROOT/Dockerfile" "$tmp/Dockerfile"
    mkdir -p "$tmp/.github"
    ln -s "$REPO_ROOT/.github/workflows" "$tmp/.github/workflows"
    cp "$REPO_ROOT/$DOC" "$tmp/$DOC"

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

echo "build-test-licenses-check.sh self-test"

# The committed artifacts must actually pass. If this fails, every case below is
# measuring the wrong thing, because they all mutate from this baseline.
assert_check "committed inventory passes unmodified" 0

# --- Go modules, both directions --------------------------------------------
# solace.dev/go/messaging is the one module here that appears in no other
# inventory, so losing its row would be invisible anywhere else.
assert_check "missing Go module row fails" 1 \
    drop_row "solace.dev/go/messaging"
assert_check "wrong Go module version fails" 1 \
    change_version "solace.dev/go/messaging" "v9.9.9"
assert_check "Go module row for an unused module fails" 1 \
    add_row "github.com/example/not-a-dependency" "v1.0.0"

# A module that also ships is still a build/test input of the submodule that
# imports it. Dropping its row must fail here even though THIRD_PARTY_LICENSES.md
# still lists it — the two files answer different questions.
assert_check "missing row for a module that also ships fails" 1 \
    drop_row "github.com/modelcontextprotocol/go-sdk"

# --- GitHub Actions, both directions ----------------------------------------
assert_check "missing action row fails" 1 \
    drop_row "actions/checkout"
assert_check "action row for an unused action fails" 1 \
    add_row "example/never-used-action" "v1"

# --- container images, both directions --------------------------------------
# The distroless base is the one image that ships, so its row is the one whose
# loss would matter most.
assert_check "missing container image row fails" 1 \
    drop_row "gcr.io/distroless/static-debian12"
assert_check "image row for an unreferenced image fails" 1 \
    add_row "example/never-pulled-image" "v1"

# --- parser integrity --------------------------------------------------------
# The silent-pass shapes. A row the parser skips must not simply disappear: it
# becomes invisible to the inventory checks, so the gate has to notice the skip
# itself.
assert_check "a row the strict parser skips is reported, not ignored" 1 \
    add_unparseable_row "github.com/example/unparseable"
assert_check "a fully unparseable table fails loudly" 1 \
    break_table_format

# --- verdict ----------------------------------------------------------------
echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
