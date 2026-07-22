#!/usr/bin/env bash
#
# Extract the CHANGELOG.md block for a release tag so the GitHub Release body is
# minted from the curated changelog rather than auto-generated from scratch.
# Tags are v-prefixed (v0.6.0); CHANGELOG headings are bare (## [0.6.0]).
#
# Usage: extract-release-notes.sh <tag> [outfile]
# Fails (non-zero) if no section exists for the version, so a release cannot ship
# with notes that never touched the CHANGELOG.
#
set -euo pipefail

TAG="${1:?usage: extract-release-notes.sh <tag> [outfile]}"
OUT="${2:-release-notes.md}"
VERSION="${TAG#v}"

# NOTE: pre-release tags (e.g. v0.6.0-beta.1) have no dedicated CHANGELOG heading
# — Keep a Changelog uses stable dated blocks — so they will fail extraction.
# Pre-release publishing is [Planned] in RELEASING.md; when it lands, strip the
# pre-release suffix here (or skip the gate for pre-release tags).

awk -v v="$VERSION" '
  BEGIN { gsub(/\./, "\\.", v) }                 # escape dots for the regex
  $0 ~ ("^## \\[" v "\\]") { p = 1; print; next }
  p && /^## \[/            { exit }
  p                        { print }
' CHANGELOG.md > "$OUT"

if [ ! -s "$OUT" ]; then
  echo "::error::No CHANGELOG.md section found for version ${VERSION} (tag ${TAG})." >&2
  echo "::error::Promote [Unreleased] to a dated ## [${VERSION}] block before tagging (see RELEASING.md). Refusing to release." >&2
  exit 1
fi

echo "Extracted release notes for ${VERSION} into ${OUT}." >&2
