# Examples

Task-oriented examples for connecting clients and using the tools. For the full
per-tool schema reference see [Tools Reference](tools-reference.md); for setup and
deployment see the [README](../README.md) and [User Guide](user-guide.md).

> The server runs as a standalone HTTP service — it has **no stdio transport** and
> cannot be auto-launched as a subprocess. Every client connects over Streamable
> HTTP to an already-running server (default `http://localhost:9090/mcp`). Start
> the server first.

## Table of Contents

- [Connect Claude Desktop](#connect-claude-desktop)
- [Connect Claude Code](#connect-claude-code)
- [Natural-language queries](#natural-language-queries)
- [Tool invocations by category](#tool-invocations-by-category)
- [Multi-broker configuration](#multi-broker-configuration)
- [Static token (local development)](#static-token-local-development)
- [OAuth (production)](#oauth-production)

## Connect Claude Desktop

Claude Desktop connects to this server as a **remote (HTTP) MCP server**, not a
stdio subprocess. Two methods:

### Method A — Custom Connector (recommended)

1. Start the MCP server (binary, Docker, or `go run`).
2. In Claude Desktop: **Settings → Connectors → Add custom connector**.
3. Set the URL to the `/mcp` endpoint, for example `http://localhost:9090/mcp`.
4. If the server runs in `mode: oauth`, Claude Desktop runs the browser login on
   first use (the server advertises its authorization server via
   `/.well-known/oauth-protected-resource`). In `mode: disabled` or `static`, no
   browser flow runs.

No config file editing required.

### Method B — `mcp-remote` bridge

Use the [`mcp-remote`](https://www.npmjs.com/package/mcp-remote) npm bridge when
you prefer file-based config. Edit `claude_desktop_config.json`:

- **macOS:** `~/Library/Application Support/Claude/claude_desktop_config.json`
- **Windows:** `%APPDATA%\Claude\claude_desktop_config.json`

(Linux is not an officially supported Claude Desktop platform; on Linux use
[Claude Code](#connect-claude-code) instead.)

```json
{
  "mcpServers": {
    "solace-broker": {
      "command": "npx",
      "args": ["-y", "mcp-remote", "http://localhost:9090/mcp"]
    }
  }
}
```

For `mode: static`, pass the dev token as a header via `mcp-remote`:

```json
{
  "mcpServers": {
    "solace-broker": {
      "command": "npx",
      "args": [
        "-y", "mcp-remote", "http://localhost:9090/mcp",
        "--header", "Authorization: Bearer my-secret-dev-token-123"
      ]
    }
  }
}
```

Restart Claude Desktop after editing the file. Requires Node.js (`npx`).

## Connect Claude Code

```bash
# No auth (mode: disabled)
claude mcp add solace-broker --transport http http://localhost:9090/mcp

# Static dev token (mode: static)
claude mcp add solace-broker --transport http http://localhost:9090/mcp \
  -H "Authorization: Bearer my-secret-dev-token-123"

# OAuth (mode: oauth) — browser login on first use
claude mcp add solace-broker --transport http http://localhost:9090/mcp \
  --client-id mcp-client --callback-port 8081
```

See [Authentication](authentication.md) for full OAuth setup.

## Natural-language queries

Once connected, ask in plain language. The agent selects the tool and fills
parameters. Representative queries and the shape they return:

| You ask | Tool invoked | Returns (shape) |
|---|---|---|
| "What brokers are configured?" | `list-brokers` | `{ "brokers": ["prod-broker", "dev-broker"] }` |
| "Is prod-broker healthy?" | `get-broker-status` | envelope with version, uptime, resource and spool utilization |
| "List queues with a backlog on the default VPN" | `list-queues` | envelope `{ "queues": [ { "queueName": ..., "spooledMsgCount": ... }, ... ] }` |
| "Why is orders.q backing up?" | `get-queue-metrics` | envelope `{ "queueMetrics": { "spooledMsgCount": ..., "txUnackedMsgCount": ..., "bindCount": ... } }` |
| "Are there slow subscribers on the default VPN?" | `list-slow-subscribers` | envelope `{ "slowSubscribers": [ ... ] }` (empty array if none) |
| "Are we dropping messages anywhere?" | `get-discard-stats` | `{ "clientDiscards": {...}, "spoolDiscards": {...} }` |

Read-only tools return their broker data in a step-keyed envelope — see
[Tools Reference → Output](tools-reference.md#output-the-step-keyed-envelope).

## Tool invocations by category

Each block shows the `name` and `arguments` (the `params` of an MCP `tools/call`
request). Replace `prod-broker` with one of your configured aliases (from
`list-brokers`).

**Discovery**

```json
{ "name": "list-brokers", "arguments": {} }
```

**Broker status / replication**

```json
{ "name": "get-broker-status", "arguments": { "broker": "prod-broker" } }
{ "name": "get-redundancy-status", "arguments": { "broker": "prod-broker" } }
{ "name": "get-replication-status", "arguments": { "broker": "prod-broker", "msgVpnName": "default" } }
```

**Message VPN**

```json
{ "name": "list-vpns", "arguments": { "broker": "prod-broker", "maxResults": 50 } }
{ "name": "get-vpn-health", "arguments": { "broker": "prod-broker", "msgVpnName": "default" } }
{ "name": "get-message-rates", "arguments": { "broker": "prod-broker", "msgVpnName": "default" } }
```

**Queues**

```json
{ "name": "list-queues", "arguments": { "broker": "prod-broker", "msgVpnName": "default" } }
{ "name": "get-queue-metrics", "arguments": { "broker": "prod-broker", "msgVpnName": "default", "queueName": "orders.q" } }
```

**Clients**

```json
{ "name": "list-clients", "arguments": { "broker": "prod-broker", "msgVpnName": "default" } }
{ "name": "get-client-details", "arguments": { "broker": "prod-broker", "msgVpnName": "default", "clientName": "consumer-7" } }
{ "name": "list-client-subscriptions", "arguments": { "broker": "prod-broker", "msgVpnName": "default", "clientName": "consumer-7" } }
{ "name": "list-slow-subscribers", "arguments": { "broker": "prod-broker", "msgVpnName": "default" } }
```

**REST Delivery Points**

```json
{ "name": "list-rdps", "arguments": { "broker": "prod-broker", "msgVpnName": "default" } }
{ "name": "get-rdp-status", "arguments": { "broker": "prod-broker", "msgVpnName": "default", "restDeliveryPointName": "webhook-rdp" } }
```

**Discards**

```json
{ "name": "get-discard-stats", "arguments": { "broker": "prod-broker" } }
{ "name": "list-queue-discards", "arguments": { "broker": "prod-broker", "msgVpnName": "default" } }
```

**Action tools** (only available when `enable_write_tools: true`; destructive tools
prompt for confirmation through the agent):

```json
{ "name": "clear-queue-stats", "arguments": { "broker": "prod-broker", "msgVpnName": "default", "queueName": "orders.q" } }
{ "name": "delete-queue-messages", "arguments": { "broker": "prod-broker", "msgVpnName": "default", "queueName": "dead-letter.q" } }
{ "name": "clear-client-stats", "arguments": { "broker": "prod-broker", "msgVpnName": "default", "clientName": "consumer-7" } }
{ "name": "disconnect-client", "arguments": { "broker": "prod-broker", "msgVpnName": "default", "clientName": "consumer-7" } }
```

## Multi-broker configuration

Configure multiple brokers under `brokers:`; the map key is the alias used as the
`broker` parameter and in `list-brokers` output. Aliases must be 1–63 characters,
letters/digits/hyphens only, starting and ending alphanumeric, and are compared
case-insensitively.

```yaml
mcp_client_auth:
  mode: disabled        # local development only

brokers:
  prod-broker:
    url: "https://prod.example.com:1943"
    auth:
      mode: basic
      username: "${PROD_BROKER_USERNAME}"
      password: "${PROD_BROKER_PASSWORD}"
  dev-broker:
    url: "http://dev.example.com:8080"
    auth:
      mode: basic
      username: "${DEV_BROKER_USERNAME}"
      password: "${DEV_BROKER_PASSWORD}"
```

Then query each by alias:

```
List the queues on prod-broker's default VPN.
Compare message rates between prod-broker and dev-broker.
```

```json
{ "name": "list-queues", "arguments": { "broker": "prod-broker", "msgVpnName": "default" } }
{ "name": "get-message-rates", "arguments": { "broker": "dev-broker", "msgVpnName": "default" } }
```

## Static token (local development)

Server config:

```yaml
mcp_client_auth:
  mode: static
  dev_token: "${DEV_TOKEN}"   # or a literal string for quick tests

brokers:
  dev-broker:
    url: "http://dev.example.com:8080"
    auth:
      mode: basic
      username: "${BROKER_USERNAME}"
      password: "${BROKER_PASSWORD}"
```

`.env`:

```env
DEV_TOKEN=my-secret-dev-token-123
BROKER_USERNAME=admin
BROKER_PASSWORD=admin
```

Connect with the matching bearer token (see [Claude Code](#connect-claude-code) or
[Claude Desktop Method B](#method-b--mcp-remote-bridge)).

## OAuth (production)

Server config (HTTPS broker URLs and issuer enforced in `oauth` mode):

```yaml
mcp_client_auth:
  mode: oauth
  issuer: "https://your-idp.example.com/realms/your-realm"
  audience: "solace-mcp-server"
  resource_url: "https://your-mcp-server.example.com/mcp"

brokers:
  prod-broker:
    url: "https://prod.example.com:1943"
    auth:
      mode: basic
      username: "${BROKER_USERNAME}"
      password: "${BROKER_PASSWORD}"
```

Clients authenticate via the OAuth 2.1 Authorization Code + PKCE flow. Full IdP
setup (Keycloak audience mapper, client registration, DCR) is in
[Authentication](authentication.md).
