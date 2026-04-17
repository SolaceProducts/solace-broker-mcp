# OAuth Integration Tests

Integration test for OAuth authentication middleware using real Keycloak OIDC provider.

## Prerequisites

1. Keycloak must be running and configured:
   ```bash
   ./setup-keycloak.sh
   ```

2. Test configuration (optional):
   - Edit `test-config.yaml` to customize broker URL or port
   - Set environment variables for broker credentials:
     ```bash
     export TEST_BROKER_USERNAME=admin
     export TEST_BROKER_PASSWORD=admin
     ```
   - Default values are used if not set

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

The integration test verifies the complete OAuth authentication flow:

1. **Token Acquisition**: Gets a real JWT access token from Keycloak using client credentials grant
2. **Server Startup**: Starts the MCP server with OAuth middleware enabled
3. **Authenticated Request**: Makes an MCP initialize request with the Bearer token
4. **Validation**: Verifies the request succeeds (HTTP 200)

This ensures that:
- Keycloak issues valid JWT tokens
- MCP server can validate JWT signatures using JWKS
- OAuth middleware correctly authenticates requests
- The full authentication flow works end-to-end

## Cleanup

The script automatically stops both the MCP server and Keycloak container on exit (whether the test passes or fails).
