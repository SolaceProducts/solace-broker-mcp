# E2E Basic-MCP Suite

End-to-end coverage for the core MCP protocol surface of the broker MCP server:
the `initialize` handshake, `tools/list`, and a representative set of tool calls
(`list-brokers`, `get-rdp-status`, `get-queue-metrics`) exercised through two
independent clients, plus a negative-path smoke over the structured-error
envelope contract. Runs two Solace brokers in containers, provisions a base
fixture set on each, and verifies multi-broker routing.

For the broader testing strategy and rationale (the "why"), see
[`docs/internal/e2e-testing.md`](../../docs/internal/e2e-testing.md). This README
is the source of truth for **what is implemented in this suite today**.

## Quickstart

Three commands for a full run:

```bash
# 1. Bring brokers up. Safe to re-run; does nothing if already up.
SUITE_DIR=test/e2e-basic-mcp bash test/e2e-common/setup-brokers.sh

# 2. Build the server, create fixtures, run all scenarios, print a summary.
bash test/e2e-basic-mcp/run-all.sh

# 3. Tear brokers down when you're done.
docker compose -f test/e2e-basic-mcp/docker-compose.yml down -v
```

`run-all.sh` builds the MCP server and the Go agent, applies the base fixtures to
both brokers, runs all four scenarios, and cleans up the fixtures and server on
exit (via an `EXIT` trap). It assumes the brokers are already up — run the
`setup-brokers.sh` step above first.

## Layout

```
test/e2e-basic-mcp/
├── README.md            # This file
├── .env                 # Single source of truth: ports, credentials, dev token
├── docker-compose.yml   # Two Solace PubSub+ broker containers
├── helpers.sh           # Suite fixtures (queue/RDP/consumer/binding); sources ../e2e-common/lib.sh
├── run-all.sh              # Master runner: orchestrates all scenarios, prints summary table
├── test-standalone.sh      # Scenario 1: raw curl MCP protocol tests
├── test-agent.sh           # Scenario 2: builds and runs the Go MCP-SDK agent
├── test-negative-paths.sh  # Scenario 3: structured-error envelope contract (SOL-150767)
├── test-throttling.sh      # Scenario 4: rate limiter + in-flight cap (SOL-153444)
├── test-throttling-analysis.sh  # Self-test of scenario 4's record arithmetic (no Docker, ~1s)
├── agent/                  # Go MCP-SDK client program (own go.mod)
└── bin/                    # Built binaries + pidfile (gitignored)
```

## Scenarios

Scenarios 1 and 2 run against **two brokers** (`broker-a`, `broker-b`) to verify
multi-broker routing. Scenario 3 uses two additional negative-path aliases
(`broker-bad-creds`, `broker-dead`) appended by this suite's `write_config()`
override in `helpers.sh` so a single MCP server sees all four broker entries.
The extras are local to this suite — other suites' server configs stay minimal.

- **Scenario 1 — Standalone (`test-standalone.sh`).** Sends raw MCP JSON-RPC over
  `POST /mcp` with `curl`. Covers: `/health`, the `initialize` handshake,
  `tools/list`, `list-brokers` (both aliases), `get-rdp-status` (broker-a,
  broker-b, and a not-found error case), and `get-queue-metrics` (broker-a,
  broker-b).
- **Scenario 2 — Agent (`test-agent.sh`).** Builds `agent/` and runs it against
  the live server using the official Go MCP SDK
  (`github.com/modelcontextprotocol/go-sdk`). Validates `list-brokers`,
  `get-rdp-status` (3-section response), and `get-queue-metrics` on both brokers.
- **Scenario 3 — Negative paths (`test-negative-paths.sh`).** SOL-150767 /
  SOL-147161 §3.7. Confirms three tool-execution failures surface as clean MCP
  structured errors through the full server + real-broker stack: bad
  credentials (SEMPv2 401, non-retryable, no credential leak), broker
  unreachable (retries exhausted, retryable, no `status` field), and a
  not-found queue on `get-queue-metrics` (SEMPv2 signals not-found as HTTP
  400 + sempCode 6, not literal 404 — asserted as both). Behavior coverage
  itself is unit-tested in
  `internal/semp/resilience/` — this scenario is the "we drove it once" smoke
  on the envelope contract, not a comprehensive negative matrix.
- **Scenario 4 — Throttling (`test-throttling.sh`).** SOL-153444. Proves
  `semp.request_min_interval` and `semp.max_concurrent_per_broker` are honored
  end to end against a real broker, which until now was covered only by unit
  tests against an `httptest` server. It runs four phases, restarting the MCP
  server with different limits each time, and measures what the broker actually
  receives through `semp-tap` (a recording reverse proxy, sources in
  `../e2e-common/semp-tap`) placed in front of broker-a behind a dedicated
  `broker-throttle` alias:

  | Phase | `request_min_interval` | `max_concurrent_per_broker` | Tap delay | Asserts |
  | --- | --- | --- | --- | --- |
  | pacer | `200ms` | `10` (slack) | 0 | every gap ≥ floor, and the aggregate span matches the interval |
  | cap | `0s` | `2` | 150ms | peak in-flight == 2 |
  | pacer-control | `0s` | `10` (slack) | 0 | the inverse of the pacer phase |
  | cap-control | `0s` | `10` (slack) | 150ms | the inverse of the cap phase |

  Each control differs from the phase it validates in exactly one variable, so a
  green control means that specific assertion is live. They are the ticket's
  "sanity-check the assertion" step, kept as real always-on CI assertions rather
  than manual ones, so they cannot rot. Read
  `semp-tap`'s package comment before touching an assertion: the measurement
  window is `[request received → response headers returned]`, matching where
  `Sender.Do` drops its semaphore slot, and it is not the same as full
  body-proxy completion.

  This scenario runs last because it takes over port `9090` from the shared
  server the earlier scenarios use.

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
| semp-tap      | 8084          | (not used)     |
| SMF broker-a  | (not exposed) | 55655          |
| SMF broker-b  | (not exposed) | 55656          |

`semp-tap` is a host-side process, not a container: the throttling scenario's
recording reverse proxy (`SEMP_TAP_PORT` in `.env`).

The MCP server listens on `9090` (override with `MCP_PORT`). All broker ports are
override-able via `.env` (`BROKER_A_SEMP_PORT`, `BROKER_B_SEMP_PORT`).