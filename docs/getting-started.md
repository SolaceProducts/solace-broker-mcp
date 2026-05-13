# Solace Broker MCP Server

## Overview

The Solace Broker MCP Server is an [MCP (Model Context Protocol)](https://modelcontextprotocol.io/) server that connects AI assistants to Solace PubSub+ event brokers. It exposes broker management and monitoring capabilities as MCP tools, allowing AI agents like Claude to query broker health, inspect queues, diagnose client issues, and monitor message traffic through natural language.

Use cases include:

- **Incident triage** -- ask an AI assistant to check broker health, find queues with backlogs, or identify slow consumers instead of navigating SEMP APIs manually.
- **Operational monitoring** -- get a quick overview of VPN health, client connections, and message rates across multiple brokers from a single conversational interface.
- **Multi-broker management** -- configure connections to multiple brokers and switch between them by name during a conversation.

The server is built in Go using the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)

## Prerequisites

Before installing the Solace Broker MCP Server, ensure you have the following:

| Requirement | Details |
|---|---|
| **Solace PubSub+ broker** | One or more brokers with SEMP management enabled. The server connects to the SEMP management API (typically port 8080 for HTTP or 1943 for HTTPS). |
| **Broker credentials** | A SEMP username and password (basic auth) to access each broker. |
| **Runtime environment** | One of: Docker, a supported OS/architecture for the binary (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64), or Kubernetes. |
| **MCP client** | An MCP-compatible AI client such as Claude Code or Claude Desktop. |
| **OAuth provider** (production only) | An OIDC-compliant identity provider (e.g., Keycloak, Auth0, Okta) is required when `development_mode` is `false`. Not needed for local development or evaluation. |
| **Go 1.25+** (development only) | Only required if building from source. Not needed for binary or Docker deployments. |

## Limitations and Considerations

- **No stdio transport** -- The server runs as a standalone HTTP service and must be started before connecting an MCP client. It cannot be auto-launched as a subprocess by clients like Claude Desktop.
- **Pagination limits** -- List tools return up to 100 results by default and cap at 500 via the `maxResults` parameter. Brokers with more than 500 queues, clients, or VPNs will require multiple queries.
- **OAuth required in production** -- When `development_mode` is `false`, all MCP client connections must present a valid OAuth/JWT token. Plan your identity provider integration before deploying to shared environments.

## Tools

The server exposes 14 read-only tools. Every tool requires a `broker` parameter to identify which configured broker to query.

### Discovery

| Tool | Description |
|---|---|
| `list-brokers` | List all configured broker aliases. Use the returned names as the `broker` parameter in other tools. |

### Broker Health

| Tool | Description |
|---|---|
| `get-broker-health` | Curated broker health snapshot: version, uptime, restart reason, broker-tier scaling limits, system resources, subscription memory, and message-spool state with HA roles and disk utilization. |
| `get-redundancy-status` | Broker redundancy and high-availability status: config/operational status, active-standby role, mate router name, mate link state, and per-virtual-router activity. |

### Message VPN

| Tool | Description |
|---|---|
| `list-vpns` | List all Message VPNs on a broker with their enabled state, connection count, and health status. Default 100 results, max 500. |
| `get-vpn-health` | Health and connection statistics for a specific VPN: enabled state, active connections, subscription count, and service states for SMF, REST, and MQTT. |
| `get-message-rates` | Current and average message/byte throughput rates for a VPN. |

### Queues

| Tool | Description |
|---|---|
| `list-queues` | List queues in a VPN with depth, bind count, and throughput rates. Default 100 results, max 500. |
| `get-queue-metrics` | Detailed metrics for a specific queue: message depth, throughput rates, spool usage, and configuration. Use to diagnose backlogs. |

### Clients

| Tool | Description |
|---|---|
| `list-clients` | List active client connections in a VPN with connection details, uptime, and slow subscriber status. Default 100 results, max 500. |
| `get-client-details` | Performance metrics for a specific connected client: message rates, slow subscriber status, and egress discard counts. Use to diagnose slow consumers. |
| `list-client-subscriptions` | Topic subscriptions for a specific client. Default 100 results, max 500. |

### REST Delivery Points

| Tool | Description |
|---|---|
| `list-rdps` | List all RDPs in a VPN with enabled state, up/down status, and last failure reason. Default 100 results, max 500. |
| `get-rdp-status` | Detailed RDP status: enabled state, up/down status, client name, last failure reason, queue bindings, and REST consumer status. |

### DMR (Dynamic Message Routing)

| Tool | Description |
|---|---|
| `get-dmr-status` | DMR cluster status: enabled state, uptime, and all link statuses. Use to diagnose mesh connectivity issues. |

## Recommended Environments

### Authentication

There are two authentication boundaries to configure:

**MCP Client → MCP Server (first hop)**

How MCP clients authenticate to the server. Three modes are available depending on your deployment:

- **Open (development only)** -- set `development_mode: true` with no static token. No credentials required; intended for local evaluation only.
- **Static token (development only)** -- set `development_mode: true` and configure a `static_token`. Clients pass this token as a Bearer token in the `Authorization` header.
- **OAuth / OIDC (production)** -- set `development_mode: false` and configure an identity provider (issuer, audience, resource URL) in the `client_auth` section. Clients must present a valid JWT.

For detailed setup instructions, see the [Client Authentication](client-authentication.md) guide.

**MCP Server → Broker (second hop)**

How the server authenticates to each Solace broker. Configured per broker in the config file:

- **Basic auth** -- provide a `username` and `password` for the broker.
- **Bearer token** -- provide a bearer `token` when the broker is configured for token-based authentication.

### Deployment Targets

| Environment | Notes |
|---|---|
| **Local / laptop** | Run the binary directly or via Docker. Use `development_mode: true` to skip OAuth setup. |
| **Docker / Docker Compose** | Multi-platform images available at `ghcr.io/solacedev/solace-broker-mcp`. Built-in health check. |
| **Kubernetes** | Example manifests in `deploy/kubernetes/`. Runs as non-root (UID 65534) with a read-only root filesystem. Health probes preconfigured. |
| **Bare metal / VM** | Statically-linked binary with no external dependencies. Handles SIGTERM/SIGINT for graceful shutdown. |

## Troubleshooting

### Server won't start

- **Config file not found** -- The server looks for the yaml configuration file in this order: `CONFIG_FILE` env var, `/etc/mcp-server/config.yaml`, then `./broker-config.yaml`. Set `CONFIG_FILE` explicitly if your file is elsewhere.
- **TLS misconfiguration** -- Both `tls_cert_file` and `tls_key_file` must be set together. Providing only one is a startup error.
- **OAuth config missing** -- When `development_mode` is `false`, the `client_auth` section (issuer, audience, resource_url) is required. For local testing, set `development_mode: true`.

### Cannot connect to broker

- **SEMP not enabled** -- Verify the broker's SEMP management interface is accessible at the configured URL (e.g., `http://broker:8080/SEMP`).
- **Authentication failure** -- Check that credentials in the `.env` file are correct. For basic auth, verify both `username` and `password`. For bearer mode, verify the `token`.
- **TLS certificate errors** -- If the broker uses a self-signed certificate, set `insecure_skip_verify: true` in the broker config (development only -- do not use in production).

### Tool returns an error

Tool errors include structured fields to help diagnose the problem:

| Field | Meaning |
|---|---|
| `error` | Human-readable error message. |
| `retryable` | `true` if retrying may succeed (rate limits, transient failures). |
| `status` | HTTP status code from the SEMP API. |
| `operation` | The SEMP operation that failed (e.g., `monitor/getMsgVpnQueues`). |

Common causes:
- **404** -- The specified VPN, queue, client, or RDP does not exist. Check the name for typos.
- **401 / 403** -- Broker credentials lack permission for the requested operation. Verify the SEMP user has monitor-level access.
- **429 / 503** -- Rate limiting or broker overload. These are retryable -- the server will retry automatically based on the configured retry policy.

### Health check fails

The server exposes a health endpoint and a CLI flag:

```bash
# HTTP health check
curl http://localhost:9090/health
# Expected: {"status": "ok"}

# Binary health check (useful for scripts and container probes)
./solace-broker-mcp --health
# Exit code 0 = healthy, 1 = unhealthy
```

The `--health` flag probes the running server's `/health` endpoint — the server must already be running for it to succeed. It reads the config file to determine the port and TLS settings, so ensure the same config file is accessible to both the server process and the health probe.

If the health check fails, verify the server process is running and the configured port is not in use by another process.

### Logging

The server writes structured JSON logs to stderr. Adjust the log level in the config file:

```yaml
log_level: debug   # debug, info, warn, error
```

Credentials are automatically redacted in all log output. Every tool invocation is logged with the tool name, target broker, status, and duration.
