# Connecting solace-broker-mcp to Solace Agent Mesh

This guide shows how to connect this Model Context Protocol (MCP) server to the Solace Agent Mesh desktop
app by registering it as an **MCP connector** and assigning it to an agent from
the Agent Mesh UI.

_Validated against Solace Agent Mesh v2.307.3 (macOS), 2026-08. UI navigation may
differ in later versions._

> **This example runs Agent Mesh locally.** The desktop app starts an in-process
> dev event broker that Agent Mesh uses for its own internal agent-to-agent
> messaging. That dev event broker is unrelated to the Solace event brokers this MCP server
> monitors — the MCP server still connects to your real event broker(s) over SEMP per
> `broker-config.yaml`. For a team or production deployment you point Agent Mesh
> at your own event broker; the connector and agent steps below are identical.

## Prerequisites

- The Solace Agent Mesh desktop app installed. See the
  [Solace Agent Mesh docs](https://docs.solace.com/Agent-Mesh/agent-mesh.htm) for
  install and getting-started guides.
- Ability to run this MCP server locally with configured `broker-config.yaml`
  and `.env` files. See [Quickstart](../README.md#quickstart).

## Steps

**1. Configure client auth on the MCP server** — `broker-config.yaml`. This
local example uses no client auth (the server binds to loopback only):

```yaml
mcp_client_auth:
  mode: disabled
```

For a shared or production deployment, use `mode: static` (a bearer token) or
`mode: oauth`, and set the connector's **Authentication Type** in step 4 to
match. See [authentication.md](authentication.md).

**2. Start the MCP server** — in non-OAuth modes it binds loopback by default,
serving the MCP endpoint at `http://127.0.0.1:9090/mcp`:

```bash
go run ./cmd/server
```

**3. Open the Agent Mesh desktop app** — the app auto-starts its in-process
event broker and runtime; no command to run. On first launch, connect an LLM provider —
this becomes the `general` model alias your agent uses.

To reach a loopback MCP server locally, set `SAM_PLATFORM_ALLOW_PRIVATE_MCP=true`
in the environment the app launches from. By default Agent Mesh blocks connectors
pointing at loopback or private addresses (SSRF protection), so discovering
`localhost:9090` fails without it. On macOS, a GUI app launched from Finder won't
see your shell env, so set it for the login session:

```bash
launchctl setenv SAM_PLATFORM_ALLOW_PRIVATE_MCP true
```

Then fully quit the app (Cmd+Q — closing the window isn't enough) and relaunch it;
an already-running app keeps its old environment and won't pick up the new value.

This is needed whenever the MCP server's URL resolves to a loopback or private
(RFC1918/RFC4193) address — local development, and also self-hosted deployments
where Agent Mesh reaches the server over a cluster-internal or otherwise private
address. It relaxes only private and loopback addresses; link-local (including
cloud instance metadata) stays blocked.

**4. Add the MCP server as a connector** — in the UI left nav, go to
**Builder** > **Connectors** > **Create Connector**, then the **Custom** tab >
**Remote MCP**. This opens the two-step **Create MCP Connector** wizard. On step
1 (**Configure Connector**):

| Field | Value |
|---|---|
| Connector Name | `Solace Broker MCP` |
| Description | e.g. `SEMP-backed Solace event broker tools.` |
| MCP Server URL | `http://localhost:9090/mcp` |
| Connection Type | `Streamable HTTP` |
| Authentication Type | `No Authentication` (matches `mode: disabled` in step 1) |

Use `http://` for the local server — it serves plaintext on loopback; the
field's `https://` example is for remote servers.

On step 2 (**Select Tools**), Agent Mesh connects and discovers the server's
tools; select the ones to expose (or all), then save.

**5. Create and deploy an agent** — go to **Builder** > **Agent Management** >
**Add Agent** > **Create New Agent** > **Create Manually**:

- **Name**: `SolaceBrokerAgent`
- **Description**: `Inspects and manages Solace event brokers via solace-broker-mcp.`
- **Instructions** (min 100 characters):

  ```text
  You manage and inspect Solace event brokers via SEMP-backed MCP tools. Use the
  event broker tools to answer questions about event broker status, queues, clients, and
  redundancy, and to perform configuration and action updates when asked.
  ```

- In the **Connectors** section, select **Edit** and add the connector from
  step 4 — it appears with its discovered tool count. (You can also attach it
  later by editing an existing agent.)
- Select **Create and Deploy**.

**6. Verify** — start a new chat and ask an event broker-related question. Select
`SolaceBrokerAgent` directly, or ask the **Orchestrator**, which delegates to it.
The agent calls the MCP tools, which query your configured event broker over SEMP.
