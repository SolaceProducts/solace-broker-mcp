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
# It exists as a separate script rather than another section of licenses-check.sh
# because the two answer different questions against different inputs, and the
# failure of one should not mask the other. licenses-check.sh reasons about the
# dependency closure of a single main package and the Apache-2.0 4(d) NOTICE
# obligation. This one reasons about three unrelated sources — submodule closures,
# workflow `uses:` references, and container image tags — none of which has a
# NOTICE obligation because none of them is redistributed.
#
# WHAT IT CHECKS
#
#   1. Every external Go module in the e2e submodules' `go list -deps -test`
#      closure has a row, at the version actually in use. Both directions: a row
#      for a module we no longer depend on fails too.
#   2. Every `uses:` reference in .github/workflows/ has a row. Both directions.
#   3. Every container image in Dockerfile and the compose files has a row. Both
#      directions.
#   4. Every table row parses. A row that silently fails to parse is worse than a
#      wrong row: it vanishes from the checks above and the gate goes green while
#      the inventory rots. This is the failure mode licenses-check.test.sh caught
#      in its own subject, so it is asserted here from the start.
#
# WHAT IT DELIBERATELY DOES NOT CHECK
#
# Licence *names*. Detecting that a component relicensed between versions needs
# the network and a warm module cache. Same limitation, same reason, as
# licenses-check.sh. Open the licence link when you bump a version.
#
# SCOPE, AND WHY IT IS NOT "EVERYTHING NOT IN THE OTHER FILE"
#
# Only the submodules under test/ are enumerated here, not the root module. The
# root module's `go list -deps -test ./...` closure is identical to its shipped
# closure, so THIRD_PARTY_LICENSES.md is already the complete answer for it and
# repeating all 31 rows here would create two lists to keep in sync for no gain.
# THIRD_PARTY_BUILD_TEST.md says so in prose.
#
# The submodules are different: each has its own go.mod and its own closure, and
# a reader asking "what does the e2e agent pull in" needs that answer in one
# place. Several of their dependencies also ship, and are therefore listed in
# both files. That overlap is deliberate. The Legal checklist asks for a list of
# all products used at build and test time, not for the delta against the release
# list, and a reader should not have to diff two files to obey it.
#
# Usage: .github/scripts/build-test-licenses-check.sh
# Exits 0 when the inventory matches, 1 when it drifts.

set -euo pipefail

DOC="THIRD_PARTY_BUILD_TEST.md"
SUBMODULES=(
    "test/e2e-basic-mcp/agent"
    "test/e2e-common/broker-driver"
)

# THIRD_PARTY_LICENSES.md is deliberately not read here. The two files are
# maintained together and cross-reference each other, but this check derives its
# expected set entirely from the repository's real inputs, so taking the other
# document as an input would only couple two gates that should fail independently.
if [ ! -f "$DOC" ]; then
    echo "::error::$DOC not found. Run from the repository root." >&2
    exit 1
fi

failures=0
err() {
    echo "::error file=$1::$2"
    failures=$((failures + 1))
}

# Rows are `| `<name>` | <version> | ...`, the same shape licenses-check.sh
# parses, so a reader moving between the two files is not learning two formats.
# `|| true` on every grep: a no-match exits 1, which under `pipefail` would kill
# the script before it could report anything.
candidate_rows=$(grep -cE '^\| `' "$DOC" || true)
parsed_rows=$(grep -cE '^\| `[^`]+` \| [^ |]+ \|' "$DOC" || true)

if [ "$candidate_rows" -eq 0 ]; then
    err "$DOC" "Parsed no component rows at all. The table format changed; this script needs updating to match it."
elif [ "$parsed_rows" -ne "$candidate_rows" ]; then
    err "$DOC" "$((candidate_rows - parsed_rows)) of $candidate_rows table row(s) did not parse, so they are invisible to this check. Expected format: | \`<name>\` | <version> | <license> | <link> |"
    grep -nE '^\| `' "$DOC" | grep -vE '^[0-9]+:\| `[^`]+` \| [^ |]+ \|' >&2 || true
fi

documented=$(
    { grep -oE '^\| `[^`]+` \| [^ |]+ \|' "$DOC" || true; } |
        sed -E 's/^\| `([^`]+)` \| ([^ |]+) \|$/\1 \2/' |
        sort -u
)
documented_names=$(awk '{print $1}' <<<"$documented" | sort -u)

# --- check 1: Go modules in the submodule closures ---------------------------
expected_modules=""
for sub in "${SUBMODULES[@]}"; do
    if [ ! -f "$sub/go.mod" ]; then
        err "$DOC" "Submodule $sub has no go.mod. SUBMODULES in this script is stale; update it to match 'find test -name go.mod'."
        continue
    fi
    sub_root=$(cd "$sub" && go list -m)
    while read -r mod_path mod_version; do
        [ -n "$mod_path" ] || continue
        [ "$mod_path" = "$sub_root" ] && continue
        case "$mod_path" in github.com/SolaceProducts/solace-broker-mcp*) continue ;; esac
        expected_modules="${expected_modules}${mod_path} ${mod_version}"$'\n'
    done < <(cd "$sub" && go list -deps -test -f '{{with .Module}}{{.Path}} {{.Version}}{{end}}' ./... 2>/dev/null | grep -v '^$' | sort -u)
done
expected_modules=$(sort -u <<<"$expected_modules" | grep -v '^$' || true)

while read -r mod_path mod_version; do
    [ -n "$mod_path" ] || continue
    doc_version=$(awk -v m="$mod_path" '$1 == m { print $2; exit }' <<<"$documented")
    if [ -z "$doc_version" ]; then
        err "$DOC" "${mod_path}@${mod_version} is used by a test submodule but is not listed. Add a row, with its license read from the module's own LICENSE file."
    elif [ "$doc_version" != "$mod_version" ]; then
        err "$DOC" "\`$mod_path\` is listed at $doc_version but the submodule uses $mod_version."
    fi
done <<<"$expected_modules"

# --- check 2: GitHub Actions -------------------------------------------------
# `uses:` values that are not local paths. Strip the ref: the tables record
# versions in their own column, and a SHA pin would never match a `vN` row.
expected_actions=$(
    { grep -rhoE '^[[:space:]]*uses:[[:space:]]*[^[:space:]]+' .github/workflows/ || true; } |
        sed -E 's/^[[:space:]]*uses:[[:space:]]*//' |
        grep -v '^\./' |
        sed -E 's/@.*$//' |
        sort -u
)
while read -r action; do
    [ -n "$action" ] || continue
    if ! grep -qxF "$action" <<<"$documented_names"; then
        err "$DOC" "Action \`$action\` is used by a workflow but is not listed. Add a row, with its license read from 'gh api repos/$action --jq .license.spdx_id'."
    fi
done <<<"$expected_actions"

# --- check 3: container images ----------------------------------------------
# Dockerfile FROM lines and compose `image:` values. Our own published image is
# excluded: it is the artifact, not a dependency of it.
expected_images=$(
    {
        grep -hoE '^FROM[[:space:]]+[^[:space:]]+' Dockerfile 2>/dev/null || true
        grep -rhoE '^[[:space:]]*image:[[:space:]]*[^[:space:]]+' test/ .github/workflows/ 2>/dev/null || true
    } |
        sed -E 's/^(FROM[[:space:]]+|[[:space:]]*image:[[:space:]]*)//' |
        sed -E 's/:[^:]*$//' |
        grep -vE '^(ghcr\.io/solace|solace-broker-mcp)' |
        grep -E '/|^[a-z]+$' |
        sort -u
)
while read -r image; do
    [ -n "$image" ] || continue
    if ! grep -qxF "$image" <<<"$documented_names"; then
        err "$DOC" "Container image \`$image\` is referenced but is not listed. Add a row naming the licence of the project that publishes it."
    fi
done <<<"$expected_images"

# --- reverse direction: every row still corresponds to something in use ------
in_use=$(
    {
        awk '{print $1}' <<<"$expected_modules"
        echo "$expected_actions"
        echo "$expected_images"
    } | grep -v '^$' | sort -u
)
while read -r name; do
    [ -n "$name" ] || continue
    if ! grep -qxF "$name" <<<"$in_use"; then
        err "$DOC" "\`$name\` is listed but nothing in the repository uses it any more. Drop the row."
    fi
done <<<"$documented_names"

# --- verdict ----------------------------------------------------------------
if [ "$failures" -gt 0 ]; then
    cat >&2 <<EOF

$DOC no longer matches what this repository builds and tests with ($failures problem(s) above).

It is one of the two third-party lists the Solace public-repository Legal
checklist requires, so a mismatch is a compliance defect rather than a
formatting nit.

To rebuild the expected sets by hand:

    for m in $(printf '%s ' "${SUBMODULES[@]}"); do
        (cd "\$m" && go list -deps -test -f '{{with .Module}}{{.Path}} {{.Version}}{{end}}' ./...)
    done | sort -u
    grep -rhoE 'uses: [^ ]+' .github/workflows/ | sed 's/uses: //' | sort -u
    grep -hE '^FROM' Dockerfile

Read each new component's licence from its own LICENSE file, or from
'gh api repos/OWNER/REPO --jq .license.spdx_id' for an action. Do not infer it
from a package name or copy it from a neighbouring row.
EOF
    exit 1
fi

echo "✅ $DOC matches the build and test inputs ($(grep -c . <<<"$in_use") component(s) checked, $candidate_rows rows parsed)."
