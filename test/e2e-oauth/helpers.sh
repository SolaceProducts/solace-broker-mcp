#!/usr/bin/env bash
# OAuth-suite helpers on top of the shared e2e-common/lib.sh scaffold. Adds
# everything OAuth-specific: TLS-aware MCP wire calls (per-user minted JWTs,
# not the shared lib's static dev token), an OAuth-mode config writer, token
# minting, broker OAuth-profile configuration, and token-exchange counting.

set -euo pipefail

SUITE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export SUITE_DIR
# shellcheck source=../e2e-common/lib.sh
source "$SUITE_DIR/../e2e-common/lib.sh"

# OAuth mode enforces TLS on the server's own listener — override the shared
# lib's http default now that MCP_PORT is known.
MCP_URL="https://localhost:${MCP_PORT}"

# ── Certs ────────────────────────────────────────────────────────────────────
MCP_SERVER_CERT_DIR="$BIN_DIR/certs/mcp-server"
KEYCLOAK_CERT_DIR="$BIN_DIR/certs/keycloak"
BROKER_TLS_CERT_DIR="$BIN_DIR/certs/broker"
MCP_SERVER_CERT="$MCP_SERVER_CERT_DIR/mcp-server.crt"
MCP_SERVER_KEY="$MCP_SERVER_CERT_DIR/mcp-server.key"
KEYCLOAK_CERT="$KEYCLOAK_CERT_DIR/keycloak.crt"
KEYCLOAK_KEY="$KEYCLOAK_CERT_DIR/keycloak.key"

# ensure_dev_cert <dir> <name>
# Idempotent: keeps an existing cert if it has >30 days validity, else
# (re)generates a 10-year self-signed RSA-2048 cert for localhost.
ensure_dev_cert() {
    local dir="$1" name="$2"
    local crt="$dir/$name.crt" key="$dir/$name.key"
    if [[ -f "$crt" && -f "$key" ]] \
       && openssl x509 -in "$crt" -noout -checkend $((30 * 24 * 3600)) >/dev/null 2>&1; then
        log_info "  keeping   $name.crt (valid)"
        return
    fi
    mkdir -p "$dir"
    openssl req -x509 -newkey rsa:2048 -sha256 -days 3650 -nodes \
        -keyout "$key" -out "$crt" -subj "/CN=localhost" \
        -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" >/dev/null 2>&1
    # World-readable, not 0600: these are throwaway self-signed dev certs, and
    # a container's internal user UID doesn't necessarily match the host UID
    # that owns the bind-mounted file.
    chmod 0644 "$key"
    log_info "  generated $name.crt (valid 3650d)"
}

# Generates the MCP-server and Keycloak cert/key pairs. Called once, before
# `docker compose up` (Keycloak's compose service bind-mounts these paths, so
# they must exist at container-creation time).
ensure_tls_certs() {
    log_info "==> generating dev TLS certs"
    ensure_dev_cert "$MCP_SERVER_CERT_DIR" "mcp-server"
    ensure_dev_cert "$KEYCLOAK_CERT_DIR" "keycloak"
}

# ── Keycloak URLs ────────────────────────────────────────────────────────────
KEYCLOAK_ISSUER="https://localhost:${KEYCLOAK_HTTPS_PORT}/realms/${KEYCLOAK_REALM}"
KEYCLOAK_TOKEN_ENDPOINT="$KEYCLOAK_ISSUER/protocol/openid-connect/token"

# ── Keycloak readiness ───────────────────────────────────────────────────────
wait_for_keycloak() {
    local max_attempts="${1:-60}"
    local url="$KEYCLOAK_ISSUER/.well-known/openid-configuration"
    local attempt=0
    log_info "Waiting for Keycloak OIDC discovery at $url ..."
    while [ $attempt -lt "$max_attempts" ]; do
        if curl -sf --cacert "$KEYCLOAK_CERT" "$url" >/dev/null 2>&1; then
            log_info "Keycloak ready after ${attempt}s"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done
    log_fail "Keycloak not ready within ${max_attempts}s"
    return 1
}

# ── OAuth config writer ──────────────────────────────────────────────────────
# Suite-local — not e2e-common/lib.sh's write_config, which hardcodes
# mcp_client_auth.mode: static / brokers.*.auth.mode: basic for the other e2e
# suites. Field names match internal/config/config.go's yaml tags.
#
# Includes a third, deliberately-misconfigured broker alias
# (test-us-wrong-audience) for the wrong-audience scenario — same URL/port as
# broker B but with broker A's audience, so a well-formed exchanged token
# reaches broker B with the wrong required audience and gets rejected.
write_oauth_config() {
    local config_file="$1"
    cat > "$config_file" <<EOF
port: ${MCP_PORT}
log_level: debug

enable_write_tools: true
allow_insecure_broker_tls: true

tls_cert_file: "${MCP_SERVER_CERT}"
tls_key_file: "${MCP_SERVER_KEY}"

mcp_client_auth:
  mode: oauth
  issuer: "${KEYCLOAK_ISSUER}"
  audience: "${HOP1_AUDIENCE}"
  resource_url: "https://localhost:${MCP_PORT}/mcp"

broker_oauth:
  idp_token_endpoint: "${KEYCLOAK_TOKEN_ENDPOINT}"
  mcp_server_client_id: "${HOP2_CLIENT_ID}"
  mcp_server_client_auth:
    client_secret_basic:
      secret: "${HOP2_CLIENT_SECRET}"
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: "audience"

semp:
  max_concurrent_per_broker: 10
  request_timeout_duration: 30s
  request_min_interval: 50ms
  retries: 2
  retry_min_interval: 200ms
  retry_max_interval: 2s

brokers:
  ${BROKER_A_ALIAS}:
    url: "https://localhost:${BROKER_A_SEMP_TLS_PORT}"
    insecure_skip_verify: true
    auth:
      mode: oauth
      audience: "${BROKER_A_AUDIENCE}"
  ${BROKER_B_ALIAS}:
    url: "https://localhost:${BROKER_B_SEMP_TLS_PORT}"
    insecure_skip_verify: true
    auth:
      mode: oauth
      audience: "${BROKER_B_AUDIENCE}"
  test-us-wrong-audience:
    url: "https://localhost:${BROKER_B_SEMP_TLS_PORT}"
    insecure_skip_verify: true
    auth:
      mode: oauth
      audience: "${BROKER_A_AUDIENCE}"
EOF
    log_info "OAuth config written to $config_file"
}

# ── TLS-aware server start ──────────────────────────────────────────────────
# Mirrors e2e-common/lib.sh's start_server, differing only in the readiness
# curl needing --cacert for our self-signed cert. build_server, stop_server,
# kill_gracefully, check_build_deps are reused as-is from the shared lib.
start_oauth_server() {
    local config_file="$1"
    log_info "Starting MCP server (config=$config_file, port=$MCP_PORT, TLS) ..."

    local existing_pid
    existing_pid=$(lsof -ti:"$MCP_PORT" 2>/dev/null || true)
    if [ -n "$existing_pid" ]; then
        log_warn "Killing existing process on port $MCP_PORT (PID=$existing_pid)"
        kill "$existing_pid" 2>/dev/null || true
        sleep 1
    fi

    # ENABLE_UNRELEASED_BROKER_OAUTH gates broker-side OAuth support behind a
    # feature flag. SSL_CERT_FILE is Go's stdlib override (crypto/x509, Linux)
    # for the server's own outbound trust of Keycloak's self-signed cert.
    ENABLE_UNRELEASED_BROKER_OAUTH=true \
    SSL_CERT_FILE="$KEYCLOAK_CERT" \
    CONFIG_FILE="$config_file" \
    ENV_FILE="$ENV_FILE" \
        "$BIN_DIR/mcp-server" >"$MCP_SERVER_LOG" 2>&1 &
    MCP_SERVER_PID=$!

    local attempt=0
    while [ $attempt -lt 30 ]; do
        if curl -sf --cacert "$MCP_SERVER_CERT" "$MCP_URL/health" >/dev/null 2>&1; then
            log_info "MCP server ready (PID=$MCP_SERVER_PID)"
            return 0
        fi
        sleep 0.5
        attempt=$((attempt + 1))
    done
    log_fail "MCP server failed to start; last 50 lines of $MCP_SERVER_LOG:"
    tail -n 50 "$MCP_SERVER_LOG" >&2 2>/dev/null || true
    return 1
}

# ── Token minting (Hop 1) ────────────────────────────────────────────────────
# Direct password grant against agentic-app-client (public,
# directAccessGrantsEnabled: true in realm-export.json) — bypasses the
# browser-based Authorization Code + PKCE flow.
mint_token() {
    local username="$1" password="$2"
    local response token
    # password@- reads that field's value from stdin instead of argv, keeping
    # it out of `ps`/`/proc` — same off-argv convention as semp_curl.
    response=$(printf '%s' "$password" | curl -sf --cacert "$KEYCLOAK_CERT" -X POST "$KEYCLOAK_TOKEN_ENDPOINT" \
        -H "Content-Type: application/x-www-form-urlencoded" \
        -d "grant_type=password" \
        -d "client_id=${HOP1_CLIENT_ID}" \
        -d "username=${username}" \
        --data-urlencode "password@-")
    token=$(jq -r '.access_token // empty' <<<"$response")
    if [ -z "$token" ]; then
        log_fail "mint_token($username): no access_token in response: $response"
        return 1
    fi
    echo "$token"
}

# ── MCP wire (token-parameterized) ──────────────────────────────────────────
# Mirrors e2e-common/lib.sh's mcp_initialize/mcp_call_tool/mcp_request, taking
# the bearer token as an argument instead of the shared static dev token, and
# using --cacert for the server's self-signed cert.

mcp_initialize_as() {
    local token="$1"
    local response session_id
    response=$(curl -sf --cacert "$MCP_SERVER_CERT" -D - -X POST "$MCP_URL/mcp" \
        -H "Content-Type: application/json" \
        -H "Accept: application/json, text/event-stream" \
        -H "Authorization: Bearer $token" \
        -d '{
            "jsonrpc": "2.0",
            "id": 1,
            "method": "initialize",
            "params": {
                "protocolVersion": "2025-03-26",
                "capabilities": {},
                "clientInfo": { "name": "e2e-oauth-test", "version": "1.0.0" }
            }
        }')

    session_id=$(echo "$response" | grep -i 'mcp-session-id' | tr -d '\r' | awk -F': ' '{print $2}')
    if [ -z "$session_id" ]; then
        log_fail "No Mcp-Session-Id in initialize response"
        echo "$response" >&2
        return 1
    fi

    curl -sf --cacert "$MCP_SERVER_CERT" -X POST "$MCP_URL/mcp" \
        -H "Content-Type: application/json" \
        -H "Accept: application/json, text/event-stream" \
        -H "Authorization: Bearer $token" \
        -H "Mcp-Session-Id: $session_id" \
        -d '{"jsonrpc": "2.0", "method": "notifications/initialized"}' >/dev/null 2>&1 || true

    echo "$session_id"
}

mcp_request_as() {
    local token="$1" session_id="$2" body="$3"
    local raw
    raw=$(curl -s --cacert "$MCP_SERVER_CERT" -X POST "$MCP_URL/mcp" \
        -H "Content-Type: application/json" \
        -H "Accept: application/json, text/event-stream" \
        -H "Authorization: Bearer $token" \
        -H "Mcp-Session-Id: $session_id" \
        -d "$body")
    echo "$raw" | grep '^data: ' | sed 's/^data: //'
}

# Initialize a session as the given bearer token and call one tool.
#   $1 token   $2 tool name   $3 args_json
mcp_call_tool_as() {
    local token="$1" tool="$2" args_json="$3"
    local sid
    sid=$(mcp_initialize_as "$token") || return 1
    mcp_request_as "$token" "$sid" "$(jq -nc --arg t "$tool" --argjson a "$args_json" \
        '{jsonrpc:"2.0",id:1,method:"tools/call",params:{name:$t,arguments:$a}}')"
}

# ── Broker OAuth profile configuration ──────────────────────────────────────
# Kept here rather than in configure-oauth-profiles.sh so
# test-oauth-scenarios.sh can also call upsert_profile directly, to
# poison/restore a broker's required audience for the cache-invalidation test.

OAUTH_ISSUER="https://localhost:${KEYCLOAK_HTTPS_PORT}/realms/${KEYCLOAK_REALM}"
OAUTH_JWKS_URL="http://keycloak:8080/realms/${KEYCLOAK_REALM}/protocol/openid-connect/certs"
OAUTH_REQUIRED_SCOPE="solace.admin"

wait_for_semp_port() {
    local port="$1" i
    for i in $(seq 1 90); do
        if semp_curl -sf -o /dev/null --max-time 2 \
             "http://localhost:${port}/SEMP/v2/config/about" 2>/dev/null; then
            log_info "SEMP on port $port ready (${i}s)"
            return 0
        fi
        sleep 1
    done
    log_fail "SEMP on port $port did not become ready in 90s"
    return 1
}

# semp_call <method> <url> [data]
# Authenticated SEMP call with failure diagnostics: captures status and body
# instead of discarding them on a bare curl -f failure, so a non-2xx response
# is diagnosable from CI output alone.
semp_call() {
    local method="$1" url="$2" data="${3:-}"
    local response status body
    if [ -n "$data" ]; then
        response=$(semp_curl -s -w $'\n%{http_code}' -X "$method" \
            -H "Content-Type: application/json" -d "$data" "$url")
    else
        response=$(semp_curl -s -w $'\n%{http_code}' -X "$method" "$url")
    fi
    status="${response##*$'\n'}"
    body="${response%$'\n'*}"
    if [ "$status" -ge 200 ] && [ "$status" -lt 300 ]; then
        printf '%s\n' "$body"
        return 0
    fi
    log_fail "SEMP $method $url returned HTTP $status"
    [ -n "$body" ] && printf '%s\n' "$body" >&2
    return 1
}

# install_broker_tls_cert <semp-port>
# PATCHes the broker's top-level tlsServerCertContent so its SEMP-TLS listener
# (port 1943) completes a handshake. A fresh Solace broker has that port
# enabled but no certificate installed. Idempotent.
install_broker_tls_cert() {
    local port="$1"
    local combined body
    combined=$(cat "$BROKER_TLS_CERT_DIR/broker.crt" "$BROKER_TLS_CERT_DIR/broker.key")
    body=$(jq -n --arg cert "$combined" '{tlsServerCertContent: $cert}')
    semp_call PATCH "http://localhost:${port}/SEMP/v2/config" "$body" >/dev/null
    log_info "TLS server cert installed on port $port"
}

# upsert_profile <semp-port> <audience>
# Creates keycloak_profile if absent, PATCHes to desired shape either way.
# Also called directly by test-oauth-scenarios.sh's cache-invalidation
# scenario to temporarily poison, then restore, a broker's required audience.
upsert_profile() {
    local port="$1" audience="$2"

    # Solace SEMPv2 returns HTTP 400 (not 404) when the resource is absent, so
    # a plain status-code check isn't enough — inspect the responseCode inside
    # the SEMP envelope. 200 means present.
    local status
    status=$(semp_curl -s \
        "http://localhost:${port}/SEMP/v2/config/oauthProfiles/keycloak_profile" \
        | jq -r '.meta.responseCode // "?"' 2>/dev/null || echo "?")

    if [ "$status" = "200" ]; then
        log_info "keycloak_profile already exists on port $port — patching to desired shape"
    else
        log_info "creating keycloak_profile on port $port"
        semp_call POST "http://localhost:${port}/SEMP/v2/config/oauthProfiles" \
            '{"oauthProfileName":"keycloak_profile","oauthRole":"resource-server"}' >/dev/null
    fi

    semp_call PATCH "http://localhost:${port}/SEMP/v2/config/oauthProfiles/keycloak_profile" \
        "$(cat <<JSON
{
  "oauthRole": "resource-server",
  "issuer": "$OAUTH_ISSUER",
  "resourceServerRequiredIssuer": "$OAUTH_ISSUER",
  "endpointJwks": "$OAUTH_JWKS_URL",
  "resourceServerRequiredAudience": "$audience",
  "resourceServerRequiredScope": "$OAUTH_REQUIRED_SCOPE",
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
    # broker actually reports. Retries briefly to absorb commit lag.
    local i
    for i in 1 2 3 4 5; do
        if semp_curl -s \
             "http://localhost:${port}/SEMP/v2/config/oauthProfiles/keycloak_profile?select=resourceServerRequiredAudience" \
             | jq -e ".data.resourceServerRequiredAudience==\"$audience\"" >/dev/null 2>&1; then
            break
        fi
        sleep 1
        if [ "$i" = "5" ]; then
            log_fail "profile PATCH on port $port did not take effect within 5s"
            return 1
        fi
    done
    log_info "profile patched on port $port (audience=$audience)"
}

# upsert_group <semp-port> <group-name> <global-access-level> <description>
upsert_group() {
    local port="$1" name="$2" level="$3" desc="$4"

    local status
    status=$(semp_curl -s \
        "http://localhost:${port}/SEMP/v2/config/oauthProfiles/keycloak_profile/accessLevelGroups/$name" \
        | jq -r '.meta.responseCode // "?"' 2>/dev/null || echo "?")

    local method url
    if [ "$status" = "200" ]; then
        method=PATCH
        url="http://localhost:${port}/SEMP/v2/config/oauthProfiles/keycloak_profile/accessLevelGroups/$name"
    else
        method=POST
        url="http://localhost:${port}/SEMP/v2/config/oauthProfiles/keycloak_profile/accessLevelGroups"
    fi

    local body
    if [ "$method" = "POST" ]; then
        body="{\"groupName\":\"$name\",\"globalAccessLevel\":\"$level\",\"msgVpnAccessLevel\":\"none\",\"description\":\"$desc\"}"
    else
        body="{\"globalAccessLevel\":\"$level\",\"msgVpnAccessLevel\":\"none\",\"description\":\"$desc\"}"
    fi

    semp_call "$method" "$url" "$body" >/dev/null
    log_info "group $name -> $level configured on port $port"
}

# ── Cache-hit observability ─────────────────────────────────────────────────
# Counts successful token-exchange events Keycloak's own container log has
# recorded so far. Callers snapshot before/after a sequence of tool calls and
# diff the two counts. Needs org.keycloak.events at DEBUG (set via
# KC_LOG_LEVEL in docker-compose.yml) — at the default level Keycloak logs
# failed exchanges but not successful ones. The trailing quote in the pattern
# excludes type="TOKEN_EXCHANGE_ERROR" lines.
count_token_exchanges() {
    docker logs keycloak-e2e-oauth 2>&1 | grep -c 'type="TOKEN_EXCHANGE"' || true
}
