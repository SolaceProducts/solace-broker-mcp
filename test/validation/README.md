# MCP Tool Validation Suite

End-to-end validation of all 9 MCP tools against a non-trivially configured
Solace PubSub+ Enterprise broker. Each tool is exercised with realistic broker
state -- multiple VPNs, queue backlogs, disabled endpoints, live client
connections, and a REST Delivery Point with failing consumers.

## Prerequisites

| Dependency | Purpose |
|------------|---------|
| Docker | Runs the Solace broker container |
| Terraform CLI | Provisions broker objects declaratively |
| Go toolchain | Builds the MCP server binary |
| `jq` | JSON assertions in test scripts |
| `curl` | REST messaging and MCP protocol calls |
| `~/pubSubTools/sdkperf_c` | Live SMF client connections (optional -- client tests are skipped if absent) |

Access to the internal registry `docker.solacedev.ca` is required for the
Enterprise broker image.

## Quick Start

```bash
cd test/validation

# 1. Start the broker
docker compose up -d

# 2. Full run: setup -> validate -> cleanup
./run.sh
```

That's it. The orchestrator waits for the broker, provisions everything,
publishes messages, connects clients, builds and starts the MCP server,
runs all 11 tests, then tears everything down.

## Phases

`run.sh` accepts an optional mode argument to run individual phases:

```bash
./run.sh setup      # Provision broker + publish messages + start clients
./run.sh validate   # Build MCP server + run tests (assumes setup done)
./run.sh cleanup    # Tear down Terraform resources + stop clients + stop server
./run.sh            # All three in sequence (cleanup runs on exit via trap)
```

Running phases separately is useful for iterating on tests without
re-provisioning the broker each time.

## What Gets Provisioned

### Terraform (`terraform/main.tf`)

| Resource | Count | Notes |
|----------|-------|-------|
| Message VPNs | 3 | `default` (enabled, REST on port 9000), `val-vpn-active` (enabled), `val-vpn-disabled` (disabled) |
| Client Usernames | 1 | `val-user` in default VPN |
| Queues (default VPN) | 8 | Healthy, backlog, large backlog, egress-down, exclusive, with-consumer, 2 RDP queues |
| Queues (val-vpn-active) | 3 | Orders, invoices, notifications |
| Queue Subscriptions | 3 | Topic subscriptions on backlog, large-backlog, and egress-down queues |
| REST Delivery Point | 1 | `val-rdp` -- enabled but operationally down |
| RDP Queue Bindings | 2 | Events and alerts queues |
| RDP REST Consumers | 2 | Primary (enabled, unreachable), secondary (disabled) |

### Runtime (post-Terraform)

| Action | Purpose |
|--------|---------|
| Publish 5 messages to `backlog/>` | Small queue backlog for list-queues |
| Publish 50 messages to `load/>` | Large backlog for get-queue-metrics |
| Publish 10 messages to `stuck/>` | Messages on egress-down queue |
| sdkperf topic subscriber 1 | 3 topic subscriptions (`sensor/temperature/>`, `sensor/humidity/>`, `alerts/critical`) |
| sdkperf topic subscriber 2 | 3 topic subscriptions (`orders/new`, `orders/cancel`, `orders/status`) |
| sdkperf queue consumer | Bound to `val-q-with-consumer` |

## Test Coverage

| Test | Tool | What It Validates |
|------|------|-------------------|
| 1 | `list-vpns` | Returns all 3 VPNs, identifies disabled VPN |
| 2a | `get-vpn-health` | Default VPN health, REST service enabled |
| 2b | `get-vpn-health` | Disabled VPN shows `enabled: false` |
| 2c | `get-vpn-health` | Active VPN with queues but no clients |
| 3 | `list-queues` | 8+ queues, backlog detection, egress-down state, exclusive access type |
| 4 | `get-queue-metrics` | 50+ spooled messages, spool usage > 0, bind count = 0 |
| 5 | `list-clients` | Discovers connected sdkperf clients |
| 6 | `get-client-details` | Fetches details for a specific client (URL-encodes `/` in names) |
| 7 | `list-client-subscriptions` | Lists topic subscriptions for a connected client |
| 8 | `get-rdp-status` | 3-step query: RDP status + queue bindings + REST consumers; enabled-but-down scenario |
| 9 | `get_redundancy_status` | SEMPv1 XML-to-JSON path; standalone broker returns valid redundancy data |

## Configuration

All settings live in `.env`:

```
BROKER_SEMP_PORT=8080      # SEMP management API
BROKER_SMF_PORT=55555      # SMF messaging (sdkperf clients)
BROKER_REST_PORT=9000      # REST messaging (publish via curl)
BROKER_USERNAME=admin       # SEMP admin credentials
BROKER_PASSWORD=admin
BROKER_VPN=default
VAL_USERNAME=val-user       # Messaging client credentials
VAL_PASSWORD=valpass
SDKPERF=~/pubSubTools/sdkperf_c
```

The MCP server starts on port 9091 (configured in `helpers.sh`) to avoid
conflicts with other services.

## File Layout

```
test/validation/
  .env                  # Ports, credentials, paths
  docker-compose.yml    # Solace Enterprise broker container
  run.sh                # Orchestrator (setup/validate/cleanup)
  validate.sh           # 11 test functions for 9 MCP tools
  helpers.sh            # Broker readiness, MCP protocol, assertions, sdkperf
  terraform/
    main.tf             # All broker objects
    variables.tf        # Input variable definitions
    terraform.tfvars    # Localhost defaults
```

## Troubleshooting

**Broker not starting:** Check Docker has at least 2 GB shared memory
(`shm_size: 2g` in docker-compose.yml). Run `docker logs solace-validation`
for broker startup logs.

**REST publish returns 403:** The default VPN must have
`authentication_basic_type = "internal"` (set in main.tf). If the broker was
previously configured with RADIUS auth, run `./run.sh cleanup` then
`./run.sh setup` to re-apply.

**MCP server port conflict:** The server runs on port 9091. If something else
occupies that port, change `MCP_PORT` in `helpers.sh`.

**sdkperf not found:** Client connection tests (topic subscribers, queue
consumer) are skipped gracefully. Set the `SDKPERF` path in `.env` if
sdkperf_c is installed elsewhere.

**Terraform state drift:** If a previous run was interrupted, run
`./run.sh cleanup` to destroy state, or `cd terraform && terraform destroy
-auto-approve` directly.
