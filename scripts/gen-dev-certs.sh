#!/usr/bin/env bash
# Idempotent self-signed cert generator for local OAuth dev.
#
# Generates two cert/key pairs and a combined CA bundle under .local/certs/:
#   - mcp-server/mcp-server.{crt,key}  — presented by the MCP server on :9090
#   - keycloak/keycloak.{crt,key}      — presented by Keycloak on :8443
#   - combined-ca-bundle.crt           — both certs concatenated, for NODE_EXTRA_CA_CERTS
#
# Re-running is safe: existing certs are kept if they have >30 days validity.

set -euo pipefail

# Project-local cert root by default; override with $1 if needed.
CERT_ROOT="${1:-$(cd "$(dirname "$0")/.." && pwd)/.local/certs}"
DAYS=3650  # 10 years

mkdir -p "$CERT_ROOT/mcp-server" "$CERT_ROOT/keycloak"

ensure_cert() {
  local dir="$1" name="$2"
  local crt="$dir/$name.crt" key="$dir/$name.key"

  if [[ -f "$crt" && -f "$key" ]] \
     && openssl x509 -in "$crt" -noout -checkend $((30 * 24 * 3600)) >/dev/null 2>&1; then
    echo "  keeping   $name.crt (valid)"
    return
  fi

  openssl req -x509 -newkey rsa:2048 -sha256 -days "$DAYS" -nodes \
    -keyout "$key" -out "$crt" \
    -subj "/CN=localhost" \
    -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" \
    >/dev/null 2>&1
  chmod 0600 "$key"
  echo "  generated $name.crt (valid ${DAYS}d)"
}

echo "→ cert root: $CERT_ROOT"
ensure_cert "$CERT_ROOT/mcp-server" mcp-server
ensure_cert "$CERT_ROOT/keycloak"   keycloak

# Always refresh the bundle so it stays in sync with the leaves.
cat "$CERT_ROOT/mcp-server/mcp-server.crt" \
    "$CERT_ROOT/keycloak/keycloak.crt" \
    > "$CERT_ROOT/combined-ca-bundle.crt"
echo "  refreshed combined-ca-bundle.crt"

# Sanity check: fail loud if anything ended up unusable.
for f in \
  "$CERT_ROOT/mcp-server/mcp-server.crt" \
  "$CERT_ROOT/keycloak/keycloak.crt" \
  "$CERT_ROOT/combined-ca-bundle.crt"; do
  openssl x509 -in "$f" -noout -checkend 0 >/dev/null 2>&1 \
    || { echo "✗ $f is expired or invalid" >&2; exit 1; }
done
echo "✓ all certs valid"
