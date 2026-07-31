#!/usr/bin/env bash
#
# Fails when THIRD_PARTY_LICENSES.md stops matching the binary it claims to
# describe.
#
# WHY THIS EXISTS
#
# THIRD_PARTY_LICENSES.md is the OSS compliance artifact that ships with the
# release. It said "Regenerate before each release", and it still drifted: two
# components were listed that had been removed, and five that ship in the binary
# were missing (SOL-152414). A sentence in a Markdown file is not a gate.
#
# WHAT IT CHECKS, AND WHAT IT DELIBERATELY DOES NOT
#
# It compares the module inventory: every module in the binary's dependency
# closure appears in the document at the right version, and every documented row
# maps to a module that is actually in that closure. Drift in either direction
# fails.
#
# It does not verify the license *strings*. That needs `go-licenses`, which wants
# the network and a populated module cache, and a wrong license name is a
# reviewer's job on a dependency change. Missing or stale *components* are the
# audit risk, and that is what this catches. Regenerate with the command in the
# document's header when this fails.
#
# `go list -deps` without `-test` is the authority for "compiled into the
# binary". Note that it legitimately includes test *libraries* when a dependency
# imports one from non-test code — `github.com/maypok86/otter/v2` does, via a
# file named `issue_test_1.25.go` that Go compiles as ordinary package code
# because the name does not end in `_test.go`. Those libraries are part of the
# distribution and belong in the inventory.
#
# Usage: .github/scripts/licenses-check.sh
# Exits 0 when the inventory matches, 1 when it drifts.

set -euo pipefail

DOC="THIRD_PARTY_LICENSES.md"
TARGET="./cmd/server"

if [ ! -f "$DOC" ]; then
    echo "::error::$DOC not found. Run from the repository root." >&2
    exit 1
fi

ROOT_MODULE=$(go list -m)

# Expected: every module in the binary's closure, minus this repository itself.
# Format: "<module path> <version>".
expected=$(
    go list -deps -f '{{with .Module}}{{.Path}} {{.Version}}{{end}}' "$TARGET" |
        grep -v '^$' |
        grep -v "^${ROOT_MODULE} " |
        sort -u
)

# Documented: the component and version columns of every table row. Rows look
# like: | `github.com/foo/bar` | v1.2.3 | MIT | [license](...) |
documented=$(
    grep -oE '^\| `[^`]+` \| [^ |]+ \|' "$DOC" |
        sed -E 's/^\| `([^`]+)` \| ([^ |]+) \|$/\1 \2/' |
        sort -u
)

if [ -z "$documented" ]; then
    echo "::error::Parsed no component rows out of $DOC. The table format changed; this script needs updating." >&2
    exit 1
fi

# `go list` reports pseudo-versions as v0.0.0-<timestamp>-<commit>, while the
# document records the bare commit. Reduce both to a comparable form.
normalize_version() {
    case "$1" in
        v0.0.0-*-*) echo "${1##*-}" ;;
        *) echo "$1" ;;
    esac
}

# A documented component is a *package* path, so it may be longer than its
# module path (`github.com/coreos/go-oidc/v3/oidc` in module
# `github.com/coreos/go-oidc/v3`). Resolve by longest matching module prefix so
# that `github.com/foo/bar-baz` is never matched against module
# `github.com/foo/bar`.
resolve_module() {
    local component="$1" best=""
    while read -r mod_path _; do
        if [ "$component" = "$mod_path" ] || case "$component" in "${mod_path}/"*) true ;; *) false ;; esac; then
            if [ "${#mod_path}" -gt "${#best}" ]; then
                best="$mod_path"
            fi
        fi
    done <<<"$expected"
    echo "$best"
}

failures=0

# Direction 1: every documented row must map to a module in the binary, at the
# version the document claims.
covered_modules=""
while read -r component doc_version; do
    [ -n "$component" ] || continue
    module=$(resolve_module "$component")

    if [ -z "$module" ]; then
        echo "::error file=$DOC::\`$component\` is listed but is not in the dependency closure of $TARGET. It was probably removed from go.mod; drop the row."
        failures=$((failures + 1))
        continue
    fi

    actual_version=$(awk -v m="$module" '$1 == m { print $2; exit }' <<<"$expected")
    if [ "$(normalize_version "$doc_version")" != "$(normalize_version "$actual_version")" ]; then
        echo "::error file=$DOC::\`$component\` is listed at $doc_version but the binary uses $actual_version."
        failures=$((failures + 1))
    fi

    covered_modules="${covered_modules}${module}"$'\n'
done <<<"$documented"

# Direction 2: every module in the binary must be documented by at least one row.
while read -r mod_path mod_version; do
    [ -n "$mod_path" ] || continue
    if ! grep -qxF "$mod_path" <<<"$covered_modules"; then
        echo "::error file=$DOC::${mod_path}@${mod_version} is compiled into the binary but is not listed. Add it, with its license verified from the generator."
        failures=$((failures + 1))
    fi
done <<<"$expected"

if [ "$failures" -gt 0 ]; then
    cat >&2 <<EOF

$DOC no longer matches the binary ($failures problem(s) above).

It is the OSS compliance inventory that ships with the release, so a mismatch is
a compliance defect, not a formatting nit. Regenerate it:

    go run github.com/google/go-licenses@v1.6.0 csv ./cmd/server

Then reconcile the tables, excluding this repository's own module, and update the
"Generated" date. Verify any new component's license from that output rather than
assuming it.
EOF
    exit 1
fi

echo "✅ $DOC matches the $(wc -l <<<"$expected" | tr -d ' ') modules in the dependency closure of $TARGET."
