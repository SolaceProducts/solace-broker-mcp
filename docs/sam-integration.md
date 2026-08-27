# Connecting solace-broker-mcp to Solace Agent Mesh

This guide shows how to connect this MCP server to Solace Agent Mesh (the Go `sam` CLI, 2.x)
by registering it as an **MCP connector** and assigning it to an agent from the
Agent Mesh UI. For general MCP support in Agent Mesh — connection types, tool
filtering, auth options, TLS — see the bundled docs (`sam docs`) or
[docs.solace.com](https://docs.solace.com).

> **This example runs Agent Mesh locally.** `sam run --embedded` starts an
> in-process dev event broker that Agent Mesh uses for its own internal
> agent-to-agent messaging. That dev broker is unrelated to the Solace brokers
> this MCP server monitors — the MCP server still connects to your real
> broker(s) over SEMP per `broker-config.yaml`. For a team or production
> deployment you run Agent Mesh against your own broker; the connector and agent steps below are identical.

## Prerequisites

- The Go `sam` CLI installed (`sam --version` reports 2.x).
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

**2. Start the MCP server** (listens on `:9090`, endpoint `/mcp`):

```bash
go run ./cmd/server
```

**3. Start Agent Mesh locally** — allow connectors to reach a loopback MCP URL:

```bash
SAM_PLATFORM_ALLOW_PRIVATE_MCP=true sam run --embedded
```

By default Agent Mesh blocks connectors pointing at loopback or private
addresses (SSRF protection), so discovering `localhost:9090` fails without this
flag. It is only needed for local development — a production MCP server has a
routable URL.

The embedded runtime starts an in-process broker and serves the web UI at
**http://127.0.0.1:8800** by default (health probe on `:8090`). Open the UI and
connect an LLM provider on first launch — this becomes the `general` model alias
your agent uses.

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
  broker tools to answer questions about broker status, queues, clients, and
  redundancy, and to perform management actions when asked.
  ```

- In the **Connectors** section, select **Edit** and add the connector from
  step 4 — it appears with its discovered tool count. (You can also attach it
  later by editing an existing agent.)
- Select **Create and Deploy**.

**6. Verify** — from the UI or the terminal.

- **UI:** start a new chat and ask a broker-related question. Select
  `SolaceBrokerAgent` directly, or ask the **Orchestrator**, which delegates to it.

- **CLI:** send a task from the terminal. `sam task send` targets
  `http://localhost:8800` and the `orchestrator` by default; the orchestrator
  delegates to your agent.

  ```bash
  sam task send "List the Solace brokers that are configured."
  ```

  Target a specific agent with `-a/--agent <agent-name>`. No auth token is
  needed for this local no-auth instance. Note: if a tool is backed by an OAuth
  connector, `sam task send` prints an authorization URL and waits for a browser
  login — so OAuth-backed tools cannot complete a fully headless CLI run.

Either way, the agent calls the MCP tools, which query your configured broker
over SEMP.

## Authentication Chain

```
Agent (in Agent Mesh) ─(optional bearer token)→ MCP server ─(basic BROKER_USERNAME/PASSWORD)→ Solace event broker
```

In this local example the agent-to-MCP hop is unauthenticated. When
`mcp_client_auth` is `static` or `oauth`, the agent sends a bearer token that
the connector's **Authentication Type** must supply — and it must equal the
`dev_token` in the MCP server configuration.

## Defining the integration as code

The UI flow above is the quickest path. You can also declare the connector and
agent as YAML and apply them with `sam config apply` — the version-controllable
path for GitOps and automation. That path targets the Platform service and
requires `sam auth login` (Early Access; commands and schema may change). See
the declarative config docs via `sam docs`.
