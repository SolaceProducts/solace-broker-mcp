#!/usr/bin/env bash
# Copyright 2024-2026 Solace Corporation. All rights reserved.
#
# sanitize.sh — replace lab-appliance identifiers in captured perf fixtures.
#
# The perf harness captures SEMP responses from a real 3560 appliance. Those
# captures carry chassis/board/disk/blade serials, MAC addresses, WWPNs, lab
# IPs, and a non-GA build string. The repo is going public, so those values
# cannot land in git history.
#
# Drives from a literal value -> replacement table applied across both
# mock-semp/canned/ (what mock-semp serves) and fidelity/golden/ (what the
# exact-mode gate compares against). Substituting on the *value* rather than
# the surrounding field name keeps the two sides consistent by construction:
# the same serial in an XML attribute and in JSON is one entry, one edit.
#
# Idempotent: real values only exist before the first run; subsequent runs
# find nothing to change. Safe to re-invoke from regen-golden.sh on every
# recapture without diffing the world.
#
# Usage:
#   ./sanitize.sh                      # scrub the local fixtures in place
#
# Invoked automatically from ../../regen-golden.sh between the capture and the
# manifest write, so a fresh capture is scrubbed before anything reads it and
# the recorded hashes describe the scrubbed bytes.

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
canned_dir="$here"
golden_dir="$(cd "$here/../../fidelity/golden" && pwd)"
perf_dir="$(cd "$here/../.." && pwd)"

# Substitution table: real value on the left, replacement on the right.
# Applied in order, top to bottom. Each pair covers both XML and JSON —
# the same literal appears in different surrounding syntax across the two
# formats, and a value-level match hits both.
#
# Placeholders:
#   TESTSERIAL-*        — synthetic serials, obviously not machine-derived
#   00:00:5E:00:53:01/2 — RFC 7042 documentation MACs
#   0x2[01]00...        — WWPN/WWNN pairs with the low-nibble sharing preserved
#                          (real FC addressing convention; keeps the fixture
#                          visibly realistic to a reader who knows the format)
#   198.51.100.0/24     — TEST-NET-2 (RFC 5737 documentation range)
#   10.25.1.10          — plausible-looking GA-form build string
#
# The SFP serial replacement preserves the original 16-char width including
# trailing spaces — the field is space-padded on the wire and the width
# survives round-trips through the fixtures.
subs=(
  # Hardware identity — show_hardware_details.xml, also mirrored into
  # fidelity/golden/get-broker-status.json for chassis/disk/blade serials.
  "S009001344=TESTSERIAL-CHASSIS-01"
  "QSCP51300944=TESTSERIAL-BOARD-01"
  "PHWL701000HH120LGN=TESTSERIAL-DISK-01"
  "PHWL70100090120LGN=TESTSERIAL-DISK-02"
  "P004045330=TESTSERIAL-BLADE-13"
  "P004048935=TESTSERIAL-BLADE-14"
  "P004047778=TESTSERIAL-BLADE-16"
  "BFD1450B68835=TESTSERIAL-BLADE-17"
  "P1T11003216702260062=TESTSERIAL-FLASH-01"
  "P1T11003216702260013=TESTSERIAL-FLASH-02"
  "AD15303144Y     =TESTSERIAL-SFP-01 "
  "70:b3:d5:44:e1:ce=00:00:5E:00:53:01"
  "70:b3:d5:44:e1:cf=00:00:5E:00:53:02"
  "0x21000024ff5e8360=0x2100000000000001"
  "0x21000024ff5e8361=0x2100000000000002"
  "0x20000024ff5e8360=0x2000000000000001"
  "0x20000024ff5e8361=0x2000000000000002"

  # Network addresses — spool disk keys, queues page links/paging, capture
  # and run-script usage comments. Real lab IPs replaced with TEST-NET-2.
  "192.168.164.178=198.51.100.10"
  "192.168.129.78=198.51.100.20"
  "192.168.2.180=198.51.100.30"
  "192.168.2.194=198.51.100.31"

  # Build identifiers — non-GA main-branch build string appears in
  # semp-version attributes, version.description, current-load, NAB fw-ver,
  # and URL-encoded inside every cursorQuery on the queues pages. URL
  # encoding is a no-op for [A-Za-z0-9.] so one substitution hits both.
  # Load-history versions with +lo.NNN markers substituted to plain GA
  # forms so nothing in the file reads as an internal build number.
  "100.0main.0.7305=10.25.1.10"
  "soltr_10.4.1+lo.485=soltr_10.24.0.5"
  "soltr_10.8.1+lo.456=soltr_10.24.1.3"
  "2026-05-01T21:25:17-04:00=2026-03-15T12:00:00+00:00"
)

# Files touched — enumerated explicitly rather than globbed so a stray file
# under canned/ (e.g. a *.new during a partial recapture) doesn't get
# silently rewritten.
files=(
  "$canned_dir/show_version.xml"
  "$canned_dir/show_system.xml"
  "$canned_dir/show_memory.xml"
  "$canned_dir/show_message_spool.xml"
  "$canned_dir/show_hardware_details.xml"
  "$canned_dir/queues_page1.json"
  "$canned_dir/queues_page2.json"
  "$canned_dir/queues_page3.json"
  "$canned_dir/capture.sh"
  "$golden_dir/get-broker-status.json"
  "$golden_dir/list-queues.json"
  "$perf_dir/run-mcp.sh"
  "$perf_dir/run-loadgen.sh"
)

# Guard against a partial checkout — every file in the list must exist.
# A silent skip would leave a real value behind on a broken tree.
for f in "${files[@]}"; do
  if [[ ! -f "$f" ]]; then
    echo "sanitize: missing file: $f" >&2
    exit 2
  fi
done

changed=0
for pair in "${subs[@]}"; do
  # Split on the first '=' only — replacements may contain '=' safely.
  from="${pair%%=*}"
  to="${pair#*=}"
  if [[ -z "$from" ]]; then
    echo "sanitize: empty 'from' in table entry '$pair'" >&2
    exit 2
  fi

  # \Q...\E disables regex metacharacters in the search — dots, plus, and
  # pipe are literal. The replacement side goes through a variable, so
  # embedded $ or @ won't interpolate. Perl's -i edits in place; -0777
  # slurps each file whole (so multi-line matches would work — none of
  # the current entries span lines, but this keeps the door open).
  FROM="$from" TO="$to" perl -0777 -i -pe '
    my $f = quotemeta $ENV{FROM};
    my $t = $ENV{TO};
    s/$f/$t/g;
  ' "${files[@]}"
  changed=1
done

if [[ $changed -eq 1 ]]; then
  echo "sanitize: applied ${#subs[@]} substitutions across ${#files[@]} files"
fi
