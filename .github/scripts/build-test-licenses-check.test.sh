#!/usr/bin/env bash
#
# Self-test for build-test-licenses-check.sh, in the same spirit as
# licenses-check.test.sh.
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
# This subject reasons about four unrelated input sources — Go modules, npm
# packages, GitHub Actions, container images — so each gets its own case in both
# directions. A case that only proves "Go modules are checked" would leave the
# actions and images paths untested, and those are the ones a reader is most
# likely to forget to update.
#
# One case asserts exit 0 on a *changed* tree rather than on the committed one, so
# the suite covers the gate staying quiet on a legitimate edit and not only the
# gate firing on a broken one.
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

# A syntactically valid 40-character SHA that no action is pinned to, used by the
# version-bump cases. A constant rather than a literal at each use site, so the
# mutation and the row it expects cannot drift apart.
BUMPED_SHA="0123456789abcdef0123456789abcdef01234567"

pass=0
fail=0

# --- mutations -------------------------------------------------------------
# Functions rather than inline sed: the table rows are full of '|', which makes
# sed delimiters a source of bugs in the test itself.

drop_row() { # <tmp> <name>
    # Confirm a row actually went. `grep -v` that matches nothing exits 0 and
    # copies the file unchanged, so a case naming a row that has been renamed or
    # deleted would assert against a pristine tree. That is not hypothetical: the
    # licence-hint case below named the SCA reusable workflow, and when
    # DATAGO-147232 removed it the mutation quietly became a no-op.
    local before after
    before=$(wc -l <"$1/$DOC")
    grep -vF "\`$2\`" "$1/$DOC" >"$1/t" && mv "$1/t" "$1/$DOC"
    after=$(wc -l <"$1/$DOC")
    [ "$after" -lt "$before" ]
}

add_row() { # <tmp> <name> <version>
    printf '| `%s` | %s | MIT | [license](https://example.invalid/LICENSE) |\n' "$2" "$3" >>"$1/$DOC"
}

add_unparseable_row() { # <tmp> <name> — a row the strict parser skips
    printf '| `%s` | v0.1.0 (vendored) | Apache-2.0 | [license](x) |\n' "$2" >>"$1/$DOC"
}

add_duplicate_row() { # <tmp> <name> <version> — a second row for a component that
    # already has one. doc_version_of() returns the first match and stops, so
    # only one of the two rows is ever compared and the other is invisible. Both
    # sort directions get a case: the bug is asymmetric, because whether the
    # stale row wins depends on how its version sorts against the real one, and a
    # case in only one direction passes against a still-broken gate.
    printf '| `%s` | %s | Bogus | [license](https://example.invalid/LICENSE) |\n' "$2" "$3" >>"$1/$DOC"
}

change_version() { # <tmp> <name> <new version>
    # Rebuild from NF rather than from a fixed four columns. The tables here
    # carry three, four, or five: the actions table gained a "Release" column
    # when every action moved to a SHA pin, and a hardcoded rebuild silently
    # truncated its last column — leaving a row that still parsed, so nothing
    # complained, but that no longer matched what the file was supposed to say.
    #
    # `END { exit !hit }` makes a mutation that matched no row fail the case
    # loudly instead of asserting against an unchanged document.
    awk -v comp="\`$2\`" -v ver="$3" -F' \\| ' '
        index($0, comp) && /^\| `/ {
            $2 = ver
            row = $1
            for (i = 2; i <= NF; i++) row = row " | " $i
            print row
            hit = 1
            next
        }
        { print }
        END { exit !hit }
    ' "$1/$DOC" >"$1/t" && mv "$1/t" "$1/$DOC"
}

set_solace_action_ref() { # <tmp> <new ref> — rewrite the short SHA on one
    # Solace composite-action row. Those rows have three columns and carry the
    # only short-SHA refs left in the file, so they are what exercises
    # version_matches()'s prefix branch.
    #
    # Addressed to the row, then replacing the backticked hex run. Matching the
    # whole row instead would need escaped `|` inside an ERE, which BSD sed reads
    # as an empty alternation and rejects. The action path in the same row is
    # backticked too but is not all hex, so it cannot match.
    #
    # `guardian-db-sync` is an arbitrary but stable pick among the five. The
    # other four keep the correct ref, so each case isolates one wrong row.
    sed -E "/guardian-db-sync/ s#\`[0-9a-f]+\`#\`$2\`#" \
        "$1/$DOC" >"$1/t" && mv "$1/t" "$1/$DOC"
    # A sed that matched nothing exits 0, which would make every case using this
    # mutation vacuous. Confirm the row actually changed.
    grep -qF "\`$2\`" "$1/$DOC"
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
    printf '        - uses: example/sneaky-action@v1\n' >>"$1/.github/workflows/ci-pr.yaml"
}

bump_action_version() { # <tmp> — Dependabot bumps actions daily; a version
    # column nothing defends is decoration.
    #
    # Matches the pin by shape rather than by value. This mutation used to
    # rewrite the literal `actions/checkout@v4`, and when DATAGO-147232 SHA-pinned
    # every action it matched nothing: the bump case asserted exit 1 against an
    # untouched tree, and its paired case asserted exit 0 against a row change
    # with no workflow change behind it. Naming today's SHA would reintroduce the
    # same rot on the next Dependabot re-pin.
    unlink_workflows "$1"
    sed -i.bak -E "s|(actions/checkout@)[0-9a-f]{40}|\1$BUMPED_SHA|g" "$1/.github/workflows/"*.y*ml
    rm -f "$1/.github/workflows/"*.bak
    # sed exits 0 whether or not it substituted, so prove the workflows changed.
    grep -rqF "$BUMPED_SHA" "$1/.github/workflows/"
}

bump_action_version_and_row() { # <tmp> — the same bump, with the row updated to
    # match: valid to valid, the maintainer's most common edit.
    #
    # Honest about what this adds. It buys no new coverage of version comparison
    # itself — the baseline already exercises the exact-match branch on every
    # action row. What it does buy is a control over the workflow-copy mutations:
    # it is the only case that replaces the symlinked workflows and still expects
    # exit 0, so a corrupted or truncated `unlink_workflows` shows up here and
    # nowhere else. The three cases that mutate workflows all expect exit 1, and
    # they stay green against a copy that lost a file, passing for the wrong
    # reason. Verified by deleting a file inside unlink_workflows: only this case
    # noticed.
    bump_action_version "$1" && change_version "$1" "actions/checkout" "${BUMPED_SHA:0:7}"
}

add_second_dockerfile() { # <tmp> — discovery must be derived, not hardcoded
    printf 'FROM python:3.12\n' >"$1/Dockerfile.tools"
}

add_sibling_worktree_dockerfile() { # <tmp> — a checked-out worktree is not an
    # input to *this* tree. `.gitignore` ignores .worktrees/ and
    # .claude/worktrees/ for that reason, and discovery must prune them rather
    # than filter them out afterwards: `find` descends into a directory it merely
    # excludes from the results, so another branch's Dockerfile arrives as a
    # phantom component of this one. Pairs with the case above — one asserts a
    # real second Dockerfile is found, this one asserts a sibling's is not.
    mkdir -p "$1/.claude/worktrees/other-branch"
    printf 'FROM python:3.12\n' >"$1/.claude/worktrees/other-branch/Dockerfile"
}

add_port_registry_image() { # <tmp> — a colon before the last '/' is a registry
    # port, not a tag. Splitting on the last colon renames the component to
    # `localhost`, and the gate then reports a name nothing uses while the real
    # image goes undocumented.
    unlink_workflows "$1"
    printf '\n# fixture\n    image: localhost:5000/testonly/thing\n' >>"$1/.github/workflows/ci-pr.yaml"
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
assert_check "bumped action version with its row updated passes" 0 \
    bump_action_version_and_row
assert_check "wrong container image tag fails" 1 \
    change_version "apache/kafka" "4.1.0"

# --- short-SHA comparison, both directions ------------------------------------
# The Solace composite actions are pinned to a 40-character SHA and documented by
# a short prefix, so version_matches() accepts a prefix of at least 7 characters.
# All three properties of that rule need a case: a longer prefix must still pass,
# a prefix below the floor must not, and a prefix that is merely the right length
# must still have to be correct. Without the middle case the floor can be
# weakened to 1 with the whole suite staying green, which is a fail-open — a
# one-character stale row would then satisfy any future re-pin.
assert_check "a longer correct short SHA still passes" 0 \
    set_solace_action_ref "63228a0981"
assert_check "a correct short SHA below the 7-character floor fails" 1 \
    set_solace_action_ref "47931"
assert_check "a wrong SHA of acceptable length fails" 1 \
    set_solace_action_ref "47931ec"

# --- discovery is derived, not hardcoded -------------------------------------
# A hand-maintained input list fails open as the repository grows, which is the
# worst direction for a compliance gate.
assert_check "an action written in the '- uses:' step form is still seen" 1 \
    add_dash_uses_action
assert_check "a second Dockerfile is discovered" 1 \
    add_second_dockerfile
assert_check "a sibling worktree's Dockerfile is not mistaken for an input" 0 \
    add_sibling_worktree_dockerfile

# --- remediation hints are actionable ----------------------------------------
# A hint that names a repository which does not exist costs the reader a
# detour and teaches them to distrust the message. An action inside another
# repository is referenced as OWNER/REPO/path/to/action@ref, so deriving the repo
# with `basename` names the action's directory instead. `sca/sca-scan` is the
# deepest such path in the repository, and the one where basename is furthest
# from the right answer.
EXPECT_STDERR="gh api repos/SolaceDev/solace-public-workflows" \
    assert_check "the licence hint names the repo, not the action subpath" 1 \
    drop_row "SolaceDev/solace-public-workflows/.github/actions/sca/sca-scan"
unset EXPECT_STDERR

# --- image reference parsing -------------------------------------------------
# Assert on the message, not just the exit code: the bug renames the component
# rather than dropping it, so it fails either way and only the name distinguishes
# a correct gate from a broken one.
EXPECT_STDERR="localhost:5000/testonly/thing" \
    assert_check "a registry port is not mistaken for a tag" 1 \
    add_port_registry_image
unset EXPECT_STDERR

# --- parser integrity --------------------------------------------------------
# The silent-pass shapes. A row the parser skips must not simply disappear: it
# becomes invisible to the inventory checks, so the gate has to notice the skip
# itself.
assert_check "a row the strict parser skips is reported, not ignored" 1 \
    add_unparseable_row "github.com/example/unparseable"

# A duplicate row is the same silent-pass shape as an unparseable one: the row is
# present, wrong, and invisible. Only the version-sorts-after case actually failed
# open, which is precisely why both directions are asserted.
assert_check "a duplicate row whose version sorts after the real one fails" 1 \
    add_duplicate_row "apache/kafka" "4.9.9"
assert_check "a duplicate row whose version sorts before the real one fails" 1 \
    add_duplicate_row "apache/kafka" "0.0.1"
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
