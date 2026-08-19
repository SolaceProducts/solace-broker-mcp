#!/usr/bin/env bash
#
# Fails when the OSS compliance artifacts stop matching the binary they describe.
#
# WHY THIS EXISTS
#
# THIRD_PARTY_LICENSES.md ships with the release and satisfies a Legal release
# condition. It carried the sentence "Regenerate before each release" and drifted
# anyway: two components listed that had been removed, five shipping that were
# missing, and an un-propagated NOTICE (SOL-152414). A sentence in a Markdown file
# is not a gate.
#
# WHAT IT CHECKS
#
#   1. Every module in the binary's dependency closure appears in
#      THIRD_PARTY_LICENSES.md at the right version, and every documented row maps
#      to a module actually in that closure. Drift either way fails.
#   2. Every table row parses. A row that silently fails to parse is worse than a
#      wrong row, because it vanishes from check 1 and the gate goes green while
#      the inventory rots.
#   3. Components that carry a licence distinct from their module's still have
#      their own row (see REQUIRED_COMPONENTS).
#   4. Every dependency that ships a NOTICE file is named in ours, which is the
#      Apache-2.0 section 4(d) propagation obligation.
#
# WHAT IT DELIBERATELY DOES NOT CHECK
#
# Licence *names* and *URLs*. That needs `go-licenses`, the network, and a warm
# module cache. Note that the generator is not authoritative on URLs either: it
# emitted a 404 for `sony/gobreaker/v2` by inferring a `v2/` subdirectory that
# does not exist. Check a new row's link by opening it.
#
# `go list -deps` without `-test` is the authority for "compiled into the binary",
# and `./cmd/server` is the only main package. Note it legitimately includes test
# *libraries* when a dependency imports one from non-test code:
# `github.com/maypok86/otter/v2` does, via a file named `issue_test_1.25.go` that
# Go compiles as ordinary package code because the name does not end in
# `_test.go`. Those libraries are linked into the shipped binary and belong in the
# inventory.
#
# Usage: .github/scripts/licenses-check.sh
# Exits 0 when the artifacts match, 1 when they drift.

set -euo pipefail

DOC="THIRD_PARTY_LICENSES.md"
NOTICE_FILE="NOTICE"
TARGET="./cmd/server"

# Components whose licence differs from their own module's, so the row must
# survive independently. Without this, deleting the row below still passes: the
# parent module is covered by its own row, and check 1 only reasons about modules.
REQUIRED_COMPONENTS=(
    # BSD-3-Clause (a vendored copy of the standard library's encoding/json),
    # where the parent github.com/go-jose/go-jose/v4 is Apache-2.0.
    "github.com/go-jose/go-jose/v4/json"
)

for f in "$DOC" "$NOTICE_FILE"; do
    if [ ! -f "$f" ]; then
        echo "::error::$f not found. Run from the repository root." >&2
        exit 1
    fi
done

ROOT_MODULE=$(go list -m)

# Expected: "<module path> <version> <dir>" for every module in the binary's
# closure, minus this repository itself.
closure=$(
    go list -deps -f '{{with .Module}}{{.Path}} {{.Version}} {{.Dir}}{{end}}' "$TARGET" |
        grep -v '^$' |
        grep -v "^${ROOT_MODULE} " |
        sort -u
)
expected=$(awk '{print $1 " " $2}' <<<"$closure")

failures=0
err() {
    echo "::error file=$1::$2"
    failures=$((failures + 1))
}

# --- check 2: every candidate row parses ------------------------------------
# Run before the inventory checks: if rows are being dropped, check 1's verdict
# is meaningless. `|| true` on both greps because a no-match exits 1, which under
# `pipefail` would kill the script before it could report anything — that bug made
# a fully unparseable document exit 1 with no output at all.
candidate_rows=$(grep -cE '^\| `' "$DOC" || true)
documented=$(
    { grep -oE '^\| `[^`]+` \| [^ |]+ \|' "$DOC" || true; } |
        sed -E 's/^\| `([^`]+)` \| ([^ |]+) \|$/\1 \2/' |
        sort -u
)
parsed_rows=$(grep -cE '^\| `[^`]+` \| [^ |]+ \|' "$DOC" || true)

if [ "$candidate_rows" -eq 0 ]; then
    err "$DOC" "Parsed no component rows at all. The table format changed; this script needs updating to match it."
elif [ "$parsed_rows" -ne "$candidate_rows" ]; then
    err "$DOC" "$((candidate_rows - parsed_rows)) of $candidate_rows table row(s) did not parse, so they are invisible to this check. Expected format: | \`<component>\` | <version> | <license> | <link> |"
    echo "Rows that did not parse:" >&2
    grep -nE '^\| `' "$DOC" | grep -vE '^[0-9]+:\| `[^`]+` \| [^ |]+ \|' >&2 || true
fi

# --- version comparison -----------------------------------------------------
# `go list` reports pseudo-versions as v0.0.0-<timestamp>-<commit>, while the
# document records the bare commit for commit-pinned modules. Reduce both to a
# comparable form.
#
# Only the v0.0.0 pseudo-version shape is folded. `+incompatible`, prereleases
# (v2.0.0-beta.1) and non-v0 pseudo-versions (v1.2.3-0.2024...-abc) pass through
# unchanged, so they compare literally and fail *closed* — a spurious red the
# first time such a dependency arrives, which is the safe direction. Record those
# verbatim as `go list` prints them. (Two contrived values, v0.0.0-alpha-1 and
# v0.0.0-beta-1, both fold to "1"; no real module uses that shape.)
normalize_version() {
    case "$1" in
        v0.0.0-*-*) echo "${1##*-}" ;;
        *) echo "$1" ;;
    esac
}

# A documented component is a *package* path, so it may be longer than its module
# path (`github.com/coreos/go-oidc/v3/oidc` in module `github.com/coreos/go-oidc/v3`,
# `golang.org/x/sys/cpu` in `golang.org/x/sys`). Match on a `/` boundary so
# `golang.org/x/sync-extra` is never resolved against module `golang.org/x/sync`,
# and take the longest match so a module nested inside another resolves to the
# nearer one.
resolve_module() {
    local component="$1" best=""
    while read -r mod_path _; do
        [ -n "$mod_path" ] || continue
        if [ "$component" = "$mod_path" ] || case "$component" in "${mod_path}/"*) true ;; *) false ;; esac; then
            if [ "${#mod_path}" -gt "${#best}" ]; then
                best="$mod_path"
            fi
        fi
    done <<<"$expected"
    echo "$best"
}

# --- check 1a: every documented row maps to a module, at the right version ---
covered_modules=""
while read -r component doc_version; do
    [ -n "$component" ] || continue
    module=$(resolve_module "$component")

    if [ -z "$module" ]; then
        err "$DOC" "\`$component\` is listed but is not in the dependency closure of $TARGET. It was probably removed from go.mod; drop the row."
        continue
    fi

    actual_version=$(awk -v m="$module" '$1 == m { print $2; exit }' <<<"$expected")
    if [ "$(normalize_version "$doc_version")" != "$(normalize_version "$actual_version")" ]; then
        err "$DOC" "\`$component\` is listed at $doc_version but the binary uses $actual_version."
    fi

    covered_modules="${covered_modules}${module}"$'\n'
done <<<"$documented"

# --- check 1b: every module in the binary is documented ---------------------
while read -r mod_path mod_version; do
    [ -n "$mod_path" ] || continue
    if ! grep -qxF "$mod_path" <<<"$covered_modules"; then
        err "$DOC" "${mod_path}@${mod_version} is compiled into the binary but is not listed. Add it, with its license verified from the generator."
    fi
done <<<"$expected"

# --- check 3: distinct-licence components keep their own row ----------------
for component in "${REQUIRED_COMPONENTS[@]}"; do
    if ! grep -qF "\`$component\`" "$DOC"; then
        err "$DOC" "\`$component\` must keep its own row: its license differs from its parent module's, so the parent's row does not cover it."
    fi
done

# --- check 4: NOTICE propagation (Apache-2.0 section 4(d)) ------------------
while read -r mod_path mod_version mod_dir; do
    [ -n "$mod_path" ] || continue

    if [ -z "$mod_dir" ] || [ ! -d "$mod_dir" ]; then
        err "$NOTICE_FILE" "Cannot inspect ${mod_path}@${mod_version} for a NOTICE file: its module directory is unavailable. Run 'go mod download' and retry rather than treating this as a pass."
        continue
    fi

    # Only the module root: a NOTICE deeper in the tree belongs to a subpackage we
    # may not link.
    if find "$mod_dir" -maxdepth 1 -iname 'NOTICE*' -print -quit | grep -q .; then
        if ! grep -qF "$mod_path" "$NOTICE_FILE"; then
            err "$NOTICE_FILE" "${mod_path}@${mod_version} ships a NOTICE file, but $NOTICE_FILE does not name it. Apache-2.0 section 4(d) requires propagating its attribution notices."
        fi
    fi
done <<<"$closure"

# --- verdict ----------------------------------------------------------------
if [ "$failures" -gt 0 ]; then
    cat >&2 <<EOF

The compliance artifacts no longer match the binary ($failures problem(s) above).

They ship with the release, so a mismatch is a compliance defect, not a
formatting nit.

    make refresh-third-party-inventory

does this automatically: it diffs against this script's own verdict, rewrites
only the row(s) that drifted, reads each new licence fresh rather than
inferring one, and refuses outright — leaving the file untouched — on
anything it doesn't recognize (a strong-copyleft licence, a duplicate or
unparseable row, a component whose licence genuinely differs from its
parent's), same as this script already does. Review its diff and commit it.

If it refuses, or you're reconciling by hand:

    go run github.com/google/go-licenses@v1.6.0 csv ./cmd/server

supplies the module, version, and licence data for every row. Reconcile the
tables, excluding this repository's own module, and update the "Generated"
date. Verify a new component's license from that output, and open its
license link rather than trusting the generated URL.
EOF
    exit 1
fi

echo "✅ $DOC and $NOTICE_FILE match the $(grep -c . <<<"$expected") modules in the dependency closure of $TARGET ($candidate_rows rows checked)."
