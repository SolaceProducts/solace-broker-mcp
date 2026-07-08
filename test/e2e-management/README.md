# E2E Management Suite

End-to-end tests for the config/management tools — `create/update/delete-message-vpn`,
`create/update/delete-queue`, and `create/update/delete-topic-endpoint`. Runs two Solace
brokers and exercises each tool family through a full create → verify → update → verify →
delete → verify-absent round-trip, plus cross-broker isolation, annotation, and
error-translation checks.

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

MCP server: `9090` (same default as the other suites).

## Fixture model

Per-test ownership: each test creates its own `e2e-config-*` object, acts, asserts, and
deletes it. A suite-level sweep runs on entry (pre-clean) and on exit (safety net), so a
mid-run failure never leaks state and re-runs start clean. These fixtures never touch the
monitoring suite's F1–F7.

## Test catalog — tool ↔ fixture

| Test | Tools | Fixture | Round-trip assertion |
|---|---|---|---|
| VPN round-trip (a, b) | create/update/delete-message-vpn | `e2e-config-vpn-<broker>` | create → in `list-vpns`; update `maxConnectionCount` → SEMP read-back; delete → absent from `list-vpns` |
| Queue round-trip (a, b) | create/update/delete-queue | `e2e-config-queue-<broker>` (default VPN) | create → in `list-queues`; update `maxMsgSpoolUsage` → SEMP read-back; delete → absent from `list-queues` |
| Topic-endpoint round-trip (a, b) | create/update/delete-topic-endpoint | `e2e-config-te-<broker>` (default VPN) | create → SEMP read-back (no monitoring tool); update `maxSpoolUsage` → SEMP read-back; delete → SEMP 404 |
| Cross-broker isolation | create/delete-queue | `e2e-config-iso` (broker-a only) | present on broker-a `list-queues`, absent on broker-b |
| Annotations | `tools/list` | — | each config tool advertised; `readOnlyHint=false`; `destructiveHint` false for create-*, true for update-*/delete-* |
| Error translation | create-message-vpn | `e2e-config-vpn-broker-a` | duplicate create → `isError=true`, HTTP 400, "already exists" surfaced through the wire |

Presence/absence is verified through the monitoring tools (`list-vpns`/`list-queues`) — the
read-after-write check (there is no response cache; reads hit the broker live). Updated
attributes and topic-endpoints (no monitoring tool) are verified via SEMP-direct monitor GET.
