#!/usr/bin/env bash
set -euo pipefail

# Troubleshooting: "address already in use" on WSL2
# -------------------------------------------------
# If `docker compose up` fails with:
#   failed to bind host port for 0.0.0.0:<port>: address already in use
# but `ss -tlnp` shows nothing on that port, Windows has likely reserved the
# range for Hyper-V / WSL. Docker on WSL2 inherits those reservations.
# Check the current Windows reservations from inside WSL:
#   netsh.exe interface ipv4 show excludedportrange protocol=tcp
# Either remap the host port in docker-compose.yml to one outside every
# excluded range, or move the Windows dynamic range (admin PowerShell):
#   netsh int ipv4 set dynamic tcp start=49152 num=10000

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [ ! -f "${HERE}/.env" ]; then
  echo "missing ${HERE}/.env (need ADMIN_PASSWORD and PSK)" >&2
  exit 1
fi

ADMIN_USER="admin"
ADMIN_PASS="$(grep '^ADMIN_PASSWORD=' "${HERE}/.env" | cut -d= -f2-)"

PORTS=(8080 8081 8082)
NAMES=(primary backup monitoring)

curl_semp() {
  local url="$1" body="$2"
  curl -sf -u "${ADMIN_USER}:${ADMIN_PASS}" \
    -X POST -H 'Content-Type: text/xml' \
    --data-binary "$body" "$url"
}

echo "==> docker compose up -d"
docker compose --project-directory "${HERE}" up -d

echo
echo "==> waiting for SEMP on all 3 brokers (up to 5 min each)"
for i in "${!PORTS[@]}"; do
  port="${PORTS[$i]}"
  name="${NAMES[$i]}"
  for attempt in $(seq 1 60); do
    status=$(curl -s -o /dev/null -w "%{http_code}" \
      -u "${ADMIN_USER}:${ADMIN_PASS}" \
      "http://localhost:${port}/SEMP/v2/config/about" || true)
    if [ "$status" = "200" ]; then
      echo "    ${name} (port ${port}): ready (${attempt} attempts)"
      break
    fi
    sleep 5
    if [ "$attempt" = "60" ]; then
      echo "    ${name} (port ${port}): NOT READY after 300s (last HTTP $status)" >&2
      exit 1
    fi
  done
done

echo
echo "==> asserting config-sync leader on primary"
echo "    -> table: router"
curl_semp "http://localhost:8080/SEMP" \
  '<rpc semp-version="soltr/10_25"><admin><config-sync><assert-leader><router/></assert-leader></config-sync></admin></rpc>'
echo
echo "    -> table: vpn 'default'"
curl_semp "http://localhost:8080/SEMP" \
  '<rpc semp-version="soltr/10_25"><admin><config-sync><assert-leader><vpn-name>default</vpn-name></assert-leader></config-sync></admin></rpc>'
echo

echo
echo "==> waiting 45s for matelink + spool sync to settle"
sleep 45

echo
echo "==> redundancy status"
for i in "${!PORTS[@]}"; do
  port="${PORTS[$i]}"
  name="${NAMES[$i]}"
  echo "--- ${name} ---"
  curl_semp "http://localhost:${port}/SEMP" \
    '<rpc semp-version="soltr/10_25"><show><redundancy><detail/></redundancy></show></rpc>' \
    | grep -E "<redundancy-status>|<active-standby-role>|<adb-link-up>|<adb-hello-up>|<last-failure-reason>|<ssl>" \
    | sed 's/^[[:space:]]*//'
done

echo
echo "==> config-sync database (primary)"
curl_semp "http://localhost:8080/SEMP" \
  '<rpc semp-version="soltr/10_25"><show><config-sync><database/></config-sync></show></rpc>' \
  | grep -E "<type>|<name>|<role>|<sync-state>|<ownership>" \
  | paste -d' ' - - - - -

echo
echo "==> done"
