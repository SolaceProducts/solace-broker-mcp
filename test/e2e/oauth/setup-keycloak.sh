#!/bin/bash
set -e  # Exit on any error

# =============================================================================
# Keycloak Setup Script
# =============================================================================
# This script orchestrates the complete Keycloak setup process:
# 1. Starts Keycloak container using Docker Compose
# 2. Waits for Keycloak to be healthy and ready
# 3. Configures Keycloak using Terraform (realm, clients, scopes)
# 4. Validates the OAuth configuration
# 5. Updates MCP server with OAuth credentials
# =============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TERRAFORM_DIR="${SCRIPT_DIR}/terraform"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
MCP_SERVER_ENV="${PROJECT_ROOT}/.env"

echo "========================================"
echo "Starting Keycloak Setup"
echo "========================================"
echo ""

# -----------------------------------------------------------------------------
# Step 1: Start Keycloak Container
# -----------------------------------------------------------------------------
echo "Step 1: Starting Keycloak container..."
cd "${SCRIPT_DIR}"

if docker ps | grep -q keycloak-dev; then
    echo "✓ Keycloak container is already running"
else
    docker compose up -d
    echo "✓ Keycloak container started"
fi

# -----------------------------------------------------------------------------
# Step 2: Wait for Keycloak to be Healthy
# -----------------------------------------------------------------------------
echo ""
echo "Step 2: Waiting for Keycloak to be healthy..."

MAX_WAIT=120  # Maximum wait time in seconds
ELAPSED=0
INTERVAL=5

while [ $ELAPSED -lt $MAX_WAIT ]; do
    HEALTH_STATUS=$(docker inspect --format='{{.State.Health.Status}}' keycloak-dev 2>/dev/null || echo "unknown")

    if [ "$HEALTH_STATUS" = "healthy" ]; then
        echo "✓ Keycloak is healthy and ready"
        break
    fi

    echo "  Waiting for Keycloak... (${ELAPSED}s/${MAX_WAIT}s)"
    sleep $INTERVAL
    ELAPSED=$((ELAPSED + INTERVAL))
done

if [ "$HEALTH_STATUS" != "healthy" ]; then
    echo "✗ Error: Keycloak failed to become healthy after ${MAX_WAIT}s"
    echo "  Check logs with: docker logs keycloak-dev"
    exit 1
fi

# Give Keycloak a few extra seconds to fully initialize
sleep 5

# -----------------------------------------------------------------------------
# Step 3: Configure Keycloak with Terraform
# -----------------------------------------------------------------------------
echo ""
echo "Step 3: Configuring Keycloak with Terraform..."
cd "${TERRAFORM_DIR}"

# Initialize Terraform if needed
if [ ! -d ".terraform" ]; then
    echo "  Initializing Terraform..."
    terraform init
fi

# Apply Terraform configuration
echo "  Applying Terraform configuration..."
terraform apply -auto-approve

echo "✓ Keycloak configuration applied"

# -----------------------------------------------------------------------------
# Step 4: Retrieve OAuth Configuration
# -----------------------------------------------------------------------------
echo ""
echo "Step 4: Retrieving OAuth configuration..."

CLIENT_SECRET=$(terraform output -raw mcp_client_secret)
TOKEN_ENDPOINT=$(terraform output -raw token_endpoint)
ISSUER=$(terraform output -raw issuer)
AUDIENCE=$(terraform output -raw audience)

echo "✓ OAuth configuration retrieved"

# -----------------------------------------------------------------------------
# Step 5: Test OAuth Token Endpoint
# -----------------------------------------------------------------------------
echo ""
echo "Step 5: Testing OAuth token endpoint..."

TOKEN_RESPONSE=$(curl -s -X POST "${TOKEN_ENDPOINT}" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    -d "grant_type=client_credentials" \
    -d "client_id=mcp-client-confidential" \
    -d "client_secret=${CLIENT_SECRET}" \
    -d "scope=solace:read solace:write")

# Check if we got an access token
if echo "${TOKEN_RESPONSE}" | grep -q "access_token"; then
    echo "✓ Successfully obtained access token"
else
    echo "✗ Failed to obtain access token"
    echo "Response: ${TOKEN_RESPONSE}"
    exit 1
fi

# Extract and decode the access token
ACCESS_TOKEN=$(echo "${TOKEN_RESPONSE}" | grep -o '"access_token":"[^"]*' | cut -d'"' -f4)

# Decode JWT (just the payload for verification)
JWT_PAYLOAD=$(echo "${ACCESS_TOKEN}" | cut -d'.' -f2)
# Add padding if needed for base64 decoding
case $((${#JWT_PAYLOAD} % 4)) in
    2) JWT_PAYLOAD="${JWT_PAYLOAD}==" ;;
    3) JWT_PAYLOAD="${JWT_PAYLOAD}=" ;;
esac

DECODED=$(echo "${JWT_PAYLOAD}" | base64 -d 2>/dev/null || echo "{}")

echo ""
echo "Token claims (decoded):"
echo "${DECODED}" | grep -E '"(iss|aud|scope|exp)"' | sed 's/^/  /'

# -----------------------------------------------------------------------------
# Step 6: Validate Token Claims
# -----------------------------------------------------------------------------
echo ""
echo "Step 6: Validating token claims..."

# Check issuer
if echo "${DECODED}" | grep -q "\"iss\":\"${ISSUER}\""; then
    echo "✓ Issuer is correct: ${ISSUER}"
else
    echo "✗ Issuer mismatch"
    exit 1
fi

# Check audience (can be string or array)
if echo "${DECODED}" | grep -q "\"${AUDIENCE}\""; then
    echo "✓ Audience is correct: ${AUDIENCE}"
else
    echo "✗ Audience mismatch"
    exit 1
fi

# Check scopes
if echo "${DECODED}" | grep -q "solace:read" && echo "${DECODED}" | grep -q "solace:write"; then
    echo "✓ Scopes are correct: solace:read solace:write"
else
    echo "✗ Scopes are missing or incorrect"
    exit 1
fi

# -----------------------------------------------------------------------------
# Step 7: Update MCP Server Configuration
# -----------------------------------------------------------------------------
echo ""
echo "Step 7: Updating MCP server .env file..."

# Check if .env file exists
if [ ! -f "${MCP_SERVER_ENV}" ]; then
    echo "  Creating new .env file..."
    touch "${MCP_SERVER_ENV}"
fi

# Update or add OAuth configuration
update_env_var() {
    local key=$1
    local value=$2
    local file=$3

    if grep -q "^${key}=" "${file}"; then
        # Update existing variable
        sed -i.bak "s|^${key}=.*|${key}=${value}|" "${file}"
    else
        # Add new variable
        echo "${key}=${value}" >> "${file}"
    fi
}

update_env_var "AUTH_ISSUER" "${ISSUER}" "${MCP_SERVER_ENV}"
update_env_var "AUTH_AUDIENCE" "${AUDIENCE}" "${MCP_SERVER_ENV}"
update_env_var "OAUTH_CLIENT_ID" "mcp-client" "${MCP_SERVER_ENV}"
update_env_var "OAUTH_CLIENT_SECRET" "${CLIENT_SECRET}" "${MCP_SERVER_ENV}"
update_env_var "OAUTH_TOKEN_URL" "${TOKEN_ENDPOINT}" "${MCP_SERVER_ENV}"

echo "✓ MCP server .env updated"

# -----------------------------------------------------------------------------
# Step 8: Retrieve Test User Credentials
# -----------------------------------------------------------------------------
TEST_USER_USERNAME=$(terraform output -raw test_user_username)
TEST_USER_PASSWORD=$(terraform output -raw test_user_password)
AUTHORIZATION_ENDPOINT=$(terraform output -raw authorization_endpoint)

# -----------------------------------------------------------------------------
# Done!
# -----------------------------------------------------------------------------
echo ""
echo "========================================"
echo "Keycloak Setup Complete! ✓"
echo "========================================"
echo ""
echo "Summary:"
echo "  • Keycloak running at: http://localhost:8090"
echo "  • Admin console: http://localhost:8090/admin (admin/admin)"
echo "  • Realm: solace"
echo ""
echo "Phase 1 (Client Credentials):"
echo "  • Client ID: mcp-client-confidential"
echo "  • Token endpoint: ${TOKEN_ENDPOINT}"
echo ""
echo "Phase 2 (Authorization Code + PKCE):"
echo "  • Client ID: mcp-client"
echo "  • Test user: ${TEST_USER_USERNAME}"
echo "  • Password: ${TEST_USER_PASSWORD}"
echo "  • Authorization endpoint: ${AUTHORIZATION_ENDPOINT}"
echo ""
echo "Next steps:"
echo "  • Start your MCP server to test OAuth integration"
echo "  • Use 'docker compose down' to stop Keycloak when done"
echo "  • Use 'terraform destroy' to remove Keycloak configuration"
echo ""
