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

**When to use:** Production deployments or any environment where you need browser-based authentication with an identity provider (IdP). This mode uses the OAuth 2.1 Authorization Code flow with PKCE, allowing MCP clients (like Claude) to authenticate users via a browser login.

### Choose a client registration method

Before starting, decide how your MCP client will register with your identity provider:

| | Option A: Client pre-registration | Option B: Dynamic Client Registration |
|---|---|---|
| User setup | Provide `--client-id` when adding the server | Just add the server URL — no credentials needed |
| IdP requirements | Standard OAuth client setup | Must support anonymous DCR (RFC 7591) |
| Best for | Most setups; more control | Zero-config end-user experience |

Both options use the same browser-based OAuth 2.1 Authorization Code flow with PKCE. The difference is in how the MCP client obtains its client credentials (and whether it uses DCR) — Option A may also involve a client secret for confidential clients, while Option B never does.

### Step 1: Set up your identity provider

You need an OAuth 2.1 / OpenID Connect identity provider. Note that any OIDC-compliant provider will work (configuration examples specific to Keycloak are provided).

#### 1.1 Create a realm or tenant

Most IdPs organize clients and users into an isolated namespace — called a realm, tenant, or organization depending on the provider. Create one dedicated to your MCP server deployment.

> **Keycloak:** Start a local instance with Docker:
> ```bash
> docker run -p 127.0.0.1:8080:8080 \
>   -e KC_BOOTSTRAP_ADMIN_USERNAME=admin \
>   -e KC_BOOTSTRAP_ADMIN_PASSWORD=admin \
>   quay.io/keycloak/keycloak:latest start-dev
> ```
> Then open the Admin Console at `http://localhost:8080/admin`, log in with `admin` / `admin`, click the realm dropdown in the top-left → **Create realm** → set a **Realm name** → **Create**. You can use the built-in `master` realm for quick local testing, but a dedicated realm is recommended.
>
> **Tip:** To automate steps 1.1–1.4, the project includes a setup script at `test/e2e/oauth/setup-keycloak.sh` that starts Keycloak, creates the realm, and configures the audience mapper, OAuth clients, DCR policies, and a test user — all via Terraform. Run it from the project root: `cd test/e2e/oauth && ./setup-keycloak.sh`.

#### 1.2 Configure an audience mapper

The MCP server validates the `aud` claim in every access token. Configure your IdP to include your chosen audience value in issued tokens.

The mapper must apply globally to all clients — not just specific pre-registered ones. This is especially important for Option B (Dynamic Client Registration): a mapper scoped to a single client will not apply to tokens issued to dynamically registered clients.

> **Keycloak:** Add the mapper to the built-in `basic` client scope, which Keycloak assigns to all clients regardless of how they were registered. Go to **Client Scopes** → **basic** → **Mappers** tab → **Add mapper** → **By configuration** → **Audience**. Fill in:
> - **Name:** any descriptive name (e.g., `solace-mcp-audience`)
> - **Included Custom Audience:** your chosen audience value (e.g., `solace-mcp-server`)
> - **Included Client Audience:** leave empty
> - **Add to access token:** ON
>
> Use **Included Custom Audience** for a free-form string. **Included Client Audience** is only for referencing an existing Keycloak client by its Client ID.

#### 1.3 Register an OAuth client (Option A only)

Skip this step if you are using Option B (Dynamic Client Registration).

Create a client in your IdP with the following settings:

- **Client ID:** any name (e.g. `mcp-client`) — you will pass this as `--client-id` in step 4
- **Flow:** Authorization Code with PKCE
- **PKCE challenge method:** `S256`
- **Client type:** Public (no secret) or Confidential (with secret) — a public client is recommended for MCP clients like Claude Code and Claude Desktop, since they run on a user's machine where a client secret cannot be stored securely. For Option B (Dynamic Client Registration), the MCP client should always be registered as public.
- **Redirect URI:** `http://localhost:<port>/callback`, where `<port>` matches the `--callback-port` you will use when adding the server to Claude Code

> **Keycloak:** **Clients** → **Create client** → set a **Client ID** (e.g. `mcp-client`) → click **Next**.
> - **Client authentication:** leave **OFF** for a public client (no secret required); turn **ON** for a confidential client (requires `--client-secret`)
>
> Enable **Standard flow** and disable all other flows. Enable **Require PKCE** and set the **PKCE Method** to `S256` → click **Next**. Under **Login settings**, set **Valid redirect URIs** to `http://localhost:*` → **Save**.

#### 1.4 Create users

Create user accounts in your IdP. These users will log in via the browser window that opens during the OAuth flow.

> **Keycloak:** **Users** → **Create new user** → enter a username → **Create**. Go to the **Credentials** tab → **Set password**, enter a password, and turn **Temporary** off.

### Step 2: Configure the MCP server

Open your `broker-config.yaml` and set:

```yaml
development_mode: false

client_auth:
  issuer: "https://your-idp.example.com/realms/your-realm"
  audience: "solace-mcp-server"
  resource_url: "https://your-mcp-server.example.com/mcp"
```

Refer to the Keycloack configuration below for an example.

The `audience` value must exactly match what you configured in step 1.2. Set `resource_url` to the externally reachable URL of your MCP endpoint — this is advertised to clients for OAuth discovery, so it must be the public-facing URL, not the server's internal bind address (these differ when running behind a reverse proxy or ingress).

| Field | Description |
|-------|-------------|
| `issuer` | The OIDC issuer URL of your IdP. The server fetches JWKS keys from here for token validation. |
| `audience` | Must exactly match the audience value configured in your IdP in step 1.2. |
| `resource_url` | The public URL of your MCP server endpoint, advertised to MCP clients for OAuth discovery. |

> **Keycloak:** The issuer URL follows the pattern `http://<host>:<port>/realms/<realm-name>`. For example, Keycloak running locally on port 8080 with a realm named `solace`:
> ```yaml
> development_mode: false
>
> client_auth:
>   issuer: "http://localhost:8080/realms/solace"
>   audience: "solace-mcp-server"
>   resource_url: "http://localhost:9090/mcp"
> ```

### Step 3: Start the MCP server

```bash
go run ./cmd/server
```

### Step 4: Add the server to Claude Code

#### Option A: Client pre-registration

If your MCP server is already running on `http://localhost:9090`, you can add it using the Claude Code CLI:

```bash
claude mcp add --transport http \
  --client-id mcp-client \
  --callback-port 8081 \
  solace-broker http://localhost:9090/mcp
```

- `--client-id` — the client ID you registered in step 1.3
- `--callback-port` — any free port; `http://localhost:<port>/callback` must be in the client's registered redirect URIs
- `--client-secret` — add this flag only if you created a confidential client in step 1.3; prompts for masked input and stores the secret in the system keychain

Alternatively, configure via `.mcp.json` in your project root:

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

If your MCP server is already running on `http://localhost:9090`, you can add it using the Claude Code CLI:

```bash
claude mcp add --transport http solace-broker http://localhost:9090/mcp
```

A browser window will open for you to log in on first use. Your IdP must support anonymous Dynamic Client Registration (RFC 7591).

> **Keycloak:** Keycloak requires three additional policy changes to allow DCR.
>
> **1. Create an `openid` client scope placeholder** — Keycloak handles `openid` at the protocol level but its DCR policy checks for a scope by that name. Go to **Client Scopes** → **Create client scope** → **Name:** `openid`, **Protocol:** `openid-connect` → **Save**.
>
> **2. Update the Allowed Client Scopes policy** — Go to **Clients** → **Client Registration**  tab → under **Anonymous Access Policies**, click **Allowed Client Scopes** → add `openid` and `service_account` to the allowed list → **Save**.
>
> **3. Update the Trusted Hosts policy** — If Keycloak is running in a container, DCR requests arrive from the container bridge IP rather than `localhost`. Go to **Realm Settings** → **Client Registration** → **Client Registration Policies** tab → under **Anonymous Access Policies**, click **Trusted Hosts** → turn off **Host Sending Registration Request Must Match** → **Save**.

### How it works

1. Claude Code connects to the MCP server and receives a `401 Unauthorized` response
2. Claude Code fetches the OAuth Protected Resource Metadata from `/.well-known/oauth-protected-resource` to discover the authorization server
3. **Option A:** Claude Code uses the pre-registered client credentials and skips DCR
   **Option B:** Claude Code performs Dynamic Client Registration (RFC 7591) to obtain a client ID at runtime
4. A browser window opens for the user to log in with their IdP credentials
5. After login, Claude Code receives a JWT access token and includes it in all subsequent requests
6. The MCP server validates each token's signature (via JWKS), issuer, audience, and expiry — tokens are automatically refreshed when they expire

You'll see this in the server logs on success: `"using JWT token for authentication — production mode"`

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