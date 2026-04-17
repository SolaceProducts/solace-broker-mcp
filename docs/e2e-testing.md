# End-to-End Testing — Solace Broker MCP Server

This document describes the E2E testing strategy, structure, and how to run the tests locally and in CI.

---

## Goals

Unit tests mock the Solace broker entirely. E2E tests validate the full stack end-to-end:

```
HTTP transport → MCP protocol → SEMP API → live Solace broker
```

Two scenarios are covered:

1. **Standalone** — raw curl requests against the MCP server (no AI agent)
2. **Agent** — a Go program using the MCP SDK client to connect and call tools

---

## Test Structure

```
test/e2e/
├── docker-compose.yml       # Solace PubSub+ broker container
├── helpers.sh               # Shared bash functions (broker wait, server start, MCP helpers, assertions)
├── run_all.sh               # Master test runner (orchestrates both scenarios)
├── start_server.sh          # Build and start a fresh MCP server from latest source
├── test_standalone.sh       # Scenario 1: raw curl MCP protocol tests
├── test_agent.sh            # Scenario 2: builds and runs the Go agent
├── bin/                     # Built binaries (gitignored)
│   ├── mcp-server
│   ├── mcp-server.pid       # PID file when server is started with --bg
│   └── agent
└── agent/
    ├── main.go              # Go MCP SDK client agent program
    ├── go.mod
    └── go.sum
```

---

## Prerequisites

- Docker with Compose
- `curl` and `jq`
- Go (same version as `go.mod`)

---

## Usage

### Run everything (recommended)

The simplest way to run the full E2E suite. `run_all.sh` builds the MCP server from source, starts it, creates broker fixtures, runs both test scenarios, and cleans up.

```bash
# 1. Start the Solace broker
docker compose -f test/e2e/docker-compose.yml up -d

# 2. Run all E2E tests (waits for broker readiness automatically)
bash test/e2e/run_all.sh

# 3. Stop and remove the broker when done
docker compose -f test/e2e/docker-compose.yml down -v
```

### Run scenarios individually

When iterating on a specific scenario, start the server once and run tests against it repeatedly.

```bash
# 1. Start the broker (if not already running)
docker compose -f test/e2e/docker-compose.yml up -d

# 2. Build and start the MCP server in the background
bash test/e2e/start_server.sh --bg

# 3. Create broker test fixtures
source test/e2e/helpers.sh && create_fixtures

# 4. Run either or both scenarios
bash test/e2e/test_standalone.sh   # Scenario 1: raw curl
bash test/e2e/test_agent.sh        # Scenario 2: Go agent

# 5. Stop the server when done
kill $(cat test/e2e/bin/mcp-server.pid)
```

### Start the MCP server standalone

`start_server.sh` compiles the MCP server from the latest source and starts it against the E2E broker. Useful for manual testing or development.

```bash
# Foreground (Ctrl-C to stop)
bash test/e2e/start_server.sh

# Background (writes PID file for later stop)
bash test/e2e/start_server.sh --bg
kill $(cat test/e2e/bin/mcp-server.pid)   # stop
```

The script:
1. Waits for the Solace broker to be fully ready (SEMP API + message spool)
2. Builds `test/e2e/bin/mcp-server` from `./cmd/server`
3. Writes a temporary config pointing at `localhost:8080` with `admin/admin`
4. Starts the server on port `9090`

---

## Broker Setup

The Solace PubSub+ Standard Edition container is free and used for all E2E tests.

| Setting | Value |
|---|---|
| Image | `solace/solace-pubsub-standard:latest` |
| SEMP port | `8080` (mapped from container port 8080) |
| Default credentials | `admin` / `admin` |
| Default VPN | `default` (pre-created by broker) |

The broker readiness check has two phases:
1. SEMP API responds to config requests
2. Message spool is active (required for queue operations)

---

## MCP Server Setup

The test harness builds the server binary from source and runs it as a subprocess. Configuration is written to a temporary file at test time:

```yaml
brokers:
  e2e:
    url: "http://localhost:8080"
    env_prefix: "E2E"
    auth:
      method: basic
```

Credentials are passed via environment variables: `E2E_USERNAME=admin`, `E2E_PASSWORD=admin`.

The server runs on port `9090` (default). Tests target `http://localhost:9090`.

---

## Broker Test Fixtures

Before tests run, the harness creates the following objects in the `default` VPN via the SEMP config API:

| Object | Name | Purpose |
|---|---|---|
| Queue | `test-queue` | Required by the RDP queue binding and `get-queue-metrics` |
| REST Delivery Point | `test-rdp` | Target of `get-rdp-status` tool calls |
| REST Consumer | `test-consumer` | Attached to `test-rdp` |
| Queue Binding | `test-queue` | Binds `test-queue` to `test-rdp` |

Fixtures are cleaned up before creation (to handle leftover state) and after tests complete regardless of outcome.

---

## Scenario 1: Standalone (Raw curl)

`test_standalone.sh` sends raw MCP JSON-RPC requests to `POST /mcp`. It exercises:

| Test | What it validates |
|---|---|
| `test_health_endpoint` | `GET /health` returns `{"status": "ok"}` |
| `test_initialize` | MCP handshake completes, server returns `Mcp-Session-Id` |
| `test_list_tools` | `tools/list` returns all 5 tools (`get-rdp-status`, `list-brokers`, `get-queue-metrics`, `get-client-details`, `list-client-subscriptions`) |
| `test_list_brokers` | `list-brokers` tool response includes the configured alias `e2e` |
| `test_get_rdp_status` | `get-rdp-status` returns 3-step response (rdpStatus, queueBindings, restConsumers) |
| `test_get_rdp_status_not_found` | Nonexistent RDP name returns a JSON-RPC error |
| `test_get_queue_metrics` | `get-queue-metrics` returns queue data for `test-queue` |

### MCP Protocol Flow (Streamable HTTP)

Each test session follows the MCP handshake before calling tools. The server responds with SSE (`text/event-stream`) and the test helpers extract the JSON `data:` payload.

```
POST /mcp  initialize
           ← Mcp-Session-Id header + server capabilities (SSE)

POST /mcp  notifications/initialized   (Mcp-Session-Id: <id>)

POST /mcp  tools/list or tools/call    (Mcp-Session-Id: <id>)
           ← SSE with JSON-RPC response in data: line
```

---

## Scenario 2: Agent (Go MCP SDK Client)

`test/e2e/agent/main.go` is a standalone Go program that uses the official MCP Go SDK client (`github.com/modelcontextprotocol/go-sdk`) to connect to the running MCP server.

Usage: `./bin/agent <server-url>`

It performs:
1. Connect to the MCP server via `StreamableClientTransport`
2. Call `session.ListTools()` — verify all 5 tools are present
3. Call `list-brokers` tool — verify the `e2e` alias appears
4. Call `get-rdp-status` with the test fixtures — verify 3-step structured response
5. Call `get-queue-metrics` with `test-queue` — verify queue data returned

`test_agent.sh` builds the agent binary then runs it, checking exit code and output.

### Future Extension: LLM Agent via LiteLLM

The agent program is designed to extend naturally into a real LLM-driven agent:

```
Phase 1 (now):  hardcoded tool calls
Phase 2:        add go-openai client → point at LiteLLM endpoint
                → LLM decides which tools to call
                → agent executes via MCP SDK
                → feed results back → agentic loop
```

Because LiteLLM exposes an OpenAI-compatible API, a Go OpenAI client (e.g. `github.com/sashabaranov/go-openai`) can target it with no MCP changes. The agent's `session.ListTools()` output maps directly to OpenAI function definitions.

---

## CI Integration

The E2E tests run as a separate `e2e` job in `.github/workflows/build-and-test.yml`, gated on the `build` job passing:

```
lint ──┐
       ├── build ──── e2e
```

The job:
1. Starts the Solace broker with `docker compose up -d`
2. Polls the broker health endpoint until ready
3. Runs `bash test/e2e/run_all.sh`
4. On failure: dumps broker logs for debugging
5. Always tears down the broker with `docker compose down -v`
