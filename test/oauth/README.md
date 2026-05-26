# OAuth Integration Tests

Integration test for OAuth authentication middleware using real Keycloak OIDC provider.

## Prerequisites

1. Keycloak must be running and configured:
   ```bash
   ./setup-keycloak.sh
   ```

2. Test configuration (optional):
   - Edit `test-config.yaml` to customize broker URL or port
   - Optionally override broker credentials (defaults to `admin`/`admin`):
     ```bash
     export BROKER_USERNAME=myuser
     export BROKER_PASSWORD=mypass
     ```

3. Required tools:
   - `docker` (for Keycloak)
   - `terraform` (for Keycloak config)
   - `curl` (for HTTP requests)
   - `jq` (optional, for JSON parsing)

## Running the Test

```bash
./test-oauth.sh
```

## What It Tests

The integration test verifies **Phase 1: Client Credentials** flow:

1. **Token Acquisition**: Gets a real JWT access token from Keycloak using client credentials grant
2. **Server Startup**: Starts the MCP server with OAuth middleware enabled
3. **Authenticated Request**: Makes an MCP initialize request with the Bearer token
4. **Validation**: Verifies the request succeeds (HTTP 200)

This ensures that:
- Keycloak issues valid JWT tokens
- MCP server can validate JWT signatures using JWKS
- OAuth middleware correctly authenticates requests
- The full authentication flow works end-to-end

## OAuth Flows

This setup supports two OAuth flows:

### Phase 1: Client Credentials (Service-to-Service)
- **Client ID:** `mcp-client-confidential`
- **Type:** Confidential client (requires client secret)
- **Use Case:** Automated testing, backend service-to-service authentication
- **Grant Type:** `client_credentials`

### Phase 2: Authorization Code + PKCE (Browser-Based)
- **Client ID:** `mcp-client`
- **Type:** Public client (no client secret)
- **Use Case:** MCP clients like Claude Desktop
- **Grant Type:** `authorization_code` with PKCE
- **Test Credentials:**
  - Username: `testuser`
  - Password: `testpass123`

## Configuring Claude Desktop

To use this OAuth server with Claude Desktop, add to your `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "solace-broker": {
      "command": "/path/to/solace-broker-mcp",
      "args": ["--config", "/path/to/config.yaml"],
      "oauth": {
        "client_id": "mcp-client"
      }
    }
  }
}
```

**Important:** Use `mcp-client` (public client) for Claude Desktop, NOT `mcp-client-confidential`.

## Cleanup

The script automatically stops both the MCP server and Keycloak container on exit (whether the test passes or fails).
