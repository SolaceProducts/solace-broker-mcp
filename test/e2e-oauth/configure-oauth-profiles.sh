#!/usr/bin/env bash
# Configure the two e2e-oauth brokers' OAuth profiles + access-level groups
# against this suite's Keycloak, and install a TLS server cert on each broker's
# SEMP-TLS listener. Idempotent — safe to re-run.
#
# The SEMP-call functions (wait_for_semp_port, install_broker_tls_cert,
# upsert_profile, upsert_group) live in helpers.sh, not here — upsert_profile
# is also called directly by test-oauth-scenarios.sh's cache-invalidation
# scenario.
#
# Assumes: `docker compose up -d` has already been run for this suite (brokers
# + Keycloak containers exist and SEMP/HTTPS ports are published).

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/helpers.sh"

# broker-semp-port  required-audience
BROKERS=(
  "$BROKER_A_SEMP_PORT  $BROKER_A_AUDIENCE"
  "$BROKER_B_SEMP_PORT  $BROKER_B_AUDIENCE"
)

log_info "==> waiting for SEMP on both brokers"
for row in "${BROKERS[@]}"; do
  read -r port _aud <<<"$row"
  wait_for_semp_port "$port"
done

log_info "==> installing TLS server cert on both brokers (required for the SEMP-TLS port the MCP server connects to)"
ensure_dev_cert "$BROKER_TLS_CERT_DIR" "broker"
for row in "${BROKERS[@]}"; do
  read -r port _aud <<<"$row"
  install_broker_tls_cert "$port"
done

log_info "==> configuring OAuth profiles + access-level groups"
for row in "${BROKERS[@]}"; do
  read -r port audience <<<"$row"
  upsert_profile "$port" "$audience"
  upsert_group "$port" "solace-admins"   "admin"     "Maps Keycloak solace-admins group to broker admin access"
  upsert_group "$port" "solace-readonly" "read-only" "Maps Keycloak solace-readonly group to read-only access"
done

log_info "==> verifying"
for row in "${BROKERS[@]}"; do
  read -r port audience <<<"$row"
  actual=$(semp_curl -s \
    "http://localhost:${port}/SEMP/v2/config/oauthProfiles/keycloak_profile?select=resourceServerRequiredAudience" \
    | jq -r '.data.resourceServerRequiredAudience')
  if [ "$actual" = "$audience" ]; then
    log_info "port $port audience=$actual (OK)"
  else
    log_fail "port $port audience=$actual (want $audience)"
    exit 1
  fi
done

log_info "OAuth broker setup complete."
