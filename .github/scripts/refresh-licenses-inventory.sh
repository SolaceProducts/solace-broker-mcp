#!/usr/bin/env bash
#
# Rewrites THIRD_PARTY_LICENSES.md's table rows to match the binary's current
# dependency closure, driven entirely by licenses-check.sh's own diagnostics
# (SOL-152956). Written so a Dependabot Go-module bump lands with this file
# already correct, instead of red until a human hand-edits it (SOL-152414,
# and again on PR #251).
#
# WHY IT PARSES THE CHECK SCRIPT'S OUTPUT INSTEAD OF RECOMPUTING THE CLOSURE
#
# licenses-check.sh is out of scope to change for this ticket, and it already
# computes the one thing that matters here: the exact difference between what
# is documented and what `go list -deps ./cmd/server` actually resolves to,
# emitted as one `::error file=THIRD_PARTY_LICENSES.md::...` line per
# discrepancy. Recomputing that closure a second way here would be a second,
# independently-drifting copy of exactly the logic this ticket exists to stop
# drifting. So this script treats licenses-check.sh as the single source of
# truth for *what* changed, and — because of that — re-running it unmodified
# is also the correct way to verify the fix, not a weaker stand-in for one.
#
# WHAT IT REFUSES TO DO
#
# Every row this script writes carries a licence read fresh from
# `go-licenses` (see lib/inventory-refresh-common.sh's fetch_go_module_license),
# the same generator this file's own regenerate instructions name — never
# inferred from a package name, never copied from a neighbouring row. If
# licenses-check.sh reports anything this script does not recognise (a
# duplicate row, an unparseable row, a NOTICE-propagation gap, `go list`
# failing outright), it stops and asks for a human rather than guessing. A
# partially-applied fix is worse than no fix, so this refuses to write
# anything unless it can make the *entire* diagnosed set of problems go away.
#
# Usage: .github/scripts/refresh-licenses-inventory.sh
# Exits 0 if the file already matched, or now matches after a rewrite.
# Exits 1 if any diagnostic could not be safely automated, or the rewrite did
# not converge — in both cases, the file is left exactly as the failed
# rewrite produced it (or untouched, if nothing could be attempted at all) so
# a human can see what happened via `git diff`.

set -euo pipefail

# Run-from-repo-root contract, matching licenses-check.sh exactly (no `git
# rev-parse` — the check script has no such dependency either, and this
# script's own self-test symlinks individual paths into a plain tmpdir with no
# `.git` at all, the same fixture shape licenses-check.test.sh already uses).
DOC="THIRD_PARTY_LICENSES.md"
CHECK="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/licenses-check.sh"
LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib"

if [ ! -f "$DOC" ]; then
    echo "::error::$DOC not found. Run from the repository root." >&2
    exit 1
fi

# shellcheck source=lib/inventory-refresh-common.sh
source "$LIB_DIR/inventory-refresh-common.sh"

run_check() { # prints combined stdout+stderr, returns the check's exit code
    "$CHECK" 2>&1
}

echo "--- Running licenses-check.sh to find what changed ---"
# `|| rc=$?`, not a bare `rc=$?` after: with -e now active, an unguarded
# `output=$(run_check)` would abort the whole script the instant
# licenses-check.sh exits nonzero — exactly the normal, expected case this
# line exists to detect, not a bug. Same idiom build-test-licenses-check.sh
# itself uses for `go list` for the identical reason (see its own comment).
rc=0
output=$(run_check) || rc=$?
echo "$output"

if [ "$rc" -eq 0 ]; then
    echo "✅ $DOC already matches the binary. Nothing to refresh."
    exit 0
fi

# sort -u: this script's sibling (refresh-build-test-inventory.sh) has a
# confirmed, reproduced bug from *not* doing this — its checker loops per
# test submodule and can emit byte-identical diagnostics for one dependency
# shared by two of them, which without deduplication insert the same row
# twice while still reporting success. licenses-check.sh reasons about a
# single closure (./cmd/server) so this specific shape is less likely here,
# but deduplicating byte-identical lines is always safe (two genuinely
# different diagnostics differ in text and are never merged) and costs
# nothing, so it applies here too rather than relying on that distinction.
doc_errors=$(grep -E "^::error file=${DOC}::" <<<"$output" | sort -u || true)
if [ -z "$doc_errors" ]; then
    echo "::error::licenses-check.sh failed, but reported no ::error file=${DOC}:: line." \
        "The failure is in something this script does not touch (see the full output above" \
        "— NOTICE propagation is the most likely case) and needs a human." >&2
    exit 1
fi

to_add=()   # "mod_path\tmod_version"
to_fix=()   # "name\told_version\tnew_version"
to_drop=()  # "name"
unhandled=()

while IFS= read -r line; do
    [ -n "$line" ] || continue
    msg="${line#::error file=${DOC}::}"

    if [[ "$msg" =~ ^\`([^\`]+)\`\ is\ listed\ but\ is\ not\ in\ the\ dependency\ closure ]]; then
        to_drop+=("${BASH_REMATCH[1]}")
        continue
    fi
    # Trailing-period caveat: versions legitimately contain periods (v1.2.3),
    # so this is anchored on the literal "but the binary uses " marker and the
    # message's own terminating period, not on \S+.
    if [[ "$msg" =~ ^\`([^\`]+)\`\ is\ listed\ at\ (.+)\ but\ the\ binary\ uses\ (.+)\.$ ]]; then
        to_fix+=("${BASH_REMATCH[1]}"$'\t'"${BASH_REMATCH[2]}"$'\t'"${BASH_REMATCH[3]}")
        continue
    fi
    if [[ "$msg" =~ ^([^\ ]+)@([^\ ]+)\ is\ compiled\ into\ the\ binary\ but\ is\ not\ listed ]]; then
        to_add+=("${BASH_REMATCH[1]}"$'\t'"${BASH_REMATCH[2]}")
        continue
    fi
    unhandled+=("$line")
done <<<"$doc_errors"

if [ "${#unhandled[@]}" -gt 0 ]; then
    echo "::error::licenses-check.sh reported ${#unhandled[@]} problem(s) this script does not know" \
        "how to fix automatically. Refusing to write a partial fix. A human needs to look at:" >&2
    printf '%s\n' "${unhandled[@]}" >&2
    exit 1
fi

echo "--- Applying ${#to_fix[@]} version fix(es), ${#to_add[@]} new row(s), ${#to_drop[@]} removal(s) ---"

for entry in "${to_fix[@]}"; do
    IFS=$'\t' read -r name old_version new_version <<<"$entry"
    new_version=$(normalize_pseudo_version "$new_version")
    echo "  fix: \`$name\` $old_version -> $new_version"
    set_row_field "$DOC" "$name" 2 "$new_version"
    substitute_in_row_field "$DOC" "$name" 4 "$old_version" "$new_version"
done

for entry in "${to_add[@]}"; do
    IFS=$'\t' read -r mod_path mod_version <<<"$entry"
    mod_version=$(normalize_pseudo_version "$mod_version")
    echo "  add: \`$mod_path\` @ $mod_version"
    license_line=$(fetch_go_module_license "$mod_path")
    if [ -z "$license_line" ]; then
        echo "::error::Could not resolve a single, unambiguous licence for \`$mod_path\` via" \
            "go-licenses. Add its row by hand, verifying the licence from its own LICENSE file" \
            "as THIRD_PARTY_LICENSES.md's own regenerate instructions describe." >&2
        exit 1
    fi
    spdx="${license_line%%$'\t'*}"
    url="${license_line#*$'\t'}"
    row="| \`${mod_path}\` | ${mod_version} | ${spdx} | [license](${url}) |"
    # An allow-list, not "MPL-2.0 or else Permissive": this file's own Verdict
    # section states as fact that no strong-copyleft licence (GPL, LGPL, AGPL,
    # EPL, CDDL) is linked into the binary. A binary equality check against
    # only the one known weak-copyleft id would silently file a real
    # strong-copyleft dependency under "## Permissive components" the moment
    # go-licenses ever resolved one — the exact kind of guess this script's
    # own header promises never to make, and one that would make the file
    # actively lie about a compliance-relevant fact while the gate stays
    # green (licenses-check.sh never inspects licence *names*, only that a
    # row exists at the right version). Every SPDX id currently in this file
    # is permissive; anything else, known-copyleft or simply unrecognized,
    # refuses rather than picks a table.
    case "$spdx" in
        MIT | BSD-3-Clause | Apache-2.0 | ISC)
            insert_row_in_table "$DOC" "## Permissive components" "$row"
            ;;
        MPL-2.0)
            insert_row_in_table "$DOC" "Weak-copyleft components (MPL-2.0)" "$row"
            ;;
        *)
            echo "::error::\`$mod_path\` resolved to licence '$spdx', which this script does not" \
                "recognize as either the permissive set or MPL-2.0. This may be a genuine" \
                "strong-copyleft dependency (GPL, LGPL, AGPL, EPL, CDDL) — verify by hand and add" \
                "its row, choosing its table deliberately rather than trusting either default." >&2
            exit 1
            ;;
    esac
done

for name in "${to_drop[@]}"; do
    echo "  drop: \`$name\`"
    delete_row "$DOC" "$name"
done

# Housekeeping only — never load-bearing for the gate, but a rewritten file
# with a stale "Generated" date reads as though nobody looked at it.
today=$(date -u +%Y-%m-%d 2>/dev/null || true)
if [ -n "$today" ]; then
    sed -i.bak -E "s/^\*\*Generated\*\* [0-9]{4}-[0-9]{2}-[0-9]{2}/\*\*Generated\*\* ${today}/" "$DOC" 2>/dev/null || true
    rm -f "$DOC.bak"
fi

echo "--- Re-running licenses-check.sh to verify the rewrite ---"
verify_rc=0
verify_output=$(run_check) || verify_rc=$?
echo "$verify_output"

if [ "$verify_rc" -ne 0 ]; then
    echo "::error::$DOC still does not match after the automated rewrite. Refusing to leave this" \
        "half-fixed silently — see the errors above for what's left, and fix the rest by hand." >&2
    exit 1
fi

echo "✅ $DOC refreshed and verified."
