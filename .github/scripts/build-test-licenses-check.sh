#!/usr/bin/env bash
#
# Fails when THIRD_PARTY_BUILD_TEST.md stops matching what the repository
# actually uses to build and test itself.
#
# WHY THIS EXISTS
#
# The Solace public-repository Legal checklist asks for two third-party lists:
# one for what ships, and one for what we use at build and test time. The first
# is THIRD_PARTY_LICENSES.md, gated by licenses-check.sh. This gates the second.
#
# It is a separate script rather than another section of licenses-check.sh
# because the two answer different questions against different inputs.
# licenses-check.sh reasons about the dependency closure of a single main package
# and the Apache-2.0 4(d) NOTICE obligation. This one reasons about four unrelated
# sources and asserts no NOTICE obligation over them.
#
# One exception, stated because the tidy version of this sentence is false and a
# future reader must not draw a compliance conclusion from it:
# gcr.io/distroless/static-debian12 IS redistributed, as the runtime base layer
# of the container image we publish. Its layers are not enumerated by this script
# or by THIRD_PARTY_LICENSES.md — see the Scope section of
# THIRD_PARTY_BUILD_TEST.md. Everything else here is a tool we run, not something
# we ship.
#
# Both run as steps of the same CI job, each with `if: always()`, so a failure in
# one does not hide the other.
#
# WHAT IT CHECKS
#
#   1. Go modules: every external module in every test submodule's
#      `go list -deps -test` closure has a row at the version in use.
#   2. npm packages: every entry in every package-lock.json under test/ has a
#      row at the version in use.
#   3. GitHub Actions: every `uses:` reference in .github/workflows/ has a row,
#      at the ref in use.
#   4. Container images: every FROM and every compose `image:` has a row, at the
#      tag in use.
#   5. All four in reverse: a row for something nothing uses any more fails.
#   6. Every table row parses. A row that silently fails to parse is worse than a
#      wrong row: it vanishes from the checks above and the gate goes green while
#      the inventory rots.
#
# DISCOVERY IS DERIVED, NOT LISTED
#
# Submodules, lockfiles, and Dockerfiles are found by `find -L` rather than
# hardcoded. A hand-maintained list is a silent pass waiting to happen: adding a
# third submodule or a second Dockerfile would introduce undocumented components
# that a fixed list cannot see. This is the single most important property of
# this script, because it is the one that fails *closed* as the repository grows.
#
# `-L` follows symlinks deliberately. A checkout can legitimately symlink
# subtrees — git worktrees, some CI layouts, and this script's own self-test
# fixture all do — and `find` without `-L` silently returns nothing through a
# symlinked directory. That is a fail-*open* discovery bug, and it is exactly the
# shape that made two self-test cases pass vacuously before it was caught.
#
# WHAT IT DELIBERATELY DOES NOT CHECK
#
# Licence *names*. Detecting that a component relicensed between versions needs
# the network and a warm module cache. Same limitation, same reason, as
# licenses-check.sh. Open the licence link when you bump a version.
#
# Container image *layers*. An image is a stack of OS packages under their own
# licences, which is a different question from the licence of the project that
# publishes the image. THIRD_PARTY_BUILD_TEST.md says so explicitly rather than
# implying coverage it does not have.
#
# Usage: .github/scripts/build-test-licenses-check.sh
# Exits 0 when the inventory matches, 1 when it drifts.

set -euo pipefail

DOC="THIRD_PARTY_BUILD_TEST.md"

# Image references that are not third-party components. Build-stage aliases
# (`FROM x AS builder` then `FROM builder`), the empty base, and anything
# templated by Actions. An explicit list, because the alternative — inferring
# "looks like an image name" from its shape — fails *open* on any name the shape
# does not anticipate, and a compliance gate must never do that.
IMAGE_EXCLUDE_RE='^(builder|scratch|\$\{\{.*|.*\$\{\{.*)$'
# Images we publish. They are the artifact, not a dependency of it.
IMAGE_OURS_RE='^(ghcr\.io/solace|solace-broker-mcp)'

if [ ! -f "$DOC" ]; then
    echo "::error::$DOC not found. Run from the repository root." >&2
    exit 1
fi

failures=0
err() {
    echo "::error file=$1::$2"
    failures=$((failures + 1))
}

# --- check 6, first: every candidate row parses ------------------------------
# Run before the inventory checks: if rows are being dropped, their verdicts are
# meaningless. `|| true` on every grep in this script without exception — a
# no-match exits 1, which under `pipefail` kills the script at an assignment
# before it can report anything, and "exit 1, no output" is indistinguishable in
# CI from real drift.
candidate_rows=$(grep -cE '^\| `' "$DOC" || true)
parsed_rows=$(grep -cE '^\| `[^`]+` \| [^ |]+ \|' "$DOC" || true)

if [ "$candidate_rows" -eq 0 ]; then
    err "$DOC" "Parsed no component rows at all. The table format changed; this script needs updating to match it."
elif [ "$parsed_rows" -ne "$candidate_rows" ]; then
    err "$DOC" "$((candidate_rows - parsed_rows)) of $candidate_rows table row(s) did not parse, so they are invisible to this check. Expected format: | \`<name>\` | <version> | <license> | <link> |"
    { grep -nE '^\| `' "$DOC" || true; } | { grep -vE '^[0-9]+:\| `[^`]+` \| [^ |]+ \|' || true; } >&2
fi

documented=$(
    { grep -oE '^\| `[^`]+` \| [^ |]+ \|' "$DOC" || true; } |
        sed -E 's/^\| `([^`]+)` \| `?([^ |`]+)`? \|$/\1 \2/' |
        sort -u
)
documented_names=$(awk '{print $1}' <<<"$documented" | sort -u)

doc_version_of() { # <name>
    awk -v n="$1" '$1 == n { print $2; exit }' <<<"$documented"
}

# Compare a documented version against the one in use. A 40-character SHA pin is
# satisfied by any prefix of it, so the tables can carry a readable short SHA
# without the check going blind to a re-pin.
version_matches() { # <documented> <actual>
    local doc="$1" actual="$2"
    [ "$doc" = "$actual" ] && return 0
    if [[ "$actual" =~ ^[0-9a-f]{40}$ ]] && [ ${#doc} -ge 7 ]; then
        [ "${actual:0:${#doc}}" = "$doc" ] && return 0
    fi
    return 1
}

# name<TAB>version pairs for everything in use, accumulated by the checks below
# and reused for the reverse direction.
in_use=""
record() { in_use="${in_use}$1 $2"$'\n'; }

expect() { # <name> <actual version> <kind> <how to find the licence>
    local name="$1" actual="$2" kind="$3" hint="$4"
    record "$name" "$actual"
    local doc
    doc=$(doc_version_of "$name")
    if [ -z "$doc" ]; then
        err "$DOC" "$kind \`$name\` (${actual}) is used but is not listed. Add a row. $hint"
    elif ! version_matches "$doc" "$actual"; then
        err "$DOC" "$kind \`$name\` is listed at $doc but the repository uses $actual."
    fi
}

# --- check 1: Go modules in every test submodule -----------------------------
while read -r gomod; do
    [ -n "$gomod" ] || continue
    sub=$(dirname "$gomod")
    sub_root=$(cd "$sub" && go list -m 2>/dev/null || true)
    if [ -z "$sub_root" ]; then
        err "$DOC" "Cannot resolve the module in $sub. Run 'go mod download' there and retry; treating this as a pass would drop its whole dependency set from the inventory."
        continue
    fi

    # Capture the exit status rather than discarding it. A failing `go list`
    # inside process substitution is invisible to both `set -e` and `pipefail`,
    # so the closure silently empties and the reverse-direction check then
    # reports every real component as unused — telling a maintainer to delete
    # legitimate rows from a compliance artifact.
    list_out=""
    list_rc=0
    list_out=$(cd "$sub" && go list -deps -test -f '{{with .Module}}{{.Path}} {{.Version}}{{end}}' ./... 2>&1) || list_rc=$?
    if [ "$list_rc" -ne 0 ]; then
        err "$DOC" "'go list' failed in $sub, so its dependency closure is unknown. Fix that before trusting this check. Output: $(head -3 <<<"$list_out" | tr '\n' ' ')"
        continue
    fi

    while read -r mod_path mod_version; do
        [ -n "$mod_path" ] || continue
        [ "$mod_path" = "$sub_root" ] && continue
        case "$mod_path" in github.com/SolaceProducts/solace-broker-mcp*) continue ;; esac
        expect "$mod_path" "$mod_version" "Go module" "Read its licence from the module's own LICENSE file."
    done < <(grep -v '^$' <<<"$list_out" | sort -u || true)
done < <(find -L test -name go.mod -type f 2>/dev/null | sort || true)

# --- check 2: npm packages ---------------------------------------------------
# The LLM eval suite installs the Claude Code CLI with `npm ci`. These are test
# inputs like any other, and they are the one source here whose licences are not
# permissive, so omitting them would falsify the document's own verdict.
while read -r lockfile; do
    [ -n "$lockfile" ] || continue
    if ! command -v jq >/dev/null 2>&1; then
        err "$DOC" "jq is required to read $lockfile and is not installed. Refusing to skip an entire dependency source."
        break
    fi
    while read -r pkg_name pkg_version; do
        [ -n "$pkg_name" ] || continue
        expect "$pkg_name" "$pkg_version" "npm package" "Read its licence from the package's own LICENSE/README, not from the npm registry summary."
    done < <(jq -r '.packages | to_entries[] | select(.key != "") | "\(.key | sub("^node_modules/"; "")) \(.value.version // "?")"' "$lockfile" 2>/dev/null | sort -u || true)
done < <(find -L test -name package-lock.json -type f 2>/dev/null | sort || true)

# --- check 3: GitHub Actions -------------------------------------------------
# The `-` alternative is load-bearing: `- uses: foo@v1` is the idiomatic step
# form and an anchor without it silently misses every action written that way.
# This repo happens to use `- name:` / `uses:` throughout, which is exactly why
# the omission would have gone unnoticed until someone wrote a step normally.
while read -r ref; do
    [ -n "$ref" ] || continue
    action="${ref%@*}"
    version="${ref##*@}"
    [ "$action" = "$ref" ] && version="(unpinned)"
    expect "$action" "$version" "Action" "Read its licence from 'gh api repos/${action%%/*}/$(basename "${action}") --jq .license.spdx_id'."
done < <(
    { grep -rhoE '^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*[^[:space:]]+' .github/workflows/ || true; } |
        sed -E 's/^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*//' |
        { grep -v '^\./' || true; } |
        sort -u
)

# --- check 4: container images ----------------------------------------------
while read -r ref; do
    [ -n "$ref" ] || continue
    # Split on the LAST path segment only. A colon before the final `/` is a
    # registry port (`localhost:5000/foo`), not a tag separator, and treating it
    # as one silently renames the component to `localhost` — a compliance gate
    # checking a name nothing uses. A digest pin splits on `@` instead.
    last_segment="${ref##*/}"
    if [[ "$last_segment" == *"@"* ]]; then
        image="${ref%@*}"
        tag="${ref##*@}"
    elif [[ "$last_segment" == *":"* ]]; then
        image="${ref%:*}"
        tag="${ref##*:}"
    else
        image="$ref"
        tag="(untagged)"
    fi
    [[ "$image" =~ $IMAGE_EXCLUDE_RE ]] && continue
    [[ "$image" =~ $IMAGE_OURS_RE ]] && continue
    expect "$image" "$tag" "Container image" "Name the licence of the project that publishes it."
done < <(
    {
        while read -r df; do
            [ -n "$df" ] || continue
            # `FROM x AS y` — keep the image, drop the stage alias.
            { grep -hoE '^FROM[[:space:]]+[^[:space:]]+' "$df" || true; } | sed -E 's/^FROM[[:space:]]+//'
        done < <(find -L . -name 'Dockerfile*' -type f -not -path './.git/*' 2>/dev/null | sort || true)
        { grep -rhoE '^[[:space:]]*image:[[:space:]]*[^[:space:]]+' test/ .github/workflows/ 2>/dev/null || true; } |
            sed -E 's/^[[:space:]]*image:[[:space:]]*//'
    } | tr -d '"'"'" | sort -u
)

# --- check 5: reverse direction ---------------------------------------------
in_use_names=$(awk '{print $1}' <<<"$in_use" | grep -v '^$' | sort -u || true)
while read -r name; do
    [ -n "$name" ] || continue
    if ! grep -qxF "$name" <<<"$in_use_names"; then
        err "$DOC" "\`$name\` is listed but nothing in the repository uses it any more. Drop the row."
    fi
done <<<"$documented_names"

# --- verdict ----------------------------------------------------------------
if [ "$failures" -gt 0 ]; then
    cat >&2 <<'EOF'

THIRD_PARTY_BUILD_TEST.md no longer matches what this repository builds and
tests with (see the errors above).

It is one of the two third-party lists the Solace public-repository Legal
checklist requires, so a mismatch is a compliance defect rather than a
formatting nit.

If an error says a row is unused, check that the corresponding check above did
not fail first: a broken 'go list' empties a whole closure and every row it
covers then looks unused. Fix the upstream error before deleting any row.

To rebuild the expected sets by hand:

    find test -name go.mod   -exec dirname {} \; | while read -r m; do \
        (cd "$m" && go list -deps -test -f '{{with .Module}}{{.Path}} {{.Version}}{{end}}' ./...); done | sort -u
    find test -name package-lock.json -exec jq -r '.packages | keys[]' {} \;
    grep -rhoE '(-[[:space:]]+)?uses: [^ ]+' .github/workflows/ | sort -u
    find . -name 'Dockerfile*' -not -path './.git/*' -exec grep -hE '^FROM' {} \;

Read each new component's licence from its own LICENSE file, or from
'gh api repos/OWNER/REPO --jq .license.spdx_id' for an action. Do not infer it
from a package name or copy it from a neighbouring row.
EOF
    exit 1
fi

echo "✅ $DOC matches the build and test inputs ($(grep -c . <<<"$in_use_names") component(s) checked, $candidate_rows rows parsed)."
