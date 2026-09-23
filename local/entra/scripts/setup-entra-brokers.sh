#!/usr/bin/env bash
#
# Bring up three Solace brokers for the laptop Entra lab. Idempotent.
#
# Distinct names AND host ports so they can sit beside solace-local-infra
# (that stack already owns 8081/1943 and 8083/1945):
#   mcp-entra-solace    28081/21943  Entra OAuth  (prod-us)
#   mcp-entra-solace-c  28082/21944  basic auth only (local-basic)
#   mcp-entra-solace-b  28083/21945  Entra OAuth  (test-us)
#
# Do NOT run solace-local-infra/brokers/setup-oauth-brokers.sh on these
# containers — that script PATCHes Keycloak issuer/JWKS and joins the
# Keycloak docker network. Brokers here need outbound HTTPS to Microsoft
# for JWKS; they must not join that network.

set -euo pipefail

BROKER_IMAGE="solace/solace-pubsub-standard:latest"

# Entra tenant (solacetest.com). Audience is the test-broker-us Application
# (client) ID, not the Application ID URI.
TENANT="REDACTED"
ISSUER="https://login.microsoftonline.com/${TENANT}/v2.0"
JWKS_URL="https://login.microsoftonline.com/${TENANT}/discovery/v2.0/keys"
AUDIENCE="REDACTED"
REQUIRED_SCOPE="solace.admin"
# OAuth profile name kept as keycloak_profile (prototype / existing YAML).
PROFILE_NAME="keycloak_profile"
# Entra security group Object ID — not display name solace-bms-admins,
# not Keycloak group name solace-admins.
ENTRA_GROUP_OBJECT_ID="REDACTED"

# name  semp-host-port  smf-host-port  oauth|none
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

if ! command -v python3 >/dev/null 2>&1; then
  echo "error: python3 is required to parse SEMP JSON" >&2
  exit 1
fi
if ! command -v curl >/dev/null 2>&1; then
  echo "error: curl is required" >&2
  exit 1
fi

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
      --shm-size=1g --ulimit core=-1 --ulimit memlock=-1 --ulimit nofile=1048576:1048576 \
      -e username_admin_globalaccesslevel=admin \
      -e username_admin_password=admin \
      "$BROKER_IMAGE" >/dev/null
  fi
}

# wait_for_semp <semp-port>
wait_for_semp() {
  local port="$1"
  local name="$2"
  local i
  for i in $(seq 1 90); do
    if [ -n "$name" ]; then
      local status
      status=$("$CONTAINER_CLI" inspect "$name" --format '{{.State.Status}}' 2>/dev/null || echo missing)
      if [ "$status" != "running" ]; then
        echo "  ERROR: $name is $status (not waiting 90s). Last logs:" >&2
        "$CONTAINER_CLI" logs --tail 20 "$name" >&2 || true
        return 1
      fi
    fi
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

# upsert_profile <semp-port>
# Creates keycloak_profile if absent, PATCHes Entra values every run.
upsert_profile() {
  local port="$1"

  local status
  status=$(curl -s -u admin:admin \
    "http://localhost:${port}/SEMP/v2/config/oauthProfiles/${PROFILE_NAME}" \
    | python3 -c "import sys,json;print(json.load(sys.stdin).get('meta',{}).get('responseCode','?'))" 2>/dev/null || echo "?")

  if [ "$status" = "200" ]; then
    echo "  ${PROFILE_NAME} already exists — patching to Entra shape"
  else
    echo "  creating ${PROFILE_NAME}"
    curl -sf -u admin:admin -X POST \
      "http://localhost:${port}/SEMP/v2/config/oauthProfiles" \
      -H "Content-Type: application/json" \
      -d "{\"oauthProfileName\":\"${PROFILE_NAME}\",\"oauthRole\":\"resource-server\"}" >/dev/null
  fi

  curl -sf -u admin:admin -X PATCH \
    "http://localhost:${port}/SEMP/v2/config/oauthProfiles/${PROFILE_NAME}" \
    -H "Content-Type: application/json" \
    -d "$(cat <<JSON
{
  "oauthRole": "resource-server",
  "issuer": "$ISSUER",
  "resourceServerRequiredIssuer": "$ISSUER",
  "endpointJwks": "$JWKS_URL",
  "resourceServerRequiredAudience": "$AUDIENCE",
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
         "http://localhost:${port}/SEMP/v2/config/oauthProfiles/${PROFILE_NAME}?select=issuer,resourceServerRequiredAudience" \
         | python3 -c "import sys,json
d=json.load(sys.stdin)['data']
sys.exit(0 if d.get('issuer')=='$ISSUER' and d.get('resourceServerRequiredAudience')=='$AUDIENCE' else 1)" 2>/dev/null; then
      break
    fi
    sleep 1
    if [ "$i" = "5" ]; then
      echo "  ERROR: Entra profile PATCH did not take effect within 5s" >&2
      return 1
    fi
  done
  echo "  profile patched (issuer+audience)"
}

# upsert_group <semp-port> <group-name> <global-access-level> <description>
upsert_group() {
  local port="$1" name="$2" level="$3" desc="$4"

  local status
  status=$(curl -s -u admin:admin \
    "http://localhost:${port}/SEMP/v2/config/oauthProfiles/${PROFILE_NAME}/accessLevelGroups/$name" \
    | python3 -c "import sys,json;print(json.load(sys.stdin).get('meta',{}).get('responseCode','?'))" 2>/dev/null || echo "?")

  local method url
  if [ "$status" = "200" ]; then
    method=PATCH
    url="http://localhost:${port}/SEMP/v2/config/oauthProfiles/${PROFILE_NAME}/accessLevelGroups/$name"
    echo "  patching group $name → $level"
  else
    method=POST
    url="http://localhost:${port}/SEMP/v2/config/oauthProfiles/${PROFILE_NAME}/accessLevelGroups"
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

verify_entra_profile() {
  local name="$1" port="$2"
  local actual
  actual=$(curl -s -u admin:admin \
    "http://localhost:${port}/SEMP/v2/config/oauthProfiles/${PROFILE_NAME}?select=issuer,resourceServerRequiredAudience" \
    | python3 -c "import sys,json
d=json.load(sys.stdin)['data']
print(d.get('issuer',''))
print(d.get('resourceServerRequiredAudience',''))")
  local got_issuer got_aud
  got_issuer=$(printf '%s\n' "$actual" | sed -n '1p')
  got_aud=$(printf '%s\n' "$actual" | sed -n '2p')
  if [ "$got_issuer" = "$ISSUER" ] && [ "$got_aud" = "$AUDIENCE" ]; then
    echo "  [$name] issuer+audience ✓"
  else
    echo "  [$name] issuer=$got_issuer (want $ISSUER)" >&2
    echo "  [$name] audience=$got_aud (want $AUDIENCE) ✗" >&2
    exit 1
  fi
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
  wait_for_semp "$semp" "$name"
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
  upsert_profile "$semp"
  upsert_group "$semp" "$ENTRA_GROUP_OBJECT_ID" "admin" \
    "Maps Entra security group Object ID to broker admin access"
done

echo
echo "==> verifying Entra issuer+audience"
for row in "${BROKERS[@]}"; do
  read -r name semp _smf mode <<<"$row"
  if [ "$mode" != "oauth" ]; then
    continue
  fi
  verify_entra_profile "$name" "$semp"
done

echo
echo "Done. Brokers:"
echo "  mcp-entra-solace    http://localhost:28081 / https://localhost:21943  (Entra OAuth, prod-us)"
echo "  mcp-entra-solace-c  http://localhost:28082 / https://localhost:21944  (basic admin/admin, local-basic)"
echo "  mcp-entra-solace-b  http://localhost:28083 / https://localhost:21945  (Entra OAuth, test-us)"
echo "Teardown: make -C local/entra brokers-down"
echo "Do not run solace-local-infra setup-oauth-brokers.sh on these names."
