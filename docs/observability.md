# Observability Schema

> **Status: Draft for pilot review.** This document is the proposed metric, audit, and
> trace schema for the Broker MCP Server. It is published for review **before** the names
> freeze at GA. After GA we commit to only ever *adding* to this schema, never renaming, so
> the time to change a name is now. See [How to Give Feedback](#how-to-give-feedback).
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
>
> **Saturation visibility is a partial exception.** It ships as structured log lines, not
> as the metric described here, behind `OBS_SATURATION_EVENTS_ENABLED` (default off). See
> [Load and Saturation Visibility](#load-and-saturation-visibility--interim--logs-only).
> The metric form remains roadmap.

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

### Implementation Status

This schema is published **ahead of the code** so the names can be reviewed before they
freeze at GA. Each capability is tagged with its status as of this draft, and the following
capability headings carry the same tag:

| Capability | Status | Notes |
|---|---|---|
| Correlation ID | **[Implemented]** | Wired and on by default (`OBS_CORRELATION_ID_ENABLED`). |
| Metrics | **[Planned]** | The `/metrics` endpoint and instruments are not yet wired; the names and labels here are the proposal under review. |
| Audit trail | **[Planned]** | Only the capability gate exists today; event emission lands in a later story. |
| Distributed tracing | **[Interim — provider wired, no spans yet]** | Tracer provider and OTLP export are live behind `OBS_TRACING_ENABLED`; no code creates a span yet. See [Distributed Tracing](#distributed-tracing--interim-provider-wired-spans-not-yet-emitted). |
| Saturation visibility | **[Interim — logs only]** | Shipped as structured log lines behind `OBS_SATURATION_EVENTS_ENABLED`, **not** as the metric this schema describes. See [Load and Saturation Visibility](#load-and-saturation-visibility--interim--logs-only). |

Present-tense wording in a **[Planned]** section describes the **target** behavior under
review, not what the current build emits. Only the **[Implemented]** capability is live today.

---

## How to Give Feedback

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
| Naming basis | Where OpenTelemetry publishes a semantic convention, we adopt it and translate `.` to `_` for Prometheus (for example `http.request.method` becomes `http_request_method`). Where OTel has no convention, we use a documented Solace-specific name. Each name in this document is tagged **OTel** or **Solace**. |
| Cardinality | Every metric name and label key is documented here. A CI check that fails the build on any undocumented name or label key is planned for GA; today the catalog is maintained by review. Label values are drawn from finite domains (configured brokers, SEMP operations, HTTP status codes, the retry cap), so series cardinality stays bounded. No label carries a free-text or unbounded value. |
| Redaction | Credentials, tokens, and raw tool arguments are never written to any signal. |

### Schema Versioning

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
> build; the following names, types, and labels are the proposal under review._

All metrics are served on the `/metrics` endpoint in Prometheus text exposition
format, behind `OBS_METRICS_ENABLED`. One exception: whether the authentication-failure
counter (`mcp_auth_failure_total`) is recorded has its own flag,
`OBS_AUTH_FAILURE_COUNTER_ENABLED`. It defaults to whatever `OBS_METRICS_ENABLED` is, but an
operator can set it independently, so the counter can be suppressed while the rest of the
surface is on, or kept while the rest is off. The flag governs recording only. What is
exposed when it is forced on while `OBS_METRICS_ENABLED` is false is a property of the
`/metrics` endpoint and is settled when that endpoint is wired, not by this schema.

The same instruments can additionally be **pushed over OTLP**, behind its own flag,
`OBS_METRICS_OTLP_ENABLED`. The endpoint comes from the standard
`OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`. Push is off by
default and does not activate merely because an endpoint variable is present in the
environment; see [Decided Since the First Draft](#decided-since-the-first-draft) for why.
Setting `OBS_METRICS_OTLP_ENABLED=true` while `OBS_METRICS_ENABLED` is false fails config
load with an explicit error rather than emitting nothing quietly, because both egresses
share one meter provider.

### Server and Scrape Health

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

### Tool Invocations (RED)

The core Rate / Errors / Duration signal for every tool call that reaches its handler. A call
refused by tool authorization never reaches one, so it is absent here and counted by
`mcp_authz_denied_total` instead.

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_tool_invocation_total` | Counter | `tool`, `broker`, `outcome`, `error_type` | Solace |
| `mcp_tool_invocation_duration_seconds` | Histogram | `tool`, `broker`, `outcome`, `error_type` | Solace |

- `tool`: the MCP tool name (kebab-case, for example `get-broker-status`). Bounded by the
  number of tools the server exposes.
- `broker`: the broker alias from your configuration. Bounded by the number of configured
  brokers.
- `outcome`: see [The Outcome Vocabulary](#the-outcome-vocabulary).
- `error_type`: the failure cause, from the ten values in
  [`error_type`](#error_type). Empty on any non-error outcome.
- Histogram buckets (seconds): `0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10`.

**Cardinality:** `error_type` is non-empty only on the error path, so the series per `tool` and
`broker` is bounded at `success (1) + cancelled (1) + error x 10 = 12`, not the 33 a naive
product of the two label domains would suggest. That 12 is the worst case once every outcome
is emitted; until `cancelled` ships (see [The Outcome Vocabulary](#the-outcome-vocabulary)) you
will observe 11. All domains are finite (CI enforcement planned for GA).

### SEMP Requests (RED, per Attempt)

Request rate, errors, and latency for each call the server makes to a broker over SEMP,
recorded per retry attempt so you can see retry storms and per-broker latency.

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_semp_request_total` | Counter | `http_request_method`, `http_response_status_code`, `server_address`, `broker`, `api`, `operation`, `attempt` | Mixed (see the following list) |
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

### Broker Reachability

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

### Authentication Failures

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_auth_failure_total` | Counter | `reason` | Solace |

- `reason` is a closed set: `invalid_token`, `expired`, `audience_mismatch`,
  `signature_invalid` (a token-signing or JWKS-rotation failure, distinct from a malformed
  token), `missing`.
- The values are deliberately coarse so no token content is ever exposed as a label.
- There is no `broker` label. Authentication happens at the HTTP boundary, before any broker
  is selected, so there is no broker in scope to name. Use the resource attributes on
  `target_info` to attribute failures to a server instance.

**Cardinality:** `|reason|` (five values).

### Authorization Denials

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_authz_denied_total` | Counter | `tool`, `reason` | Solace |

- `reason` is a closed set of two: `missing_claim`, `not_permitted`. Same values as the
  `authz_denied` audit record, taken from the same decision, so the metric and the audit
  stream cannot disagree.
- `tool` **is** a label here, unlike on `mcp_auth_failure_total`. Authorization runs after the
  tool is known, so the tool name is in scope and is the first thing you need on a denial.
- Emitted only where tool authorization is enabled. With it off, the series is absent rather
  than zero.

**Cardinality:** `|tool| x 2`.

### Audit Pipeline Health

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_audit_events_dropped_total` | Counter | none | Solace |

Increments if an audit event cannot be written (see [Audit Delivery](#audit-delivery)). A
flat-zero series is your evidence that no audit event was lost. Alert on any increase.

### OTLP Export Health

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_otel_spans_exported_total` | Counter | none | Solace |
| `mcp_otel_spans_dropped_total` | Counter | `reason` | Solace |
| `mcp_otel_metrics_exported_total` | Counter | none | Solace |
| `mcp_otel_metrics_dropped_total` | Counter | `reason` | Solace |

Self-observation for the two OTLP exporters, but the span pair's reach depends on **both**
flags, not just one. The counters are always registered in-process while tracing is enabled
(`OBS_TRACING_ENABLED`); they reach this scrape surface only when a meter provider also exists
to register them against, i.e. only when `OBS_METRICS_ENABLED` is **also** on. Tracing on with
metrics off keeps the totals in-process only — reported solely by the periodic
`event=otel_self_stats` INFO log (see [Distributed
Tracing](#distributed-tracing--interim-provider-wired-spans-not-yet-emitted)) — so an alert on
`mcp_otel_spans_dropped_total` sees a permanently absent series in that mode, which reads as
healthy rather than as "not exposed here." The metric pair's own flag is OTLP metrics push
(`OBS_METRICS_OTLP_ENABLED`, not `OBS_METRICS_ENABLED`, which governs the scrape surface alone;
see [Metrics](#metrics--planned)). `reason` is a closed set on both: `queue_full`,
`export_timeout`, `export_error`, `shutdown`.

**`queue_full` is reserved but currently inert on the span pair** (SOL-152420): the OTel Go
SDK's batch span processor drops queue-overflow spans against an internal counter with no
public accessor, so there is no supported way to surface that specific reason from outside the
SDK today. The value stays in the schema for forward compatibility; do not alert on it as if it
were live. `export_timeout`, `export_error`, and `shutdown` are all live and distinguish real
causes: a gRPC-status timeout from the exporter, any other export failure (including a refused
connection), and an in-progress export that didn't finish flushing before shutdown's deadline,
respectively — `shutdown` counts one event per incomplete drain, not one per dropped span, since
the SDK doesn't report how many spans it failed to flush.

**These live on the scrape surface deliberately.** Diagnosing a broken push must not depend on
the push working, so you can answer "is our OTLP export landing?" from Prometheus even when the
collector is the thing that is down. The scrape path and the push path fail independently by
design.

### `otel self stats` — periodic, when metrics are off

The fallback for the span pair above when there is no meter provider to register it against —
tracing on, metrics off, **or** metrics configured but its provider failing to build; that
second case is why the trigger is "no meter provider", not simply `OBS_METRICS_ENABLED: false`.
With no `/metrics` surface to read span-export health from, this periodic `INFO` line is the
only signal.

Emitted every `observability.otel_self_stats_interval_s` (default `60`). One reading fires
immediately on startup, same as [`broker in-flight
occupancy`](#broker-in-flight-occupancy--periodic-per-broker), so turning tracing on
mid-incident does not cost a full interval of silence.

| Field | Meaning |
|---|---|
| `event` | Always `otel_self_stats` — filter on this, not on the message text. |
| `spans_exported_total` | Successfully exported so far. |
| `spans_dropped_queue_full_total` | Reserved, currently always `0` — see the `queue_full` note above; the SDK exposes no counter for this. |
| `spans_dropped_export_timeout_total` | A gRPC-status timeout from the exporter. |
| `spans_dropped_export_error_total` | Any other export failure, including a refused connection. |
| `spans_dropped_shutdown_total` | An in-progress export that didn't finish flushing before shutdown's deadline — one event per incomplete drain, not one per dropped span. |

These are the exact field names, not the metric names above: flattened per-reason fields
(`spans_dropped_export_timeout_total`), not a single `reason`-labelled field. A query built by
substituting the metric schema's label value into a field name (e.g. guessing
`spans_dropped_total{reason="export_timeout"}` has a log-line equivalent of the same shape)
matches nothing.

### Trace Exemplars

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

### Go Runtime and Process Metrics

Standard `go_*` and `process_*` collectors from the Prometheus Go client library
(`collectors.NewGoCollector()` and `collectors.NewProcessCollector()`): goroutine count,
garbage-collection timing, memory stats, file descriptors, CPU. These names are upstream
Prometheus conventions, not Solace-defined, and are listed here only so you know they will be
present, once the metrics endpoint is wired, for diagnosing memory pressure and goroutine leaks.

---

## Audit Trail — [Planned]

> _Status: **[Planned]**. Only the capability gate exists today; audit-event emission lands in
> a later story. The following fields and delivery behavior are the proposed schema._

One JSON event is emitted per **state-changing** operation (for example `disconnect-client`,
`delete-queue`, broker shutdown), at completion, with the outcome known. Read-only calls are
not audited. Authentication lifecycle events are also emitted (see [Authentication Events](#authentication-events)). The stream is
enabled with `OBS_AUDIT_LOG_ENABLED`.

Every event carries a top-level `"event": "audit"` tag so your log shipper can route the
audit sub-stream to a dedicated SIEM index.

### Event Fields

| Field | Meaning | Type |
|---|---|---|
| `event` | Routing tag, always `audit` | string |
| `audit_event_type` | Which kind of audit record this is; discriminate on this, not on `event` | string (closed set; see following paragraph) |
| `timestamp_utc` | When the event was recorded | RFC 3339 UTC |
| `started_at_utc` | When the call began | RFC 3339 UTC |
| `duration_ms` | How long the call took | integer (ms) |
| `principal.sub` | The authenticated human user (the OIDC `sub` claim) | string |
| `agent_client_id` | Which AI agent or client made the call, distinct from the human user | string |
| `tool` | The MCP tool invoked | string |
| `broker` | The broker targeted | string |
| `outcome` | The result; see [The Outcome Vocabulary](#the-outcome-vocabulary) | string |
| `error_type` | Why an operation failed; present on `outcome: error` only | string (closed set) |
| `arguments_hash` | SHA-256 over an RFC 8785 (JCS) canonicalization of the call arguments | hex string |
| `correlation_id` | Join key to logs, traces, and the broker-side entry | string |
| `reason` | Why authentication or authorization failed; present on `auth_failure` and `authz_denied` | string (closed set) |
| `audit_schema_version` | The schema version, for query pinning | string (`1.0`) |

**`audit_event_type`** is a closed set of six: `operation` (a state-changing tool call),
`auth_success`, `auth_failure`, `authz_denied`, `broker_auth_retry`, and `audit_drop`.

**Which fields appear on which record.** Not every field is on every record, so a SIEM author
can tell record kinds apart from field presence alone. `event`, `audit_event_type`,
`timestamp_utc`, `correlation_id`, and `audit_schema_version` are on all six.

| `audit_event_type` | `outcome` | `error_type` | `reason` | `tool`, `arguments_hash`, `started_at_utc`, `duration_ms` | `broker` | `principal.sub`, `agent_client_id` |
|---|---|---|---|---|---|---|
| `operation` | yes | on `error` only | — | yes | yes | yes |
| `auth_success` | — | — | — | — | — | yes |
| `auth_failure` | — | — | yes | — | — | see following note |
| `authz_denied` | — | — | yes | `tool` only | — | yes |
| `broker_auth_retry` | `success` or `error` | — | — | — | yes | yes |
| `audit_drop` | — | — | — | — | — | — |

- **`auth_success` and `auth_failure` carry no `outcome`.** The record type already says what
  happened, so one predicate does the job of two.
- **On `auth_failure` the principal is unknown by definition**, since authentication is what
  failed. `principal.sub` and `agent_client_id` appear only when the token parsed far enough to
  yield them: an expired or audience-mismatched token does, a malformed or absent one does not.
- **`audit_drop` is a notice, not an outcome.** It reports that a record could not be written,
  and carries only the five common fields.

**Time fields carry their zone or unit in the name:** `_utc` for an instant, `_ms` for a
duration. Hence `timestamp_utc` and `started_at_utc` alongside `duration_ms`. Names freeze at
GA, so the rule is stated here for any field added before then.

**`principal` is a nested object, and `principal.sub` is a path into it** — not a literal key
with a dot in it. A record carries `"principal": { "sub": "..." }`. It is the schema's only
nested field; every other field is flat and snake_case. The nesting is deliberate: it leaves
room for a second member without renaming a field, which is what makes deferring
`preferred_username` (open item 3) a reversible choice. Collectors that flatten nested
objects render it as `principal.sub` regardless, which is why the preceding tables use the
dotted form.

**`principal` identity.** The audit event records only `principal.sub`, the opaque OIDC
subject of the human user. That subject is read once from the verified token and carried end
to end through token exchange, so the broker's own SEMP log records the same user. The full
claim set propagated for that exchange is `sub`, `scope`, `client_id`, `iss`, `jti`; of these,
only `sub` is written to the audit event. A human-readable username (`preferred_username`) is
**deliberately omitted in v1**: it is directly identifying PII that would land in an
append-only store. Adding it later is a pure addition (see open item 3).

**`arguments_hash`.** SHA-256 (FIPS 180-4) over an RFC 8785 JSON Canonicalization Scheme
form of the arguments (keys sorted, insignificant whitespace removed, nulls preserved). The
hash is deterministic, so an auditor can recompute it from the same arguments to prove a
recorded event corresponds to a specific call, without the raw argument values ever being
stored.

### Authentication Events

Alongside destructive-operation events, the audit stream records authentication lifecycle
events. The following names are distinct **event types**, not values of the shared `outcome`
field:

- `auth_success`, carrying `principal` and `agent_client_id`.
- `auth_failure`, carrying `reason` (same closed set as `mcp_auth_failure_total`).
- `broker_auth_retry`, carrying `broker`, for a broker-side 401 and cookie-clear retry.

This keeps failed **authentication** a distinct, queryable signal rather than folding it
into a generic error, so a query like "show me every rejected credential" stays clean.

**Authorization has its own record type: `authz_denied`.** The three preceding event types cover
authentication, meaning who proved who they were and who failed to. Authorization is the
separate question of whether an authenticated caller was permitted the tool they asked for,
and a denial emits `audit_event_type: authz_denied` carrying `tool`, the principal, and
`reason`, drawn from a closed set of two:

| `reason` | Meaning |
|---|---|
| `missing_claim` | The token carried no groups claim, so no grant could match. Usually an IdP-side misconfiguration. |
| `not_permitted` | The caller's groups matched no grant for that tool. |

Like `auth_failure`, an `authz_denied` record carries no `outcome`: the record type already
says what happened. So "show me every denied privileged attempt" is a single predicate,
`audit_event_type: authz_denied`, exactly parallel to the authentication case.

**Two things to know when querying this.** A denied call produces **no `operation` record and
no entry in the tool-invocation metrics** — authorization runs before the instrumented handler,
so the attempt is recorded only as an `authz_denied` record. Do not read a missing `operation`
record as "no attempt was made". And these records exist only where tool authorization is
enabled; with it off, no authorization check runs and none are emitted.

The caller's actual group memberships are deliberately **not** recorded on a denial, as a
separation-of-duties measure. `reason` tells you why without disclosing the caller's
entitlements to whoever reads the audit stream.

### Audit Delivery

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

## Distributed Tracing — [Interim: provider and one span wired]

> _Status: **[Interim]** (SOL-152420, SOL-153333). The tracer provider, OTLP export, and
> self-observation counters are wired and live behind `OBS_TRACING_ENABLED` (Story 25). One
> application span exists today — `tokenexchange.Exchange`, wrapping the OAuth token-exchange
> call (Story 50) — described under [Spans](#spans) below. The HTTP-boundary, tool-dispatcher,
> composite-executor, and per-SEMP-attempt spans are still the proposed design (later stories),
> so flipping the flag today exports a resource, the token-exchange span, and nothing else yet.
> Sampling and propagation are live; the rest of this section stays **[Planned]** until those
> stories land. **A consequence of shipping only one span first:** the default sampler samples
> everything (see Sampling below), and until Story 26's SEMP-layer spans exist, most calls have
> no upstream span to attach to — so a token exchange not triggered from an already-sampled agent
> trace exports as its own single-span root trace, one per token exchange — cache hits included,
> since `tracer.Start` runs before the cache lookup — rather than nested inside a larger request
> trace. Cache hits are the large majority of exchanges, so budget from the *total* call rate, not
> a live-round-trip rate. Self-correcting once Story 26 lands; worth knowing before then if trace
> volume looks higher than the request volume suggests._

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

A successful end-to-end call will eventually produce spans at the HTTP boundary, the tool
dispatcher, the composite executor, and each SEMP attempt (Story 26, not yet landed). Named
spans today:

- `tokenexchange.Exchange`: one per call to the OAuth token exchange (Story 50, SOL-153333) —
  a cache hit, a singleflight follower, and the singleflight winner triggering a live IdP round
  trip each get their own span. Child of whichever span is active where
  `OAuthAuthenticator.AddAuth` is called (a future SEMP-per-attempt span once Story 26 lands;
  today, whatever the caller's own context carries).
- `semp.attempt`: one per SEMP request attempt (Story 26, not yet landed).

Other span names follow the OpenTelemetry HTTP semantic conventions where applicable. Span
names beyond the two above, and span kinds, are open items in this review (see
[Open Items for This Review](#open-items-for-this-review), item 4).

### Span Attributes

| Attribute | Meaning | Basis |
|---|---|---|
| `correlation_id` | The shared request ID, joining the trace to logs and audit | Solace |
| `outcome` | The result; the same three values used as a metric label and an audit field | Solace |
| `error_type` | Why the call failed; present on `outcome: error` only, same ten values | Solace |
| `retry.decision` | The retry decision on a SEMP attempt | Solace |
| `retry.exhausted` | `true` on the final attempt when retries are exhausted | Solace |
| `cache_hit` | `tokenexchange.Exchange` only: true when served from cache, false when a live IdP round trip was needed (or waited on). **Isolating actual live round trips needs `singleflight_role="winner"` too** — a follower also reports `cache_hit=false` despite doing no IdP work itself, so filtering on `cache_hit` alone counts one winner plus every follower waiting on it | Solace |
| `singleflight_role` | `tokenexchange.Exchange` only, absent on a cache hit: `winner` (this call ran the live IdP round trip) or `follower` (this call shared another's result) | Solace |
| `winner_trace_id` / `winner_span_id` | `tokenexchange.Exchange` only, present on a `follower` span only: the winner's own IDs, so an operator can pivot from a follower's span to the trace that actually did the IdP work. The follower span also carries a span `Link` to the same span | Solace |

`outcome` and `error_type` carry the **same vocabulary here as on the metric labels and the
audit record**, which is the point of a single vocabulary: filter a dashboard by
`error_type="broker_init_error"` and you can carry that predicate into the trace backend and
the SIEM unchanged, with no translation table.

**Exception: `tokenexchange.Exchange` never sets `error_type`, even on `outcome: error`.** The
ten-value `error_type` set above is scoped to tool-invocation outcomes and has no value
describing a token-exchange failure mode (rate-limited, circuit-open, retries-exhausted,
transport, request-build). The span still carries the actual cause via the span's recorded
exception event and its status (`codes.Error`), just not through this shared field. A future
story may extend the vocabulary or give token-exchange failures their own attribute; until then,
do not expect `error_type` on this span.

**On the span, the key is `error_type`, not the OTel-conventional `error.type`.** This is a
deliberate exception to the preceding naming rule, taken so the key is identical across metrics,
logs, audit, and spans and the four surfaces cannot disagree about why a call failed. The
values match OTel's `error.type` semantics. If your trace backend or trace-based SLOs key off
the dotted `error.type`, tell us in your feedback, because this is the kind of thing that is
cheap to change now and expensive after the freeze.

**Enabling `OBS_TRACING_ENABLED` exports authentication-event content to your collector.**
Every `tokenexchange.Exchange` span carries a correlation ID, a timestamp, and the outcome of
that authentication attempt — `cache_hit`, `singleflight_role`, and (on a follower)
`winner_trace_id` / `winner_span_id` besides — which now travels to wherever
`OTEL_EXPORTER_OTLP_ENDPOINT` points, a system that may sit outside this deployment's own
residency or audit-scope boundary. **A failed exchange exports more than that summary:** the
span's exception event carries the error text verbatim, and for a transport failure that
includes the IdP token endpoint's hostname and resolved network address (from the underlying
`*url.Error`) — infrastructure topology, not just an authentication outcome. Worth one line in
your own data-flow review before pointing this at a collector you don't operate.

### Resource Attributes

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
  same dashboard will show empty variable drop-down lists.

---

## Correlation ID — [Implemented]

> _Status: **[Implemented]** and on by default (`OBS_CORRELATION_ID_ENABLED`)._

One ID threads a request from the AI agent, through the server and every retry, out to the
broker, and back. Today it anchors your logs and the broker's own log entry on the same
call; once traces and the audit trail ship, it is the key that joins those to them.

This section describes behavior that is implemented and on by default, unlike the preceding
metric, audit, and trace schemas.

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

**Shared broker-token exchanges log under the initiating request's ID.** When several
concurrent requests need the same broker token, the server performs one exchange on behalf of
all of them, and the lines describing that work — the identity-provider request/response, the
cache write, retry exhaustion, the Retry-After gate, the audience-mismatch WARN, and the
recovered-panic ERROR — carry the correlation ID of the request that initiated it. Every request still logs its own `broker token exchange completed` line
under its own ID. So if a request's ID turns up no identity-provider lines, that request rode
an exchange another request started: pivot to the `broker` attribute plus the time window
around the request's own completion line. A failed exchange surfaces through each caller's own
error handling rather than a per-caller line from the exchange itself. Two lines never carry a
correlation ID by design: the circuit-breaker state-change WARN (a transition is the verdict
on a window of failures, not on any one request — filter on its `breaker` attribute instead)
and the startup configuration WARN, which runs before any request exists.

---

## The Outcome Vocabulary

A single `outcome` vocabulary is shared across metrics, the audit trail, and traces, so the
same call reads the same way in all three and you can join on one key.

`outcome` answers "what happened". A companion attribute, `error_type`, answers "why", and is
present only when `outcome` is `error` — except on the `tokenexchange.Exchange` span, which
never sets it (see [Span Attributes](#span-attributes)). Splitting the two keeps `outcome`
small enough to group by on a dashboard while still carrying the detail an investigation needs.

| Value | Meaning |
|---|---|
| `success` | The call completed successfully. |
| `error` | The call failed. The cause is in `error_type`, except on the `tokenexchange.Exchange` span, which never sets it — see [Span Attributes](#span-attributes). A `context.DeadlineExceeded` timeout is classified here. |
| `cancelled` | The caller cancelled the request. **Reserved in the schema now for tool-invocation `outcome`; emitted from a later release (Story 42, v1.x) — already emitted today on the `tokenexchange.Exchange` span, see the exception below.** |

**Exception — the token-exchange span (SOL-153333, Story 50) already emits `cancelled`, ahead of the tool-invocation level above.** That span classifies by *where* the error originated, not by Go error type alone: a caller's own context ending the call (`context.Canceled` **or** `context.DeadlineExceeded` — for example the SEMP retry budget in `internal/semp/resilience/sender.go` expiring while the exchange waits) is `cancelled`, while a `context.DeadlineExceeded` from the exchange's *own* internal retry-chain deadline is `error`, per the general rule above. Classifying every `DeadlineExceeded` as `error` regardless of source would misattribute a caller's own timeout to the exchange.

### `error_type`

Present only on `outcome: error`, drawn from a closed set of ten values:

| Value | Meaning |
|---|---|
| `panic` | An unexpected failure was caught by the recovery layer and returned as a clean error. |
| `unknown_tool` | The requested tool is not registered. |
| `missing_broker` | No broker was named on a call that requires one. |
| `unknown_broker` | The named broker is not configured. |
| `broker_init_error` | The broker is configured but could not be initialized. |
| `validation_error` | The arguments failed input validation. |
| `execution_error` | The tool ran and failed. |
| `nil_result` | The tool returned no result. |
| `output_validation_error` | The tool's output failed schema validation. |
| `marshal_error` | The result could not be serialized. |

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
- **Authorization denial is not an `outcome` value.** Like failed authentication, it is a
  separate signal: the `authz_denied` audit event and the `mcp_authz_denied_total` metric,
  whose closed `reason` set (`missing_claim`, `not_permitted`) is authorization throughout.
  A denied call produces no `operation` record at all.
- **Load-shedding / saturation is not an `outcome` value** either; it is a separate signal.
  It ships today as log lines (see
  [Load and Saturation Visibility](#load-and-saturation-visibility--interim--logs-only))
  and is planned as a metric in a later release (see
  [Planned for a Later Release](#planned-for-a-later-release-not-frozen-in-this-review)).

---

## Deployment Topology and Resource Policy — [Implemented]

Unlike the signal schemas above, this section describes the example manifests in
`deploy/kubernetes/` as they ship today. They are a starting point to copy and edit, not a
supported product surface. Note there is no `kustomization.yaml` in that directory, so
adapt them by editing your copies — or add a base of your own first if you want to layer
Kustomize overlays on top.

### Availability

The Deployment defaults to `replicas: 2`. A single replica goes fully dark during any rolling
update or node drain. Two *separate* mechanisms keep a pod serving, and it is worth being
precise about which covers what — because a PodDisruptionBudget does **not** govern rolling
updates:

| Disruption | Enforced by | Mechanism |
|---|---|---|
| Rolling update (`kubectl rollout restart`, image change) | Deployment controller | `strategy.rollingUpdate.maxUnavailable: 0` — the replacement reaches Ready before the pod it replaces retires |
| Node drain, cluster upgrade | Eviction API | `poddisruptionbudget.yaml` — `maxUnavailable: 1`, one eviction at a time |
| Node failure, OOM kill | nothing | Involuntary; bypasses both |

A rollout deletes pods directly through the ReplicaSet and never consults the PDB, so the
strategy is pinned explicitly rather than inherited. The default `maxUnavailable: 25%`
resolves to `0` only via `floor(0.25 × 2)` at exactly two replicas — too incidental to rest
the guarantee on.

The PDB uses `maxUnavailable: 1`, not `minAvailable: 1`. The two are identical at two
replicas, but `minAvailable: 1` against `replicas: 1` permits zero disruptions and hangs
every drain of that node indefinitely — a cluster-wide hazard created by an application
manifest, and one that outlives a `git revert`, since `kubectl apply -f` does not prune the
PDB object. `maxUnavailable: 1` degrades to a brief outage instead, so scaling down stays
safe and the replica count and the PDB no longer have to move in lockstep.

`topologySpreadConstraints` (`maxSkew: 1` over `kubernetes.io/hostname`,
`whenUnsatisfiable: ScheduleAnyway`) asks the scheduler to place the replicas on different
nodes. Without it `replicas: 2` is two pods in one failure domain, and node failure — the
involuntary case nothing protects — stays a full outage. It is best-effort by design so
single-node dev clusters still schedule both pods.

#### The PDB protects availability, not sessions

MCP sessions live in pod memory (see below), so **any** pod replacement takes its sessions
with it. A rolling update recycles both pods, so no session survives an upgrade: affinity
re-routes those clients onto a pod that never issued their session and they receive
`404 session not found`. The graceful-shutdown budget in `deployment.yaml` drains in-flight
*requests*; it cannot make session state portable.

Two replicas buy a Service endpoint that stays reachable throughout. They do not buy a
conversation that survives an upgrade. Clients re-initialize on the 404 — plan upgrades
accordingly.

### Session affinity is required above one replica

The MCP streamable-HTTP handler runs in the SDK's **stateful** mode: it issues an
`Mcp-Session-Id` on initialize and holds that session in process memory, per pod. A request
carrying a session ID that reaches a pod which did not issue it is answered
`404 session not found` — the SDK does not transparently re-initialize.

`service.yaml` therefore sets `sessionAffinity: ClientIP`, pinning a client to the pod
holding its session. **Do not scale beyond one replica without it.** Two constraints follow:

- `sessionAffinityConfig.clientIP.timeoutSeconds` (10800, 3h) must stay **above** the MCP
  session idle timeout, currently the compile-time constant
  `defaults.DefaultMCPSessionIdleTimeout` (2h). If the routing entry expired first,
  kube-proxy would re-route a still-live session onto another pod and produce exactly the 404
  the affinity prevents. The timeout is not operator-configurable today, so only a code change
  can invert this. Nothing enforces the relationship today — though it is checkable in
  principle: `gopkg.in/yaml.v3` is already a direct dependency, so a test could parse the
  manifest and compare it against the constant. That guard has not been written.
- **Service-level affinity does not survive an ingress, gateway, or mesh.** kube-proxy applies
  it on the ClusterIP path only. An Ingress or Gateway controller load-balances straight to pod
  IPs, never transiting the ClusterIP, so the field is ignored and the 404 returns in full —
  and that is the topology [Authentication](authentication.md) recommends for OAuth ("keep the
  Service `ClusterIP` and put the TLS-terminating ingress in front of it"). A service mesh
  sidecar bypasses it as well. Behind any of these, configuring stickiness at *that* layer is
  a required deployment step, and the hash key is not the obvious one — the session ID is
  wrong. [Authentication](authentication.md#session-routing-at-the-ingress-required-above-one-replica)
  § "Session Routing at the Ingress" is authoritative; `deploy/kubernetes/ingress.yaml.example`
  is the manifest.
- Where affinity *is* in effect it is keyed on source IP, so every client behind one NAT or
  egress gateway — including an in-cluster proxy, whose own pod IP is what gets hashed —
  lands on a single pod. Sessions stay correct, but the second replica takes no traffic and
  recycling the loaded pod drops every session at once.

### What else is per-pod

The session map is the most visible piece of per-process state, but it is not the only one, and
a second replica doubles all of them:

- **SEMP concurrency and pacing.** `semp.max_concurrent_per_broker` and `request_min_interval`
  are enforced per process, so the load a broker actually sees is `replicas ×` the configured
  value. At the example config's `max_concurrent_per_broker: 10`, two replicas can put 20
  concurrent SEMP requests on one broker — and that limit exists to protect the broker's
  management plane, which is shared with human operators and other tooling. Divide these by the
  replica count, or raise the replica count deliberately knowing the multiplier.
- **Retries and circuit breakers** trip independently per pod, so a broker brownout produces
  one retry storm per replica instead of one in total.
- **The broker token cache** starts cold on each pod, so token exchanges against the IdP also
  scale with the replica count.

None of this is wrong, but none of it is visible from one pod's logs either — and under
ClientIP affinity it means two clients can legitimately observe different behaviour from the
same deployment at the same moment.

### Resource requests and limits

The container sets `requests: cpu 100m / memory 128Mi` and `limits: memory 512Mi`. The
asymmetry is deliberate: **memory is capped, CPU is not.**

**No CPU limit.** A CPU limit is enforced by CFS throttling, which stalls the process at
burst even when the node has idle cores — and burst is the normal shape of this workload,
where a tool call fans out to SEMP and parses the response. `requests.cpu` guarantees this
pod's scheduling share under contention, so it stays served while neighbours are busy; a limit
would only add latency at the moments that matter most.

Be precise about what that guarantee covers, though: `requests` protects this pod *from*
others, not others *from* this pod. Dropping the limit is exactly what lets this container
burst into otherwise-idle cores — which is the intent — but on a shared cluster that is the
platform team's call, expressed as a `LimitRange` or `ResourceQuota` rather than here. Both
cut the other way too, and neither is visible from this repo: a namespace `ResourceQuota`
carrying a `limits.cpu` entry **rejects** a pod that omits one (the Deployment is admitted
and then no pods appear), and a `LimitRange` with a default CPU limit **silently re-injects**
the throttling this section argues against.

**A memory limit, because the failure modes are not symmetric.** Memory is incompressible:
an unbounded leak has no equivalent of "runs slower", it takes the node down with it. Capping
it makes an over-consuming pod the kernel's problem via OOM kill, after which the Deployment
replaces it. That division of labour is intentional — **memory eviction is Kubernetes' job,
not the application's.** In particular, **`/readyz` is not a memory-pressure signal**: it
reports the server's own initialization state and will happily return 200 from a pod moments
from being OOM-killed. Nothing in process is watching the heap, and nothing is meant to be.

The 512Mi ceiling over a 128Mi request leaves headroom for the in-process session map
described above plus buffered SEMP responses under concurrent tool calls.

Two caveats on that ceiling. There is no session-count cap — sessions are bounded only by the
2h idle timeout — so sustained growth ends in an OOM kill rather than backpressure. And
because both replicas run identical code against one client population, they approach the
limit together; OOM is involuntary, so neither the PDB nor the rollout strategy protects
against losing both. Until a session gauge exists (metrics are `[Planned]` above, and no
session metric appears in the proposed set), the signal to watch is the container's own
`container_memory_working_set_bytes` against its limit, via cAdvisor or `kubectl top pods`.

---

## Open Items for This Review

These are the decisions we most want pilot input on. Most are unresolved; where we have
taken a position, we say so and name what would change it. Resolving them is the point of
the review.

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
3. **`principal.preferred_username`.** **Decided for v1: we omit it.** The audit event carries
   the opaque `sub` only. A readable username helps access reviews, but it places directly
   identifying PII in an append-only store, which conflicts with erasure obligations under
   GDPR and PIPL. Adding the field later is a pure addition this schema permits; removing it
   later would need a major version. So we have taken the reversible option.
   **What we still want from you:** can your access review resolve `sub` to a human at review
   time, including for a deprovisioned user? If it cannot, say so and we will add
   `principal.preferred_username` in a later minor.
4. **Trace span names and span kinds.** Beyond `tokenexchange.Exchange` and `semp.attempt`, we
   intend to follow OTel HTTP conventions. If your trace backend or trace-based SLOs key off
   specific span names or `SpanKind` values, tell us what you expect.
5. **The `outcome` / `error_type` split.** We have settled on three `outcome` values with the
   cause in a separate `error_type` of ten values, rather than folding causes into `outcome`.
   Does that split match how your SIEM queries distinguish failures, and do the ten
   `error_type` values cover how you classify them? If you would separate something we have
   merged — a `timeout` distinct from other errors, say — now is the time.
6. **Authorization denials — the signal is decided, the vocabulary is what we want checked.**
   Denials get their own record, `audit_event_type: authz_denied`, with `reason` drawn
   from `missing_claim` and `not_permitted`, plus a matching
   `mcp_authz_denied_total{tool,reason}` counter. Does that two-value `reason` set match how
   your access reviews classify a refusal, or do you distinguish cases we have merged? And is
   the single-predicate query the shape you need? One caveat worth knowing: no shipped build
   emits this record yet, so denial history begins at the release that first does and cannot
   be back-filled.
### Decided Since the First Draft

Three items that appeared as open questions in earlier drafts are now settled, so you do not
need to spend review time on them:

- **`server_address` and `broker` on SEMP metrics: we keep both.** They answer different
  questions. `server_address` is the OTel-conventional host, which is what correlates this
  service with everything else OTel-instrumented in your estate; `broker` is your configured
  alias, which is what dashboards and alerts group by. Neither is redundant.
- **`region` is now `cloud.region`.** See [Resource Attributes](#resource-attributes).
- **OTLP metrics push has its own flag, `OBS_METRICS_OTLP_ENABLED`.** We considered activating
  push as soon as `OTEL_EXPORTER_OTLP_ENDPOINT` was set, which would be tidier and would match
  what your collectors already configure. We rejected it: that variable is frequently set
  cluster-wide for other services, so an upgrade could silently start egressing telemetry from
  a Solace pod that nobody asked to export. An explicit capability flag keeps the decision
  yours, and it keeps this signal inside the same `OBS_*` model as every other capability. See
  [Metrics](#metrics--planned).

---

## Load and Saturation Visibility — [Interim — logs only]

> _Status: **[Interim]**. This ships as structured **log lines**, not as metrics. The
> `mcp_saturation_total` counter and the occupancy gauge described in
> [Planned for a Later Release](#planned-for-a-later-release-not-frozen-in-this-review)
> are still roadmap. Nothing in this section appears on `/metrics`, because no meter
> provider or OpenTelemetry dependency exists yet to feed these instruments onto that endpoint._

Support and SRE needed one question answered quickly: when a customer says "MCP feels
slow", is the server pacing or shedding requests to protect a broker, or is something else
slow? The metric form of that answer depends on a metrics pipeline this build does not
have — no meter provider, no OpenTelemetry dependency, no saturation instruments — and
standing one up here would duplicate work already in flight under SOL-150254. So the
signal ships as logs now and moves to metrics when that pipeline lands.

Both lines are gated on `OBS_SATURATION_EVENTS_ENABLED` (default off). Neither is a stable
interface: they are diagnostic output, and the metric that replaces them is where the
compatibility commitment will live.

### `broker admission slow` — per request, at `WARN`

Emitted while a request is **still waiting** to be admitted to a broker, once its wait
passes `observability.saturation_threshold_ms` (default `1000`).

| Field | Meaning |
|---|---|
| `broker` | The broker's URL, sanitized. Matches the `broker` field on `request shed`. |
| `operation` | The caller's operation ID, or `unknown`. |
| `stage` | `rate_limit` (waiting on `semp.request_min_interval`) or `concurrency` (waiting on `semp.max_concurrent_per_broker`). |
| `waited` | Time queued so far, measured from entry to the admission path. |
| `threshold` | The configured trip point that fired. |
| `max_queue_wait` | The configured `semp.max_queue_wait` bound. |

Three things worth knowing:

- **It fires during the wait, not after.** A resolution-time line would arrive up to
  `max_queue_wait` later, and under `max_queue_wait: 0` (unbounded, a supported setting) it
  would never arrive at all — precisely the case that most needs a signal.
- **One line per request, whatever happens next.** It does not re-fire on a timer, and no
  companion line is emitted when the wait ends. A request that goes on to be shed is
  reported again by `request shed: broker admission bound exceeded`, using the same `stage`
  vocabulary.
- **The threshold has a floor and a ceiling, and both matter.** The floor is
  `semp.request_min_interval`: a request routinely waits about one pacing interval with
  nothing wrong, and a fan-out step issues up to 8 calls at once, so the last row of a
  healthy fan-out waits roughly 700-800ms at the default pace. The default trip point of
  `1000` sits just above that. Lower `request_min_interval`, raise fan-out concurrency, or
  run many concurrent callers, and you should raise the threshold rather than read the
  resulting volume as saturation. The ceiling is `semp.max_queue_wait`: set the threshold
  at or above it and the signal is silently dead, because the request is shed before the
  timer fires. Nothing validates either bound today.

### `broker in-flight occupancy` — periodic, per broker

Emitted every `observability.otel_self_stats_interval_s` (default `60`) for each broker
**that is carrying load**. Idle brokers are skipped: the presence of a line is itself the
information that a broker is busy.

| Field | Meaning |
|---|---|
| `broker` | The configured broker alias, as it appears in your config file. |
| `in_flight` | Requests currently holding an in-flight slot. |
| `limit` | The configured `semp.max_concurrent_per_broker` cap. |

`WARN` when `in_flight` has reached `limit` — every further request to that broker now
queues at the concurrency gate — and `INFO` below it. One reading is emitted at startup so
turning the capability on mid-incident does not cost you a full interval of silence.

Only brokers the server has actually connected to appear. Broker clients are created on
first use, so a configured but unused broker has no in-flight cap to report.

**This line detects sustained pressure, not bursts.** It is a point sample on a timer, so
a saturation episode shorter than the interval can fall entirely between two ticks and
produce no line at any level. That is an acceptable trade for an interim signal, because
the episodes that matter most are long: a semaphore slot is held for a request's whole
retry chain, roughly 16 minutes at default settings, so a genuinely degraded broker stays
visible across many ticks. For short spikes, rely on the per-request `broker admission
slow` warning above, which is evaluated on every request rather than on a timer. Lowering
`otel_self_stats_interval_s` narrows the gap but does not close it; the metric form will,
because a gauge is scraped rather than sampled by the process.

**Joining the two lines:** the per-request line identifies a broker by sanitized URL and
the periodic line by configured alias, because each reuses the identifier already
established in its own layer. The `broker connection created` line logged at first use
carries both, which is what maps one to the other. The metric form will carry `broker`
(alias) and `server_address` on the same series and remove the need — see
[Decided Since the First Draft](#decided-since-the-first-draft).

---

## Planned for a Later Release (Not Frozen in This Review)

The following are on the roadmap and **not part of this freeze**. Names are indicative and
will get their own review before they ship.

- Load and saturation visibility **as metrics** (a `mcp_saturation_total` counter and a
  rate-limiter health gauge). An interim log-based signal ships today — see
  [Load and Saturation Visibility](#load-and-saturation-visibility--interim--logs-only) —
  but the metric form is still roadmap, tracked under SOL-150254.
- Broker connection-pool gauges.
- A SEMP retry-outcome counter.
- Cancellation and progress signals (which populate the reserved `cancelled` outcome).

---

## Standards This Schema Supports

The audit and metrics surfaces are designed to map to the logging and monitoring
requirements of PCI DSS Requirement 10, SOC 2 (CC7.2 / CC7.3), SOX Section 404, and
ISO/IEC 27001 Annex A.8.15 / A.8.16 / A.8.17. Note that log **integrity** (tamper-evidence)
and **retention** are properties of the SIEM destination you route the audit stream to, not
of the server (see [Audit Delivery](#audit-delivery)).

---

*This draft corresponds to milestone SOL-150251 (Broker MCP Server observability) and is the
artifact referenced by the observability preview brief. It will be finalized and version-frozen
at GA.*
