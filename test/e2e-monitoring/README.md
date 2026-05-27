# E2E Monitoring Suite (SOL-150024)

End-to-end coverage for the broker MCP server's monitoring-oriented tools (queue
listing, RDP discovery, client/subscriber state, message-rate and discard
counters). Runs two Solace brokers in containers, provisions baseline + extended
fixtures, and drives both SEMP-layer and messaging-layer broker state.

## Quickstart

Brokers must already be running. Bring them up once and leave them up across
runs:

```bash
docker compose -f test/e2e-monitoring/docker-compose.yml up -d
```

Then run the suite:

```bash
bash test/e2e-monitoring/run_all.sh
```

`run_all.sh` builds the MCP server, applies all fixtures to both brokers
(`solace-e2e-mon-a`, `solace-e2e-mon-b`), runs both test scenarios
(standalone curl + Go agent), then cleans up fixtures and stops the MCP
server. Brokers are left running for the next invocation. Exit 0 on success.

Cleanup runs on every exit path including `Ctrl-C` and `SIGTERM`; re-running
after a forced kill (`SIGKILL` of the parent) succeeds — the cleanup-first
pattern in `helpers.sh` handles stale fixtures from prior aborted runs.

Tear down brokers when done:

```bash
docker compose -f test/e2e-monitoring/docker-compose.yml down -v
```

## Fixtures

All fixtures apply to both brokers in parallel (`solace-e2e-mon-a`,
`solace-e2e-mon-b`). `list-brokers`, `list-rdps`, and the existing RDP/queue
tools run against the base fixture copied from `e2e-basic-mcp` — no new
fixture needed.

F1 and F2 are implemented today. F3–F6 are planned (tracked under
SOL-150024); the Go agent work that drives the client-bearing fixtures has
not yet landed.

| ID       | Status      | Fixture                  | Required broker state                                                                                                                                  | Lifecycle                          | MCP tools supported                                                                |
| -------- | ----------- | ------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------ | ---------------------------------- | ---------------------------------------------------------------------------------- |
| F1       | Implemented | Multi-VPN                | Additional non-default VPN `test-vpn` on each broker, created with `enabled=false`                                                                     | one-shot SEMP                      | `list-vpns`, `get-vpn-health` (enabled + disabled state coverage)                  |
| F2       | Implemented | Multi-queue              | `test-queue-2` (bound to a test RDP) and `test-queue-3` (unbound), both non-exclusive on default VPN                                                   | one-shot SEMP                      | `list-queues` (multi-entry + pagination), `get-queue-metrics` (named-object lookup) |
| F3       | Planned     | Connected client         | One long-lived persistent receiver per broker on default VPN with deterministic `clientName` and ≥1 named topic subscription                           | background goroutine in agent      | `list-clients`, `get-client-details`, `list-client-subscriptions`                  |
| F4       | Planned     | Sustained traffic        | Persistent publisher to topic `t1` at 100 msg/s, 256-byte payload; the F3 receiver drains via its subscription                                         | background goroutine               | `get-message-rates`                                                                |
| F5       | Planned     | Slow subscriber          | Fast publisher + throttled consumer bound to dedicated queue `test-queue-slow`; sustained until broker flips `slowSubscriber=true` on the consumer's client | background goroutine          | `list-slow-subscribers`, `slowSubscriber` field on `list-clients` / `get-client-details` |
| F6-spool | Planned     | Discards via spool quota | Queue `test-queue-discards-spool` with `maxMsgSpoolUsage=1 MB` + `egressEnabled=false`; one-shot publish ~2 MB                                          | one-shot SEMP + one-shot Go publish | `get-discard-stats` — `maxMsgSpoolUsageExceededDiscardedMsgCount > 0`              |
| F6-ttl   | Planned     | Discards via TTL expiry  | Queue `test-queue-discards-ttl` with `maxTtl=1 s` + no consumer; one-shot publish + 2 s wait                                                            | one-shot SEMP + one-shot Go publish | `get-discard-stats` — `maxTtlExpiredDiscardedMsgCount > 0`                         |

Activation order is deterministic: F1 and F2 (SEMP-only) before F3/F4/F5
(client-bearing). F6 runs in parallel — its queues are independent of the
others.

## Build prerequisites

The agent links the Solace Go messaging client (`solace.dev/go/messaging`),
which depends on a native library. CGO must be available.

| Platform                                          | Needed                       | Install                                                                                       |
| ------------------------------------------------- | ---------------------------- | --------------------------------------------------------------------------------------------- |
| Linux (Ubuntu / Debian)                           | `gcc`, `libssl-dev`          | `sudo apt-get install build-essential libssl-dev`                                             |
| macOS                                             | Xcode CLI tools, OpenSSL 3   | `xcode-select --install && brew install openssl@3`                                            |
| macOS (Homebrew `openssl@3` in non-standard path) | `CGO_LDFLAGS`, `CGO_CFLAGS`  | `export CGO_LDFLAGS="-L$(brew --prefix openssl@3)/lib" CGO_CFLAGS="-I$(brew --prefix openssl@3)/include"` |
| Windows                                           | WSL2 only (treat as Linux)   | —                                                                                             |

Any actively supported Go version works. `run_all.sh` warns at start-up if the
toolchain or OpenSSL headers are missing.

The shell harness also requires `jq` for JSON assertions in the standalone
scenario (Linux: `sudo apt-get install jq`, macOS: `brew install jq`).

## Design note — expanded agent role

In [test/e2e-basic-mcp](../e2e-basic-mcp), the agent binary's sole role is MCP
tool testing: it speaks the MCP protocol against the running server and
validates tool responses.

SOL-150024 expands the agent's role. In this suite, the agent also drives
**runtime broker activity** for fixtures F3–F6 (connected client, sustained
publisher, slow consumer, publish-batch). This is a deliberate departure from
the basic-mcp pattern because SEMP `curl` alone cannot produce messaging-layer
state — only a real SMF client can open a connection, publish at a target rate,
or stall acks long enough to flip `slowSubscriber=true`.

The `test` subcommand preserves the original MCP-test-runner role. New
subcommands drive fixture state. Both modes share the same binary and the same
`.env`-derived broker credentials.

## Agent CLI surface

### Subcommand pattern

```
agent <subcommand> [flags]
```

| Subcommand          | Purpose                              | Key flags                                                                                       | Fixture     | MCP tools exercised                                                          |
| ------------------- | ------------------------------------ | ----------------------------------------------------------------------------------------------- | ----------- | ---------------------------------------------------------------------------- |
| `test <mcp-url>`    | (existing) MCP SDK validation        | —                                                                                               | —           | —                                                                            |
| `connected-client`  | F3 long-lived receiver               | `--broker {a\|b}`, `--vpn`, `--client-name`, `--queue`, `--subscriptions`                       | F3          | `list-clients`, `get-client-details`, `list-client-subscriptions`            |
| `publisher`         | F4 / F5-driver sustained publisher   | `--broker`, `--vpn`, `--topic`, `--rate`, `--size`, `--message-type`, `--duration`              | F4, F5      | `get-message-rates`                                                          |
| `slow-consumer`     | F5 throttled receiver                | `--broker`, `--vpn`, `--client-name`, `--queue`, `--ack-delay`, `--max-unacked`                 | F5          | `list-slow-subscribers`, `slowSubscriber` field on `list-clients` / `get-client-details` |
| `publish-batch`     | F6 one-shot publisher                | `--broker`, `--vpn`, `--topic`, `--count`, `--size`, `--rate`                                   | F6-spool, F6-ttl | `get-discard-stats`                                                     |

### Common conventions

All non-`test` subcommands share the following contract:

- **Broker target.** `--broker=a` or `--broker=b` selects which container. The
  agent resolves `host:port` from the suite's `.env` (`BROKER_A_SMF_PORT`,
  `BROKER_B_SMF_PORT`); never accept a raw URL on the CLI.
- **VPN.** Defaults to `default`. Override with `--vpn` only when targeting the
  multi-VPN fixture (`test-vpn`).
- **Credentials.** Sourced from `.env` — `E2E_A_USERNAME` / `E2E_A_PASSWORD`
  for broker-a, `E2E_B_USERNAME` / `E2E_B_PASSWORD` for broker-b. Never accept
  credentials on the CLI.
- **Shutdown.** Subcommands install a `SIGINT`/`SIGTERM` handler with a 5 s
  grace window: disconnect SMF session cleanly, flush any in-flight publishes,
  exit 0. On grace expiry, exit 1.
- **PID file.** Each long-running subcommand writes
  `bin/agent-<subcommand>-<broker>.pid` on startup and removes it on clean
  exit. `helpers.sh` reaps stragglers via these pidfiles during teardown.
- **One-shot vs long-running.** `publish-batch` is one-shot (publishes
  `--count` messages, then exits). `connected-client`, `publisher`, and
  `slow-consumer` are long-running until signaled or `--duration` elapses.

### Example invocations

`agent connected-client` (F3):

```bash
./bin/agent connected-client \
    --broker=a \
    --client-name=e2e-monitoring-connected-a \
    --queue=test-queue \
    --subscriptions=t1
```

`agent publisher` for F4 (sustained traffic):

```bash
./bin/agent publisher --broker=a --topic=t1 --rate=100 --size=256 --message-type=persistent
```

`agent publisher` for F5 driver (overruns slow consumer):

```bash
./bin/agent publisher --broker=a --topic=t-slow --rate=200 --size=256 --message-type=persistent
```

`agent publisher` with `--duration` (auto-exits, useful when a wrapping script
prefers a bounded process to one it has to signal):

```bash
./bin/agent publisher --broker=a --topic=t1 --rate=100 --size=256 --message-type=persistent --duration=30s
```

Omit `--duration` for long-running fixtures (the default); `helpers.sh` reaps
the process via its pidfile during teardown.

`agent slow-consumer` (F5):

```bash
./bin/agent slow-consumer \
    --broker=a \
    --client-name=e2e-monitoring-slow-a \
    --queue=test-queue-slow \
    --ack-delay=10000
```

`agent publish-batch` for F6-spool (publish ~2 MB to overflow 1 MB quota):

```bash
./bin/agent publish-batch --broker=a --topic=t-discards-spool --count=8000 --size=256 --message-type=persistent
```

`agent publish-batch` for F6-ttl:

```bash
./bin/agent publish-batch --broker=a --topic=t-discards-ttl --count=200 --size=256 --message-type=persistent
```

## Empirical timing notes

These numbers were measured against the suite's broker images. Treat them as
load-bearing constants for assertion windows; revisit if the broker image or
fixture sizes change.

### F4 — message rates

- Instantaneous `rxMsgRate` stabilizes within **~5 s** at **~92 msg/s** against
  a target of 100 (~8 % undershoot from publisher loop overhead).
- `averageRxMsgRate` requires **~3–5 min** to converge. **Do not assert against
  it** — the F4 window is too short.
- Use the instantaneous fields: `rxMsgRate`, `txMsgRate`.
- A 25 s assertion window gives ~20 s margin above the empirical stable point.

### F5 — slow subscriber

- `slowSubscriber=true` flips when the broker has had to block delivery for "a
  few seconds" — exact threshold is undocumented. Typical settle is **~30 s**;
  the assertion polls for up to **~60 s** before failing. Don't tune the
  window down based on the typical number — the broker is variable.
- Throttling recipe: don't ack (or `--ack-delay ≥ 5000 ms`); the broker flow
  window stalls; eventually the flag flips.

### F6 — discard mechanics

- Spool-quota field on monitor API:
  `maxMsgSpoolUsageExceededDiscardedMsgCount`.
- TTL field on monitor API: `maxTtlExpiredDiscardedMsgCount`.
- Both are **cumulative counters** — non-zero persists, so no sustained traffic
  is required after the one-shot publish.

## Cleanup order

Strict ordering — violations leave dangling state that fails the next run:

1. Kill agent fixture goroutines (or kill the agent binary via its pidfile).
2. `DELETE` queue bindings on RDPs.
3. `DELETE` REST consumers.
4. `DELETE` RDPs.
5. `DELETE` queues.
6. `DELETE` VPNs (cascade removes the user).

`helpers.sh` enforces this order in `cleanup_fixtures` / `cleanup_*_on`
helpers.

## Code-reuse strategy

This suite intentionally duplicates `helpers.sh` and related scripts from
[test/e2e-basic-mcp](../e2e-basic-mcp). Duplication is accepted for SOL-150024
to keep scope focused. A separate refactor story will extract shared helpers
into `test/lib/common-helpers.sh` once ≥3 e2e suites exist.

## Port allocation

Distinct from `e2e-basic-mcp` so both suites can run concurrently:

| Resource       | e2e-basic-mcp | e2e-monitoring |
| -------------- | ------------- | -------------- |
| SEMP broker-a  | 8080          | 8090           |
| SEMP broker-b  | 8082          | 8092           |
| SMF broker-a   | (not exposed) | 55655          |
| SMF broker-b   | (not exposed) | 55656          |

All override-able via `.env`: `BROKER_A_SEMP_PORT`, `BROKER_B_SEMP_PORT`,
`BROKER_A_SMF_PORT`, `BROKER_B_SMF_PORT`.
