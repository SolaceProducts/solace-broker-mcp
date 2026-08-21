#!/usr/bin/env bash
# Copyright 2024-2026 Solace Corporation. All rights reserved.
#
# capture.sh — record real broker responses for the mock's canned/ dir.
#
# Fires the exact SEMP requests MCP would send for get-broker-status,
# list-queues, list-rdps and get-rdp-status, and saves the raw response bodies
# alongside this script. Overwrites any existing files, which are gitignored —
# see README.md in this directory. mock-semp replays them straight from disk.
#
# Prefer ../../regen-golden.sh: it drives this script AND the golden capture in
# one pass, so the two fixture sets describe the same instant. Running this
# alone leaves the goldens behind and fails the exact-mode fidelity gate.
#
# Usage:
#   BROKER_URL=http://198.51.100.20:80 \
#   BROKER_USERNAME=... \
#   BROKER_PASSWORD=... \
#   MSG_VPN=default \
#   RDP_NAME=rdp_1 \
#   ./capture.sh
#
# RDP_NAME pins the single RDP that get-rdp-status is replayed for. mock-semp
# reads the pinned name back out of the captured object rather than taking a
# flag, so the two cannot disagree; a request for any other RDP misses.
#
# Exits non-zero on any HTTP failure — a missing response file is a build
# mistake, not a runtime one.
set -euo pipefail

: "${BROKER_URL:?BROKER_URL is required (e.g. http://198.51.100.20:80)}"
: "${BROKER_USERNAME:?BROKER_USERNAME is required}"
: "${BROKER_PASSWORD:?BROKER_PASSWORD is required}"
MSG_VPN="${MSG_VPN:-default}"
RDP_NAME="${RDP_NAME:-rdp_1}"

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$DIR"

# curl_esc quotes a value for curl's config-file syntax. Backslash and
# double-quote are the only characters interpreted inside a "quoted" value
# (see curl(1) "Config File"); everything else passes through literally.
curl_esc() { printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'; }

# Build the `user = "..."` stanza once and pipe it to each curl call via
# --config on stdin. -u puts credentials in argv where any user on a shared
# host can read them from /proc/<pid>/cmdline; a here-string keeps the
# password in an unreadable pipe.
_curl_user_cfg="$(printf 'user = "%s:%s"\n' \
  "$(curl_esc "$BROKER_USERNAME")" \
  "$(curl_esc "$BROKER_PASSWORD")")"

curl_semp() {
  local out="$1" body="$2"
  # -f: fail on 4xx/5xx so the script aborts loudly
  # -sS: quiet but show errors
  # --config -: read auth options from stdin (see _curl_user_cfg above)
  curl -fsS --config - \
    -H "Content-Type: application/xml" \
    --data-raw "$body" \
    "$BROKER_URL/SEMP" \
    -o "$out" <<<"$_curl_user_cfg"
  echo "wrote $out ($(wc -c < "$out") bytes)"
}

curl_get() {
  local out="$1" ref="$2" url
  # $ref may be an absolute URL (broker often returns absolute nextPageUri
  # whose scheme/host/port don't line up with $BROKER_URL) or a root-relative
  # path. Blindly concatenating "$BROKER_URL$ref" corrupts the URL in the
  # absolute case ("https://host:943https://host:943/..."), which curl
  # rejects with "Port number was not a decimal number".
  case "$ref" in
    http://*|https://*) url="$ref" ;;
    /*)                 url="$BROKER_URL$ref" ;;
    *)                  url="$BROKER_URL/$ref" ;;
  esac
  curl -fsS --config - \
    "$url" \
    -o "$out" <<<"$_curl_user_cfg"
  echo "wrote $out ($(wc -c < "$out") bytes)"
}

echo "--- SEMPv1: get-broker-status inputs ---"
curl_semp show_version.xml       '<rpc><show><version/></show></rpc>'
curl_semp show_system.xml        '<rpc><show><system/></show></rpc>'
curl_semp show_memory.xml        '<rpc><show><memory/></show></rpc>'
curl_semp show_message_spool.xml '<rpc><show><message-spool><detail/></message-spool></show></rpc>'
# The appliance-only fifth call. get-broker-status fires it when show-version
# identifies the broker as an appliance (internal/tools/sempv1/brokerstatus:
# hardwareXML), and mock-semp registers its rule unconditionally, reading the
# file at startup — so without this line a fresh capture cannot start the mock
# at all. On a software broker the broker answers with a SEMP failure rather
# than an HTTP error, which is captured and simply never requested.
curl_semp show_hardware_details.xml '<rpc><show><hardware><details/></hardware></show></rpc>'

# All SEMPv2 captures use the private monitor endpoint, because that is what
# MCP uses: the monitor spec's basePath is /SEMP/v2/__private_monitor__, and
# list-queues needs it for bindCount, a private-schema attribute the public
# /SEMP/v2/monitor/... path rejects as "not a valid attribute". Capturing from
# the private path keeps the fixture byte-comparable with what MCP sees
# end-to-end.
SEMP_BASE="/SEMP/v2/__private_monitor__/msgVpns/$MSG_VPN"

# capture_pages follows a collection's pagination chain into
# <prefix><N>.json, one file per page.
#
# Stale pages are deleted first. A previous capture with more pages than this
# one would otherwise leave its tail behind, and mock-semp chains pages by the
# cursor each one advertises — so the leftover would be recorded in the
# manifest and served as part of a chain it never belonged to.
capture_pages() {
  local prefix="$1" next="$2" page=1
  rm -f "${prefix}"[0-9]*.json
  while [[ -n "$next" ]]; do
    local out="${prefix}${page}.json"
    curl_get "$out" "$next"
    # Extract nextPageUri from meta.paging (may be absent on last page).
    # jq is preferred; fall back to grep for environments without it.
    if command -v jq >/dev/null 2>&1; then
      next=$(jq -r '.meta.paging.nextPageUri // ""' "$out")
    else
      next=$(grep -oE '"nextPageUri"[[:space:]]*:[[:space:]]*"[^"]*"' "$out" | sed -E 's/.*"([^"]+)"$/\1/' || true)
    fi
    # SEMPv2's nextPageUri may be absolute or root-relative; curl_get handles both.
    page=$((page + 1))
    if [[ $page -gt 20 ]]; then
      echo "aborting: >20 pages of $prefix, likely a bug" >&2
      exit 1
    fi
  done
}

# Every select= below is the comma-joined field list from that tool's step in
# internal/composite/definitions/tools.yaml (SEMP wire format — verified in
# internal/semp/sempv2/client_test.go). A field captured that the tool does not
# select, or the reverse, breaks the exact-mode gate.
echo "--- SEMPv2: list-queues (page 1 + follow pagination) ---"
QUEUES_SELECT="accessType,bindCount,egressEnabled,ingressEnabled,lowPriorityMsgCongestionState,maxMsgSpoolUsage,msgSpoolUsage,msgVpnName,queueName,rxMsgRate,spooledMsgCount,txMsgRate,txUnackedMsgCount"
capture_pages queues_page "$SEMP_BASE/queues?count=100&select=$QUEUES_SELECT"

echo "--- SEMPv2: list-rdps (page 1 + follow pagination) ---"
RDPS_SELECT="clientName,enabled,lastFailureReason,lastFailureTime,msgVpnName,restDeliveryPointName,up"
capture_pages rdps_page "$SEMP_BASE/restDeliveryPoints?count=100&select=$RDPS_SELECT"

echo "--- SEMPv2: get-rdp-status for RDP '$RDP_NAME' (3 requests) ---"
RDP_SELECT="clientName,clientProfileName,enabled,lastFailureReason,lastFailureTime,msgVpnName,restDeliveryPointName,timeConnectionsBlocked,up"
BINDINGS_SELECT="lastFailureReason,lastFailureTime,postRequestTarget,queueBindingName,restDeliveryPointName,up,uptime"
CONSUMERS_SELECT="authenticationScheme,enabled,httpRequestOutstandingTxMsgCount,httpRequestTimedOutTxMsgCount,httpRequestTxMsgCount,httpResponseErrorRxMsgCount,httpResponseSuccessRxMsgCount,lastConnectionFailureReason,lastConnectionFailureTime,lastFailureReason,lastFailureTime,outgoingConnectionCount,remoteHost,remotePort,restConsumerName,restDeliveryPointName,up"

# get-rdp-status asks for count=100 on both sub-collections. One page each is
# all this captures; an RDP with more than 100 queue bindings or REST consumers
# would need page files, and would announce itself as a mock miss rather than
# quietly answering with a partial list.
rdp_path="$SEMP_BASE/restDeliveryPoints/$RDP_NAME"
curl_get rdp_object.json         "$rdp_path?select=$RDP_SELECT"
curl_get rdp_queue_bindings.json "$rdp_path/queueBindings?count=100&select=$BINDINGS_SELECT"
curl_get rdp_rest_consumers.json "$rdp_path/restConsumers?count=100&select=$CONSUMERS_SELECT"

echo "--- done ---"
ls -la *.xml *.json 2>/dev/null
