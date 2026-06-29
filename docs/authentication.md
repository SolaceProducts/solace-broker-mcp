# Authentication Guide

The Solace Event Broker MCP Server supports three authentication modes for MCP client-to-server communication. This guide describes how to configure each mode.

## Table of Contents

- [Mode 1: No Authentication (`mode: disabled`)](#mode-1-no-authentication-mode-disabled)
- [Mode 2: Static Dev Token (`mode: static`)](#mode-2-static-dev-token-mode-static)
- [Mode 3: OAuth / JWT (`mode: oauth`)](#mode-3-oauth--jwt-mode-oauth)
  - [Choose a client registration method](#choose-a-client-registration-method)
  - [Step 1: Set up the identity provider](#step-1-set-up-the-identity-provider)
  - [Step 2: Configure the MCP server](#step-2-configure-the-mcp-server)
  - [Step 3: Start the MCP server](#step-3-start-the-mcp-server)
  - [Step 4: Add the server to Claude Code](#step-4-add-the-server-to-claude-code)
- [Troubleshooting](#troubleshooting)

## Mode 1: No Authentication (`mode: disabled`)

**When to use:** Local development and testing when authentication is not required.

**Security Warning:** This mode provides unrestricted access to the MCP server. Not suitable for production or network-accessible deployments.

### Configuration

1. Open the configuration file (`broker-config.yaml`)

2. Set the following values:

```yaml
mcp_client_auth:
  mode: disabled
```

3. Start the MCP server

### Configuring the MCP Client

#### Claude Code

With the MCP server running on `http://localhost:9090`, add it using the Claude Code CLI:

```bash
claude mcp add solace-broker --transport http http://localhost:9090/mcp
```

No authentication headers required.

### What Happens

- All client requests are accepted automatically
- No tokens or credentials are needed
- The server binds `127.0.0.1` only by default, so the unauthenticated listener is not reachable from the network. To expose it on another interface you must set `listen_address` **and** `allow_remote_unauthenticated: true` — without the override, a non-loopback `listen_address` is refused at startup. See [Configuration](configuration.md#server-settings).
- A prominent banner appears in the logs at startup:
  ```
  ============================================================
    INSECURE MODE: mcp_client_auth.mode = disabled
    Client authentication is DISABLED.
    All MCP requests pass through without verification.
    This is development mode — NOT FOR PRODUCTION USE.
  ============================================================
  ```

---

## Mode 2: Static Dev Token (`mode: static`)

**When to use:** Local development or testing when basic protection is required without a full OAuth identity provider.

**Security Warning:** This mode uses a fixed token that does not expire. Suitable for development but not recommended for production environments.

### Configuration

1. Choose a token string (for example, `"my-secret-dev-token-123"`)

2. Open the configuration file (`broker-config.yaml`)

3. Set the following values:

```yaml
mcp_client_auth:
  mode: static
  dev_token: "my-secret-dev-token-123"
```

**Note:** For better security, use an environment variable instead of hardcoding the token:

```yaml
mcp_client_auth:
  mode: static
  dev_token: "${DEV_TOKEN}"
```

Set the environment variable before starting the server using one of these methods:

**Option 1: .env file**
Add to the `.env` file:
```env
DEV_TOKEN=my-secret-dev-token-123
```

**Option 2: Export directly**
```bash
export DEV_TOKEN="my-secret-dev-token-123"
```

4. Start the MCP server

### Configuring the MCP Client

#### Claude Code

With the MCP server running on `http://localhost:9090`, add it using the Claude Code CLI:

```bash
claude mcp add solace-broker --transport http http://localhost:9090/mcp \
  -H "Authorization: Bearer my-secret-dev-token-123"
```

#### Other MCP Clients

For other MCP clients or manual HTTP requests, include the token in the `Authorization` header:

```
Authorization: Bearer my-secret-dev-token-123
```

### What Happens

- The server validates each request by comparing the provided token to the configured `dev_token`
- If the token matches, the request is accepted
- If the token is missing or incorrect, the request is rejected with an authentication error
- Tokens are fixed and do not expire until the user changes them
- The server binds `127.0.0.1` only by default. Set `listen_address` explicitly to expose it on another interface (no override is required for `static`, since requests still need the token). See [Configuration](configuration.md#server-settings).
- **Cleartext caveat:** the dev token is a long-lived shared bearer token. On a non-loopback bind **without TLS** it travels in plaintext and can be sniffed and replayed for the same broker-admin-backed access — enable TLS (`tls_cert_file`/`tls_key_file`) whenever `static` is network-reachable. The server emits a startup WARN in this case.
- A prominent banner appears in the logs at startup:
  ```
  ============================================================
    INSECURE MODE: mcp_client_auth.mode = static
    Authentication uses a shared static dev token.
    This is development mode — NOT FOR PRODUCTION USE.
  ============================================================
  ```

### Important Notes

**Important: Reconnect, do not re-authenticate.** If the MCP client disconnects, access the server in your client's MCP server list and choose "reconnect" (not "re-authenticate"). Re-authenticating triggers an OAuth flow that fails in dev mode. Reconnecting re-uses the configured static token.
---

## Mode 3: OAuth / JWT (`mode: oauth`)

**When to use:** Production deployments or any environment where browser-based authentication with an identity provider (IdP) is required. This mode uses the OAuth 2.1 Authorization Code flow with PKCE, allowing MCP clients (like Claude) to authenticate users via a browser login.

### Choose a Client Registration Method

Before starting, decide how the MCP client registers with the identity provider:

| | Option A: Client pre-registration | Option B: Dynamic Client Registration |
|---|---|---|
| User setup | Provide `--client-id` when adding the server | Add the server URL — no credentials needed |
| IdP requirements | Standard OAuth client setup | Must support anonymous DCR (RFC 7591) |
| Best for | Most setups; more control | Zero-config end-user experience |

Both options use the same OAuth 2.1 Authorization Code flow with PKCE. The difference: Option A requires pre-registering a client ID (and optionally a client secret). Option B uses Dynamic Client Registration (DCR) to obtain a client ID at runtime—no client secret needed.

### Step 1: Set Up the Identity Provider

An OAuth 2.1 / OpenID Connect identity provider such as Keycloak, Auth0, or Okta is required. This guide uses Keycloak for examples, but any OIDC-compliant provider works.

#### 1.1 Create a Realm or Tenant

Most IdPs organize clients and users into an isolated namespace — called a realm, tenant, or organization depending on the provider. Create one dedicated to the MCP server deployment.

> **Keycloak:** Start a local instance with Docker:
> ```bash
> docker run -p 127.0.0.1:8080:8080 \
>   -e KC_BOOTSTRAP_ADMIN_USERNAME=admin \
>   -e KC_BOOTSTRAP_ADMIN_PASSWORD=admin \
>   quay.io/keycloak/keycloak:latest start-dev
> ```
> Then open the Admin Console at `http://localhost:8080/admin`, log in with `admin` / `admin`, click the realm dropdown in the top-left → **Create realm** → set a **Realm name** → **Create**. The built-in `master` realm can be used for quick local testing, but a dedicated realm is recommended.
>
> **Tip:** To automate steps 1.1–1.4, the project includes a setup script at `test/oauth/setup-keycloak.sh` that starts Keycloak, creates the realm, and configures the audience mapper, OAuth clients, DCR policies, and a test user — all via Terraform. Run it from the project root: `cd test/oauth && ./setup-keycloak.sh`.

#### 1.2 Configure an Audience Mapper

The MCP server validates the `aud` claim in every access token. Configure the IdP to include the chosen audience value in issued tokens.

The mapper must apply globally to all clients — not just specific pre-registered ones. This is especially important for Option B (Dynamic Client Registration): a mapper scoped to a single client does not apply to tokens issued to dynamically registered clients.

> **Keycloak:** Add the mapper to the built-in `basic` client scope, which Keycloak assigns to all clients regardless of how they were registered. Go to **Client Scopes** → **basic** → **Mappers** tab → **Add mapper** → **By configuration** → **Audience**. Fill in:
> - **Name:** any descriptive name (for example, `solace-mcp-audience`)
> - **Included Custom Audience:** the chosen audience value (for example, `solace-mcp-server`)
> - **Included Client Audience:** leave empty
> - **Add to access token:** ON
>
> Use **Included Custom Audience** for a free-form string. **Included Client Audience** is only for referencing an existing Keycloak client by its Client ID.

#### 1.3 Register an OAuth Client (Option A Only)

Skip this step when using Option B (Dynamic Client Registration).

Create a client in the IdP with the following settings:

- **Client ID:** any name (e.g. `mcp-client`) — pass this as `--client-id` in step 4
- **Flow:** Authorization Code with PKCE
- **PKCE challenge method:** `S256`
- **Client type:** Public (no secret) or Confidential (with secret) — a public client is recommended for MCP clients like Claude Code and Claude Desktop, since they run on a user's machine where a client secret cannot be stored securely. For Option B (Dynamic Client Registration), the MCP client should always be registered as public.
- **Redirect URI:** `http://localhost:<port>/callback`, where `<port>` matches the `--callback-port` used when adding the server to Claude Code

> **Keycloak:** **Clients** → **Create client** → set a **Client ID** (e.g. `mcp-client`) → click **Next**.
> - **Client authentication:** leave **OFF** for a public client (no secret required); turn **ON** for a confidential client (requires `--client-secret`)
>
> Enable **Standard flow** and disable all other flows. Enable **Require PKCE** and set the **PKCE Method** to `S256` → click **Next**. Under **Login settings**, set **Valid redirect URIs** to `http://localhost:*` → **Save**.

#### 1.4 Create Users

Create user accounts in the IdP. These users log in via the browser window that opens during the OAuth flow.

> **Keycloak:** **Users** → **Create new user** → enter a username → **Create**. Go to the **Credentials** tab → **Set password**, enter a password, and turn **Temporary** off.

### Step 2: Configure the MCP Server

Open `broker-config.yaml` and set:

```yaml
mcp_client_auth:
  mode: oauth
  issuer: "https://your-idp.example.com/realms/your-realm"
  audience: "solace-mcp-server"
  resource_url: "https://your-mcp-server.example.com/mcp"
```

See the Keycloak configuration below for an example.

The `audience` value must exactly match the value configured in step 1.2. Set `resource_url` to the externally reachable URL of the MCP endpoint — this is advertised to clients for OAuth discovery, so it must be the public-facing URL, not the server's internal bind address (these differ when running behind a reverse proxy or ingress).

| Field | Description |
|-------|-------------|
| `issuer` | The OIDC issuer URL of the IdP. The server fetches JWKS keys from here for token validation. |
| `audience` | Must exactly match the audience value configured in the IdP in step 1.2. |
| `resource_url` | The public URL of the MCP server endpoint, advertised to MCP clients for OAuth discovery. |

> **Keycloak:** The issuer URL follows the pattern `https://<host>:<port>/realms/<realm-name>`. For example, Keycloak running locally on port 8443 with TLS and a realm named `solace`:
> ```yaml
> mcp_client_auth:
>   mode: oauth
>   issuer: "https://localhost:8443/realms/solace"
>   audience: "solace-mcp-server"
>   resource_url: "http://localhost:9090/mcp"
> ```

> **Note:** Under `mode: oauth` the validator enforces `https://` on the `issuer` URL. When running Keycloak locally for testing, terminate TLS in front of it (for example, via Caddy or a reverse proxy) or run Keycloak with a TLS cert. The `resource_url` may remain `http://` for local-bind testing.

### Step 3: Start the MCP Server

Run the server:

```bash
go run ./cmd/server
```

### Step 4: Add the Server to Claude Code

#### Option A: Client Pre-Registration

With the MCP server running on `http://localhost:9090`, add it using the Claude Code CLI:

```bash
claude mcp add solace-broker --transport http http://localhost:9090/mcp \
  --client-id mcp-client \
  --callback-port 8081
```

- `--client-id` — the client ID registered in step 1.3
- `--callback-port` — any free port; `http://localhost:<port>/callback` must be in the client's registered redirect URIs
- `--client-secret` — add this flag only if a confidential client was created in step 1.3; prompts for the secret (input is hidden for security) and stores the secret in the system keychain

Alternatively, configure via `.mcp.json` in the project root:

```json
{
  "mcpServers": {
    "solace-broker": {
      "type": "http",
      "url": "http://localhost:9090/mcp",
      "oauth": {
        "clientId": "mcp-client",
        "callbackPort": 8081
      }
    }
  }
}
```

#### Option B: Dynamic Client Registration

With the MCP server running on `http://localhost:9090`, add it using the Claude Code CLI:

```bash
claude mcp add solace-broker --transport http http://localhost:9090/mcp
```

A browser window opens on first use for user login. The IdP must support anonymous Dynamic Client Registration (RFC 7591).

> **Keycloak:** Keycloak requires three additional policy changes to allow DCR:
>
> **1. Create an `openid` client scope placeholder** — Keycloak handles `openid` at the protocol level but its DCR policy checks for a scope by that name. Go to **Client Scopes** → **Create client scope** → **Name:** `openid`, **Protocol:** `openid-connect` → **Save**.
>
> **2. Update the Allowed Client Scopes policy** — Go to **Clients** → **Client Registration**  tab → under **Anonymous Access Policies**, click **Allowed Client Scopes** → add `openid` and `service_account` to the allowed list → **Save**.
>
> **3. Update the Trusted Hosts policy** — If Keycloak is running in a container, DCR requests arrive from the container bridge IP rather than `localhost`. Go to **Realm Settings** → **Client Registration** → **Client Registration Policies** tab → under **Anonymous Access Policies**, click **Trusted Hosts** → turn off **Host Sending Registration Request Must Match** → **Save**.

### How It Works

**Authentication flow (`mode: oauth`):**

```
 AI Agent              MCP Server                 OAuth IdP            Solace Broker
(Claude Code)                                  (Keycloak etc.)
    │                       │                         │                      │
    │ 1. MCP request (no token)                       │                      │
    │──────────────────────▶│                         │                      │
    │ 2. 401 + resource metadata pointer              │                      │
    │◀──────────────────────│                         │                      │
    │ 3. GET /.well-known/oauth-protected-resource    │                      │
    │──────────────────────▶│  (discovers issuer)     │                      │
    │◀──────────────────────│                         │                      │
    │ 4. Register client (DCR) or use pre-registered client                  │
    │────────────────────────────────────────────────▶│                      │
    │ 5. Browser login — Authorization Code + PKCE     │                      │
    │◀───────────────────────────────────────────────▶│                      │
    │ 6. Access token (JWT, aud = configured audience) │                      │
    │◀─────────────────────────────────────────────────                      │
    │ 7. MCP request + Bearer JWT                      │                      │
    │──────────────────────▶│                         │                      │
    │                       │ 8. Validate JWT:        │                      │
    │                       │    signature (JWKS), iss, aud, exp              │
    │                       │────────fetch JWKS──────▶│                      │
    │                       │◀────────keys────────────│                      │
    │                       │ 9. Tool call → SEMP request                    │
    │                       │    (broker auth: basic or bearer, separate)     │
    │                       │────────────────────────────────────────────────▶
    │                       │◀───────────10. SEMP response───────────────────│
    │ 11. Tool result       │                         │                      │
    │◀──────────────────────│                         │                      │
```

> **Two independent auth legs.** Client→server auth (steps 1–8, the JWT above) is
> distinct from server→broker auth (step 9), which uses each broker's configured
> `auth.mode` (`basic` or `bearer`). Broker-bound OAuth via RFC 8693 token exchange
> (the `broker_oauth:` config block) is **schema-only** in the current release and
> not yet wired — see the [CHANGELOG](../CHANGELOG.md).

The numbered steps in detail:

1. Claude Code connects to the MCP server and receives a `401 Unauthorized` response
2. Claude Code fetches the OAuth Protected Resource Metadata from `/.well-known/oauth-protected-resource` to discover the authorization server
3. **Option A:** Claude Code uses the pre-registered client credentials and skips DCR
   **Option B:** Claude Code performs Dynamic Client Registration (RFC 7591) to obtain a client ID at runtime
4. A browser window opens for the user to log in with IdP credentials
5. After login, Claude Code receives a JWT access token and includes it in all subsequent requests
6. The MCP server validates each token's signature (via JWKS), issuer, audience, and expiry — tokens are automatically refreshed when they expire

On success, the server logs: `"using JWT token for authentication — production mode"`

---

## Troubleshooting

### Banner appears at startup ("INSECURE MODE")

This is expected for `mode: disabled` and `mode: static`. The banner is the deliberate signal that the server is running without production-grade auth. If production mode was intended, switch to `mode: oauth`.

### Cannot reach the server from another host (Modes 1 and 2)

Under `mode: disabled` and `mode: static` the server binds `127.0.0.1` only by default, so it is reachable from the local host but not the network. Check the startup log line (the `bind_address` field) for the effective host:port. To bind another interface, set `listen_address` in the config (for `mode: disabled` this also requires `allow_remote_unauthenticated: true`). See [Configuration](configuration.md#server-settings).

### "401 Unauthorized" errors in Mode 2

- Verify that your client is sending the `Authorization: Bearer <token>` header
- Confirm the token value matches exactly what's in your configuration (no extra spaces or quotes)
- Check that `mcp_client_auth.mode: static` and `mcp_client_auth.dev_token` are both set in your config

### Token Does Not Load

- If using environment variables like `${DEV_TOKEN}`, export the variable before starting the server
- Check the server logs for configuration parsing errors

### Re-Authentication Errors in Claude (Modes 1 and 2)

- Access the server in your client's MCP server list and choose "reconnect" (not "re-authenticate")
- Modes 1 and 2 do not have an authorization server configured to handle OAuth flows

### "failed to connect to identity provider" on server startup

- Verify the `issuer` URL is correct and reachable from the server
- If using Keycloak locally, ensure the container is running and healthy before starting the MCP server
- The MCP server connects to the issuer's `/.well-known/openid-configuration` at startup to fetch JWKS keys

### "403 Forbidden" with a valid token

- Verify the audience mapper is configured in the IdP so the `aud` claim matches the `audience` config value
- Decode the JWT (for example, at jwt.io) to inspect the actual `aud` claim

### Browser Login Window Does Not Appear

- Verify the MCP client supports OAuth (Claude Code and Claude Desktop do)
- Check that the `resource_url` matches the URL the client is connecting to
- Verify the `/.well-known/oauth-protected-resource` endpoint returns valid metadata:
  ```bash
  curl http://localhost:9090/.well-known/oauth-protected-resource
  ```

### "Allowed Client Scopes rejected request to client-registration service"

This error occurs during Dynamic Client Registration when the IdP's client registration policy blocks one or more scopes. Common causes:

- The `openid` scope is not recognized as a client scope in the IdP (some IdPs handle it at the protocol level). Create a placeholder `openid` client scope and add it to the allowed list.
- Internal IdP scopes (for example, `service_account`) are automatically assigned during registration but not in the allowed list. Add them to the registration policy.
- Custom scopes are not in the allowed list. If using custom scopes, ensure they are permitted in the registration policy.

Check the IdP's logs for the specific scope being rejected.

### "Trusted Hosts rejected request to client-registration service"

This error occurs during Dynamic Client Registration when the IdP rejects the request based on the source host. Common causes:

- The IdP is running in a container (for example, Docker/Podman), so requests arrive from the container bridge gateway IP, not localhost. Disable source-host matching in the registration policy or add the gateway IP to the trusted hosts list.
- The redirect URIs in the registration request do not match the trusted hosts. Ensure `localhost` and `127.0.0.1` are permitted.

### "invalid_redirect_uri" with pre-registered client

- The redirect URI in the authorization request does not match what is registered in the IdP
- Verify that `http://localhost:<port>/callback` is in the client's valid redirect URIs, where `<port>` matches `--callback-port`
- Some IdPs require an exact match — wildcards may not be supported for pre-registered clients

### "invalid_client" with pre-registered client

- The `--client-id` does not match any client in the IdP
- If using a confidential client, the client secret may be incorrect — re-add the server with `claude mcp add ... --client-secret` to re-enter it
- Verify the client is not disabled or expired in the IdP