#!/usr/bin/env bash
#
# Verifies the release SBOM's dependency set matches THIRD_PARTY_LICENSES.md —
# SOL-153188. Both are meant to describe the same thing (the module closure
# compiled into ./cmd/server) via two different tools (cyclonedx-gomod vs.
# go-licenses), so a real divergence between them is a bug in one of the two,
# not something to reconcile by hand.
#
# WHY MODULE-LEVEL COMPARISON, NOT STRING-EXACT
#
# The SBOM lists one component per Go MODULE. THIRD_PARTY_LICENSES.md lists one
# row per licence boundary, which is sometimes a PACKAGE deeper than its module
# (`golang.org/x/sys/cpu` in module `golang.org/x/sys`; `github.com/go-jose/
# go-jose/v4/json` in module `github.com/go-jose/go-jose/v4`, kept as its own
# row specifically because its licence differs from its parent's — see that
# file's own Notes section). Comparing the two sets as raw strings would flag
# every one of those rows as "missing from the SBOM" on every single run —
# not a real divergence, just a granularity mismatch. Confirmed by actually
# generating an SBOM and diffing it against the committed document before
# writing this script: seven of thirty rows differ this way today. Both sides
# are resolved to their module before comparing.
#
# resolve_module()/normalize_version() below mirror licenses-check.sh's own
# functions of the same name exactly (same longest-prefix-match, same
# pseudo-version folding) rather than sourcing that script, which is not
# written to be sourced and is out of scope for this story to refactor.
#
# Usage: sbom-check.sh <sbom-path>
# Exits 0 when every module in the SBOM is documented at the matching version
# and vice versa. Exits 1 and names the specific divergence otherwise.

set -euo pipefail

SBOM="${1:?usage: sbom-check.sh <sbom-path>}"
DOC="THIRD_PARTY_LICENSES.md"

for f in "$SBOM" "$DOC"; do
    if [ ! -f "$f" ]; then
        echo "::error::$f not found." >&2
        exit 1
    fi
done

# --- SBOM's module set: "module version" per line ---------------------------
# cyclonedx-gomod's `components` array already excludes the root module
# (that's `metadata.component` instead, per the CycloneDX spec) — confirmed by
# inspecting real output, not assumed — so no root-module filtering is needed
# here the way licenses-check.sh needs for raw go-licenses/go-list output.
# `.components[]?` (with `?`), not `.components[]` — a malformed SBOM missing
# the key entirely makes plain indexing raise a jq error, which under `set -e`
# aborts the script right here with a raw jq trace and no `::error::`
# annotation. `?` turns that into empty output instead, so the check below
# reports it properly the same way a genuinely empty array does.
sbom_modules=$(jq -r '.components[]? | "\(.name) \(.version)"' "$SBOM" | sort -u)

if [ -z "$sbom_modules" ]; then
    echo "::error file=$SBOM::lists no components (or is missing a components array). Refusing to compare against an empty set." >&2
    exit 1
fi

# --- THIRD_PARTY_LICENSES.md's documented rows -------------------------------
# Same extraction pattern licenses-check.sh uses, deliberately not the strict
# candidate/parsed-row distinction that script's check 2 makes — an unparseable
# row there is that document's own problem to catch, not this script's.
# `|| true` on the grep for the same reason licenses-check.sh's own version
# does: under `pipefail`, a table with zero matching rows makes grep exit 1,
# which would abort this script right here rather than let the `-z` check
# below report it — the exact failure mode Copilot's review caught. `sort -u`
# so a duplicate row (already impossible per licenses-check.sh, but this
# script doesn't rely on that) can't produce two conflicting resolutions.
documented=$( { grep -oE '^\| `[^`]+` \| [^ |]+ \|' "$DOC" || true; } |
    sed -E 's/^\| `([^`]+)` \| ([^ |]+) \|$/\1 \2/' | sort -u)

if [ -z "$documented" ]; then
    echo "::error::Parsed no component rows at all from $DOC. The table format may have changed; this script needs updating to match it." >&2
    exit 1
fi

# Mirrors licenses-check.sh's normalize_version exactly: a commit-pinned
# module's pseudo-version needs folding to its bare commit hash to compare
# against the bare hash THIRD_PARTY_LICENSES.md records for those rows.
normalize_version() {
    case "$1" in
        v0.0.0-*-*) echo "${1##*-}" ;;
        *) echo "$1" ;;
    esac
}

# Mirrors licenses-check.sh's resolve_module exactly: longest `/`-boundary
# prefix match against the SBOM's module set, so a package path deeper than
# its module resolves to the nearer enclosing module.
resolve_module() {
    local component="$1" best=""
    while read -r mod_path _; do
        [ -n "$mod_path" ] || continue
        if [ "$component" = "$mod_path" ] || case "$component" in "${mod_path}/"*) true ;; *) false ;; esac; then
            if [ "${#mod_path}" -gt "${#best}" ]; then
                best="$mod_path"
            fi
        fi
    done <<<"$sbom_modules"
    echo "$best"
}

failures=0
err() { # <file> <message>
    echo "::error file=$1::$2" >&2
    failures=$((failures + 1))
}

# --- every documented row resolves to an SBOM module at the same version ----
covered_modules=""
while read -r component doc_version; do
    [ -n "$component" ] || continue
    module=$(resolve_module "$component")

    if [ -z "$module" ]; then
        err "$DOC" "\`$component\` is documented but is not in the SBOM's module set ($SBOM). Either the SBOM is missing a component, or the row should have been dropped."
        continue
    fi

    sbom_version=$(awk -v m="$module" '$1 == m { print $2; exit }' <<<"$sbom_modules")
    if [ "$(normalize_version "$doc_version")" != "$(normalize_version "$sbom_version")" ]; then
        err "$DOC" "\`$component\` is documented at $doc_version but the SBOM reports \`$module\`@$sbom_version."
    fi

    covered_modules="${covered_modules}${module}"$'\n'
done <<<"$documented"

# --- every SBOM module is documented somewhere -------------------------------
while read -r mod_path mod_version; do
    [ -n "$mod_path" ] || continue
    if ! grep -qxF "$mod_path" <<<"$covered_modules"; then
        err "$SBOM" "\`${mod_path}\`@${mod_version} is in the SBOM but not documented anywhere in $DOC."
    fi
done <<<"$sbom_modules"

if [ "$failures" -gt 0 ]; then
    echo "::error::$failures divergence(s) between the SBOM and $DOC (above) — a divergence is a bug in one of the two, not something to reconcile by hand." >&2
    exit 1
fi

echo "✅ SBOM ($SBOM) and $DOC agree on $(grep -c . <<<"$sbom_modules") module(s)."
