# E2E Management Suite

End-to-end tests for the config/management tools — `create/update/delete-message-vpn`,
`create/update/delete-queue`, `create/update/delete-topic-endpoint`, and
`create/update/delete-rdp`. Runs two Solace brokers and exercises each tool family through
a full create → verify → update → verify → delete → verify-absent round-trip, plus
cross-broker isolation, annotation, and error-translation checks.

Builds on the shared scaffold in [`../e2e-common/`](../e2e-common/README.md).

## Quickstart

```bash
# Full cycle (recommended)
make e2e-management-all

# Or step by step
SUITE_DIR=test/e2e-management bash test/e2e-common/setup-brokers.sh
bash test/e2e-management/run-all.sh
docker compose -f test/e2e-management/docker-compose.yml down -v
```

Prerequisites: Docker + Compose, `curl`, `jq`, Go (per `go.mod`). No `broker-driver`/CGo —
config tools are SEMP-only.

## Ports (distinct from the other suites, see `.env`)

| | broker-a | broker-b |
|---|---|---|
| Container | `solace-e2e-mgmt-a` | `solace-e2e-mgmt-b` |
| SEMP port | `8094` | `8096` |

The Kafka bridge tests (below) also start one **real Kafka** container, `kafka-e2e-mgmt`
(`apache/kafka`, single-node KRaft, no host port): PLAINTEXT on `9092` and SASL_PLAINTEXT/PLAIN
on `9094` (user `kuser`; a throwaway test credential defined in `docker-compose.yml`). Both
Solace containers reach it over the compose network.

MCP server: `9090` (same default as the other suites).

## Fixture model

Per-test ownership: each test creates its own `e2e-config-*` object, acts, asserts, and
deletes it. A suite-level sweep runs on entry (pre-clean) and on exit (safety net), so a
mid-run failure never leaks state and re-runs start clean. These fixtures never touch the
monitoring suite's F1–F7.

## Kafka bridge tests (`test-kafka-tools.sh`)

Run by `run-all.sh` after the config-tool tests, against the real Kafka above. They cover
`create-/update-/delete-kafka-receiver`, `create-/delete-kafka-receiver-topic-binding`,
`create-/update-/delete-kafka-sender` and `create-/delete-kafka-sender-queue-binding`.

**Broker prerequisite.** Kafka bridges are off by default: `system_scaling_maxkafkabridgecount`
and `system_scaling_maxkafkabrokerconnectioncount` are both `0`, and with `0` every create fails
`Kafka Bridge limit of 0 reached`. `docker-compose.yml` sets them (`SYSTEM_SCALING_MAXKAFKABRIDGECOUNT=10`,
`SYSTEM_SCALING_MAXKAFKABROKERCONNECTIONCOUNT=300`). They are fixed tiers, not free integers
(bridges `0/10/50/200`, connections `0/300/2000/10000`); any other value makes the container
exit at boot.

**The secret never touches the MCP server.** In the authenticated test the tool creates the
receiver with the real scheme and the non-secret username, disabled; the test then plays the
user's out-of-band step by `PATCH`ing the username and password straight into SEMP; only then
does it call `update-kafka-receiver {"enabled": true}`. The suite also checks that the password
is not in the MCP server log and that SEMP never returns it.

Status checks poll with a bounded deadline (up to ~150 s) instead of fixed sleeps: a bridge takes
10 to 40 s to come up after it is enabled.

Bridges are cleaned up bindings-first (the broker refuses to delete a bridge that still has
bindings and does not cascade); the suite sweep finds any `e2e-config-kafka-*` remnant by prefix.

## Test catalog — tool ↔ fixture

| Test | Tools | Fixture | Round-trip assertion |
|---|---|---|---|
| VPN round-trip (a, b) | create/update/delete-message-vpn | `e2e-config-vpn-<broker>` | create → in `list-vpns`; update `maxConnectionCount` → SEMP read-back; delete → absent from `list-vpns` |
| Queue round-trip (a, b) | create/update/delete-queue | `e2e-config-queue-<broker>` (default VPN) | create → in `list-queues`; update `maxMsgSpoolUsage` → SEMP read-back; delete → absent from `list-queues` |
| Topic-endpoint round-trip (a, b) | create/update/delete-topic-endpoint | `e2e-config-te-<broker>` (default VPN) | create → SEMP read-back (no monitoring tool); update `maxSpoolUsage` → SEMP read-back; delete → SEMP 404 |
| RDP round-trip (a, b) | create/update/delete-rdp | `e2e-config-rdp-<broker>` (default VPN) | create → in `list-rdps` (disabled by default) + resolves the specific RDP via `get-rdp-status`; update `enabled:true` → `list-rdps` reads back `enabled=true`; delete → absent from `list-rdps` |
| Cross-broker isolation (queue) | create/delete-queue | `e2e-config-iso` (broker-a only) | present on broker-a, absent on broker-b (`list-queues`) |
| Cross-broker isolation (RDP) | create/delete-rdp | `e2e-config-rdp-iso` (both brokers) | created identically on both; broker-a delete leaves broker-b's copy present (`list-rdps`) |
| Annotations | `tools/list` | — | each config tool advertised; `readOnlyHint=false`; `destructiveHint` false for create-*, true for update-*/delete-* |
| Error translation | create-message-vpn | `e2e-config-vpn-broker-a` | duplicate create → `isError=false`, `outcome="already_exists"`, `changed=false` (SOL-153341: a desired-state noop, not a failure) |
| Desired-state noop (delete) | delete-message-vpn | a name never created | delete of a nonexistent VPN → `isError=false`, `outcome="already_absent"`, `changed=false` |
| Parent-missing (real error) | create-rdp | a nonexistent VPN | `isError=true`, missing-parent message names the VPN in plain language |
| Kafka annotations | `tools/list` | — | the ten Kafka bridge tools advertised, `readOnlyHint=false`; `destructiveHint=true` for create-kafka-receiver/sender (deliberately audited) and every update/delete, `false` for the two binding creates |
| Kafka secret rejection (a, b) | create/update-kafka-receiver/sender | `e2e-config-kafka-rej-*` (never created) | each of the 7 write-only secrets, as a config key and as a top-level param, on create and on update (4 shapes): every rejection is `isError=true`, names the field, says to rotate, and never echoes the value; nothing created; value absent from the MCP server log (run at debug level, with a check that the log actually contains the server's call lines) |
| Kafka enabled/scheme rules + outcomes (a, b) | create/update/delete-kafka-receiver | `e2e-config-kafka-rules-<broker>` | create enabled with a scheme rejected; update switching scheme without `enabled:false` rejected, with it accepted (and the per-scheme secret hand-off returned); update of `bootstrapAddressList`, the OAuth token endpoint, or `transportTlsEnabled:false` rejected as a retarget; delete twice → `already_absent` |
| Kafka Receiver, no authentication (a, b) | create/delete-kafka-receiver, create/delete-kafka-receiver-topic-binding, create-queue(-subscription) | `e2e-config-kafka-rx-none-<broker>`, a queue, a Kafka topic | created and enabled in one call; duplicate → `already_exists` with the scheme hint; delete with a binding refused by the broker; receiver and binding up (status); records produced to Kafka reach the queue; bindings-first teardown |
| Kafka Receiver, secret set out of band (a, b) | create/update/delete-kafka-receiver, create/delete-kafka-receiver-topic-binding | `e2e-config-kafka-rx-auth-<broker>` | created disabled with the handoff; premature enable → `Username or password missing`; secret PATCHed into SEMP; up; SEMP never returns the password; secret not in the server log |
| Kafka Sender incl. missing topic (a, b) | create/delete-kafka-sender, create/delete-kafka-sender-queue-binding | `e2e-config-kafka-tx-<broker>`, two queues, two Kafka topics (one created late) | binding to a missing topic stays down with `no leader`, and recovers by itself once the topic exists; delete with bindings refused; bindings-first teardown |

Presence/absence is verified through the monitoring tools (`list-vpns`/`list-queues`/`list-rdps`)
— the read-after-write check (there is no response cache; reads hit the broker live). Updated
attributes are verified via the monitoring tool where one exposes them (RDP `enabled` through
`list-rdps`) or via SEMP-direct monitor GET otherwise; topic-endpoints have no monitoring tool,
so all of their verification is SEMP-direct.

## Manual check against Nisshi (not run in CI)

CI uses Apache Kafka. To check the none-authentication mode against [Nisshi](https://github.com/SolaceDev/2026-hackathon6-nisshi)
by hand (heavy images; Nisshi has no authentication, so the authenticated mode cannot be checked this way):

1. Start the Nisshi stack from its `compose/` directory with its advertised address set to a name the
   broker can resolve, for example `NISSHI_ADVERTISED_HOST=nisshi-kafka NISSHI_PORT=9092`, and a
   compose override that gives the `nisshi` service that network alias (add `ports: !override []` if
   host port 9092 is taken). Attach the Solace container to the same compose network.
2. Create topics with the Kafka CLI from a container on that network (auto-creation is off in Nisshi's
   default setup too; a missing topic leaves a sender binding down with `Remote topic has no leader`).
3. Drive the tools: `create-queue`, `create-queue-subscription`, `create-kafka-receiver`
   (`authenticationScheme: none`, `enabled: true`, bootstrap `<alias>:9092`),
   `create-kafka-receiver-topic-binding` (`enabled: true`), then poll `get-kafka-receiver-status`.
   Measured 2026-10-16: receiver and binding up in about 35 s (consumer-group join is slower on Nisshi
   than on Apache Kafka), and records produced to the topic reached the queue.
