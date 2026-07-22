# User Guide

The Solace Event Broker MCP Server is an [MCP (Model Context Protocol)](https://modelcontextprotocol.io/) server that connects AI assistants to Solace event brokers. It exposes broker management and monitoring capabilities as MCP tools, allowing AI agents like Claude to query broker status, inspect queues, diagnose client issues, and monitor message traffic through natural language.

Application scenarios:

- **Incident triage** — Query event broker status, queue activity, and slow consumers using natural language queries instead of direct SEMP API calls.
- **Operational monitoring** — Monitor VPN status, client connections, and message rates across multiple brokers through a conversational interface.
- **Multi-broker management** — Configure multiple event broker connections and address them by alias in queries.

Built with Go using the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk).

## Table of Contents

- [Prerequisites](#prerequisites)
- [Limitations and Considerations](#limitations-and-considerations)
- [Quick Start](#quick-start)
  - [Deployment](#deployment)
  - [Connecting an MCP Client](#connecting-an-mcp-client)
  - [Example Queries](#example-queries)
- [Tools Reference](#tools-reference)
- [Troubleshooting](#troubleshooting)

## Prerequisites

The Solace Event Broker MCP Server requires:

| Requirement | Details |
|---|---|
| **Solace event broker** | One or more brokers with SEMP management enabled. The server connects to the SEMP management API (typically port 8080 for HTTP or 1943 for HTTPS). |
| **SEMPv1+v2 reachability** | The machine running the MCP server must have network access to both the SEMPv1 (`/SEMP`) and SEMPv2 (`/SEMP/v2`) endpoints on each broker's SEMP management port. |
| **Broker credentials** | A SEMP username and password (basic auth) to access each broker. |
| **Runtime environment** | One of: Docker, a supported OS/architecture for the binary (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64), or Kubernetes. |
| **MCP client** | An MCP-compatible AI client such as Claude Code or Claude Desktop. |
| **OAuth provider** (production only) | An OIDC-compliant identity provider (for example, Keycloak, Auth0, Okta) is required when `mcp_client_auth.mode` is `oauth`. An OAuth provider is not required when `mode` is `disabled` or `static` (local development). |
| **Go 1.25+** (development only) | Only required if building from source. Go is not required for binary or Docker deployments. |

## Limitations and Considerations

- **No stdio transport** — The server runs as a standalone HTTP service and must be started before connecting an MCP client. It cannot be auto-launched as a subprocess by clients like Claude Desktop.
- **Pagination limits** — List tools return up to 100 results by default and cap at 500 via the `maxResults` parameter. Brokers with more than 500 queues, clients, or VPNs require multiple queries.
- **OAuth required in production** — Production deployments use `mcp_client_auth.mode: oauth`; the boot banner flags `disabled` and `static` as insecure modes. All MCP client connections must present a valid OAuth/JWT token. Plan your identity provider integration before deploying to shared environments.

## Quick Start

### Deployment

The server can be deployed in three ways. See the [README](../README.md#quickstart) for detailed setup instructions:

| Environment | Notes |
|---|---|
| **Binary** | Single executable with no dependencies; suitable for local development and VM deployment |
| **Docker** | Multi-platform images available at `ghcr.io/solacedev/solace-broker-mcp`; built-in health check |
| **Development** | Run from source with Go for development and testing |

All methods use the same YAML configuration file and `.env` credentials. Configuration must be completed before starting the server.

### Connecting an MCP Client

Once the server is running, connect Claude Code to it:

```bash
claude mcp add solace-broker --transport http http://localhost:9090/mcp
```

For details on securing this connection with a static token or OAuth/OIDC, see [Authentication](authentication.md).

### Example Queries

After connecting, try these example queries:

**Check event broker status:**
```
Get the status of my-broker
```

**List queues:**
```
Show me all queues in the default VPN on my-broker
```

**Diagnose a slow consumer:**
```
Get client details for client-name in default VPN on my-broker
```

**Monitor message rates:**
```
What are the current message rates for default VPN on my-broker?
```

## Tools Reference

The server exposes 19 read-only tools plus 16 write tools (35 total when write tools are enabled). For full per-tool parameters, output shape, and example invocations, see the [Tools Reference](tools-reference.md); this section is the narrative overview. All broker-querying tools require a `broker` parameter to identify which configured event broker to query; `list-brokers` is the exception and returns the available event broker aliases. The write tools split into four action-API tools (operational actions against live objects) and 12 Config-API management tools (create/update/delete for Message VPNs, queues, topic endpoints, and REST delivery points); all are gated behind `enable_write_tools` (default off). Tools marked destructive via the MCP `destructiveHint` annotation (including `delete-queue-messages`, `disconnect-client`, and the service-affecting `update-*`/`delete-*` management tools) have descriptions that instruct the calling LLM to obtain explicit user confirmation before invocation.

### Discovery

| Tool | Description |
|---|---|
| `list-brokers` | List all configured event broker aliases. Use the returned names as the `broker` parameter in other tools. |

### Event Broker Status

| Tool | Description |
|---|---|
| `get-broker-status` | Curated point-in-time broker status snapshot: edition and version, uptime and restart reason, broker-tier scaling limits and resource headroom, memory utilization, and message-spool state with HA roles and disk utilization. On hardware appliances it additionally returns a `hardwareDetails` section with chassis identity, CPU, memory, power, disks, and blade inventory. Reports raw state, not a pass/fail verdict. |
| `get-redundancy-status` | Event broker redundancy and high-availability status: config/operational status, active-standby role, mate router name, mate link state, and per-virtual-router activity. |

### Replication

| Tool | Description |
|---|---|
| `get-replication-status` | Replication state for a Message VPN: role, sync eligibility, bridge status, transaction mode, and queued-message counts. |

### Message VPN

| Tool | Description |
|---|---|
| `list-vpns` | List all Message VPNs on an event broker with their enabled state, connection count, and status. Default 100 results, max 500. |
| `get-vpn-status` | Operational status and connection statistics for a specific VPN: enabled state, active connections, subscription count, and service states for AMQP, MQTT, REST, and SMF. |
| `get-message-rates` | Current and average message/byte throughput rates for a VPN. |

### Queues

| Tool | Description |
|---|---|
| `list-queues` | List queues in a VPN with cumulative spooled count (`spooledMsgCount`, lifetime — not live depth), bind count, and throughput rates. Default 100 results, max 500. |
| `get-queue-metrics` | Detailed metrics for a specific queue. Returns `liveDepth.currentMsgCount` — the **authoritative current queue depth** (messages in the queue right now, decreases as they are consumed; sourced from SEMPv1) — plus a `queueMetrics` block with throughput rates, spool usage, configuration, and cumulative counters. Note `queueMetrics.spooledMsgCount` is a lifetime counter (messages ever spooled), not the current depth. |

### Clients

| Tool | Description |
|---|---|
| `list-clients` | List active client connections in a VPN with connection details, uptime, and slow subscriber status. Default 100 results, max 500. |
| `get-client-details` | Performance metrics for a specific connected client: message rates, slow subscriber status, and egress discard counts. Use to diagnose slow consumers. |
| `list-client-subscriptions` | Topic subscriptions for a specific client. Default 100 results, max 500. |
| `list-slow-subscribers` | Filtered list of clients in a VPN flagged with the broker's slow-subscriber field (server-side `where` filter). Narrow signal — catches direct-messaging or replication-bridge backpressure; does NOT flip for slow guaranteed-message consumers (slow to ACK). For those, use `list-queues` / `get-queue-metrics`. Default 100 results, max 500. |

### REST Delivery Points

| Tool | Description |
|---|---|
| `list-rdps` | List all RDPs in a VPN with enabled state, up/down status, and last failure reason. Default 100 results, max 500. |
| `get-rdp-status` | Detailed RDP status: enabled state, up/down status, client name, last failure reason, queue bindings, and REST consumer status. |

### Bridges

| Tool | Description |
|---|---|
| `list-bridges` | List bridges in a VPN with enabled state, inbound/outbound connection state, and last inbound failure reason. Default 100 results, max 500. |
| `get-bridge-status` | Detailed status for a single bridge: enabled state, inbound/outbound connection state, last inbound failure reason, uptime, remote VPN/broker, connection establisher, and failure category. Bridges are identified by `bridgeName` + `bridgeVirtualRouter`. |

### Discards

| Tool | Description |
|---|---|
| `get-discard-stats` | Broker-wide or per-VPN discard aggregates: client-level ingress/egress discards plus broker-wide spool discards (native SEMPv1). Per-VPN scope returns client-level discards only — the broker exposes no per-VPN spool breakdown via SEMPv1. |
| `list-queue-discards` | Per-queue discard counters for a VPN: TTL-expired, max-redelivery, spool-quota-exceeded, and other discard categories. Complements `get-discard-stats` with queue-level granularity. Default 100 results, max 500. |

### Actions

These tools modify broker state via the SEMPv2 action API. There is **one tool per action** so each tool's behavior is unambiguous: the destructive tools carry the MCP `destructiveHint` annotation and a description that instructs the calling LLM to obtain explicit user confirmation — restating the target (broker, VPN, queue or client) and the effect — before invocation; the non-destructive stats-reset tools do not. The tool manager logs a WARNING line on every destructive invocation for audit.

**Naming convention.** Action-API tools use `<verb>-<resource>-<object>` (`delete-queue-messages`, `clear-queue-stats`, `disconnect-client`, `clear-client-stats`). The Config-API management tools ([below](#management-config-api)) use `<verb>-<object>` — a `create-`, `update-`, or `delete-` prefix on `message-vpn`, `queue`, or `topic-endpoint`. Action tools run an operational action against a live object; management tools change configuration.

**Disabled by default.** These four action tools — together with the nine Config-API management tools below — are write tools (they change broker state) and are gated behind the server-level `enable_write_tools` flag. With the default (`false`) they are not registered with the MCP server and do not appear in `tools/list` — clients see only the read-only tool set. Set `enable_write_tools: true` in the YAML config to expose them. This is independent of `mcp_client_auth.mode`: an authenticated client still cannot invoke these tools when the flag is off, because the server never registers them.

`enable_write_tools` is the only enforced control. `destructiveHint` and the confirmation text in tool descriptions are hints, not enforced by the MCP protocol — whether the user is actually prompted depends on the client and the model.

| Tool | Destructive | Description |
|---|---|---|
| `delete-queue-messages` | **Yes** | Permanently delete all spooled messages from a queue. Irreversible — deleted messages cannot be recovered. Requires user confirmation before invocation. Use after confirmed intent to drain a queue (e.g. clearing a dead-letter backlog). |
| `clear-queue-stats` | No | Reset a queue's statistics counters. Non-destructive: affects monitoring counters only, not spooled messages or delivery. |
| `disconnect-client` | **Yes** | Forcibly disconnect a connected client. Service-impacting — terminates the session; the client must reconnect. Requires user confirmation before invocation. Common use: disconnect a slow subscriber identified via `list-slow-subscribers` or `get-client-details`. |
| `clear-client-stats` | No | Reset a client's per-connection statistics counters. Non-destructive: affects monitoring counters only, does not disconnect the client. |

### Management (Config API)

These tools create, update, and delete broker configuration objects via the SEMPv2 config API, and are gated behind `enable_write_tools` alongside the action tools above. `create-*` is additive; `update-*` applies a partial (PATCH) update, changing only the fields supplied; `delete-*` removes the object. `update-*` and `delete-*` can be service-affecting (for example, disabling a VPN drops its client connections) and both carry the `destructiveHint` annotation; `create-*` does not. Every tool's description instructs the calling LLM to obtain explicit user confirmation — restating the target and effect — before invocation. Create and update tools accept a config object (`msgVpnConfig`, `queueConfig`, `topicEndpointConfig`); any attribute omitted takes the broker default.

| Tool | Destructive | Description |
|---|---|---|
| `create-message-vpn` | No | Create a Message VPN. |
| `update-message-vpn` | **Yes** | Partially update a Message VPN's attributes (for example, `enabled`, connection limits, spool quota). Service-affecting: disabling a VPN drops its client connections. |
| `delete-message-vpn` | **Yes** | Delete a Message VPN. Fails if it still has active client connections or child endpoints. |
| `create-queue` | No | Create a queue in a VPN. |
| `update-queue` | **Yes** | Partially update a queue's attributes (for example, `egressEnabled`, spool quota, redelivery limit). Service-affecting: disabling egress halts delivery; lowering a spool quota can evict messages. |
| `delete-queue` | **Yes** | Delete a queue and discard any messages still spooled on it. |
| `create-topic-endpoint` | No | Create a topic endpoint in a VPN. |
| `update-topic-endpoint` | **Yes** | Partially update a topic endpoint's attributes. Service-affecting: disabling egress halts delivery; lowering a spool quota can evict messages. |
| `delete-topic-endpoint` | **Yes** | Delete a topic endpoint and discard any messages still spooled on it. |

## Recommended Environments

### Authentication

The server supports open access, static token, and OAuth/OIDC authentication for MCP clients, and basic auth or bearer token for broker connections. See the [Authentication](authentication.md) guide for setup instructions.

## Deployment Targets

| Environment | Notes |
|---|---|
| **Local / laptop** | Run the binary directly or via Docker. Use `mcp_client_auth.mode: disabled` (no auth) or `static` (with a dev token) to skip OAuth setup. |
| **Docker / Docker Compose** | Multi-platform images available at `ghcr.io/solacedev/solace-broker-mcp`. Built-in health check. |
| **Bare metal / VM** | Statically-linked binary with no external dependencies. Handles SIGTERM/SIGINT for graceful shutdown. |

## Troubleshooting

### Server Won't Start

- **Config file not found** — The server looks for the yaml configuration file in this order: `CONFIG_FILE` env var, `/etc/mcp-server/config.yaml`, then `./broker-config.yaml`. Set `CONFIG_FILE` explicitly if the file is in a non-standard location.
- **TLS misconfiguration** — Both `tls_cert_file` and `tls_key_file` must be set together. Providing only one is a startup error.
- **OAuth config missing** — When `mcp_client_auth.mode` is `oauth`, the `issuer`, `audience`, and `resource_url` fields are required. For local testing, set `mcp_client_auth.mode: disabled` or `static`.

### Cannot Connect to Broker

- **SEMP not enabled** — Verify the broker's SEMP management interface is accessible at the configured URL (for example, `http://broker:8080/SEMP`).
- **Authentication failure** — Check that credentials in the `.env` file are correct. For basic auth, verify both `username` and `password`. For bearer mode, verify the `token`.
- **TLS certificate errors** — If the event broker uses a self-signed certificate, enable `insecure_skip_verify` in the broker config. In production (`mcp_client_auth.mode: oauth`) this is refused at startup unless you also set `allow_insecure_broker_tls: true` to acknowledge the risk. See [Configuration](configuration.md) for details.

### Tool Returns an Error

Tool errors include structured fields to help diagnose the problem:

| Field | Meaning | Present when |
|---|---|---|
| `error` | Human-readable error message. | Always |
| `retryable` | `true` if retrying may succeed (rate limits, transient failures). | Always |
| `status` | HTTP status code from the SEMP API. | SEMPv2, SEMPv1, or retries-exhausted (when non-zero) |
| `operation` | The SEMP operation that failed (for example, `monitor/getMsgVpnQueues`). | SEMPv2 |
| `sempStatus` | Broker error status string from `meta.error.status` (for example, `NOT_FOUND`). | SEMPv2, when non-empty |
| `sempCode` | Broker error code from `meta.error.code` (for example, `6`). | SEMPv2, when non-zero |
| `kind` | SEMPv1 error classification: `http`, `execute-fail`, `parse`, `permission`, `limit`, or `unknown`. | SEMPv1 |
| `reasonCode` | SEMPv1 reason code from the broker response. | SEMPv1 `execute-fail` responses |
| `attempts` | Number of attempts made before retries were exhausted. | Retries exhausted |
| `suggestions` | Array of actionable hints for resolving the error. | Any source, when available |

Common causes:
- **404** — The specified VPN, queue, client, or RDP does not exist. Check the name for typos.
- **401 / 403** — Event broker credentials lack permission for the requested operation. Verify the SEMP user has monitor-level access.
- **429** — Rate limiting from a proxy, gateway, or load balancer in front of the broker. (The broker itself does not emit 429 over SEMP.) Retryable — the server retries automatically based on the configured retry policy.
- **503** — The broker is overloaded or out of resources. Retryable — the server retries automatically based on the configured retry policy.

### "Session not found" errors

After restarting the MCP server, "session not found" errors indicate the client session has become stale.

**Solution:** Reconnect the MCP client:

```bash
# Remove old connection
claude mcp remove solace-broker

# Re-add with auth header (if using Mode 2)
claude mcp add solace-broker --transport http http://localhost:9090/mcp \
  -H "Authorization: Bearer <your-dev-token>"
```

### Health Check Fails

The server exposes a health endpoint and a CLI flag:

```bash
# HTTP liveness check (/livez is the canonical liveness endpoint)
curl http://localhost:9090/livez
# Expected: {"status":"alive"}

# /health is retained for backward compatibility (preserves its original body)
curl http://localhost:9090/health
# Expected: {"status":"healthy"}

# Binary health check (useful for scripts and container probes)
./solace-broker-mcp --health
# Exit code 0 = healthy, 1 = unhealthy
```

The `--health` flag probes the running server's `/health` endpoint without requiring curl or network tools. This flag is useful for container health checks, scripts, and environments where curl is not available. The server must be running for the check to succeed. The flag reads the config file to determine the port and TLS settings. Ensure the same config file is accessible to both the server process and the health probe.

If the health check fails, verify the server process is running and the configured port is not in use by another process.
