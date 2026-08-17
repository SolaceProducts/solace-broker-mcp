# E2E Test Scaffold

Shared infrastructure for the E2E test suites (`e2e-monitoring`, `e2e-management`, and future suites). Each suite provisions its own brokers and runs its own tests; this directory holds the common pieces.

## What's here

| File | Purpose |
|------|---------|
| `lib.sh` | Shared bash library: broker readiness, MCP server lifecycle, config generation, SEMP operations, MCP JSON-RPC wire helpers, assertions, test runner |
| `lib.sh::_lib_write_config()` / `write_config()` | `_lib_write_config` emits the base two-broker config (`broker-a`, `broker-b`). `write_config` is the public entry point and defaults to calling `_lib_write_config`; a suite's `helpers.sh` can override it to append suite-local aliases (bash later-wins) — see `e2e-basic-mcp/helpers.sh` for an example |
| `lib.sh::MCP_PROTOCOL_VERSION` | The `protocolVersion` every suite pins in its `initialize` handshake (default `2025-11-25`), overridable from the environment. `mcp_initialize` fails if the server negotiates something else — see the comment at the definition for why the pin is not go-sdk's latest revision |
| `setup-brokers.sh` | Bring a suite's brokers up and wait for readiness |
| `start-server.sh` | Build the MCP server and start it against a suite's brokers |

## Quickstart pattern

All suites follow the same structure:

```bash
# Full cycle (recommended)
make e2e-<suite>-all

# Or step by step
SUITE_DIR=test/e2e-<suite> bash test/e2e-common/setup-brokers.sh
bash test/e2e-<suite>/run-all.sh
docker compose -f test/e2e-<suite>/docker-compose.yml down -v
```

During development, repeat `run-all.sh` without restarting brokers. Each suite cleans up on exit (fixtures, MCP server) so re-runs start clean.

## Prerequisites

- Docker + Compose
- `curl`, `jq`
- Go (per `go.mod`)

The `e2e-monitoring` suite additionally requires CGo for the `broker-driver` binary — see its README.

## Suite layout

Each suite directory contains:

| File | Purpose |
|------|---------|
| `.env` | Ports, credentials (single source of truth) |
| `docker-compose.yml` | Broker containers |
| `helpers.sh` | Suite-specific fixtures; sources `lib.sh` |
| `run-all.sh` | Orchestrator (start server, run tests, cleanup) |
| `test-<suite>-tools.sh` | Actual MCP tool tests |

## Port allocation

Each suite uses distinct ports so they can run independently:

| Suite | SEMP broker-a | SEMP broker-b | Containers |
|-------|---------------|---------------|------------|
| `e2e-basic-mcp` | 8080 | 8082 | `solace-e2e-a/b` |
| `e2e-monitoring` | 8090 | 8092 | `solace-e2e-mon-a/b` |
| `e2e-management` | 8094 | 8096 | `solace-e2e-mgmt-a/b` |

MCP server: `9090` (shared default, suites run sequentially).

Override via each suite's `.env`.
