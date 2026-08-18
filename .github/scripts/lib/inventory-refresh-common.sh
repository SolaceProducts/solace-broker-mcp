#!/usr/bin/env bash
#
# Shared primitives for refresh-licenses-inventory.sh and
# refresh-build-test-inventory.sh (SOL-152956).
#
# WHY THIS EXISTS AS A SEPARATE FILE, SOURCED RATHER THAN COPIED
#
# Both refresh scripts edit the same shape of thing — a markdown table row
# addressed by its backticked first column — using different diagnostic
# grammars (licenses-check.sh's and build-test-licenses-check.sh's own error
# text differs; see each refresh script's header). The row-editing primitives
# below don't care which grammar produced the edit, so one copy here is safer
# than two copies that could drift from each other.
#
# WHAT THIS DELIBERATELY DOES NOT DO
#
# It does not read or reimplement anything from build-test-licenses-check.sh or
# licenses-check.sh — those two are out of scope to change (SOL-152956), and a
# second copy of their discover()/expect() logic here would be exactly the kind
# of drift-prone duplication this ticket exists to eliminate. The refresh
# scripts that source this file get their "what changed" list by parsing the
# check scripts' own `::error file=...::` diagnostics, not by recomputing
# anything independently. This file only rewrites markdown once told what to
# change and where.
#
# Every row-mutating function here fails loudly (nonzero return, or `set -e`
# propagates) rather than silently no-op'ing, mirroring
# build-test-licenses-check.test.sh's own mutation helpers (drop_row,
# change_version): a sed/awk/grep that matches nothing exits 0 in bash, and
# treating that as success would mean the refresh script reports "fixed" while
# the file is untouched.

# --- row field rewrite -------------------------------------------------------
#
# Tables in these two files carry 3, 4, or 5 columns depending on which table
# (see each refresh script). Rebuilding from NF rather than a fixed column
# count avoids the exact bug build-test-licenses-check.test.sh's own
# change_version() comment warns about: a hardcoded 4-column rebuild silently
# truncates a 5-column row.

# All four functions below match a row by its exact FIRST column
# ($1 == "| `name`"), not by whether the backticked name appears anywhere in
# the line. A plain substring/grep match is not safe here: several rows carry
# free-form prose in a later column, or hand-written notes elsewhere in the
# document, that repeat another row's exact backticked name — e.g.
# THIRD_PARTY_BUILD_TEST.md has both the `golang.org/x/oauth2` table row and a
# later paragraph starting "**`golang.org/x/oauth2` is pinned here at
# v0.35.0...**", and a substring match would touch both. This is production
# automation that rewrites a real compliance artifact, not a test's throwaway
# mutation helper, so it holds to a stricter bar than
# build-test-licenses-check.test.sh's own change_version() (which accepts the
# same substring risk, but only ever corrupts a disposable tmpdir copy).

# get_row_field <doc> <name> <field-num>
#   Print field <field-num> (1-indexed, split on ' | ') of the row whose first
#   column is the backticked <name>, or nothing if no row matches. Needed
#   before overwriting a field whose old value must still be read once — e.g.
#   the old release tag, to find-and-replace it inside the license-URL cell
#   before the tag cell itself is overwritten with the new one.
get_row_field() {
    local doc="$1" name="$2" field="$3"
    awk -v comp="| \`$name\`" -v f="$field" -F' \\| ' '
        $1 == comp { print $f; exit }
    ' "$doc"
}

# set_row_field <doc> <name> <field-num> <new-value>
#   Replace field <field-num> (1-indexed, split on ' | ') of the row whose
#   first column is the backticked <name>, leaving every other field
#   untouched. Fails if no row matches.
set_row_field() {
    local doc="$1" name="$2" field="$3" value="$4"
    awk -v comp="| \`$name\`" -v val="$value" -v f="$field" -F' \\| ' '
        $1 == comp {
            $f = val
            row = $1
            for (i = 2; i <= NF; i++) row = row " | " $i
            print row
            hit = 1
            next
        }
        { print }
        END { exit !hit }
    ' "$doc" >"$doc.tmp" && mv "$doc.tmp" "$doc"
}

# substitute_in_row_field <doc> <name> <field-num> <old> <new>
#   Within field <field-num> of the row named <name>, replace the first
#   occurrence of literal string <old> with <new>. Used for license-URL cells,
#   which embed a version or tag as a substring of a larger URL rather than
#   being the whole cell. A no-op (old not found in that field) is not treated
#   as failure — not every version bump changes a URL-visible substring (e.g. a
#   commit-pinned row whose link never embedded the old value), so the caller
#   decides whether that is expected.
substitute_in_row_field() {
    local doc="$1" name="$2" field="$3" old="$4" new="$5"
    awk -v comp="| \`$name\`" -v f="$field" -v old="$old" -v new="$new" -F' \\| ' '
        $1 == comp {
            n = index($f, old)
            if (n > 0) {
                $f = substr($f, 1, n - 1) new substr($f, n + length(old))
            }
            row = $1
            for (i = 2; i <= NF; i++) row = row " | " $i
            print row
            hit = 1
            next
        }
        { print }
        END { exit !hit }
    ' "$doc" >"$doc.tmp" && mv "$doc.tmp" "$doc"
}

# delete_row <doc> <name>
#   Remove the one row whose first column is the backticked <name> — not, as a
#   plain `grep -vF "\`$name\`"` would, every line that merely contains that
#   substring. build-test-licenses-check.test.sh's own drop_row() uses exactly
#   that plain grep, but it is a test mutation operating on a disposable tmpdir
#   copy; this deletes from the real, committed document, where the same
#   substring can legitimately reappear in a hand-written note about the very
#   row being dropped (see the column-1-anchoring comment above this section).
#   Confirms exactly one line was removed — not merely "fewer", which a
#   substring match could also satisfy while taking an unrelated line with it.
delete_row() {
    local doc="$1" name="$2" before after
    before=$(wc -l <"$doc")
    awk -v comp="| \`$name\`" -F' \\| ' '$1 != comp' "$doc" >"$doc.tmp" && mv "$doc.tmp" "$doc"
    after=$(wc -l <"$doc")
    [ "$((before - after))" -eq 1 ]
}

# --- row insertion ------------------------------------------------------------
#
# A new row is inserted into a specific table, found by a literal anchor string
# that appears once before it (a heading, or other unique preceding text), then
# placed in alphabetical order among the `| \`...\` | ...` lines that
# immediately follow that table's header-separator line. Alphabetical order
# matches every table in both files as committed; it is cosmetic (the gate
# scripts don't care what order rows are in, or which table a row lives in —
# only that a row with the right name and version exists somewhere), but
# matching it means a human reviewing the bot's diff sees the same shape they
# would have produced by hand.
#
# insert_row_in_table <doc> <anchor> <new-row-line>
#   <anchor> is matched as a literal substring, not a regex — table headings
#   here contain backticks and parentheses that would need escaping as a
#   regex, and a wrong escape fails silently in a way a literal match cannot.
insert_row_in_table() {
    local doc="$1" anchor="$2" newrow="$3" name
    # Refuse rather than duplicate if a row for this exact name already exists
    # anywhere in the document. This function has no way to know why it was
    # called with a name that's already there — the caller believed this was a
    # genuinely new row — so the safe assumption is that something upstream is
    # wrong (a duplicated diagnostic, a caller bug), not that inserting a
    # second row is what was intended. This is the guard that would have
    # caught a real, reproduced bug: build-test-licenses-check.sh's per-submodule
    # Go-module check can emit the same "not listed" diagnostic twice for one
    # module shared by two submodules, and without this check both calls
    # succeeded, silently writing the row in twice.
    # Same column-1-exact-match anchoring as get_row_field/set_row_field/etc.
    # above, not a substring search — a name can legitimately appear inside
    # another row's free-form prose (see those functions' own comment), and
    # this check must not false-positive on that any more than they do.
    name=$(sed -E 's/^\| `([^`]+)`.*/\1/' <<<"$newrow")
    if [ -n "$name" ] && awk -v comp="| \`$name\`" -F' \\| ' '$1 == comp { found = 1 } END { exit !found }' "$doc"; then
        echo "insert_row_in_table: refusing to insert \`$name\` — a row for it already exists" >&2
        return 1
    fi
    awk -v anchor="$anchor" -v newrow="$newrow" '
        BEGIN { found_anchor = 0; in_table = 0; inserted = 0 }
        {
            if (!found_anchor) {
                print
                if (index($0, anchor) > 0) { found_anchor = 1 }
                next
            }
            if (found_anchor && !in_table) {
                print
                if ($0 ~ /^\|---/) { in_table = 1 }
                next
            }
            if (in_table && !inserted) {
                if ($0 ~ /^\| `/) {
                    if (newrow < $0) {
                        print newrow
                        inserted = 1
                    }
                    print
                    next
                } else {
                    # Table ended (blank line, prose, or next heading) before a
                    # later row was found: the new row sorts last in this table.
                    print newrow
                    inserted = 1
                    print
                    next
                }
            }
            print
        }
        END {
            if (!found_anchor) {
                print "insert_row_in_table: anchor not found: " anchor > "/dev/stderr"
                exit 1
            }
            # The table ran to the end of the file with no trailing line to
            # trigger the sorts-last branch above (e.g. the very last table in
            # the document, no blank line after its last row). The new row
            # still sorts last in that case; print it here instead of failing.
            if (in_table && !inserted) {
                print newrow
                inserted = 1
            }
            if (!inserted) {
                print "insert_row_in_table: anchor found but its table never started: " anchor > "/dev/stderr"
                exit 1
            }
        }
    ' "$doc" >"$doc.tmp" && mv "$doc.tmp" "$doc"
}

# --- GitHub Action license resolution -----------------------------------------
#
# resolve_action_tag <owner/repo> <action-path> <sha>
#   Prints the release tag for a SHA-pinned action. Dependabot's github-actions
#   ecosystem update rewrites the trailing `# vX.Y.Z` comment in the same commit
#   as the SHA it pins to, so that comment is checked first — zero network calls
#   in the common case. Falls back to matching the SHA against the repository's
#   tag list via the GitHub API. Prints nothing (not an error) if neither
#   source resolves a tag, e.g. a branch-pinned action with no tag at all — the
#   Solace-internal composite actions are exactly this case, and callers must
#   treat an empty result as "no Release column to fill", not as a failure.
resolve_action_tag() {
    local owner_repo="$1" action="$2" sha="$3" tag
    tag=$(
        grep -rhoE "uses:[[:space:]]*${action//\//\\/}@${sha}[[:space:]]*#[[:space:]]*v[0-9][^[:space:]]*" \
            .github/workflows/ 2>/dev/null |
            head -1 | grep -oE 'v[0-9][^[:space:]]*$' || true
    )
    if [ -z "$tag" ]; then
        local candidates full_form_tags
        candidates=$(gh api "repos/${owner_repo}/tags" --paginate --jq \
            ".[] | select(.commit.sha == \"${sha}\") | .name" 2>/dev/null || true)
        # A commit can carry both a full release tag (v7.0.1) and a rolling
        # major-version alias (v7) pointing at the same SHA; the API gives no
        # ordering guarantee between them. Every existing row in both docs uses
        # the full form, so prefer a full x.y.z tag over a bare-major one.
        full_form_tags=$(grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' <<<"$candidates" | sort -u || true)
        if [ "$(grep -c . <<<"$full_form_tags")" -gt 1 ]; then
            # More than one *distinct* full-form tag resolves to this SHA — a
            # backport or re-tag, not the usual major-alias case. Nothing here
            # says which one this document should record, and guessing (e.g.
            # `head -1`, whose order the API doesn't guarantee) can silently
            # write the wrong version into the Release column and the licence
            # URL. Refuse: an empty result is the existing "could not resolve"
            # signal every caller already treats as an error to surface.
            printf ''
            return 0
        fi
        tag="$full_form_tags"
        [ -n "$tag" ] || tag=$(head -1 <<<"$candidates")
    fi
    printf '%s' "$tag"
}

# fetch_action_license <owner/repo> <ref>
#   Prints "<spdx-id>\t<html-url>" for the given ref (a tag, never a bare SHA —
#   see THIRD_PARTY_BUILD_TEST.md's segmentio/asm note on why a ref-less query
#   answers for the default branch, not the pinned version). Prints nothing on
#   any failure; callers must treat that as "could not verify, do not guess"
#   and fail the job rather than write a licence they didn't confirm.
fetch_action_license() {
    local owner_repo="$1" ref="$2"
    gh api "repos/${owner_repo}/license?ref=${ref}" --jq '[.license.spdx_id, .html_url] | @tsv' 2>/dev/null || true
}

# normalize_pseudo_version <version>
#   Mirrors licenses-check.sh's own normalize_version() exactly: folds a Go
#   pseudo-version (v0.0.0-<timestamp>-<commit>) down to the bare trailing
#   commit; every other shape (an ordinary tag, a prerelease, `+incompatible`,
#   a non-v0 pseudo-version) passes through unchanged, matching that script's
#   own documented exceptions. Duplicated deliberately, not sourced from that
#   out-of-scope script: this is a small, pure string transform, not
#   discovery/business logic, needed only so refresh-licenses-inventory.sh
#   WRITES rows in the same convention licenses-check.sh already tolerates
#   when READING them. Without it, a version bump on a commit-pinned row
#   (github.com/xeipuuv/* are exactly this shape today) would write the raw,
#   un-folded pseudo-version into both the version cell and — via
#   substitute_in_row_field — into the licence URL, producing a link like
#   `.../blob/v0.0.0-<ts>-<sha>/LICENSE` that does not resolve, while
#   licenses-check.sh's own comparison folds both sides and would report
#   success anyway.
normalize_pseudo_version() {
    case "$1" in
        v0.0.0-*-*) printf '%s' "${1##*-}" ;;
        *) printf '%s' "$1" ;;
    esac
}

# --- Go module license resolution ---------------------------------------------
#
# fetch_go_module_license <module-path>
#   Prints "<spdx-id>\t<url>" for a single Go module, read via go-licenses —
#   the same generator THIRD_PARTY_LICENSES.md's own regenerate instructions
#   name, so this is not a second, independent way of deciding a licence.
#   go-licenses reports at package granularity and can print more than one row
#   when the target resolves to several packages; only an unambiguous result
#   is accepted; anything else (no rows, or more than one distinct license
#   among matching rows) prints nothing so the caller fails rather than picks.
fetch_go_module_license() {
    local module_path="$1" csv matches distinct_licenses
    # Try the bare module path first — the common case, and it avoids pulling in
    # every subpackage's own transitive imports for no reason. Some modules have
    # no importable package at their root (github.com/google/jsonschema-go is a
    # real example already in this repo's closure: its own package lives at
    # .../jsonschema, not at the module root), which go-licenses reports as a
    # hard error rather than an empty result, so fall back to a wildcard under
    # the module path to reach it.
    csv=$(go run github.com/google/go-licenses@v1.6.0 csv "$module_path" 2>/dev/null || true)
    if [ -z "$csv" ]; then
        csv=$(go run github.com/google/go-licenses@v1.6.0 csv "${module_path}/..." 2>/dev/null || true)
    fi
    [ -n "$csv" ] || return 0
    # Prefer an exact package-path match; fall back to packages nested under
    # the module path (a module whose root has no importable package of its
    # own, only subpackages).
    matches=$(awk -F',' -v m="$module_path" '$1 == m' <<<"$csv")
    if [ -z "$matches" ]; then
        matches=$(awk -F',' -v m="${module_path}/" 'index($1, m) == 1' <<<"$csv")
    fi
    [ -n "$matches" ] || return 0
    distinct_licenses=$(awk -F',' '{print $3}' <<<"$matches" | sort -u)
    [ "$(wc -l <<<"$distinct_licenses")" -eq 1 ] || return 0
    awk -F',' -v OFS='\t' '{print $3, $2; exit}' <<<"$matches"
}
