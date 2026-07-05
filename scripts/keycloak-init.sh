#!/usr/bin/env bash
# Idempotent post-startup init for the local Keycloak dev container.
#
# Realm import creates users and clients but strips password credentials
# and doesn't touch realm-level security headers.  This script fills both
# gaps via the admin API:
#
#   1. Disable HSTS on mcp-test-realm  — Keycloak's default Strict-Transport-Security
#      header causes browsers to auto-upgrade the plain-HTTP OAuth callback to HTTPS,
#      which breaks the flow (see hop1-manual-test-setup.md Problem #4).
#   2. Reset passwords for known test users so login actually works
#      (realm exports do not carry credential hashes — Problem #5).
#
# Every action is a PUT — safe to re-run.

set -euo pipefail

KC_HTTPS="https://localhost:18443"
REALM="mcp-test-realm"
CA_CERT="$(cd "$(dirname "$0")/.." && pwd)/.local/certs/keycloak/keycloak.crt"

# Users to (re)set — same password for all in local dev.
USERS=("test-admin-user" "test-readonly-user")
DEV_PASSWORD="password"

curl_kc() { curl -sS --cacert "$CA_CERT" "$@"; }

wait_for_oidc() {
  local url="$KC_HTTPS/realms/$REALM/.well-known/openid-configuration"
  local attempts=30
  for i in $(seq 1 $attempts); do
    if curl_kc -o /dev/null -w '%{http_code}' "$url" | grep -q '^200$'; then
      echo "  Keycloak OIDC endpoint ready"
      return 0
    fi
    sleep 2
  done
  echo "✗ Keycloak did not become ready within $((attempts * 2))s" >&2
  exit 1
}

get_admin_token() {
  curl_kc -X POST "$KC_HTTPS/realms/master/protocol/openid-connect/token" \
    -d "client_id=admin-cli" \
    -d "username=admin" \
    -d "password=admin" \
    -d "grant_type=password" \
    | python3 -c 'import json,sys;print(json.load(sys.stdin)["access_token"])'
}

disable_hsts() {
  local token="$1"
  curl_kc -X PUT "$KC_HTTPS/admin/realms/$REALM" \
    -H "Authorization: Bearer $token" \
    -H "Content-Type: application/json" \
    -d '{
      "browserSecurityHeaders": {
        "contentSecurityPolicyReportOnly": "",
        "xContentTypeOptions": "nosniff",
        "referrerPolicy": "no-referrer",
        "xRobotsTag": "none",
        "xFrameOptions": "SAMEORIGIN",
        "contentSecurityPolicy": "frame-src '\''self'\''; frame-ancestors '\''self'\''; object-src '\''none'\'';",
        "strictTransportSecurity": ""
      }
    }' \
    -o /dev/null -w '  HSTS disable: HTTP %{http_code}\n'
}

reset_password() {
  local token="$1" username="$2"
  local user_id
  user_id=$(curl_kc "$KC_HTTPS/admin/realms/$REALM/users?username=$username&exact=true" \
    -H "Authorization: Bearer $token" \
    | python3 -c 'import json,sys;
users=json.load(sys.stdin);
print(users[0]["id"] if users else "", end="")')

  if [[ -z "$user_id" ]]; then
    echo "  ✗ user not found: $username" >&2
    return 1
  fi

  curl_kc -X PUT "$KC_HTTPS/admin/realms/$REALM/users/$user_id/reset-password" \
    -H "Authorization: Bearer $token" \
    -H "Content-Type: application/json" \
    -d "{\"type\":\"password\",\"value\":\"$DEV_PASSWORD\",\"temporary\":false}" \
    -o /dev/null -w "  password reset [$username]: HTTP %{http_code}\n"
}

echo "→ waiting for Keycloak..."
wait_for_oidc

echo "→ fetching admin token..."
TOKEN=$(get_admin_token)
[[ -n "$TOKEN" ]] || { echo "✗ failed to obtain admin token" >&2; exit 1; }

echo "→ disabling HSTS on realm '$REALM'..."
disable_hsts "$TOKEN"

echo "→ resetting user passwords..."
for u in "${USERS[@]}"; do
  reset_password "$TOKEN" "$u"
done

echo "✓ keycloak init complete"
