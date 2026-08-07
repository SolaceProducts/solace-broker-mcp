# E2E Monitoring Suite

End-to-end tests for the monitoring tools — `list-vpns`, `list-queues`, `list-clients`,
`get-message-rates`, `list-slow-subscribers`, `list-queue-discards`, `get-discard-stats`,
`get-broker-status`, `list-bridges`, `get-bridge-status`, `list-kafka-receivers`,
`get-kafka-receiver-status`, `list-kafka-senders`, `get-kafka-sender-status`, and others.
Runs two Solace brokers, provisions baseline + extended fixtures (F1–F8), and drives
both SEMP-layer and messaging-layer broker state.

Builds on the shared scaffold in [`../e2e-common/`](../e2e-common/README.md).

An LLM-driven eval harness that exercises these fixtures (F1–F7) through
natural-language prompts via the Claude Code CLI lives in the sibling
[`test/e2e-llm/`](../e2e-llm/README.md) suite. It sources this suite's
`helpers.sh` for fixture provisioning, but is otherwise independent — it's
non-gating and runs manually via the
[`LLM E2E Eval`](../../.github/workflows/llm-eval.yml) workflow.

## Quickstart

```bash
# Full cycle (recommended)
make e2e-monitoring-all

# Or step by step
SUITE_DIR=test/e2e-monitoring bash test/e2e-common/setup-brokers.sh
bash test/e2e-monitoring/run-all.sh
docker compose -f test/e2e-monitoring/docker-compose.yml down -v
```

During development you can repeat `run-all.sh` as many times as you need without
restarting brokers. The orchestrator builds the MCP server, applies all fixtures
to both brokers (`solace-e2e-mon-a`, `solace-e2e-mon-b`), then cleans up fixtures
and stops the MCP server on exit. Cleanup runs on every exit path including
`Ctrl-C`, `SIGTERM`, and terminal close (`SIGHUP`); re-running after a forced
kill (`SIGKILL` of the parent) succeeds — the cleanup-first pattern in
`helpers.sh` handles stale fixtures from prior aborted runs.

## Fixtures

All fixtures apply to both brokers in parallel (`solace-e2e-mon-a`,
`solace-e2e-mon-b`). `list-brokers`, `list-rdps`, `get-broker-status`, and the
existing RDP/queue tools run against the base fixture copied from
`e2e-basic-mcp` — no new fixture needed. `get-broker-status` is broker-wide
(version, scaling tier, system resources, memory and message-spool
utilization), so the default Dockerized broker state already populates every
curated field.

F1–F10 are implemented today, driven by the `broker-driver` binary for the
client-bearing fixtures (F3–F7); F8 is one-shot SEMP only, like F1/F2. F9/F10
are one-shot SEMP too, but unlike every other fixture here they also depend
on a real external Kafka broker (the `kafka` service in docker-compose.yml)
being up first — see `wait_for_kafka` in helpers.sh.

| ID       | Fixture                  | Required broker state                                                                                                                                  | Lifecycle                          | MCP tools supported                                                                |
| -------- | ------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------ | ---------------------------------- | ---------------------------------------------------------------------------------- |
| Base     | None (default state)     | Default Dockerized broker state from `e2e-basic-mcp` — version, scaling tier, system resources, memory, and message-spool utilization are populated out of the box | none (built-in)                    | `list-brokers`, `list-rdps`, `get-broker-status`                                   |
| F1       | Multi-VPN                | Two additional non-default VPNs on each broker: `test-vpn` with `enabled=false` (feeds `list-vpns.disabledCount`), and `test-vpn-empty` with `enabled=true` and no client-user / ACL / queue setup — reports `msgVpnConnections==1` (reserved `#client` broker invariant) and feeds `list-vpns.zeroConnectionCount` via the handler's `<=1` predicate. | one-shot SEMP                      | `list-vpns` (`disabledCount`, `zeroConnectionCount` aggregation coverage), `get-vpn-status` (enabled + disabled state coverage) |
| F2       | Multi-queue              | `test-queue-2` (bound to a test RDP) and `test-queue-3` (unbound), both non-exclusive on default VPN                                                   | one-shot SEMP                      | `list-queues` (multi-entry + pagination), `get-queue-metrics` (named-object lookup) |
| F3       | Connected client         | One long-lived persistent receiver per broker on default VPN with deterministic `clientName` and ≥1 named topic subscription. **Verification:** client appears in `list-clients` and reports the expected subscription. | background broker-driver           | `list-clients`, `get-client-details`, `list-client-subscriptions`                  |
| F4       | Sustained traffic        | Publisher targets 100 msg/s, 256-byte payload, persistent; broker observes ~92 msg/s sustained after ~5 s settle. **Verification:** after 25 s of fixture runtime, peak `rxMsgRate ≥ 80` and peak `txMsgRate ≥ 50` over 5 polls / ~5 s (tx is lower/noisier than rx). | background broker-driver           | `get-message-rates`                                                                |
| F5       | Slow guaranteed consumer | Queue `test-queue-slow-consumer` (`maxDeliveredUnackedMsgsPerFlow=10`) fed fast while a queue-bound receiver ACKs every 2 s, so unacked pins at the per-flow ceiling and the spool backs up. **Verification:** queue-level signals (`bindCount`, `txUnackedMsgCount`, `rxMsgRate > txMsgRate`, growing `spooledMsgCount`) — see [F5 — slow-consumer signals](#f5--slow-consumer-signals). | background broker-driver           | `get-queue-metrics`, `list-queues`                                                 |
| F6       | Slow direct subscriber   | A direct topic subscriber on each broker is `SIGSTOP`ed while a separate publisher floods its topic (`rate=3000`, `size=50000`), closing its TCP egress window so the broker sets the per-client `slowSubscriber` flag. Distinct from F5: this is the per-client flag a slow-ACK guaranteed consumer never trips (SOL-150328). **Verification:** `clients/<name>.slowSubscriber == true` (polled — rolling ~1 min window). | background broker-driver (subscriber `SIGSTOP`ed + flood publisher) | `list-slow-subscribers`                                                            |
| F7-spool | Discards via spool quota | Queue `test-queue-discards-spool` with `maxMsgSpoolUsage=1 MB` + `egressEnabled=false`; one-shot publish ~2 MB. **Verification:** `maxMsgSpoolUsageExceededDiscardedMsgCount > 0` after one-shot publish. | one-shot SEMP + one-shot broker-driver publish | `list-queue-discards` (per-queue), `get-discard-stats` (broker-wide)               |
| F7-ttl   | Discards via TTL expiry  | Queue `test-queue-discards-ttl` with `maxTtl=1 s` + no consumer; one-shot publish + 2 s wait. **Verification:** `maxTtlExpiredDiscardedMsgCount > 0` after one-shot publish. | one-shot SEMP + one-shot broker-driver publish | `list-queue-discards` (per-queue), `get-discard-stats` (broker-wide)               |
| F8       | Bridges (SOL-152231)     | Three bridges per broker, each pointed at the *other* broker (the only fixture that's inherently cross-broker rather than independent per side): `test-bridge` (healthy, bidirectional once both sides exist), `test-bridge-failing` (enabled, remote location `127.0.0.1:1` — connection refused, never converges), `test-bridge-disabled` (`enabled=false`). **Verification:** `list-bridges`/`get-bridge-status` field values — see [F8 — bridge connection-state findings](#f8--bridge-connection-state-findings). | one-shot SEMP                      | `list-bridges` (`downCount`/`disabledCount` aggregation), `get-bridge-status` (compound-identifier lookup) |
| F9       | Kafka Receivers (SOL-152370) | Three Kafka Receivers per broker, all bridging to the real `kafka` service (apache/kafka:3.7.0, KRaft mode) in this compose file: `test-kafka-receiver` (healthy, topic-bound to a real Kafka topic), `test-kafka-receiver-failing` (enabled, unreachable bootstrap address `127.0.0.1:1`), `test-kafka-receiver-disabled` (`enabled=false`). **Verification:** `list-kafka-receivers`/`get-kafka-receiver-status` field values — see [F9/F10 — Kafka Receiver/Sender findings](#f9f10--kafka-receiversender-findings). | one-shot SEMP (+ real Kafka broker) | `list-kafka-receivers` (`downCount`/`disabledCount`/`byFailureReason` aggregation), `get-kafka-receiver-status` (healthy + down detail) |
| F10      | Kafka Senders (SOL-152370)   | Three Kafka Senders per broker, same pattern as F9: `test-kafka-sender` (healthy, queue-bound to a real Kafka topic), `test-kafka-sender-failing` (unreachable), `test-kafka-sender-disabled` (`enabled=false`). **Verification:** `list-kafka-senders`/`get-kafka-sender-status` field values — see [F9/F10 — Kafka Receiver/Sender findings](#f9f10--kafka-receiversender-findings). | one-shot SEMP (+ real Kafka broker) | `list-kafka-senders` (`downCount`/`disabledCount`/`byFailureReason` aggregation), `get-kafka-sender-status` (healthy + down detail) |

Activation order is deterministic: F1 and F2 (SEMP-only) before F3/F4
(client-bearing). F5, F6, and F7 follow F3/F4 — each owns dedicated resources
independent of the others. F5's consumer binds to its queue, so teardown reaps
the broker-driver before deleting the queue (see Cleanup order). F6 owns no
queue (direct messaging); its `create_*` blocks until the `slowSubscriber` flag
has flipped, so tool tests can read it immediately afterward.

## Build prerequisites

The `broker-driver` binary links the Solace Go messaging client
(`solace.dev/go/messaging`), which depends on a native library. CGO must be
available. On Linux the native library (including its OpenSSL dependency) is
statically linked, so only a C compiler is required — no `libssl-dev`, and the
built binary has no `libssl`/`libcrypto` runtime dependency.

| Platform                                          | Needed                       | Install                                                                                       |
| ------------------------------------------------- | ---------------------------- | --------------------------------------------------------------------------------------------- |
| Linux (Ubuntu / Debian)                           | `gcc` (for cgo)              | `sudo apt-get install build-essential`                                                        |
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

**Why a dedicated binary?** In [test/e2e-basic-mcp](../e2e-basic-mcp), MCP tool
testing is the only job: a single binary (named `agent/` there, predating this
suite's split) speaks the MCP protocol against the running server and validates
tool responses. SOL-150024 adds `broker-driver` because SEMP `curl` alone cannot
produce the **messaging-layer** state fixtures F3–F7 need (connected client,
sustained publisher, slow consumer, slow direct subscriber, publish-batch) —
only a real SMF client can open a connection or publish at a target rate. The
heavy CGO dependency stays scoped to `broker-driver`.

The MCP tools themselves are exercised through the bash + `curl` helpers in
`helpers.sh` (`mcp_call_tool`), not a dedicated Go client.

## broker-driver CLI surface

### Subcommand pattern

```
broker-driver <subcommand> [flags]
```

| Subcommand               | Purpose                                                                | Key flags                                                                          | Fixture          | MCP tools exercised                                               |
| ------------------------ | ---------------------------------------------------------------------- | ---------------------------------------------------------------------------------- | ---------------- | ----------------------------------------------------------------- |
| `connected-client`       | F3 long-lived receiver                                                 | `--broker {a\|b}`, `--vpn`, `--client-name`, `--queue`, `--subscriptions`          | F3               | `list-clients`, `get-client-details`, `list-client-subscriptions` |
| `publisher`              | F4 sustained publisher                                                 | `--broker`, `--vpn`, `--topic`, `--rate`, `--size`, `--message-type`, `--duration` | F4               | `get-message-rates`                                               |
| `slow-consumer`          | F5 fast publisher + slow consumer                                      | `--broker`, `--vpn`, `--queue`, `--topic`, `--rate`, `--size`, `--ack-delay`       | F5               | `get-queue-metrics`, `list-queues`                                |
| `slow-direct-subscriber` | F6 direct subscriber (`SIGSTOP`ed by harness to flip `slowSubscriber`) | `--broker`, `--vpn`, `--client-name`, `--topic`                                    | F6               | `list-slow-subscribers`                                           |
| `publish-batch`          | F7 one-shot publisher                                                  | `--broker`, `--vpn`, `--topic`, `--count`, `--size`, `--rate`                      | F7-spool, F7-ttl | `list-queue-discards`, `get-discard-stats`                        |

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

`broker-driver publish-batch` for F7-spool (publish ~2 MB to overflow 1 MB
quota):

```bash
./bin/broker-driver publish-batch --broker=a --topic=t-discards-spool --count=8000 --size=256 --message-type=persistent
```

`broker-driver publish-batch` for F7-ttl:

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

- The F4 publisher's own sustained rate stabilizes within **~5 s** at **~92
  msg/s** against a target of 100 (~8 % undershoot from publisher loop
  overhead). The assertion reads SEMP's VPN-level `rxMsgRate` — a *VPN-wide
  aggregate* that may be higher than 92 msg/s while concurrent fixtures (e.g.
  F6 flood publisher) are running — so it's a floor check (`≥ 80`), not an
  exact-match. The 80 floor is grounded in the F4 publisher's ~92 msg/s
  contribution alone, so it holds even when F4 is the only active publisher.
- Instantaneous `txMsgRate` (delivery to the F3 receiver) is inherently lower
  and noisier than the publish rate — single-read samples on CI runners have
  been observed across **57–88 msg/s** (SOL-150715). Asserted against `≥ 50`
  with **peak-of-5 polling** (~5 s window) to absorb that variance without
  lowering the bar further.
- `averageRxMsgRate` requires **~3–5 min** to converge. **Do not assert against
  it** — the F4 window is too short.
- Use the instantaneous fields: `rxMsgRate`, `txMsgRate`.
- A 25 s settle window gives ~20 s margin above the empirical stable point;
  the assertion then polls 5× / ~5 s on top of that.

### F5 — slow-consumer signals

F5 asserts **queue-level** slow-consumer signals, not the per-client
`slowSubscriber` flag. A slow-ACK guaranteed consumer never trips that flag —
flipping it is F6's job (see SOL-150328/SOL-150344). Once the consumer binds and
the publisher ramps, check:

- `bindCount > 0` — the slow consumer is attached.
- `txUnackedMsgCount` near the `maxDeliveredUnackedMsgsPerFlow=10` ceiling
  (≥ 80%, i.e. ≥ 8). A slow-but-nonzero ACK rate makes it oscillate by one, so
  assert the threshold, not equality.
- `rxMsgRate > txMsgRate` — the queue ingests faster than it drains.
- `spooledMsgCount` growing across two samples — the backlog is building.

### F7 — discard mechanics

- Spool-quota field on monitor API:
  `maxMsgSpoolUsageExceededDiscardedMsgCount`.
- TTL field on monitor API: `maxTtlExpiredDiscardedMsgCount`.
- Both are **cumulative counters** — non-zero persists, so no sustained traffic
  is required after the one-shot publish.

### F8 — bridge connection-state findings

Lab-verified against SEMP 2.46 (the `solace-pubsub-standard:latest` image this
suite runs). These are load-bearing for `list_bridges.go`'s classification
logic and for how the F8 fixtures/assertions are built — don't assume the
SEMP spec's prose description of a field is the full story without checking
real broker behavior first.

- **`outboundState` depends on the peer's reciprocal bridge existing.** A
  bridge configured on only one side (e.g. broker-a → broker-b with no
  bridge configured back from broker-b) reports `outboundState:
  "not-applicable"` even though `inboundState` is already `"ready-in-sync"`.
  Once both sides are configured, `outboundState` converges to `"ready"` on
  both. This is why `create_bridges_on` must run for **both** brokers before
  `verify_bridges_on` polls either one.
- **`inboundFailureReason` does not reliably populate for connection-level
  failures**, unlike RDPs' `lastFailureReason`. Tested three ways — an
  unreachable host (connection refused), a nonexistent remote VPN name, and
  wrong credentials — all three left `inboundFailureReason` empty and
  `rxConnectionFailureCategory` at `"no-failure"` indefinitely (polled up to
  40s), while `inboundState` correctly settled at `"not-ready-wait-next"`.
  The `test-bridge-failing` fixture and its assertions rely on `inboundState`
  / `downCount`, not `byInboundFailureReason`, for this reason. An
  **admin-disabled** bridge is the one case that *does* populate a reason
  (`inboundFailureReason: "Bridge disabled"`) — which is exactly why the
  postprocess handler must exclude admin-disabled bridges from
  `byInboundFailureReason` (mirrors `list-rdps`' "RDP Shutdown" exclusion);
  without a real down-but-unreachable fixture to contrast against, that
  exclusion's necessity wouldn't be obvious from unit tests alone.
- **Deleting a bridge cascades its `remoteMsgVpns` sub-resource** — unlike
  RDPs, which require deleting `queueBindings`/`restConsumers` before the RDP
  itself. `cleanup_bridges_on` only issues one `DELETE` per bridge.
- **Bridges use the public `/SEMP/v2/config` API**, same as every other
  fixture in this suite (`BROKER_A_SEMP_CONFIG`/`BROKER_B_SEMP_CONFIG`) — no
  need for `__private_config__`.
- **The pre-provisioned `default` client-username** (empty password, already
  used by `connected_client.go` for F3/F4/F5) works for a bridge's remote
  authentication too — no new client-username fixture was needed.

### F9/F10 — Kafka Receiver/Sender findings

SOL-152328 added `list-kafka-receivers`, `get-kafka-receiver-status`,
`list-kafka-senders`, and `get-kafka-sender-status` spec-derived, not
lab-verified. SOL-152370 initially concluded live verification was blocked
by a license/edition gate with no way around it; further investigation
during the same story found that conclusion was wrong — it's a scaling
default, not a license restriction, and is fully unlockable. These findings
are load-bearing for `docker-compose.yml`'s `SYSTEM_SCALING_*` env vars and
for how the F9/F10 fixtures/assertions are built.

- **Creating a Kafka Receiver/Sender at all is gated by two scaling
  settings that default to 0** on `solace/solace-pubsub-standard:latest`:
  `POST .../kafkaReceivers` initially failed with `400 MAX_NUM_EXCEEDED:
  "Kafka Bridge limit of 0 reached."` The confd env-backend keys are
  `/system/scaling/maxkafkabridgecount` and
  `/system/scaling/maxkafkabrokerconnectioncount` — set via
  `SYSTEM_SCALING_MAXKAFKABRIDGECOUNT`/
  `SYSTEM_SCALING_MAXKAFKABROKERCONNECTIONCOUNT` (note the `SYSTEM_` prefix,
  not `SOLACE_` — an initial wrong guess at the variable name silently
  no-ops instead of erroring, since confd's env backend just falls through
  to the default for an unrecognized key rather than failing). **Both are
  enums, not free integers** — confirmed against the broker's own SEMP RPC
  schema (`consolidated-semp-rpc-soltr.xsd`): bridge count must be one of
  `0, 10, 50, 200`; broker-connection count must be one of `0, 300, 2000,
  10000`. Any other value fails confd's config check and the container
  exits at boot (`ERROR "Invalid max kafka broker connections config key
  value 100"` — a value we tried before finding the real enum). This suite
  uses `10`/`300`.
- **`failureReason` DOES populate reliably**, unlike bridges'
  `inboundFailureReason` above (which never populates for connection-level
  failures). Lab-verified stable across 45s of polling: `"Shutdown"` for an
  admin-disabled Kafka Receiver/Sender, `"No remote-broker in UP state"` for
  one pointed at an unreachable bootstrap address. This is why
  `byFailureReason` is asserted to have a real entry in
  `test_list_kafka_receivers_summary`/`test_list_kafka_senders_summary`,
  unlike the equivalent bridges assertion.
- **A healthy Kafka Receiver/Sender converges to `up: true` quickly**
  (5–7s observed) once it can actually reach a real Kafka broker and has at
  least one enabled topic/queue binding — confirmed against
  `apache/kafka:3.7.0` in KRaft mode (the `kafka` service in
  `docker-compose.yml`). A **queue binding's `enabled` field defaults to
  `false`** regardless of the rest of the request body — a binding created
  without explicitly setting it never carries traffic and the sender never
  reports up; `create_kafka_senders_on` sets it explicitly in the same POST
  that creates the binding (config field names: `topicName`/`localTopic`
  for a receiver's topic binding, `queueName`/`remoteTopic` for a sender's
  queue binding).
- **Deleting a Kafka Receiver/Sender does NOT cascade its
  topicBindings/queueBindings sub-resource** — unlike bridges'
  `remoteMsgVpns`, but like RDPs' `queueBindings`/`restConsumers`.
  `cleanup_kafka_receivers_on`/`cleanup_kafka_senders_on` delete the binding
  before the parent object.
- **`get-kafka-receiver-status`/`get-kafka-sender-status`'s `select` clause
  (`topicBindingCount`, `topicBindingUpCount`, `queueBindingCount`,
  `queueBindingUpCount`) initially looked broken** — those exact field
  names 400 with `select query error: 'topicBindingCount' not a valid
  attribute` against the broker's *public* `/SEMP/v2/monitor` API. That was
  a red herring from testing the wrong API by hand: this server's composite
  executor has always called the *private* `/SEMP/v2/__private_monitor__`
  API instead (the embedded spec's actual `basePath` — an existing,
  deliberate pattern in this codebase, see
  `internal/tools/queuemetrics/handler.go`), where these fields work
  correctly. No code change was needed; this is a note for the next person
  who reaches for `curl` to spot-check a Kafka tool's fields by hand against
  the public API and gets confused by a mismatch that isn't real.

## Cleanup order

`test-monitoring-tools.sh` runs the following cleanup steps, in this order,
when it exits (for any reason — normal end, error, Ctrl-C, killed):

1. **Stop the MCP server** (`stop_server` in `helpers.sh`).
2. **Stop the broker-driver processes** (`stop_broker_drivers` in
   `helpers.sh`). Each broker-driver fixture writes a PID file named
   `bin/broker-driver-f<N>-<broker>.pid` (e.g. `bin/broker-driver-f4-a.pid`)
   when it starts. The stop helper finds all of them, first sends `SIGCONT`
   (so the deliberately-`SIGSTOP`ed F6 subscriber can react), then a polite
   termination signal, waits up to 5 seconds, then force-kills anything
   still running.
3. **Delete broker fixtures** (`cleanup_fixtures` in `helpers.sh`).
   Order: bindings → consumers → RDPs → queues → VPNs → bridges. Bridges have
   no ordering dependency on any other fixture (their delete cascades their
   own `remoteMsgVpns` sub-resource — see F8 above) — they're placed here
   only because that's where `create_bridges_on` was inserted.
4. **Remove the MCP server PID file** (`bin/mcp-server.pid`).

Step 2 must run before step 3 — the broker refuses to delete a queue with
an attached client.

## Port allocation

Distinct from `e2e-basic-mcp` and `e2e-llm` so all three suites can run
concurrently. `e2e-llm` matters here specifically because it sources this
suite's `helpers.sh` for its fixture code (see the Fixtures section above),
so the two stacks are meant to coexist, not just avoid clashing by luck:

| Resource       | e2e-basic-mcp | e2e-monitoring | e2e-llm |
| -------------- | ------------- | -------------- | ------- |
| SEMP broker-a  | 8080          | 8090           | 8102    |
| SEMP broker-b  | 8082          | 8092           | 8104    |
| SMF broker-a   | (not exposed) | 55655          | 55661   |
| SMF broker-b   | (not exposed) | 55656          | 55662   |
| Kafka (F9/F10) | n/a           | 9096           | n/a     |
| MCP server     | (varies)      | 9090           | 9094    |

`KAFKA_PORT` defaults to 9096, not 9094 — 9094 is already claimed by
`e2e-llm`'s MCP server (`test/e2e-llm/targets/local-docker.env`), and CI
running the two suites as separate jobs on separate runners would not catch
a collision that only bites on a local concurrent run.

All override-able via `.env`: `BROKER_A_SEMP_PORT`, `BROKER_B_SEMP_PORT`,
`BROKER_A_SMF_PORT`, `BROKER_B_SMF_PORT`, `KAFKA_PORT`.
