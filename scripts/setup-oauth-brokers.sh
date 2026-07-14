#!/usr/bin/env bash
#
# Set up two Solace brokers configured for OAuth against the local Keycloak
# dev stack. Idempotent — safe to re-run. See dev/oauth-token-exchange/broker-setup.md.
#
# Brokers created:
#   solace     → host ports 8081 (SEMP HTTP), 1943 (SEMP TLS/SMF)
#   solace-b   → host ports 8083 (SEMP HTTP), 1945 (SEMP TLS/SMF)
#
# Both brokers get an identical keycloak_profile except for the required
# audience — solace expects "solace-broker", solace-b expects
# "solace-broker-second". This mirrors the two audience mappers on the
# mcp-server-client in the realm export.

set -euo pipefail

# ------------------------------------------------------------------------------
# Config
# ------------------------------------------------------------------------------

BROKER_IMAGE="solace/solace-pubsub-standard:latest"
KEYCLOAK_NETWORK="solace-broker-mcp-oauth-dev_default"
ISSUER="https://localhost:18443/realms/mcp-test-realm"
JWKS_URL="http://keycloak:8080/realms/mcp-test-realm/protocol/openid-connect/certs"
REQUIRED_SCOPE="solace.admin"

# broker-name  semp-host-port  smf-host-port  required-audience
BROKERS=(
  "solace       8081  1943  solace-broker"
  "solace-b     8083  1945  solace-broker-second"
)

# ------------------------------------------------------------------------------
# Preconditions
# ------------------------------------------------------------------------------

# Prefer docker if it's a real binary; otherwise fall back to podman.
# ('docker' is an alias to podman on many dev machines, but aliases don't
# resolve inside a non-interactive shell script — need the actual binary.)
if command -v docker >/dev/null 2>&1; then
  CONTAINER_CLI=docker
elif command -v podman >/dev/null 2>&1; then
  CONTAINER_CLI=podman
else
  echo "error: neither 'docker' nor 'podman' found on PATH" >&2
  exit 1
fi
echo "using container CLI: $CONTAINER_CLI"

if ! "$CONTAINER_CLI" network inspect "$KEYCLOAK_NETWORK" >/dev/null 2>&1; then
  echo "error: Keycloak network '$KEYCLOAK_NETWORK' not found." >&2
  echo "       Run 'make dev-up' first to bring up Keycloak." >&2
  exit 1
fi

# ------------------------------------------------------------------------------
# Helpers
# ------------------------------------------------------------------------------

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

# join_network <container>
# Idempotent: attaches to the Keycloak network only if not already attached.
join_network() {
  local name="$1"
  if "$CONTAINER_CLI" inspect "$name" --format \
       '{{range $net, $_ := .NetworkSettings.Networks}}{{$net}} {{end}}' \
     | grep -qw "$KEYCLOAK_NETWORK"; then
    echo "  [$name] already on $KEYCLOAK_NETWORK"
  else
    echo "  [$name] joining $KEYCLOAK_NETWORK"
    "$CONTAINER_CLI" network connect "$KEYCLOAK_NETWORK" "$name"
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
# Creates keycloak_profile if absent, PATCHes to desired shape either way.
upsert_profile() {
  local port="$1" audience="$2"

  # Solace SEMPv2 returns HTTP 400 (not 404) when the resource is absent,
  # so a plain status-code check isn't enough — inspect the responseCode
  # inside the SEMP envelope. 200 means present.
  local status
  status=$(curl -s -u admin:admin \
    "http://localhost:${port}/SEMP/v2/config/oauthProfiles/keycloak_profile" \
    | python3 -c "import sys,json;print(json.load(sys.stdin).get('meta',{}).get('responseCode','?'))" 2>/dev/null || echo "?")

  if [ "$status" = "200" ]; then
    echo "  keycloak_profile already exists — patching to desired shape"
  else
    echo "  creating keycloak_profile"
    curl -sf -u admin:admin -X POST \
      "http://localhost:${port}/SEMP/v2/config/oauthProfiles" \
      -H "Content-Type: application/json" \
      -d "{\"oauthProfileName\":\"keycloak_profile\",\"oauthRole\":\"resource-server\"}" >/dev/null
  fi

  # Full-shape PATCH. Aligns any drifted fields to the desired configuration.
  # oauthRole must remain "resource-server"; the profile validates issuer/audience/type,
  # accepts group memberships from the "groups" claim, and stays disabled for interactive
  # (browser SSO) since this is a resource server.
  #
  # On a fresh profile we PATCH and then immediately try to POST child groups;
  # occasionally the broker rejects the group POST because the profile PATCH
  # hasn't been committed yet. A brief read-after-write confirms it's applied
  # before we proceed.
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

  # Read-after-write: confirm the audience we just PATCHed is what the
  # broker actually reports. Retries briefly to absorb the commit lag.
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

# upsert_group <semp-port> <group-name> <global-access-level> <description>
upsert_group() {
  local port="$1" name="$2" level="$3" desc="$4"

  # Same 400-means-absent convention as upsert_profile — check the
  # responseCode inside the envelope, not the HTTP status.
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

# ------------------------------------------------------------------------------
# Main
# ------------------------------------------------------------------------------

echo "==> ensuring broker containers exist and are running"
for row in "${BROKERS[@]}"; do
  read -r name semp smf _aud <<<"$row"
  ensure_container "$name" "$semp" "$smf"
  join_network "$name"
done

echo
echo "==> waiting for SEMP to come up"
for row in "${BROKERS[@]}"; do
  read -r name semp _smf _aud <<<"$row"
  echo "  [$name]"
  wait_for_semp "$semp"
done

echo
echo "==> configuring OAuth profiles + access-level groups"
for row in "${BROKERS[@]}"; do
  read -r name semp _smf audience <<<"$row"
  echo "  [$name]"
  upsert_profile "$semp" "$audience"
  upsert_group "$semp" "solace-admins"   "admin"     "Maps Keycloak solace-admins group to broker admin access"
  upsert_group "$semp" "solace-readonly" "read-only" "Maps Keycloak solace-readonly group to read-only access"
done

echo
echo "==> verifying"
for row in "${BROKERS[@]}"; do
  read -r name semp _smf audience <<<"$row"
  actual=$(curl -s -u admin:admin \
    "http://localhost:${semp}/SEMP/v2/config/oauthProfiles/keycloak_profile?select=resourceServerRequiredAudience" \
    | python3 -c "import sys,json;print(json.load(sys.stdin)['data']['resourceServerRequiredAudience'])")
  if [ "$actual" = "$audience" ]; then
    echo "  [$name] audience=$actual ✓"
  else
    echo "  [$name] audience=$actual (want $audience) ✗"
    exit 1
  fi
done

echo
echo "Done. Next steps:"
echo "  1. cp broker-config.oauth-test.example.yaml broker-config.oauth-test.yaml"
echo "  2. Fill in mcp_server_client_auth.client_secret_basic.secret from the Keycloak client"
echo "  3. In another terminal: make run-oauth"
