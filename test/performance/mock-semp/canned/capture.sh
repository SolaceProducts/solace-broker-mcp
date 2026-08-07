#!/usr/bin/env bash
# Copyright 2024-2026 Solace Corporation. All rights reserved.
#
# capture.sh — record real broker responses for the mock's canned/ dir.
#
# Fires the exact SEMP requests MCP would send for get-broker-status and
# list-queues, and saves the raw response bodies alongside this script.
# Overwrites any existing files. Run once, commit or gitignore, and the
# mock replays from these files.
#
# Usage:
#   BROKER_URL=http://198.51.100.20:80 \
#   BROKER_USERNAME=... \
#   BROKER_PASSWORD=... \
#   MSG_VPN=default \
#   ./capture.sh
#
# Exits non-zero on any HTTP failure — a missing response file is a build
# mistake, not a runtime one.
set -euo pipefail

: "${BROKER_URL:?BROKER_URL is required (e.g. http://198.51.100.20:80)}"
: "${BROKER_USERNAME:?BROKER_USERNAME is required}"
: "${BROKER_PASSWORD:?BROKER_PASSWORD is required}"
MSG_VPN="${MSG_VPN:-default}"

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

echo "--- SEMPv2: list-queues (page 1 + follow pagination) ---"
# select= matches the tools.yaml declaration for list-queues, comma-joined
# (SEMP wire format — verified in internal/semp/sempv2/client_test.go).
#
# bindCount is a private-schema attribute (see
# semp-v2-swagger-private-monitor-extended.json). It lives at
# /SEMP/v2/__private_monitor__/... — that's the endpoint MCP uses. The
# public /SEMP/v2/monitor/... path rejects bindCount as "not a valid
# attribute". We hit the private path here so the captured body carries
# bindCount and matches what MCP sees end-to-end.
SELECT="accessType,bindCount,egressEnabled,ingressEnabled,lowPriorityMsgCongestionState,maxMsgSpoolUsage,msgSpoolUsage,msgVpnName,queueName,rxMsgRate,spooledMsgCount,txMsgRate,txUnackedMsgCount"
SEMP_BASE="/SEMP/v2/__private_monitor__"

page=1
next="$SEMP_BASE/msgVpns/$MSG_VPN/queues?count=100&select=$SELECT"
while [[ -n "$next" ]]; do
  out="queues_page${page}.json"
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
    echo "aborting: >20 pages, likely a bug" >&2
    exit 1
  fi
done

echo "--- done ---"
ls -la *.xml *.json 2>/dev/null
