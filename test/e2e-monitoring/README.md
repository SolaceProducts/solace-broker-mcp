# E2E Monitoring Suite (SOL-150024)

End-to-end coverage for the broker MCP server's monitoring-oriented tools (queue
listing, RDP discovery, client/subscriber state, message-rate and discard
counters). Runs two Solace brokers in containers, provisions baseline + extended
fixtures, and drives both SEMP-layer and messaging-layer broker state.

> **Status:** Design-gated. The Go agent binary is not yet implemented. The
> sections below define the CLI surface that must be reviewed and approved by
> the architect reviewer before any Go fixture code lands.

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
| `slow-consumer`     | F5 throttled receiver                | `--broker`, `--vpn`, `--client-name`, `--queue`, `--ack-delay`, `--max-unacked`                 | F5          | `list-slow-subscribers` (asserts `slowSubscriber=true`)                      |
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
  few seconds" — exact threshold is undocumented; F5 must poll for up to
  **~60 s**.
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
