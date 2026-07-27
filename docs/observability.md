# Observability Schema

> **Status: Draft for pilot review.** This document is the proposed metric, audit, and
> trace schema for the Broker MCP Server. It is published for review **before** the names
> freeze at GA. After GA we commit to only ever *adding* to this schema, never renaming, so
> the time to change a name is now. See [How to give feedback](#how-to-give-feedback).
>
> **Metrics, audit, and tracing are not emitted by the current build.** Only their feature
> flags exist today: there is no `/metrics` handler, no audit emission, and no OTLP
> exporter. Those sections are written in the present tense of the *proposed* design,
> because the design is what you are being asked to review. Read them as a specification,
> not as a description of a running system, and do not point a dashboard or SIEM query at
> them until the corresponding signal ships.
>
> **Correlation IDs are the exception: they are implemented and on by default.** The
> [Correlation ID](#correlation-id) section describes shipped behaviour you can rely on
> today.

Once implemented, the Broker MCP Server will emit three observability signals:

- **Metrics**, on a Prometheus `/metrics` endpoint, for dashboards and alerts.
- **An audit trail**, one JSON event per state-changing operation, for compliance evidence.
- **Distributed traces**, exported over OTLP, for end-to-end request diagnosis.

One correlation ID threads each request through logs, traces, and audit records, so they
line up on the same call. Metrics join to those signals on the shared `outcome` label rather
than the correlation ID; a per-request ID as a metric label would blow up cardinality.

Metrics, audit, and tracing are each off by default and enabled per feature flag, so you
turn each on when your operations model is ready. Correlation IDs are on by default, since
they carry no schema to review. This document describes what each signal contains and what
each name means.

---

## How to give feedback

We are asking pilot operators one question:

> **If you were going to build a Grafana dashboard or a SIEM query against this, would you
> rename or relabel anything?**

Specifically:

- Do the metric and label names match what your dashboards and alert rules expect?
- Does the audit field set cover your access-review and SOC 2 / SOX / PCI DSS needs?
- Does the single [`outcome` vocabulary](#the-outcome-vocabulary) work for your SIEM
  queries, or do you separate these differently today?
- Is anything missing that you would need on day one?

Send comments through your Solace pilot channel or to the contacts in the observability
brief. The review window runs up to the GA freeze. Anything we do not hear back on ships as
drafted; because the schema is additive-only, we can always add fields later, we just will
not rename what is already there.

---

## Conventions

| Convention | Rule |
|---|---|
| Metric prefix | All first-party metrics start with `mcp_`. |
| Metric units | Base units, per Prometheus convention. Durations are in **seconds** (`_seconds`); counters end in `_total`. |
| Audit units | Durations are in **milliseconds** (`duration_ms`). This differs from metrics on purpose: metrics follow Prometheus base units, audit follows common SIEM JSON convention. |
| Timestamps | RFC 3339, UTC. |
| Naming basis | Where OpenTelemetry publishes a semantic convention, we adopt it and translate `.` to `_` for Prometheus (for example `http.request.method` becomes `http_request_method`). Where OTel has no convention, we use a documented Solace-specific name. Each name below is tagged **OTel** or **Solace**. |
| Cardinality | A CI check fails the build on any undocumented metric name or label key. Label values are drawn from finite domains (configured brokers, SEMP operations, HTTP status codes, the retry cap), so series cardinality stays bounded. No label carries a free-text or unbounded value. |
| Redaction | Credentials, tokens, and raw tool arguments are never written to any signal. |

### Schema versioning

Two independent versions are published, so your queries can pin to a version and detect drift:

- `metrics_schema` (current: **1.0**), surfaced by the `mcp_schema_version` metric.
- `audit_schema` (current: **1.0**), surfaced as the `audit_schema_version` field on every audit event.

Versioning is `MAJOR.MINOR`. A **minor** bump is additive and backward compatible (a new
metric, a new audit field, a new `outcome` value). A **major** bump is reserved for a
breaking change (a rename or removal), which the additive-only commitment is designed to
avoid. Pin dashboards to `mcp_schema_version` and SIEM queries to `audit_schema_version`.

---

## Metrics

All metrics are served on the `/metrics` endpoint (Story 14) behind `OBS_METRICS_ENABLED`,
in Prometheus text exposition format.

### Server and scrape health

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_build_info` | Gauge (constant `1`) | `version` | Solace |
| `mcp_schema_version` | Gauge (constant `1`) | `metrics_schema`, `audit_schema` | Solace |
| `mcp_metrics_scrape_total` | Counter | none | Solace |
| `mcp_http_active_requests` | Gauge | none | Solace |

- `mcp_build_info` and `mcp_schema_version` are the standard Prometheus "info metric"
  pattern: a constant `1` carrying identifying labels. They let a dashboard show the running
  version and the schema versions it was built against.
- `mcp_metrics_scrape_total` answers "is Prometheus actually scraping this instance?"
- `mcp_http_active_requests` is the in-flight request gauge, for separating a capacity
  problem from a tail-latency problem.

**Cardinality:** trivial (one series each, plus one per label value on the info metrics).

### Tool invocations (RED)

The core Rate / Errors / Duration signal for every tool the server exposes.

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_tool_invocation_total` | Counter | `tool`, `broker`, `outcome` | Solace |
| `mcp_tool_invocation_duration_seconds` | Histogram | `tool`, `broker`, `outcome` | Solace |

- `tool`: the MCP tool name (kebab-case, for example `get-broker-status`). Bounded by the
  number of tools the server exposes.
- `broker`: the broker alias from your configuration. Bounded by the number of configured
  brokers.
- `outcome`: see [The outcome vocabulary](#the-outcome-vocabulary).
- Histogram buckets (seconds): `0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10`.

**Cardinality:** `|tool| x |broker| x |outcome|`. All three are finite and CI-enforced.

### SEMP requests (RED, per attempt)

Request rate, errors, and latency for each call the server makes to a broker over SEMP,
recorded per retry attempt so you can see retry storms and per-broker latency.

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_semp_request_total` | Counter | `http_request_method`, `http_response_status_code`, `server_address`, `broker`, `api`, `operation`, `attempt` | Mixed (see below) |
| `mcp_semp_request_duration_seconds` *(name proposed, see open items)* | Histogram | same label set | Mixed |

- **OTel** labels (adopted from the OpenTelemetry HTTP semantic conventions,
  https://opentelemetry.io/docs/specs/semconv/http/http-spans/): `http_request_method`,
  `http_response_status_code`, `server_address`.
- **Solace** labels: `broker` (the configured alias), `api` (`v1` or `v2`), `operation`
  (the SEMP operation), `attempt` (the retry attempt as an integer string, `"1"`, `"2"`, ...).

**Cardinality:** bounded by the product of these finite sets. `attempt` is bounded by the
retry cap. See open items for two decisions we want your input on: the duration histogram
buckets, and whether `server_address` and `broker` are redundant for your queries.

### Broker reachability

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_broker_reachable` | Gauge (`1`/`0`) | `broker` | Solace |
| `mcp_broker_unreachable_reason` | Gauge (`1`/`0`) | `broker`, `reason` | Solace |

- `mcp_broker_reachable` is set passively from the result of real calls; it is not a
  heartbeat. A broker is reported unreachable only after a real call fails, so a broker
  outage shows up here as a metric to alert on, not as a failed pod.
- `reason` is a closed set: `credential_invalid` (a 401 from the broker), `unreachable`
  (connection refused, DNS failure, or I/O timeout), `broker_error` (any other non-2xx).
- `mcp_broker_unreachable_reason` is **one-hot per broker**: at most one `reason` series
  per broker is `1` at any moment, and every other `reason` for that broker is explicitly
  `0`. All reasons for a broker are published once it has been seen, so a series never
  disappears mid-incident and `max by (reason)` cannot straddle two causes. A broker that
  is reachable has every `reason` at `0`.
- Alert on `mcp_broker_reachable == 0` and use this gauge only to attribute the cause;
  `mcp_broker_unreachable_reason == 1` is deliberately redundant with it rather than a
  second, separately-timed source of truth.

**Cardinality:** `|broker|` for the first metric; `|broker| x |reason|` for the second.

### Authentication failures

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_auth_failure_total` | Counter | `reason` | Solace |

- `reason` is a closed set: `invalid_token`, `expired`, `audience_mismatch`,
  `signature_invalid` (a token-signing or JWKS-rotation failure, distinct from a malformed
  token), `missing`.
- The values are deliberately coarse so no token content is ever exposed as a label.

**Cardinality:** `|reason|` (five values).

### Audit pipeline health

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_audit_events_dropped_total` | Counter | none | Solace |

Increments if an audit event cannot be written (see [Audit delivery](#audit-delivery)). A
flat-zero series is your evidence that no audit event was lost. Alert on any increase.

### Distributed tracing pipeline health

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_otel_spans_exported_total` | Counter | none | Solace |
| `mcp_otel_spans_dropped_total` | Counter | `reason` | Solace |

Self-observation for the trace exporter, exposed when both tracing and metrics are enabled.
`reason` value domain is under review (see open items).

### Go runtime and process metrics

Standard `go_*` and `process_*` collectors from the Prometheus Go client library
(`collectors.NewGoCollector()` and `collectors.NewProcessCollector()`): goroutine count,
garbage-collection timing, memory stats, file descriptors, CPU. These names are upstream
Prometheus conventions, not Solace-defined, and are listed here only so you know they are
present for diagnosing memory pressure and goroutine leaks.

---

## Audit trail

One JSON event is emitted per **state-changing** operation (for example `disconnect-client`,
`delete-queue`, broker shutdown), at completion, with the outcome known. Read-only calls are
not audited. Authentication lifecycle events are also emitted (see below). The stream is
enabled with `OBS_AUDIT_LOG_ENABLED`.

Every event carries a top-level `"event": "audit"` tag so your log shipper can route the
audit sub-stream to a dedicated SIEM index.

### Event fields

| Field | Meaning | Type |
|---|---|---|
| `event` | Routing tag, always `audit` | string |
| `audit_event_type` | Which kind of audit record this is; discriminate on this, not on `event` | string (closed set, below) |
| `timestamp_utc` | When the event was recorded | RFC 3339 UTC |
| `started_at` | When the call began | RFC 3339 UTC |
| `duration_ms` | How long the call took | integer (ms) |
| `principal.sub` | The authenticated human user (the OIDC `sub` claim) | string |
| `agent_client_id` | Which AI agent or client made the call, distinct from the human user | string |
| `tool` | The MCP tool invoked | string |
| `broker` | The broker targeted | string |
| `outcome` | The result; see [The outcome vocabulary](#the-outcome-vocabulary) | string |
| `arguments_hash` | SHA-256 over an RFC 8785 (JCS) canonicalization of the call arguments | hex string |
| `correlation_id` | Join key to logs, traces, and the broker-side entry | string |
| `reason` | Why the attempt failed; present on `auth_failure` only | string (closed set) |
| `audit_schema_version` | The schema version, for query pinning | string (`1.0`) |

**`audit_event_type`** is a closed set: `operation` (a state-changing tool call),
`auth_success`, `auth_failure`, `broker_auth_retry`, and `audit_drop`. `tool`, `broker`,
`outcome`, and `arguments_hash` are present on `operation` records; the authentication types
carry the fields listed under [Authentication events](#authentication-events) instead.

**`principal` identity.** The audit event records only `principal.sub`, the opaque OIDC
subject of the human user. That subject is read once from the verified token and carried end
to end through token exchange, so the broker's own SEMP log records the same user. The full
claim set propagated for that exchange is `sub`, `scope`, `client_id`, `iss`, `jti`; of these,
only `sub` is written to the audit event. A human-readable username (`preferred_username`) is
**under review** as an open item, because it is PII that would land in an immutable audit
store (see open items).

**`arguments_hash`.** SHA-256 (FIPS 180-4) over an RFC 8785 JSON Canonicalization Scheme
form of the arguments (keys sorted, insignificant whitespace removed, nulls preserved). The
hash is deterministic, so an auditor can recompute it from the same arguments to prove a
recorded event corresponds to a specific call, without the raw argument values ever being
stored.

### Authentication events

Alongside destructive-operation events, the audit stream records authentication lifecycle
events. The names below are distinct **event types**, not values of the shared `outcome`
field:

- `auth_success`, carrying `principal` and `agent_client_id`.
- `auth_failure`, carrying `reason` (same closed set as `mcp_auth_failure_total`).
- `broker_auth_retry`, carrying `broker`, for a broker-side 401 and cookie-clear retry.

This keeps failed **authentication** a distinct, queryable signal rather than folding it
into a generic error, so a query like "show me every rejected credential" stays clean.

**Authorization is a different question, and the schema does not yet answer it.** These
event types cover authentication only: who proved who they were, and who failed to. A
caller who authenticates successfully and is then refused a privileged operation currently
lands in an `operation` record with `outcome: error`, indistinguishable from a broker
timeout or a malformed argument. There is no `denied` outcome and no authorization event
type. If your access reviews need "show me every denied privileged attempt" as a clean
query, say so in your feedback — this is the kind of gap the pilot is meant to catch, and
it is cheaper to add a value now than after the GA freeze.

### Audit delivery

Delivery is **non-blocking by design**: writing an audit event never stalls or fails the
broker operation. The event rides the server's structured JSON log stream on stderr, tagged
`"event": "audit"`; your log shipper filters on the tag and routes it.

If the local sink backpressures, the event is **dropped rather than buffered**, and every
drop is counted in `mcp_audit_events_dropped_total` and recorded as a JSON record carrying
`"event": "audit"` and `"audit_event_type": "audit_drop"`, so a gap is visible, never
silent.

`"event"` stays constant at `"audit"` on every record, drops included, precisely so the one
filter your shipper routes on cannot miss them — a drop notice that fell outside the audit
stream would go unseen in exactly the situation it exists to report. Discriminate record
kinds on `audit_event_type`, never on `event`.

For guaranteed delivery, route the tagged stream to an acknowledged sink on your side (TCP
syslog per RFC 5424, or a SIEM ingest endpoint such as Splunk HEC with indexer
acknowledgement). **Retention and tamper-evident storage are properties of that destination,
which you own.** The server does not itself persist or sign events.

---

## Distributed tracing

OpenTelemetry spans at each hop of a request, exported over OTLP, enabled with
`OBS_TRACING_ENABLED` (never automatic; you opt in after deploying a collector).

- **Export protocol:** OTLP over gRPC. Endpoint set via the standard
  `OTEL_EXPORTER_OTLP_ENDPOINT`.
- **Sampling:** honors the standard OpenTelemetry SDK environment contract,
  `OTEL_TRACES_SAMPLER` (default `parentbased_traceidratio`) and `OTEL_TRACES_SAMPLER_ARG`.
  A child of a sampled upstream span is sampled, so a trace started by your AI agent
  continues unbroken through the server into the broker.
- **Propagation:** W3C Trace Context. When an inbound `traceparent` header is present, the
  server's entry span is a child of your agent's span; when absent, it starts a new root.

### Spans

A successful end-to-end call produces spans at the HTTP boundary, the tool dispatcher, the
composite executor, and each SEMP attempt. Named spans:

- `semp.attempt`: one per SEMP request attempt.

Other span names follow the OpenTelemetry HTTP semantic conventions where applicable. Span
names beyond `semp.attempt`, and span kinds, are open items in this review (see below).

### Span attributes

| Attribute | Meaning | Basis |
|---|---|---|
| `correlation_id` | The shared request ID, joining the trace to logs and audit | Solace |
| `retry.decision` | The retry decision on a SEMP attempt | Solace |
| `retry.exhausted` | `true` on the final attempt when retries are exhausted | Solace |

### Resource attributes

Set on all spans from server configuration. `service.name`, `service.version`, and
`deployment.environment` follow the OpenTelemetry resource semantic conventions
(https://opentelemetry.io/docs/specs/semconv/resource/). `region` (when configured) is
Solace-specific; OTel's convention is `cloud.region`, which we may adopt instead based on
pilot feedback.

---

## Correlation ID

One ID threads a request from the AI agent, through the server and every retry, out to the
broker, and back. Today it anchors your logs and the broker's own log entry on the same
call; once traces and the audit trail ship, it is the key that joins those to them.

This section describes behaviour that is implemented and on by default, unlike the metric,
audit, and trace schemas above.

- **Inbound**, in priority order: the W3C `traceparent` header (its trace-id is used); then
  a legacy `X-Correlation-ID` header; otherwise the server generates a time-sortable UUIDv7.
- **Returned** to the caller on the response `X-Correlation-ID` header and in
  `CallToolResult.Meta["correlation_id"]`.
- **Propagated** to the broker: every outbound SEMP request carries `X-Correlation-ID`.
  `traceparent` is sent only when the ID is a valid W3C trace-id (32 lowercase hex, not
  all-zero) — that is, when the caller supplied one inbound. A server-generated UUIDv7,
  which is the default when no caller header arrives, is not a valid trace-id, so no
  `traceparent` goes out rather than a malformed one. On retry the same ID is reused, so
  all attempts share one ID in the broker's logs.
- **Logged** as the `correlation_id` attribute on every log line within the request.

---

## The outcome vocabulary

A single `outcome` vocabulary is shared across metrics, the audit trail, and traces, so the
same call reads the same way in all three and you can join on one key.

| Value | Meaning |
|---|---|
| `success` | The call completed successfully. |
| `error` | The call failed. A `context.DeadlineExceeded` timeout is classified here. |
| `panic` | An unexpected failure was caught by the recovery layer and returned as a clean error. |
| `cancelled` | The caller cancelled the request. **Reserved in the schema now; emitted from a later release.** |

Notes:

- The metric label is `outcome`, not `status`, precisely so metrics, audit, and spans share
  one join key.
- **Failed authentication is not an `outcome` value.** It is a separate signal: the
  `auth_failure` audit event and the `mcp_auth_failure_total` metric, whose closed `reason`
  set (`invalid_token`, `expired`, `audience_mismatch`, `signature_invalid`, `missing`) is
  authentication throughout. This keeps security queries clean.
- **Authorization denials have no signal of their own yet.** A caller who authenticates and
  is then refused a privileged operation currently lands in `outcome: error`, alongside
  timeouts and malformed arguments. There is no `denied` value and no authorization event
  type. See [Authentication events](#authentication-events); tell us if your access reviews
  need this separated.
- **Load-shedding / saturation is not an `outcome` value** either; it is planned as a
  separate metric in a later release (see below).

---

## Open items for this review

These are the decisions we most want pilot input on. They are deliberately unresolved in
this draft, resolving them is the point of the review.

1. **SEMP duration histogram.** We propose the name `mcp_semp_request_duration_seconds` and
   the same buckets as the tool histogram. Do those bucket boundaries fit your broker's
   latency profile under stress? Bucket boundaries are effectively unchangeable after the
   freeze, so this is the highest-value thing to check.
2. **`server_address` vs `broker` on SEMP metrics.** We carry both: `server_address` is the
   OTel-conventional host label, `broker` is your configured alias. Is carrying both useful,
   or redundant for your dashboards?
3. **`principal.preferred_username`.** Should the audit event carry a human-readable
   username in addition to the opaque `sub`? It improves readability for access reviews but
   places PII in an immutable store. We currently omit it pending this decision.
4. **Trace span names and span kinds.** Beyond `semp.attempt`, we intend to follow OTel HTTP
   conventions. If your trace backend or trace-based SLOs key off specific span names or
   `SpanKind` values, tell us what you expect.
5. **`outcome` vocabulary.** Does `success` / `error` / `panic` / `cancelled` cover what
   your SIEM queries distinguish, or do you split these differently (for example a separate
   `timeout`)?

---

## Planned for a later release (not frozen in this review)

The following are on the roadmap and **not part of this freeze**. Names are indicative and
will get their own review before they ship.

- Load and saturation visibility (a `mcp_saturation_total` metric and a rate-limiter health
  gauge), so operators can see when the server sheds load to protect a broker.
- Broker connection-pool gauges.
- A SEMP retry-outcome counter.
- Cancellation and progress signals (which populate the reserved `cancelled` outcome).

---

## Standards this schema supports

The audit and metrics surfaces are designed to map to the logging and monitoring
requirements of PCI DSS Requirement 10, SOC 2 (CC7.2 / CC7.3), SOX Section 404, and
ISO/IEC 27001 Annex A.8.15 / A.8.16 / A.8.17. Note that log **integrity** (tamper-evidence)
and **retention** are properties of the SIEM destination you route the audit stream to, not
of the server (see [Audit delivery](#audit-delivery)).

---

*This draft corresponds to milestone SOL-150251 (Broker MCP Server observability) and is the
artifact referenced by the observability preview brief. It will be finalized and version-frozen
at GA.*
