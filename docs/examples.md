# Examples

Task-oriented examples for connecting clients and using the tools. For the full
per-tool schema reference see [Tools Reference](tools-reference.md); for setup and
deployment see the [README](../README.md) and [User Guide](user-guide.md).

> The server runs as a standalone HTTP service. It has **no stdio transport** and
> cannot be auto-launched as a subprocess. Every client connects over Streamable
> HTTP to an already-running server (default `http://localhost:9090/mcp`). Start
> the server first.

## Table of Contents

- [Connect Claude Desktop](#connect-claude-desktop)
- [Connect Claude Code](#connect-claude-code)
- [Natural-Language Queries](#natural-language-queries)
- [Tool Invocations by Category](#tool-invocations-by-category)
- [Multi-Broker Configuration](#multi-broker-configuration)
- [Static Token (Local Development)](#static-token-local-development)
- [OAuth (Production)](#oauth-production)

## Connect Claude Desktop

Claude Desktop connects to this server as a **remote (HTTP) Model Context
Protocol (MCP) server**, not a stdio subprocess. Two methods:

### Method A — Custom Connector (Recommended)

1. Start the MCP server (binary, Docker, or `go run`).
2. In Claude Desktop: **Settings → Connectors → Add custom connector**.
3. Set the URL to the `/mcp` endpoint, for example `http://localhost:9090/mcp`.
4. If the server runs in `mode: oauth`, Claude Desktop runs the browser login on
   first use (the server advertises its authorization server via
   `/.well-known/oauth-protected-resource`). In `mode: disabled` or `static`, no
   browser flow runs.

No configuration file editing required.

### Method B — `mcp-remote` Bridge

Use the [`mcp-remote`](https://www.npmjs.com/package/mcp-remote) npm bridge when
you prefer file-based configuration. Edit `claude_desktop_config.json`:

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

## Natural-Language Queries

After connecting, ask in plain language. The agent selects the tool and fills
parameters. Representative queries and the shape they return:

| You ask | Tool invoked | Returns (shape) |
|---|---|---|
| "What event brokers are configured?" | `list-brokers` | `{ "brokers": ["prod-broker", "dev-broker"] }` |
| "What's prod-broker's current status?" | `get-broker-status` | envelope with version, uptime, resource and spool utilization |
| "List queues with a backlog on the default VPN" | `list-queues` | envelope `{ "queues": [ { "queueName": ..., "spooledMsgCount": ... }, ... ] }` |
| "Why is orders.q backing up?" | `get-queue-metrics` | envelope `{ "queueMetrics": { "spooledMsgCount": ..., "txUnackedMsgCount": ..., "bindCount": ... } }` |
| "Are there slow subscribers on the default VPN?" | `list-slow-subscribers` | envelope `{ "slowSubscribers": [ ... ] }` (empty array if none) |
| "Are we dropping messages anywhere?" | `get-discard-stats` | `{ "clientDiscards": {...}, "spoolDiscards": {...} }` |
| "Create a queue orders.q in the default VPN" | `create-queue` | envelope `{ "createQueue": { ... } }` (agent confirms first; write tool) |

Read-only tools return their event broker data in a step-keyed envelope. See
[Tools Reference → Output](tools-reference.md#output-the-step-keyed-envelope).

## Tool Invocations by Category

Each block shows the `name` and `arguments` (the `params` of an MCP `tools/call`
request). Replace `prod-broker` with one of your configured aliases (from
`list-brokers`).

**Discovery**

```json
{ "name": "list-brokers", "arguments": {} }
```

**Event Broker Status / Replication**

```json
{ "name": "get-broker-status", "arguments": { "broker": "prod-broker" } }
{ "name": "get-redundancy-status", "arguments": { "broker": "prod-broker" } }
{ "name": "get-replication-status", "arguments": { "broker": "prod-broker", "msgVpnName": "default" } }
```

**Message VPN**

```json
{ "name": "list-vpns", "arguments": { "broker": "prod-broker", "maxResults": 50 } }
{ "name": "get-vpn-status", "arguments": { "broker": "prod-broker", "msgVpnName": "default" } }
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

**Action Tools** (only available when `enable_write_tools: true`; destructive tools
prompt for confirmation through the agent):

```json
{ "name": "clear-queue-stats", "arguments": { "broker": "prod-broker", "msgVpnName": "default", "queueName": "orders.q" } }
{ "name": "delete-queue-messages", "arguments": { "broker": "prod-broker", "msgVpnName": "default", "queueName": "dead-letter.q" } }
{ "name": "clear-client-stats", "arguments": { "broker": "prod-broker", "msgVpnName": "default", "clientName": "consumer-7" } }
{ "name": "disconnect-client", "arguments": { "broker": "prod-broker", "msgVpnName": "default", "clientName": "consumer-7" } }
```

**Management Tools** (Config API; only available when `enable_write_tools: true`;
`update-*`/`delete-*` prompt for confirmation through the agent). Create and
update a configuration object; omitted attributes take event broker defaults on
create or are left unchanged on update:

```json
{ "name": "create-message-vpn", "arguments": { "broker": "prod-broker", "msgVpnName": "orders-vpn", "msgVpnConfig": { "enabled": true, "maxConnectionCount": 100 } } }
{ "name": "update-message-vpn", "arguments": { "broker": "prod-broker", "msgVpnName": "orders-vpn", "msgVpnConfig": { "enabled": false } } }
{ "name": "delete-message-vpn", "arguments": { "broker": "prod-broker", "msgVpnName": "orders-vpn" } }
{ "name": "create-queue", "arguments": { "broker": "prod-broker", "msgVpnName": "default", "queueName": "orders.q", "queueConfig": { "ingressEnabled": true, "egressEnabled": true } } }
{ "name": "update-queue", "arguments": { "broker": "prod-broker", "msgVpnName": "default", "queueName": "orders.q", "queueConfig": { "egressEnabled": false } } }
{ "name": "delete-queue", "arguments": { "broker": "prod-broker", "msgVpnName": "default", "queueName": "orders.q" } }
{ "name": "create-topic-endpoint", "arguments": { "broker": "prod-broker", "msgVpnName": "default", "topicEndpointName": "orders.te", "topicEndpointConfig": { "ingressEnabled": true, "egressEnabled": true } } }
{ "name": "update-topic-endpoint", "arguments": { "broker": "prod-broker", "msgVpnName": "default", "topicEndpointName": "orders.te", "topicEndpointConfig": { "egressEnabled": false } } }
{ "name": "delete-topic-endpoint", "arguments": { "broker": "prod-broker", "msgVpnName": "default", "topicEndpointName": "orders.te" } }
```

## Multi-Broker Configuration

Configure multiple event brokers under `brokers:`; the map key is the alias used as the
`broker` parameter and in `list-brokers` output. Aliases must be 1-63 characters,
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

## Static Token (Local Development)

Server configuration:

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

## OAuth (Production)

Server configuration (HTTPS event broker URLs and issuer enforced in `oauth` mode):

```yaml
mcp_client_auth:
  mode: oauth
  issuer: "https://your-idp.example.com/realms/your-realm"
  audience: "solace-mcp-server"
  resource_url: "https://your-mcp-server.example.com/mcp"

# TLS for the server's own listener. Either terminate TLS here with
# tls_cert_file/tls_key_file, or acknowledge that a proxy/ingress terminates it
# (oauth mode refuses a plaintext listener otherwise). This example assumes an
# upstream terminator; the server then logs a startup WARN while serving plaintext.
tls_terminated_upstream: true

brokers:
  prod-broker:
    url: "https://prod.example.com:1943"
    auth:
      mode: basic
      username: "${BROKER_USERNAME}"
      password: "${BROKER_PASSWORD}"
```

Clients authenticate via the OAuth 2.1 Authorization Code + PKCE flow. Full
identity provider (IdP) setup (Keycloak audience mapper, client registration,
Dynamic Client Registration (DCR)) is in [Authentication](authentication.md).
