# E2E Monitoring Suite (SOL-150024)

End-to-end coverage for the broker MCP server's monitoring-oriented tools (queue
listing, RDP discovery, client/subscriber state, message-rate and discard
counters). Runs two Solace brokers in containers, provisions baseline + extended
fixtures, and drives both SEMP-layer and messaging-layer broker state.

## Quickstart

Three commands for a full test run:

```bash
# 1. Bring brokers up. Safe to re-run; does nothing if already up.
bash test/e2e-monitoring/setup-brokers.sh

# 2. Run the monitoring-tools test suite. Safe to re-run many times.
bash test/e2e-monitoring/test-monitoring-tools.sh

# 3. Tear brokers down when you're done.
docker compose -f test/e2e-monitoring/docker-compose.yml down -v
```

During development you can repeat step 2 as many times as you need without
restarting brokers. `test-monitoring-tools.sh` builds the MCP server, applies
all fixtures to both brokers (`solace-e2e-mon-a`, `solace-e2e-mon-b`), then
cleans up fixtures and stops the MCP server on exit. Cleanup runs on every
exit path including `Ctrl-C`, `SIGTERM`, and terminal close (`SIGHUP`);
re-running after a forced kill (`SIGKILL` of the parent) succeeds — the
cleanup-first pattern in `helpers.sh` handles stale fixtures from prior
aborted runs.

## Fixtures

All fixtures apply to both brokers in parallel (`solace-e2e-mon-a`,
`solace-e2e-mon-b`). `list-brokers`, `list-rdps`, and the existing RDP/queue
tools run against the base fixture copied from `e2e-basic-mcp` — no new
fixture needed.

F1, F2, F3, F4, F5, and F6 are implemented today, driven by the
`broker-driver` binary for the client-bearing fixtures (F3–F6).

| ID       | Status      | Fixture                  | Required broker state                                                                                                                                  | Lifecycle                          | MCP tools supported                                                                |
| -------- | ----------- | ------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------ | ---------------------------------- | ---------------------------------------------------------------------------------- |
| F1       | Implemented | Multi-VPN                | Additional non-default VPN `test-vpn` on each broker, created with `enabled=false`                                                                     | one-shot SEMP                      | `list-vpns`, `get-vpn-health` (enabled + disabled state coverage)                  |
| F2       | Implemented | Multi-queue              | `test-queue-2` (bound to a test RDP) and `test-queue-3` (unbound), both non-exclusive on default VPN                                                   | one-shot SEMP                      | `list-queues` (multi-entry + pagination), `get-queue-metrics` (named-object lookup) |
| F3       | Implemented | Connected client         | One long-lived persistent receiver per broker on default VPN with deterministic `clientName` and ≥1 named topic subscription. **Verification:** client appears in `list-clients` and reports the expected subscription. | background broker-driver | `list-clients`, `get-client-details`, `list-client-subscriptions`                  |
| F4       | Implemented | Sustained traffic        | Publisher targets 100 msg/s, 256-byte payload, persistent; broker observes ~92 msg/s sustained after ~5 s settle. **Verification:** `rxMsgRate ≥ 80` and `txMsgRate ≥ 80` after 25 s of fixture runtime. | background broker-driver | `get-message-rates`                                                                |
| F5       | Implemented | Slow guaranteed consumer | Queue `test-queue-slow-consumer` (`maxDeliveredUnackedMsgsPerFlow=10`) fed fast while a queue-bound receiver ACKs every 2 s, so unacked pins at the per-flow ceiling and the spool backs up. **Verification (queue-level signals, not client `slowSubscriber` — SOL-150328/SOL-150344):** `bindCount > 0`, `txUnackedMsgCount` near the `maxDeliveredUnackedMsgsPerFlow=10` ceiling (≥ 80%, i.e. ≥ 8 — a slow-but-nonzero ACK rate makes it oscillate by one), `rxMsgRate > txMsgRate`, and `spooledMsgCount` growing across two samples. | background broker-driver | `get-queue-metrics`, `list-queues` |
| F6-spool | Implemented | Discards via spool quota | Queue `test-queue-discards-spool` with `maxMsgSpoolUsage=1 MB` + `egressEnabled=false`; one-shot publish ~2 MB. **Verification:** `maxMsgSpoolUsageExceededDiscardedMsgCount > 0` after one-shot publish. | one-shot SEMP + one-shot broker-driver publish | `get-discard-stats`              |
| F6-ttl   | Implemented | Discards via TTL expiry  | Queue `test-queue-discards-ttl` with `maxTtl=1 s` + no consumer; one-shot publish + 2 s wait. **Verification:** `maxTtlExpiredDiscardedMsgCount > 0` after one-shot publish. | one-shot SEMP + one-shot broker-driver publish | `get-discard-stats`                         |

Activation order is deterministic: F1 and F2 (SEMP-only) before F3/F4
(client-bearing). F5 and F6 follow F3/F4 — each owns a dedicated queue
independent of the others. F5's consumer binds to its queue, so teardown reaps
the broker-driver before deleting the queue (see Cleanup order).

## Build prerequisites

The `broker-driver` binary links the Solace Go messaging client
(`solace.dev/go/messaging`), which depends on a native library. CGO must be
available. On Linux the native library (including its OpenSSL dependency) is
statically linked, so only a C compiler is required — no `libssl-dev`, and the
built binary has no `libssl`/`libcrypto` runtime dependency.

| Platform                                          | Needed                       | Install                                                                                       |
| ------------------------------------------------- | ---------------------------- | --------------------------------------------------------------------------------------------- |
| Linux (Ubuntu / Debian)                           | `gcc` (for cgo)              | `sudo apt-get install build-essential`                                                         |
| macOS                                             | Xcode CLI tools, OpenSSL 3   | `xcode-select --install && brew install openssl@3`                                            |
| macOS (Homebrew `openssl@3` in non-standard path) | `CGO_LDFLAGS`, `CGO_CFLAGS`  | `export CGO_LDFLAGS="-L$(brew --prefix openssl@3)/lib" CGO_CFLAGS="-I$(brew --prefix openssl@3)/include"` |
| Windows                                           | WSL2 only (treat as Linux)   | —                                                                                             |

GitHub's `ubuntu-latest` runner ships `gcc`, so the CI job needs no extra
install step. Any actively supported Go version works.
`test-monitoring-tools.sh` warns at start-up if the C compiler (or, on macOS,
the OpenSSL headers) is missing.

The shell harness also requires `jq` for JSON assertions in the standalone
scenario (Linux: `sudo apt-get install jq`, macOS: `brew install jq`).

## broker-driver binary

The suite ships one Go program in its own directory with its own `go.mod`:

- **`broker-driver/`** — connects directly to the Solace brokers via the
  Solace messaging client. Publishes, consumes, and sustains traffic to
  produce the broker states the monitoring tools observe. Requires the C
  library `libsolclient` at build time (see Build prerequisites).

The MCP tools themselves are exercised through the bash + `curl` helpers in
`helpers.sh` (`mcp_call_tool`), not a dedicated Go client.

## Design note — broker-driver role

In [test/e2e-basic-mcp](../e2e-basic-mcp), MCP tool testing is the only job:
a single binary (named `agent/` there, predating this suite's split) speaks
the MCP protocol against the running server and validates tool responses.

SOL-150024 adds `broker-driver` — a binary that produces **runtime broker
activity** for fixtures F3–F6 (connected client, sustained publisher,
publish-batch, slow consumer). This is a deliberate departure from the
basic-mcp pattern because SEMP `curl` alone cannot produce messaging-layer
state — only a real SMF client can open a connection or publish at a target
rate. The heavy CGO dependency stays scoped to `broker-driver`; the MCP-tool
assertions run through plain `curl`.

## broker-driver CLI surface

### Subcommand pattern

```
broker-driver <subcommand> [flags]
```

| Subcommand          | Purpose                              | Key flags                                                                                       | Fixture     | MCP tools exercised                                                          |
| ------------------- | ------------------------------------ | ----------------------------------------------------------------------------------------------- | ----------- | ---------------------------------------------------------------------------- |
| `connected-client`  | F3 long-lived receiver               | `--broker {a\|b}`, `--vpn`, `--client-name`, `--queue`, `--subscriptions`                       | F3          | `list-clients`, `get-client-details`, `list-client-subscriptions`            |
| `publisher`         | F4 sustained publisher               | `--broker`, `--vpn`, `--topic`, `--rate`, `--size`, `--message-type`, `--duration`              | F4          | `get-message-rates`                                                          |
| `publish-batch`     | F6 one-shot publisher                | `--broker`, `--vpn`, `--topic`, `--count`, `--size`, `--rate`                                   | F6-spool, F6-ttl | `get-discard-stats`                                                     |
| `slow-consumer`     | F5 fast publisher + slow consumer    | `--broker`, `--vpn`, `--queue`, `--topic`, `--rate`, `--size`, `--ack-delay`                     | F5          | `get-queue-metrics`, `list-queues`                                          |

### Common conventions

All `broker-driver` subcommands share the following contract:

- **Broker target.** `--broker=a` or `--broker=b` selects which container.
  `broker-driver` resolves `host:port` from the suite's `.env`
  (`BROKER_A_SMF_PORT`, `BROKER_B_SMF_PORT`); never accept a raw URL on the
  CLI.
- **VPN.** Defaults to `default`. Override with `--vpn` only when targeting the
  multi-VPN fixture (`test-vpn`).
- **Credentials.** Sourced from `.env` — `E2E_A_USERNAME` / `E2E_A_PASSWORD`
  for broker-a, `E2E_B_USERNAME` / `E2E_B_PASSWORD` for broker-b. Never accept
  credentials on the CLI.
- **Shutdown.** Subcommands install a `SIGINT`/`SIGTERM` handler with a 5 s
  grace window: disconnect SMF session cleanly, flush any in-flight publishes,
  exit 0. On grace expiry, exit 1.
- **PID file.** Each long-running subcommand writes
  `bin/broker-driver-f<N>-<broker>.pid` on startup (e.g.
  `bin/broker-driver-f4-a.pid` for the F4 publisher on broker-a) and removes
  it on clean exit. `helpers.sh`'s `stop_broker_drivers` reaps stragglers via
  these pidfiles during teardown — the glob it watches is
  `bin/broker-driver-f*.pid`.
- **One-shot vs long-running.** `publish-batch` is one-shot (publishes
  `--count` messages, then exits). `connected-client` and `publisher` are
  long-running until signaled or `--duration` elapses.

### Example invocations

`broker-driver connected-client` (F3):

```bash
./bin/broker-driver connected-client \
    --broker=a \
    --client-name=e2e-monitoring-connected-a \
    --queue=test-queue \
    --subscriptions=t1
```

`broker-driver publisher` for F4 (sustained traffic):

```bash
./bin/broker-driver publisher --broker=a --topic=t1 --rate=100 --size=256 --message-type=persistent
```

`broker-driver publisher` with `--duration` (auto-exits, useful when a
wrapping script prefers a bounded process to one it has to signal):

```bash
./bin/broker-driver publisher --broker=a --topic=t1 --rate=100 --size=256 --message-type=persistent --duration=30s
```

Omit `--duration` for long-running fixtures (the default); `helpers.sh` reaps
the process via its pidfile during teardown.

`broker-driver publish-batch` for F6-spool (publish ~2 MB to overflow 1 MB
quota):

```bash
./bin/broker-driver publish-batch --broker=a --topic=t-discards-spool --count=8000 --size=256 --message-type=persistent
```

`broker-driver publish-batch` for F6-ttl:

```bash
./bin/broker-driver publish-batch --broker=a --topic=t-discards-ttl --count=200 --size=256 --message-type=persistent
```

`broker-driver slow-consumer` for F5 (fast publish into a queue + a queue-bound
consumer that ACKs slowly, so the queue-level slow-consumer signals develop):

```bash
./bin/broker-driver slow-consumer \
    --broker=a \
    --queue=test-queue-slow-consumer \
    --topic=e2e-monitoring/slow-consumer/topic \
    --rate=100 --size=256 --ack-delay=2s
```

Long-running like F3/F4; `helpers.sh` reaps it via its pidfile during teardown.

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

### F6 — discard mechanics

- Spool-quota field on monitor API:
  `maxMsgSpoolUsageExceededDiscardedMsgCount`.
- TTL field on monitor API: `maxTtlExpiredDiscardedMsgCount`.
- Both are **cumulative counters** — non-zero persists, so no sustained traffic
  is required after the one-shot publish.

## Cleanup order

`test-monitoring-tools.sh` runs the following cleanup steps, in this order,
when it exits (for any reason — normal end, error, Ctrl-C, killed):

1. **Stop the MCP server** (`stop_server` in `helpers.sh`).
2. **Stop the broker-driver processes** (`stop_broker_drivers` in
   `helpers.sh`). Each broker-driver fixture writes a PID file named
   `bin/broker-driver-f<N>-<broker>.pid` (e.g. `bin/broker-driver-f4-a.pid`)
   when it starts. The stop helper finds all of them, sends a polite
   termination signal, waits up to 5 seconds, then force-kills anything
   still running.
3. **Delete broker fixtures** (`cleanup_fixtures` in `helpers.sh`).
   Order: bindings → consumers → RDPs → queues → VPNs.
4. **Remove the MCP server PID file** (`bin/mcp-server.pid`).

Step 2 must run before step 3 — the broker refuses to delete a queue with
an attached client.

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
