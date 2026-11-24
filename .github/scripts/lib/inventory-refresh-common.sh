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
    # LC_ALL=C: the sort-position decision below (`newrow < $0`) is a string
    # comparison, and awk's `<` collates according to the active locale. Under
    # anything but a C/POSIX locale that collation can differ from plain byte
    # order (and differ between a contributor's machine and CI), making row
    # placement non-deterministic across environments — cosmetic (the gate
    # doesn't care what order rows are in), but still unwanted diff churn.
    # Forcing the C locale for just this invocation makes the comparison a
    # fixed byte-order sort everywhere this runs.
    LC_ALL=C awk -v anchor="$anchor" -v newrow="$newrow" '
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
# check_gh_available — verifies `gh` is installed and authenticated. Both
# resolve_action_tag's tag-list fallback and fetch_action_license shell out to
# `gh api` with stderr suppressed and errors folded to empty output (by
# design: a rate limit, a 404, and a missing tool must all resolve to the same
# "could not verify, refuse rather than guess" signal). Without this check,
# `gh` being missing or unauthenticated looks identical to a real "no tag
# found", and the caller's generic error names the wrong cause. Cached after
# the first call — a run touching several actions should print this once, not
# once per action.
_gh_preflight_ok=""
check_gh_available() {
    if [ -n "$_gh_preflight_ok" ]; then
        [ "$_gh_preflight_ok" = "1" ]
        return
    fi
    if ! command -v gh >/dev/null 2>&1; then
        echo "::error::'gh' is not installed. Action licence/tag lookups need it — install the" \
            "GitHub CLI (https://cli.github.com) and retry." >&2
        _gh_preflight_ok=0
        return 1
    fi
    if ! gh auth status >/dev/null 2>&1; then
        echo "::error::'gh' is installed but not authenticated (checked via 'gh auth status')." \
            "Run 'gh auth login', or set GH_TOKEN/GITHUB_TOKEN in the environment, and retry." >&2
        _gh_preflight_ok=0
        return 1
    fi
    _gh_preflight_ok=1
    return 0
}

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
    local owner_repo="$1" action="$2" sha="$3" tag action_re
    # Escape every ERE metacharacter in the action path, not only `/` (which
    # isn't actually special in ERE and needed no escaping in the first
    # place). Action paths routinely contain `.` — a reusable-workflow
    # reference like `owner/repo/.github/workflows/file.yaml@ref` is exactly
    # this shape — and an unescaped `.` matches any character, so it could
    # match a similarly-shaped `uses:` line for a *different* action and
    # resolve the wrong trailing-comment tag.
    # The bracket expression puts `.` right after the leading literal `]`
    # rather than right after the opening `[` (i.e. `[].^$...]`, not
    # `[][.^$...]`): `[` immediately followed by `.` starts a POSIX collating
    # symbol (`[.symbol.]`), which sed then reads as unterminated and aborts
    # with a parse error — caught by testing this exact pattern by hand.
    action_re=$(sed -E 's/[].^$*+?(){}|\\]/\\&/g' <<<"$action")
    tag=$(
        grep -rhoE "uses:[[:space:]]*${action_re}@${sha}[[:space:]]*#[[:space:]]*v[0-9][^[:space:]]*" \
            .github/workflows/ 2>/dev/null |
            head -1 | grep -oE 'v[0-9][^[:space:]]*$' || true
    )
    if [ -z "$tag" ]; then
        local candidates full_form_tags
        check_gh_available || { printf ''; return 0; }
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

# is_recognized_license <spdx-id>
#   True for a licence this tooling may fold into a table without a human
#   specifically deciding where it goes: the permissive set both documents'
#   own Verdict sections name, plus MPL-2.0 (the one weak-copyleft exception
#   THIRD_PARTY_LICENSES.md documents and gives its own table). Anything
#   else — a real strong-copyleft licence, `NOASSERTION` (a real value
#   GitHub's API returns at HTTP 200 for some repos, e.g. torvalds/linux),
#   `Unknown`, a typo — is deliberately unrecognized: never guess a licence
#   classification, surface it for a human instead.
is_recognized_license() {
    case "$1" in
        MIT | BSD-3-Clause | Apache-2.0 | ISC | MPL-2.0) return 0 ;;
        *) return 1 ;;
    esac
}

# fetch_action_license <owner/repo> <ref>
#   Prints "<spdx-id>\t<html-url>" for the given ref (a tag, never a bare SHA —
#   see THIRD_PARTY_BUILD_TEST.md's segmentio/asm note on why a ref-less query
#   answers for the default branch, not the pinned version). Prints nothing on
#   any failure; callers must treat that as "could not verify, do not guess"
#   and fail the job rather than write a licence they didn't confirm.
#
#   Validates its own result before returning it, rather than trusting
#   whatever `gh api` printed: a failed or rate-limited call can still exit
#   with output on stdout (an error-shaped JSON body that `--jq` folds to
#   something non-empty but not the two real fields), and `2>/dev/null`
#   hides the stderr message that would otherwise explain why. Requiring the
#   tab-separated two-field shape, both fields non-empty, AND a recognized
#   licence closes that gap without needing to know exactly which failure
#   mode produced the bad value.
fetch_action_license() {
    local owner_repo="$1" ref="$2" raw spdx url
    check_gh_available || return 0
    raw=$(gh api "repos/${owner_repo}/license?ref=${ref}" --jq '[.license.spdx_id, .html_url] | @tsv' 2>/dev/null) || raw=""
    [ -n "$raw" ] || return 0
    IFS=$'\t' read -r spdx url <<<"$raw"
    [ -n "$spdx" ] && [ -n "$url" ] || return 0
    is_recognized_license "$spdx" || return 0
    printf '%s\t%s' "$spdx" "$url"
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

# --- NOTICE propagation (SOL-159006) -----------------------------------------
#
# Used only by refresh-licenses-inventory.sh's opt-in REFRESH_NOTICE=1 mode.
# licenses-check.sh's NOTICE check finds a module's NOTICE with
# `find <dir> -maxdepth 1 -iname 'NOTICE*'` and then only checks that NOTICE
# names the module path; it never reads the attribution text. So the copy
# below is the only thing standing between "the gate is green" and "the
# attribution is right", and it refuses every shape it cannot copy verbatim
# with confidence rather than guessing.

# notice_source_file <module-dir>
#   Prints the path of the module's single root NOTICE file. Returns 1, with
#   the reason on stderr, if there is not exactly one NOTICE* file at the
#   root, if its name is not NOTICE or NOTICE.txt (any case), if it is empty
#   or whitespace-only, or if it is not plain text this repo's NOTICE can take
#   verbatim (a NUL byte, not valid UTF-8, CRLF line endings, or a byte-order
#   mark, which would land mid-file). The same find expression licenses-check.sh uses, so the two can
#   never disagree about which files are candidates.
notice_source_file() {
    local dir="$1" files count f upper
    files=$(find "$dir" -maxdepth 1 -iname 'NOTICE*')
    count=$(grep -c . <<<"$files" || true)
    if [ "$count" -ne 1 ]; then
        echo "notice_source_file: expected exactly one NOTICE file at the root of $dir, found $count" >&2
        return 1
    fi
    f="$files"
    upper=$(basename "$f" | tr '[:lower:]' '[:upper:]')
    if [ ! -f "$f" ] || { [ "$upper" != NOTICE ] && [ "$upper" != NOTICE.TXT ]; }; then
        echo "notice_source_file: $f is not a plain NOTICE or NOTICE.txt file" >&2
        return 1
    fi
    if ! LC_ALL=C grep -q '[^[:space:]]' "$f"; then
        echo "notice_source_file: $f is empty" >&2
        return 1
    fi
    if [ "$(tr -d '\000' <"$f" | wc -c)" -ne "$(wc -c <"$f")" ]; then
        echo "notice_source_file: $f contains NUL bytes, so it is not plain text" >&2
        return 1
    fi
    if ! iconv -f UTF-8 -t UTF-8 "$f" >/dev/null 2>&1; then
        echo "notice_source_file: $f is not valid UTF-8 text" >&2
        return 1
    fi
    if LC_ALL=C grep -q $'\r' "$f"; then
        echo "notice_source_file: $f has CRLF line endings" >&2
        return 1
    fi
    if [ "$(head -c 3 "$f" | od -An -tx1 | tr -d ' \n')" = efbbbf ]; then
        echo "notice_source_file: $f starts with a byte-order mark" >&2
        return 1
    fi
    printf '%s\n' "$f"
}

# insert_notice_block <notice> <module-path> <source-notice>
#   Adds <source-notice> to the "Required attributions propagated from bundled
#   dependencies" section of <notice> as a block: a header line naming the
#   module path, then the source text with every non-blank line indented two
#   spaces (the rest of each line byte-for-byte as upstream has it), blank and
#   whitespace-only lines left empty, and trailing blank lines dropped. That
#   is the shape of every block already in the file.
#
#   Placement: a block's sort key is its smallest header line (a hand-merged
#   block can carry several headers, e.g. the yaml group), compared in byte
#   order. The new block goes before the first block whose key sorts after
#   <module-path>, or at the end of the section. A header line is a
#   non-indented line with no spaces that follows a blank line (or another
#   header line); the section's intro paragraph never matches that.
#
#   Returns 1 and leaves <notice> untouched if the section is missing or a
#   block for <module-path> already exists.
insert_notice_block() {
    local notice="$1" mod="$2" src="$3" block
    block=$(mktemp)
    LC_ALL=C awk -v mod="$mod" '
        { lines[NR] = $0 }
        END {
            n = NR
            while (n > 0 && lines[n] ~ /^[[:space:]]*$/) n--
            print mod
            for (i = 1; i <= n; i++) {
                if (lines[i] ~ /^[[:space:]]*$/) print ""
                else print "  " lines[i]
            }
        }
    ' "$src" >"$block" || { rm -f "$block"; return 1; }
    # LC_ALL=C for the same reason as insert_row_in_table: awk string
    # comparison collates by locale otherwise.
    if ! LC_ALL=C awk -v mod="$mod" -v blockfile="$block" '
        function is_header(s) {
            return s != "" && s !~ /[[:space:]]/ && s !~ /^-+$/
        }
        { lines[NR] = $0 }
        END {
            n = NR
            sec = 0
            for (i = 1; i <= n; i++) {
                if (lines[i] == "Required attributions propagated from bundled dependencies") { sec = i; break }
            }
            if (!sec) {
                print "insert_notice_block: no \"Required attributions\" section in NOTICE" > "/dev/stderr"
                exit 1
            }
            sec_end = n + 1
            for (i = sec + 1; i <= n; i++) {
                if (lines[i] ~ /^---+$/) { sec_end = i; break }
            }
            at = 0
            for (i = sec + 1; i < sec_end; i++) {
                if (!is_header(lines[i]) || lines[i - 1] != "") continue
                key = lines[i]
                j = i
                while (j + 1 < sec_end && is_header(lines[j + 1])) {
                    j++
                    if (lines[j] < key) key = lines[j]
                }
                for (k = i; k <= j; k++) {
                    if (lines[k] == mod) {
                        print "insert_notice_block: NOTICE already has a block for " mod > "/dev/stderr"
                        exit 1
                    }
                }
                if (!at && key > mod) at = i
                i = j
            }
            if (!at) at = sec_end
            nb = 0
            while ((getline line < blockfile) > 0) blk[++nb] = line
            for (i = 1; i <= n; i++) {
                if (i == at) {
                    for (k = 1; k <= nb; k++) print blk[k]
                    print ""
                }
                print lines[i]
            }
            if (at == n + 1) {
                print ""
                for (k = 1; k <= nb; k++) print blk[k]
            }
        }
    ' "$notice" >"$notice.tmp"; then
        rm -f "$notice.tmp" "$block"
        return 1
    fi
    rm -f "$block"
    mv "$notice.tmp" "$notice"
}
