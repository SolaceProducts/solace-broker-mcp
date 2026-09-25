# Observability

This guide describes the observability features shipped with the Solace Broker MCP
Server. Use it to choose a signal, enable it, connect it to your monitoring stack, and
understand the data it emits.

The server provides:

- correlation IDs in responses, logs, and outbound SEMP requests;
- metrics through a Prometheus scrape endpoint, OTLP push, or both;
- audit records as structured JSON on stderr;
- distributed traces through OTLP;
- structured saturation logs for broker admission pressure.

Correlation IDs are on by default. Every other optional signal is off by default. The
`observability:` YAML block tunes enabled features but does not enable them.

## Start here

| Goal | What to enable | Also required |
|---|---|---|
| Scrape metrics with Prometheus | `OBS_METRICS_SCRAPE_ENABLED=true` | Restrict the unauthenticated metrics listener |
| Push metrics over OTLP | `OBS_METRICS_OTLP_ENABLED=true` | An OTLP gRPC endpoint |
| Record audit events | `OBS_AUDIT_LOG_ENABLED=true` | `log_level: info` or lower and a JSON log shipper |
| Export traces | `OBS_TRACING_ENABLED=true` | An OTLP gRPC endpoint; set sampling for busy deployments |
| See broker admission pressure in logs | `OBS_SATURATION_EVENTS_ENABLED=true` | A useful `saturation_threshold_ms` |
| Disable request correlation | `OBS_CORRELATION_ID_ENABLED=false` | Nothing; disabling it removes the cross-signal join key |

Typical Prometheus setup:

```bash
export OBS_METRICS_SCRAPE_ENABLED=true
```

Prometheus then scrapes `http://<server>:9091/metrics`. The listener is absent when the flag
is off.

Typical OTLP setup:

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=https://otel-collector.example.com:4317
export OBS_METRICS_OTLP_ENABLED=true
export OBS_TRACING_ENABLED=true
export OTEL_TRACES_SAMPLER=parentbased_traceidratio
export OTEL_TRACES_SAMPLER_ARG=0.10
```

Setting an `OTEL_*` endpoint does not enable metrics or tracing. The matching `OBS_*` flag is
always required.

## Feature switches

Capability switches are environment variables. They are deliberately not accepted in YAML.

| Variable | Default | When `true` | When `false` | Important interaction |
|---|---:|---|---|---|
| `OBS_CORRELATION_ID_ENABLED` | `true` | Accepts or creates a correlation ID and propagates it through the request | No correlation ID is added to responses, logs, audit records, or SEMP requests | Independent of metrics, audit, and tracing |
| `OBS_METRICS_SCRAPE_ENABLED` | `false` | Builds the shared meter provider, registers Go/process collectors, and opens `/metrics` on `observability.metrics_bind_address` | Opens no scrape listener | Either metrics flag builds the shared meter provider |
| `OBS_METRICS_OTLP_ENABLED` | `false` | Builds the shared meter provider and pushes `mcp_*` instruments over OTLP | Pushes no metrics | Does not open `/metrics`; may be combined with scrape |
| `OBS_AUDIT_LOG_ENABLED` | `false` | Emits audit JSON records on stderr | Emits no audit records; destructive calls retain their normal warning | Use `log_level: info` or lower or INFO audit records become `audit_drop` records |
| `OBS_TRACING_ENABLED` | `false` | Builds the tracer provider and exports request-path spans over OTLP | Installs no tracer provider and exports no spans | An OTLP endpoint alone does nothing |
| `OBS_SATURATION_EVENTS_ENABLED` | `false` | Emits admission-delay and in-flight occupancy log records | Emits neither saturation record | This is logs only; no saturation metric ships in this release |
| `OBS_AUTH_FAILURE_COUNTER_ENABLED` | follows the two metrics flags | Records `mcp_auth_failure_total` and `mcp_authz_denied_total` | Suppresses those two counters | If unset, it is true whenever scrape or OTLP metrics is enabled; forcing it true without either egress records nothing |

`OBS_METRICS_ENABLED` is retired and ignored. If it is present, the server warns and tells you
to replace it with `OBS_METRICS_SCRAPE_ENABLED`.

### Common combinations

| Configuration | Result |
|---|---|
| No observability variables | Correlation IDs only |
| Scrape on, OTLP metrics off | `/metrics` exists; includes `mcp_*`, `go_*`, and `process_*` |
| Scrape off, OTLP metrics on | `mcp_*` metrics are pushed; no `/metrics`, `go_*`, or `process_*` |
| Both metrics flags on | Both readers observe the same `mcp_*` instruments; `/metrics` also carries Go/process metrics |
| Tracing on, metrics off | Traces are exported; span-export health is reported by the periodic `otel self stats` log |
| Tracing and a metrics egress on | Traces are exported; span-export health counters are available through the metrics egress |
| Audit on, metrics off | Audit JSON is emitted; there is no metrics-side audit-drop counter |
| Audit off, metrics on | `mcp_audit_events_dropped_total` is present at zero; this does not mean auditing is enabled |

## Configuration

The YAML `observability:` block contains tunables and identity fields. It does not contain
on/off switches.

| YAML field | Default | Used when | Effect |
|---|---:|---|---|
| `observability.metrics_bind_address` | `:9091` | Scrape metrics is on | Address for the separate `/metrics` listener |
| `observability.shutdown_drain_delay_s` | `10` | Always | Delay after readiness becomes false and before graceful HTTP shutdown |
| `observability.saturation_threshold_ms` | `1000` | Saturation logs are on | Queue wait that triggers `broker admission slow` |
| `observability.otel_self_stats_interval_s` | `60` | Tracing has no meter provider; also saturation occupancy reporting | Periodic reporting interval in seconds |
| `observability.progress_signal_threshold_ms` | `5000` | Not used in this release | Reserved; changing it has no effect |
| `observability.service_name` | `solace-broker-mcp` | Metrics, traces, and logs | OTel `service.name` |
| `observability.service_instance_id` | pod name, else hostname | Metrics and traces | OTel `service.instance.id` |
| `observability.deployment_environment` | omitted | Metrics, traces, and logs | OTel `deployment.environment.name` |
| `observability.cloud_region` | omitted | Metrics, traces, and logs | OTel `cloud.region` |

All numeric tunables use their default when they are omitted or non-positive. Every YAML
value supports `${VAR}` substitution. See [Configuration](configuration.md#observability-settings)
for the complete configuration reference.

### Resource attributes

Identity resolves in this order:

1. a non-empty YAML identity field;
2. `OTEL_SERVICE_NAME` or the matching `OTEL_RESOURCE_ATTRIBUTES` entry;
3. the built-in default shown above.

For `service.name`, `OTEL_SERVICE_NAME` takes precedence over a `service.name` entry in
`OTEL_RESOURCE_ATTRIBUTES`. Empty and whitespace-only values count as unset.

At startup, the `observability identity resolved` INFO record reports each resolved value and
its source. Compare `service_instance_id` across replicas: a shared config or environment
value can accidentally collapse several pods into one monitoring identity.

On the Prometheus scrape path, resource attributes appear on `target_info`; join them rather
than expecting them on every series. On the OTLP path, configure your backend to promote the
attributes you want to query. For Prometheus OTLP ingestion, include at least
`service.name`, `service.instance.id`, `deployment.environment.name`, and `cloud.region` in
`promote_resource_attributes`.

## Schema and compatibility

Two versions identify the published contract:

- `metrics_schema` is **1.8**, exposed by `mcp_schema_version`.
- `audit_schema` is **1.2**, exposed by `mcp_schema_version` and by
  `audit_schema_version` on every audit record.

Within a major version, changes are additive. A new metric, field, or closed-set value bumps
the minor version. A rename or removal requires a major version, an announcement, and at
least two minor releases of notice. Metric or audit-field renames are dual-emitted when that
can be done without changing the meaning or doubling a single-valued record.

Pin dashboards to `mcp_schema_version` and SIEM queries to `audit_schema_version`. The Go
runtime and process collectors are upstream Prometheus schema and are outside this
compatibility commitment.

### Naming and data handling

- First-party metrics start with `mcp_`.
- Prometheus counters end in `_total`; metric durations use seconds.
- Audit durations use milliseconds and end in `_ms`.
- Audit timestamps use RFC 3339 UTC and end in `_utc`.
- Metric labels use bounded domains. No correlation ID, raw argument, token, or credential is
  used as a metric label.
- Credentials, tokens, and raw tool arguments are not written to metrics or audit records.

## Metrics

> The following `mcp_*` instruments are wired and emitted today when their required flags and
> runtime paths are active: `mcp_build_info`, `mcp_schema_version`,
> `mcp_metrics_scrape_total`, `mcp_http_active_requests`, `mcp_tool_invocation_total`,
> `mcp_tool_invocation_duration_seconds`, `mcp_semp_request_total`,
> `mcp_semp_request_duration_seconds`, `mcp_broker_reachable`,
> `mcp_broker_unreachable_reason`, `mcp_broker_last_result_timestamp_seconds`,
> `mcp_token_exchange_circuit_breaker_state`, `mcp_auth_failure_total`,
> `mcp_authz_denied_total`, `mcp_broker_authz_denied_total`,
> `mcp_audit_events_dropped_total`, `mcp_panic_recovered_total`,
> `mcp_otel_spans_exported_total`, `mcp_otel_spans_dropped_total`,
> `mcp_otel_metrics_exported_total`, and `mcp_otel_metrics_dropped_total`.

The tables below are the live first-party metric schema. A family can still be absent because
its capability is off or its runtime prerequisite does not exist; each section states those
conditions.

### Server and Scrape Health

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_build_info` | Gauge (constant `1`) | `version` | Solace |
| `mcp_schema_version` | Gauge (constant `1`) | `metrics_schema`, `audit_schema` | Solace |
| `mcp_metrics_scrape_total` | Counter | none | Solace |
| `mcp_http_active_requests` | Gauge | none | Solace |

`mcp_http_active_requests` counts requests on `/mcp` from request entry, before authentication.
It therefore includes requests later rejected with 401, 403, or 413.

The `/metrics` endpoint is unauthenticated and unencrypted. It binds to all interfaces by
default. Restrict it with a NetworkPolicy, bind it to loopback for a sidecar, or place an
equivalent network control around it.

### Tool Invocations (RED)

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_tool_invocation_total` | Counter | `tool`, `broker`, `outcome`, `error_type` | Solace |
| `mcp_tool_invocation_duration_seconds` | Histogram | `tool`, `broker`, `outcome`, `error_type` | Solace |

These cover calls that reach tool dispatch. A hop-1 authorization denial occurs before the
handler and appears in `mcp_authz_denied_total`, not in these metrics.

`broker` is the configured alias, or `none` / `unknown` before successful resolution. The
histogram buckets in seconds are `0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10`.

### SEMP Requests (RED, per Attempt)

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_semp_request_total` | Counter | `http_request_method`, `http_response_status_code`, `server_address`, `broker`, `api`, `operation`, `attempt` | Mixed |
| `mcp_semp_request_duration_seconds` | Histogram | same label set, minus `attempt` | Mixed |

Each retry attempt increments the counter. The histogram measures one broker round trip to
the first response byte; it excludes admission wait, response-body read, and retry backoff.
When no response arrives, `http_response_status_code=""`.

Histogram buckets in seconds are
`0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10`.

### Broker Reachability

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_broker_reachable` | Gauge (`1`/`0`) | `broker` | Solace |
| `mcp_broker_unreachable_reason` | Gauge (`1`/`0`) | `broker`, `reason` | Solace |
| `mcp_broker_last_result_timestamp_seconds` | Gauge (Unix seconds) | `broker` | Solace |

These are updated by real SEMP calls; they are not heartbeats. A broker with no call since
process start is absent. Alert on both `mcp_broker_reachable == 0` and absence.

`reason` is `credential_invalid`, `unreachable`, or `broker_error_NNN`. The reason gauge is
one-hot per broker. Other 4xx responses count as reachable because the broker answered.

### Token-Exchange Circuit Breaker State

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_token_exchange_circuit_breaker_state` | Gauge (`1`/`0`) | `breaker`, `state` | Solace |

For `breaker="idp-token-exchange"`, `state` is `closed`, `open`, or `half-open`; exactly one
series is `1` per process. Alert per scrape target rather than summing replicas.

The gauge reports the last materialized breaker state, not IdP health. The open-to-half-open
transition is materialized lazily by the next live token exchange, so the gauge can remain
open after the timeout. Scraping is passive and does not trigger that transition.

The family is absent when Hop-2 OAuth is inactive, metrics are off, or the breaker is disabled.
Absence does not mean closed.

### Authentication Failures

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_auth_failure_total` | Counter | `reason` | Solace |

`reason` is one of:

| `reason` | Meaning |
|---|---|
| `invalid_token` | Malformed token, issuer mismatch, static-token mismatch, or malformed Authorization header |
| `expired` | Token expiry passed |
| `audience_mismatch` | Audience is not accepted |
| `signature_invalid` | Signature or JWKS verification failed |
| `missing` | No credential, or an otherwise valid token without `sub` |

All five series are seeded at zero. There is no broker label because authentication occurs
before broker selection.

### Authorization Denials

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_authz_denied_total` | Counter | `tool`, `reason` | Solace |

This is hop 1: the MCP server refused an authenticated caller before dispatch. `reason` is
`missing_claim` or `not_permitted`. Series appear only after their first denial; alert on
`increase()`, not `absent()`.

### Broker-Side Authorization Denials

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_broker_authz_denied_total` | Counter | `tool`, `broker`, `reason` | Solace |

This is hop 2: the broker refused the SEMP operation after dispatch. `reason` is
`permission_denied`. A destructive call also produces its tool-invocation sample and
operation audit record because execution had already started.

### Audit Pipeline Health

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_audit_events_dropped_total` | Counter | none | Solace |

This counter is registered whenever a metrics provider exists, even when audit is off. It is
seeded at zero. With audit off, zero means nothing was emitted and therefore nothing dropped;
it does not mean auditing is active.

With audit on, alert on any increase and on unexpected absence:

```promql
increase(mcp_audit_events_dropped_total[5m]) > 0
```

The matching `audit_drop` record carries attribution. The counter has no labels and survives
a failure of the log stream because it uses a separate metrics egress.

### Panic Recovery

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_panic_recovered_total` | Counter | `boundary` | Solace |

`boundary` is `http` or `tool`. Both series are seeded at zero. Recovery and
`event=panic_recovered` ERROR logs remain active when metrics are off; only the counter is
absent.

### OTLP Export Health

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_otel_spans_exported_total` | Counter | none | Solace |
| `mcp_otel_spans_dropped_total` | Counter | `reason` | Solace |

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_otel_metrics_exported_total` | Counter | none | Solace |
| `mcp_otel_metrics_dropped_total` | Counter | `reason` | Solace |

The metrics pair exists only when OTLP metrics push is enabled. The span pair is maintained
while tracing is enabled and is exposed as metrics only when a meter provider also exists.

For spans, live reasons are `export_timeout`, `export_error`, and `shutdown`; `queue_full` is
reserved but not observable through the OTel SDK. For metrics, live reasons are
`export_timeout` and `export_error`; there is no queue and shutdown is reported by a warning.

When both scrape and OTLP metrics are on, use the scrape copy of these counters to diagnose a
broken push without depending on the broken path.

### `otel self stats` — periodic, when metrics are off

When tracing is enabled but no meter provider exists, the server emits an immediate and then
periodic INFO record with `event=otel_self_stats`. It contains:

- `spans_exported_total`;
- `spans_dropped_queue_full_total`;
- `spans_dropped_export_timeout_total`;
- `spans_dropped_export_error_total`;
- `spans_dropped_shutdown_total`.

The interval is `observability.otel_self_stats_interval_s`.

### Trace Exemplars

The tool and SEMP latency histograms carry exemplars when metrics and tracing are both enabled
and the active trace is sampled. Prometheus must negotiate OpenMetrics and enable exemplar
storage. In Grafana, map the `trace_id` exemplar label to the trace data source.

`OTEL_METRICS_EXEMPLAR_FILTER=always_off` suppresses exemplars. The default OTel behavior is
`trace_based`.

### Go Runtime and Process Metrics

`go_*` and `process_*` collectors are included only on the Prometheus scrape path. They never
reach OTLP metrics push because they are registered directly with the Prometheus registry.
Their upstream names are outside the `mcp_*` schema commitment.

### Grafana Dashboard

Import `deploy/grafana/solace-broker-mcp-overview.json` through
**Dashboards → New → Import** and bind its Prometheus data-source input.

The dashboard supports scraped metrics and metrics ingested into Prometheus through OTLP.
For OTLP ingestion, this server needs an OTel Collector bridge because its exporter is gRPC
and Prometheus's native OTLP receiver is HTTP. Enable Prometheus's OTLP receiver, use an
`otlphttp` collector exporter, and configure `promote_resource_attributes`.

The dashboard's service variable uses Prometheus's `job` label so it works with both
ingestion paths. Give each service its own scrape job. Go/process panels intentionally have
no data on the OTLP-only path.

## Audit Trail

Enable audit records with:

```bash
export OBS_AUDIT_LOG_ENABLED=true
```

Audit records are structured JSON on stderr and carry `"event":"audit"`. Configure your log
shipper to route that sub-stream to a protected SIEM index. Set `log_level: info` or lower:
operation and successful-authentication records are INFO. If the configured level filters one
out, the server emits an ERROR `audit_drop` record instead.

Audit delivery is best effort. A failed write never fails or delays the broker operation. A
refused write is dropped and reported; a blocked stderr pipe can still block the process and
does not increment the drop counter because no write failure is returned.

### Coverage

An `operation` record is emitted for a tool carrying `destructiveHint` after broker resolution
and argument validation. It is not a record of every state change. In particular,
`create-message-vpn`, `create-queue`, `create-queue-subscription`,
`create-topic-endpoint`, `create-rdp`, `clear-queue-stats`, and `clear-client-stats`
do not emit an operation record.

A missing/unknown broker, unknown tool, or argument-validation failure happens before the
destructive gate and emits no operation record.

One destructive call normally produces one operation record at completion. A broker-side
authorization denial also produces `broker_authz_denied`; therefore a destructive call can
produce two audit records. Query by `audit_event_type`, not by counting records per
correlation ID.

### Event Fields

| Field | Meaning | Type |
|---|---|---|
| `event` | Routing tag, always `audit` | string |
| `audit_event_type` | Record discriminator | closed-set string |
| `audit_schema_version` | Audit contract version | string (`1.2`) |
| `timestamp_utc` | Record time | RFC 3339 UTC |
| `correlation_id` | Join key; omitted when unavailable | string |
| `started_at_utc` | Operation start | RFC 3339 UTC |
| `duration_ms` | Operation duration | integer |
| `principal.sub` | Authenticated human OIDC subject | string |
| `agent_client_id` | Authenticated client/agent ID | string |
| `tool` | MCP tool | string |
| `broker` | Configured broker alias | string |
| `outcome` | Operation result | closed-set string |
| `error_type` | Failure cause on an error operation | closed-set string |
| `panic_recovered` | Recovered destructive-handler panic | boolean |
| `arguments_hash` | SHA-256 over redacted, canonicalized arguments | lowercase hex |
| `reason` | Authentication/authorization failure reason | closed-set string |
| `dropped_audit_event_type` | Record type that could not be emitted | closed-set string |

`audit_event_type` is one of `operation`, `auth_success`, `auth_failure`, `authz_denied`,
`broker_authz_denied`, `broker_auth_retry`, or `audit_drop`.

| `audit_event_type` | `outcome` | `error_type` | `reason` | `tool` | `arguments_hash` | timing | `broker` | identity | dropped type | panic |
|---|---|---|---|---|---|---|---|---|---|---|
| `operation` | yes | on error | — | yes | yes | yes | yes | optional | — | optional |
| `auth_success` | — | — | — | — | — | — | — | optional | — | — |
| `auth_failure` | — | — | yes | — | — | — | — | optional | — | — |
| `authz_denied` | — | — | yes | yes | — | — | — | optional | — | — |
| `broker_authz_denied` | — | — | yes | yes | — | — | yes | optional | — | — |
| `broker_auth_retry` | yes | — | — | — | — | optional | yes | optional | — | — |
| `audit_drop` | — | — | — | optional | — | — | optional | — | optional | — |

`principal` is a nested object. Only the opaque OIDC `sub` is stored; a human-readable
username is not written to the append-only stream. Identity is absent when authentication is
disabled or no verified principal exists.

`arguments_hash` excludes `broker` and replaces sensitive values with `[REDACTED]` before
RFC 8785 canonicalization and SHA-256 hashing. Raw arguments are never stored in an audit
record.

### Authentication Events

- `auth_success`: verified request, with identity when available.
- `auth_failure`: rejected credential. `reason` uses the same five values as
  `mcp_auth_failure_total`.
- `authz_denied`: hop-1 tool refusal, with `tool` and reason `missing_claim` or
  `not_permitted`.
- `broker_authz_denied`: hop-2 broker refusal, with `tool`, `broker`, and reason
  `permission_denied`.
- `broker_auth_retry`: broker 401 recovery attempt; its outcome says whether the credential
  problem recovered, not whether the whole tool call ultimately succeeded.

Authentication success/failure records are per `/mcp` request, not per destructive call.
Plan SIEM volume accordingly.

### Canonical Audit Queries

Use the version emitted by your deployment:

```text
event="audit" AND audit_schema_version="1.2" AND audit_event_type="operation"
event="audit" AND audit_schema_version="1.2" AND audit_event_type="auth_failure"
event="audit" AND audit_schema_version="1.2" AND audit_event_type="authz_denied"
event="audit" AND audit_schema_version="1.2" AND audit_event_type="broker_authz_denied"
event="audit" AND audit_schema_version="1.2" AND audit_event_type="audit_drop"
```

A complete authorization-refusal review requires both `authz_denied` and
`broker_authz_denied`.

## Distributed Tracing

Tracing exports OTLP over gRPC and is never enabled automatically:

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=https://otel-collector.example.com:4317
export OBS_TRACING_ENABLED=true
```

The server honors `OTEL_EXPORTER_OTLP_INSECURE`,
`OTEL_EXPORTER_OTLP_CERTIFICATE`, and the standard client-certificate/key variables.
Use TLS to a collector you control. Traces carry client network identity, tool/broker
attributes, and token-exchange error details.

Sampling uses `OTEL_TRACES_SAMPLER` and `OTEL_TRACES_SAMPLER_ARG`. With neither set, the OTel
SDK uses `parentbased_always_on`. Setting only the argument does not select a ratio sampler.

W3C Trace Context is propagated. Baggage is not propagated because the server has no
redaction policy for arbitrary baggage values.

### Spans

| Span | Kind | One per |
|---|---|---|
| `POST /mcp` | Server | inbound MCP POST |
| `tools.CallTool` | Internal | dispatched tool invocation |
| `composite.Execute` | Internal | composite execution; absent for native tools |
| `semp.request` | Client | SEMP call covering the complete retry chain |
| `semp.attempt` | Client | SEMP HTTP attempt |
| `tokenexchange.Exchange` | Internal | token-exchange request, cache hit, winner, or follower |
| `tokenexchange.attempt` | Client | live IdP HTTP attempt made by the singleflight winner |

The long-lived SSE `GET /mcp` stream is not traced. A hop-1 authorization denial stops before
tool dispatch and therefore has only the HTTP entry span. A hop-2 broker denial has the full
request hierarchy.

### Span Attributes

| Attribute | Meaning |
|---|---|
| `correlation_id` | Join to logs and audit |
| `outcome` | Shared result vocabulary |
| `error_type` | Shared tool-failure vocabulary; only on `tools.CallTool` errors |
| `tool` | MCP tool |
| `broker` | Canonical configured alias, or `none` / `unknown` |
| `semp.version` | `v1` or `v2` |
| `semp.operation` | SEMPv2 operation ID |
| `composite.steps` | Declared composite step count |
| `attempt` | 1-based attempt number |
| `http.response.status_code` | Attempt response status; absent without a response |
| `retry.decision` | Decision the retry policy acted on |
| `retry.exhausted` | Retry allowance was spent |
| `cache_hit` | Token exchange used cache |
| `singleflight_role` | `winner` or `follower` on a live/shared token exchange |
| `winner_trace_id`, `winner_span_id` | Pivot from a follower to the winner trace |

Attempt spans have no `outcome`; an attempt can fail inside a successful retry chain.
`tokenexchange.Exchange` has an outcome but no `error_type`.

Entry spans include standard OTel HTTP attributes. `network.peer.address` is the transport
peer; `client.address` comes from `X-Forwarded-For` and is not trusted. Authorization headers,
cookies, and URL query strings are not exported. Failed token-exchange spans record the
exception text and can reveal the IdP host/address; include that in your data-flow review.

### Stand up tracing

The reference collector is under `deploy/otel-collector/`. For Kubernetes:

```bash
kubectl apply -f deploy/otel-collector/kubernetes/
```

For local Tempo:

```bash
cd deploy/otel-collector/docker
docker compose up
```

After a tool call, search your backend for `service.name = solace-broker-mcp`. A trace should
contain `POST /mcp`, `tools.CallTool`, and the relevant child spans.

### Ingesting OTLP metrics into Prometheus (collector required)

Scraping `/metrics` is the shortest Prometheus path and needs no collector.

For OTLP push into Prometheus, place an OTel Collector between the server and Prometheus:

1. receive OTLP/gRPC from this server;
2. export OTLP/HTTP to `http(s)://<prometheus>/api/v1/otlp`;
3. start Prometheus with `--web.enable-otlp-receiver`;
4. configure `promote_resource_attributes`;
5. tune `storage.tsdb.out_of_order_time_window` if your environment needs it.

Direct push from this server to bare Prometheus does not work: the server exporter is gRPC
and Prometheus's receiver is HTTP.

## Correlation ID

Correlation is on by default. For each request the server uses, in order:

1. the trace ID from a valid W3C `traceparent`;
2. a valid `X-Correlation-ID`;
3. a generated UUIDv7.

An `X-Correlation-ID` must be printable ASCII, non-empty after trimming, and at most 128
characters. Invalid input is discarded rather than modified.

The chosen ID is returned in the response `X-Correlation-ID` header and
`CallToolResult.Meta["correlation_id"]`, added to request-scoped logs and audit records, and
propagated to SEMP requests. A W3C `traceparent` is sent outbound only when the correlation
ID is a valid trace ID.

Shared token exchanges log their IdP work under the initiating request's ID. At DEBUG, a
follower emits `waited for concurrent broker token exchange` under its own ID. That message
is attribution, not a fault or saturation signal.

## The Outcome Vocabulary

For tool-invocation metrics, logs, audit records, and spans:

| Value | Meaning |
|---|---|
| `success` | Tool call completed successfully |
| `error` | Tool call failed; `error_type` explains why |
| `cancelled` | Reserved for tool invocations in this release; emitted today only by `tokenexchange.Exchange` when its caller context ends |

### `error_type`

Present only with `outcome=error`. The tool-invocation vocabulary has thirteen values:

| Value | Meaning |
|---|---|
| `panic` | Recovery caught an unexpected panic |
| `unknown_tool` | Tool is not registered |
| `missing_broker` | Required broker was not supplied |
| `unknown_broker` | Broker alias is not configured |
| `broker_init_error` | Configured broker could not initialize |
| `bad_request` | Malformed request or invalid parameter handled before normal validation |
| `validation_error` | Input schema validation failed |
| `not_found` | Requested item does not exist |
| `execution_error` | Tool ran and failed |
| `nil_result` | Tool returned no result |
| `output_validation_error` | Tool output failed schema validation |
| `marshal_error` | Result serialization failed |
| `broker_permission_denied` | Broker refused the exchanged identity |

Notes:

- Only `execution_error`, `nil_result`, `output_validation_error`, `marshal_error`, `panic`,
  and `broker_permission_denied` can reach an `operation` audit record.
- Authentication and authorization refusals are separate metric/audit signals, not outcome
  values.
- A desired-state no-op is `outcome=success`; the operational `tool invoked` log carries
  `desired_state`. The operation audit record does not carry that field.

## Load and Saturation Visibility

Saturation visibility is structured logging, not metrics, in this release. Enable it with
`OBS_SATURATION_EVENTS_ENABLED=true`.

### `broker admission slow` — per request

This WARN record fires once while a request is still queued longer than
`observability.saturation_threshold_ms`.

| Field | Meaning |
|---|---|
| `broker` | Sanitized broker URL |
| `operation` | SEMP operation ID or `unknown` |
| `stage` | `rate_limit` or `concurrency` |
| `waited` | Current queue time |
| `threshold` | Configured trigger |
| `max_queue_wait` | Configured admission limit |

Set the threshold above normal pacing (`semp.request_min_interval`) and below
`semp.max_queue_wait`. At or above `max_queue_wait`, the request is shed before this warning
can fire.

### `broker in-flight occupancy` — periodic

This record reports `broker`, `in_flight`, and `limit` for active broker clients. It is WARN
at the limit and INFO below it. Idle/unrealized brokers are skipped. The interval is
`observability.otel_self_stats_interval_s`; short spikes can occur between samples.

The admission record identifies a sanitized broker URL, while occupancy uses the configured
alias. The `broker connection created` record carries both values.

## Deployment Topology and Resource Policy

The examples in `deploy/kubernetes/` are starting points. They ship with two replicas,
rolling updates with `maxUnavailable: 0`, a PodDisruptionBudget, best-effort node spreading,
and ClientIP session affinity.

MCP sessions are process-local. Two replicas keep an endpoint available, but do not preserve
sessions across pod replacement. Behind an ingress, gateway, or service mesh, configure
stickiness there; Kubernetes Service affinity is bypassed.

SEMP concurrency, pacing, retry state, breakers, and token caches are also per process.
Broker load can therefore scale with replica count.

### Resource requests and limits

The Kubernetes example requests `100m` CPU and `128Mi` memory, sets a `512Mi` memory limit,
and deliberately has no CPU limit. When you set a container memory cap, set `GOMEMLIMIT` to
about 75% of that cap; the shipped pair is `512Mi` and `384MiB`. Change them together.

Go uses `MiB`, not Kubernetes's `Mi`. An invalid `GOMEMLIMIT` prevents startup. On bare metal
or a VM without a process memory cap, leaving it unset is normally correct.

### Scraping and securing the metrics endpoint

The scrape listener is separate from the MCP listener, unauthenticated, and wildcard-bound by
default. The Kubernetes example provides:

- a named `metrics` Service/container port;
- a NetworkPolicy allowing port 9091 only from the `monitoring` namespace;
- `servicemonitor.yaml.example` for Prometheus Operator.

The Service port exists even when scrape metrics is off; the connection is refused until
`OBS_METRICS_SCRAPE_ENABLED=true`.

If you change `metrics_bind_address`, update the Service, container port, NetworkPolicy, and
any literal Prometheus annotation. A NetworkPolicy is effective only when the cluster CNI
enforces it, and allowed traffic is the union of all policies selecting the pod. Verify from
a pod outside the monitoring namespace.

The shipped policy is ingress-only. If your cluster default-denies egress, add DNS and TCP
4317 access to your OTLP collector in your own egress policy.

## Operator Runbook

### `/metrics` is unavailable

Check `OBS_METRICS_SCRAPE_ENABLED`; OTLP-only mode intentionally opens no listener. Confirm
the bind address differs from the MCP port and inspect `/readyz` for `metrics_endpoint`.

### Metrics are pushed but do not arrive

Check the collector endpoint, TLS variables, and egress policy. Read
`mcp_otel_metrics_dropped_total` on the scrape path when available, otherwise inspect the
rate-limited `OTLP metrics export failed` warning. Do not point the gRPC exporter directly at
Prometheus's HTTP receiver.

### Traces do not arrive

Confirm `OBS_TRACING_ENABLED=true`, not just `OTEL_EXPORTER_OTLP_ENDPOINT`. With no metrics
provider, inspect `event=otel_self_stats`. If sampling is unexpectedly 100%, ensure
`OTEL_TRACES_SAMPLER` is set; the argument alone is ignored.

### Audit records do not arrive

Confirm `OBS_AUDIT_LOG_ENABLED=true`, `log_level: info` or lower, and a shipper routing
`event=audit`. Alert on `mcp_audit_events_dropped_total`; inspect `audit_drop` for
attribution. A blocked stderr pipe is backpressure, not a reported drop.

### Broker reachability is absent

The gauges are passive. Drive a real SEMP request before treating absence as a fault.

### Token-exchange breaker appears stuck open

The transition to half-open is lazy and requires a live cache-miss exchange. The gauge is the
last materialized state, not an IdP health probe.

### Saturation logs do not appear

Confirm the flag, then ensure `saturation_threshold_ms` is below `semp.max_queue_wait` and
above normal request pacing. Occupancy reports only active broker clients and can miss short
bursts.

### Exemplars are missing

Confirm tracing is enabled and sampled, Prometheus negotiates OpenMetrics and stores
exemplars, Grafana has a trace-data-source mapping, and
`OTEL_METRICS_EXEMPLAR_FILTER` is not `always_off`.

## Not in this release

The following operator surfaces are not emitted:

- saturation as metrics;
- broker connection-pool gauges;
- a dedicated SEMP retry-outcome counter;
- tool-invocation cancellation/progress signals (`cancelled` remains reserved there).

Do not build dashboards or alerts against names proposed for those future surfaces.

