# E2E Basic-MCP Suite

End-to-end coverage for the core MCP protocol surface of the broker MCP server:
the `initialize` handshake, `tools/list`, and a representative set of tool calls
(`list-brokers`, `get-rdp-status`, `get-queue-metrics`) exercised through two
independent clients. Runs two Solace brokers in containers, provisions a base
fixture set on each, and verifies multi-broker routing.

For the broader testing strategy and rationale (the "why"), see
[`docs/internal/e2e-testing.md`](../../docs/internal/e2e-testing.md). This README
is the source of truth for **what is implemented in this suite today**.

## Quickstart

Three commands for a full run:

```bash
# 1. Bring brokers up. Safe to re-run; does nothing if already up.
SUITE_DIR=test/e2e-basic-mcp bash test/e2e-common/setup-brokers.sh

# 2. Build the server, create fixtures, run both scenarios, print a summary.
bash test/e2e-basic-mcp/run-all.sh

# 3. Tear brokers down when you're done.
docker compose -f test/e2e-basic-mcp/docker-compose.yml down -v
```

`run-all.sh` builds the MCP server and the Go agent, applies the base fixtures to
both brokers, runs both scenarios, and cleans up the fixtures and server on exit
(via an `EXIT` trap). It assumes the brokers are already up — run the
`setup-brokers.sh` step above first.

## Layout

```
test/e2e-basic-mcp/
├── README.md            # This file
├── .env                 # Single source of truth: ports, credentials, dev token
├── docker-compose.yml   # Two Solace PubSub+ broker containers
├── helpers.sh           # Suite fixtures (queue/RDP/consumer/binding); sources ../e2e-common/lib.sh
├── run-all.sh           # Master runner: orchestrates both scenarios, prints summary table
├── test-standalone.sh   # Scenario 1: raw curl MCP protocol tests
├── test-agent.sh        # Scenario 2: builds and runs the Go MCP-SDK agent
├── agent/               # Go MCP-SDK client program (own go.mod)
└── bin/                 # Built binaries + pidfile (gitignored)
```

## Scenarios

Both scenarios run against **two brokers** (`broker-a`, `broker-b`) to verify
multi-broker routing.

- **Scenario 1 — Standalone (`test-standalone.sh`).** Sends raw MCP JSON-RPC over
  `POST /mcp` with `curl`. Covers: `/health`, the `initialize` handshake,
  `tools/list`, `list-brokers` (both aliases), `get-rdp-status` (broker-a,
  broker-b, and a not-found error case), and `get-queue-metrics` (broker-a,
  broker-b).
- **Scenario 2 — Agent (`test-agent.sh`).** Builds `agent/` and runs it against
  the live server using the official Go MCP SDK
  (`github.com/modelcontextprotocol/go-sdk`). Validates `list-brokers`,
  `get-rdp-status` (3-section response), and `get-queue-metrics` on both brokers.

## Fixtures

`create_fixtures` (in `helpers.sh`) provisions the same base objects on the
`default` VPN of **both** brokers via the SEMP config API, cleaning up any leftover
state first:

| Object              | Name                    | Purpose                                                                   |
| ------------------- | ----------------------- | ------------------------------------------------------------------------- |
| Queue               | `test-queue`            | Backs the RDP queue binding and `get-queue-metrics`                       |
| REST Delivery Point | `test-rdp`              | Target of `get-rdp-status`                                                |
| REST Consumer       | `test-consumer`         | Attached to `test-rdp`                                                    |
| Queue Binding       | `test-queue`            | Binds `test-queue` to `test-rdp`                                          |
| REST Delivery Point | `test-rdp-failing`      | Enabled RDP pointed at an unreachable remote — exercises down-path fields |
| REST Consumer       | `test-consumer-failing` | Attached to `test-rdp-failing`                                            |
| Queue Binding       | `test-queue`            | Binds `test-queue` to `test-rdp-failing`                                  |

This is the same base fixture the `e2e-monitoring` suite copies and extends.

## Prerequisites

- Docker with Compose (broker containers)
- `curl` and `jq` (Scenario 1 assertions)
- Go matching `go.mod` (builds the server and the `agent/` binary)

The `agent/` binary is a plain-Go MCP client — **no CGO/native dependency**
(unlike `e2e-monitoring`'s `broker-driver`, which links `libsolclient`).

## Cleanup order

`run-all.sh` runs the following on exit (any reason — normal end, error, Ctrl-C):

1. **Stop the MCP server** (`stop_server`).
2. **Delete broker fixtures** (`cleanup_fixtures`): bindings → consumers → RDPs → queues.
3. **Remove the MCP server PID file** (`bin/mcp-server.pid`).

## Port allocation

Distinct from `e2e-monitoring` so both suites can run concurrently:

| Resource      | e2e-basic-mcp | e2e-monitoring |
| ------------- | ------------- | -------------- |
| SEMP broker-a | 8080          | 8090           |
| SEMP broker-b | 8082          | 8092           |
| SMF broker-a  | (not exposed) | 55655          |
| SMF broker-b  | (not exposed) | 55656          |

The MCP server listens on `9090` (override with `MCP_PORT`). All broker ports are
override-able via `.env` (`BROKER_A_SEMP_PORT`, `BROKER_B_SEMP_PORT`).