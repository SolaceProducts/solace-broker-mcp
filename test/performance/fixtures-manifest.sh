#!/usr/bin/env bash
# Copyright 2024-2026 Solace Corporation. All rights reserved.
#
# fixtures-manifest.sh — record and verify the perf suite's captured fixtures.
#
# The fixtures (mock-semp/canned/*.{json,xml} and fidelity/golden/*.json) are
# captures from a real lab broker, are not in git, and only mean anything as a
# matched pair from one capture: the mock replays the canned bytes and the
# fidelity gate diffs the resulting tool output against the goldens
# byte-for-byte. Mix two captures and self-changing fields (uptime, memory %,
# disk usage) guarantee a red gate that looks like a real regression.
#
# regen-golden.sh writes a manifest after each capture; the run scripts verify
# it before starting anything.
#
# Usage:
#   ./fixtures-manifest.sh write              # after a capture (regen-golden.sh)
#   ./fixtures-manifest.sh check [--no-canned]  # preflight (run.sh, run-loadgen.sh)
#
# check fails when:
#   - the manifest is absent          → nothing has been captured here
#   - a recorded file is missing      → a partial or half-deleted capture
#   - a recorded file's hash moved    → hand-edited after capture
#   - a fixture on disk is unrecorded → a stray file mixed into the set
#
# check warns, but does not fail, when the capture is older than
# FIXTURE_AGE_WARN (default 7d). Age alone never blocks a run — a week-old
# capture that still hashes clean is internally consistent, which is what the
# gate actually depends on.
#
# --no-canned restricts the check to fidelity/golden/. Use it under NO_MOCK=1,
# where the mock (and its canned/) lives on another host.
#
# Env:
#   FIXTURE_AGE_WARN  age past which check prints a warning (default 7d;
#                     accepts <N>d or <N>h). Warning only.
#   BROKER_ALIAS/VPN  recorded by write as capture provenance.

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
manifest="$here/fixtures.manifest"
fixture_dirs=(mock-semp/canned fidelity/golden)

# fixture_paths prints every fixture file, relative to $here, sorted. Only
# .json/.xml — README.md, exclusions.txt and friends are repo files, not
# capture output, and must not land in the manifest.
fixture_paths() {
  local dirs=("$@")
  ( cd "$here" && find "${dirs[@]}" -maxdepth 1 -type f \
      \( -name '*.json' -o -name '*.xml' \) -print 2>/dev/null | LC_ALL=C sort )
}

# manifest_paths prints the file paths the manifest records (comments stripped).
# sha256sum's format is "<hash>  <path>", two spaces.
manifest_paths() {
  sed -e '/^#/d' -e '/^[[:space:]]*$/d' "$manifest" | cut -d' ' -f3- | LC_ALL=C sort
}

# age_seconds converts an FIXTURE_AGE_WARN-style duration (<N>d / <N>h) to
# seconds. An unparseable value is a caller mistake, not something to guess at.
age_seconds() {
  local v=$1
  case "$v" in
    *d) echo $(( ${v%d} * 86400 )) ;;
    *h) echo $(( ${v%h} * 3600 )) ;;
    *)  echo "FIXTURE_AGE_WARN must be <N>d or <N>h, got: $v" >&2; return 2 ;;
  esac
}

cmd_write() {
  local paths
  paths="$(fixture_paths "${fixture_dirs[@]}")"
  if [[ -z "$paths" ]]; then
    echo "no fixtures found under ${fixture_dirs[*]} — nothing to record" >&2
    return 2
  fi

  # A single manifest written at the end of one regen run is what ties the two
  # fixture sets together: there is no way to record half a capture, so a
  # passing check implies canned and golden came from the same pass.
  {
    echo "# perf fixtures manifest — written by regen-golden.sh. Local only, not committed."
    echo "# Do not edit: the run scripts verify these hashes and will fail on a mismatch."
    echo "# run_id: $(date -u +%Y%m%dT%H%M%SZ)"
    echo "# captured_at: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "# broker_alias: ${BROKER_ALIAS:-unknown}"
    echo "# vpn: ${VPN:-unknown}"
    ( cd "$here" && printf '%s\n' "$paths" | xargs sha256sum )
  } >"$manifest"

  echo "recorded $(printf '%s\n' "$paths" | wc -l) fixtures in $manifest"
}

cmd_check() {
  local no_canned=0
  [[ "${1:-}" == "--no-canned" ]] && no_canned=1

  local dirs=("${fixture_dirs[@]}")
  local check_file="$manifest"
  if (( no_canned )); then
    dirs=(fidelity/golden)
  fi

  if [[ ! -f "$manifest" ]]; then
    cat >&2 <<EOF
!! no $manifest — this checkout has never captured fixtures.

   The mock's canned SEMP responses and fidelity/golden/*.json are captures
   from a real broker and are not in git. Capture them (both sets, one pass):

     CONFIG_FILE=./broker-config.real.yaml ./regen-golden.sh

   See README.md "Regenerating the fixtures".
EOF
    return 1
  fi

  # Under --no-canned, verify only the golden lines. Filtering the manifest
  # rather than skipping the check keeps hash verification on what this host
  # actually uses.
  if (( no_canned )); then
    check_file="$(mktemp)"
    # shellcheck disable=SC2064  # expand $check_file now, not at trap time
    trap "rm -f '$check_file'" RETURN
    grep -E '  fidelity/golden/' "$manifest" >"$check_file" || true
  fi

  local failed=0

  # 1. Every recorded file present and byte-identical. sha256sum -c reports
  #    missing files and hash mismatches separately, both as failures.
  if ! ( cd "$here" && sha256sum --quiet --check "$check_file" ); then
    cat >&2 <<EOF
!! fixtures do not match $manifest (missing, or modified since capture).

   Recapture rather than patching individual files — canned and golden only
   agree when they come from the same pass:

     CONFIG_FILE=./broker-config.real.yaml ./regen-golden.sh
EOF
    failed=1
  fi

  # 2. No unrecorded fixture on disk. Catches a file copied in from an older
  #    capture, which hash-checking alone would never see.
  local strays
  strays="$(comm -13 <(manifest_paths) <(fixture_paths "${dirs[@]}") || true)"
  if [[ -n "$strays" ]]; then
    echo "!! fixture files not recorded in $manifest:" >&2
    while IFS= read -r f; do echo "     $f" >&2; done <<<"$strays"
    echo "   They are from a different capture (or hand-made). Recapture with ./regen-golden.sh." >&2
    failed=1
  fi

  (( failed )) && return 1

  # 3. Age — informational. Report it either way so a run's log always says
  #    how old the data behind it was.
  local captured_at captured_epoch age_s warn_s
  captured_at="$(sed -n 's/^# captured_at: //p' "$manifest")"
  captured_epoch="$(date -d "$captured_at" +%s 2>/dev/null || true)"
  if [[ -n "$captured_epoch" ]]; then
    age_s=$(( $(date +%s) - captured_epoch ))
    warn_s="$(age_seconds "${FIXTURE_AGE_WARN:-7d}")"
    printf '   fixtures captured %s (%dh ago) broker=%s vpn=%s\n' \
      "$captured_at" "$(( age_s / 3600 ))" \
      "$(sed -n 's/^# broker_alias: //p' "$manifest")" \
      "$(sed -n 's/^# vpn: //p' "$manifest")"
    if (( age_s > warn_s )); then
      echo "   WARNING: older than ${FIXTURE_AGE_WARN:-7d}. Still internally consistent, so the run proceeds," >&2
      echo "            but the broker it describes has moved on — recapture before trusting a regression." >&2
    fi
  fi
  return 0
}

case "${1:-}" in
  write) shift; cmd_write "$@" ;;
  check) shift; cmd_check "$@" ;;
  *) echo "usage: $0 {write|check [--no-canned]}" >&2; exit 2 ;;
esac
