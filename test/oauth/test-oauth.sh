#!/bin/bash
set -e

# =============================================================================
# OAuth Integration Test
# =============================================================================
# Verifies that OAuth authentication works end-to-end with real Keycloak
#
# Prerequisites: Run ./setup-keycloak.sh first
#
# Flow:
#   1. Get real OAuth token from Keycloak
#   2. Start MCP server with OAuth enabled
#   3. Make authenticated MCP request
#   4. Verify successful authentication
# =============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TERRAFORM_DIR="${SCRIPT_DIR}/terraform"
CERT_FILE="${SCRIPT_DIR}/certs/keycloak.crt"

echo "========================================"
echo "OAuth Integration Test"
echo "========================================"
echo ""

MCP_SERVER_PID=""

cleanup() {
    if [ -n "$MCP_SERVER_PID" ]; then
        echo "Stopping MCP server..."
        kill "$MCP_SERVER_PID" 2>/dev/null || true
        wait "$MCP_SERVER_PID" 2>/dev/null || true
    fi

    echo "Stopping Keycloak..."
    (cd "${SCRIPT_DIR}" && docker compose down) || echo "Warning: Failed to stop Keycloak"
}

trap cleanup EXIT

# Check Keycloak is running
if ! docker ps | grep -q keycloak-dev; then
    echo "ERROR: Keycloak not running. Run setup-keycloak.sh first."
    exit 1
fi
echo "✓ Keycloak is running"

# Get OAuth config from Terraform
cd "${TERRAFORM_DIR}"
ISSUER=$(terraform output -raw issuer)
AUDIENCE=$(terraform output -raw audience)
TOKEN_ENDPOINT=$(terraform output -raw token_endpoint)
CLIENT_ID=$(terraform output -raw mcp_client_confidential_id)
CLIENT_SECRET=$(terraform output -raw mcp_client_secret)

echo "✓ Retrieved OAuth configuration"
echo "  Issuer: ${ISSUER}"
echo "  Client: ${CLIENT_ID} (confidential client for client credentials flow)"

# Get access token from Keycloak (Phase 1: Client Credentials)
echo ""
echo "Getting OAuth token from Keycloak (client credentials flow)..."
TOKEN_RESPONSE=$(curl -s --cacert "$CERT_FILE" -X POST "${TOKEN_ENDPOINT}" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    -d "grant_type=client_credentials" \
    -d "client_id=${CLIENT_ID}" \
    -d "client_secret=${CLIENT_SECRET}")

ACCESS_TOKEN=$(echo "${TOKEN_RESPONSE}" | grep -o '"access_token":"[^"]*' | cut -d'"' -f4)

if [ -z "$ACCESS_TOKEN" ]; then
    echo "ERROR: Failed to get access token"
    echo "Response: ${TOKEN_RESPONSE}"
    exit 1
fi
echo "✓ Obtained access token"

# Build MCP server
echo ""
echo "Building MCP server..."
cd "${PROJECT_ROOT}"
go build -o "${PROJECT_ROOT}/bin/mcp-test" ./cmd/server
echo "✓ Built MCP server"

# Set broker credentials as environment variables (required by config)
export BROKER_USERNAME=${BROKER_USERNAME:-admin}
export BROKER_PASSWORD=${BROKER_PASSWORD:-admin}

# Use static test config file
TEST_CONFIG="${SCRIPT_DIR}/test-config.yaml"

if [ ! -f "${TEST_CONFIG}" ]; then
    echo "ERROR: Test config not found at ${TEST_CONFIG}"
    exit 1
fi

echo "✓ Using test configuration: ${TEST_CONFIG}"

# Start MCP server
echo ""
echo "Starting MCP server with OAuth..."
export SSL_CERT_FILE="${SCRIPT_DIR}/certs/keycloak.crt"
export CONFIG_FILE="${TEST_CONFIG}"
"${PROJECT_ROOT}/bin/mcp-test" > /tmp/mcp-oauth-test.log 2>&1 &
MCP_SERVER_PID=$!

# Wait for server to be ready
echo "Waiting for server to start..."
for i in {1..30}; do
    if curl -s http://localhost:9091/health > /dev/null 2>&1; then
        echo "✓ MCP server is ready"
        break
    fi
    sleep 1
    if [ $i -eq 30 ]; then
        echo "ERROR: Server failed to start"
        cat /tmp/mcp-oauth-test.log
        exit 1
    fi
done

# Make authenticated MCP request
echo ""
echo "Testing authenticated MCP request..."
HTTP_STATUS=$(curl -s -o /tmp/mcp-response.json -w "%{http_code}" \
    -X POST "http://localhost:9091/mcp" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer ${ACCESS_TOKEN}" \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test-client","version":"1.0.0"}}}')

if [ "$HTTP_STATUS" = "200" ]; then
    echo "✓ Authentication successful (HTTP 200)"
    echo ""
    echo "========================================"
    echo "Integration Test PASSED ✓"
    echo "========================================"
    exit 0
else
    echo "ERROR: Expected HTTP 200, got ${HTTP_STATUS}"
    echo "Response:"
    cat /tmp/mcp-response.json
    echo ""
    echo "Server logs:"
    cat /tmp/mcp-oauth-test.log
    exit 1
fi
