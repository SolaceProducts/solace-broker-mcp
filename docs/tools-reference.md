# Tools Reference

Complete reference for every tool the Solace Event Broker MCP Server exposes:
parameters, output shape, an example invocation, and an example natural-language
request. For task-oriented walkthroughs see [Examples](examples.md); for a
narrative overview see the [User Guide](user-guide.md).

The server exposes **17 read-only tools** plus **13 write tools** — 4 action
tools and 9 Config-API management tools. The write tools are gated behind
`enable_write_tools` (off by default) and are not registered with the MCP server
when disabled — see
[Action tools and `enable_write_tools`](#action-tools-and-enable_write_tools).

## Conventions

These apply to every tool unless noted otherwise.

### The `broker` parameter

Every tool **except `list-brokers`** takes a required `broker` parameter
identifying which configured broker to query. It is injected automatically into
each tool's input schema at registration (`injectBrokerParam` in
`internal/tools/register.go`), so it is not declared in any tool definition:

```json
{ "type": "string", "description": "Target broker alias (required). Available brokers: <alias>, <alias>, ..." }
```

The accepted values are the broker aliases from your config. Call `list-brokers`
to discover them. Alias matching is case-insensitive; original casing is
preserved in output.

### Pagination

List tools accept an optional `maxResults` integer: **default 100, maximum 500**
(`defaultMax`/`capMax` in the composite executor). The server pages through the
broker's SEMP responses internally and returns up to `maxResults` items. Brokers
with more than 500 matching objects require a narrower query. Applies to:
`list-vpns`, `list-queues`, `list-clients`, `list-client-subscriptions`,
`list-slow-subscribers`, `list-rdps`, `list-queue-discards`.

### Rate limiting and retries

Requests to each broker are subject to per-broker concurrency limits and an
automatic retry policy with backoff. Transient upstream failures (HTTP 429 from a
proxy/gateway, 503 from an overloaded broker) are retried automatically and
reported as `retryable: true` if retries are exhausted. Tune these in
[Configuration](configuration.md).

### Output: the step-keyed envelope

Most read-only tools return their broker data in a **step-keyed envelope** — a
top-level JSON object whose keys are the tool's internal step IDs and whose values
are the SEMP response objects for that step (schema:
`{"type":"object","additionalProperties":{"type":"object"}}`). Single-step tools
return one key; multi-step tools (for example `get-rdp-status`) return one key per
step. The broker may omit any optional field, so the envelope schema does not
enumerate inner fields — the authoritative field list per tool is the `select`
set documented under each tool's **Returns**.

Five tools depart from the generic envelope with a strict, field-level output
schema: `get-discard-stats` and the four action tools (documented inline below).

### Errors

On failure a tool returns a structured error object. Full field reference and
common HTTP-status causes (401/403/404/429/503) are in the
[User Guide → Tool Returns an Error](user-guide.md#tool-returns-an-error). Key
fields: `error` (message), `retryable` (bool), `status` (HTTP code), plus
source-specific fields (`operation`, `sempStatus`, `sempCode` for SEMPv2; `kind`,
`reasonCode` for SEMPv1) and `suggestions`.

### Action tools and `enable_write_tools`

The four action tools (`disconnect-client`, `clear-client-stats`,
`delete-queue-messages`, `clear-queue-stats`) change broker state. They are gated
behind the server-level `enable_write_tools` flag:

- **Default (`false`):** not registered; absent from `tools/list`; an
  authenticated client still cannot invoke them.
- **`enable_write_tools: true`:** registered and callable.

This is independent of `mcp_client_auth.mode`. The two **destructive** tools
(`delete-queue-messages`, `disconnect-client`) carry the MCP `destructiveHint`
annotation, instruct the calling LLM to obtain explicit user confirmation before
invocation, and cause the server to log a WARNING audit line on every call. The
two `clear-*-stats` tools are writes but non-destructive (counters only).

The nine Config-API management tools (create/update/delete for Message VPNs,
queues, and topic endpoints) are gated behind the same flag and documented under
[Management](#management-config-api).

## Tool index

| Category | Tool | Write? |
|---|---|---|
| Discovery | [`list-brokers`](#list-brokers) | — |
| Broker Status | [`get-broker-status`](#get-broker-status), [`get-redundancy-status`](#get-redundancy-status) | — |
| Replication | [`get-replication-status`](#get-replication-status) | — |
| Message VPN | [`list-vpns`](#list-vpns), [`get-vpn-status`](#get-vpn-status), [`get-message-rates`](#get-message-rates) | — |
| Queues | [`list-queues`](#list-queues), [`get-queue-metrics`](#get-queue-metrics) | — |
| Clients | [`list-clients`](#list-clients), [`get-client-details`](#get-client-details), [`list-client-subscriptions`](#list-client-subscriptions), [`list-slow-subscribers`](#list-slow-subscribers) | — |
| REST Delivery Points | [`list-rdps`](#list-rdps), [`get-rdp-status`](#get-rdp-status) | — |
| Discards | [`get-discard-stats`](#get-discard-stats), [`list-queue-discards`](#list-queue-discards) | — |
| Actions | [`disconnect-client`](#disconnect-client), [`clear-client-stats`](#clear-client-stats), [`delete-queue-messages`](#delete-queue-messages), [`clear-queue-stats`](#clear-queue-stats) | write |
| Management | [`create-message-vpn`](#create-message-vpn), [`update-message-vpn`](#update-message-vpn), [`delete-message-vpn`](#delete-message-vpn), [`create-queue`](#create-queue), [`update-queue`](#update-queue), [`delete-queue`](#delete-queue), [`create-topic-endpoint`](#create-topic-endpoint), [`update-topic-endpoint`](#update-topic-endpoint), [`delete-topic-endpoint`](#delete-topic-endpoint) | write |

Example invocations below show the `arguments` object of an MCP `tools/call`
request. A full request wraps it: `{"method":"tools/call","params":{"name":"<tool>","arguments":{...}}}`.

---

## Discovery

### list-brokers

List all configured broker aliases. Use one of the returned names as the
`broker` parameter on any other tool. This is the only tool with **no `broker`
parameter**.

**Parameters:** none.

**Returns:** a strict object `{ "brokers": ["alias1", "alias2", ...] }`
(`brokers` is a required array of strings).

```json
{}
```

```json
{ "brokers": ["prod-broker", "dev-broker"] }
```

**Example request:** "What brokers are configured?"

---

## Broker Status

### get-broker-status

Curated point-in-time status snapshot of a broker: edition and version, uptime
and restart reason, scaling limits and resource headroom, memory and
message-spool utilization, and — on hardware appliances — chassis identity and
physical-component inventory (CPU, memory, power, disks, blades). Reports raw
state, not a health verdict.

**Parameters:**

| Name | Type | Required | Description |
|---|---|---|---|
| `broker` | string | yes | Target broker alias. |

**Returns:** step-keyed envelope (native SEMPv1). Inner fields cover version,
uptime/restart, scaling and resource utilization, spool state, and HA roles;
appliances add a `hardwareDetails` section. Field shape is documented in
`docs/internal/semp/get-broker-status-curated-fields.md`.

```json
{ "broker": "prod-broker" }
```

**Example request:** "What's prod-broker's current status? When did it last restart?"

### get-redundancy-status

Broker redundancy and high-availability status: config/operational status,
active-standby role, mate router name, mate link state, and per-virtual-router
activity. Single SEMPv1 call.

**Parameters:**

| Name | Type | Required | Description |
|---|---|---|---|
| `broker` | string | yes | Target broker alias. |

**Returns:** step-keyed envelope (native SEMPv1) with redundancy/HA fields.

```json
{ "broker": "prod-broker" }
```

**Example request:** "What's the HA status on prod-broker — which node is active?"

---

## Replication

### get-replication-status

Replication state for a Message VPN: role, sync eligibility, bridge status,
transaction mode, and queued-message counts. SEMP does not expose current
`lagSeconds`; `replicationActiveTransitionToSyncIneligibleCount` is the most
useful "has replication been flaky recently?" signal.

**Parameters:**

| Name | Type | Required | Description |
|---|---|---|---|
| `broker` | string | yes | Target broker alias. |
| `msgVpnName` | string | yes | The Message VPN. |

**Returns:** step-keyed envelope, step `replication`. Selected fields:
`replicationRole`, `replicationEnabled`, `replicationSyncEligible`,
`replicationBridgeUp`, `replicationRemoteBridgeUp`, `replicationQueueBound`,
`replicationTransactionMode`, `replicationActiveAsyncQueuedMsgCount`,
`replicationActiveSyncQueuedMsgCount`,
`replicationActiveTransitionToSyncIneligibleCount`, and related counters.

```json
{ "broker": "prod-broker", "msgVpnName": "default" }
```

**Example request:** "Is replication in sync for the default VPN on prod-broker?"

---

## Message VPN

### list-vpns

List Message VPNs with enabled state, connection count, and basic status.

**Parameters:**

| Name | Type | Required | Description |
|---|---|---|---|
| `broker` | string | yes | Target broker alias. |
| `maxResults` | integer | no | Max VPNs to return (default 100, max 500). |

**Returns:** step-keyed envelope, step `vpns` (array). Selected fields per VPN:
`msgVpnName`, `enabled`, `state`, `msgVpnConnections`,
`msgVpnTotalUniqueSubscriptions`, `msgSpoolUsage`, `msgSpoolMsgCount`,
`replicationEnabled`, `dmrEnabled`, per-service up/failure fields (SMF, MQTT,
REST, Web), and discard counts.

```json
{ "broker": "prod-broker", "maxResults": 50 }
```

**Example request:** "List the VPNs on prod-broker and flag any that are down."

### get-vpn-status

Operational status and connection statistics for one VPN: enabled state,
active connection count, total subscription count, and service states for
AMQP, MQTT, REST, and SMF (plaintext and TLS variants where applicable).
Reports raw state, not a health verdict.

**Parameters:**

| Name | Type | Required | Description |
|---|---|---|---|
| `broker` | string | yes | Target broker alias. |
| `msgVpnName` | string | yes | The Message VPN. |

**Returns:** step-keyed envelope, step `vpnStatus`. Selected fields: `msgVpnName`,
`enabled`, `state`, `msgVpnConnections`, `maxConnectionCount`,
`msgVpnTotalUniqueSubscriptions`, per-service up/failure fields, spool usage, and
discard counts.

```json
{ "broker": "prod-broker", "msgVpnName": "default" }
```

**Example request:** "Is the default VPN on prod-broker operational?"

### get-message-rates

Current and average message and byte throughput rates for a VPN.

**Parameters:**

| Name | Type | Required | Description |
|---|---|---|---|
| `broker` | string | yes | Target broker alias. |
| `msgVpnName` | string | yes | The Message VPN. |

**Returns:** step-keyed envelope, step `rates`. Selected fields: `msgVpnName`,
`rxMsgRate`, `txMsgRate`, `rxByteRate`, `txByteRate`, and the `average*`
equivalents.

```json
{ "broker": "prod-broker", "msgVpnName": "default" }
```

**Example request:** "What are the current message rates on the default VPN?"

---

## Queues

### list-queues

List queues in a VPN with depth, unacked count, bind count, congestion state, and
rates. Primary VPN-wide scan for slow guaranteed-message consumers (growing
`spooledMsgCount`, high `txUnackedMsgCount`, `rxMsgRate > txMsgRate`,
`bindCount > 0`).

**Parameters:**

| Name | Type | Required | Description |
|---|---|---|---|
| `broker` | string | yes | Target broker alias. |
| `msgVpnName` | string | yes | The Message VPN. |
| `maxResults` | integer | no | Max queues to return (default 100, max 500). |

**Returns:** step-keyed envelope, step `queues` (array). Selected fields per queue:
`queueName`, `accessType`, `spooledMsgCount`, `txUnackedMsgCount`, `bindCount`,
`rxMsgRate`, `txMsgRate`, `msgSpoolUsage`, `maxMsgSpoolUsage`,
`lowPriorityMsgCongestionState`, `ingressEnabled`, `egressEnabled`.

```json
{ "broker": "prod-broker", "msgVpnName": "default", "maxResults": 100 }
```

**Example request:** "Show queues with a backlog on the default VPN."

### get-queue-metrics

Detailed metrics for one queue: spool, unacked, bind count, rates, redelivery
counts, congestion state, and spool usage. The right starting point for diagnosing
a slow guaranteed-message consumer (the per-client `slowSubscriber` field does not
flip for slow ACKs).

**Parameters:**

| Name | Type | Required | Description |
|---|---|---|---|
| `broker` | string | yes | Target broker alias. |
| `msgVpnName` | string | yes | The Message VPN. |
| `queueName` | string | yes | The queue name. |

**Returns:** step-keyed envelope, step `queueMetrics`. Selected fields include
`queueName`, `spooledMsgCount`, `txUnackedMsgCount`, `bindCount`, `maxBindCount`,
`rxMsgRate`, `txMsgRate`, `rxByteRate`, `txByteRate`, `redeliveredMsgCount`,
`maxDeliveredUnackedMsgsPerFlow`, `msgSpoolUsage`, `maxMsgSpoolUsage`,
`lowPriorityMsgCongestionState`, plus per-category discard counters and config
(`accessType`, `durable`, `owner`, `maxTtl`, `maxRedeliveryCount`, `maxMsgSize`).

```json
{ "broker": "prod-broker", "msgVpnName": "default", "queueName": "orders.q" }
```

**Example request:** "Why is orders.q backing up on prod-broker?"

---

## Clients

### list-clients

List active clients in a VPN with rates, uptime, and the `slowSubscriber` field.
Note: `slowSubscriber` flags TCP-egress stalls (mainly direct messaging); it does
not flip for slow guaranteed consumers — use `list-queues` for those.

**Parameters:**

| Name | Type | Required | Description |
|---|---|---|---|
| `broker` | string | yes | Target broker alias. |
| `msgVpnName` | string | yes | The Message VPN. |
| `maxResults` | integer | no | Max clients to return (default 100, max 500). |

**Returns:** step-keyed envelope, step `clients` (array). Selected fields per
client: `clientName`, `clientUsername`, `clientAddress`, `platform`, `rxMsgRate`,
`txMsgRate`, `slowSubscriber`, `uptime`.

```json
{ "broker": "prod-broker", "msgVpnName": "default", "maxResults": 100 }
```

**Example request:** "List the connected clients on the default VPN."

### get-client-details

Per-client performance metrics including `slowSubscriber`. `slowSubscriber=false`
does not rule out a slow consumer — for slow guaranteed-message consumers use
`get-queue-metrics`.

**Parameters:**

| Name | Type | Required | Description |
|---|---|---|---|
| `broker` | string | yes | Target broker alias. |
| `msgVpnName` | string | yes | The Message VPN the client is connected to. |
| `clientName` | string | yes | The client connection name. |

**Returns:** step-keyed envelope, step `clientDetails`. Selected fields include
`clientName`, `clientUsername`, `clientAddress`, `platform`, `softwareVersion`,
`rxMsgRate`/`txMsgRate`, `rxByteRate`/`txByteRate`, `averageRxMsgRate`/
`averageTxMsgRate`, `rxMsgCount`/`txMsgCount`, `rxDiscardedMsgCount`/
`txDiscardedMsgCount`, `slowSubscriber`, `subscriptionCount`, `rxFlowCount`/
`txFlowCount`, `elidingTopicCount`, `keepalive`, `uptime`.

```json
{ "broker": "prod-broker", "msgVpnName": "default", "clientName": "consumer-7" }
```

**Example request:** "Get details for client consumer-7 on the default VPN."

### list-client-subscriptions

List topic subscriptions for a specific client.

**Parameters:**

| Name | Type | Required | Description |
|---|---|---|---|
| `broker` | string | yes | Target broker alias. |
| `msgVpnName` | string | yes | The Message VPN the client is connected to. |
| `clientName` | string | yes | The client connection name. |
| `maxResults` | integer | no | Max subscriptions to return (default 100, max 500). |

**Returns:** step-keyed envelope, step `subscriptions` (array of subscription
records as returned by the broker).

```json
{ "broker": "prod-broker", "msgVpnName": "default", "clientName": "consumer-7" }
```

**Example request:** "What topics is consumer-7 subscribed to?"

### list-slow-subscribers

Clients in a VPN flagged with the broker's `slowSubscriber` field
(server-side `where: slowSubscriber==true`). Narrow signal — catches direct-
messaging/replication-bridge backpressure; does **not** flip for slow guaranteed
consumers. For those, use `list-queues` / `get-queue-metrics`.

**Parameters:**

| Name | Type | Required | Description |
|---|---|---|---|
| `broker` | string | yes | Target broker alias. |
| `msgVpnName` | string | yes | The Message VPN to search. |
| `maxResults` | integer | no | Max results to return (default 100, max 500). |

**Returns:** step-keyed envelope, step `slowSubscribers` (array). Selected fields
per client: `clientName`, `clientUsername`, `clientAddress`, `platform`,
`rxMsgRate`, `txMsgRate`, `txDiscardedMsgCount`, `slowSubscriber`, `uptime`.

```json
{ "broker": "prod-broker", "msgVpnName": "default" }
```

**Example request:** "Are there any slow subscribers on the default VPN?"

---

## REST Delivery Points

### list-rdps

List all REST Delivery Points in a VPN with enabled state, up/down status, and
last failure reason. For full detail use `get-rdp-status`.

**Parameters:**

| Name | Type | Required | Description |
|---|---|---|---|
| `broker` | string | yes | Target broker alias. |
| `msgVpnName` | string | yes | The Message VPN. |
| `maxResults` | integer | no | Max RDPs to return (default 100, max 500). |

**Returns:** step-keyed envelope, step `rdps` (array). Selected fields per RDP:
`restDeliveryPointName`, `enabled`, `up`, `clientName`, `lastFailureReason`,
`lastFailureTime`.

```json
{ "broker": "prod-broker", "msgVpnName": "default" }
```

**Example request:** "List the RDPs on the default VPN and flag any that are down."

### get-rdp-status

Detailed RDP status across three parallel SEMP calls: the RDP itself, its queue
bindings, and its REST consumers.

**Parameters:**

| Name | Type | Required | Description |
|---|---|---|---|
| `broker` | string | yes | Target broker alias. |
| `msgVpnName` | string | yes | The Message VPN containing the RDP. |
| `restDeliveryPointName` | string | yes | The RDP name. |

**Returns:** step-keyed envelope with three keys:
- `rdpStatus` — `enabled`, `up`, `clientName`, `clientProfileName`,
  `lastFailureReason`, `lastFailureTime`, `timeConnectionsBlocked`.
- `queueBindings` — per binding: `queueBindingName`, `postRequestTarget`, `up`,
  `uptime`, `lastFailureReason`, `lastFailureTime`.
- `restConsumers` — per consumer: `restConsumerName`, `enabled`, `up`,
  `remoteHost`, `remotePort`, `authenticationScheme`, HTTP request/response
  counters, and last-failure fields.

```json
{ "broker": "prod-broker", "msgVpnName": "default", "restDeliveryPointName": "webhook-rdp" }
```

**Example request:** "Why is the webhook-rdp on the default VPN failing?"

---

## Discards

### get-discard-stats

Pre-aggregated message discard counters. **Without `vpnName`:** broker-wide totals
(client-level ingress/egress discards plus broker-wide spool-level discards).
**With `vpnName`:** client-level discards scoped to that VPN only — SEMPv1 exposes
no per-VPN spool breakdown (use `list-queue-discards` for per-queue spool
discards). Note the parameter is `vpnName`, not `msgVpnName`.

**Parameters:**

| Name | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `broker` | string | yes | — | Target broker alias. |
| `vpnName` | string | no | `minLength: 1` | Scope to a single VPN (client-level only). Omit for broker-wide totals. Empty string is rejected. |

**Returns:** strict object (field-level schema):

```json
{
  "type": "object",
  "properties": {
    "vpnName":        { "type": "string" },
    "clientDiscards": { "type": "object" },
    "spoolDiscards":  { "type": "object" }
  },
  "required": ["clientDiscards"],
  "additionalProperties": false
}
```

`clientDiscards` is always present; `spoolDiscards` appears only for broker-wide
scope; `vpnName` echoes the requested VPN when scoped.

```json
{ "broker": "prod-broker" }
```

```json
{ "broker": "prod-broker", "vpnName": "default" }
```

**Example request:** "Are we dropping messages anywhere on prod-broker?"

### list-queue-discards

Per-queue message discard counts for a VPN: TTL-expired, max-redelivery-exceeded,
spool-quota-exceeded, and other categories. For broker/VPN aggregates use
`get-discard-stats`.

**Parameters:**

| Name | Type | Required | Description |
|---|---|---|---|
| `broker` | string | yes | Target broker alias. |
| `msgVpnName` | string | yes | The Message VPN. |
| `maxResults` | integer | no | Max queues to return (default 100, max 500). |

**Returns:** step-keyed envelope, step `queueDiscards` (array). Selected fields per
queue: `queueName`, `maxTtlExpiredDiscardedMsgCount`,
`maxRedeliveryExceededDiscardedMsgCount`,
`maxMsgSpoolUsageExceededDiscardedMsgCount`, `maxMsgSizeExceededDiscardedMsgCount`,
`lowPriorityMsgCongestionDiscardedMsgCount`, `disabledDiscardedMsgCount`,
`noLocalDeliveryDiscardedMsgCount`, `clientProfileDeniedDiscardedMsgCount`,
`destinationGroupErrorDiscardedMsgCount`, DMQ counters, and
`xaTransactionNotSupportedDiscardedMsgCount`.

```json
{ "broker": "prod-broker", "msgVpnName": "default" }
```

**Example request:** "Which queues on the default VPN are discarding messages, and why?"

---

## Action Tools

Write tools, gated behind `enable_write_tools` (off by default). See
[Action tools and `enable_write_tools`](#action-tools-and-enable_write_tools).
All four return the same strict success envelope on completion.

### disconnect-client

**Destructive.** Forcibly disconnect a connected client — service-impacting; the
client must reconnect. The description instructs the LLM to obtain explicit user
confirmation (restating broker, VPN, and client) as a separate reply before
invoking. The server logs a WARNING audit line on every call.

Annotations: `readOnly: false`, `destructiveHint: true`, `idempotentHint: false`.

**Parameters:**

| Name | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `broker` | string | yes | — | Target broker alias. |
| `msgVpnName` | string | yes | `minLength: 1` | The Message VPN the client is connected to. |
| `clientName` | string | yes | `minLength: 1` | The client connection to disconnect. |

**Returns:** strict object `{ status, msgVpnName, clientName }`, `status` is the
enum `"ok"`, `additionalProperties: false`.

```json
{ "broker": "prod-broker", "msgVpnName": "default", "clientName": "consumer-7" }
```

```json
{ "status": "ok", "msgVpnName": "default", "clientName": "consumer-7" }
```

**Example request:** "Disconnect consumer-7 on the default VPN." (The agent will
ask you to confirm before acting.)

### clear-client-stats

Write, non-destructive. Reset a client's per-connection statistics counters. Does
not disconnect the client or affect delivery.

Annotations: `readOnly: false`, `destructiveHint: false`, `idempotentHint: true`.

**Parameters:** same as `disconnect-client` (`broker`, `msgVpnName`, `clientName`).

**Returns:** `{ status: "ok", msgVpnName, clientName }`.

```json
{ "broker": "prod-broker", "msgVpnName": "default", "clientName": "consumer-7" }
```

**Example request:** "Reset the stats counters for consumer-7 on the default VPN."

### delete-queue-messages

**Destructive.** Permanently delete ALL spooled messages from a queue —
irreversible. The description instructs the LLM to obtain explicit user
confirmation (restating broker, VPN, and queue) as a separate reply before
invoking. The server logs a WARNING audit line on every call.

Annotations: `readOnly: false`, `destructiveHint: true`, `idempotentHint: false`.

**Parameters:**

| Name | Type | Required | Constraints | Description |
|---|---|---|---|---|
| `broker` | string | yes | — | Target broker alias. |
| `msgVpnName` | string | yes | `minLength: 1` | The Message VPN containing the queue. |
| `queueName` | string | yes | `minLength: 1` | The queue to drain. |

**Returns:** strict object `{ status, msgVpnName, queueName }`, `status` is the
enum `"ok"`, `additionalProperties: false`.

```json
{ "broker": "prod-broker", "msgVpnName": "default", "queueName": "dead-letter.q" }
```

```json
{ "status": "ok", "msgVpnName": "default", "queueName": "dead-letter.q" }
```

**Example request:** "Drain dead-letter.q on the default VPN." (The agent will ask
you to confirm before acting.)

### clear-queue-stats

Write, non-destructive. Reset a queue's statistics counters. Does not affect
spooled messages or delivery.

Annotations: `readOnly: false`, `destructiveHint: false`, `idempotentHint: true`.

**Parameters:** same as `delete-queue-messages` (`broker`, `msgVpnName`,
`queueName`).

**Returns:** `{ status: "ok", msgVpnName, queueName }`.

```json
{ "broker": "prod-broker", "msgVpnName": "default", "queueName": "orders.q" }
```

**Example request:** "Reset the stats counters for orders.q on the default VPN."

---

## Management (Config API)

Create, update, and delete SEMPv2 **config** objects — Message VPNs, queues, and
topic endpoints. Gated behind `enable_write_tools` (off by default), the same as
the action tools above.

- `create-*` is **additive** (not destructive); config attributes you omit take
  the broker default.
- `update-*` applies a **partial (PATCH)** update — only the attributes you
  supply change — and is marked **destructive** because it can be
  service-affecting (for example, disabling a VPN drops its client connections).
- `delete-*` removes the object (and any messages spooled on a queue or topic
  endpoint) and is **destructive**.

Create and update tools take a config object (`msgVpnConfig`, `queueConfig`,
`topicEndpointConfig`) whose attributes are spread into the SEMPv2 request body.
Do **not** put the object's own name (`msgVpnName`, `queueName`,
`topicEndpointName`) inside the config object — the name comes from its dedicated
parameter. A reserved name, or any attribute the object's schema doesn't define,
placed inside the config object is rejected before the broker call rather than
sent on. Every management tool's description instructs the LLM to obtain explicit
user confirmation — restating the target and effect — as a separate reply before
invoking.

All management tools return a step-keyed envelope whose single key maps to the
SEMPv2 response: the created or updated object for `create-*`/`update-*`, and an
empty object for `delete-*`.

### create-message-vpn

Create a Message VPN. Fails if one with the same name already exists.

Annotations: `readOnly: false`, `destructive: false`.

| Name | Type | Required | Description |
|---|---|---|---|
| `broker` | string | yes | Target broker alias. |
| `msgVpnName` | string | yes | Name of the VPN to create. |
| `msgVpnConfig` | object | no | MsgVpn attributes (e.g. `enabled`, `maxConnectionCount`, `maxMsgSpoolUsage`). Omitted attributes take broker defaults. |

**Returns:** step-keyed envelope, step `createVpn`.

```json
{ "broker": "prod-broker", "msgVpnName": "orders-vpn", "msgVpnConfig": { "enabled": true, "maxConnectionCount": 100 } }
```

**Example request:** "Create a VPN called orders-vpn on prod-broker, enabled, with 100 max connections." (The agent will ask you to confirm before acting.)

### update-message-vpn

**Destructive.** Partially update a Message VPN — only the supplied attributes
change. Service-affecting: disabling a VPN drops its client connections.

Annotations: `readOnly: false`, `destructive: true`.

| Name | Type | Required | Description |
|---|---|---|---|
| `broker` | string | yes | Target broker alias. |
| `msgVpnName` | string | yes | The VPN to modify. |
| `msgVpnConfig` | object | yes | MsgVpn attributes to change. Do not include `msgVpnName`. |

**Returns:** step-keyed envelope, step `updateVpn`.

```json
{ "broker": "prod-broker", "msgVpnName": "orders-vpn", "msgVpnConfig": { "enabled": false } }
```

**Example request:** "Disable the orders-vpn VPN on prod-broker." (The agent will ask you to confirm before acting.)

### delete-message-vpn

**Destructive.** Delete a Message VPN. Fails if it still has active client
connections or child endpoints.

Annotations: `readOnly: false`, `destructive: true`.

| Name | Type | Required | Description |
|---|---|---|---|
| `broker` | string | yes | Target broker alias. |
| `msgVpnName` | string | yes | The VPN to delete. |

**Returns:** step-keyed envelope, step `deleteVpn`.

```json
{ "broker": "prod-broker", "msgVpnName": "orders-vpn" }
```

**Example request:** "Delete the orders-vpn VPN on prod-broker." (The agent will ask you to confirm before acting.)

### create-queue

Create a queue in a VPN. Fails if one with the same name already exists in the
VPN.

Annotations: `readOnly: false`, `destructive: false`.

| Name | Type | Required | Description |
|---|---|---|---|
| `broker` | string | yes | Target broker alias. |
| `msgVpnName` | string | yes | The VPN to create the queue in. |
| `queueName` | string | yes | Name of the queue to create. |
| `queueConfig` | object | no | Queue attributes (e.g. `accessType`, `egressEnabled`, `ingressEnabled`, `maxMsgSpoolUsage`, `permission`). Omitted attributes take broker defaults. |

**Returns:** step-keyed envelope, step `createQueue`.

```json
{ "broker": "prod-broker", "msgVpnName": "default", "queueName": "orders.q", "queueConfig": { "ingressEnabled": true, "egressEnabled": true } }
```

**Example request:** "Create a queue orders.q in the default VPN on prod-broker." (The agent will ask you to confirm before acting.)

### update-queue

**Destructive.** Partially update a queue — only the supplied attributes change.
Service-affecting: disabling egress halts delivery; lowering the spool quota can
evict messages.

Annotations: `readOnly: false`, `destructive: true`.

| Name | Type | Required | Description |
|---|---|---|---|
| `broker` | string | yes | Target broker alias. |
| `msgVpnName` | string | yes | The VPN containing the queue. |
| `queueName` | string | yes | The queue to modify. |
| `queueConfig` | object | yes | Queue attributes to change. Do not include `msgVpnName` or `queueName`. |

**Returns:** step-keyed envelope, step `updateQueue`.

```json
{ "broker": "prod-broker", "msgVpnName": "default", "queueName": "orders.q", "queueConfig": { "egressEnabled": false } }
```

**Example request:** "Turn off egress on orders.q in the default VPN." (The agent will ask you to confirm before acting.)

### delete-queue

**Destructive.** Delete a queue and discard any messages still spooled on it.

Annotations: `readOnly: false`, `destructive: true`.

| Name | Type | Required | Description |
|---|---|---|---|
| `broker` | string | yes | Target broker alias. |
| `msgVpnName` | string | yes | The VPN containing the queue. |
| `queueName` | string | yes | The queue to delete. |

**Returns:** step-keyed envelope, step `deleteQueue`.

```json
{ "broker": "prod-broker", "msgVpnName": "default", "queueName": "orders.q" }
```

**Example request:** "Delete orders.q from the default VPN on prod-broker." (The agent will ask you to confirm before acting.)

### create-topic-endpoint

Create a topic endpoint in a VPN. Fails if one with the same name already exists
in the VPN.

Annotations: `readOnly: false`, `destructive: false`.

| Name | Type | Required | Description |
|---|---|---|---|
| `broker` | string | yes | Target broker alias. |
| `msgVpnName` | string | yes | The VPN to create the topic endpoint in. |
| `topicEndpointName` | string | yes | Name of the topic endpoint to create. |
| `topicEndpointConfig` | object | no | TopicEndpoint attributes (e.g. `accessType`, `egressEnabled`, `ingressEnabled`, `maxMsgSpoolUsage`, `permission`). Omitted attributes take broker defaults. |

**Returns:** step-keyed envelope, step `createTopicEndpoint`.

```json
{ "broker": "prod-broker", "msgVpnName": "default", "topicEndpointName": "orders.te", "topicEndpointConfig": { "ingressEnabled": true, "egressEnabled": true } }
```

**Example request:** "Create a topic endpoint orders.te in the default VPN on prod-broker." (The agent will ask you to confirm before acting.)

### update-topic-endpoint

**Destructive.** Partially update a topic endpoint — only the supplied attributes
change. Service-affecting: disabling egress halts delivery; lowering the spool
quota can evict messages.

Annotations: `readOnly: false`, `destructive: true`.

| Name | Type | Required | Description |
|---|---|---|---|
| `broker` | string | yes | Target broker alias. |
| `msgVpnName` | string | yes | The VPN containing the topic endpoint. |
| `topicEndpointName` | string | yes | The topic endpoint to modify. |
| `topicEndpointConfig` | object | yes | TopicEndpoint attributes to change. Do not include `msgVpnName` or `topicEndpointName`. |

**Returns:** step-keyed envelope, step `updateTopicEndpoint`.

```json
{ "broker": "prod-broker", "msgVpnName": "default", "topicEndpointName": "orders.te", "topicEndpointConfig": { "egressEnabled": false } }
```

**Example request:** "Turn off egress on the orders.te topic endpoint in the default VPN." (The agent will ask you to confirm before acting.)

### delete-topic-endpoint

**Destructive.** Delete a topic endpoint and discard any messages still spooled on
it.

Annotations: `readOnly: false`, `destructive: true`.

| Name | Type | Required | Description |
|---|---|---|---|
| `broker` | string | yes | Target broker alias. |
| `msgVpnName` | string | yes | The VPN containing the topic endpoint. |
| `topicEndpointName` | string | yes | The topic endpoint to delete. |

**Returns:** step-keyed envelope, step `deleteTopicEndpoint`.

```json
{ "broker": "prod-broker", "msgVpnName": "default", "topicEndpointName": "orders.te" }
```

**Example request:** "Delete the orders.te topic endpoint from the default VPN." (The agent will ask you to confirm before acting.)
