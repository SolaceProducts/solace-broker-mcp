#!/usr/bin/env bash
#
# Produce the GitHub Release body for a release tag.
#
# The CHANGELOG.md block is the gate and the fallback; a curated human-readable
# summary is preferred when the release PR committed one. Concretely:
#   1. Extract the ## [X.Y.Z] block from CHANGELOG.md and REFUSE to release unless
#      it exists AND carries at least one real entry (a "- " line) — a heading-only
#      block ships empty notes and usually means the promotion never merged.
#   2. If .github/release-notes/<tag>.md exists (a concise, skimmable summary the
#      cut-release skill drafts and commits in the prepare-release PR), publish
#      that as the Release body. Otherwise fall back to the verbatim CHANGELOG
#      block — so older tags and skill-less cuts behave exactly as before.
# The detailed CHANGELOG stays the verbose source of truth either way.
#
# Tags are v-prefixed (v0.6.0); CHANGELOG headings are bare (## [0.6.0]).
#
# Usage: extract-release-notes.sh <tag> [outfile]
#
set -euo pipefail

TAG="${1:?usage: extract-release-notes.sh <tag> [outfile]}"
OUT="${2:-release-notes.md}"
VERSION="${TAG#v}"
SUMMARY=".github/release-notes/${TAG}.md"

# NOTE: pre-release tags (e.g. v0.6.0-beta.1) have no dedicated CHANGELOG heading
# — Keep a Changelog uses stable dated blocks — so they will fail extraction.
# Pre-release publishing is [Planned] in RELEASING.md; when it lands, strip the
# pre-release suffix here (or skip the gate for pre-release tags).

# Gate + fallback source: the verbatim CHANGELOG block for this version.
BLOCK="$(awk -v v="$VERSION" '
  BEGIN { gsub(/\./, "\\.", v) }                 # escape dots for the regex
  $0 ~ ("^## \\[" v "\\]") { p = 1; print; next }
  p && /^## \[/            { exit }
  p                        { print }
' CHANGELOG.md)"

if [ -z "$BLOCK" ]; then
  echo "::error::No CHANGELOG.md section found for version ${VERSION} (tag ${TAG})." >&2
  echo "::error::Promote [Unreleased] to a dated ## [${VERSION}] block before tagging (see RELEASING.md). Refusing to release." >&2
  exit 1
fi

# A heading-only block passes a bare "section exists" check but ships empty notes.
# Require at least one real entry line so an unfilled promotion can't slip through.
# Herestring, not `printf … | grep -q`: grep -q short-circuits on the first match
# and would SIGPIPE the writer on a block larger than the pipe buffer (~64KB),
# which pipefail then turns into a false "no entries" refusal of a valid release.
if ! grep -q '^- ' <<<"$BLOCK"; then
  echo "::error::CHANGELOG.md section for version ${VERSION} (tag ${TAG}) has no entries." >&2
  echo "::error::The ## [${VERSION}] block must contain at least one '- ' entry (see RELEASING.md). Refusing to release." >&2
  exit 1
fi

# Prefer the curated summary committed in the release PR; else the verbatim block.
# A committed-but-empty summary would otherwise wipe the release body even though the
# CHANGELOG gate passed, so treat empty as absent: fall back to the (already
# gate-validated) verbatim block and warn, rather than shipping empty notes.
if [ -s "$SUMMARY" ]; then
  cp "$SUMMARY" "$OUT"
  echo "Published curated release notes for ${VERSION} from ${SUMMARY} into ${OUT}." >&2
else
  if [ -e "$SUMMARY" ]; then
    echo "::warning::Curated release notes ${SUMMARY} exist but are empty; falling back to the verbatim CHANGELOG block." >&2
  fi
  printf '%s\n' "$BLOCK" > "$OUT"
  echo "Published verbatim CHANGELOG block for ${VERSION} into ${OUT}." >&2
fi

