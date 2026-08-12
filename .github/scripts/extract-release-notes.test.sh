#!/usr/bin/env bash
#
# Self-test for extract-release-notes.sh. Builds throwaway CHANGELOG fixtures,
# runs the real script against them, and asserts the exit code and published
# body. Covers the release gate (block must exist AND carry a real entry) and
# the curated-summary preference/fallback, so this gating script's logic is
# verified on every PR rather than trusted — same pattern as the other gates.
#
# Run manually:  .github/scripts/extract-release-notes.test.sh
# Runs in CI as the `release_notes_selftest` job in .github/workflows/ci-pr.yaml.
#
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
EXTRACT="${SCRIPT_DIR}/extract-release-notes.sh"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

pass_count=0
fail_count=0
pass() { pass_count=$((pass_count + 1)); printf 'ok   - %s\n' "$1"; }
fail() { fail_count=$((fail_count + 1)); printf 'FAIL - %s\n' "$1"; [ -n "${2:-}" ] && printf '       %s\n' "$2"; return 0; }

# fixture <name> — make a dir with a CHANGELOG.md read from stdin; echo its path.
fixture() {
  local dir="$WORK/$1"
  mkdir -p "$dir"
  cat > "$dir/CHANGELOG.md"
  printf '%s' "$dir"
}

# run <dir> <tag> — run the script in <dir>; set globals RC, OUT_BODY, ERR_LOG.
run() {
  local dir="$1" tag="$2"
  RC=0
  ( cd "$dir" && bash "$EXTRACT" "$tag" out.md >/dev/null 2>err.log ) || RC=$?
  OUT_BODY=""; ERR_LOG=""
  if [ -f "$dir/out.md" ]; then OUT_BODY=$(cat "$dir/out.md"); fi
  if [ -f "$dir/err.log" ]; then ERR_LOG=$(cat "$dir/err.log"); fi
  return 0
}

# Herestring, not printf|grep -q: grep short-circuits and would SIGPIPE the
# writer on a large body (the Case 9 fixture is >64KB), a false negative here.
has()  { grep -q "$2" <<<"$1"; }        # body/log contains pattern
lacks() { ! grep -q "$2" <<<"$1"; }     # body/log does not contain

# --- Case 1: version not present in CHANGELOG -> refuse -----------------------
d=$(fixture missing <<'EOF'
# Changelog
## [Unreleased]
## [0.6.0] - 2026-01-01
- real entry.
EOF
)
run "$d" v9.9.9
if [ "$RC" -ne 0 ]; then pass "missing version refuses"; else fail "missing version refuses" "rc=$RC"; fi

# --- Case 2: heading-only block (no entries) -> refuse ------------------------
d=$(fixture headingonly <<'EOF'
# Changelog
## [0.7.0] - 2026-01-02
## [0.6.0] - 2026-01-01
- real entry.
EOF
)
run "$d" v0.7.0
if [ "$RC" -ne 0 ]; then pass "entry-less block refuses"; else fail "entry-less block refuses" "rc=$RC"; fi

# --- Case 3: entries present, no summary -> verbatim block, scoped -----------
d=$(fixture verbatim <<'EOF'
# Changelog
## [0.7.0] - 2026-01-02
### Added
- shiny thing.
## [0.6.0] - 2026-01-01
- old thing.
EOF
)
run "$d" v0.7.0
if [ "$RC" -eq 0 ] && has "$OUT_BODY" 'shiny thing' && lacks "$OUT_BODY" 'old thing'; then
  pass "verbatim block when no summary, scoped to version"
else
  fail "verbatim block when no summary, scoped to version" "rc=$RC body=[$OUT_BODY]"
fi

# --- Case 4: non-empty curated summary -> publish summary, not the block ------
d=$(fixture summary <<'EOF'
# Changelog
## [0.7.0] - 2026-01-02
### Added
- verbose detailed entry.
## [0.6.0] - 2026-01-01
- old thing.
EOF
)
mkdir -p "$d/.github/release-notes"
printf '## Highlights\n- concise summary.\n' > "$d/.github/release-notes/v0.7.0.md"
run "$d" v0.7.0
if [ "$RC" -eq 0 ] && has "$OUT_BODY" 'concise summary' && lacks "$OUT_BODY" 'verbose detailed'; then
  pass "non-empty curated summary is published"
else
  fail "non-empty curated summary is published" "rc=$RC body=[$OUT_BODY]"
fi

# --- Case 5: EMPTY curated summary -> fall back to verbatim + warning ---------
d=$(fixture emptysummary <<'EOF'
# Changelog
## [0.7.0] - 2026-01-02
### Added
- real entry here.
## [0.6.0] - 2026-01-01
- old thing.
EOF
)
mkdir -p "$d/.github/release-notes"
: > "$d/.github/release-notes/v0.7.0.md"
run "$d" v0.7.0
if [ "$RC" -eq 0 ] && has "$OUT_BODY" 'real entry here' && has "$ERR_LOG" 'empty'; then
  pass "empty curated summary falls back to verbatim and warns"
else
  fail "empty curated summary falls back to verbatim and warns" "rc=$RC body=[$OUT_BODY] err=[$ERR_LOG]"
fi

# --- Case 6: summary file for a DIFFERENT tag is ignored ----------------------
d=$(fixture othertag <<'EOF'
# Changelog
## [0.7.0] - 2026-01-02
- seven entry.
EOF
)
mkdir -p "$d/.github/release-notes"
printf -- '- WRONG.\n' > "$d/.github/release-notes/v9.9.9.md"
run "$d" v0.7.0
if [ "$RC" -eq 0 ] && has "$OUT_BODY" 'seven entry' && lacks "$OUT_BODY" 'WRONG'; then
  pass "summary for another tag is ignored"
else
  fail "summary for another tag is ignored" "rc=$RC body=[$OUT_BODY]"
fi

# --- Case 7: prefix collision — 0.1.0 must not match 0.11.0 -------------------
d=$(fixture prefix <<'EOF'
# Changelog
## [0.11.0] - 2026-01-03
- eleven.
## [0.1.0] - 2026-01-02
- one.
## [0.0.1] - 2026-01-01
- zero.
EOF
)
run "$d" v0.1.0
if [ "$RC" -eq 0 ] && has "$OUT_BODY" '^- one\.' && lacks "$OUT_BODY" 'eleven' && lacks "$OUT_BODY" 'zero'; then
  pass "0.1.0 is not confused with 0.11.0 and stops at 0.0.1"
else
  fail "0.1.0 is not confused with 0.11.0 and stops at 0.0.1" "rc=$RC body=[$OUT_BODY]"
fi

# --- Case 8: newest block must not swallow [Unreleased] above it --------------
d=$(fixture newest <<'EOF'
# Changelog
## [Unreleased]
## [0.7.0] - 2026-01-02
- seven.
## [0.6.0] - 2026-01-01
- six.
EOF
)
run "$d" v0.7.0
if [ "$RC" -eq 0 ] && has "$OUT_BODY" 'seven' && lacks "$OUT_BODY" 'Unreleased' && lacks "$OUT_BODY" 'six'; then
  pass "newest block excludes [Unreleased] and the previous version"
else
  fail "newest block excludes [Unreleased] and the previous version" "rc=$RC body=[$OUT_BODY]"
fi

# --- Case 9: block larger than the pipe buffer (~64KB) is accepted -----------
# Regression for the printf|grep -q SIGPIPE+pipefail false-refusal.
d="$WORK/large"; mkdir -p "$d"
{ echo "# Changelog"; echo "## [0.7.0] - 2026-01-02"; echo "- early entry"; \
  head -c 200000 /dev/zero | tr '\0' 'x' | fold -w 120; \
  echo "## [0.6.0] - 2026-01-01"; echo "- old."; } > "$d/CHANGELOG.md"
run "$d" v0.7.0
if [ "$RC" -eq 0 ] && has "$OUT_BODY" 'early entry'; then
  pass "block larger than the pipe buffer is accepted (SIGPIPE regression)"
else
  fail "block larger than the pipe buffer is accepted (SIGPIPE regression)" "rc=$RC"
fi

printf '\n%d passed, %d failed\n' "$pass_count" "$fail_count"
[ "$fail_count" -eq 0 ]
