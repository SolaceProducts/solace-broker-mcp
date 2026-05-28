# Connecting solace-broker-mcp to Solace Agent Mesh (SAM)

This guide covers only the pieces specific to wiring this MCP server into SAM. For SAM's general MCP support — connection types, tool filtering, TLS/SSL config, environment variable passing — see the upstream [MCP Integration tutorial](https://github.com/SolaceLabs/solace-agent-mesh/blob/main/docs/docs/documentation/developing/tutorials/mcp-integration.md).

## Prerequisites

- A working SAM project (`sam init` complete, agents start cleanly)
- This MCP server runnable locally with valid `broker-config.yaml` + `.env` (see the main [Quickstart](../README.md#quickstart))

## Steps

**1. Set a static token on the MCP server** — `broker-config.yaml`:

```yaml
client_auth:
  mode: static
  dev_token: "sam-mcp-dev-token-local-only"  # change for your environment
```

For production, use `mode: oauth` instead — see [authentication.md](authentication.md).

**2. Create the SAM agent** — `<sam-project>/configs/agents/solace_broker_agent.yaml`:

```yaml
!include ../shared_config.yaml

apps:
  - name: solace_broker_agent_app
    app_base_path: .
    app_module: solace_agent_mesh.agent.sac.app
    broker:
      <<: *broker_connection

    app_config:
      namespace: ${NAMESPACE}
      agent_name: "SolaceBrokerAgent"
      display_name: "Solace Broker"
      model: *general_model

      instruction: |
        You manage and inspect Solace event brokers via SEMP-backed MCP tools.

      tools:
        - tool_type: mcp
          connection_params:
            type: streamable-http
            url: "${SOLACE_BROKER_MCP_URL, http://localhost:9090/mcp}"
            headers:
              Authorization: "Bearer ${SOLACE_BROKER_MCP_TOKEN}"

      session_service: *default_session_service
      artifact_service: *default_artifact_service
      agent_card:
        description: "Inspects Solace event brokers via solace-broker-mcp."
        defaultInputModes: ["text"]
        defaultOutputModes: ["text", "file"]
      agent_card_publishing: { interval_seconds: 10 }
      agent_discovery: { enabled: true }
      inter_agent_communication:
        allow_list: ["*"]
        request_timeout_seconds: 60
```

**3. Wire the matching token in SAM's `.env`**:

```env
SOLACE_BROKER_MCP_URL="http://localhost:9090/mcp"
SOLACE_BROKER_MCP_TOKEN="sam-mcp-dev-token-local-only"   # must equal dev_token above
```

**4. Run both in separate terminals**:

```bash
# Terminal 1 — this MCP server
go run ./cmd/server

# Terminal 2 — SAM (auto-discovers the new agent)
sam run
```

**5. Verify** — SAM's logs should include:

```
Initialized MCPToolset for server: url='http://localhost:9090/mcp'
Agent card tool manifest populated with 14 tools.
Registered new agent 'SolaceBrokerAgent' in registry.
```

Open SAM's WebUI (`http://localhost:8000`) and ask the orchestrator something broker-related — it will delegate to `SolaceBrokerAgent`, which calls the MCP tools.

## Auth chain

```
SAM agent ─(Bearer SOLACE_BROKER_MCP_TOKEN)→ MCP server ─(basic BROKER_USERNAME/PASSWORD)→ Solace event broker
```

The bearer token in SAM's `.env` and the `dev_token` in this MCP server's config must be identical.

## What's covered upstream

For these, refer to the [SAM MCP Integration tutorial](https://github.com/SolaceLabs/solace-agent-mesh/blob/main/docs/docs/documentation/developing/tutorials/mcp-integration.md):

- Other connection types (`stdio`, `sse`, Docker)
- Tool filtering (`tool_name`, `allow_list`, `deny_list`)
- TLS/SSL config for self-signed certs (`ssl_config`)
- Passing environment variables to the MCP server process
