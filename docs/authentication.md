# Authentication Guide

The Solace Event Broker MCP Server supports three authentication modes for MCP client-to-server communication. This guide describes how to configure each mode.

## Table of Contents

- [Mode 1: No Authentication (`mode: disabled`)](#mode-1-no-authentication-mode-disabled)
- [Mode 2: Static Dev Token (`mode: static`)](#mode-2-static-dev-token-mode-static)
- [Mode 3: OAuth / JWT (`mode: oauth`)](#mode-3-oauth--jwt-mode-oauth)
  - [Choose a client registration method](#choose-a-client-registration-method)
  - [Step 1: Set up the identity provider](#step-1-set-up-the-identity-provider)
  - [Step 2: Configure the MCP server](#step-2-configure-the-mcp-server)
  - [Step 2b: Configure broker OAuth (Hop 2)](#step-2b-configure-broker-oauth-hop-2)
  - [TLS for the MCP server's own listener](#tls-for-the-mcp-servers-own-listener)
  - [Tool authorization (claim-based RBAC)](#tool-authorization-claim-based-rbac)
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
> **Reference:** [`test/e2e-oauth/realm-export.json`](../test/e2e-oauth/realm-export.json) is a concrete, working example of this shape (realm, audience mappers, OAuth clients, test users) — test-only, not a production template, but useful to see the pieces fit together.

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
>   resource_url: "https://localhost:9090/mcp"
> ```

> **Note:** Under `mode: oauth` the validator enforces `https://` on **both** the `issuer` and `resource_url` URLs (an `http://` value is rejected at startup). When running Keycloak locally for testing, terminate TLS in front of it (for example, via Caddy or a reverse proxy) or run Keycloak with a TLS cert. `resource_url` is the externally advertised identifier for OAuth discovery, so it must be `https://` even when the MCP server's own listener is plaintext behind an upstream terminator — see [TLS for the MCP server's own listener](#tls-for-the-mcp-servers-own-listener) below.

### Step 2b: Configure broker OAuth (Hop 2)

This step is only needed if one or more brokers should use `auth.mode: oauth` instead of `basic`/`bearer`. Under this mode, the MCP server obtains each broker's token by exchanging the calling agent's Hop 1 token (RFC 8693 token exchange) against the identity provider. `mcp_client_auth.mode: oauth` (Hop 1) is required first — RFC 8693 token exchange consumes the agent's Hop 1 JWT as its `subject_token`, so with `mode: static` or `mode: disabled` there is no agent token to exchange and Hop 2 has nothing to do.

> **Note:** Configuring `auth.mode: oauth` on a broker while Hop 1 is `static`/`disabled` is rejected at startup with:
> ```
> mcp_client_auth.mode is "static" but 1 broker has auth.mode: oauth; the MCP server
> needs the agent's token (received via mcp_client_auth) to obtain a broker token, so
> mcp_client_auth.mode must be oauth
> ```

Add the top-level `broker_oauth:` block with the IdP's token-exchange coordinates, and set `auth.mode: oauth` on each broker that should use it:

```yaml
broker_oauth:
  idp_token_endpoint: "https://your-idp.example.com/realms/your-realm/protocol/openid-connect/token"
  mcp_server_client_id: "mcp-server"
  mcp_server_client_auth:
    client_secret_basic:
      secret: "${MCP_SERVER_CLIENT_SECRET}"
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: "audience"

brokers:
  prod:
    url: "https://broker.example.com:943"
    auth:
      mode: oauth
      audience: "solace-broker-prod"
```

| Field | Description |
|-------|-------------|
| `broker_oauth.idp_token_endpoint` | The IdP's token endpoint — where the MCP server POSTs the token-exchange request. Must be `https://` in production. |
| `broker_oauth.mcp_server_client_id` | The MCP server's own `client_id`, registered at the IdP (this is a separate client registration from the one used for Hop 1 in step 1.2). |
| `broker_oauth.mcp_server_client_auth` | How the MCP server authenticates itself to the IdP's token endpoint — a discriminated union, exactly one sub-block populated: `client_secret_basic.secret` (sent via HTTP Basic auth) or `client_secret_post.secret` (sent in the form body). |
| `broker_oauth.grant_type` | The OAuth grant type used for the Hop 2 exchange — see [Grant type](#grant-type) below. |
| `broker_oauth.audience_parameter_name` | Which request parameter carries the per-broker audience value — see [Audience parameter name](#audience-parameter-name) below. |
| `brokers.<alias>.auth.mode` | Set to `oauth` to use token exchange for this broker. |
| `brokers.<alias>.auth.audience` | Optional, even under `auth.mode: oauth` — omitting it does not fail startup. This broker's audience value, forwarded to the IdP during exchange using whichever request parameter `audience_parameter_name` selects; when omitted, the exchange request carries no audience parameter at all. Omit if the broker's OAuth profile does not validate audience; set it only if it does. If set, it must not be whitespace-only (a `${VAR}` resolving to blank fails config load). |

The IdP needs a second client registration for the MCP server itself (distinct from the Hop 1 client in step 1.2) — a **confidential** client with a client secret, since the MCP server authenticates itself directly to the token endpoint rather than involving a browser. Grant it whatever token-exchange permissions your IdP requires (for Keycloak, enable the token-exchange feature for the client and permit it to exchange tokens for the target broker's audience).

#### Grant type

`grant_type` tells the IdP which OAuth flow this request is: RFC 8693 token exchange trades one already-issued token (the agent's Hop 1 JWT) for another (a broker-bound token), rather than the IdP verifying a password or a client secret directly. Set it to the literal RFC 8693 grant-type URN:

```yaml
grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
```

This is the only grant type this version implements. The field exists (rather than being hardcoded) so a future grant type can be added without a config schema change, but today any other value — including a value your IdP itself recognizes for some other flow — is rejected at startup with `broker_oauth.grant_type is required` (if empty) or an "unsupported grant_type" error (if set to anything else).

#### Audience parameter name

`audience_parameter_name` tells the runtime which OAuth request parameter should carry each broker's `auth.audience` value in the token-exchange POST. Different IdP families expect the audience on a different parameter:

| Value | Style | Runtime support |
|-------|-------|------------------|
| `audience` | RFC 8693's own `audience` parameter — the default for Keycloak and most OIDC-compliant IdPs. | **Implemented.** |
| `scope` | Microsoft Entra On-Behalf-Of style (the audience is prefixed onto the `scope` value instead of a separate parameter). | Schema-accepted, **not yet implemented**. |
| `resource` | RFC 8707 resource-indicator style. | Schema-accepted, **not yet implemented**. |

Only `audience` works today. The other two values pass config-file validation at startup (the YAML schema accepts all three so configs targeting a future IdP integration can be staged in advance), but the token-exchange runtime itself rejects them when it is actually constructed — which only happens once Hop 1 is `oauth`, `broker_oauth:` is set, and at least one broker uses `auth.mode: oauth` together. If your IdP is Entra (or otherwise expects `scope`/`resource`), broker OAuth is not yet usable against it in this version.

> **Note:** a schema-accepted but unimplemented value fails at server startup with:
> ```
> tokenexchange: audience_parameter_name "scope" is schema-accepted but not yet implemented
> ```

Two optional sub-blocks tune the runtime's resilience behavior — see [Configuration](configuration.md#broker-oauth-hop-2) for every field and its default:

- `broker_oauth.circuit_breaker` — fails token-exchange calls fast during a sustained IdP outage, instead of letting every broker's requests queue up against a dead IdP. On by default; every field optional.
- `broker_oauth.retry_after` — shares a process-wide backoff across every broker when the IdP asks callers to slow down (HTTP 429 with `Retry-After`), so one throttled broker doesn't let every other broker keep hammering the same IdP.

### TLS for the MCP server's own listener

`mode: oauth` is a production profile, so the server must not silently serve its
own listener over plaintext — client bearer tokens and tool results would travel
in cleartext. There are two supported deployment patterns; OAuth mode requires at
least one of them, or startup fails with a config error.

1. **Direct TLS at the server.** Set both `tls_cert_file` and `tls_key_file`. The
   server listens over HTTPS itself.

   ```yaml
   tls_cert_file: "/etc/mcp-server/tls/tls.crt"
   tls_key_file: "/etc/mcp-server/tls/tls.key"
   ```

2. **TLS terminated upstream.** A reverse proxy, load balancer, or Kubernetes
   ingress terminates TLS and forwards plaintext to the server on a private
   network. Acknowledge this explicitly:

   ```yaml
   tls_terminated_upstream: true
   ```

   The server then serves plaintext on its bind address and logs a startup
   `WARN` naming `tls_terminated_upstream`, so a missing terminating proxy stays
   visible in triage logs. Make sure the proxy is actually in front of the bind
   address.

   > **Bind scope:** this flag only permits the plaintext listener — it does not
   > restrict where the server binds. Under `mode: oauth`, `listen_address`
   > defaults to all interfaces (`0.0.0.0`), so the plaintext port is reachable by
   > anything that can route to it, not only the terminating proxy. Make sure the
   > network scope is trusted: on Kubernetes keep the Service `ClusterIP` and put
   > the TLS-terminating ingress in front of it; on bare metal set `listen_address`
   > to the proxy-facing interface (loopback for a same-host proxy) so only the
   > terminator can reach the port.

If **both** are set, direct TLS takes precedence: the server terminates TLS
itself and `tls_terminated_upstream` is ignored (no plaintext, no `WARN`).
Providing **neither** is a fatal config error. The setting is ignored entirely in
the `disabled`/`static` dev modes.

### Tool authorization (claim-based RBAC)

Under `mode: oauth`, the server can gate individual MCP tools by the caller's
group or role memberships. Configure a claim in the IdP that lists these
memberships and map each name to the tools it may invoke; the server compares
every incoming tool call against the policy and denies calls whose memberships
do not grant that tool. Tool authorization is only supported under `mode: oauth`
— a `tool_authorization` block under `mode: static` or `mode: disabled` is
refused at startup.

**Every oauth deployment must opt in or out explicitly.** The `tool_authorization`
block is required under `mode: oauth` and its `enabled` field must be set to
`true` or `false` — there is no default. This forces a deliberate choice on
tool-level access control instead of silently allowing everything.

At the IdP, emit a claim in each access token that lists the caller's group or
role memberships. In Keycloak this is a **Group Membership** (or **Realm Roles**)
mapper on the same scope you used for the audience mapper — mapping the claim
name to whatever you choose to set as `groups_claim_name` (the default is
`groups`, matching the broker's own OAuth profile).

At the MCP server, add a `tool_authorization` block under `mcp_client_auth`. To
turn the feature ON, set `enabled: true` and populate `access_level_groups`:

```yaml
mcp_client_auth:
  mode: oauth
  issuer: "https://your-idp.example.com/realms/your-realm"
  audience: "solace-mcp-server"
  resource_url: "https://your-mcp-server.example.com/mcp"
  tool_authorization:
    enabled: true
    groups_claim_name: "groups"
    access_level_groups:
      Ops:
        - list-vpns
        - list-queues
        - get-queue-metrics
      Admin:
        - list-vpns
        - list-queues
        - get-queue-metrics
        - delete-queue-messages
```

To turn the feature OFF (every authenticated caller can invoke any tool), the
block still has to be present — set only `enabled: false` and omit the rest:

```yaml
mcp_client_auth:
  mode: oauth
  issuer: "https://your-idp.example.com/realms/your-realm"
  audience: "solace-mcp-server"
  resource_url: "https://your-mcp-server.example.com/mcp"
  tool_authorization:
    enabled: false
```

`list-brokers` is structurally exempt — every authenticated caller can invoke it
regardless of their groups, so a caller can always discover which broker aliases
exist. See [Tool authorization](configuration.md#tool-authorization) in the
configuration reference for the full field description, the audit-log shape
emitted on each decision, and the meaning of the `decision_reason` codes
(`missing_claim`, `not_permitted`).

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
 Agent          MCP Server     IdP            Broker
   │              │              │              │
   │───── 1 ──────▶              │              │       1. MCP request (no token)
   ◀───── 2 ──────│              │              │       2. 401 + resource-metadata pointer
   │───── 3 ──────▶              │              │       3. GET /.well-known/oauth-protected-resource
   │              │              │              │          (discovers authorization server)
   │───────────── 4 ─────────────▶              │       4. Register (DCR) or use pre-registered client
   ◀───────────── 5 ─────────────▶              │       5. Browser login: Authorization Code + PKCE
   ◀───────────── 6 ─────────────│              │       6. Access token (JWT, aud = configured audience)
   │───── 7 ──────▶              │              │       7. MCP request + Bearer JWT
   │              │───── 8 ──────▶              │       8. Validate JWT — fetch JWKS (sig, iss, aud, exp)
   │              │───────────── 9 ─────────────▶       9. Tool call → SEMP (broker auth: basic/bearer)
   │              ◀──────────── 10 ─────────────│       10. SEMP response
   ◀──── 11 ──────│              │              │       11. Tool result
   │              │              │              │
```

**Authentication flow (`mode: oauth` with `tool_authorization.enabled: true`):**

```
 Agent          MCP Server     IdP            Broker
   │              │              │              │
   │───── 1 ──────▶              │              │       1. MCP request (no token)
   ◀───── 2 ──────│              │              │       2. 401 + resource-metadata pointer
   │───── 3 ──────▶              │              │       3. GET /.well-known/oauth-protected-resource
   │───────────── 4 ─────────────▶              │       4. Register (DCR) or use pre-registered client
   ◀───────────── 5 ─────────────▶              │       5. Browser login: Authorization Code + PKCE
   ◀───────────── 6 ─────────────│              │       6. Access token (JWT with memberships in
   │              │              │              │          `groups_claim_name`, aud = configured
   │              │              │              │          audience)
   │───── 7 ──────▶              │              │       7. MCP request + Bearer JWT
   │              │───── 8 ──────▶              │       8. Validate JWT — fetch JWKS (sig, iss, aud, exp)
   │              │──┐           │              │       9. Read `groups_claim_name` from JWT
   │              │  │ 9         │              │          → run policy: is tool granted?
   │              │◀─┘           │              │          → allow → emit `tool authorization` INFO,
   │              │              │              │            continue to step 10
   │              │              │              │          → deny → emit `tool authorization` WARN,
   │              │              │              │            jump straight to step 12
   │              │              │              │            with a "not authorized" error
   │              │───────────── 10 ────────────▶       10. (Allow path only) Tool call → SEMP
   │              ◀──────────── 11 ─────────────│       11. (Allow path only) SEMP response
   ◀──── 12 ──────│              │              │       12. Tool result — real result on allow,
   │              │              │              │           "You are not authorized to use this tool."
   │              │              │              │           error on deny
   │              │              │              │
```

> **`list-brokers` skips the step-9 gate** — it is structurally exempt and
> answers locally from the server's configured broker list without a step-9
> policy check or a step-10/11 SEMP call. The audit line for a `list-brokers`
> call is the regular `tool invoked` line only; no `tool authorization` line
> is emitted. This lets a caller always discover configured broker aliases
> before invoking any other tool.

> **Two independent auth legs.** Client→server auth (steps 1–8, the JWT above) is
> distinct from server→broker auth (step 9 or 10 depending on whether tool
> authorization is enabled), which uses each broker's configured `auth.mode`
> (`basic`, `bearer`, or `oauth`). Broker-bound OAuth via RFC 8693 token
> exchange (the `broker_oauth:` config block) obtains a broker-bound token by
> exchanging the client's Hop 1 token, and requires `mcp_client_auth.mode:
> oauth` — see [Step 2b: Configure broker OAuth (Hop 2)](#step-2b-configure-broker-oauth-hop-2) below.

**Server→broker flow when `auth.mode: oauth` (Hop 2), cache miss:**

```
 MCP Server     Cache          IdP            Broker
   │              │              │              │
   │───── 9a ─────▶              │              │
   ◀───── 9b ─────│              │              │
   │──────────── 9c ─────────────▶              │
   ◀──────────── 9d ─────────────│              │
   │───── 9e ─────▶              │              │
   │──────────────────── 10 ────────────────────▶
   ◀──────────────────── 11 ────────────────────│
   │              │              │              │
```

9a. Tool call arrives — look up a cached broker-bound token for this (agent, broker) pair
9b. Cache miss
9c. RFC 8693 token exchange: `subject_token` = the agent's Hop 1 JWT, audience = this broker's configured `auth.audience`
9d. Broker-bound access token
9e. Cache the token, keyed by (agent, broker), until it expires
10. Tool call → SEMP with the broker-bound token as `Authorization: Bearer`
11. SEMP response

**Server→broker flow when `auth.mode: oauth` (Hop 2), cache hit:**

```
 MCP Server     Cache          Broker
   │              │              │
   │───── 9a ─────▶              │
   ◀───── 9b ─────│              │
   │──────────── 10 ─────────────▶
   ◀──────────── 11 ─────────────│
   │              │              │
```

9a. Tool call arrives — look up a cached broker-bound token for this (agent, broker) pair
9b. Cache hit — no IdP round-trip
10. Tool call → SEMP with the cached token as `Authorization: Bearer`
11. SEMP response

> **Cache key and lifetime.** The cache is keyed on the (agent identity, broker alias) pair, derived from the agent's Hop 1 `subject_token` — the same agent talking to two different brokers gets two independently cached tokens, and two different agents talking to the same broker never share one. An entry lives until the token it holds expires; there is no separate cache TTL setting. Concurrent tool calls that miss the cache for the same (agent, broker) pair at the same time are collapsed into a single IdP round-trip — only one exchange happens, and every caller shares its result. On a broker `401`, the SEMP transport evicts that pair's cached token and retries once with a freshly exchanged one (see [CHANGELOG](../CHANGELOG.md)); a persistently rejected credential still surfaces as a `401` after that single retry, not a loop.

> **What a cache miss can fail with.** Step 9c is subject to the circuit breaker, the `Retry-After` gate, and the exchange retry loop described in [Step 2b](#step-2b-configure-broker-oauth-hop-2) — a sustained IdP outage or a still-throttling IdP fails the tool call immediately at 9c rather than reaching the broker at all, distinct from a broker-side `401`/`403`.

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

### "You are not authorized to use this tool." from a valid user (Mode 3)

This is a tool-authorization denial. The token authenticated successfully, but the server's claim-based policy did not grant the tool. The caller-facing message is deliberately generic — grep the server logs for a `msg: "tool authorization"` line at the same `correlation_id` to see why:

- `decision_reason: "missing_claim"` with `expected_claim: "<name>"` — the token had no claim by that name at the top level of the JWT. Common causes, in order of likelihood: (a) the IdP's memberships mapper is not applied to the caller's client scope (fix at the IdP); (b) `groups_claim_name` in the server config does not match the claim the IdP actually emits (fix at the server); (c) the IdP emits memberships inside a nested object (e.g. `authorization.roles`) — the server only reads top-level claims, so flatten the memberships into a top-level claim with an IdP mapper.
- `decision_reason: "not_permitted"` with `matched_groups: []` — the claim was present but none of the caller's memberships grant the tool. Fix by adding the tool to a group the caller is a member of, or adding the caller to a group that already grants it.

`list-brokers` is exempt and will always succeed for any authenticated caller — a successful `list-brokers` call in the same session as a denied tool call confirms the token itself is valid and it is the tool-authorization policy that is denying. See [Tool authorization](configuration.md#tool-authorization) for the full audit-line schema.