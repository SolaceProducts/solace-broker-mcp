#!/usr/bin/env bash
# Copyright 2024-2026 Solace Corporation. All rights reserved.
#
# sanitize.sh — replace lab-appliance identifiers in captured perf fixtures.
#
# The perf harness captures SEMP responses from a real appliance. Those
# captures carry chassis/board/disk/blade serials, MAC addresses, WWPNs, lab
# IPs, and possibly a non-GA build string. The repo is public, so none of that
# can reach a ticket, a run report, or git.
#
# Two mechanisms, because the two classes of identifier are not equally
# detectable:
#
#   1. A substitution table (sanitize.local.tsv, gitignored) maps literal
#      values to placeholders. Serials have no recognizable shape, so nothing
#      short of naming them works — and naming them in a tracked file would
#      publish the very strings the table exists to remove. The table is
#      per-checkout: yours describes your appliance.
#
#   2. A residual scan over the scrubbed bytes fails the capture when a
#      mechanically-recognizable identifier survives — a lab MAC, a
#      192.168/172.16 address, an FC WWPN, a non-GA build marker. This is what
#      makes the harness safe on an appliance the table was not written for:
#      previously that case scrubbed nothing and said nothing.
#
#      The scan cannot see serials. If your appliance is not the one
#      sanitize.local.tsv describes, write a table for it — a clean scan is
#      not proof the fixtures are clean.
#
# Substituting on the *value* rather than the surrounding field name keeps
# canned/ and golden/ consistent by construction: the same serial in an XML
# attribute and in JSON is one entry, one edit. An inconsistent substitution
# would fail the exact-mode fidelity gate.
#
# Idempotent: real values only exist before the first run; later runs find
# nothing to change. Safe to re-invoke from regen-golden.sh on every recapture.
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
table="$here/sanitize.local.tsv"

# --- files -------------------------------------------------------------------
#
# The SEMPv1 captures, the per-RDP captures and the goldens are a fixed set.
# The paginated collections are not: capture.sh follows pagination, so the page
# count tracks how many queues or RDPs the broker holds. Globbing them (rather
# than naming page 1..3, as this script once did) is what keeps a broker with
# two pages from aborting the capture and a broker with four from leaving page 4
# unscrubbed.
#
# Every capture must appear here. A file left off the list is not merely
# unscrubbed — it is also invisible to the residual scan below, so the run
# would report "clean" while carrying whatever the omitted file carries.
#
# Only capture output is listed. This script must never rewrite tracked source
# files: a capture that edits scripts in the working tree is a surprise, and a
# bad table entry would silently corrupt them.
files=(
  "$canned_dir/show_version.xml"
  "$canned_dir/show_system.xml"
  "$canned_dir/show_memory.xml"
  "$canned_dir/show_message_spool.xml"
  "$canned_dir/show_hardware_details.xml"
  "$canned_dir/rdp_object.json"
  "$canned_dir/rdp_queue_bindings.json"
  "$canned_dir/rdp_rest_consumers.json"
  "$golden_dir/get-broker-status.json"
  "$golden_dir/list-queues.json"
  "$golden_dir/list-rdps.json"
  "$golden_dir/list-rdps-paged.json"
  "$golden_dir/get-rdp-status.json"
)

# Paginated collections, one glob each. An empty glob means the capture never
# finished — the mock cannot serve that tool at all, so stop here rather than
# scrubbing a partial tree.
for prefix in queues_page rdps_page; do
  shopt -s nullglob
  pages=("$canned_dir/$prefix"*.json)
  shopt -u nullglob
  if (( ${#pages[@]} == 0 )); then
    echo "sanitize: no ${prefix}*.json in $canned_dir — capture is incomplete" >&2
    exit 2
  fi
  files+=("${pages[@]}")
done

# Guard against a partial capture — every named file must exist. A silent skip
# would leave a real value behind on a broken tree.
for f in "${files[@]}"; do
  if [[ ! -f "$f" ]]; then
    echo "sanitize: missing file: $f" >&2
    exit 2
  fi
done

# --- substitution table ------------------------------------------------------
#
# Format: one "value<TAB>replacement" pair per line, "#" comments and blank
# lines ignored. See sanitize.local.tsv.example for the placeholder
# conventions (RFC 5737 TEST-NET-2 addresses, RFC 7042 documentation MACs,
# TESTSERIAL-* for serials) and why they were chosen.
subs=()
if [[ -f "$table" ]]; then
  while IFS=$'\t' read -r from to; do
    [[ -z "${from// }" || "${from:0:1}" == "#" ]] && continue
    if [[ -z "$to" ]]; then
      echo "sanitize: $table: no replacement for '$from' (fields are TAB-separated)" >&2
      exit 2
    fi
    subs+=("$from"$'\t'"$to")
  done < "$table"
fi

if (( ${#subs[@]} == 0 )); then
  echo "sanitize: no substitution table at $table — serials and build strings will NOT be scrubbed." >&2
  echo "          Copy sanitize.local.tsv.example and fill in your appliance's values." >&2
  echo "          The residual scan below still runs, but it cannot see serials." >&2
else
  for pair in "${subs[@]}"; do
    from="${pair%%$'\t'*}"
    to="${pair#*$'\t'}"
    # quotemeta disables regex metacharacters in the search — dots, plus, and
    # pipe are literal. The replacement goes through a variable, so embedded
    # $ or @ won't interpolate. -i edits in place; -0777 slurps each file
    # whole, so a multi-line value would work too.
    FROM="$from" TO="$to" perl -0777 -i -pe '
      my $f = quotemeta $ENV{FROM};
      my $t = $ENV{TO};
      s/$f/$t/g;
    ' "${files[@]}"
  done
  echo "sanitize: applied ${#subs[@]} substitutions across ${#files[@]} files"
fi

# --- residual scan -----------------------------------------------------------
#
# Runs on the scrubbed bytes. Each pattern covers a class of identifier with a
# shape distinctive enough to match without false positives:
#
#   mac    any MAC except the RFC 7042 documentation range 00:00:5E:00:53:xx
#   ip     192.168/16 and 172.16/12 only. 10/8 is deliberately excluded:
#          Solace version strings are 10.x.y.z and would match an IPv4 regex,
#          so scanning that range would fail every capture on its own version
#          attribute. A lab on 10/8 needs a table entry — noted in the README.
#   wwpn   FC WWPN/WWNN (0x21.../0x20...) other than the doc placeholders
#   build  non-GA build markers: the "+lo.NNN" suffix and the "NNNmain.N"
#          main-branch form. GA versions never carry either.
#
# The RDP captures carry three fields that can name a real endpoint: a REST
# consumer's remoteHost/remotePort and a queue binding's postRequestTarget. The
# scan below only recognizes 192.168/16 and 172.16/12 addresses, so an RDP
# pointed at a real internal service by hostname passes it. Rather than ask the
# reader to know that, report_rdp_endpoints prints those values at the end of
# every run: the one thing a clean scan cannot vouch for is put in front of
# whoever ran the scrub. On the capture this harness was built against they were
# "example.com", 80 and "/", which is why no table entries exist for them.
#
# A hit is fatal: regen-golden.sh runs under set -e, so the capture stops
# before the manifest is written and before anything replays the fixture.
# scan reports matches of $pattern that do not also match $allow. grep -E has
# no lookahead, so the documentation placeholders this script substitutes in
# are filtered out after matching rather than excluded in the pattern itself.
# Pass '$^' as $allow to allow nothing.
scan() {
  local label=$1 pattern=$2 allow=$3
  local hits
  hits="$(grep -oaEn "$pattern" "${files[@]}" 2>/dev/null | grep -vaiE "$allow" || true)"
  [[ -z "$hits" ]] && return 0
  echo "!! sanitize: unscrubbed $label in the captured fixtures:" >&2
  # Report relative to the fixture dirs; the absolute paths are noise here.
  sed -e "s|$canned_dir/|canned/|" -e "s|$golden_dir/|golden/|" <<<"$hits" | sort -u | head -20 >&2
  return 1
}

failed=0
scan "MAC address" \
  '([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}' \
  '00:00:5e:00:53:' || failed=1
scan "lab IP address" \
  '(192\.168|172\.(1[6-9]|2[0-9]|3[01]))\.[0-9]{1,3}\.[0-9]{1,3}' \
  '$^' || failed=1
scan "FC WWPN/WWNN" \
  '0x2[01]00[0-9a-fA-F]{12}' \
  '0x2[01]000{11}[0-9a-f]' || failed=1
scan "non-GA build marker" \
  '(\+lo\.[0-9]+|[0-9]+main\.[0-9]+)' \
  '$^' || failed=1

if (( failed )); then
  cat >&2 <<EOF

   These are captures from your appliance and must not leave this machine.
   Add each value to $table (value<TAB>replacement), then re-run:

     ./sanitize.sh && ../../fixtures-manifest.sh write

   sanitize.local.tsv.example lists the placeholder conventions.
EOF
  exit 1
fi

echo "sanitize: residual scan clean (MACs, lab IPs, WWPNs, build markers)"
echo "sanitize: note — serials have no detectable shape; the scan cannot vouch for them."

# report_rdp_endpoints prints the RDP endpoint fields the scan cannot judge, so
# the human check does not depend on anyone remembering it exists. Reported, not
# gated: "example.com" and a real internal hostname are indistinguishable by
# shape, and only a reader can tell which one is on screen.
report_rdp_endpoints() {
  local rdp_files=() f
  for f in "$canned_dir/rdp_rest_consumers.json" "$canned_dir/rdp_queue_bindings.json" \
           "$golden_dir/get-rdp-status.json"; do
    [[ -f "$f" ]] && rdp_files+=("$f")
  done
  (( ${#rdp_files[@]} )) || return 0

  local values
  # -h suppresses the filename prefix: postRequestTarget values contain
  # slashes, so stripping a prefix after the fact would mangle them.
  values="$(grep -ohaE '"(remoteHost|postRequestTarget)":"[^"]*"|"remotePort":[0-9]+' "${rdp_files[@]}" 2>/dev/null |
    sort -u || true)"
  [[ -z "$values" ]] && return 0

  echo "sanitize: RDP endpoints in the capture — confirm none names a real internal service:"
  while IFS= read -r v; do echo "            $v"; done <<<"$values"
}
report_rdp_endpoints
