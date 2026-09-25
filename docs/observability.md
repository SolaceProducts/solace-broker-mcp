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

Each entry is a failure you can see from a metric, a log line, or a probe. Most signals
are off by default. Several failures present as silence rather than an error.

| What you are seeing | Entry |
|---|---|
| A broker's tools all fail; reachability gauge is `0` | [Broker unreachable](#broker-unreachable) |
| A broker's tools fail with 401 or 403 | [Broker credentials rejected](#broker-credentials-rejected) |
| One broker fails; others on the same host are fine | [Broker TLS handshake failure](#broker-tls-handshake-failure) |
| Broker answered but gauges show `0` with `broker_error_*` | [Broker answered with an application error](#broker-answered-with-an-application-error) |
| Callers see `401`; auth failures rising | [Caller tokens rejected](#caller-tokens-rejected) |
| Auth failures rising, all `signature_invalid` | [Signature verification failing after a key rotation](#signature-verification-failing-after-a-key-rotation) |
| Token exchange failing; breaker `open` | [IdP unreachable or token exchange failing](#idp-unreachable-or-token-exchange-failing) |
| `mcp_panic_recovered_total` rising | [Panic recovered](#panic-recovered) |
| Container restarting; `OOMKilled` | [Pod OOM-killed](#pod-oom-killed) |
| Pod takes the full grace period to terminate | [SIGTERM never reaches the process](#sigterm-never-reaches-the-process) |
| Traffic routed to a pod that is not ready | [`/health` and `/livez` are liveness only](#health-and-livez-are-liveness-only) |
| Prometheus target down or absent | [Metrics endpoint not being scraped](#metrics-endpoint-not-being-scraped) |
| Server refuses to start, port-collision error | [Metrics port collides with the MCP port](#metrics-port-collides-with-the-mcp-port) |
| `/readyz` unready naming `metrics_endpoint` or `metrics_provider` | [Metrics endpoint not being scraped](#metrics-endpoint-not-being-scraped) |
| Some `mcp_*` families missing, process still up | [A metrics family is missing after startup](#a-metrics-family-is-missing-after-startup) |
| Slow tool calls under load; no metric | [Requests queueing behind the broker limit](#requests-queueing-behind-the-broker-limit) |
| OTLP push on, collector receiving nothing | [OTLP metrics push arrives nowhere](#otlp-metrics-push-arrives-nowhere) |
| Scrape healthy, OTLP push absent | [OTLP push failed while scrape still works](#otlp-push-failed-while-scrape-still-works) |
| OTLP-only, collector receives nothing | [OTLP-only metrics with nothing arriving](#otlp-only-metrics-with-nothing-arriving) |
| Pushing to Prometheus directly | [Ingesting OTLP into Prometheus without a collector](#ingesting-otlp-into-prometheus-without-a-collector) |
| `go_*` absent from an OTLP backend | [Runtime metrics missing from the OTLP pipeline](#runtime-metrics-missing-from-the-otlp-pipeline) |
| Traces stop arriving | [OTLP collector unreachable](#otlp-collector-unreachable) |
| Tracing on, no spans, process still ready | [Tracing provider failed to start](#tracing-provider-failed-to-start) |
| Diagnose tracing with metrics off | [Diagnosing tracing export without metrics](#diagnosing-tracing-export-without-metrics) |
| Trace volume too high or too low | [Tuning the trace sampler](#tuning-the-trace-sampler) |
| Audit records not in the SIEM | [Audit records not arriving](#audit-records-not-arriving) |
| Server stalls; log throughput collapsed | [Log-shipper or stderr backpressure](#log-shipper-or-stderr-backpressure) |
| Dashboard panels empty after an upgrade | [A dashboard broke after an upgrade](#a-dashboard-broke-after-an-upgrade) |
| Latency panels have no exemplar links | [Exemplar links missing from latency panels](#exemplar-links-missing-from-latency-panels) |
| A few exemplars, most buckets none | [Most buckets carry no exemplar](#most-buckets-carry-no-exemplar) |

### Broker unreachable

**Symptom.** `mcp_broker_reachable{broker="..."} == 0` with
`mcp_broker_unreachable_reason{reason="unreachable"} == 1`. Every tool call against that
broker fails. `mcp_broker_last_result_timestamp_seconds` still advances: it is stamped on
every result, including transport failures.

These gauges are updated by real SEMP calls, not heartbeats. A broker with no call since
process start is absent. Alert on both `== 0` and `absent()`.

**Likely cause.** Connection refused, DNS failure, or I/O timeout.

**First response.** Confirm SEMP answers from somewhere else. Check DNS and pod egress. The
server retries on its own; a restart does not help.

**Escalate.** To the broker owners if SEMP does not answer from any client. To the platform
team if it answers elsewhere but not from this pod.

### Broker credentials rejected

**Symptom.** `mcp_broker_unreachable_reason{reason="credential_invalid"} == 1` — the broker
returned 401 or 403. This is the broker rejecting the server, not a caller token
(`mcp_auth_failure_total`).

**Likely cause.** Wrong, expired, disabled, or locked SEMP account.

**First response.** Verify the Secret against the broker management user. Restart the pod
after correcting the Secret so it re-reads the value.

### Broker TLS handshake failure

**Symptom.** `mcp_broker_unreachable_reason{reason="unreachable"} == 1` — the same value as
DNS or connection refused. The metric cannot tell you it was TLS. The `tool invoked` error
record's `detail` carries the handshake error. `broker connection created` is emitted only
on a successful client build.

**Likely cause.** Expired broker certificate, changed chain, or a CA bundle that does not
cover it.

**First response.** Confirm TLS from the error `detail`, then check certificate dates.

### Broker answered with an application error

**Symptom.** `mcp_broker_reachable == 0` with
`mcp_broker_unreachable_reason{reason="broker_error_NNN"} == 1` (for example
`broker_error_503` or `broker_error_429`). The broker returned an HTTP status, so this is
not a transport failure.

**Likely cause.** SEMP overload, maintenance, or a 5xx from the management API.

**First response.** Treat it as broker-side. Do not rotate MCP credentials. Check broker
health and SEMP load.

### Caller tokens rejected

**Symptom.** `mcp_auth_failure_total{reason="expired"}` rising; callers see 401. Other
`reason` values (`invalid_token`, `audience_mismatch`, `missing`) point elsewhere — see
[Authentication Failures](#authentication-failures).

**Likely cause.** A long-lived agent caching a token, or clock skew versus the IdP.

**First response.** Confirm the caller refreshes tokens. Check node time against the IdP.

### Signature verification failing after a key rotation

**Symptom.** A sharp rise in `mcp_auth_failure_total{reason="signature_invalid"}`, typically
all callers at once.

**Likely cause.** IdP signing-key rotation while JWKS is stale, or an unreachable JWKS
endpoint so the refresh fails quietly.

**First response.** Do not restart the pod. The verifier re-fetches JWKS when it meets an
unknown `kid`. If failures persist: confirm the published JWKS contains that `kid`, that
the pod can reach the JWKS endpoint, and that `iss` mismatches are not being counted
separately as `invalid_token`.

### IdP unreachable or token exchange failing

**Symptom.** `mcp_token_exchange_circuit_breaker_state{breaker="idp-token-exchange",state="open"} == 1`.
The family is one-hot per process. It is absent when Hop-2 OAuth is inactive, metrics are
off, or the breaker is disabled — absence is not closed.

**Likely cause.** IdP down, token endpoint rejecting client credentials, or blocked egress.

**First response.** Check IdP health, DNS, and credentials. The breaker recovers on
successful exchanges; do not restart to force it. At DEBUG,
`waited for concurrent broker token exchange` names followers of a shared exchange; that
is attribution, not saturation.

The open-to-half-open transition is lazy: the next live cache-miss exchange materializes
it. The gauge can remain `open` after the timeout. Alert per scrape target, not by summing
replicas.

### Panic recovered

**Symptom.** `mcp_panic_recovered_total{boundary}` increments (`http` or `tool`); a log line
carries `event="panic_recovered"`. The caller gets an error; the process keeps running.

**Likely cause.** A bug. A recovered panic is never a configuration problem.

**First response.** Capture the log with `correlation_id` and stack. Note `boundary`.
Escalate to engineering.

The counter covers only the request goroutine (`http` middleware and tool `withRecovery`).
`internal/safego` workers and `internal/tokenexchange` singleflight recover and log
`event="panic_recovered"` without incrementing the counter. Alerting on the log attribute
reaches those sites; alerting on the metric does not.

### Pod OOM-killed

**Symptom.** Kubernetes `OOMKilled` and a container restart. On the scrape path,
`go_memstats_heap_inuse_bytes` approaches the container limit. `/readyz` does not report
memory pressure.

**Likely cause.** Limit too low, a leak, or an invalid `GOMEMLIMIT` (that case fails
startup rather than OOM).

**First response.** Compare heap trend to the limit. Raise a stable-but-high limit together
with `GOMEMLIMIT` (~75% of the cap). A climbing heap under flat traffic is a leak.

### SIGTERM never reaches the process

**Symptom.** The pod uses the full `terminationGracePeriodSeconds`. In-flight calls are
cut. `/readyz` never returns `{"status":"shutting_down"}`, and no `draining before shutdown`
line appears — that line is the first thing the drain sequence logs.

**Likely cause.** The server is not PID 1, so Kubernetes SIGTERM never reaches it. The
shipped image is distroless with the binary as entrypoint.

**First response.** Confirm PID 1 with an ephemeral debug container. Run the server as
PID 1, or make the supervisor forward SIGTERM.

### `/health` and `/livez` are liveness only

**Symptom.** Traffic is routed to a pod that cannot serve, or a "health" alert stays green
through a readiness problem.

**Likely cause.** A load balancer or alert pointed at `/health` or `/livez`. Both only
report that the process is alive. `/livez` returns `{"status":"alive"}`; `/health` returns
`{"status":"healthy"}` for compatibility and is not a body-identical alias.

**First response.** Route traffic and readiness alerts to `/readyz` (alias `/ready`). Keep
liveness probes on `/livez`. On SIGTERM, `/readyz` returns 503 immediately so the pod
leaves rotation before it stops accepting `/mcp` work.

### Metrics endpoint not being scraped

**Symptom.** The Prometheus target is down, or absent from the targets page. `/metrics` does
not answer. `mcp_metrics_scrape_total` is flat or missing.

**Likely cause.** In order: the retired `OBS_METRICS_ENABLED` is still set (startup warning
`retired observability flag is set and ignored`); `OBS_METRICS_SCRAPE_ENABLED` is off,
including OTLP-only, so nothing listens on `:9091`; `/readyz` reason `metrics_endpoint`
means the scrape listener failed to bind; `/readyz` reason `metrics_provider` means the
shared meter provider failed to build (`metrics provider build failed` in the startup
log); the ServiceMonitor is not selected; the NetworkPolicy does not admit Prometheus.

**First response.** Grep startup logs for `retired observability flag` and
`metrics provider build failed`. Check `/readyz` for `metrics_endpoint` and
`metrics_provider`. Confirm `OBS_METRICS_SCRAPE_ENABLED`. An unselected ServiceMonitor is
simply not listed. Verify with `mcp_metrics_scrape_total` rising, not with
`kubectl get servicemonitor`.

### Metrics port collides with the MCP port

**Symptom.** The server refuses to start. Config load names
`observability.metrics_bind_address` colliding with the MCP `port`.

**Likely cause.** Both set to the same port. Defaults are `9090` for `/mcp` and `:9091` for
metrics. Collision is checked only when scrape metrics is on.

**First response.** Separate the ports and move Service, container port, and NetworkPolicy
together.

### A metrics family is missing after startup

**Symptom.** The process is ready, but some `mcp_*` families are absent from `/metrics`.
Startup logs include one of: `tool metrics unavailable`, `SEMP metrics unavailable`,
`broker reachability metrics unavailable`, `token exchange circuit breaker metrics unavailable`,
`panic counter unavailable: registration failed`, or `audit drop counter unavailable: registration failed`.

**Likely cause.** That instrument group failed to register. The server continues with a
partial surface.

**First response.** Treat the named family as missing, not zero. Restart after fixing the
error. Do not alert `absent()` on a family whose registration log already failed.

### Requests queueing behind the broker limit

**Symptom.** Tool calls slow under load with no errors. WARN
`broker admission slow: request still waiting to be admitted`, and periodic
`broker in-flight occupancy`. Requires `OBS_SATURATION_EVENTS_ENABLED`. There is no
saturation metric.

**Likely cause.** Concurrent SEMP calls hit `semp.max_concurrent_per_broker`.

**First response.** Read occupancy versus `limit`. Raise the cap or reduce concurrency.
Confirm `saturation_threshold_ms` is below `semp.max_queue_wait` and above normal pacing
(`semp.request_min_interval`). Occupancy skips idle brokers and can miss short bursts.

### OTLP metrics push arrives nowhere

**Symptom.** Silence. The process starts, `/metrics` may look healthy, the collector
receives nothing.

**Likely cause.** Blocked egress, wrong endpoint, or a collector receiver that is not
OTLP/gRPC 4317.

**First response.** Read `mcp_otel_metrics_dropped_total` from scrape when scrape is on.
Read the rate-limited WARN `OTLP metrics export failed`. Check `OTEL_EXPORTER_OTLP_ENDPOINT`
scheme and port 4317. The shipped NetworkPolicy is ingress-only; your egress policy must
allow DNS and TCP 4317.

### OTLP push failed while scrape still works

**Symptom.** `/metrics` is healthy. OTLP metrics never arrive. Startup log:
`OTLP metrics egress unavailable: exporter build failed`.

**Likely cause.** Scrape and OTLP are independent. A failed OTLP exporter does not fail
the scrape listener. Push is simply absent.

**First response.** Do not trust scrape health as proof of push. Fix the OTLP exporter
build (endpoint, TLS). Until then, scrape is the only metrics egress.

### OTLP-only metrics with nothing arriving

**Symptom.** `OBS_METRICS_OTLP_ENABLED=true` without scrape. No `/metrics`. The collector
receives nothing.

**Likely cause.** Same as a broken push, or the meter provider failed to construct. In
OTLP-only mode that failure is `/readyz` `metrics_provider` plus
`metrics provider build failed`. Drop counters themselves ride the broken push.

**First response.** Check `/readyz` for `metrics_provider`. If ready, use
`OTLP metrics export failed`. To diagnose push from an independent surface, also enable
scrape.

### Ingesting OTLP into Prometheus without a collector

**Symptom.** Push aimed at Prometheus; nothing arrives.

**Likely cause.** This server's exporter is OTLP/gRPC. Prometheus's native receiver is
OTLP/HTTP. Direct push cannot work.

**First response.** Scrape `/metrics`, or put a collector in between
(`deploy/otel-collector/`). We emit cumulative temporality only.

### Runtime metrics missing from the OTLP pipeline

**Symptom.** `go_*` and `process_*` absent from an OTLP backend; `mcp_*` arrive.

**Likely cause.** Expected. Those collectors register only on the Prometheus scrape path.

**First response.** Scrape `/metrics` if you need them. Both egresses can run together.

### OTLP collector unreachable

**Symptom.** Traces stop. `mcp_otel_spans_dropped_total{reason}` rises
(`export_error` or `export_timeout`). With metrics off, the same totals are on
`event=otel_self_stats`. Tool calls still succeed; export is best-effort.

**Likely cause.** Collector down, wrong `OTEL_EXPORTER_OTLP_ENDPOINT`, or blocked egress.

**First response.** Confirm `OBS_TRACING_ENABLED=true`, not only the endpoint. Check
collector health and DNS.

### Tracing provider failed to start

**Symptom.** `OBS_TRACING_ENABLED=true`, no spans, `/readyz` still ready. Startup:
`tracing unavailable: provider build failed`.

**Likely cause.** The tracer provider is optional. A build failure logs and continues;
it does not register a readiness probe.

**First response.** Treat tracing as off until that error is gone. This is not a
collector-down case (no drop counters, no `otel_self_stats`).

### Diagnosing tracing export without metrics

**Symptom.** You need export health and no meter provider exists, so span counters are
not on `/metrics`.

**First response.** Read the periodic `otel self stats` INFO line (`event=otel_self_stats`):
`spans_exported_total`, `spans_dropped_queue_full_total` (reserved, never increments),
`spans_dropped_export_timeout_total`, `spans_dropped_export_error_total`,
`spans_dropped_shutdown_total`. Interval:
`observability.otel_self_stats_interval_s`.

The log uses flat per-reason fields; the metric uses a `reason` label.

### Tuning the trace sampler

**Symptom.** Trace volume or collector cost is too high, or too few traces to diagnose.

**First response.** Set both `OTEL_TRACES_SAMPLER` and `OTEL_TRACES_SAMPLER_ARG`. The
argument alone is ignored; the SDK then stays at `parentbased_always_on` (100%). Lowering
the ratio also reduces exemplar coverage.

### Audit records not arriving

**Symptom.** Destructive calls run but no `operation` records in the SIEM, or
`mcp_audit_events_dropped_total` is rising.

**Likely cause.** `OBS_AUDIT_LOG_ENABLED` off; `log_level` above info; stderr write
failure; shipper not collecting this container; gap between stderr and the index.

**First response.** Confirm the flag and `log_level: info` or lower. With metrics on, a
rising drop counter means the server produced records and lost them; flat zero means
look downstream. `kubectl logs` splits the problem: present in stderr is the shipper;
absent is the server. Search `audit_event_type=audit_drop`.

A counter that rises with no `audit_drop` in stderr means stderr itself refused the
ERROR notice — platform, not SIEM. A blocked pipe is backpressure, not a drop: see
the next entry. A counter absent with metrics on means registration failed
(`audit drop counter unavailable`) or a binary before `metrics_schema` 1.8.

### Log-shipper or stderr backpressure

**Symptom.** The server slows or stalls. Log throughput collapses. Tool calls time out
without a broker fault. `mcp_audit_events_dropped_total` stays flat: a blocked write is
not a failed write.

**Likely cause.** The stderr consumer stopped draining. Audit and application logs share
stderr, so a stuck shipper blocks the process. This is the observability failure that
can affect serving.

**First response.** Check the node log shipper and its destination. Restarting the
shipper usually clears it; restarting this pod does not.

### A dashboard broke after an upgrade

**Symptom.** Panels empty, or a query returns no series.

**Likely cause.** A renamed or removed metric or label.

**First response.** Read `mcp_schema_version` (`metrics_schema`, `audit_schema`) and the
`CHANGELOG.md` entry for the version you moved to. Within a major version the schema is
additive; a surprise removal is a policy violation.

### Exemplar links missing from latency panels

**Symptom.** Latency panels render with **no** exemplar links. Metrics otherwise look
right.

**First response.** In order: `OTEL_METRICS_EXEMPLAR_FILTER` is not `always_off`;
`OBS_TRACING_ENABLED` is on and sampled; Prometheus negotiates OpenMetrics and was
started with `--enable-feature=exemplar-storage`; Grafana maps the `trace_id` exemplar
label to the trace data source.

### Most buckets carry no exemplar

**Symptom.** Some exemplar links exist; most histogram buckets have none.

**Likely cause.** Expected under low sampling. An exemplar can only point at a sampled
trace.

**First response.** Raise `OTEL_TRACES_SAMPLER_ARG` only if coverage matters more than
collector volume. This is not the same as zero exemplars everywhere.

## Not in this release

The following operator surfaces are not emitted:

- saturation as metrics;
- broker connection-pool gauges;
- a dedicated SEMP retry-outcome counter;
- tool-invocation cancellation/progress signals (`cancelled` remains reserved there).

Do not build dashboards or alerts against names proposed for those future surfaces.

