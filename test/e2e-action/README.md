# E2E Action Suite (SOL-150455)

End-to-end coverage for the broker MCP server's **action tools** — the SEMPv2
Action-API PUTs that mutate broker state:

| Tool | Action | Destructive |
| ---- | ------ | ----------- |
| `delete-queue-messages` | drain all spooled messages from a queue | yes |
| `clear-queue-stats`     | reset a queue's statistics counters      | no  |
| `disconnect-client`     | forcibly disconnect a connected client   | yes |
| `clear-client-stats`    | reset a client's statistics counters     | no  |

Unlike the read-only monitoring suite, these tools change broker state, so each
test (a) sets up a known mutable state on a disposable fixture, (b) invokes the
action over the MCP JSON-RPC wire, and (c) verifies the state actually changed —
on both brokers, with cross-broker isolation and tool-annotation checks.

## Quickstart

```bash
# 1. Bring the action brokers up (own compose; distinct ports).
SUITE_DIR=test/e2e-action bash test/e2e-common/setup-brokers.sh
# or: make e2e-action-up

# 2. Run the suite (builds broker-driver + a write-enabled MCP server, runs tests).
bash test/e2e-action/run-all.sh
# or: make e2e-action

# 3. Tear the brokers down.
docker compose -f test/e2e-action/docker-compose.yml down -v
# or: make e2e-action-down
```

`make e2e-action-all` runs the full up → wait → run → teardown cycle (tears down
even on failure).

## Reuse assessment (FR-0)

The `test/e2e-monitoring/` scaffold was reusable, so per the story it was
promoted to a shared location rather than copied. The generic half of the
scaffold lives in [`test/e2e-common/lib.sh`](../e2e-common/lib.sh) — broker
readiness, MCP server lifecycle, config generation, SEMP ops, the MCP JSON-RPC
wire, assertions, the test runner, **and the broker-driver lifecycle helpers**
(`build_broker_driver`, `wait_for_pidfile`, `stop_broker_drivers`), which this
suite added there. The `broker-driver/` sources also moved to
`test/e2e-common/broker-driver/` now that two suites use them. (Post-action
value polling reuses the existing `verify_monitor_object` — its optional jq
`predicate` argument waits until a field matches, so no new poll helper was
needed.) This suite is a category-based peer of `e2e-monitoring` and
`e2e-management`: its `helpers.sh` sources the shared lib and adds only the
action fixtures. No helper logic is duplicated by copy-paste; the monitoring and
basic-mcp suites continue to pass unchanged.

The MCP server runs with `enable_write_tools: true` (unconditional in the shared
`write_config`), which registers read **and** write tools on one server — there
is no separate read-only server (a disposable harness gains nothing from
mirroring a monitoring-only prod deployment).

## The broker-driver

The action tools act on **messaging-layer** state — spooled messages, connected
clients — that the SEMP config/monitor API cannot create. The
[`broker-driver`](../e2e-common/broker-driver/) binary (Go + libsolclient over
SMF) manufactures it: `publish-batch` spools N persistent messages into a queue;
`connected-client` holds a long-lived named client connection. Building it needs
a C compiler (CGo); see the monitoring suite README for platform notes.

## Test catalog & action↔fixture mapping

Fixture model: **per-test ownership** — each test creates its own `e2e-action-*`
object, acts, asserts, and deletes it. `sweep_action_fixtures` runs on entry
(pre-clean) and via an exit trap (safety net), reaping broker-driver clients
before dropping queues.

| Test | Fixture | Setup | Assertion (lab-verified) |
| ---- | ------- | ----- | ------------------------ |
| `clear-queue-stats` (a/b) | `e2e-action-clearstats-queue-<broker>` | spool 20, no consumer → `spooledMsgCount=20` | after: `spooledMsgCount == 0` |
| `clear-client-stats` (a/b) | `e2e-action-clearstats-client-<broker>` (+ bind queue) | direct traffic → `dataTxMsgCount > 0` | after: `dataTxMsgCount == 0` |
| `delete-queue-messages` (a/b) | `e2e-action-deletemsgs-<broker>` | spool 20, no consumer → `currentMsgCount=20` | after: `liveDepth.currentMsgCount == 0` |
| `disconnect-client` (a/b) | `e2e-action-disc-<broker>` | connected → `clientId=C1` | after: `clientId != C1` (new session) |
| read-after-write (a/b) | `e2e-action-deletemsgs-<broker>` | read N via `get-queue-metrics` | after delete: same tool reads `0`, not stale N |
| cross-broker isolation (deleteMsgs) | `e2e-action-deletemsgs-iso` on both | spool 20 on both | delete on a → a is 0, **b still 20** |
| cross-broker isolation (disconnect) | `e2e-action-disc-iso` on both | connected on both | disconnect on a → **b's `clientId` unchanged** |
| annotations | none (`tools/list`) | — | destructive/read-only hints per tool (below) |

### Load-bearing broker facts (verified on lab-129-78, 2026-07-07)

The action tools reset **different** fields, and the SEMP client counters are
**broker-centric** — getting these wrong makes the tests assert the wrong thing:

- **`clear-queue-stats`** resets `spooledMsgCount`/`spooledByteCount` to 0, but
  the messages physically remain (`msgSpoolUsage` is unchanged). Assert
  `spooledMsgCount == 0`.
- **`delete-queue-messages`** drains the **live** depth — `liveDepth.currentMsgCount`
  (SEMPv1 `num-messages-spooled`, via `get-queue-metrics`) → 0 — while the
  **cumulative** `spooledMsgCount` stays at N (SOL-150260). Assert the live
  depth, **never** `spooledMsgCount`.
- **`clear-client-stats`** resets the data-plane counters to 0. A client that
  *receives* shows `dataTxMsgCount` rising (broker transmits **to** the client);
  `dataRxMsgCount` is client→broker and never moves for a pure receiver. Assert
  `dataTxMsgCount == 0`. (The aggregate `rxMsgCount`/`txMsgCount` never fully
  zero — control-plane chatter — so don't assert on those.)
- **`disconnect-client`** drops the session, but the messaging SDK auto-reconnects
  under the same `clientName` within ~1s, so "absent from `list-clients`" flakes.
  The reliable signal is a new broker-assigned `clientId` (or transient absence).

Because post-action reads can lag the state change on the private monitor
endpoint, every post-action assertion **polls** (`verify_monitor_object` with a
jq predicate, or `poll_queue_depth` for tool reads) rather than reading once.

### Annotations

`tools/list` advertises all four tools as write tools (`readOnlyHint=false`);
`delete-queue-messages` and `disconnect-client` carry `destructiveHint=true`, the
`clear-*-stats` pair `destructiveHint=false`. There is **no
`requiresConfirmation` annotation** — the confirmation guidance lives in each
destructive tool's description prose (asserted by SOL-148462's unit tests, not
re-asserted here).

## Note on FR-3 (cache invalidation → read-after-write)

The story's FR-3 asks for a cache-invalidation test, citing SOL-148462's DoD. The
server has **no response cache** — `get-queue-metrics` reads the broker live on
every call (verified across the codebase; the action handlers invalidate
nothing). So the "read-after-write consistency" test is what FR-3 becomes: it
proves a write's effect is visible through the monitoring tool immediately, not
that a stale cached read was invalidated (there is nothing to invalidate). See
the PR description for the DoD-discrepancy note.

## Port allocation

Distinct from the other suites so all can run concurrently:

| Resource      | e2e-action |
| ------------- | ---------- |
| SEMP broker-a | 8098       |
| SEMP broker-b | 8100       |
| SMF broker-a  | 55657      |
| SMF broker-b  | 55658      |
| MCP server    | 9092       |

All override-able via `.env`.
