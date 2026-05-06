# `get-broker-health` — Curated Field List

**Status:** proposal pending review.
**Story:** SOL-148428 (Story 8 — Broker-Level Monitoring Tools).
**Branch:** `amorade/sempv1-tools` (built on top of `amorade/sempv1-client`).
**Last updated:** 2026-04-29

This document captures the curated field set the `get-broker-health` MCP tool will surface to LLMs, with evidence for each inclusion and a record of every field that was considered and dropped.

## Why curate?

The four SEMPv1 commands underpinning this tool return ~462 fields total in the XSD-aligned response structs. Returning all of them would:
- bloat LLM context windows for every invocation,
- bury the operationally meaningful signals,
- mix configuration facts (e.g., `cpu-cores`) with health indicators (e.g., `physical-memory-usage-percent`).

Research across Solace public docs, internal Confluence runbooks, and the Solace Community forum identified a much smaller set of fields that operators *actually* check when assessing broker health. That set is what this tool returns.

## Decision — protocol

**SEMPv1 only.** A separate analysis recommended a hybrid SEMPv1 + SEMPv2 approach, but every field in the curated list is reachable via SEMPv1, and Story 8's premise was that v1 is required precisely because v2 lacks these fields. Mixing protocols would add failure modes without adding signal.

| Source command | Fields contributed |
|---|---|
| `<rpc><show><version/></show></rpc>` | 2 |
| `<rpc><show><system/></show></rpc>` | 17 |
| `<rpc><show><memory/></show></rpc>` | 2 |
| `<rpc><show><message-spool><detail/></message-spool></show></rpc>` | 16 |
| `<rpc><show><system><health/></system></show></rpc>` *(optional, see below)* | 1 |
| **Total (without health)** | **37** |
| **Total (with health)** | **38** |

## Curated fields per command

### `show version` → 2 fields

| XML field | JSON key (camelCase) | Operational meaning |
|---|---|---|
| `description` | `description` | Broker version (e.g. `"Solace PubSub+ Standard Version 10.25.0.217"`) — support-lifecycle check, every operator runbook starts here |
| `uptime/total-secs` | `uptime.totalSecs` | Broker uptime in seconds — surfaces unexpected reboots |

### `show system` → 17 fields

Two groups: uptime/restart context, plus scaling/capacity (added per reviewer feedback — under- or mis-scaled brokers are a recurring source of issues, and the broker exposes "available vs required" pairs so the LLM can flag misconfiguration).

**Uptime / restart**

| XML field | JSON key | Operational meaning |
|---|---|---|
| `system-uptime-seconds` | `systemUptimeSeconds` | Broker uptime, directly numeric (no nested `<uptime>` parsing) |
| `last-restart-reason` | `lastRestartReason` | Was the most recent reboot intentional or a crash? |

**Scaling — broker-tier limits**

| XML field | JSON key | Operational meaning |
|---|---|---|
| `max-bridges` | `maxBridges` | Bridge tier limit |
| `max-connections` | `maxConnections` | Connection tier limit |
| `max-queue-messages` | `maxQueueMessages` | Queue message tier limit |
| `max-kafka-bridges` | `maxKafkaBridges` | Kafka bridge tier limit |
| `max-kafka-broker-connections` | `maxKafkaBrokerConnections` | Kafka broker connection tier limit |
| `max-subscriptions` | `maxSubscriptions` | Subscription tier limit |
| `max-guaranteed-message-size` | `maxGuaranteedMessageSize` | Largest single message the broker will accept |

**System resources — available vs required**

| XML field | JSON key | Operational meaning |
|---|---|---|
| `cpu-cores` | `cpuCores` | CPU cores **available** to the broker container |
| `cpu-cores-required` | `cpuCoresRequired` | CPU cores **required** by the configured scaling tier |
| `host-virtual-memory` | `hostVirtualMemory` | Host virtual memory available (GiB) |
| `host-virtual-memory-required` | `hostVirtualMemoryRequired` | Host virtual memory required (GiB) |
| `memory-cgroup-limit` | `memoryCgroupLimit` | cgroup memory limit available (GiB) |
| `memory-cgroup-limit-required` | `memoryCgroupLimitRequired` | cgroup memory limit required (GiB) |
| `shared-memory` | `sharedMemory` | Shared memory available (GiB) |
| `shared-memory-required` | `sharedMemoryRequired` | Shared memory required (GiB) |

> **Why these are health signals, not just config:** when `*-required` exceeds the corresponding non-required field, the broker is under-scaled — a known source of past issues per reviewer. The pair is far more useful together than either alone.

### `show memory` → 2 fields

| XML field | JSON key | Operational meaning |
|---|---|---|
| `physical-memory-usage-percent` | `physicalMemoryUsagePercent` | Headline memory pressure indicator — Datadog alarms on this |
| `subscription-memory-usage-percent` | `subscriptionMemoryUsagePercent` | Subscription-table memory pressure |

### `show message-spool detail` → 15 fields

All under `<message-spool-info>`.

| XML field | JSON key | Operational meaning |
|---|---|---|
| `config-status` | `configStatus` | `Enabled (Primary)` or `Shutdown` |
| `operational-status` | `operationalStatus` | `AD-Active` / `AD-Standby` / `AD-NotReady` — most-cited spool field across all sources |
| `datapath-up` | `datapathUp` | `true`/`false` — datapath health |
| `synchronization-status` | `synchronizationStatus` | HA synchronization state |
| `spool-sync-status` | `spoolSyncStatus` | HA spool synchronization |
| `active-disk-partition-usage` | `activeDiskPartitionUsage` | Disk partition % — drives the **spool full** alarm at 80% (Solace default) |
| `message-count-utilization-percentage` | `messageCountUtilizationPercentage` | Spool message-count quota % |
| `spool-files-utilization-percentage` | `spoolFilesUtilizationPercentage` | Spool file-count alarm (`SYSTEM_AD_SPOOL_FILES_EXCEEDED`) |
| `transaction-resource-utilization-percentage` | `transactionResourceUtilizationPercentage` | Transaction resource pressure |
| `transacted-session-resource-utilization-percentage` | `transactedSessionResourceUtilizationPercentage` | Transacted session pressure |
| `delivered-unacked-msgs-utilization-percentage` | `deliveredUnackedMsgsUtilizationPercentage` | Unacked message queue pressure |
| `total-messages-currently-spooled` | `totalMessagesCurrentlySpooled` | Current message count |
| `max-message-count` | `maxMessageCount` | Quota for spooled messages |
| `defrag-est-fragmentation-percentage` | `defragEstFragmentationPercentage` | Sparse-spool / fragmentation detection (real community pain point) |
| `last-failure-reason` | `lastFailureReason` | Most recent spool failure reason |
| `last-failure-time` | `lastFailureTime` | Most recent spool failure timestamp |

> The two `last-failure-*` fields are bundled because operators always want them as a pair when investigating recent issues.

### *(optional)* `show system health` → 1 field

A separate SEMPv1 command (`<rpc><show><system><health/></system></show></rpc>`). This is **not** in our current 4 captured fixtures — it would add a 5th parallel call.

| XML field | JSON key | Operational meaning |
|---|---|---|
| `compute-latency-current-value` | `computeLatencyCurrentValue` | Closest available proxy for CPU pressure (no direct CPU% exists in SEMP) |

**Decision pending:** include this 5th call, or ship without it and add later if the LLM needs CPU signal.

## Output envelope shape

Step-keyed envelope, one top-level key per source command:

```json
{
  "version": {
    "description": "Solace PubSub+ Standard Version 10.25.0.217",
    "uptime": { "totalSecs": 87143 }
  },
  "system": {
    "systemUptimeSeconds": 87143,
    "lastRestartReason": ""
  },
  "memory": {
    "physicalMemoryUsagePercent": 60.26,
    "subscriptionMemoryUsagePercent": 0.0078
  },
  "spool": {
    "messageSpoolInfo": {
      "configStatus": "Enabled (Primary)",
      "operationalStatus": "AD-Active",
      "datapathUp": "true",
      "synchronizationStatus": "Synced",
      "spoolSyncStatus": "Synced",
      "activeDiskPartitionUsage": "42.22",
      "messageCountUtilizationPercentage": "0.00",
      "spoolFilesUtilizationPercentage": "0.00",
      "transactionResourceUtilizationPercentage": "0.00",
      "transactedSessionResourceUtilizationPercentage": "0.00",
      "deliveredUnackedMsgsUtilizationPercentage": "0.00",
      "totalMessagesCurrentlySpooled": 0,
      "maxMessageCount": "100M",
      "defragEstFragmentationPercentage": 0,
      "lastFailureReason": "N/A",
      "lastFailureTime": ""
    }
  }
}
```

## Excluded fields — Story 8 vs. operator reality

Story 8's acceptance criteria listed 11 specific output fields. Comparing them against the curated set:

| Story 8 field | Status | Reason |
|---|---|---|
| `version` | ✅ kept | Renamed in curated form to `description` to match wire field |
| `uptimeSeconds` | ✅ kept | Sourced from `system-uptime-seconds` (preferred over the nested `uptime` object) |
| `cpuCores` | ✅ kept (added back) | Now part of the available-vs-required scaling pair — `cpu-cores` and `cpu-cores-required` together flag under-scaling |
| `memoryUsagePercent` | ✅ kept (renamed) | Sourced from `physical-memory-usage-percent` |
| `memoryTotalKB` | 🔴 excluded | Operators monitor the percentage; raw totals are configuration data |
| `memoryUsedKB` | 🔴 excluded | Same — derivable from total × percentage if ever needed |
| `memoryFreeKB` | 🔴 excluded | Same |
| `diskUsagePercent` | ✅ kept (renamed) | Sourced from `active-disk-partition-usage` (the partition operators alarm on) |
| `currentSpoolUsageMB` | 🟡 partially | We expose `total-messages-currently-spooled` (count) and `messageCountUtilizationPercentage` (%). MB-as-bytes is derivable but not directly exposed; can add `current-disk-usage` if MB is required. |
| `maxSpoolUsageMB` | 🟡 partially | Same — `max-message-count` exposed as count quota, MB quota would require an additional field |
| `totalMessagesSpooled` | ✅ kept | Sourced from `total-messages-currently-spooled` |

### Story 8 fields *not in our curated list at all*: 3 of 11

- `memoryTotalKB`, `memoryUsedKB`, `memoryFreeKB`

### Story 8 fields with naming/source differences but operationally equivalent: 6 of 11

- `version`, `uptimeSeconds`, `cpuCores`, `memoryUsagePercent`, `diskUsagePercent`, `totalMessagesSpooled`

### Story 8 fields needing follow-up if MB-precision is required: 2 of 11

- `currentSpoolUsageMB`, `maxSpoolUsageMB` — MB granularity wasn't cited as critical in any operator source we reviewed; usage percentage and message count are what runbooks actually use. If a stakeholder requires MB, add `current-disk-usage` and `max-disk-usage` to the curation.

## Fields *added* beyond Story 8 (operator-justified)

These appear in operator runbooks but were missing from Story 8's list:

| Added field | Justification source |
|---|---|
| `last-restart-reason` | Internal SS-space runbooks; appliance-health-check public doc |
| `max-bridges`, `max-connections`, `max-queue-messages`, `max-kafka-bridges`, `max-kafka-broker-connections`, `max-subscriptions`, `max-guaranteed-message-size` | Reviewer feedback — broker tier scaling limits, source of misconfiguration issues |
| `cpu-cores-required`, `host-virtual-memory`, `host-virtual-memory-required`, `memory-cgroup-limit`, `memory-cgroup-limit-required`, `shared-memory`, `shared-memory-required` | Reviewer feedback — available-vs-required pairs flag under-scaled deployments (recurring incident pattern) |
| `subscription-memory-usage-percent` | Datadog metric catalogue (internal) |
| `config-status` | Every internal runbook |
| `operational-status` | Every internal runbook + community |
| `datapath-up` | Internal runbooks |
| `synchronization-status` | Internal HA runbooks |
| `spool-sync-status` | Internal HA runbooks |
| `message-count-utilization-percentage` | Solace public docs |
| `spool-files-utilization-percentage` | Community thread (`SYSTEM_AD_SPOOL_FILES_EXCEEDED`) |
| `transaction-resource-utilization-percentage` | Solace public docs |
| `transacted-session-resource-utilization-percentage` | Solace public docs |
| `delivered-unacked-msgs-utilization-percentage` | Solace public docs |
| `defrag-est-fragmentation-percentage` | Multiple community threads |
| `last-failure-reason` + `last-failure-time` | Internal runbooks |

## Sources reviewed

### Solace public documentation
- [Appliance Event Broker Health Check](https://docs.solace.com/Appliance/broker-appliance-health-check.htm)
- [Monitoring Guaranteed Messaging via CLI](https://docs.solace.com/Messaging/Guaranteed-Msg/Monitoring-Guaranteed-Messaging.htm)
- [Monitoring the Health of the Solace Software Event Broker](https://docs.solace.com/Monitoring/SW-Health-Monitoring.htm)
- [Performance Monitoring](https://docs.solace.com/Monitoring/Perf-Mon.htm)
- [Gathering Statistics with SEMP](https://docs.solace.com/Monitoring/Gathering-Stats-SEMP.htm)
- [eG Innovations — Solace Memory Test (3rd-party reference)](https://www.eginnovations.com/documentation/Solace-PubSub-Event-Broker/Solace-Memory-Test.htm)

### Internal Confluence (sol-jira.atlassian.net)
- [Final version for Broker Health Check Items (Solace Support, page 5698256918)](https://sol-jira.atlassian.net/wiki/spaces/SS/pages/5698256918)
- [Broker (Appliance) Health Check Docs Draft (Solace Support, page 3983114243)](https://sol-jira.atlassian.net/wiki/spaces/SS/pages/3983114243)
- [Broker Health Check Items (Solace Support, page 3856597270)](https://sol-jira.atlassian.net/wiki/spaces/SS/pages/3856597270)
- [Health Check for brokers before/after maintenance activity (PSG, page 5907579440)](https://sol-jira.atlassian.net/wiki/spaces/PSG/pages/5907579440)
- [How is the available disk space calculated? (page 975570559)](https://sol-jira.atlassian.net/wiki/spaces/.../pages/975570559)
- Reference: DataDog All Monitors (page 969900909) — too large to fully fetch; confirms `physical-memory-usage-percent` is monitored
- Solace Cloud Clusters - Common Issues Runbook (page 6267633681)

### Solace Community forum threads
- [Slow subscriber causing solace spool quota blow up](https://solace.community/discussion/491/slow-subscriber-causing-solace-spool-quota-blow-up)
- [Messages getting discarded from broker](https://solace.community/discussion/679/messages-getting-discarded-from-broker)
- [Message Spool Ingress Discard](https://solace.community/discussion/3422/message-spool-ingress-discard)
- [503: Spool Over Quota — Message VPN limit exceeded](https://solace.community/discussion/513/503-spool-over-quota-message-vpn-limit-exceeded)
- [SYSTEM_AD_SPOOL_FILES_EXCEEDED occurred while running Event Broker](https://solace.community/discussion/3473/system-ad-spool-files-exceeded-occurred-while-running-event-broker)
- [Troubleshooting frequent / high amounts of fragmentation on an event broker](https://solace.community/discussion/3452/troubleshooting-frequent-high-amounts-of-fragmentation-on-an-event-broker)
- [Defragmentation Failing: Unmovable Local Transaction](https://solace.community/discussion/3299/defragmentation-failing-unmovable-local-transaction)
- [Why the disk is full while only few messages are spooled](https://solace.community/discussion/17/why-the-disk-is-full-while-only-few-messages-are-spooled-when-using-internal-disk)
- [Exceeded Spool File Limit](https://solace.community/discussion/1720/exceeded-spool-file-limit-topic-my-topic)
- [HA Pair Issue: AD-NotReady Local ADB Key Invalid](https://solace.community/discussion/3971/ha-pair-issue-ad-notready-local-adb-key-invalid)
- [Spool Over Quota. Router limit exceeded](https://solace.community/discussion/1256/spool-over-quota-router-limit-exceeded)
- [High disk usage](https://solace.community/discussion/3320/high-disk-usage)

### Internal Jira
- SOL-148428 — Story 8 (this story; cited for the original AC list and the team decision to land MVP without curation)
- SOL-148912 — tracks post-MVP curation (this document is the input for that effort)

## Open questions for review

1. **5th SEMPv1 call** for `<show><system><health>` to surface `compute-latency-current-value`? Pro: only CPU-pressure proxy in SEMP. Con: extra call, extra fixture, extra struct.
2. **MB precision for spool usage?** Story 8 listed `currentSpoolUsageMB` and `maxSpoolUsageMB`. Curated list provides percent and message-count instead. Add MB fields if needed.
3. **`cpu-cores` and `system-memory` (total) as context fields?** Operators don't use them for health, but an LLM may want them when answering "describe my broker." Could be exposed in a separate `get_broker_info` tool rather than `get-broker-health`.
4. **Curation strategy in code:** separate health-view structs (Option 1) vs `json:"-"` tags on the full XSD-aligned structs (Option 2)?

## Next steps

Once this curated list is signed off:
- Update `docs/semp/get-broker-health-implementation-plan.md` to reflect the curated set instead of raw passthrough.
- Decide curation strategy (Option 1 / Option 2) — affects B3 and B4.
- B5 round-trip tests assert presence of curated fields; cmp.py (B5b) uses this list as the expected output set.
