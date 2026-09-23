#!/usr/bin/env bash
#
# Bring-up is copied from solace-local-infra/brokers/setup-oauth-brokers.sh
# (same image, shm/ulimits, 90s SEMP wait).
# Differences vs that script (Entra lab only):
#   - names mcp-entra-solace* (do not reuse solace / solace-b)
#   - host ports 28081/21943, 28082/21944, 28083/21945 so they do not steal infra 8081/1943
#   - third container for local-basic (no OAuth PATCH)
#   - no Keycloak docker network (JWKS is Microsoft HTTPS)
#   - PATCH issuer/JWKS/audience/group to the solacetest.com Entra tenant
#
# Brokers:
#   mcp-entra-solace    → 28081 (SEMP HTTP), 21943 (SEMP TLS)  prod-us
#   mcp-entra-solace-c  → 28082 (SEMP HTTP), 21944 (SEMP TLS)  local-basic
#   mcp-entra-solace-b  → 28083 (SEMP HTTP), 21945 (SEMP TLS)  test-us
#
# Do not run setup-oauth-brokers.sh on these containers — it writes Keycloak back.

set -euo pipefail

BROKER_IMAGE="solace/solace-pubsub-standard:latest"
TENANT="REDACTED"
ISSUER="https://login.microsoftonline.com/${TENANT}/v2.0"
JWKS_URL="https://login.microsoftonline.com/${TENANT}/discovery/v2.0/keys"
REQUIRED_SCOPE="solace.admin"
AUDIENCE="REDACTED"
ENTRA_GROUP_OBJECT_ID="REDACTED"

# broker-name  semp-host-port  smf-host-port  oauth|none
BROKERS=(
  "mcp-entra-solace    28081  21943  oauth"
  "mcp-entra-solace-c  28082  21944  none"
  "mcp-entra-solace-b  28083  21945  oauth"
)

if command -v docker >/dev/null 2>&1; then
  CONTAINER_CLI=docker
elif command -v podman >/dev/null 2>&1; then
  CONTAINER_CLI=podman
else
  echo "error: neither 'docker' nor 'podman' found on PATH" >&2
  exit 1
fi
echo "using container CLI: $CONTAINER_CLI"

# ensure_container <name> <semp-port> <smf-port>
# Idempotent: creates the container if missing, starts it if stopped.
ensure_container() {
  local name="$1" semp="$2" smf="$3"

  if "$CONTAINER_CLI" inspect "$name" >/dev/null 2>&1; then
    local status
    status=$("$CONTAINER_CLI" inspect "$name" --format '{{.State.Status}}')
    if [ "$status" = "running" ]; then
      echo "  [$name] already running"
    else
      echo "  [$name] starting existing container"
      "$CONTAINER_CLI" start "$name" >/dev/null
    fi
  else
    echo "  [$name] creating (SEMP $semp, SMF $smf)"
    "$CONTAINER_CLI" run -d --name "$name" \
      -p "${semp}:8080" -p "${smf}:1943" \
      --shm-size=1g --ulimit core=-1 --ulimit memlock=-1 --ulimit nofile=2448:42192 \
      -e username_admin_globalaccesslevel=admin \
      -e username_admin_password=admin \
      "$BROKER_IMAGE" >/dev/null
  fi
}

# wait_for_semp <semp-port>
wait_for_semp() {
  local port="$1"
  local i
  for i in $(seq 1 90); do
    if curl -sf -o /dev/null --max-time 2 \
         -u admin:admin "http://localhost:${port}/SEMP/v2/config/about" 2>/dev/null; then
      echo "  SEMP on port $port ready (${i}s)"
      return 0
    fi
    sleep 1
  done
  echo "  ERROR: SEMP on port $port did not become ready in 90s" >&2
  return 1
}

# upsert_profile <semp-port> <audience>
upsert_profile() {
  local port="$1" audience="$2"

  local status
  status=$(curl -s -u admin:admin \
    "http://localhost:${port}/SEMP/v2/config/oauthProfiles/keycloak_profile" \
    | python3 -c "import sys,json;print(json.load(sys.stdin).get('meta',{}).get('responseCode','?'))" 2>/dev/null || echo "?")

  if [ "$status" = "200" ]; then
    echo "  keycloak_profile already exists — patching to Entra shape"
  else
    echo "  creating keycloak_profile"
    curl -sf -u admin:admin -X POST \
      "http://localhost:${port}/SEMP/v2/config/oauthProfiles" \
      -H "Content-Type: application/json" \
      -d "{\"oauthProfileName\":\"keycloak_profile\",\"oauthRole\":\"resource-server\"}" >/dev/null
  fi

  curl -sf -u admin:admin -X PATCH \
    "http://localhost:${port}/SEMP/v2/config/oauthProfiles/keycloak_profile" \
    -H "Content-Type: application/json" \
    -d "$(cat <<JSON
{
  "oauthRole": "resource-server",
  "issuer": "$ISSUER",
  "resourceServerRequiredIssuer": "$ISSUER",
  "endpointJwks": "$JWKS_URL",
  "resourceServerRequiredAudience": "$audience",
  "resourceServerRequiredScope": "$REQUIRED_SCOPE",
  "resourceServerRequiredType": "JWT",
  "resourceServerValidateIssuerEnabled": true,
  "resourceServerValidateAudienceEnabled": true,
  "resourceServerValidateScopeEnabled": false,
  "resourceServerParseAccessTokenEnabled": true,
  "accessLevelGroupsClaimName": "groups",
  "usernameClaimName": "sub",
  "sempEnabled": true,
  "enabled": true
}
JSON
)" >/dev/null

  local i
  for i in 1 2 3 4 5; do
    if curl -s -u admin:admin \
         "http://localhost:${port}/SEMP/v2/config/oauthProfiles/keycloak_profile?select=resourceServerRequiredAudience" \
         | python3 -c "import sys,json;sys.exit(0 if json.load(sys.stdin)['data']['resourceServerRequiredAudience']=='$audience' else 1)" 2>/dev/null; then
      break
    fi
    sleep 1
    if [ "$i" = "5" ]; then
      echo "  ERROR: profile PATCH did not take effect within 5s" >&2
      return 1
    fi
  done
  echo "  profile patched (audience=$audience)"
}

upsert_group() {
  local port="$1" name="$2" level="$3" desc="$4"

  local status
  status=$(curl -s -u admin:admin \
    "http://localhost:${port}/SEMP/v2/config/oauthProfiles/keycloak_profile/accessLevelGroups/$name" \
    | python3 -c "import sys,json;print(json.load(sys.stdin).get('meta',{}).get('responseCode','?'))" 2>/dev/null || echo "?")

  local method url
  if [ "$status" = "200" ]; then
    method=PATCH
    url="http://localhost:${port}/SEMP/v2/config/oauthProfiles/keycloak_profile/accessLevelGroups/$name"
    echo "  patching group $name → $level"
  else
    method=POST
    url="http://localhost:${port}/SEMP/v2/config/oauthProfiles/keycloak_profile/accessLevelGroups"
    echo "  creating group $name → $level"
  fi

  local body
  if [ "$method" = "POST" ]; then
    body="{\"groupName\":\"$name\",\"globalAccessLevel\":\"$level\",\"msgVpnAccessLevel\":\"none\",\"description\":\"$desc\"}"
  else
    body="{\"globalAccessLevel\":\"$level\",\"msgVpnAccessLevel\":\"none\",\"description\":\"$desc\"}"
  fi

  curl -sf -u admin:admin -X "$method" "$url" \
    -H "Content-Type: application/json" -d "$body" >/dev/null
}

echo "==> ensuring broker containers exist and are running"
for row in "${BROKERS[@]}"; do
  read -r name semp smf _mode <<<"$row"
  ensure_container "$name" "$semp" "$smf"
done

echo
echo "==> waiting for SEMP to come up"
for row in "${BROKERS[@]}"; do
  read -r name semp _smf _mode <<<"$row"
  echo "  [$name]"
  wait_for_semp "$semp"
done

echo
echo "==> configuring Entra OAuth profiles + access-level group"
for row in "${BROKERS[@]}"; do
  read -r name semp _smf mode <<<"$row"
  if [ "$mode" != "oauth" ]; then
    echo "  [$name] skip OAuth PATCH (basic auth only)"
    continue
  fi
  echo "  [$name]"
  upsert_profile "$semp" "$AUDIENCE"
  upsert_group "$semp" "$ENTRA_GROUP_OBJECT_ID" "admin" \
    "Maps Entra security group Object ID to broker admin access"
done

echo
echo "==> verifying"
for row in "${BROKERS[@]}"; do
  read -r name semp _smf mode <<<"$row"
  if [ "$mode" != "oauth" ]; then
    continue
  fi
  actual=$(curl -s -u admin:admin \
    "http://localhost:${semp}/SEMP/v2/config/oauthProfiles/keycloak_profile?select=resourceServerRequiredAudience" \
    | python3 -c "import sys,json;print(json.load(sys.stdin)['data']['resourceServerRequiredAudience'])")
  if [ "$actual" = "$AUDIENCE" ]; then
    echo "  [$name] audience=$actual ✓"
  else
    echo "  [$name] audience=$actual (want $AUDIENCE) ✗"
    exit 1
  fi
done

echo
echo "Done. Host ports are the Entra lab band (not infra 8081/1943):"
echo "  mcp-entra-solace    http://localhost:28081  https://localhost:21943  (prod-us)"
echo "  mcp-entra-solace-c  http://localhost:28082  https://localhost:21944  (local-basic)"
echo "  mcp-entra-solace-b  http://localhost:28083  https://localhost:21945  (test-us)"
echo "Teardown: make entra-down"
echo "Do not run solace-local-infra setup-oauth-brokers.sh on these names."
