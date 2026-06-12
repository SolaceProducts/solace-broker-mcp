# `get-discard-stats` — Curated Field List

**Status:** initial — landed with SOL-148432 review feedback.
**Story:** SOL-148432 (Advanced Monitoring Tools — Replication, Discards, Slow Subscribers).
**Last updated:** 2026-05-25.

This document captures the curated discard-counter field set the
`get-discard-stats` MCP tool surfaces to LLMs, with rationale for each
inclusion and a record of what was considered and dropped.

## Why curate?

The three underlying SEMPv1 commands return ~250 fields combined, covering
connection counts, message throughput, byte counters, certificate-revocation
stats, transaction operation counters, replication state, and many fields
that have nothing to do with the question "is the broker dropping messages,
and if so where?". Returning all of them would bloat LLM context and bury
the operational signal.

This tool keeps only the fields that directly answer "where did messages
go?" — every other class of stat belongs in a different tool
(`get-broker-status` for capacity/uptime, `get-replication-status` for
replication, etc.).

## Decision — protocol

**SEMPv1.** Story SOL-148432's premise was that broker-wide aggregated
discard counters are required, and SEMPv2 does not expose broker-level
aggregates — it only exposes per-queue/per-VPN raw counters that the caller
would have to sum. The companion `list-queue-discards` composite YAML tool
covers the per-queue SEMPv2 inspection path; this tool covers the SEMPv1
broker-level path.

## Modes

| Mode | RPC | Fields returned |
|---|---|---|
| `vpnName` omitted (broker-wide) | `<rpc><show><stats><client/></stats></show></rpc>` + `<rpc><show><message-spool><stats/></message-spool></show></rpc>` (parallel) | client (21) + spool (~26) |
| `vpnName` provided (per-VPN) | `<rpc><show><message-vpn><vpn-name>X</vpn-name><stats/></message-vpn></show></rpc>` | client (19) |

**Asymmetry.** The per-VPN response is a near-complete subset of the
broker-wide client-stats view: 10 ingress + 9 egress categories
(vs 11 + 10 broker-wide). The two missing client fields are broker-global
by nature: `web-parse-error` on ingress (REST/web ingress is shared
infrastructure) and `payload-could-not-be-formatted` on egress
(protocol-bridge transforms are not VPN-scoped). The bigger gap is at the
spool layer: SEMPv1 does not expose `<message-spool-stats>` scoped to a
single VPN — `<rpc><show><message-spool><stats/></message-spool></show></rpc>`
is broker-global. Per-VPN spool-level discards are simply not available
at this layer of the broker's API and are therefore omitted from the
per-VPN mode. Operators asking "where are messages going in VPN X at the
spool/queue layer?" should follow up with `list-queue-discards`, which
uses SEMPv2's per-queue counters.

## Response envelope

**Broker-wide:**
```json
{
  "clientDiscards": {
    "ingress": { ... 11 fields ... },
    "egress":  { ... 10 fields ... }
  },
  "spoolDiscards": { ... ~26 fields ... }
}
```

**Per-VPN:**
```json
{
  "vpnName": "<vpn>",
  "clientDiscards": {
    "ingress": { ... 10 fields ... },
    "egress":  { ... 9 fields ... }
  }
}
```

## Curated fields per source

### `<show stats client>` → `clientDiscards` (21 fields)

**`ingress` — 11 categories** (messages dropped on the way in)

| XML field | JSON key | Operational meaning |
|---|---|---|
| `total-ingress-discards` | `totalIngressDiscards` | Roll-up — the headline number |
| `no-subscription-match` | `noSubscriptionMatch` | Publisher sent to a topic no one subscribed to |
| `topic-parse-error` | `topicParseError` | Malformed topic from a publisher |
| `parse-error` | `parseError` | Malformed SMF frame |
| `msg-too-big` | `msgTooBig` | Above broker max message size |
| `ttl-exceeded` | `ttlExceeded` | TTL hit before the broker could route |
| `web-parse-error` | `webParseError` | REST/web ingress parse failure |
| `publish-topic-acl` | `publishTopicAcl` | ACL denied the publish |
| `msg-spool-discards` | `msgSpoolDiscards` | Spool refused the message (quota/state) |
| `message-promotion-congestion` | `messagePromotionCongestion` | Direct→guaranteed promotion blocked |
| `message-spool-congestion` | `messageSpoolCongestion` | Spool busy / back-pressure |

**`egress` — 10 categories** (messages dropped on the way out to consumers)

| XML field | JSON key | Operational meaning |
|---|---|---|
| `total-egress-discards` | `totalEgressDiscards` | Roll-up |
| `transmit-congestion` | `transmitCongestion` | Slow subscriber back-pressure → dropped |
| `compression-congestion` | `compressionCongestion` | Compressor couldn't keep up |
| `message-elided` | `messageElided` | Elision feature dropped older copy |
| `ttl-exceeded` | `ttlExceeded` | TTL hit while sitting in egress |
| `payload-could-not-be-formatted` | `payloadCouldNotBeFormatted` | Protocol-bridge transform failed |
| `message-promotion-congestion` | `messagePromotionCongestion` | Promotion blocked on egress |
| `message-spool-congestion` | `messageSpoolCongestion` | Spool back-pressure on egress |
| `client-not-connected` | `clientNotConnected` | Tried to send to a disconnected client |
| `msg-spool-egress-discards` | `msgSpoolEgressDiscards` | Spool egress dropped the message |

### `<show message-spool stats>` → `spoolDiscards` (~26 fields)

Spool-level counters that aren't visible in client-stats. Grouped here by
operator intent, not by XML position.

**Quota / capacity** (the most common "too much, drop it" reason)

| XML | JSON | Why operators care |
|---|---|---|
| `discard-spool-over-quota` | `discardSpoolOverQuota` | Whole spool above quota |
| `discard-qendpt-over-quota` | `discardQueueEndpointOverQuota` | A queue endpoint above its quota |
| `discard-replay-log-over-quota` | `discardReplayLogOverQuota` | Replay log full |
| `discard-max-msg-usage-exceeded` | `discardMaxMsgUsageExceeded` | Per-message resource limit |
| `discard-max-msg-size-exceeded` | `discardMaxMsgSizeExceeded` | Message above per-queue size limit |
| `discard-spool-file-limit-exceeded` | `discardSpoolFileLimitExceeded` | Spool-file count limit reached |

**Storage path failures** (the spool itself failing — page these on a real broker)

| XML | JSON | Why |
|---|---|---|
| `discard-spool-to-adb-fail` | `discardSpoolToAdbFail` | Async disk buffer write failed |
| `discard-spool-to-disk-fail` | `discardSpoolToDiskFail` | Disk write failed |
| `spool-shutdown-discard` | `spoolShutdownDiscard` | Spool shutdown while message in-flight |

**Routing / delivery**

| XML | JSON | Why |
|---|---|---|
| `discard-nodest` | `discardNoDest` | No destination queue for the message |
| `discard-queue-not-found` | `discardQueueNotFound` | Named queue is gone |
| `no-local-delivery-discard` | `noLocalDeliveryDiscard` | NoLocal subscription dropped own publish |
| `discard-other` | `discardOther` | Unclassified — always check when non-zero |

**Congestion**

| XML | JSON | Why |
|---|---|---|
| `low-priority-msg-congestion-discard` | `lowPriorityMsgCongestionDiscard` | Low-prio dropped under load |

**Replication**

| XML | JSON | Why |
|---|---|---|
| `replication-is-standby-discard` | `replicationIsStandbyDiscard` | Standby broker dropping (correct on standby; alarming on active) |
| `sync-replication-ineligible-discard` | `syncReplicationIneligibleDiscard` | Replication ineligible → drop |

**TTL** — the broker emits both "discarded" (no DMQ route) and
"to-dmq" (routed to DMQ) counts. Keep both so operators can tell whether
messages were *lost* or just *moved*. Also include the "failures" counter
for the to-dmq path — that's "we tried to DMQ but couldn't, so dropped."

| XML | JSON |
|---|---|
| `total-ttl-expired-discard-messages` | `totalTtlExpiredDiscardMessages` |
| `total-ttl-expired-to-dmq-messages` | `totalTtlExpiredToDmqMessages` |
| `total-ttl-expired-to-dmq-failures` | `totalTtlExpiredToDmqFailures` |
| `total-ttl-exceeded-discard-messages` | `totalTtlExceededDiscardMessages` |

**Max-redelivery** — same DMQ split as TTL.

| XML | JSON |
|---|---|
| `max-redelivery-exceeded-discard-messages` | `maxRedeliveryExceededDiscardMessages` |
| `max-redelivery-exceeded-to-dmq-messages` | `maxRedeliveryExceededToDmqMessages` |
| `max-redelivery-exceeded-to-dmq-failures` | `maxRedeliveryExceededToDmqFailures` |

**Sequence-number / ordering**

| XML | JSON |
|---|---|
| `seq-num-messages-discarded` | `seqNumMessagesDiscarded` |

**Aggregate roll-ups**

| XML | JSON |
|---|---|
| `total-discarded-messages` | `totalDiscardedMessages` |
| `total-discarded-egress-messages` | `totalDiscardedEgressMessages` |

### `<show message-vpn ... stats>` → per-VPN `clientDiscards` (19 fields)

The broker's per-VPN ingress/egress sub-trees are a near-complete subset
of the broker-wide client-stats view, missing only the two fields that
are broker-global by nature (`web-parse-error`, `payload-could-not-be-formatted`).

**`ingress` — 10 categories:** `totalIngressDiscards`, `noSubscriptionMatch`,
`topicParseError`, `parseError`, `msgTooBig`, `ttlExceeded`,
`publishTopicAcl`, `msgSpoolDiscards`, `messagePromotionCongestion`,
`messageSpoolCongestion`.

**`egress` — 9 categories:** `totalEgressDiscards`, `transmitCongestion`,
`compressionCongestion`, `messageElided`, `ttlExceeded`,
`messagePromotionCongestion`, `messageSpoolCongestion`,
`clientNotConnected`, `msgSpoolEgressDiscards`.

Operational semantics for each field match the broker-wide tables above.

## What was considered and dropped

- **`show stats client` non-discard fields** — connection counts, byte
  rates, message totals, certificate revocation stats, control-plane
  messages, subscription operations. These belong in `get-broker-status` /
  `get-client-info` / future tools, not here.
- **`show message-spool stats` non-discard fields** — `total-spooled-messages`,
  `egress-messages-redelivered`, `request-for-redelivery`, transaction
  operation counters, replay state. These are throughput / capacity, not
  discards.
- **Per-queue per-category counters via SEMPv2** — covered by the companion
  `list-queue-discards` composite tool, not duplicated here.

## References

- [SEMPv1 schema — show stats client](https://docs.solace.com/Admin/SEMP/SEMP-Reference.htm)
- Companion tool: `internal/composite/definitions/tools.yaml` →
  `list-queue-discards` (per-queue SEMPv2 path).
- Story SOL-148432, Balazs's review comment recommending SEMPv1.
