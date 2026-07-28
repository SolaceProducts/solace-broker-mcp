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
> [Correlation ID](#correlation-id--implemented) section describes shipped behavior you can rely on
> today.

The Broker MCP Server is designed to emit three observability signals:

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

### Implementation status

This schema is published **ahead of the code** so the names can be reviewed before they
freeze at GA. Each capability is tagged with its status as of this draft, and the capability
headings below carry the same tag:

| Capability | Status | Notes |
|---|---|---|
| Correlation ID | **[Implemented]** | Wired and on by default (`OBS_CORRELATION_ID_ENABLED`). |
| Metrics | **[Planned]** | The `/metrics` endpoint and instruments are not yet wired; the names and labels here are the proposal under review. |
| Audit trail | **[Planned]** | Only the capability gate exists today; event emission lands in a later story. |
| Distributed tracing | **[Planned]** | OTLP export is not yet wired. |

Present-tense wording in a **[Planned]** section describes the **target** behavior under
review, not what the current build emits. Only the **[Implemented]** capability is live today.

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
| Cardinality | Every metric name and label key is documented here. A CI check that fails the build on any undocumented name or label key is planned for GA; today the catalog is maintained by review. Label values are drawn from finite domains (configured brokers, SEMP operations, HTTP status codes, the retry cap), so series cardinality stays bounded. No label carries a free-text or unbounded value. |
| Redaction | Credentials, tokens, and raw tool arguments are never written to any signal. |

### Schema versioning

Two independent versions are published, so your queries can pin to a version and detect drift:

- `metrics_schema` (current: **1.0**), surfaced by the `mcp_schema_version` metric.
- `audit_schema` (current: **1.0**), surfaced as the `audit_schema_version` field on every audit
  event **and** as a label on `mcp_schema_version`, so both versions are discoverable from a
  scrape without ingesting audit events.

Versioning is `MAJOR.MINOR`. A **minor** bump is additive and backward compatible (a new
metric, a new audit field, a new `outcome` value). A **major** bump is reserved for a
breaking change (a rename or removal), which the additive-only commitment is designed to
avoid. Pin dashboards to `mcp_schema_version` and SIEM queries to `audit_schema_version`.

---

## Metrics — [Planned]

> _Status: **[Planned]**. The `/metrics` endpoint and instruments are not yet wired in the
> build; the names, types, and labels below are the proposal under review._

All metrics are served on the `/metrics` endpoint in Prometheus text exposition
format, behind `OBS_METRICS_ENABLED`. One exception: the authentication-failure counter
(`mcp_auth_failure_total`) has its own flag, `OBS_AUTH_FAILURE_COUNTER_ENABLED`. It defaults
to whatever `OBS_METRICS_ENABLED` is, but can be set independently, so a security team can
collect auth-failure signal without turning on the full metrics surface.

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
| `mcp_tool_invocation_total` | Counter | `tool`, `broker`, `outcome`, `error_type` | Solace |
| `mcp_tool_invocation_duration_seconds` | Histogram | `tool`, `broker`, `outcome`, `error_type` | Solace |

- `tool`: the MCP tool name (kebab-case, for example `get-broker-status`). Bounded by the
  number of tools the server exposes.
- `broker`: the broker alias from your configuration. Bounded by the number of configured
  brokers.
- `outcome`: see [The outcome vocabulary](#the-outcome-vocabulary).
- `error_type`: the failure cause, from the ten values in
  [`error_type`](#error_type). Empty on any non-error outcome.
- Histogram buckets (seconds): `0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10`.

**Cardinality:** `error_type` is non-empty only on the error path, so the series per `tool` and
`broker` is bounded at `success (1) + cancelled (1) + error x 10 = 12`, not the 33 a naive
product of the two label domains would suggest. That 12 is the worst case once every outcome
is emitted; until `cancelled` ships (see [The outcome vocabulary](#the-outcome-vocabulary)) you
will observe 11. All domains are finite (CI enforcement planned for GA).

### SEMP requests (RED, per attempt)

Request rate, errors, and latency for each call the server makes to a broker over SEMP,
recorded per retry attempt so you can see retry storms and per-broker latency.

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_semp_request_total` | Counter | `http_request_method`, `http_response_status_code`, `server_address`, `broker`, `api`, `operation`, `attempt` | Mixed (see below) |
| `mcp_semp_request_duration_seconds` | Histogram | same label set | Mixed |

- **OTel** labels (adopted from the OpenTelemetry HTTP semantic conventions,
  https://opentelemetry.io/docs/specs/semconv/http/http-spans/): `http_request_method`,
  `http_response_status_code`, `server_address`.
- **Solace** labels: `broker` (the configured alias), `api` (`v1` or `v2`), `operation`
  (the SEMP operation), `attempt` (the retry attempt as an integer string, `"1"`, `"2"`, ...).
- **When no response arrives** — DNS failure, connection refused, TLS handshake failure,
  or a timeout — `http_response_status_code` is the **empty string**. The attempt is still
  counted; only the status is unknown. This follows the OTel convention of leaving the
  attribute unset when there is no response, which in Prometheus surfaces as `""` rather
  than an absent label. Alert on `http_response_status_code=""` to catch a broker you
  cannot reach at all, which no status-code range would match.
- Histogram buckets (seconds): `0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10`.
  **Deliberately coarser at the low end than the tool histogram**, because a SEMP call is a
  network round-trip to a broker and sub-millisecond resolution would buy nothing.

**Cardinality:** bounded by the product of these finite sets. `attempt` is bounded by the
retry cap, and the empty status adds one value to that dimension rather than an open set.
See open items for the two decisions we want your input on here: whether the histogram bucket
boundaries fit your brokers under stress, and whether a bare empty status is enough for the
no-response case or you need the reason (DNS, TLS, timeout) as a label.
`mcp_broker_unreachable_reason` carries a coarser version of that reason today, but only as
broker state, not per attempt.

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

### OTLP export health

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_otel_spans_exported_total` | Counter | none | Solace |
| `mcp_otel_spans_dropped_total` | Counter | `reason` | Solace |
| `mcp_otel_metrics_exported_total` | Counter | none | Solace |
| `mcp_otel_metrics_dropped_total` | Counter | `reason` | Solace |

Self-observation for the two OTLP exporters: the span pair when tracing is enabled
(`OBS_TRACING_ENABLED`), the metric pair when OTLP metrics push is enabled. `reason` is a
closed set on both: `queue_full`, `export_timeout`, `export_error`, `shutdown`.

**How OTLP metrics push is turned on is not settled** — see open item 7. `OBS_METRICS_ENABLED`
governs the scrape surface, and there is no OBS_* flag for the push path today. Until that is
decided, treat `mcp_otel_metrics_exported_total` and `mcp_otel_metrics_dropped_total` as
present only once push ships.

**These live on the scrape surface deliberately.** Diagnosing a broken push must not depend on
the push working, so you can answer "is our OTLP export landing?" from Prometheus even when the
collector is the thing that is down. The scrape path and the push path fail independently by
design.

### Trace exemplars

The two latency histograms (`mcp_tool_invocation_duration_seconds` and
`mcp_semp_request_duration_seconds`) carry **trace exemplars** when both metrics and tracing are
enabled, so a slow bucket on a Grafana panel links straight to the trace that produced it and
you skip correlating by timestamp.

Two things to know, because both look like bugs otherwise:

- **Your Prometheus must negotiate OpenMetrics to receive them.** Exemplars are not part of the
  older Prometheus text exposition format. Recent Prometheus versions request OpenMetrics by
  default; if yours does not, exemplars will be silently absent from an otherwise healthy
  scrape.
- **An exemplar can only point at a *sampled* trace.** Under a low `OTEL_TRACES_SAMPLER_ARG`
  most buckets carry no exemplar. That is expected, not a gap.

Exemplars add no new label keys and no new series.

### Go runtime and process metrics

Standard `go_*` and `process_*` collectors from the Prometheus Go client library
(`collectors.NewGoCollector()` and `collectors.NewProcessCollector()`): goroutine count,
garbage-collection timing, memory stats, file descriptors, CPU. These names are upstream
Prometheus conventions, not Solace-defined, and are listed here only so you know they will be
present, once the metrics endpoint is wired, for diagnosing memory pressure and goroutine leaks.

---

## Audit trail — [Planned]

> _Status: **[Planned]**. Only the capability gate exists today; audit-event emission lands in
> a later story. The fields and delivery behavior below are the proposed schema._

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
| `started_at_utc` | When the call began | RFC 3339 UTC |
| `duration_ms` | How long the call took | integer (ms) |
| `principal.sub` | The authenticated human user (the OIDC `sub` claim) | string |
| `agent_client_id` | Which AI agent or client made the call, distinct from the human user | string |
| `tool` | The MCP tool invoked | string |
| `broker` | The broker targeted | string |
| `outcome` | The result; see [The outcome vocabulary](#the-outcome-vocabulary) | string |
| `error_type` | Why an operation failed; present on `outcome: error` only | string (closed set) |
| `arguments_hash` | SHA-256 over an RFC 8785 (JCS) canonicalization of the call arguments | hex string |
| `correlation_id` | Join key to logs, traces, and the broker-side entry | string |
| `reason` | Why a credential was rejected; present on `auth_failure` only | string (closed set) |
| `audit_schema_version` | The schema version, for query pinning | string (`1.0`) |

**`audit_event_type`** is a closed set of five: `operation` (a state-changing tool call),
`auth_success`, `auth_failure`, `broker_auth_retry`, and `audit_drop`.

**Which fields appear on which record.** Not every field is on every record, so a SIEM author
can tell record kinds apart from field presence alone. `event`, `audit_event_type`,
`timestamp_utc`, `correlation_id`, and `audit_schema_version` are on all five.

| `audit_event_type` | `outcome` | `error_type` | `reason` | `tool`, `arguments_hash`, `started_at_utc`, `duration_ms` | `broker` | `principal.sub`, `agent_client_id` |
|---|---|---|---|---|---|---|
| `operation` | yes | on `error` only | — | yes | yes | yes |
| `auth_success` | — | — | — | — | — | yes |
| `auth_failure` | — | — | yes | — | — | see below |
| `broker_auth_retry` | `success` or `error` | — | — | — | yes | yes |
| `audit_drop` | — | — | — | — | — | — |

- **`auth_success` and `auth_failure` carry no `outcome`.** The record type already says what
  happened, so one predicate does the job of two.
- **On `auth_failure` the principal is unknown by definition**, since authentication is what
  failed. `principal.sub` and `agent_client_id` appear only when the token parsed far enough to
  yield them: an expired or audience-mismatched token will, a malformed or absent one will not.
- **`audit_drop` is a notice, not an outcome.** It reports that a record could not be written,
  and carries only the five common fields.

**Time fields carry their zone or unit in the name:** `_utc` for an instant, `_ms` for a
duration. Hence `timestamp_utc` and `started_at_utc` alongside `duration_ms`. Names freeze at
GA, so the rule is stated here for any field added before then.

**`principal` is a nested object, and `principal.sub` is a path into it** — not a literal key
with a dot in it. A record carries `"principal": { "sub": "..." }`. It is the schema's only
nested field; every other field is flat and snake_case. The nesting is deliberate: it leaves
room for a second member without renaming a field, which matters because `preferred_username`
is under review below. Collectors that flatten nested objects will render it as
`principal.sub` regardless, which is why the tables above use the dotted form.

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

## Distributed tracing — [Planned]

> _Status: **[Planned]**. OTLP export is not yet wired; the spans, attributes, and export
> protocol below are the proposed design._

OpenTelemetry spans at each hop of a request, exported over OTLP, enabled with
`OBS_TRACING_ENABLED` (never automatic; you opt in after deploying a collector).

- **Export protocol:** OTLP over gRPC. Endpoint set via the standard
  `OTEL_EXPORTER_OTLP_ENDPOINT`.
- **Sampling:** honors the standard OpenTelemetry SDK environment contract,
  `OTEL_TRACES_SAMPLER` and `OTEL_TRACES_SAMPLER_ARG`, and ships no default of its own. The
  SDK's default is `parentbased_always_on`, which exports every trace — set
  `parentbased_traceidratio` with a ratio in `OTEL_TRACES_SAMPLER_ARG` before pointing a busy
  server at a collector. A child of a sampled upstream span is sampled, so a trace started by
  your AI agent continues unbroken through the server into the broker.
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

Set from server configuration on **both** metrics and spans, so an aggregated dashboard can
tell instances apart without a label duplicated onto every series. All five follow the
OpenTelemetry resource semantic conventions
(https://opentelemetry.io/docs/specs/semconv/resource/).

| Attribute | Source |
|---|---|
| `service.name` | config |
| `service.version` | build-time injection |
| `service.instance.id` | config, or the pod name |
| `deployment.environment` | config, when set |
| `cloud.region` | config, when set |

An earlier draft called this attribute `region` and flagged the OTel name as a possible
change. **It is now `cloud.region`**: where OTel publishes a convention we adopt it, and
`cloud.region` is what a multi-broker aggregator expects to pivot on.

**How to query them, per egress.** These are resource attributes, not per-series labels, so
they arrive differently on each of the two metric egresses:

- **Prometheus scrape.** The exporter publishes them on the `target_info` series, with `.`
  translated to `_`, so they read as `service_name`, `cloud_region`, and so on. Join to it,
  for example
  `mcp_tool_invocation_total * on (instance, job) group_left(service_name, cloud_region) target_info`.
- **OTLP push.** Resource attributes are **not promoted to labels by default**. If you ingest
  our OTLP metrics straight into Prometheus, set `promote_resource_attributes` to include
  `service.name`, `service.instance.id`, `deployment.environment`, and `cloud.region`, or the
  same dashboard will show empty variable dropdowns.

---

## Correlation ID — [Implemented]

> _Status: **[Implemented]** and on by default (`OBS_CORRELATION_ID_ENABLED`)._

One ID threads a request from the AI agent, through the server and every retry, out to the
broker, and back. Today it anchors your logs and the broker's own log entry on the same
call; once traces and the audit trail ship, it is the key that joins those to them.

This section describes behavior that is implemented and on by default, unlike the metric,
audit, and trace schemas above.

- **Inbound**, in priority order: the W3C `traceparent` header (its trace-id is used); then
  a legacy `X-Correlation-ID` header; otherwise the server generates a time-sortable UUIDv7.
- **`X-Correlation-ID` acceptance.** An inbound value is trimmed of surrounding whitespace,
  then accepted only if it is non-empty, at most 128 characters, and printable ASCII
  (`0x21`–`0x7E`). Anything empty, longer, or carrying a control character (CR, LF, tab, NUL)
  is rejected, and the server falls back to generating a UUIDv7. The value is rejected rather
  than stripped, so a malformed or hostile ID never mutates into a different accepted one, and
  an accepted ID is safe to echo on the response header and write into logs, traces, and audit
  without escaping. A `traceparent` trace-id is always 32 hex characters, so the length cap
  only ever bites an oversized `X-Correlation-ID`.
- **Returned** to the caller on the response `X-Correlation-ID` header and in
  `CallToolResult.Meta["correlation_id"]`.
- **Propagated** to the broker: every outbound SEMP request carries `X-Correlation-ID`. It
  also carries a `traceparent`, with a fresh child span-id, only when the ID is a valid W3C
  trace-id: 32 lowercase hex characters, not all-zero, which in practice means it arrived on
  an inbound `traceparent`. A new child span per outbound hop is correct W3C behavior. For a
  server-generated UUIDv7 or a legacy `X-Correlation-ID` value, no `traceparent` is sent,
  since a non-conformant trace-id would be worse than none. On retry, the same ID is reused,
  so all attempts share one ID in the broker's logs.
- **Logged** as the `correlation_id` attribute on every log line within the request.

---

## The outcome vocabulary

A single `outcome` vocabulary is shared across metrics, the audit trail, and traces, so the
same call reads the same way in all three and you can join on one key.

`outcome` answers "what happened". A companion attribute, `error_type`, answers "why", and is
present only when `outcome` is `error`. Splitting the two keeps `outcome` small enough to group
by on a dashboard while still carrying the detail an investigation needs.

| Value | Meaning |
|---|---|
| `success` | The call completed successfully. |
| `error` | The call failed. The cause is in `error_type`. A `context.DeadlineExceeded` timeout is classified here. |
| `cancelled` | The caller cancelled the request. **Reserved in the schema now; emitted from a later release.** |

### `error_type`

Present only on `outcome: error`, drawn from a closed set of ten values:

| Value | Meaning |
|---|---|
| `panic` | An unexpected failure was caught by the recovery layer and returned as a clean error. |
| `unknown_tool` | The requested tool is not registered. |
| `missing_broker` | No broker was named on a call that requires one. |
| `unknown_broker` | The named broker is not configured. |
| `broker_init_error` | The broker is configured but could not be initialised. |
| `validation_error` | The arguments failed input validation. |
| `execution_error` | The tool ran and failed. |
| `nil_result` | The tool returned no result. |
| `output_validation_error` | The tool's output failed schema validation. |
| `marshal_error` | The result could not be serialised. |

Notes:

- The metric label is `outcome`, not `status`, precisely so metrics, audit, and spans share
  one join key. `error_type` follows the OTel semantic-convention pattern of pairing a small
  status with a separate `error.type` attribute, rendered `error_type` for Prometheus.
- **`panic` is an `error_type`, not an `outcome`.** A recovered panic reads
  `outcome=error`, `error_type=panic`. If you saw an earlier draft that listed `panic` as a
  fourth `outcome` value, this supersedes it.
- **Failed authentication is not an `outcome` value.** It is a separate signal: the
  `auth_failure` audit event and the `mcp_auth_failure_total` metric, whose closed `reason`
  set (`invalid_token`, `expired`, `audience_mismatch`, `signature_invalid`, `missing`) is
  authentication throughout. This keeps security queries clean.
- **Authorization denials have no signal of their own yet.** A caller who authenticates and
  is then refused a privileged operation currently lands in `outcome: error`, alongside
  timeouts and malformed arguments. There is no `denied` value and no authorization event
  type. This is an open item below, and it is the one we would most like a compliance
  reviewer's answer on.
- **Load-shedding / saturation is not an `outcome` value** either; it is planned as a
  separate metric in a later release (see below).

---

## Open items for this review

These are the decisions we most want pilot input on. They are deliberately unresolved in
this draft, resolving them is the point of the review.

1. **SEMP duration histogram buckets.** The name `mcp_semp_request_duration_seconds` is
   settled. The buckets are `0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10` seconds,
   deliberately coarser at the low end than the tool histogram because a SEMP call is a network
   round-trip. Do those boundaries fit your broker's latency profile under stress? Bucket
   boundaries are effectively unchangeable after the freeze, so this is the highest-value thing
   to check.
2. **The no-response case on SEMP metrics.** When an attempt fails before any response —
   DNS, connection refused, TLS, timeout — `http_response_status_code` is the empty string,
   following OTel. Is that enough to alert on, or do you need the reason as its own label?
   Splitting it out costs cardinality and would duplicate, per attempt, what
   `mcp_broker_unreachable_reason` already carries as broker state.
3. **`principal.preferred_username`.** Should the audit event carry a human-readable
   username in addition to the opaque `sub`? It improves readability for access reviews but
   places PII in an immutable store. We currently omit it pending this decision.
4. **Trace span names and span kinds.** Beyond `semp.attempt`, we intend to follow OTel HTTP
   conventions. If your trace backend or trace-based SLOs key off specific span names or
   `SpanKind` values, tell us what you expect.
5. **The `outcome` / `error_type` split.** We have settled on three `outcome` values with the
   cause in a separate `error_type` of ten values, rather than folding causes into `outcome`.
   Does that split match how your SIEM queries distinguish failures, and do the ten
   `error_type` values cover how you classify them? If you would separate something we have
   merged — a `timeout` distinct from other errors, say — now is the time.
6. **Authorization denials, which currently have no signal of their own.** A caller who
   authenticates successfully and is then refused a privileged operation lands in
   `outcome: error`, indistinguishable from a broker timeout or a malformed argument. There is
   no `denied` outcome value and no authorization event type. Do your access reviews need
   "show me every denied privileged attempt" as a clean query? Adding a value now is cheap;
   adding one after the freeze is not.
7. **How OTLP metrics push is enabled.** Tracing has `OBS_TRACING_ENABLED`; the push path for
   metrics has no flag yet, and the scrape surface's `OBS_METRICS_ENABLED` is the wrong lever
   since the two fail independently by design. Should push be its own OBS_* capability flag, or
   should it follow the standard `OTEL_EXPORTER_OTLP_*` variables your collectors already set?
   The second is less for us to document but puts one signal outside the OBS_* model the rest
   of this page uses.

### Decided since the first draft

Two items that appeared as open questions in the draft circulated on 2026-07-20 are now
settled, so you do not need to spend review time on them:

- **`server_address` and `broker` on SEMP metrics: we keep both.** They answer different
  questions. `server_address` is the OTel-conventional host, which is what correlates this
  service with everything else OTel-instrumented in your estate; `broker` is your configured
  alias, which is what dashboards and alerts group by. Neither is redundant.
- **`region` is now `cloud.region`.** See [Resource attributes](#resource-attributes).

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
