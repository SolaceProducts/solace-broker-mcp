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

# Default tool_authorization block: RBAC off. The token-exchange scenarios
# (test-oauth-scenarios.sh) exercise hop 2, not tool RBAC, and oauth mode
# requires an explicit opt-out. The RBAC suite passes its own block instead.
# Indentation is significant — these lines sit under mcp_client_auth: at two
# spaces, so the block is interpolated already-indented rather than being
# re-indented at the call site.
TOOL_AUTHZ_DISABLED='  tool_authorization:
    enabled: false'

# The group the RBAC policy grants on, and the single tool it grants.
#
# Single-sourced because three places must agree: the policy below, the
# allow scenario's matched_groups assertion, and the broker-isolation
# scenario's absence check. If they were separate literals, renaming the group
# in the policy would leave the isolation scenario asserting the absence of a
# group nobody grants on any more — passing forever while proving nothing,
# which is precisely the decay this suite exists to prevent.
RBAC_GRANT_GROUP="Ops"
RBAC_GRANTED_TOOL="list-vpns"

# A tool the policy does NOT grant, used to prove the grant is tool-scoped.
# Must be a real registered read tool, or ValidatePolicyToolNames would reject
# the config — it is referenced only by the scenario, never by the policy.
RBAC_UNGRANTED_TOOL="list-queues"

# ── Structurally exempt tools ────────────────────────────────────────────────
# Tools registered outside RegisterWithServer's policy-wrapping path in
# cmd/server/main.go, and therefore never wrapped by withAuthorization at all.
#
# There is no runtime predicate to query: exemption is expressed structurally at
# the registration API (RegisterListBrokers and RegisterDescribeSempSchema take
# no policy argument), not by a function the suite could call. So the set is
# derived from the tool-name constants instead — see
# exempt_tool_names_from_source — and the scenario fails if this table does not
# cover exactly what the source declares. Adding a third exempt tool therefore
# breaks the suite loudly rather than going silently uncovered.
#
# Each entry: <tool-name>|<args-json>|<jq predicate over the success payload>
RBAC_EXEMPT_TOOLS=(
    'list-brokers|{}|(.brokers | length) > 0'
    'describe-semp-schema|{"operation":"config/createMsgVpnQueue"}|.operation == "config/createMsgVpnQueue"'
)

# Emits, one per line and sorted, the tool names declared by the `*ToolName`
# constants under internal/tools. Both exempt tools follow that convention and
# no other constant does, which is what makes it a usable anchor. If a future
# refactor changes the convention this over-reports rather than under-reports —
# the safe direction, since the scenario then fails and a human looks.
exempt_tool_names_from_source() {
    grep -rhoE 'ToolName[[:space:]]*=[[:space:]]*"[^"]+"' "$SUITE_DIR/../../internal/tools/" 2>/dev/null \
        | sed -E 's/.*"([^"]+)"/\1/' | sort -u
}

# The tool-RBAC policy applied by test-rbac-scenarios.sh.
#
# The grant is deliberately on RBAC_GRANT_GROUP — a group the MCP server knows
# and that NO broker maps to an access level (configure-oauth-profiles.sh
# configures only solace-admins and solace-readonly). Granting on solace-admins
# instead would make a passing test meaningless: it could not distinguish "the
# MCP server authorized this call" from "the broker would have allowed it
# anyway", and would keep passing with the RBAC layer removed entirely.
#
# Note the group DOES travel to the broker in the exchanged token —
# mcp-server-client carries its own realm-roles-to-groups mapper — it is simply
# unmapped there. "MCP-only" means "no broker grants anything for it", not
# "the broker never sees it".
#
# test-admin-user holds BOTH solace-admins and Ops, so an allow whose
# matched_groups is exactly ["Ops"] proves the grant came from the MCP-only
# group rather than the broker-shared one.
#
# list-brokers is deliberately absent: it is structurally exempt
# (RegisterListBrokers takes no policy), and one scenario proves it stays
# callable by a caller this policy grants nothing to.
TOOL_AUTHZ_RBAC="  tool_authorization:
    enabled: true
    groups_claim_name: groups
    access_level_groups:
      ${RBAC_GRANT_GROUP}:
        - ${RBAC_GRANTED_TOOL}"

# The SAME policy with the feature flag off, used by the disabled phase.
#
# Deliberately not TOOL_AUTHZ_DISABLED (which has no access_level_groups at
# all): with an empty policy, a bug where `enabled: false` is ignored but the
# policy map is still consulted would be invisible, because there would be
# nothing to consult. Carrying the real grants here isolates the flag itself.
# config.validate only rejects enabled:true with an empty map, so enabled:false
# with grants is legal config.
TOOL_AUTHZ_RBAC_OFF="  tool_authorization:
    enabled: false
    groups_claim_name: groups
    access_level_groups:
      ${RBAC_GRANT_GROUP}:
        - ${RBAC_GRANTED_TOOL}"

# write_oauth_config <config_file> [tool_authorization_block]
#
# The optional second argument replaces the mcp_client_auth.tool_authorization
# block verbatim and must already carry the two-space indentation shown in
# TOOL_AUTHZ_DISABLED above. Omitted → RBAC off.
write_oauth_config() {
    local config_file="$1"
    local tool_authz="${2:-$TOOL_AUTHZ_DISABLED}"
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
${tool_authz}

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

    # SSL_CERT_FILE is Go's stdlib override (crypto/x509, Linux) for the
    # server's own outbound trust of Keycloak's self-signed cert.
    SSL_CERT_FILE="$KEYCLOAK_CERT" \
    CONFIG_FILE="$config_file" \
    ENV_FILE="$ENV_FILE" \
        "$BIN_DIR/mcp-server" >"$MCP_SERVER_LOG" 2>&1 &
    MCP_SERVER_PID=$!

    local attempt=0
    while [ $attempt -lt 30 ]; do
        # Liveness before readiness. /health alone is not proof that OUR server
        # came up: if a previous server still holds MCP_PORT, the one just
        # started fails to bind and exits, while the old one keeps answering
        # /health — reporting "ready" for a dead PID and running the phase
        # against the wrong config. Since stop_server SIGKILLs without
        # confirming exit, that race is reachable across a restart.
        if ! kill -0 "$MCP_SERVER_PID" 2>/dev/null; then
            log_fail "MCP server process exited during startup (PID=$MCP_SERVER_PID); last 50 lines of $MCP_SERVER_LOG:"
            tail -n 50 "$MCP_SERVER_LOG" >&2 2>/dev/null || true
            return 1
        fi
        if curl -sf --max-time 5 --cacert "$MCP_SERVER_CERT" "$MCP_URL/health" >/dev/null 2>&1; then
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
#
# mint_token <username> <password> [client_id]
#
# client_id defaults to HOP1_CLIENT_ID (agentic-app-client), whose
# realm-roles-to-groups mapper puts the caller's realm roles into a `groups`
# claim. Pass HOP1_CLIENT_ID_NOGROUPS to mint an otherwise-identical token that
# carries no `groups` claim at all — which claims a token gets depends on which
# client requested it, and that is the whole point of the missing-claim
# scenario. See test/e2e-oauth/README.md.
mint_token() {
    local username="$1" password="$2" client_id="${3:-$HOP1_CLIENT_ID}"
    local response token curl_status=0
    # No -f: on a 4xx, curl -f discards the body, and Keycloak puts the actual
    # reason there ("invalid_client" when the realm fixture is stale, for
    # instance). Losing it turns a one-line diagnosis into a CI archaeology
    # session. --max-time bounds a Keycloak that accepts TCP but never answers.
    #
    # The transport status is captured rather than left to errexit. A DNS
    # failure, TLS error, connection reset or --max-time expiry makes the
    # assignment non-zero, and under `set -e` that would abort before any of the
    # diagnostics below ran — the caller would see the function's exit status
    # and nothing about why.
    #
    # password@- reads that field's value from stdin instead of argv, keeping
    # it out of `ps`/`/proc` — same off-argv convention as semp_curl.
    response=$(printf '%s' "$password" | curl -s --max-time 15 --cacert "$KEYCLOAK_CERT" -X POST "$KEYCLOAK_TOKEN_ENDPOINT" \
        -H "Content-Type: application/x-www-form-urlencoded" \
        -d "grant_type=password" \
        -d "client_id=${client_id}" \
        -d "username=${username}" \
        --data-urlencode "password@-") || curl_status=$?

    if [ "$curl_status" -ne 0 ]; then
        log_fail "mint_token($username via $client_id): curl exited $curl_status reaching $KEYCLOAK_TOKEN_ENDPOINT"
        case "$curl_status" in
            28) log_fail "  timed out after 15s — Keycloak accepted the connection but did not answer" ;;
            7)  log_fail "  connection refused — is the Keycloak container up?" ;;
            60|77) log_fail "  TLS verification failed — check $KEYCLOAK_CERT matches the running Keycloak" ;;
        esac
        return 1
    fi

    token=$(jq -r '.access_token // empty' <<<"$response" 2>/dev/null)
    if [ -z "$token" ]; then
        log_fail "mint_token($username via $client_id): no access_token"
        log_fail "  Keycloak said: ${response:-<empty response>}"
        # The overwhelmingly common cause locally: a Keycloak container created
        # before this realm fixture changed. --import-realm only imports on
        # container creation, so `up -d` on an existing container keeps the old
        # realm and the new clients/roles simply do not exist.
        if grep -q "invalid_client\|unauthorized_client" <<<"$response" 2>/dev/null; then
            log_fail "  hint: realm fixture may be stale — run 'make e2e-oauth-down' (down -v) then 'make e2e-oauth-up'"
        fi
        return 1
    fi
    echo "$token"
}

# ── Server audit log (tool-RBAC assertions) ─────────────────────────────────
# A tool-authorization denial is NOT an HTTP 401 and NOT a JSON-RPC error —
# authentication failures are handled well before the tool layer. It is an
# ordinary 200 carrying a tool-level error result, and the two deny reasons
# are deliberately indistinguishable to the caller (see the message constants
# in internal/tools/authorization.go). The server-side audit event is the only
# place that separates them, so RBAC assertions read the log, not the wire.
#
# The server writes JSON slog records to $MCP_SERVER_LOG synchronously (Go's
# os.File writes are unbuffered syscalls) and withAuthorization logs *before*
# returning, so a record is on disk by the time curl sees the response — no
# polling needed here, unlike count_token_exchanges' docker-log round trip.

# Records the current end of the server log. Pair with authz_records_since so
# an assertion only considers records appended by the call under test.
#
# The fallback is on wc, not on the pipeline: `wc | tr || echo 0` never fires
# the fallback because tr is the last element and always succeeds, which would
# silently yield an empty mark and widen every subsequent window to the whole
# file.
log_mark() {
    local n
    n=$(wc -l < "$MCP_SERVER_LOG" 2>/dev/null) || n=0
    echo "${n// /}"
}

# ── RBAC mode positive control ──────────────────────────────────────────────
# Several scenarios assert the ABSENCE of audit records. Absence assertions are
# unfalsifiable on their own: rename the slog message, break the log path, or
# point a phase at the wrong server, and they pass forever while proving
# nothing. assert_server_rbac_mode is the positive control — it confirms the
# server under test really is in the mode the phase expects, by reading the
# startup config record that ToolAuthorizationConfig.LogValue emits.
#
# cmd/server/main.go announces the RBAC posture at startup with one of:
#   "tool authorization is enabled"                        (+ policy counts)
#   "tool authorization is disabled (enabled=false in config)"
#   "tool authorization is disabled"                       (+ auth_mode)
# Matching the "is enabled"/"is disabled" prefix covers all three without
# depending on the parenthetical.
#
# assert_server_rbac_mode <enabled|disabled>
assert_server_rbac_mode() {
    local want="$1" record actual
    case "$want" in
        enabled|disabled) ;;
        *) log_fail "assert_server_rbac_mode: bad mode '$want'"; return 1 ;;
    esac

    # Last occurrence wins. start_oauth_server truncates the log per start, so
    # there is normally exactly one, but be explicit rather than rely on it.
    record=$(jq -c -R 'fromjson? | select(.msg | startswith("tool authorization is "))' \
        "$MCP_SERVER_LOG" 2>/dev/null | tail -n 1)

    if [ -z "$record" ]; then
        log_fail "no 'tool authorization is ...' startup record in $MCP_SERVER_LOG"
        log_fail "  that record is the positive control for every absence assertion in this phase; without it they prove nothing"
        return 1
    fi

    case "$(jq -r '.msg' <<<"$record")" in
        "tool authorization is enabled"*)  actual=enabled ;;
        "tool authorization is disabled"*) actual=disabled ;;
        *) log_fail "unrecognised startup record: $record"; return 1 ;;
    esac

    if [ "$actual" != "$want" ]; then
        log_fail "server reports tool authorization $actual, but this phase expects $want"
        log_fail "  the phase is pointed at the wrong server config — its results would be meaningless"
        log_fail "  record: $record"
        return 1
    fi

    # On the enabled path the record also carries the compiled policy shape, so
    # confirm the grants actually made it in. A policy that compiled to zero
    # grants would deny everything and make the deny scenarios pass for the
    # wrong reason.
    if [ "$want" = "enabled" ]; then
        local grants
        grants=$(jq -r '.policy.tool_grant_count // 0' <<<"$record")
        if [ "$grants" -lt 1 ]; then
            log_fail "policy compiled with $grants tool grants; the deny scenarios would pass for the wrong reason"
            log_fail "  record: $record"
            return 1
        fi
    fi
    return 0
}

# authz_records_since <mark> [tool]
# Emits, one JSON object per line, the "tool authorization" audit records
# appended after <mark>, optionally filtered to a single tool name.
# fromjson? skips any non-JSON line (e.g. a Go panic trace) rather than
# aborting the whole read.
authz_records_since() {
    local mark="$1" tool="${2:-}"
    tail -n +"$((mark + 1))" "$MCP_SERVER_LOG" 2>/dev/null \
        | jq -c -R --arg t "$tool" '
            fromjson?
            | select(.msg == "tool authorization")
            | select($t == "" or .tool == $t)'
}

# assert_authz_decision <mark> <tool> <decision> [decision_reason] [msg]
# Asserts exactly one audit record for <tool> since <mark>, with the given
# decision and (when supplied) decision_reason.
assert_authz_decision() {
    local mark="$1" tool="$2" want_decision="$3" want_reason="${4:-}" msg="${5:-}"
    local records count actual_decision actual_reason
    records=$(authz_records_since "$mark" "$tool")
    count=$(grep -c . <<<"$records" || true)
    if [ "$count" -ne 1 ]; then
        log_fail "${msg:-authz audit}: expected exactly 1 audit record for $tool, saw $count"
        [ -n "$records" ] && log_fail "  records: $records"
        return 1
    fi
    actual_decision=$(jq -r '.decision' <<<"$records")
    if [ "$actual_decision" != "$want_decision" ]; then
        log_fail "${msg:-authz audit}: $tool decision=$actual_decision, want $want_decision"
        log_fail "  record: $records"
        return 1
    fi
    if [ -n "$want_reason" ]; then
        actual_reason=$(jq -r '.decision_reason // "<none>"' <<<"$records")
        if [ "$actual_reason" != "$want_reason" ]; then
            log_fail "${msg:-authz audit}: $tool decision_reason=$actual_reason, want $want_reason"
            log_fail "  record: $records"
            return 1
        fi
    fi
    return 0
}

# ── Server restart (config-swap scenarios) ──────────────────────────────────
# The tool-RBAC phase needs the same calls run against a differently configured
# server. Restarting in place keeps one MCP_PORT and one log path.
#
# start_oauth_server truncates $MCP_SERVER_LOG, so the outgoing phase's log is
# rotated aside first — otherwise a phase-1 failure would have its evidence
# erased by the phase-2 restart before CI ever uploaded it. Rotated logs are
# named mcp-server.log.1, .2, ... in restart order; CI's failure step collects
# mcp-server.log*.
#
# Take a fresh log_mark after calling this.
restart_oauth_server() {
    local config_file="$1"
    stop_server

    # stop_server SIGKILLs and returns without confirming the port was
    # released. Wait for it here rather than relying on start_oauth_server's
    # one-second sleep: if the old process still holds MCP_PORT, the new one
    # cannot bind, and the old server would go on answering /health.
    local waited=0
    while [ -n "$(lsof -ti:"$MCP_PORT" 2>/dev/null || true)" ]; do
        if [ "$waited" -ge 20 ]; then
            log_warn "port $MCP_PORT still held after 10s; start_oauth_server will try to clear it"
            break
        fi
        sleep 0.5
        waited=$((waited + 1))
    done

    if [ -f "$MCP_SERVER_LOG" ]; then
        local n=1
        while [ -f "$MCP_SERVER_LOG.$n" ]; do n=$((n + 1)); done
        mv "$MCP_SERVER_LOG" "$MCP_SERVER_LOG.$n"
        log_info "Previous server log rotated to $(basename "$MCP_SERVER_LOG").$n"
    fi
    start_oauth_server "$config_file"
}

# ── MCP wire (token-parameterized) ──────────────────────────────────────────
# Mirrors e2e-common/lib.sh's mcp_initialize/mcp_call_tool/mcp_request, taking
# the bearer token as an argument instead of the shared static dev token, and
# using --cacert for the server's self-signed cert. See mcp_initialize there for
# why protocolVersion pins 2025-11-25 rather than go-sdk's latest revision.

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
                "protocolVersion": "2025-11-25",
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
