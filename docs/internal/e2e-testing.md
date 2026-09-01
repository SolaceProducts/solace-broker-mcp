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

Four scenarios are covered:

1. **Standalone** — raw curl requests against the MCP server (no AI agent)
2. **Agent** — a Go program using the MCP SDK client to connect and call tools
3. **Negative paths** — tool-execution failures surface as clean MCP structured errors
4. **Throttling** — the SEMP rate limiter and in-flight cap hold against a real broker

Scenarios 1 and 2 run against **two independent brokers** (`broker-a`, `broker-b`) to verify multi-broker routing.

---

## Test Structure

```
test/e2e-basic-mcp/
├── README.md                # Suite overview, quickstart, port allocation
├── .env                     # Single source of truth: ports, credentials
├── docker-compose.yml       # Two Solace PubSub+ broker containers
├── helpers.sh               # Shared bash functions (broker wait, server start, MCP helpers, assertions)
├── run-all.sh               # Master test runner (orchestrates every scenario, prints summary table)
├── test-standalone.sh       # Scenario 1: raw curl MCP protocol tests
├── test-agent.sh            # Scenario 2: builds and runs the Go agent
├── test-negative-paths.sh   # Scenario 3: structured-error envelope contract
├── test-throttling.sh       # Scenario 4: rate limiter + in-flight cap
├── test-throttling-analysis.sh  # Self-test of scenario 4's record arithmetic (no Docker)
├── bin/                     # Built binaries, generated configs, records (gitignored)
│   ├── mcp-server
│   ├── mcp-server.pid       # PID file when server is started with --bg
│   ├── semp-tap             # recording reverse proxy (scenario 4)
│   ├── mcp-config-throttle-*.yaml   # per-phase configs (scenario 4)
│   ├── throttle-record-*.csv        # per-phase request records (scenario 4)
│   └── agent
└── agent/
    ├── main.go              # Go MCP SDK client agent program
    ├── go.mod
    └── go.sum
```

Broker startup (`setup-brokers.sh`) and server startup (`start-server.sh`) are
shared and live in `test/e2e-common/`, not in the suite directory.

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
| `test_list_tools` | `tools/list` includes the expected tools — asserts a representative subset is present (`get-rdp-status`, `list-brokers`, `get-queue-metrics`, `get-client-details`, `list-client-subscriptions`). The server runs with `enable_write_tools` on, so `tools/list` returns all 40 tools: 24 read-only plus 16 write (4 action, 12 management). |
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

## Scenario 4: Throttling (SEMP rate limiter and in-flight cap)

`test-throttling.sh` (SOL-153444) proves that `semp.request_min_interval` and `semp.max_concurrent_per_broker` are honored end to end against a real Dockerized broker. Both were previously covered only by unit tests against an `httptest` server (`internal/semp/resilience/ratelimiter_test.go`, `internal/semp/rate_limiter_shared_test.go`).

### How it measures

One MCP tool call is not one SEMP request — composites fan out, pagination adds more, retries add attempts the client never sees — so client-side timing cannot say what rate the broker experienced. Instead `semp-tap` (`test/e2e-common/semp-tap`, own `go.mod`, stdlib only) is placed between the MCP server and broker-a, and a dedicated `broker-throttle` alias points at it. The broker is real and does the real work; the tap forwards everything and records one CSV line per request. Because the rate limiter and the in-flight semaphore are keyed by broker alias, the record contains exactly this scenario's traffic.

**The measurement window matters.** `Sender.Do` releases its semaphore slot when the response *headers* arrive, with the body still open (`internal/semp/resilience/sender.go`). The tap therefore measures in-flight over `[request received → response headers returned]` via `ModifyResponse`, not over full body-proxy completion. Measuring the wider window would report a correct cap of 2 as an occasional 3. For the same reason the tap counts requests, never connections: `max_concurrent_per_broker` also sizes the transport's `MaxConnsPerHost` per protocol client, so up to 2x the cap in TCP connections can legitimately exist.

### Phases

`semp:` is a single global block, and the two limits mask each other — with the pacer at 200ms and a local broker answering in tens of milliseconds, in-flight count never reaches 2, so a cap of 2 could never be shown to bind. Each limit gets a phase where it is the only constraint, plus a shared control. The MCP server is restarted between phases (the `e2e-oauth` pattern).

| Phase | `request_min_interval` | `max_concurrent_per_broker` | Tap delay | Asserts |
|---|---|---|---|---|
| pacer | `200ms` | `10` | 0 | every gap ≥ 100ms, and the aggregate span ≥ gaps × 200ms − 200ms |
| cap | `0s` | `2` | 150ms | peak in-flight == 2 |
| pacer-control | `0s` | `10` | 0 | the inverse of the pacer phase |
| cap-control | `0s` | `10` | 150ms | the inverse of the cap phase |

The two control phases are the answer to "sanity-check that the assertion can actually fail". Each runs the identical load with its limit off and asserts the inverse. They are real, always-on CI assertions rather than commented-out or manual steps, so if the tap ever stops being able to see a violation, the control goes red and names the assertion that has gone blind.

Each control differs from the phase it validates in exactly one variable. A single shared control would differ from the pacer phase in both the interval and the tap delay at once. That is also empirically the wrong shape: with no tap delay the same load peaks at only ~5 in flight rather than 10, so a delay-free run is a poor control for the cap even though it is the right control for the pacer.

### Keeping it deterministic

- Every assertion is count-based or a generous lower bound. A loaded CI runner makes gaps longer and requests overlap more, which is the safe direction for all of them.
- Pacing is asserted twice, mirroring `rate_limiter_shared_test.go`. The per-pair minimum gap carries a deliberately wide floor (half the interval) because transit jitter can shorten a single gap, and widening costs nothing: every defect this guards against produces gaps near zero, not gaps a few milliseconds short. The aggregate span is the precise check, because that jitter cancels over a run.
- The record arithmetic itself is self-tested against synthetic records by `test-throttling-analysis.sh`, which runs first in the scenario and also standalone in about a second with no Docker. Every numeric result passes a `require_int` guard first, because `[ "" -lt 13 ]` exits 2 and would otherwise fall through an `if` and return success.
- `retries: 0` in the throttle config. Retries are explicitly *not* paced (they run inside one limiter tick and one semaphore slot), so a transient blip would otherwise inject arrivals no gap assertion accounts for.
- The scenario warms the broker client up and then skips the first arrivals. `RateLimiter` is a bare `time.Ticker`, not a token bucket: its channel buffers one tick, so idle time is credit and the first request after an idle period is admitted with no wait. The warm-up makes that behavior deterministic rather than accidental.
- Phases with a concurrency assertion hold each response an identical extra 150ms at the tap, so overlap is arithmetic rather than a race against how fast the broker answers. Both limits are still enforced by the real server against a real broker; only the measurement window is widened, and identically in the phase and its control.

The scenario runs last in `run-all.sh` because it takes over port `9090` from the shared server the earlier scenarios use.

## Test Output

`run-all.sh` prints a summary table at the end:

```
┏━━━━━━━━━━━━━━━━━━━━━━━━━┳━━━━━━━┳━━━━━━━━━┳━━━━━━━━━┓
┃ Scenario                ┃  Run  ┃ Passed  ┃ Failed  ┃
┣━━━━━━━━━━━━━━━━━━━━━━━━━╋━━━━━━━╋━━━━━━━━━╋━━━━━━━━━┫
┃ Standalone tests        ┃     9 ┃       9 ┃       0 ┃
┃ Agent tests             ┃     1 ┃       1 ┃       0 ┃
┃ Negative-path tests     ┃     3 ┃       3 ┃       0 ┃
┃ Throttling tests        ┃     4 ┃       4 ┃       0 ┃
┣━━━━━━━━━━━━━━━━━━━━━━━━━╋━━━━━━━╋━━━━━━━━━╋━━━━━━━━━┫
┃ TOTAL                   ┃    17 ┃      17 ┃       0 ┃
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
