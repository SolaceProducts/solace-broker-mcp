#!/usr/bin/env bash
# Idempotent self-signed certs for the local Entra lab.
# Adapted from solace-local-infra/scripts/gen-dev-certs.sh (no Keycloak leaf).
#
# Under .local/certs/:
#   - mcp-server/mcp-server.{crt,key}  — MCP HTTPS
#   - combined-ca-bundle.crt           — same cert, for NODE_EXTRA_CA_CERTS
#
# Re-running keeps existing certs if they still have RENEW_WITHIN days left.
#
# LEAF certs — CA:FALSE, serverAuth only. openssl req -x509 defaults to
# CA:TRUE without basicConstraints; trusting that in an OS store would let
# the key mint a cert for any hostname.

set -euo pipefail

CERT_ROOT="${1:-$(cd "$(dirname "$0")/.." && pwd)/.local/certs}"

DAYS=90
RENEW_WITHIN=15

mkdir -p "$CERT_ROOT/mcp-server"

ensure_cert() {
  local dir="$1" name="$2"
  local crt="$dir/$name.crt" key="$dir/$name.key"

  if [[ -f "$crt" && -f "$key" ]] \
     && openssl x509 -in "$crt" -noout -checkend $((RENEW_WITHIN * 24 * 3600)) >/dev/null 2>&1 \
     && openssl x509 -in "$crt" -noout -ext basicConstraints 2>/dev/null | grep -q "CA:FALSE" \
     && openssl x509 -in "$crt" -noout -text 2>/dev/null | grep -q "DNS:mcp-lab.solacetest.com"; then
    echo "  keeping   $name.crt (valid, CA:FALSE)"
    return
  fi

  if [[ -f "$crt" ]] \
     && ! openssl x509 -in "$crt" -noout -ext basicConstraints 2>/dev/null | grep -q "CA:FALSE"; then
    echo "  replacing $name.crt (was an unconstrained CA — see header)"
  fi

  openssl req -x509 -newkey rsa:2048 -sha256 -days "$DAYS" -nodes \
    -keyout "$key" -out "$crt" \
    -subj "/CN=localhost" \
    -addext "subjectAltName=DNS:localhost,DNS:mcp-lab.solacetest.com,IP:127.0.0.1" \
    -addext "basicConstraints=critical,CA:FALSE" \
    -addext "keyUsage=critical,digitalSignature,keyEncipherment" \
    -addext "extendedKeyUsage=serverAuth" \
    >/dev/null 2>&1
  chmod 0600 "$key"
  echo "  generated $name.crt (leaf, serverAuth, ${DAYS}d)"
}

echo "→ cert root: $CERT_ROOT"
ensure_cert "$CERT_ROOT/mcp-server" mcp-server

cat "$CERT_ROOT/mcp-server/mcp-server.crt" > "$CERT_ROOT/combined-ca-bundle.crt"
echo "  refreshed combined-ca-bundle.crt"

for f in \
  "$CERT_ROOT/mcp-server/mcp-server.crt" \
  "$CERT_ROOT/combined-ca-bundle.crt"; do
  openssl x509 -in "$f" -noout -checkend 0 >/dev/null 2>&1 \
    || { echo "✗ $f is expired or invalid" >&2; exit 1; }
done

if ! openssl x509 -in "$CERT_ROOT/mcp-server/mcp-server.crt" -noout -ext basicConstraints 2>/dev/null | grep -q "CA:FALSE"; then
  echo "✗ mcp-server.crt is a signing CA (CA:FALSE missing) — refusing" >&2
  echo "  Delete $CERT_ROOT and re-run to regenerate as leaf certs." >&2
  exit 1
fi
echo "✓ all certs valid (leaf, serverAuth, ${DAYS}d)"
