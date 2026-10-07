# Authentication Guide

The Solace Broker MCP Server supports three authentication modes for MCP client-to-server communication. This guide describes how to configure each mode.

## Table of Contents

- [Mode 1: No Authentication (`mode: disabled`)](#mode-1-no-authentication-mode-disabled)
- [Mode 2: Static Dev Token (`mode: static`)](#mode-2-static-dev-token-mode-static)
- [Mode 3: OAuth / JWT (`mode: oauth`)](#mode-3-oauth--jwt-mode-oauth)
  - [Choose a Client Registration Method](#choose-a-client-registration-method)
  - [Step 1: Set Up the Identity Provider](#step-1-set-up-the-identity-provider)
  - [Step 2: Configure the MCP Server](#step-2-configure-the-mcp-server)
  - [Step 2b: Configure Broker OAuth (Hop 2)](#step-2b-configure-broker-oauth-hop-2)
  - [Step 2c: Configure the Event Broker's OAuth Profile](#step-2c-configure-the-event-brokers-oauth-profile)
  - [TLS for the MCP Server's Own Listener](#tls-for-the-mcp-servers-own-listener)
  - [Session Routing at the Ingress (Required Above One Replica)](#session-routing-at-the-ingress-required-above-one-replica)
  - [Tool Authorization (Claim-Based RBAC)](#tool-authorization-claim-based-rbac)
  - [Step 3: Start the MCP Server](#step-3-start-the-mcp-server)
  - [Step 4: Add the Server to Claude Code](#step-4-add-the-server-to-claude-code)
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
- **Cleartext caveat:** the dev token is a long-lived shared bearer token. On a non-loopback bind **without TLS** it travels in plaintext and can be sniffed and replayed for the same event-broker-admin-backed access — enable TLS (`tls_cert_file`/`tls_key_file`) whenever `static` is network-reachable. The server emits a startup WARN in this case.
- A prominent banner appears in the logs at startup:
  ```
  ============================================================
    INSECURE MODE: mcp_client_auth.mode = static
    Authentication uses a shared static dev token.
    This is development mode — NOT FOR PRODUCTION USE.
  ============================================================
  ```

### Important Notes

**Important: Reconnect, do not re-authenticate.** If the MCP client disconnects, access the server in your client's MCP server list and select "reconnect" (not "re-authenticate"). Re-authenticating triggers an OAuth flow that fails in dev mode. Reconnecting re-uses the configured static token.
---

## Mode 3: OAuth / JWT (`mode: oauth`)

**When to use:** Production deployments or any environment where browser-based authentication with an identity provider (IdP) is required. This mode uses the OAuth 2.1 Authorization Code flow with PKCE, allowing MCP clients (like Claude) to authenticate users via a browser login.

### Choose a Client Registration Method

Before starting, decide how the MCP client registers with the identity provider:

| | Option A: Client pre-registration | Option B: Dynamic Client Registration |
|---|---|---|
| User setup | Provide `--client-id` when adding the server | Add the server URL — no credentials needed |
| IdP requirements | Standard OAuth client setup | Must support anonymous Dynamic Client Registration (DCR) (RFC 7591) |
| Best for | Most setups; more control | Zero-configuration end-user experience |

Both options use the same OAuth 2.1 Authorization Code flow with PKCE. The difference: Option A requires pre-registering a client ID (and optionally a client secret). Option B uses Dynamic Client Registration (DCR) to obtain a client ID at runtime—no client secret needed.

### Step 1: Set Up the Identity Provider

An OAuth 2.1 / OpenID Connect (OIDC) identity provider such as Keycloak, Auth0, or Okta is required. This guide uses Keycloak for examples, but any OIDC-compliant provider works.

#### 1.1 Create a Realm or Tenant

Most identity providers (IdPs) organize clients and users into an isolated namespace — called a realm, tenant, or organization depending on the provider. Create one dedicated to the MCP server deployment.

> **Keycloak:** Start a local instance with Docker:
> ```bash
> docker run -p 127.0.0.1:8080:8080 \
>   -e KC_BOOTSTRAP_ADMIN_USERNAME=admin \
>   -e KC_BOOTSTRAP_ADMIN_PASSWORD=admin \
>   quay.io/keycloak/keycloak:latest start-dev
> ```
> Then open the Admin Console at `http://localhost:8080/admin`, log in with `admin` / `admin`, click the realm drop-down list in the top-left → **Create realm** → set a **Realm name** → **Create**. The built-in `master` realm can be used for quick local testing, but a dedicated realm is recommended.
>
> **Reference:** [`test/e2e-oauth/realm-export.json`](../test/e2e-oauth/realm-export.json) is a concrete, working example of this shape (realm, audience mappers, OAuth clients, test users) — test-only, not a production template, but useful to see the pieces fit together.

> **Microsoft Entra ID:** the tenant is your existing Entra tenant; there is nothing to create. You register applications in it — see [Quickstart: Register an application](https://learn.microsoft.com/entra/identity-platform/quickstart-register-app). This server needs two application registrations: one for the MCP server itself (the API that MCP clients authenticate against), and one representing the event broker API. One Entra application can serve several brokers; see [Target](#target).
>
> You configure your own tenant. This guide states what this server requires of it, and links to Microsoft for how to do each piece.

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

> **Microsoft Entra ID:** no mapper is needed — Entra sets `aud` to the application the token was requested for. Set `audience` below to the MCP server application's Application (client) ID.
>
> Two things to get right on the MCP server's registration:
> - **Expose an API,** so clients can request a delegated scope against it. The resulting Application ID URI is what `scopes_supported` advertises — see [Step 2](#step-2-configure-the-mcp-server).
> - **Pin the access-token version to 2.** New registrations may issue v1 tokens by default, whose `iss` is `https://sts.windows.net/{tenant-id}/` rather than `https://login.microsoftonline.com/{tenant-id}/v2.0`. A v1 token fails issuer validation against the v2 issuer. In our testing this had to be set explicitly on the application manifest (`requestedAccessTokenVersion: 2`). Decode a token and check `iss` before assuming which you are getting.

#### 1.3 Register an OAuth Client (Option A Only)

Skip this step when using Option B (Dynamic Client Registration).

Create a client in the IdP with the following settings:

- **Client ID:** any name (for example, `mcp-client`) — pass this as `--client-id` in step 4
- **Flow:** Authorization Code with PKCE
- **PKCE challenge method:** `S256`
- **Client type:** Public (no secret) or Confidential (with secret) — a public client is recommended for MCP clients like Claude Code and Claude Desktop, since they run on a user's machine where a client secret cannot be stored securely. For Option B (Dynamic Client Registration), always register the MCP client as public.
- **Redirect URI:** `http://localhost:<port>/callback`, where `<port>` matches the `--callback-port` used when adding the server to Claude Code

> **Keycloak:** **Clients** → **Create client** → set a **Client ID** (for example, `mcp-client`) → click **Next**.
> - **Client authentication:** leave **OFF** for a public client (no secret required); turn **ON** for a confidential client (requires `--client-secret`)
>
> Enable **Standard flow** and disable all other flows. Enable **Require PKCE** and set the **PKCE Method** to `S256` → click **Next**. Under **Login settings**, set **Valid redirect URIs** to `http://localhost:*` → **Save**.

> **Microsoft Entra ID:** use Option A. Microsoft documents that Dynamic Client Registration is not available for an MCP server protected by Entra, so plan on pre-registering the MCP client. Register it as a public client with the redirect URI above, grant it the delegated scope you exposed on the MCP server application, and hand the Application (client) ID to whoever configures the MCP client.

#### 1.4 Create Users

Create user accounts in the IdP. These users log in via the browser window that opens during the OAuth flow.

> **Keycloak:** **Users** → **Create new user** → enter a username → **Create**. Go to the **Credentials** tab → **Set password**, enter a password, and turn **Temporary** off.

> **Microsoft Entra ID:** use your existing tenant users. If you plan to use tool authorization below, decide now which claim will carry a caller's entitlements — app roles assigned on the MCP server application, or security group membership — and configure the application to emit it. That claim name goes in `tool_authorization.groups_claim_name`; see [Tool Authorization](#tool-authorization-claim-based-rbac).

### Step 2: Configure the MCP Server

Open `broker-config.yaml` and set:

```yaml
mcp_client_auth:
  mode: oauth
  issuer: "https://your-idp.example.com/realms/your-realm"
  audience: "solace-mcp-server"
  resource_url: "https://your-mcp-server.example.com/mcp"
```

That YAML is a valid load. `scopes_supported` is omitted, so the server advertises `["openid"]` in its [RFC 9728](https://www.rfc-editor.org/rfc/rfc9728.html) Protected Resource Metadata — the document MCP clients fetch from `/.well-known/oauth-protected-resource` to discover how to authenticate. Omitting the field, setting it to `null`, and setting it to `[]` all produce the same `["openid"]`; the process starts in every case.

That default is enough for Keycloak. It is not enough for Entra, and the failure is easy to misread: the server starts normally, PRM is served, and the problem only appears when a client tries to log in. Keep the default for Keycloak (example issuer block below). For Microsoft Entra, set the list explicitly — keep `openid`, add `offline_access`, and add the delegated scope you exposed on this MCP server's application registration:

```yaml
mcp_client_auth:
  mode: oauth
  issuer: "https://login.microsoftonline.com/00000000-0000-0000-0000-000000000000/v2.0"
  audience: "11111111-1111-1111-1111-111111111111"
  resource_url: "https://mcp.example.com/mcp"
  scopes_supported:
    - "openid"
    - "offline_access"
    - "https://mcp.example.com/mcp/access_as_user"
```

In our testing, advertising `openid` alone while the client sent RFC 8707 `resource` equal to the MCP URL was refused by Entra with `AADSTS9010010`. Do not drop `openid` to avoid that — advertise all three. This is an IdP refusal at login, not a configuration error on this server: nothing here rejects an `openid`-only list.

`offline_access` is in that list for a different reason, and leaving it out fails much later than `AADSTS9010010` does. Entra issues a refresh token only when the authorization request itself carries `offline_access`. Microsoft notes that the scope is implicitly *granted* once any delegated permission is granted, but that covers consent, not the request — the string still has to be in the request. A client that builds its authorization request from the `scopes_supported` list this server advertises asks for exactly what is in the list, which is why the list is where this belongs. Leave it out and, for such a client, login succeeds and tools work — and then it has no refresh token to renew its access token with, so each expiry sends the user back through a browser login. Nothing fails on this server when that happens: it never sees a failing request, so there is nothing in these logs to find. A client that chooses its own scopes, or that can renew silently some other way, behaves differently; what this server controls is only what it advertises.

Two things to expect once it is set. `offline_access` is an OpenID Connect scope rather than a scope of this API, so it is normal for it not to appear in the access token's `scp` claim, which carries the delegated scopes of this API; whether a refresh token came back in the token response is the thing to check. And on a deployment that has already been consented, widening the list can surface the "Maintain access to data you have given it access to" item the next time users authenticate — in a tenant where user consent is disabled, that needs an administrator's consent before logins succeed again.

This list is Hop 1 discovery, advertised to MCP clients. The server does not gate inbound tokens on it, and it is **not** `brokers.*.auth.target` (Hop 2) — see [Target](#target). The word "scope" appears on both hops and means different things: this list is what clients request at the IdP, while under the jwt-bearer grant `auth.target` is what travels to the IdP *as* the `scope` parameter.

The `audience` value must exactly match the value configured in step 1.2. Set `resource_url` to the externally reachable URL of the MCP endpoint — this is advertised to clients for OAuth discovery, so it must be the public-facing URL, not the server's internal bind address (these differ when running behind a reverse proxy or ingress).

| Field | Description |
|-------|-------------|
| `issuer` | The OIDC issuer URL of the IdP. The server fetches JWKS keys from here for token validation. |
| `audience` | Must exactly match the audience value configured in the IdP in step 1.2. |
| `resource_url` | The public URL of the MCP server endpoint, advertised to MCP clients for OAuth discovery. |
| `scopes_supported` | Ordered string list advertised as PRM `scopes_supported`. Omitted, `null`, or `[]` becomes `["openid"]`. A non-empty list is advertised as written (order and duplicates kept). An empty string or any entry that contains whitespace is a load error (`mcp_client_auth.scopes_supported[N] must be a non-empty string without whitespace`). Scopes are public identifiers, not secrets. **Entra:** include `offline_access` — Entra issues a refresh token only when the authorization request carries it, so a client that requests the advertised list has none to renew with and falls back to an interactive login at each access-token expiry. Hop 1 only; it does not belong in `brokers.*.auth.target`. Full field table: [Configuration](configuration.md#client-authentication-settings). |

> **Keycloak:** The issuer URL follows the pattern `https://<host>:<port>/realms/<realm-name>`. For example, Keycloak running locally on port 8443 with TLS and a realm named `solace`:
> ```yaml
> mcp_client_auth:
>   mode: oauth
>   issuer: "https://localhost:8443/realms/solace"
>   audience: "solace-mcp-server"
>   resource_url: "https://localhost:9090/mcp"
> ```

> **Note:** Under `mode: oauth` the validator enforces `https://` on **both** the `issuer` and `resource_url` URLs (an `http://` value is rejected at startup). When running Keycloak locally for testing, terminate TLS in front of it (for example, via Caddy or a reverse proxy) or run Keycloak with a TLS cert. `resource_url` is the externally advertised identifier for OAuth discovery, so it must be `https://` even when the MCP server's own listener is plaintext behind an upstream terminator — see [TLS for the MCP Server's Own Listener](#tls-for-the-mcp-servers-own-listener).

### Step 2b: Configure Broker OAuth (Hop 2)

This step is only needed if one or more event brokers use `auth.mode: oauth` instead of `basic`/`bearer`. Under this mode, the MCP server obtains each event broker's token by exchanging the calling agent's Hop 1 token against the identity provider, via RFC 8693 token exchange or RFC 7523 jwt-bearer (Entra On-Behalf-Of) — see [Grant Type](#grant-type). `mcp_client_auth.mode: oauth` (Hop 1) is required first — either grant consumes the agent's Hop 1 JWT as the exchange request's subject (token exchange: `subject_token`; jwt-bearer: `assertion`), so with `mode: static` or `mode: disabled` there is no agent token to exchange and Hop 2 has nothing to do.

> **Note:** Configuring `auth.mode: oauth` on an event broker while Hop 1 is `static`/`disabled` is rejected at startup with:
> ```
> mcp_client_auth.mode is "static" but 1 broker has auth.mode: oauth; the MCP server
> needs the agent's token (received via mcp_client_auth) to obtain a broker token, so
> mcp_client_auth.mode must be oauth
> ```

Add the top-level `broker_oauth:` block with the IdP's token-exchange coordinates, and set `auth.mode: oauth` on each event broker that uses it:

```yaml
broker_oauth:
  idp_token_endpoint: "https://your-idp.example.com/realms/your-realm/protocol/openid-connect/token"
  mcp_server_client_id: "mcp-server"
  mcp_server_client_auth:
    client_secret_basic:
      secret: "${MCP_SERVER_CLIENT_SECRET}"
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  # token_expiry_fallback: 1h  # optional; omit = fail-closed when the IdP is silent

brokers:
  prod:
    url: "https://broker.example.com:943"
    auth:
      mode: oauth
      target: "solace-broker-prod"
```

| Field | Description |
|-------|-------------|
| `broker_oauth.idp_token_endpoint` | The IdP's token endpoint — where the MCP server POSTs the token-exchange request. Must be `https://` in production. |
| `broker_oauth.mcp_server_client_id` | The MCP server's own `client_id`, registered at the IdP (this is a separate client registration from the one used for Hop 1 in step 1.2). |
| `broker_oauth.mcp_server_client_auth` | How the MCP server authenticates itself to the IdP's token endpoint — a discriminated union, exactly one sub-block populated: `client_secret_basic.secret` (sent via HTTP Basic auth) or `client_secret_post.secret` (sent in the form body). |
| `broker_oauth.grant_type` | The OAuth grant type used for the Hop 2 exchange — see [Grant Type](#grant-type). |
| `broker_oauth.token_expiry_fallback` | Optional positive duration used only when the IdP omits `expires_in`, returns `null`, or returns `0`. Omit it to preserve fail-closed behavior. Any positive duration passes configuration validation, but `30s` or less returns an immediately stale token that is not cached, while cache residency is capped at 24 hours. |
| `brokers.<alias>.auth.mode` | Set to `oauth` to use token exchange for this event broker. |
| `brokers.<alias>.auth.target` | Optional at configuration load for every grant type, even under `auth.mode: oauth` — omitting it does not fail startup. One string naming this event broker's API at the IdP, forwarded during exchange in the request parameter the grant type selects (token exchange: `audience`; jwt-bearer: `scope`) — see [Target](#target). Omission behaves differently per grant: token exchange's request simply carries no audience parameter, safe to omit if the event broker's OAuth profile does not validate audience; jwt-bearer's request instead fails before any HTTP call (`jwt-bearer request missing scope`), since Entra's On-Behalf-Of marks scope required — jwt-bearer brokers must set it. A whitespace-only value fails configuration load regardless of grant type; an empty value (for example a `${VAR}` that resolves to `""`) is treated as omitted. |

The IdP needs a second client registration for the MCP server itself (distinct from the Hop 1 client in step 1.2) — a **confidential** client with a client secret, because the MCP server authenticates itself directly to the token endpoint rather than involving a browser. Grant it whatever token-exchange permissions your IdP requires (for Keycloak, enable the token-exchange feature for the client and permit it to exchange tokens for the target event broker's audience).

#### Grant Type

`grant_type` tells the IdP which OAuth flow this request is: RFC 8693 token exchange trades one already-issued token (the agent's Hop 1 JWT) for another (an event-broker-bound token), rather than the IdP verifying a password or a client secret directly. Set it to the literal RFC 8693 grant-type URN:

```yaml
grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
```

Two grant types are implemented: RFC 8693 token exchange (above) and `"urn:ietf:params:oauth:grant-type:jwt-bearer"` (RFC 7523, for Entra On-Behalf-Of). Any other value — including a value your IdP itself recognizes for some other flow — is rejected at configuration load with `broker_oauth.grant_type is required` (if empty) or `broker_oauth.grant_type "…" is not supported in this version` (if set to anything else).

Under the jwt-bearer grant, this server obtains broker tokens via Entra On-Behalf-Of, the same role token exchange plays for Keycloak. Either way the result is cached per caller and event broker, so a tool call reuses a live token rather than contacting the IdP again — see the cache-hit flow in [How It Works](#how-it-works).

> **Microsoft Entra ID — consent must already exist before the first tool call.** On-Behalf-Of
> exchanges a user's token for one addressed to the event broker's API, and Entra requires
> that the user has consented to the MCP server calling that downstream API on their behalf.
> This is a **separate** grant from the user's own sign-in consent to the MCP server, and
> nothing in the Hop 1 login flow requests it — the sign-in only covers the MCP server's own
> scope, not its downstream permission.
>
> There is no consent prompt at tool-call time. The exchange simply fails, and in our testing
> a user who had never consented saw `invalid_grant` from Entra on every Hop 2 attempt while
> Hop 1 login continued to work normally. Grant admin consent for the MCP server
> application's permission to the event broker API before onboarding users — or, where
> tenant policy allows user consent, have each user complete it once.
>
> Because Hop 1 keeps working, this presents as "login succeeds, every tool call fails."
> Check consent before looking at `auth.target` or the broker's OAuth profile.

#### Target

`brokers.<alias>.auth.target` names this event broker's API at the IdP — one string per event broker. The grant type decides which request parameter carries it on the wire, and whether it's actually required:

```yaml
brokers:
  prod:
    auth:
      mode: oauth
      target: "solace-broker-prod"        # token exchange: sent as RFC 8693 "audience"
      # target: "api://<APP_ID>/.default" # jwt-bearer: sent as Entra "scope" — required
```

**Token exchange** sends it as the RFC 8693 `audience` parameter and treats it as optional: set it to the audience value your IdP expects, or omit it if the event broker's OAuth profile does not validate audience.

**jwt-bearer** (Entra On-Behalf-Of) sends it as Entra's `scope` parameter, which Microsoft marks required — an omitted or whitespace-only target loads at configuration time but fails every exchange with `jwt-bearer request missing scope`, before the request reaches Entra. Typically set to `{App ID URI}/.default`.

Either way it is a single string: to reach a second event broker, configure a second alias with its own `target`, not a list. There is no separate parameter-name setting — `broker_oauth.audience_parameter_name` was removed and now fails configuration load, as does the old `brokers.<alias>.auth.audience` key.

One optional field controls handling of IdPs that omit token lifetime, and two optional sub-blocks tune runtime resilience — see [Configuration](configuration.md#event-broker-oauth-hop-2) for every field and its default:

- `broker_oauth.token_expiry_fallback` — supplies a lifetime only when the IdP returns no usable `expires_in`; a positive IdP value always takes precedence. Values of `30s` or less pass configuration validation but return an immediately stale token that is not cached.
- `broker_oauth.circuit_breaker` — fails token-exchange calls fast during a sustained IdP outage, instead of letting every event broker's requests queue up against a dead IdP. On by default; every field optional. Each transition still logs a `WARN`. With scrape metrics enabled, the current state is also on `/metrics` as `mcp_token_exchange_circuit_breaker_state`; with OTLP metrics enabled it is pushed instead. With neither metrics egress on the family is absent. See [Token-Exchange Circuit Breaker State](observability.md#token-exchange-circuit-breaker-state).
- `broker_oauth.retry_after` — shares a process-wide backoff across every event broker when the IdP asks callers to slow down (HTTP 429 with `Retry-After`), so one throttled event broker doesn't let every other event broker keep hammering the same IdP.

> **Configured vs. used vs. first-fire.** Creating the Hop 2 exchanger logs one INFO line, `token exchanger created for broker OAuth`, with `expiry_fallback_configured` (plus `expiry_fallback` when the setting is present) — that reports the fallback is armed, not that the IdP ever omitted `expires_in`. The first time a live exchange on that exchanger actually applies that duration, the server logs one INFO, `broker OAuth token expiry fallback supplied a lifetime`, again with `expiry_fallback`. Later fallback uses on the same exchanger do not repeat that INFO. Production constructs one Hop 2 exchanger per process, so operators normally see one first-fire line per server. Per-exchange `used_fallback` stays on the Debug line `identity provider issued broker token`; a cache hit produces none, and a fail-closed exchange returns an error instead. Absence of a later first-fire line does not mean the IdP started sending `expires_in`.

### Step 2c: Configure the Event Broker's OAuth Profile

Steps 2 and 2b configure the MCP server. The event broker independently validates
every token it receives, using an OAuth profile configured **on the broker**.
Until that profile exists and trusts your identity provider, Hop 2 fails even
when the MCP server obtains a token successfully.

Configure one profile per event broker that uses `auth.mode: oauth`. The profile
must have the resource-server role — the broker validates tokens, it does not
obtain them. See [Configuring OAuth Authentication](https://docs.solace.com/Configuring-and-Managing/Configuring-OAuth-for-Management-Access.htm)
in the event broker documentation for how to create and configure one. What
follows is only what this integration requires of it.

**The broker's required audience is not the same value as `auth.target`.** Both
identify the same application at the IdP, in different forms. Under jwt-bearer,
`auth.target` is the scope form — `api://<app-id>/.default` — while the broker's
required audience is the bare application ID. Copying one into the other rejects
every token.

**The broker's claim setting is separate from this server's.** The broker's
access-level-groups claim name governs what a caller may do on the broker.
`mcp_client_auth.tool_authorization.groups_claim_name` governs which MCP tools a
caller may invoke. They may name the same claim or different ones — set each
deliberately.

**Access levels are mapped on the broker, not in this server's YAML.** Map each
claim value to an access level in the profile's access-level groups. The value
must match what appears in the token exactly.

> **Microsoft Entra ID:** if the broker reads the `groups` claim, the values are
> group object IDs, not display names — in our testing the display name did not
> match. One Entra application can serve several event brokers; to give a caller
> different access on different brokers, use per-broker claim values rather than
> a second application.

**Unmapped values get no access.** The profile's default access level applies to
any caller whose claim value is not mapped. Leave it at none unless you intend
otherwise.

#### An MCP tool role is not event broker access

| | Controls | Configured in |
|---|---|---|
| `tool_authorization.access_level_groups` | which MCP tools a caller may invoke | this server's YAML |
| the OAuth profile's access-level groups | what the caller may do on the event broker | the event broker |

These are parallel layers. A caller may pass tool authorization and still be
refused by the broker, or the reverse. Configure both.

### TLS for the MCP Server's Own Listener

`mode: oauth` is a production profile, so the server must not silently serve its
own listener over plaintext — client bearer tokens and tool results would travel
in cleartext. There are two supported deployment patterns; OAuth mode requires at
least one of them, or startup fails with a configuration error.

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
   > terminator can reach the port. See
   > [`deploy/kubernetes/README.md`](../deploy/kubernetes/README.md) for the
   > shipped manifests and how to switch them to `mode: oauth`.

   > **Above one replica:** an ingress in this position bypasses the Service's
   > `sessionAffinity: ClientIP`, and the deployment ships with `replicas: 2`.
   > Configuring session routing at the ingress is a **required** step, not a
   > tuning option — see [Session Routing at the Ingress](#session-routing-at-the-ingress-required-above-one-replica)
   > directly below.

If **both** are set, direct TLS takes precedence: the server terminates TLS
itself and `tls_terminated_upstream` is ignored (no plaintext, no `WARN`).
Providing **neither** is a fatal configuration error. The setting is ignored entirely in
the `disabled`/`static` dev modes.

### Session Routing at the Ingress (Required Above One Replica)

The deployment ships `replicas: 2`. Putting an ingress, gateway, or mesh in
front of the Service — which the pattern above tells you to do — **requires**
configuring session routing at that layer. Skipping it breaks the deployment
rather than degrading it.

#### Why It Breaks

`sessionAffinity: ClientIP` does not apply: kube-proxy enforces it on the
ClusterIP path only, and an ingress load-balances straight to pod IPs.

Streamable HTTP sends every call as an independent POST, and the server holds
each session in one pod's memory. A session ID reaching any other pod is
answered `404 session not found`; the SDK does not re-initialize transparently.

Unpinned, each call after initialize has roughly a `1/replicas` chance of
reaching the right pod — at two replicas a 10-call session survives with
probability `(1/2)^10` ≈ **0.1%**. In practice it is often worse: with a single
controller replica, nginx's round-robin cursor makes the very next call after
initialize land on the other pod deterministically.

#### What to Configure

```yaml
metadata:
  annotations:
    nginx.ingress.kubernetes.io/upstream-hash-by: "$remote_addr"
```

Full manifest: [`deploy/kubernetes/ingress.yaml.example`](../deploy/kubernetes/ingress.yaml.example).

This restores at the ingress what `sessionAffinity: ClientIP` gave on the
ClusterIP path, and inherits its weakness: clients behind one NAT or egress
gateway all hash to the same pod, so the second replica may take little traffic.
Three caveats:

- Under `externalTrafficPolicy: Cluster` (the default) the controller may see a
  SNAT'd node address instead of the client IP, collapsing affinity to one pod
  per node. Set `Local` on the controller's own Service, or enable PROXY
  protocol.
- `$remote_addr` is only trustworthy if the controller's real-IP config is
  scoped. ingress-nginx defaults `proxy-real-ip-cidr` to `0.0.0.0/0`, so with
  `use-forwarded-headers` on — common behind a cloud L7 load balancer — any
  client can set its own `X-Forwarded-For` and choose which pod serves it.
  Restrict `proxy-real-ip-cidr` to the load balancer's CIDR.
- Sessions still die with their pod. Affinity pins a live session; it cannot
  carry state across a rolling update.

#### What Does Not Work

All three of these apply cleanly and leave the 404s in place.

**Cookie stickiness** (`nginx.ingress.kubernetes.io/affinity: cookie`) needs the
client to echo a cookie the ingress set. Whether that happens depends entirely
on the client's HTTP stack — the Go SDK keeps no cookie jar, httpx-based clients
including the Python SDK do — so it silently works for some clients and not
others. That inconsistency is worse than a clean failure, and this is the most
likely wrong turn.

**Hashing on the session ID** (`upstream-hash-by: "$http_mcp_session_id"`) is
the intuitive choice and is subtly broken: the initialize POST carries no such
header — the server mints the ID onto the *response* — so it hashes on a
constant empty key, putting every new session on whichever single pod that key
maps to, while the ID it returns hashes independently of it. At two replicas
half of all sessions are born on the wrong pod. The session ID is the exact
routing key for every request except the one that creates the session, and that
exception is what disqualifies it.

**These annotations on another controller.** Every annotation here is
ingress-nginx-specific. Traefik, HAProxy, Contour, and cloud ALB controllers
ignore unknown `nginx.ingress.kubernetes.io/*` keys silently, so changing
`ingressClassName` and applying gives a resource that reconciles clean and 404s
exactly as before. Use that controller's own affinity feature — see below.

#### Gateway API and Service Meshes

Gateway API's `sessionPersistence` (`BackendLBPolicy`) does not solve this — it
is gateway-assigned persistence, where the gateway issues a token the client
echoes back, so it fails exactly as cookies do. It is also experimental-channel
with thin support.

With no portable answer today, use your controller's consistent-hash-on-source-address
extension — Istio `DestinationRule` `consistentHash.useSourceIp`, Envoy Gateway
`BackendTrafficPolicy` with `consistentHash.type: SourceIP` — or run
`replicas: 1` and accept the rollout downtime.

### Tool Authorization (Claim-Based RBAC)

Under `mode: oauth`, the server can gate individual MCP tools by the caller's
group or role memberships (role-based access control, or RBAC). Configure a
claim in the IdP that lists these memberships and map each name to the tools
it can invoke; the server compares
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
`groups`, matching the event broker's own OAuth profile).

**Exactly one claim is read.** `groups_claim_name` names a single claim; the
server looks up that one key and does not fall back to another, merge two, or
accept a nested path. Whichever claim you name must carry every value the policy
below matches on.

> **Microsoft Entra ID:** Entra can be configured to emit entitlements in more
> than one way — app roles assigned on an application registration, or security
> group membership — and which one carries the values this server should match
> on is your choice to make deliberately. Configure the MCP server's application
> registration to emit the claim you intend, and name that claim here.
>
> The values differ in shape depending on which you pick: app-role values are the
> strings you define on the registration, while in our testing the `groups` claim
> carried group **object IDs** (GUIDs), not display names. Decode a real token
> and read the claim you chose rather than assuming its contents.
>
> `scp` is not a membership list — it carries the delegated scopes the client
> requested, so do not point `groups_claim_name` at it.
>
> This setting is also independent of the event broker's own
> `accessLevelGroupsClaimName`. The two may name the same claim or different
> ones, and granting an MCP tool here grants nothing on the broker — see
> [Step 2c](#step-2c-configure-the-event-brokers-oauth-profile).

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

By default callers still see every registered tool in `tools/list` — the policy is
enforced when they invoke one. Add `filter_tools_list: true` to the same block to
also narrow the list to what each caller can invoke; see
[Filtering `tools/list`](configuration.md#filtering-toolslist).

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

`list-brokers` and `describe-semp-schema` are structurally exempt — every authenticated
caller can invoke them regardless of their groups. See [Tool authorization](configuration.md#tool-authorization) in the
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
> answers locally from the server's configured event broker list without a step-9
> policy check or a step-10/11 SEMP call. The audit line for a `list-brokers`
> call is the regular `tool invoked` line only; no `tool authorization` line
> is emitted. This lets a caller always discover configured event broker aliases
> before invoking any other tool.

> **Two independent authentication legs.** Client→server auth (steps 1-8, the JSON Web
> Token (JWT) shown in the preceding diagram) is distinct from server→event
> broker auth (step 9 or 10 depending on whether tool authorization is
> enabled), which uses each event broker's configured `auth.mode` (`basic`,
> `bearer`, or `oauth`). Event-broker-bound OAuth via RFC 8693 token exchange
> (the `broker_oauth:` configuration block) obtains an event-broker-bound
> token by exchanging the client's Hop 1 token, and requires
> `mcp_client_auth.mode: oauth` — see
> [Step 2b: Configure Broker OAuth (Hop 2)](#step-2b-configure-broker-oauth-hop-2).

**Server→event broker flow when `auth.mode: oauth` (Hop 2), cache miss:**

```
 MCP Server     Cache          IdP            Broker
   │              │              │              │
   │───── 9a ─────▶              │              │       9a. Tool call arrives — look up a cached
                                                            broker-bound token for this (agent,
                                                            broker) pair
   ◀───── 9b ─────│              │              │       9b. Cache miss
   │──────────── 9c ─────────────▶              │       9c. RFC 8693 token exchange: subject_token
                                                            = the agent's Hop 1 JWT, audience = this
                                                            broker's configured auth.target
   ◀──────────── 9d ─────────────│              │       9d. Broker-bound access token
   │───── 9e ─────▶              │              │       9e. Cache the access token just received
                                                            from the IdP in 9d, keyed by (agent,
                                                            broker), until it expires
   │──────────────────── 10 ────────────────────▶       10. Tool call → SEMP with the broker-
                                                            bound token as Authorization: Bearer
   ◀──────────────────── 11 ────────────────────│       11. SEMP response
   │              │              │              │
```

**Server→event broker flow when `auth.mode: oauth` (Hop 2), cache hit:**

```
 MCP Server     Cache          Broker
   │              │              │
   │───── 9a ─────▶              │                      9a. Tool call arrives — look up a cached
                                                            broker-bound token for this (agent,
                                                            broker) pair
   ◀───── 9b ─────│              │                      9b. Cache hit — no IdP round-trip
   │──────────── 10 ─────────────▶                      10. Tool call → SEMP with the cached
                                                            token as Authorization: Bearer
   ◀──────────── 11 ─────────────│                      11. SEMP response
   │              │              │
```

> **Cache key and lifetime.** The cache is keyed on the (agent identity, event broker alias) pair, derived from the agent's Hop 1 `subject_token` — the same agent talking to two different event brokers gets two independently cached tokens, and two different agents talking to the same event broker never share one. An entry's residency is the lesser of its remaining usable lifetime and the server's fixed 24-hour cache ceiling; there is no operator-configurable cache TTL setting today. The token lifetime comes from a positive IdP `expires_in`, or from `broker_oauth.token_expiry_fallback` only when the IdP omits that value, returns `null`, or returns `0`; without either, exchange fails closed. Concurrent tool calls that miss the cache for the same (agent, event broker) pair at the same time are collapsed into a single IdP round-trip — only one exchange happens, and every caller shares its result. On an event broker `401`, the SEMP transport evicts that pair's cached token and retries once with a freshly exchanged one (see [CHANGELOG](../CHANGELOG.md)); a persistently rejected credential still surfaces as a `401` after that single retry, not a loop.

> **What a cache miss can fail with.** Step 9c is subject to the circuit breaker, the `Retry-After` gate, and the exchange retry loop described in [Step 2b](#step-2b-configure-broker-oauth-hop-2) — a sustained IdP outage or a still-throttling IdP fails the tool call immediately at 9c rather than reaching the event broker at all, distinct from an event-broker-side `401`/`403`.

The numbered steps in detail:

1. Claude Code connects to the MCP server and receives a `401 Unauthorized` response
2. Claude Code fetches the OAuth Protected Resource Metadata (PRM) from `/.well-known/oauth-protected-resource` to discover the authorization server (the server also serves the same document at the RFC 9728 canonical path `/.well-known/oauth-protected-resource/mcp` for clients that build that URL directly)
3. **Option A:** Claude Code uses the pre-registered client credentials and skips DCR
   **Option B:** Claude Code performs Dynamic Client Registration (RFC 7591) to obtain a client ID at runtime
4. A browser window opens for the user to log in with IdP credentials
5. After login, Claude Code receives a JWT access token and includes it in all subsequent requests
6. The MCP server validates each token's signature (via JWKS), issuer, audience, and expiry. It never refreshes a token: when one expires, the MCP client renews it against the IdP. A refresh token is the direct way to do that without the user; without one, renewal depends on the client and the IdP session, and commonly means an interactive login

On success, the server logs: `"using JWT token for authentication — production mode"`

---

## Troubleshooting

### Banner Appears at Startup ("INSECURE MODE")

This is expected for `mode: disabled` and `mode: static`. The banner is the deliberate signal that the server is running without production-grade auth. If production mode was intended, switch to `mode: oauth`.

### Cannot Reach the Server from Another Host (Modes 1 and 2)

Under `mode: disabled` and `mode: static` the server binds `127.0.0.1` only by default, so it is reachable from the local host but not the network. Check the startup log line (the `bind_address` field) for the effective host:port. To bind another interface, set `listen_address` in the configuration (for `mode: disabled` this also requires `allow_remote_unauthenticated: true`). See [Configuration](configuration.md#server-settings).

### "401 Unauthorized" Errors in Mode 2

- Verify that your client is sending the `Authorization: Bearer <token>` header
- Confirm the token value matches exactly what's in your configuration (no extra spaces or quotes)
- Check that `mcp_client_auth.mode: static` and `mcp_client_auth.dev_token` are both set in your configuration

### Token Does Not Load

- If using environment variables like `${DEV_TOKEN}`, export the variable before starting the server
- Check the server logs for configuration parsing errors

### Re-Authentication Errors in Claude (Modes 1 and 2)

- Access the server in your client's MCP server list and select "reconnect" (not "re-authenticate")
- Modes 1 and 2 do not have an authorization server configured to handle OAuth flows

### "failed to connect to identity provider" on Server Startup

- Verify the `issuer` URL is correct and reachable from the server
- If using Keycloak locally, ensure the container is running and healthy before starting the MCP server
- The MCP server connects to the issuer's `/.well-known/openid-configuration` at startup to fetch JWKS keys
- If egress to a cloud IdP requires an HTTP proxy, set `HTTPS_PROXY` — see [Outbound HTTP proxy](configuration.md#outbound-http-proxy). The same variable also governs broker SEMP traffic, so use `NO_PROXY` to keep internal brokers direct

### Entra `AADSTS9010010` After Claude Fetches PRM

The authorize request used only `openid` while also sending RFC 8707 `resource` as this MCP URL. Entra may return `AADSTS9010010`. Grep `registered OAuth protected resource metadata endpoint` and read `scopes_supported`. If it is `["openid"]`, `mcp_client_auth.scopes_supported` was omitted or empty — the process still started. Set `openid`, `offline_access`, and this app’s Application ID URI scope as in Step 2. This is an IdP refusal, not a config load error, and not an inbound-scope check on this server.

### Entra: the browser login comes back after a while

Everything works, and then the MCP client asks the user to sign in again — and keeps asking, every time the access token expires. The server logs show nothing wrong, because nothing is: there are no 401s, no rejected tokens, and no failing request for this server to log. The client is renewing against the IdP, not against us, and something there is making it fall back to an interactive login.

Start with what this server advertises. Grep `registered OAuth protected resource metadata endpoint` and read `scopes_supported`, the same line Step 2 uses. If `offline_access` is missing from that array, a client that requests the advertised list never asked Entra for a refresh token, and that is the likely cause: add it as in Step 2 and restart. Existing clients keep their cached credentials, so remove and re-add the server at the client, or otherwise clear its stored credentials, to make it re-authenticate and pick up the new list.

That log line proves only what this server published, not what the client sent. If the array already carries `offline_access`, or the symptom survives the change, read the client's own authorization request and logs: a client may choose its own scopes, and a refresh token that has expired, been revoked, or gone unused long enough to lapse produces the same repeated login.

Being granted is not the same as being requested. Microsoft documents `offline_access` as implicitly granted once any delegated permission is granted, which is why consent can look complete while no refresh token is ever issued. Also expect `offline_access` to be absent from the access token's `scp` claim, which lists this API's delegated scopes — so look for a refresh token in the token response rather than among the granted scopes. `offline_access` belongs to Hop 1 only; it does not go in `brokers.*.auth.target`.

### Entra: login works but every tool call fails

Hop 1 succeeds and the MCP client connects, then every tool call against an event broker
using `auth.mode: oauth` fails. Server logs carry `token exchange rejected by IdP`
naming Entra's `invalid_grant`; the agent sees only a generic authentication failure.

The usual cause is missing consent, not configuration. On-Behalf-Of requires the user to
have consented to the MCP server calling the event broker's API on their behalf, which is a
separate grant from their sign-in consent to the MCP server itself — Hop 1 login never
requests it, and there is no prompt at tool-call time. Grant admin consent for that
permission, or have the user complete it once where tenant policy allows user consent. See
[Step 2b](#step-2b-configure-broker-oauth-hop-2).

Check consent before `auth.target`: a missing or blank target fails differently, with
`jwt-bearer request missing scope` logged locally before Entra is contacted at all.

### "403 Forbidden" with a Valid Token

- Verify the audience mapper is configured in the IdP so the `aud` claim matches the `audience` configuration value
- Decode the JWT (for example, at jwt.io) to inspect the actual `aud` claim

### Browser Login Window Does Not Appear

- Grep this process's logs for `registered OAuth protected resource metadata endpoint` (oauth startup only). That INFO is the snapshot of what this process advertised:
  - `resource` — PRM JSON `resource` (`mcp_client_auth.resource_url`)
  - `issuers` — PRM JSON `authorization_servers` (slog key is `issuers` because `authorization` in a log key is redacted)
  - `scopes_supported` — YAML `mcp_client_auth.scopes_supported` after defaulting (`["openid"]` when omitted or empty; otherwise the configured list)
  - `bearer_methods_supported` — always `["header"]`
  - `resource_metadata_url` — exact URL on 401 `WWW-Authenticate` `resource_metadata` (bare well-known; no `/mcp` suffix)
  - `prm_paths` — GET paths on this process that return that JSON

  Example at default log level (`info`) when `scopes_supported` is omitted (`mcp_client_auth.resource_url` `https://localhost:9090/mcp`):
  ```json
  {"time":"2026-09-11T13:43:10.924146-07:00","level":"INFO","msg":"registered OAuth protected resource metadata endpoint","resource":"https://localhost:9090/mcp","issuers":["https://auth.example.com"],"scopes_supported":["openid"],"bearer_methods_supported":["header"],"resource_metadata_url":"https://localhost:9090/.well-known/oauth-protected-resource","prm_paths":["/.well-known/oauth-protected-resource","/.well-known/oauth-protected-resource/mcp"]}
  ```
  Same message after the Entra YAML above (grep `scopes_supported`; the array is the proof the list left config):
  ```json
  {"level":"INFO","msg":"registered OAuth protected resource metadata endpoint","resource":"https://mcp.example.com/mcp","issuers":["https://login.microsoftonline.com/00000000-0000-0000-0000-000000000000/v2.0"],"scopes_supported":["openid","offline_access","https://mcp.example.com/mcp/access_as_user"],"bearer_methods_supported":["header"],"resource_metadata_url":"https://mcp.example.com/.well-known/oauth-protected-resource","prm_paths":["/.well-known/oauth-protected-resource","/.well-known/oauth-protected-resource/mcp"]}
  ```
- That line is `INFO`. `log_level: warn` or `error` hides it. You do not need `debug` to see it at the shipped default.
- If that line is missing at `info`, this process is not in oauth mode (`static` and `disabled` do not emit it), or it is an older build.
- Verify the live PRM endpoint returns the same document. Both paths return the same JSON:
  ```bash
  curl https://localhost:9090/.well-known/oauth-protected-resource
  curl https://localhost:9090/.well-known/oauth-protected-resource/mcp
  ```
  The bare path is what `WWW-Authenticate` advertises on a 401. The `/mcp`-suffixed path is the RFC 9728 §3.1 canonical path used by clients that construct it directly (for example, the Automatic OAuth discovery feature in Solace Agent Mesh).
- Verify the MCP client supports OAuth (Claude Code and Claude Desktop do)
- Check that the `resource_url` matches the URL the client is connecting to

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

### "invalid_redirect_uri" with Pre-Registered Client

- The redirect URI in the authorization request does not match what is registered in the IdP
- Verify that `http://localhost:<port>/callback` is in the client's valid redirect URIs, where `<port>` matches `--callback-port`
- Some IdPs require an exact match — wildcards may not be supported for pre-registered clients

### "invalid_client" with Pre-Registered Client

- The `--client-id` does not match any client in the IdP
- If using a confidential client, the client secret may be incorrect — re-add the server with `claude mcp add ... --client-secret` to re-enter it
- Verify the client is not disabled or expired in the IdP

### "You are not authorized to use this tool." from a Valid User (Mode 3)

This is a tool-authorization denial. The token authenticated successfully, but the server's claim-based policy did not grant the tool. The caller-facing message is deliberately generic — grep the server logs for a `msg: "tool authorization"` line at the same `correlation_id` to see why:

- `decision_reason: "missing_claim"` with `expected_claim: "<name>"` — the token had no claim by that name at the top level of the JWT. Common causes, in order of likelihood: (a) the IdP's memberships mapper is not applied to the caller's client scope (fix at the IdP); (b) `groups_claim_name` in the server configuration does not match the claim the IdP actually emits (fix at the server); (c) the IdP emits memberships inside a nested object (for example, `authorization.roles`) — the server only reads top-level claims, so flatten the memberships into a top-level claim with an IdP mapper.
- `decision_reason: "not_permitted"` with `matched_groups: []` — the claim was present but none of the caller's memberships grant the tool. Fix by adding the tool to a group the caller is a member of, or adding the caller to a group that already grants it.

`list-brokers` is exempt and always succeeds for any authenticated caller — a successful `list-brokers` call in the same session as a denied tool call confirms the token itself is valid and it is the tool-authorization policy that is denying. See [Tool authorization](configuration.md#tool-authorization) for the full audit-line schema.