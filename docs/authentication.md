# Authentication Guide

The Solace Broker MCP Server supports three authentication modes for MCP client to MCP server communication. Instructions on how to use these modes are provided below.

## Mode 1: No Authentication (Open Access)

**When to use:** Local development only, when you want to quickly test the MCP server without setting up authentication.

**⚠️ Security Warning:** This mode allows anyone to access your MCP server. Never use this in production or on a network-accessible machine.

### Configuration

1. Open your configuration file (`broker-config.yaml`)

2. Set the following values:

```yaml
development_mode: true
```

3. Start the MCP server

### Configuring your MCP client

#### Claude Code

If your MCP server is already running on `http://localhost:9090`, you can add it using the Claude Code CLI:

```bash
claude mcp add solace-broker --transport http http://localhost:9090/mcp
```

That's it! No authentication headers are needed.

### What happens

- All client requests are accepted automatically
- No tokens or credentials are needed
- You'll see a warning in the logs: `"authentication disabled — development mode with no dev token — not for production use"`

---

## Mode 2: Static Dev Token (Simple Authentication)

**When to use:** Local development or testing when you want basic protection without setting up a full OAuth identity provider.

**⚠️ Security Warning:** This mode uses a fixed token that doesn't expire. It's suitable for development but not recommended for production environments.

### Configuration

1. Choose a token string (e.g., `"my-secret-dev-token-123"`)

2. Open your configuration file (`broker-config.yaml`)

3. Set the following values:

```yaml
development_mode: true

client_auth:
  dev_token: "my-secret-dev-token-123"
```

**Tip:** For better security, use an environment variable instead of hardcoding the token:

```yaml
development_mode: true

client_auth:
  dev_token: "${DEV_TOKEN}"
```

Then set the environment variable before starting the server. This can be done in one of two ways. The token value can be configured in your .env file or exported as shown below:
```bash
export DEV_TOKEN="my-secret-dev-token-123"
```

4. Start the MCP server

### Configuring your MCP client

#### Claude Code

If your MCP server is already running on `http://localhost:9090`, you can add it using the Claude Code CLI:

```bash
claude mcp add --transport http solace-broker http://localhost:9090/mcp \
  -H "Authorization: Bearer my-secret-dev-token-123"
```

This will automatically configure the authentication header for you.

#### Other MCP clients

For other MCP clients or manual HTTP requests, include the token in the `Authorization` header:

```
Authorization: Bearer my-secret-dev-token-123
```

### What happens

- The server validates each request by comparing the provided token to your configured `dev_token`
- If the token matches, the request is accepted
- If the token is missing or incorrect, the request is rejected with an authentication error
- Tokens are fixed and do not expire until the user changes them
- You'll see a log message: `"using static dev token — development mode — not for production use"`

### Important notes

**Reconnect, don't re-authenticate.** If your MCP client disconnects, use the "reconnect" action — not "re-authenticate." Re-authentication triggers an OAuth browser login flow, which will fail because there is no authorization server in dev mode. Simply reconnecting will re-use the configured static token.

**Do not mix Mode 3 fields with dev mode.** If you include OAuth/JWT fields (`issuer`, `audience`, `resource_url`) in your config alongside `development_mode: true`, the server will advertise the configured authorization server in its metadata endpoint. This causes MCP clients to attempt OAuth flows that will fail, producing confusing errors like "Trusted Hosts rejected request" or "the requested endpoint does not exist." Keep your dev mode config clean — only set `dev_token` under `client_auth`.

---

## Mode 3: OAuth / JWT (Production Authentication)

**When to use:** Production deployments or any environment where you need browser-based authentication with an identity provider (IdP). This mode uses the OAuth 2.1 Authorization Code flow, allowing MCP clients (like Claude) to authenticate users via a browser login.

### Prerequisites

You need an OAuth 2.1 / OpenID Connect identity provider. Any OIDC-compliant provider will work:
- Keycloak
- Auth0
- Okta

### Configuration

1. Open your configuration file (`broker-config.yaml`)

2. Set the following values:

```yaml
development_mode: false

client_auth:
  issuer: "https://your-idp.example.com/realms/your-realm"
  audience: "solace-mcp-server"
  resource_url: "https://your-mcp-server.example.com/mcp"
```

3. Start the MCP server

### Configuration fields

| Field | Required | Description |
|-------|----------|-------------|
| `issuer` | Yes | The OIDC issuer URL of your identity provider. The server uses this to fetch the `.well-known/openid-configuration` and JWKS keys for token validation. |
| `audience` | Yes | The expected `aud` claim in access tokens. Tokens without this audience are rejected. Must match what your IdP includes in issued tokens. |
| `resource_url` | Yes | The public URL of your MCP server endpoint. Used in the OAuth Protected Resource Metadata response to tell clients where the protected resource lives. |

### Client registration methods

There are two ways for MCP clients to register with your identity provider. Both use the browser-based OAuth 2.1 Authorization Code flow with PKCE — the user authenticates via browser in both cases. The difference is how the MCP client obtains its OAuth client credentials.

| | Client Pre-Registration | Dynamic Client Registration |
|---|---|---|
| User setup | Provide client_id when adding the server | Just add the server URL |
| IdP requirements | Standard OAuth client setup | Must allow anonymous client registration |
| Best for | Locked-down IdPs, enterprise environments | Zero-config end-user experience |

#### Option A: Client pre-registration

Pre-register an OAuth client in your IdP and provide the credentials to the MCP client. This avoids the need for your IdP to support Dynamic Client Registration.

##### Claude Code

```bash
claude mcp add --transport http \
  --client-id your-client-id \
  --client-secret \
  --callback-port 8080 \
  solace-broker https://your-mcp-server.example.com/mcp
```

- `--client-id` — the client ID from your IdP
- `--client-secret` — prompts for masked input; stored securely in the system keychain
- `--callback-port` — must match a redirect URI registered in your IdP (e.g., `http://localhost:8080/callback`)

Alternatively, configure via `.mcp.json` in your project root:

```json
{
  "mcpServers": {
    "solace-broker": {
      "type": "http",
      "url": "https://your-mcp-server.example.com/mcp",
      "oauth": {
        "clientId": "your-client-id",
        "callbackPort": 8080
      }
    }
  }
}
```

Then set the client secret via the CLI:

```bash
claude mcp add-json solace-broker \
  '{"type":"http","url":"https://your-mcp-server.example.com/mcp","oauth":{"clientId":"your-client-id","callbackPort":8080}}' \
  --client-secret
```

The `--client-secret` flag prompts for masked input and stores the secret securely in the system keychain.

##### What happens

1. The MCP client connects to the server and receives a `401 Unauthorized` response
2. The client fetches the OAuth Protected Resource Metadata to discover the authorization server
3. The client **skips DCR** — it already has client credentials from your configuration
4. The client initiates an OAuth 2.1 Authorization Code flow (with PKCE) — a browser window opens for the user to log in
5. After successful login, the client receives a JWT access token and includes it in subsequent requests
6. The MCP server validates each token's signature (via the IdP's JWKS endpoint), issuer, audience, and expiry
- Tokens are automatically refreshed by the client when they expire
- You'll see a log message: `"using JWT token for authentication — production mode"`

##### IdP requirements

Create an OAuth client in your IdP with the following settings:

- **Client type:** Confidential (with client secret) or Public (PKCE-only, no secret)
- **Flow:** Authorization Code (also called "Standard flow" in Keycloak)
- **PKCE:** Required, with `S256` challenge method
- **Redirect URI:** `http://localhost:<port>/callback` — must match the `--callback-port` used in the CLI command
- **Scopes:** At minimum, `openid`

#### Option B: Dynamic Client Registration (zero-config)

The MCP client discovers the authorization server automatically and registers itself at runtime. No pre-shared client credentials are needed.

##### Claude Code

```bash
claude mcp add solace-broker --transport http https://your-mcp-server.example.com/mcp
```

When you first use the server, Claude will open a browser window for you to authenticate with your identity provider.

##### What happens

1. The MCP client connects to the server and receives a `401 Unauthorized` response with a `WWW-Authenticate` header containing a `resource_metadata` URL
2. The client fetches the OAuth Protected Resource Metadata (RFC 9728) from `/.well-known/oauth-protected-resource`
3. The metadata tells the client which authorization server to use
4. The client performs Dynamic Client Registration (RFC 7591) with the authorization server to register itself as an OAuth client
5. The client initiates an OAuth 2.1 Authorization Code flow (with PKCE) — a browser window opens for the user to log in with the identity provider
6. After successful login, the client receives a JWT access token and includes it in subsequent requests
7. The MCP server validates each token's signature (via the IdP's JWKS endpoint), issuer, audience, and expiry
- Tokens are automatically refreshed by the client when they expire
- You'll see a log message: `"using JWT token for authentication — production mode"`

##### IdP requirements

Your IdP must support anonymous Dynamic Client Registration (RFC 7591):

- **Expose a registration endpoint** — typically at `<issuer>/clients-registrations/openid-connect` or similar (advertised in the authorization server metadata as `registration_endpoint`)
- **Allow anonymous registration** — the MCP client has no pre-existing credentials with the IdP
- **Allow localhost redirect URIs** — the MCP client uses ephemeral localhost ports (e.g., `http://localhost:<port>/callback`) to receive authorization codes
- **Support PKCE** with `S256` challenge method (required by OAuth 2.1)

##### Client registration policies

Most IdPs enforce policies on what dynamically registered clients can do. The following must be permitted:

| Policy area | Requirement |
|-------------|-------------|
| **Allowed scopes** | The IdP must allow the scopes that get assigned to dynamically registered clients. At minimum, `openid` must be permitted. If your IdP has internal scopes (e.g., `service_account` in Keycloak), those must also be allowed. |
| **Trusted hosts** | If your IdP runs in a container or on a different network, ensure the host trust policy does not reject registration requests based on source IP. The request may arrive from a container bridge IP rather than localhost. |
| **Redirect URI validation** | Dynamically registered clients specify `http://localhost:*` redirect URIs. The IdP must allow these. |

### Setting up your identity provider

The steps below apply to both registration methods. Refer to your IdP's documentation for specifics.

#### 1. Configure an audience mapper

The MCP server validates the `aud` (audience) claim in every access token. Your IdP must include your configured `audience` value in issued tokens.

If you use DCR, the audience mapper must be attached to a scope or mechanism that applies to **all clients in the realm** — not just specific pre-registered ones. This is the most common pitfall: audience mappers configured only on one client will not apply to tokens issued to dynamically registered clients.

How to achieve this depends on your IdP:

- **Keycloak:** Add an audience protocol mapper to a built-in client scope (e.g., `basic`) that Keycloak assigns to all clients regardless of registration method.
- **Auth0:** Configure the audience as an API resource. Tokens issued for that API will include the audience automatically.
- **Okta:** Configure the audience in the authorization server settings.

For example, if your MCP server config has `audience: "solace-mcp-server"`, the issued JWT must contain:
```json
{
  "aud": "solace-mcp-server"
}
```

#### 2. Create users

Create user accounts in your IdP that will authenticate via the browser login flow. These users will log in with their individual credentials when the MCP client opens a browser window during the OAuth flow.

---

## Troubleshooting

### "authentication disabled" warning in Mode 1

- Check that `dev_token` is actually set to a non-empty value
- Verify that any environment variables (e.g., `${DEV_TOKEN}`) are properly exported

### "401 Unauthorized" errors in Mode 2

- Verify that your client is sending the `Authorization: Bearer <token>` header
- Confirm the token value matches exactly what's in your configuration (no extra spaces or quotes)
- Check that `development_mode: true` is set in your config

### Token doesn't seem to be loaded

- If using environment variables like `${DEV_TOKEN}`, make sure to export the variable before starting the server
- Check the server logs for any configuration parsing errors

### Re-authentication errors in Claude (Modes 1 and 2)

- In Modes 1 and 2, the MCP server stays connected and there is no need to re-authenticate. If a re-authentication is attempted, you will see authentication errors because there is no authorization server configured to handle the OAuth flow

### "failed to connect to identity provider" on server startup

- The MCP server connects to the issuer's `/.well-known/openid-configuration` at startup to fetch JWKS keys
- Verify the `issuer` URL is correct and reachable from the server
- If using Keycloak locally, ensure the container is running and healthy before starting the MCP server

### "403 Forbidden" with a valid token

- Verify the audience mapper is configured in your IdP so the `aud` claim matches your `audience` config value
- Decode your JWT (e.g., at jwt.io) to inspect the actual `aud` claim

### Browser login window doesn't appear

- Ensure the MCP client supports OAuth (Claude Code and Claude Desktop do)
- Verify the `resource_url` matches the URL the client is connecting to
- Check that the `/.well-known/oauth-protected-resource` endpoint returns valid metadata:
  ```bash
  curl http://localhost:9091/.well-known/oauth-protected-resource
  ```

### "Allowed Client Scopes rejected request to client-registration service"

This error occurs during Dynamic Client Registration when the IdP's client registration policy blocks one or more scopes. Common causes:

- The `openid` scope is not recognized as a client scope in your IdP (some IdPs handle it at the protocol level). Create a placeholder `openid` client scope and add it to the allowed list.
- Internal IdP scopes (e.g., `service_account`) are automatically assigned during registration but not in the allowed list. Add them to the registration policy.
- Custom scopes are not in the allowed list. If using custom scopes, ensure they are permitted in the registration policy.

Check your IdP's logs for the specific scope being rejected.

### "Trusted Hosts rejected request to client-registration service"

This error occurs during Dynamic Client Registration when the IdP rejects the request based on the source host. Common causes:

- The IdP is running in a container (e.g., Docker/Podman), so requests arrive from the container bridge gateway IP, not localhost. Disable source-host matching in the registration policy or add the gateway IP to the trusted hosts list.
- The redirect URIs in the registration request don't match the trusted hosts. Ensure `localhost` and `127.0.0.1` are permitted.

### "invalid_redirect_uri" with pre-registered client

- The redirect URI in the authorization request doesn't match what's registered in your IdP
- Verify that `http://localhost:<port>/callback` is in the client's valid redirect URIs, where `<port>` matches your `--callback-port`
- Some IdPs require an exact match — wildcards may not be supported for pre-registered clients

### "invalid_client" with pre-registered client

- The `--client-id` doesn't match any client in your IdP
- If using a confidential client, the client secret may be incorrect — re-add the server with `claude mcp add ... --client-secret` to re-enter it
- Verify the client is not disabled or expired in your IdP