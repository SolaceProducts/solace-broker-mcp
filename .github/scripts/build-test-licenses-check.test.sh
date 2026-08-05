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

# The inputs are symlinked, so a case that needs to mutate one must first replace
# the symlink with a real copy. Mutating through it would corrupt the repository.
unlink_workflows() { # <tmp>
    rm "$1/.github/workflows"
    cp -R "$REPO_ROOT/.github/workflows" "$1/.github/workflows"
}

add_dash_uses_action() { # <tmp> — the `- uses:` step form, which an anchor
    # without the `-` alternative silently misses. A new third-party action
    # entering CI must not report green.
    unlink_workflows "$1"
    printf '        - uses: example/sneaky-action@v1\n' >>"$1/.github/workflows/dco.yaml"
}

bump_action_version() { # <tmp> — Dependabot bumps actions daily; a version
    # column nothing defends is decoration.
    unlink_workflows "$1"
    sed -i.bak 's|actions/checkout@v4|actions/checkout@v5|g' "$1/.github/workflows/"*.y*ml
    rm -f "$1/.github/workflows/"*.bak
}

add_second_dockerfile() { # <tmp> — discovery must be derived, not hardcoded
    printf 'FROM python:3.12\n' >"$1/Dockerfile.tools"
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
    # Combined, not stderr alone: err() writes `::error::` annotations to stdout,
    # because that is where GitHub Actions reads them, and only the closing
    # verdict goes to stderr. Asserting on one stream would miss the other.
    local out_file="$tmp/output.txt"
    (cd "$tmp" && "$CHECK" >"$out_file" 2>&1) || got=$?
    local captured
    captured=$(cat "$out_file" 2>/dev/null || true)

    # An exit code alone cannot tell "failed loudly" from "died silently", which
    # is exactly the M1-style bug (a dropped `|| true`) that the parser cases
    # exist to catch. When a case supplies an expected message, assert on it.
    if [ -n "${EXPECT_STDERR:-}" ] && ! grep -qF "$EXPECT_STDERR" <<<"$captured"; then
        echo "  NOT OK   $desc (exit $got as expected, but the output lacked \"$EXPECT_STDERR\" — a silent death is indistinguishable from a real verdict)"
        fail=$((fail + 1))
        rm -rf "$tmp"
        return
    fi

    if [ "$got" -eq "$want" ]; then
        echo "  ok       $desc (exit $got)"
        pass=$((pass + 1))
    else
        echo "  NOT OK   $desc (expected exit $want, got $got)"
        # Show why. Without this a failing baseline says nothing, and every
        # mutation case expecting exit 1 would pass on the wrong exit 1 —
        # a whole suite green for the wrong reason.
        sed 's/^/           | /' <<<"$captured" | head -15
        fail=$((fail + 1))
    fi

    rm -rf "$tmp"
}

# `go list -deps -test` needs every submodule's dependencies resolvable. On a
# cold cache — a fresh CI runner — it fails, the gate correctly reports that it
# cannot determine the closure, and every case expecting exit 1 then passes for
# entirely the wrong reason while the baseline is the only one that tells the
# truth. Warm the cache once, up front, and fail loudly if that is not possible.
while read -r gomod; do
    [ -n "$gomod" ] || continue
    sub=$(dirname "$gomod")
    if ! (cd "$sub" && go mod download all >/dev/null 2>&1); then
        echo "FATAL: could not download modules for $sub. Every case below would"
        echo "       pass for the wrong reason, so refusing to run them."
        exit 1
    fi
done < <(find -L "$REPO_ROOT/test" -name go.mod -type f 2>/dev/null | sort)

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

# --- npm packages ------------------------------------------------------------
# The one source here whose licences are not permissive. Omitting it silently is
# what made the first version of this file assert a verdict its scope did not
# support, so it gets a case in both directions.
assert_check "missing npm package row fails" 1 \
    drop_row "@anthropic-ai/claude-code-linux-x64"
assert_check "wrong npm package version fails" 1 \
    change_version "@anthropic-ai/claude-code" "9.9.9"

# --- versions on actions and images ------------------------------------------
# Both were unchecked in the first version. Dependabot bumps GitHub Actions
# daily, so this is the highest-frequency drift source in the repository.
assert_check "bumped action version with a stale row fails" 1 \
    bump_action_version
assert_check "wrong container image tag fails" 1 \
    change_version "apache/kafka" "4.1.0"

# --- discovery is derived, not hardcoded -------------------------------------
# A hand-maintained input list fails open as the repository grows, which is the
# worst direction for a compliance gate.
assert_check "an action written in the '- uses:' step form is still seen" 1 \
    add_dash_uses_action
assert_check "a second Dockerfile is discovered" 1 \
    add_second_dockerfile

# --- parser integrity --------------------------------------------------------
# The silent-pass shapes. A row the parser skips must not simply disappear: it
# becomes invisible to the inventory checks, so the gate has to notice the skip
# itself.
assert_check "a row the strict parser skips is reported, not ignored" 1 \
    add_unparseable_row "github.com/example/unparseable"
# "Loudly" is the whole assertion. Dropping a `|| true` makes the script die at
# an assignment under `pipefail` — still exit 1, but with no output, which in CI
# is indistinguishable from a real verdict. Asserting the exit code alone passes
# against that bug, so this case asserts the message.
EXPECT_STDERR="Parsed no component rows at all" \
    assert_check "a fully unparseable table fails loudly, with a message" 1 \
    break_table_format
unset EXPECT_STDERR

# --- verdict ----------------------------------------------------------------
echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
