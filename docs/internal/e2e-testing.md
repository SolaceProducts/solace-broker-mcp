# End-to-End Testing — Solace Broker MCP Server

This document describes the E2E testing strategy, structure, and how to run the tests locally and in CI.

For a quickstart and the suite's port allocation, see [`test/e2e-basic-mcp/README.md`](../../test/e2e-basic-mcp/README.md). A separate monitoring-focused suite lives under [`test/e2e-monitoring/`](../../test/e2e-monitoring/README.md), and a management/config-tool suite lives under [`test/e2e-management/`](../../test/e2e-management/README.md). An LLM-driven eval harness that runs natural-language scenarios through the Claude Code CLI lives under [`test/e2e-llm/`](../../test/e2e-llm/README.md). The LLM suite is non-gating and runs on demand via `workflow_dispatch` ([`llm-eval.yml`](../../.github/workflows/llm-eval.yml)), plus daily through 2026-09-03 for the SOL-153184 flake-rate and cost collection window. It costs API credits per run (~$4.78 for the full matrix). Delete this clause when the schedule goes.

## Shared scaffold — `test/e2e-common/lib.sh`

The generic scaffold shared by the tool-testing suites — broker readiness, MCP server build/start/stop, config generation (`write_config`), SEMP operations, the MCP JSON-RPC wire helpers, assertions, and the test runner — lives in [`test/e2e-common/lib.sh`](../../test/e2e-common/lib.sh). Each suite keeps only its own fixtures and sources the lib. Every suite runs one server with `enable_write_tools` on, so both read and write tools are registered.

Protocol pin: `MCP_PROTOCOL_VERSION` (default `2025-11-25`) is the `protocolVersion` every suite sends in its `initialize` handshake — exported by the lib and overridable from the environment. `2025-11-25` is the newest revision this stateful server can negotiate; the full rationale sits at the definition in `lib.sh`. Raising it does not fail the handshake — the SDK silently negotiates back down — so `mcp_initialize` asserts the negotiated revision matches the pin.

Location-independence contract: a suite sets `SUITE_DIR` (its own directory) before sourcing the lib, which derives `BIN_DIR`/`ENV_FILE`/`REPO_ROOT` from it and sources the suite's `.env`. This lets one lib serve `e2e-monitoring`, `e2e-management`, and future suites, each with its own `bin/`, `.env`, ports, and containers.

| Suite | Fixtures | Focus |
|---|---|---|
| `e2e-monitoring` | F1–F7, created up front + broker-driver traffic | monitoring tools |
| `e2e-management` | per-test `e2e-config-*`, created/torn down inside each test | config tools (create/update/delete VPN, queue, topic-endpoint) |

---

## Goals

Unit tests mock the Solace broker entirely. E2E tests validate the full stack end-to-end:

```
HTTP transport → MCP protocol → SEMP API → live Solace broker
```

Two scenarios are covered:

1. **Standalone** — raw curl requests against the MCP server (no AI agent)
2. **Agent** — a Go program using the MCP SDK client to connect and call tools

Both scenarios run against **two independent brokers** (`broker-a`, `broker-b`) to verify multi-broker routing.

---

## Test Structure

```
test/e2e-basic-mcp/
├── README.md                # Suite overview, quickstart, port allocation
├── .env                     # Single source of truth: ports, credentials
├── docker-compose.yml       # Two Solace PubSub+ broker containers
├── helpers.sh               # Shared bash functions (broker wait, server start, MCP helpers, assertions)
├── setup-brokers.sh         # Bring brokers up and wait until ready (idempotent)
├── run-all.sh               # Master test runner (orchestrates both scenarios, prints summary table)
├── start-server.sh          # Build and start a fresh MCP server from latest source
├── test-standalone.sh       # Scenario 1: raw curl MCP protocol tests
├── test-agent.sh            # Scenario 2: builds and runs the Go agent
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

## Configuration

Per-suite configuration lives in a single file: `test/e2e-basic-mcp/.env`. This file is the source of truth for ports, credentials, and MCP server env vars (suite-wide protocol settings such as `MCP_PROTOCOL_VERSION` live in `e2e-common/lib.sh` instead). It is read by:

- **docker-compose.yml** — broker port mappings and admin password
- **helpers.sh** — broker URLs, SEMP auth, fixture management
- **MCP server** — credential resolution via `ENV_FILE` (reads `E2E_A_USERNAME` and so on)

```bash
# Broker SEMP ports (host-side)
BROKER_A_SEMP_PORT=8080
BROKER_B_SEMP_PORT=8082

# Broker credentials
BROKER_USERNAME=admin
BROKER_PASSWORD=admin

# MCP server credential env vars (used by ${VAR} substitution in broker config)
E2E_A_USERNAME=admin
E2E_A_PASSWORD=admin
E2E_B_USERNAME=admin
E2E_B_PASSWORD=admin
```

To change ports or credentials, edit `.env` only — everything else derives from it.

The harness generates the MCP server config dynamically from `.env` while running
the suite, so no server config file needs to be created or maintained.

---

## Usage

### Run everything (recommended)

The simplest way to run the full E2E suite. `run-all.sh` builds the MCP server from source, starts it, creates broker fixtures on both brokers, runs both test scenarios, and prints a summary table.

```bash
# 1. Start both Solace brokers
docker compose -f test/e2e-basic-mcp/docker-compose.yml up -d

# 2. Run all E2E tests (waits for broker readiness automatically)
bash test/e2e-basic-mcp/run-all.sh

# 3. Stop and remove the brokers when done
docker compose -f test/e2e-basic-mcp/docker-compose.yml down -v
```

### Run scenarios individually

When iterating on a specific scenario, start the server once and run tests against it repeatedly.

```bash
# 1. Start the brokers (if not already running)
docker compose -f test/e2e-basic-mcp/docker-compose.yml up -d

# 2. Build and start the MCP server in the background
SUITE_DIR=test/e2e-basic-mcp bash test/e2e-common/start-server.sh --bg

# 3. Create broker test fixtures on both brokers
source test/e2e-basic-mcp/helpers.sh && create_fixtures

# 4. Run either or both scenarios
bash test/e2e-basic-mcp/test-standalone.sh   # Scenario 1: raw curl
bash test/e2e-basic-mcp/test-agent.sh        # Scenario 2: Go agent

# 5. Stop the server when done
kill $(cat test/e2e-basic-mcp/bin/mcp-server.pid)
```

### Start the MCP server standalone

The shared `e2e-common/start-server.sh` compiles the MCP server from the latest source and starts it against a suite's E2E brokers. Useful for manual testing or development.

```bash
# Foreground (Ctrl-C to stop)
SUITE_DIR=test/e2e-basic-mcp bash test/e2e-common/start-server.sh

# Background (writes PID file for later stop)
SUITE_DIR=test/e2e-basic-mcp bash test/e2e-common/start-server.sh --bg
kill $(cat test/e2e-basic-mcp/bin/mcp-server.pid)   # stop
```

The script:
1. Waits for both Solace brokers to be fully ready (SEMP API + message spool)
2. Builds `test/e2e-basic-mcp/bin/mcp-server` from `./cmd/server`
3. Generates a broker config from `.env` values (ports, env prefixes)
4. Starts the server on port `9090` with credentials from `.env`

---

## Broker Setup

Two Solace PubSub+ Standard Edition containers are used for E2E tests, configured in `docker-compose.yml`.

| Setting | broker-a | broker-b |
|---|---|---|
| Image | `solace/solace-pubsub-standard:latest` | `solace/solace-pubsub-standard:latest` |
| Container | `solace-e2e-a` | `solace-e2e-b` |
| SEMP port | `8080` (configurable via `.env`) | `8082` (configurable via `.env`) |
| Credentials | `admin` / `admin` (from `.env`) | `admin` / `admin` (from `.env`) |
| Default VPN | `default` (pre-created) | `default` (pre-created) |
| Shared memory | 1 GB | 1 GB |
| File descriptors | 1,048,576 (nofile ulimit) | 1,048,576 (nofile ulimit) |

The broker readiness check has two phases:
1. SEMP API responds to config requests
2. Message spool is active (required for queue operations)

---

## MCP Server Setup

The test harness builds the server binary from source and runs it as a subprocess. Configuration is generated dynamically from `.env` values:

```yaml
mcp_client_auth:
  mode: static
  dev_token: e2e-static-dev-token

brokers:
  broker-a:
    url: "http://localhost:8080"
    auth:
      mode: basic
      username: "${E2E_A_USERNAME}"
      password: "${E2E_A_PASSWORD}"
  broker-b:
    url: "http://localhost:8082"
    auth:
      mode: basic
      username: "${E2E_B_USERNAME}"
      password: "${E2E_B_PASSWORD}"
```

Credentials use `${VAR}` substitution, resolved from `.env` via `ENV_FILE`.

The server runs on port `9090` (default). Tests target `http://localhost:9090`.

---

## Broker Test Fixtures

Before tests run, the harness creates the following objects in the `default` VPN on **both brokers** via the SEMP config API:

| Object | Name | Purpose |
|---|---|---|
| Queue | `test-queue` | Required by the RDP queue binding and `get-queue-metrics` |
| REST Delivery Point | `test-rdp` | Target of `get-rdp-status` tool calls |
| REST Consumer | `test-consumer` | Attached to `test-rdp` |
| Queue Binding | `test-queue` | Binds `test-queue` to `test-rdp` |

Fixtures are cleaned up before creation (to handle leftover state) and after tests complete regardless of outcome.

---

## Scenario 1: Standalone (Raw curl)

`test-standalone.sh` sends raw MCP JSON-RPC requests to `POST /mcp`. It exercises both brokers:

| Test | What it validates |
|---|---|
| `test_health_endpoint` | `GET /health` returns `{"status":"healthy"}` (legacy back-compat body; `/livez` is the canonical liveness endpoint and returns `{"status":"alive"}`) |
| `test_initialize` | MCP handshake completes, server returns `Mcp-Session-Id` |
| `test_list_tools` | `tools/list` includes the expected tools — asserts a representative subset is present (`get-rdp-status`, `list-brokers`, `get-queue-metrics`, `get-client-details`, `list-client-subscriptions`). The server runs with `enable_write_tools` on, so `tools/list` returns all 43 tools: 25 read-only plus 18 write (4 action, 14 management). |
| `test_list_brokers` | `list-brokers` response includes both `broker-a` and `broker-b` |
| `test_get_rdp_status_broker_a` | `get-rdp-status` on broker-a returns 3-step response |
| `test_get_rdp_status_not_found` | Nonexistent RDP name returns a JSON-RPC error |
| `test_get_queue_metrics_broker_a` | `get-queue-metrics` on broker-a returns queue data |
| `test_get_rdp_status_broker_b` | `get-rdp-status` on broker-b returns 3-step response |
| `test_get_queue_metrics_broker_b` | `get-queue-metrics` on broker-b returns queue data |

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

`test/e2e-basic-mcp/agent/main.go` is a standalone Go program that uses the official MCP Go SDK client (`github.com/modelcontextprotocol/go-sdk`) to connect to the running MCP server.

Usage: `./bin/agent <server-url>`

It performs:
1. Connect to the MCP server via `StreamableClientTransport`
2. Call `session.ListTools()` — verify a representative subset of tools is present
3. Call `list-brokers` tool — verify both `broker-a` and `broker-b` aliases appear
4. For each broker (`broker-a`, `broker-b`):
   - Call `get-rdp-status` with the test fixtures — verify 3-step structured response
   - Call `get-queue-metrics` with `test-queue` — verify queue data returned

`test-agent.sh` builds the agent binary then runs it, checking exit code and output.

### Future Extension: LLM Agent via LiteLLM

The agent program is designed to extend naturally into a real LLM-driven agent:

```
Phase 1 (now):  hardcoded tool calls
Phase 2:        add go-openai client → point at LiteLLM endpoint
                → LLM decides which tools to call
                → agent executes via MCP SDK
                → feed results back → agentic loop
```

Because LiteLLM exposes an OpenAI-compatible API, a Go OpenAI client (for example, `github.com/sashabaranov/go-openai`) can target it with no MCP changes. The agent's `session.ListTools()` output maps directly to OpenAI function definitions.

---

## Test Output

`run-all.sh` prints a summary table at the end:

```
┏━━━━━━━━━━━━━━━━━━━━━━━━━┳━━━━━━━┳━━━━━━━━━┳━━━━━━━━━┓
┃ Scenario                ┃  Run  ┃ Passed  ┃ Failed  ┃
┣━━━━━━━━━━━━━━━━━━━━━━━━━╋━━━━━━━╋━━━━━━━━━╋━━━━━━━━━┫
┃ Standalone tests        ┃     9 ┃       9 ┃       0 ┃
┃ Agent tests             ┃     1 ┃       1 ┃       0 ┃
┣━━━━━━━━━━━━━━━━━━━━━━━━━╋━━━━━━━╋━━━━━━━━━╋━━━━━━━━━┫
┃ TOTAL                   ┃    10 ┃      10 ┃       0 ┃
┗━━━━━━━━━━━━━━━━━━━━━━━━━┻━━━━━━━┻━━━━━━━━━┻━━━━━━━━━┛
```

---

## CI Integration

The E2E tests run as a separate `e2e` job in `.github/workflows/build-and-test.yml`, gated on the `build` job passing:

```
lint ──┐
       ├── build ──── e2e
```

The job:
1. Starts both Solace brokers with `docker compose up -d`
2. Waits for both brokers to be fully ready (SEMP API + message spool, up to 120s)
3. Runs `bash test/e2e-basic-mcp/run-all.sh`
4. On failure: dumps last 100 lines of broker logs for debugging
5. Always tears down the brokers with `docker compose down -v`
