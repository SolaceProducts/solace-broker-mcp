# Observability Schema

> **Status: Draft for pilot review.** This document is the proposed metric, audit, and
> trace schema for the Broker MCP Server. It is published for review **before** the names
> freeze at GA, so the time to change a name is now. After GA the schema is **additive-only
> within a MAJOR version**: names are added freely, and a rename or removal happens only at a
> MAJOR bump, after an announced deprecation with at least two minor releases of notice,
> dual-emitting where dual emission is possible. That is a migration path, not a promise never
> to change — see
> [Compatibility and Deprecation Policy](#compatibility-and-deprecation-policy) and
> [How to Give Feedback](#how-to-give-feedback).
>
> **Most of the metrics and trace surface is not emitted by the current build.** See the
> [Implementation Status](#implementation-status) table for what is live; anything marked
> **[Planned]** has a feature flag and nothing behind it. Those sections are written in the
> present tense of the *proposed* design,
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

## Start Here

| If you are here to… | Go to |
|---|---|
| Find out what is actually live today | [Implementation Status](#implementation-status) |
| Tell us to rename something before the freeze | [How to Give Feedback](#how-to-give-feedback) |
| Understand naming rules, units, and what we commit to | [Conventions](#conventions) · [Compatibility and Deprecation Policy](#compatibility-and-deprecation-policy) |
| Build a Grafana dashboard or an alert rule | [Metrics](#metrics--planned-with-exceptions) |
| Write a SIEM rule for compliance evidence | [Audit Trail](#audit-trail--interim--all-record-types-except-broker_authz_denied) · [Canonical Audit Queries](#canonical-audit-queries) |
| Diagnose one slow or failed call end to end | [Distributed Tracing](#distributed-tracing--interim-request-path-and-per-attempt-spans-wired) · [Correlation ID](#correlation-id--implemented) |
| Look up what `outcome` or `error_type` means | [The Outcome Vocabulary](#the-outcome-vocabulary) |
| Check this works with your existing stack | [Vendor Neutrality](#vendor-neutrality) |
| Deploy to Kubernetes | [Deployment Topology and Resource Policy](#deployment-topology-and-resource-policy--implemented) |
| See what is still open, and what we already decided | [Open Items for This Review](#open-items-for-this-review) · [Planned for a Later Release](#planned-for-a-later-release-not-frozen-in-this-review) |
| Map this to PCI DSS, SOC 2, SOX, or ISO 27001 | [Standards This Schema Supports](#standards-this-schema-supports) |
| Understand load shedding and saturation | [Load and Saturation Visibility](#load-and-saturation-visibility--interim--logs-only) |

**Not in this document yet.** These are deliberately listed as plain text, not links, because
they do not exist to link to. Each names the story that lands it:

- Tracing setup and the reference OTel collector deployment, with a tested-backend matrix —
  lands with Story 40 (SOL-152423).
- Operator runbook by failure mode — lands with Story 36 (SOL-152098).
- Reference SLO sheet — lands with Story 38.

The Broker MCP Server is designed to emit three observability signals:

- **Metrics**, on a Prometheus `/metrics` endpoint, for dashboards and alerts.
- **An audit trail**, one JSON event per **destructive** tool call that reaches execution, for
  compliance evidence. Note "destructive", not "state-changing": object creation is not
  audited today, and a call that fails broker resolution or argument validation writes no
  record either — see
  [Audit Trail](#audit-trail--interim--all-record-types-except-broker_authz_denied) for both
  gaps.
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
| Metrics | **[Planned, with exceptions]** | Most instrument names and labels here are still the proposal under review. Wired and emitted today: the `/metrics` endpoint itself, `mcp_build_info`, `mcp_schema_version`, `mcp_metrics_scrape_total`, `mcp_http_active_requests`, `mcp_tool_invocation_total`, `mcp_tool_invocation_duration_seconds`, `mcp_semp_request_total`, `mcp_semp_request_duration_seconds`, both OTLP export-health counter pairs — spans and metrics (`mcp_otel_spans_exported_total` / `mcp_otel_spans_dropped_total` and `mcp_otel_metrics_exported_total` / `mcp_otel_metrics_dropped_total`, see [OTLP Export Health](#otlp-export-health)) — `mcp_panic_recovered_total` (see [Panic Recovery](#panic-recovery--implemented)), `mcp_auth_failure_total` and `mcp_authz_denied_total` (see [Authentication Failures](#authentication-failures--implemented) and [Authorization Denials](#authorization-denials--implemented)), the `go_*`/`process_*` runtime collectors (see [Go Runtime and Process Metrics](#go-runtime-and-process-metrics)), and `mcp_broker_reachable`, `mcp_broker_unreachable_reason`, and `mcp_broker_last_result_timestamp_seconds` (see [Broker Reachability](#broker-reachability)). `mcp_broker_authz_denied_total` is documented but **not** emitted yet (see [Broker-Side Authorization Denials](#broker-side-authorization-denials--not-yet-emitted)). Assume any other metric below is not yet emitted. |
| Audit trail | **[Interim — all record types except `broker_authz_denied`]** | Destructive tool calls emit an `operation` record behind `OBS_AUDIT_LOG_ENABLED` (default off). `auth_success`, `auth_failure`, `authz_denied`, and `broker_auth_retry` also emit today (SOL-152097). `broker_authz_denied` and the `mcp_audit_events_dropped_total` counter are not emitted yet. See [Audit Trail](#audit-trail--interim--all-record-types-except-broker_authz_denied). |
| Distributed tracing | **[Interim — request-path and per-attempt spans wired]** | Tracer provider, OTLP export, W3C context propagation, and spans at the HTTP boundary, the tool dispatcher, the composite executor, each SEMP call, each SEMP *attempt*, and each token-exchange attempt are live behind `OBS_TRACING_ENABLED`, with the retry attributes on the attempt spans. Trace exemplars linking the latency histograms to these traces are live too (Story 47, SOL-152419) — see [Trace Exemplars](#trace-exemplars--implemented). See [Distributed Tracing](#distributed-tracing--interim-request-path-and-per-attempt-spans-wired). |
| Saturation visibility | **[Interim — logs only]** | Shipped as structured log lines behind `OBS_SATURATION_EVENTS_ENABLED`, **not** as the metric this schema describes. See [Load and Saturation Visibility](#load-and-saturation-visibility--interim--logs-only). |
| Resource attributes | **[Implemented]** | Shared identity resource on metrics and traces, plus the committed subset on every log line. See [Resource Attributes](#resource-attributes--implemented). |

Present-tense wording in a **[Planned]** section describes the **target** behavior under
review, not what the current build emits. Only capabilities tagged **[Implemented]** — more
than one now — are live today.

### Flag Defaults at GA

Each capability is off by default unless the table says otherwise. A flag is off because
turning it on commits us to something we cannot cheaply take back — a schema name, a
listening port, a compliance record — so each default-off flag carries a written condition
that would justify changing it. The conditions are recorded here so the decision is made
against a stated test rather than re-argued each release.

| Flag | Default | Why, and what would change it |
|---|---|---|
| `OBS_CORRELATION_ID_ENABLED` | `true` | The schema is W3C-standard (`traceparent`) and purely additive, so there is no name to regret. On from day one. |
| `OBS_METRICS_ENABLED` | `false` | Turning it on publishes every metric name and label in this document as a contract, and opens a second listener on `:9091`. The schema-review condition is satisfied (see [Schema Review Record](#schema-review-record)). It now flips when the Solace SDLC security review of metric label cardinality passes. |
| `OBS_METRICS_OTLP_ENABLED` | `false` (planned) | **Not in the current build** — ships with the OTLP push egress; see [Metrics](#metrics--planned-with-exceptions). Pushes metrics to a collector you run, and there is no safe default endpoint, so it is opt-in permanently, like tracing. It will require `OBS_METRICS_ENABLED`: setting it alone is a config error. |
| `OBS_AUDIT_LOG_ENABLED` | `false` | The audit schema is a compliance contract. Both original conditions are satisfied: the identity chain landed with OAuth token exchange, and the schema review is on record. It now flips when the Solace SDLC security review of the audit schema passes **and** `mcp_audit_events_dropped_total` is emitted — a best-effort audit stream is only defensible for compliance while a dropped record is visible rather than silent. |
| `OBS_TRACING_ENABLED` | `false` | Requires an OTel collector you deploy, and there is no safe default endpoint to send spans to. **Opt-in permanently** — this one is not waiting on a condition and will not default on. |
| `OBS_SATURATION_EVENTS_ENABLED` | `false` | Emits a `WARN` line per slow admission, onto the same log stream that carries audit records. Its original condition (a configurable threshold) is satisfied — see `observability.saturation_threshold_ms`. It now flips when the metric form of this signal replaces the log lines, so operators are not opted into per-request log volume to get it. |
| `OBS_AUTH_FAILURE_COUNTER_ENABLED` | follows `OBS_METRICS_ENABLED` | A counter that `/metrics` does not expose has no consumer. Set it explicitly to override in either direction. |

Panic recovery is not a flag: it is unconditional. `/livez` and `/readyz` are unconditional
for the same reason — the check is cheap and commits us to nothing.

No flag's v1 default changes before GA. Three of them — metrics, audit, and saturation — have
satisfied the condition they originally carried, and each stays off under the replacement
condition named above rather than flipping. Tracing and OTLP push are opt-in permanently.

### Schema Review Record

**Outcome: reviewed internally with feedback incorporated, then circulated twice for wider
review with no objections returned. The names ship as drafted.**

The review ran in two phases ahead of the GA freeze.

**Phase 1, internal review (from 2026-07-20).** The first draft drew substantive feedback
from Solace engineering and field leadership, and it changed this schema. Each item below is
checkable against the current document:

| Feedback | What changed |
|---|---|
| Do not align exclusively to one telemetry vendor path | Metrics are designed for **two egresses**, Prometheus scrape and OTLP push, from one instrument set. The OTLP push half is not in this build; see the [Metrics](#metrics--planned-with-exceptions) section. |
| Add `mcp_schema_version` and `mcp_build_info` | Both are in the schema and **emitted today**. |
| Carry a broker identifier on metrics | `broker` is a label on the tool RED metrics, the SEMP metrics, and the broker reachability gauges. It is deliberately absent where no broker is in scope at the measurement point, such as authentication failures and authorization denials, which are decided before a broker is selected. |
| Enrich `mcp_auth_failure_total` | It carries `reason`, across the five documented authentication-failure reasons. |
| A two-month window before locking the schema is too limiting | The compatibility commitment is **bounded, not absolute**: additive within a MAJOR, with an announced deprecation path. It replaced an earlier unqualified "never rename". See [Compatibility and Deprecation Policy](#compatibility-and-deprecation-policy). |
| The failure-event enum design needs scrutiny | `error_type` was promoted to a first-class shared label across metrics, logs, audit, and spans, so the four surfaces cannot disagree about why a call failed, and a test now asserts the vocabularies agree. |

**Phase 2, wider solicitation (2026-07-27 and 2026-08-18).** Two further rounds went out over
roughly seven weeks, the second asking named field and sales-engineering stakeholders to
carry the brief and schema to key customers, on the explicit reasoning that changing the
schema after GA is disruptive. **Neither round drew a response.**

Read phase 2 as the absence of an objection rather than as positive confirmation: no response
from a named customer operator came back, so nothing here records an operator having built a
dashboard or a SIEM query against these names and found them workable. The fallback is
deliberate. Names follow OpenTelemetry semantic conventions wherever conventions exist, and
the compatibility policy above is bounded rather than absolute.

**Feedback is still welcome and still worth sending**; see
[How to Give Feedback](#how-to-give-feedback). What the freeze changes is the cost of acting
on it, not our willingness to.

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
drafted. Adding a field later is always cheap. Renaming one after the freeze is not: it costs
you a deprecation cycle and a dashboard migration, even though we commit to giving you both.
So a name you would change is worth flagging now. See
[Compatibility and Deprecation Policy](#compatibility-and-deprecation-policy).

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

- `metrics_schema` (current: **1.5**), surfaced by the `mcp_schema_version` metric.
- `audit_schema` (current: **1.1**), surfaced as the `audit_schema_version` field on every audit
  event **and** as a label on `mcp_schema_version`, so both versions are discoverable from a
  scrape without ingesting audit events.

Versioning is `MAJOR.MINOR`. A **minor** bump is additive and backward compatible (a new
metric, a new audit field, a new `outcome` value). A **major** bump is reserved for a
breaking change (a rename or removal), which happens only after the deprecation cycle below.
Pin dashboards to `mcp_schema_version` and SIEM queries to `audit_schema_version`.

### Compatibility and Deprecation Policy

The commitment is **additive-only within a MAJOR version**, not "never rename". A name can
change. It cannot change without notice, and it cannot change out from under a dashboard you
have not had time to migrate.

| Change | Version effect | What you get |
|---|---|---|
| A new metric, audit field, or a new value in a closed set | MINOR bump | Nothing to do. Existing queries keep working. |
| A new **label** on an existing metric | MINOR bump | Existing selectors and `sum by (...)` keep working. A `without()`/`ignoring()` aggregation or a default one-to-one `rate(a)/rate(b)` match can silently change, because a new label changes the series identity those collapse or match on — see the note below. |
| Renaming or removing a metric name or an audit field | MAJOR bump, and only after the cycle below | An announcement, then at least two minor releases emitting both the old and the new form side by side, **untouched** — the old artifact is unaffected, so a dashboard or SIEM rule pinned to it keeps working exactly as before. |
| Renaming a **label key** | MAJOR bump, and only after the cycle below | An announcement, then at least two minor releases carrying both the old and the new key on every sample. This is a weaker guarantee than the row above: dual emission is possible, but adding the new key still changes the series identity of the metric consumers already query, so — same hazard as the MINOR row above — a `without()`/`ignoring()` aggregation or default vector match against it can silently change during the window. Where an untouched old series matters, ship the new key on a new metric name instead. |
| Renaming or removing a **value** in a closed set (`outcome`, `error_type`, `reason`, `audit_event_type`) | MAJOR bump, and only after the cycle below | An announcement and at least two minor releases of notice. **Not** dual emission — see the carve-out below. |

**A label change — new or renamed — changes series identity, which some queries do not survive.**
`mcp_tool_invocation_total{broker="b1"}` and `mcp_tool_invocation_total{broker="b1",
event_broker="b1"}` are different series. A plain selector or a `sum by (broker)` still
matches either shape fine. But `sum without (broker) (...)`, `ignoring(broker)`, and a default
one-to-one `rate(a_total[5m]) / rate(b_total[5m])` all require identical label sets, so the
added label silently stops them matching or stops them collapsing the way they used to — no
error, just a different number. A `rate()`/`increase()` window that spans the moment the label
is added or removed sees the old series go stale and a new one start, which can transiently
skew both. This is exposition-valid — the Prometheus/OpenMetrics text format does not require
one label-name set per metric family, and this server's OTel-based exporter does not enforce
one either — it is a query-correctness hazard, not a wire-format one, and it is why a
label-key rename cannot promise the same "the old stays untouched" guarantee a name rename
can.

**The deprecation cycle, in order:**

1. **Announce.** The deprecation lands in `CHANGELOG.md` and is marked in this document,
   naming the old form, the new form, and the earliest release in which the old one may
   disappear.
2. **Two minor releases of notice, dual-emitting where dual emission is possible.** For a
   metric name or an audit field, both the old and the new are emitted side by side,
   untouched, for the whole window, so a dashboard or SIEM rule written against either keeps
   working and you can verify the new query against live data before cutting over. For a
   label key, both keys are emitted on every sample, which still lets you verify the new query
   before cutover but — per the series-identity note above — does not leave the old series as
   untouched as a name rename does.
3. **Remove at a MAJOR bump.** The old form is removed only in a major version of the affected
   schema (`metrics_schema` or `audit_schema`), never in a minor.

**Carve-out: a closed-set value cannot be dual-emitted, so it gets notice instead.** A record
carries exactly one `error_type` and a sample exactly one `outcome`. There is no way to emit
the old and the new value "alongside" each other: on a single-valued field it is impossible,
and on a metric it would split one series into two and silently double every `sum()` written
against it. So a value rename gets the announcement and the same two-minor-release notice
window, during which the old value keeps being emitted unchanged and the new one is documented
and reserved, then the switch happens in one step at the MAJOR bump. Plan a value rename as a
cutover, not as a migration you can run both sides of.

`audit_event_type` is in this bucket for a different reason, worth stating because the
impossibility argument above does not reach it. It is the record **discriminator**, not a
field on a record, so dual-emitting it would mean two whole records per event rather than two
values in one field — technically possible. It still gets notice-then-cutover, because
dual-emitting the discriminator doubles **every** audit record for the whole window, not only
`operation` records, doubling volume and retention on the one signal you pay a SIEM to
store — a much larger cost than the [Audit
Trail](#audit-trail--interim--all-record-types-except-broker_authz_denied) coverage gaps
this schema already tolerates.

**The two schemas version independently, except where a vocabulary is shared.** A metrics
rename does not normally force an audit-schema major bump, and a SIEM rule pinned to
`audit_schema_version` is unaffected by it.

The test for the exception is **"is this closed set rendered by more than one signal?"**, not
which package it happens to live in. Three sets are shared today:

| Shared closed set | Rendered by |
|---|---|
| `outcome` | the metric label, the audit record, the `tool invoked` log field, and the span attribute |
| `error_type` | the metric label, the audit record, and the span attribute |
| The five `auth_failure` reasons | the `auth_failure` audit record and `mcp_auth_failure_total{reason}` |

Renaming a value in any of these is **one coupled breaking change that bumps both majors
together**. Being able to carry one predicate across metrics, audit, and traces untranslated
is the point of a shared vocabulary; letting the two schemas move independently there would
reintroduce exactly the disagreement it exists to prevent. A set only one signal renders —
the `authz_denied` and `broker_authz_denied` reason sets, for example — bumps only its own
schema.

Two surfaces sit outside this commitment, and say so where they are documented: the
`go_*` / `process_*` collectors, which are upstream Prometheus conventions rather than
Solace-defined schema (see [Go Runtime and Process
Metrics](#go-runtime-and-process-metrics)), and the interim log lines under [Load and
Saturation Visibility](#load-and-saturation-visibility--interim--logs-only), which are
diagnostic output that the eventual metric form replaces.

### How OTel Instrument Names Render on `/metrics`

The server registers OpenTelemetry instruments; the Prometheus exporter derives the published
name from the instrument name and its unit. Five rules cover every `mcp_*` name the exporter
publishes. (The `go_*` / `process_*` collectors do not pass through the exporter and keep their
upstream names unchanged — see [Go Runtime and Process
Metrics](#go-runtime-and-process-metrics).)

| Rule | Effect |
|---|---|
| `.` becomes `_` | The instrument `mcp.tool.invocation` publishes as `mcp_tool_invocation`. |
| Counters gain `_total` | `mcp.tool.invocation` (a counter) publishes as `mcp_tool_invocation_total`. |
| A unit becomes a suffix | The instrument `mcp.tool.invocation.duration`, unit `s`, publishes as `mcp_tool_invocation_duration_seconds`. Unit `1` is **silent on a counter but appends `_ratio` on a gauge**, which is why the two info gauges (`mcp_build_info`, `mcp_schema_version`) are registered with no unit at all rather than with unit `1`. |
| Resource attributes go to `target_info`, not to every series | See [Resource Attributes](#resource-attributes--implemented) for how to join to it. |
| **No `otel_scope_*` labels** | The exporter is constructed with `WithoutScopeInfo()`, so `otel_scope_name` and `otel_scope_version` appear on **no** series. They are on by default in the OTel Prometheus exporter, and suppressing them keeps the published label set exactly the label set documented here. Pinned by two tests: `TestExporterFidelity_ScopeInfoSuppressed` proves the option does what it claims in both directions, and `TestGoldenSchema` (`internal/observability/metrics/provider_test.go`) scrapes a real provider, so the production exporter cannot lose the option without failing. |

The names in this document are the **published** names, after these rules. You never need to
apply them yourself; they are stated so that a name you see on the wire and a name you read
here can be reconciled.

---

## Metrics — [Planned, with exceptions]

> _Status: **[Planned, with exceptions]**. Most instrument names, types, and labels below
> are the proposal under review, not yet wired in the build. Wired and emitted today: the
> `/metrics` endpoint itself, `mcp_build_info`, `mcp_schema_version`,
> `mcp_metrics_scrape_total`, `mcp_http_active_requests`, `mcp_tool_invocation_total`,
> `mcp_tool_invocation_duration_seconds`, `mcp_semp_request_total`,
> `mcp_semp_request_duration_seconds`, the OTLP **span** export-health counters
> (`mcp_otel_spans_exported_total` / `mcp_otel_spans_dropped_total`),
> `mcp_panic_recovered_total` (see [Panic Recovery](#panic-recovery--implemented)),
> `mcp_auth_failure_total` and `mcp_authz_denied_total` (see
> [Authentication Failures](#authentication-failures--implemented) and
> [Authorization Denials](#authorization-denials--implemented)), the
> `go_*`/`process_*` runtime collectors (see [Go Runtime and Process Metrics](#go-runtime-and-process-metrics)),
> and `mcp_broker_reachable`, `mcp_broker_unreachable_reason`, and
> `mcp_broker_last_result_timestamp_seconds` (see [Broker Reachability](#broker-reachability)).
> Assume any other metric below is not yet emitted._
>
> **Two metric groups below are documented but not emitted by any build yet**, and are marked
> as such where they are defined: the OTLP **metrics** export-health pair
> `mcp_otel_metrics_exported_total` / `mcp_otel_metrics_dropped_total{reason}` (see [OTLP
> Export Health](#otlp-export-health)) and `mcp_broker_authz_denied_total{tool,broker,reason}`
> (see [Broker-Side Authorization
> Denials](#broker-side-authorization-denials--not-yet-emitted)). Their series are **absent,
> not zero**, so an `absent()` alert on either fires today for the mundane reason that the
> instrument does not exist.

All metrics are served on the `/metrics` endpoint in Prometheus text exposition
format, behind `OBS_METRICS_ENABLED`. One exception: whether the two security counters
(`mcp_auth_failure_total` and `mcp_authz_denied_total`) are recorded has its own flag,
`OBS_AUTH_FAILURE_COUNTER_ENABLED`. It defaults to whatever `OBS_METRICS_ENABLED` is, so with
nothing set the counters are on exactly when metrics are. An explicit `false` suppresses both
while the rest of the surface stays on; their series are then absent, not zero. An explicit
`true` while `OBS_METRICS_ENABLED` is `false` has nothing to register against — there is no
exporter and no `/metrics` listener — so the server logs a `WARN` naming the flag at startup
and records nothing.

The `mcp_*` instruments can additionally be **pushed over OTLP**, behind its own flag,
`OBS_METRICS_OTLP_ENABLED`. The `go_*`/`process_*` collectors are scrape-only and are
**not** pushed — see [Go Runtime and Process Metrics](#go-runtime-and-process-metrics). The endpoint comes from the standard
`OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`. Push is off by
default and does not activate merely because an endpoint variable is present in the
environment; see [Decided Since the First Draft](#decided-since-the-first-draft) for why.
Setting `OBS_METRICS_OTLP_ENABLED=true` while `OBS_METRICS_ENABLED` is false fails config
load with an explicit error rather than emitting nothing quietly, because both egresses
share one meter provider.

**Temporality is always cumulative, explicitly forced regardless of environment.** This server
sets it in code rather than relying on the SDK's own default (which happens to already be
cumulative) or on whatever `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE` a customer may
have set cluster-wide for other services. This matters because Prometheus's own OTLP receiver
needs the experimental `otlp-deltatocumulative` feature flag to accept delta at all — shipping,
or silently inheriting, delta would break the exact interop this egress exists to provide for a
customer who points it at their own Prometheus.

**Transport security.** TLS is the default: with no override, the exporter dials the collector
over gRPC with the host's root CAs. That default is opted out of by the endpoint's own scheme —
`OTEL_EXPORTER_OTLP_ENDPOINT=http://collector:4317` (the spelling most collector quickstarts use)
or `OTEL_EXPORTER_OTLP_INSECURE=true` both downgrade to cleartext gRPC, silently, with no other
signal that the choice was made. This stream carries broker aliases, tool names, `error_type`,
and the identity resource (including `service.instance.id`, `cloud.region`) across a network
boundary the in-cluster Prometheus scrape never crosses; in cleartext that is passive topology
disclosure, and with no server authentication a collector can be impersonated to harvest it. Use
an `https://` endpoint. For a private CA, set `OTEL_EXPORTER_OTLP_CERTIFICATE` (or the
`_METRICS_` variant); for mTLS, the `_CLIENT_CERTIFICATE`/`_CLIENT_KEY` pair.

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
  problem from a tail-latency problem. It counts requests on `/mcp` only, and increments on
  request entry before authentication — so it includes requests later rejected with 401/403/413.
  A spike can therefore mean rejected traffic, not accepted work.

**Cardinality:** trivial (one series each, plus one per label value on the info metrics).

**Exposure.** The `/metrics` endpoint is unauthenticated and unencrypted, and defaults to a
wildcard bind (`:9091`, all interfaces). Restrict it with a NetworkPolicy, or bind it to
loopback for a co-located sidecar scraper. The series it exposes are low-sensitivity (build
version, schema versions, and — once tools run — tool names already public in
`docs/tools-reference.md`, broker aliases, broker hostnames via `server_address` on the SEMP
metrics, and usage timing), with two exceptions: `mcp_auth_failure_total{reason}` exposes a
readable key-rotation signal through `signature_invalid`, and `mcp_authz_denied_total{tool}`
tells a reader which tools authorization is refusing. Treat restricting the listener as the
default posture, not optional hardening. The listener is absent entirely unless
`OBS_METRICS_ENABLED` is set.

### Tool Invocations (RED)

The core Rate / Errors / Duration signal for every tool call that reaches its handler. A call
refused by tool authorization never reaches one, so it is absent here and counted by
`mcp_authz_denied_total` instead (when that counter is enabled; see below).

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_tool_invocation_total` | Counter | `tool`, `broker`, `outcome`, `error_type` | Solace |
| `mcp_tool_invocation_duration_seconds` | Histogram | `tool`, `broker`, `outcome`, `error_type` | Solace |

- `tool`: the MCP tool name (kebab-case, for example `get-broker-status`). Bounded by the
  number of tools the server exposes.
- `broker`: the configured broker alias, canonicalized to your configured casing. Bounded by
  the number of configured brokers plus two sentinels — `none` (a brokerless or pre-resolution
  call) and `unknown` (an alias that is not configured). The log line keeps the raw alias the
  caller typed; only the metric label is canonicalized, so a typo cannot mint a new series.
- `outcome`: see [The Outcome Vocabulary](#the-outcome-vocabulary).
- `error_type`: the failure cause, from the twelve values in
  [`error_type`](#error_type). Empty on any non-error outcome.
- Histogram buckets (seconds): `0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10`.

**Cardinality:** all label domains are finite. `error_type` is non-empty only on the error
path and is drawn from the closed set of twelve values above; `outcome` is one of three; and
`broker` is bounded to the configured aliases plus the `none`/`unknown` sentinels. The series
count is not a clean product of these domains, because several error types only ever occur
before a broker is resolved — `bad_request`, `missing_broker`, `not_found`, and
`unknown_broker` appear only with `broker=none` or `broker=unknown`, never against a configured
alias — so the real total is well under the naive product. CI enforcement of the closed sets is
planned for GA.

### SEMP Requests (RED, per Attempt) — [Implemented]

Request rate, errors, and latency for each call the server makes to a broker over SEMP,
recorded per retry attempt so you can see retry storms and per-broker latency.

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_semp_request_total` | Counter | `http_request_method`, `http_response_status_code`, `server_address`, `broker`, `api`, `operation`, `attempt` | Mixed (see the following list) |
| `mcp_semp_request_duration_seconds` | Histogram | same label set, minus `attempt` | Mixed |

- **OTel** labels (adopted from the OpenTelemetry HTTP semantic conventions,
  https://opentelemetry.io/docs/specs/semconv/http/http-spans/): `http_request_method`,
  `http_response_status_code`, `server_address`.
- **Solace** labels: `broker` (the configured alias), `api` (`v1` or `v2`), `operation`
  (the SEMP operation ID, or `unknown` if a call reaches the metric with no operation ID;
  for SEMPv1 this is always the constant `"SEMPv1"` because the protocol routes every
  command through one endpoint with no per-operation distinction),
  `attempt` (the retry attempt as an integer string, `"1"`, `"2"`, ...). `attempt` is on the
  **counter only**: the histogram omits it so its bucket series are not multiplied by the
  retry cap. Retry-storm detection reads the counter.
- **No operator-visible change (SOL-152422):** `attempt` already reflected the real try
  number before this release — the metrics transport owned the counter and bumped it on
  every attempt, with or without tracing. This release moves ownership of that counter to
  the new `semp.attempt` span, and the metric now reads it rather than bumping it. The
  values on the wire are unchanged; what changed is that the metric's `attempt` label and
  the span's `attempt` attribute are now backed by the same counter and cannot drift
  against each other. (A *tracing-only* deployment, metrics off, is where a naive design
  could have gone wrong — a span-local counter would have read `1` on every attempt — and
  that hazard is the one this design avoids, not a regression that shipped.)
- **The duration is the time to the response's first byte for one attempt.** It excludes the
  MCP-side admission wait (rate limiting and the in-flight cap) and the response-body read,
  and it excludes retry backoff between attempts — so it is broker round-trip latency, not
  end-to-end call time.
- **When no response arrives** — DNS failure, connection refused, TLS handshake failure,
  or a timeout — `http_response_status_code` is the **empty string**. The attempt is still
  counted; only the status is unknown. This follows the OTel convention of leaving the
  attribute unset when there is no response, which in Prometheus surfaces as `""` rather
  than an absent label. Alert on `http_response_status_code=""` to catch a broker you
  cannot reach at all, which no status-code range would match.
- Histogram buckets (seconds): `0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10`.
  **Deliberately coarser at the low end than the tool histogram**, because a SEMP call is a
  network round-trip to a broker and sub-millisecond resolution would buy nothing.

**Cardinality:** bounded by the product of these finite sets. `attempt` rides the counter
only — the histogram omits it, so the histogram's many bucket series are not multiplied by the
retry cap — and the empty status adds one value to the status dimension rather than an open set.
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
| `mcp_broker_last_result_timestamp_seconds` | Gauge (Unix seconds) | `broker` | Solace |

- `mcp_broker_reachable` is set passively from the result of real calls; it is not a
  heartbeat. A broker that has not received any call since pod start is absent from this
  gauge — use `absent(mcp_broker_reachable{broker="..."})` alongside `== 0` in reachability
  alerts to catch both cases.
- `reason` vocabulary: `credential_invalid` (HTTP 401 or 403), `unreachable` (connection
  refused, DNS failure, or I/O timeout), `broker_error_NNN` (5xx or 429, where NNN is the
  HTTP status code, e.g. `broker_error_503`). Other 4xx responses (404, 400, 409, …) classify
  as `reachable` because the broker answered. `broker_error_NNN` cardinality is bounded by
  the finite set of HTTP status codes.
- `mcp_broker_unreachable_reason` is **one-hot per broker**: every reason ever recorded for a
  broker is always present in the scrape — `1` for the current reason, `0` for all prior ones.
  This guarantees series never disappear mid-incident and `max by (reason)` never straddles
  two causes. Alert on `mcp_broker_reachable == 0`; use this gauge only to attribute the cause.
- `mcp_broker_last_result_timestamp_seconds` records when the broker's most recent SEMP call
  completed. Use `time() - mcp_broker_last_result_timestamp_seconds > 900` to alert on brokers
  that have gone silent (no traffic, so reachability is unknown).

**Cardinality:** `|broker|` for the first and third metrics; `|broker| x |reason|` for the
second, where `|reason|` grows only on distinct HTTP error status codes seen per broker.

### Authentication Failures — [Implemented]

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_auth_failure_total` | Counter | `reason` | Solace |

- `reason` is a closed set of five:

  | `reason` | Meaning |
  |---|---|
  | `invalid_token` | Catch-all: a malformed or unparseable JWT, an issuer mismatch, a static dev-token mismatch, or an `Authorization` header present but not bearer-shaped (wrong scheme, empty value). |
  | `expired` | The token's `exp` claim has passed. |
  | `audience_mismatch` | The token's audience does not match what this server expects. |
  | `signature_invalid` | A token-signing or JWKS-rotation failure, distinct from a malformed token. |
  | `missing` | No `Authorization` header was presented at all, or a token that verified fully but omitted the required `sub` claim (an IdP misconfiguration, not an absent caller) — both land here rather than getting a sixth value. Any credential-less request that reaches `/mcp` counts here, including a CORS preflight or a health check pointed at the wrong path, since the SDK rejects those with a 401 too. |

- The values are deliberately coarse so no token content is ever exposed as a label.
- There is no `broker` label. Authentication happens at the HTTP boundary, before any broker
  is selected, so there is no broker in scope to name. Use the resource attributes on
  `target_info` to attribute failures to a server instance.
- All five `reason` series are seeded at zero when the counter is registered, so they are
  present from the first scrape and `increase(mcp_auth_failure_total[5m]) > 0` fires on a
  process's first rejected token — after a key rotation, the first `signature_invalid` is the
  sample that matters. A flat zero means "no failures", not "no data".
  `absent(mcp_auth_failure_total)` means the counter is not registered: metrics are off,
  `OBS_AUTH_FAILURE_COUNTER_ENABLED` was set to `false`, or the counter was never wired.
- Recorded behind `OBS_AUTH_FAILURE_COUNTER_ENABLED` (see the flag note at the top of
  [Metrics](#metrics--planned-with-exceptions)); the same flag governs `mcp_authz_denied_total`.

**Cardinality:** `|reason|` (five values).

### Authorization Denials — [Implemented]

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_authz_denied_total` | Counter | `tool`, `reason` | Solace |

- `reason` is a closed set of two: `missing_claim`, `not_permitted`. Same values as the
  `authz_denied` audit record, taken from the same decision, so the metric and the audit
  stream cannot disagree.
- `tool` **is** a label here, unlike on `mcp_auth_failure_total`. Authorization runs after the
  tool is known, so the tool name is in scope and is the first thing you need on a denial.
- Emitted only where tool authorization is enabled, and behind the same
  `OBS_AUTH_FAILURE_COUNTER_ENABLED` flag as `mcp_auth_failure_total`. With either off, the
  series is absent rather than zero. Unlike `mcp_auth_failure_total`, nothing is pre-seeded: a
  series appears on the first denial for a given `tool` and `reason`, so `absent()` is not a
  usable alert here — alert on `increase()` instead.

**Cardinality:** `|tool| x 2`.

Note the scope: this counter is **hop 1 only** — this server refusing an authenticated caller
the tool they asked for. A refusal by the *broker* is a different counter, next.

### Broker-Side Authorization Denials — [Not yet emitted]

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_broker_authz_denied_total` | Counter | `tool`, `broker`, `reason` | Solace |

> **Not emitted yet.** The name, its label set, and its `reason` vocabulary are part of this
> schema and safe to write a rule against today; the emission site ships with Story 49
> (SOL-153332), alongside the `broker_authz_denied` audit record this counter mirrors. Until
> then the series is **absent, not zero**. This matches the treatment the audit record itself
> gets — see [Authentication Events](#authentication-events).

- `reason` is a closed set of one today: `permission_denied`. Same value as the
  `broker_authz_denied` audit record, taken from the same decision, so the metric and the
  audit stream cannot disagree — the same coupling `mcp_authz_denied_total` has to
  `authz_denied` at hop 1.
- **`broker` is a label here, and deliberately is not one on `mcp_authz_denied_total`.** This
  is the asymmetry to understand before you write a dashboard against both:

  | | Where the decision is made | `broker` label |
  |---|---|---|
  | `mcp_authz_denied_total` (hop 1) | This server, before dispatch | **No** — the only broker value in scope is the caller's unvalidated alias |
  | `mcp_broker_authz_denied_total` (hop 2) | The broker, after dispatch | **Yes** — a real broker made the decision, so naming which one is the first thing you need |

  The precise reason, since it is easy to state wrongly: a hop-1 denial *does* have the
  caller's `broker` argument in hand, but authorization is composed **outside** the tool
  manager and so has no broker pool to canonicalize that argument against. The only value it
  could label with is the raw string the caller typed — unbounded, untrusted input, which as a
  metric label would hand any caller control over this counter's cardinality. That is the same
  reasoning that keeps the raw alias off the `broker` span attribute. Hop 2 has no such
  problem: the call already resolved to a configured broker, and that broker is what refused,
  so a denial that is a policy gap on one broker and correct on another is only
  distinguishable with the label.
- Behind `OBS_METRICS_ENABLED`, like the rest of the scrape surface. Nothing is pre-seeded, so
  `absent()` is not a usable alert — alert on `increase()`.

**Cardinality:** `|tool| x |broker| x 1`.

**A hop-2 denial does not suppress the call's other signals.** Execution had already started
by the time the broker refused, so the same call also produces an `mcp_tool_invocation_total`
sample with `outcome=error` and — for a destructive tool — an `operation` audit record.
Counting denials means summing this counter, not counting calls.

### Audit Pipeline Health

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_audit_events_dropped_total` | Counter | none | Solace |

Increments if an audit event cannot be written (see [Audit Delivery](#audit-delivery)). A
flat-zero series is your evidence that no audit event was lost. Alert on any increase.

### Panic Recovery — [Implemented]

> _Unlike the rest of this section, this counter **is** wired in the current build. It is
> emitted on `/metrics` whenever `OBS_METRICS_ENABLED` is on._

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_panic_recovered_total` | Counter | `boundary` | Solace |

One counter for the two panic nets that guard a request's own goroutine, separated by
`boundary` rather than split into two metric names, so a single alert covers both:

- `http` — the HTTP middleware wrapping the whole mux. The panicking request gets a clean
  `500`; the process keeps serving.
- `tool` — the MCP tool-dispatch wrapper. The panicking call gets a sanitized tool result
  (`isError: true`, `retryable: false`) over a successful MCP call, per the spec's
  application-error convention.

**Cardinality:** 2. Neither label value can be named from outside the package that owns the
counter, which exposes one recording function per boundary and no way to pass an arbitrary
one. A future recovery site cannot widen this series set without a deliberate change there
and a matching row here.

**Both series are published at zero from startup**, before anything panics. A counter that
appeared only on its first increment would defeat the alert below: PromQL's `increase()`
needs two samples in the window, so the sample that creates a series is only a baseline and
a process's first panic would pass unnoticed. Seeding also means a flat zero reads as
"nothing panicked" rather than "No data", and makes `absent(mcp_panic_recovered_total)` a
usable alert for "metrics are on but the counter was never wired".

A recovered panic is a bug that reached production, not a load condition. **Alert on any
increase**, on either boundary. Each increment is paired with an `event=panic_recovered`
ERROR log carrying the panic's Go type and a stack trace — the counter tells you it
happened, the log tells you where.

**What this counter does not cover.** Only panics on the request's own goroutine. A panic
on a goroutine a handler *spawns* is recovered somewhere else and is not counted, even
though it happened during a request:

| Site | Where it fires |
|---|---|
| `internal/safego` | Fan-out workers in the broker-status, queue-metrics and discard-stats handlers, and in the composite executor |
| `internal/tokenexchange` | The singleflight goroutine that performs the IdP round trip |
| `internal/observability/tracing` | The periodic OTLP self-stats loop (genuinely off the request path) |

All three log `event=panic_recovered` and convert the panic into an error, which then
returns through the handler normally — so the tool-dispatch net never sees it and
`boundary="tool"` does not move. **An alert on the log attribute has wider reach than an
alert on this metric.** Use the metric for the paging signal and the log for coverage.

`http.ErrAbortHandler` is exempt on the `http` boundary. `net/http` uses it as a
"client went away, say nothing" sentinel, and the MCP streamable/SSE path raises it on
ordinary client disconnect; counting it would make a panic alert fire on routine teardown.

Recovery is unconditional and does not depend on this counter. With `OBS_METRICS_ENABLED`
off, no instrument is registered, both recovery sites still recover and still log, and the
increment is a no-op.

### OTLP Export Health

Self-observation for the OTLP exporters: two pairs, one per egress, both **[Implemented]** as
of Story 46 (SOL-152418).

**The span pair.**

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_otel_spans_exported_total` | Counter | none | Solace |
| `mcp_otel_spans_dropped_total` | Counter | `reason` | Solace |

**The metrics pair.**

| Metric | Type | Labels | Basis |
|---|---|---|---|
| `mcp_otel_metrics_exported_total` | Counter | none | Solace |
| `mcp_otel_metrics_dropped_total` | Counter | `reason` | Solace |

> **Registered only when OTLP metrics push is enabled** (`OBS_METRICS_OTLP_ENABLED`, Story 46,
> SOL-152418). With push off, this pair's series are **absent, not zero** — the same treatment
> [`broker_authz_denied`](#audit-trail--interim--all-record-types-except-broker_authz_denied)
> and `mcp_audit_events_dropped_total` get: designed and schema-accepted, but a series only
> exists once its emitter is both shipped and turned on. Alert on the span pair unconditionally,
> and add the metrics pair once push is enabled in your deployment.

**The span pair's reach depends on both flags, not just one.** The counters are always
registered in-process while tracing is enabled (`OBS_TRACING_ENABLED`); they reach this scrape
surface only when a meter provider also exists to register them against, i.e. only when
`OBS_METRICS_ENABLED` is **also** on. Tracing on with metrics off keeps the totals in-process
only — reported solely by the periodic `event=otel_self_stats` INFO log (see [Distributed
Tracing](#distributed-tracing--interim-request-path-and-per-attempt-spans-wired)) — so an alert on
`mcp_otel_spans_dropped_total` sees a permanently absent series in that mode, which reads as
healthy rather than as "not exposed here." The metric pair's own flag is OTLP metrics push
(`OBS_METRICS_OTLP_ENABLED`, not `OBS_METRICS_ENABLED`, which governs the scrape surface alone;
see [Metrics](#metrics--planned-with-exceptions)) — so the two pairs can legitimately be in
different states in one process: spans exporting and their counters live, with the metrics
pair absent because push is off. `reason` is a closed set of four, `queue_full`,
`export_timeout`, `export_error`, `shutdown` — but which of the four are live differs by pair,
not a single caveat that applies to both:

- **Span pair:** `export_timeout`, `export_error`, and `shutdown` are live and distinguish real
  causes — a gRPC-status timeout from the exporter, any other export failure (including a
  refused connection), and an in-progress export that didn't finish flushing before shutdown's
  deadline, respectively; `shutdown` counts one event per incomplete drain, not one per dropped
  span, since the SDK doesn't report how many spans it failed to flush. `queue_full` is
  **reserved but currently inert** (SOL-152420): the OTel Go SDK's batch span processor drops
  queue-overflow spans against an internal counter with no public accessor, so there is no
  supported way to surface that specific reason from outside the SDK today. The value stays in
  the schema for forward compatibility; do not alert on it as if it were live.
- **Metrics pair:** only `export_timeout` and `export_error` are live. `queue_full` is not
  merely un-called but **structurally inapplicable**: a `PeriodicReader` collects into a reused
  buffer on its own goroutine and has no queue to overflow, unlike the span pair's batch
  processor. `shutdown` is also not a counter on this pair — an incomplete flush logs a WARN
  instead, because by the time `Provider.Shutdown` could record anything, that same call has
  already torn down the reader the scrape reads from, making a counter touched there
  unobservable rather than merely delayed. Do not alert on either as if they behaved like their
  span-pair counterparts.

**These live on the scrape surface deliberately.** Diagnosing a broken push must not depend on
the push working, so you can answer "is our OTLP export landing?" from Prometheus even when the
collector is the thing that is down. The scrape path and the push path fail independently by
design.

**A NetworkPolicy egress rule to the collector's host and port is required** if your cluster
enforces default-deny egress — a blocked gRPC dial fails silently rather than at startup, so the
first sign of a missing rule is telemetry that never arrives, not an error anywhere in this
server's own logs. The failure signature to alert on:
`mcp_otel_metrics_dropped_total{reason="export_error"}` rising while
`mcp_otel_metrics_exported_total` stays flat.

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

### Trace Exemplars — [Implemented]

The two latency histograms (`mcp_tool_invocation_duration_seconds` and
`mcp_semp_request_duration_seconds`) carry **trace exemplars** when both metrics and tracing are
enabled, so a slow bucket on a Grafana panel links straight to the trace that produced it and
you skip correlating by timestamp. The matching `_total` counters carry them too.

Four things to know. Each is a reason exemplars can be missing from a scrape that is
otherwise perfectly healthy, and none of them is visible from the scrape itself:

- **Your Prometheus must negotiate OpenMetrics to receive them.** Exemplars are not part of the
  older Prometheus text exposition format. Recent Prometheus versions request OpenMetrics by
  default; if yours does not, exemplars will be silently absent from an otherwise healthy
  scrape. To confirm by hand:

  ```
  curl -H 'Accept: application/openmetrics-text; version=1.0.0; charset=utf-8' \
    http://<host>:<metrics-port>/metrics
  ```

  Success looks like an exemplar appended as a `# {trace_id="…",span_id="…"} <value>
  <timestamp>` suffix on a bucket line. A plain `curl` with no `Accept` header returns the
  older text exposition and shows none — that result on its own is not a defect, only a
  scraper that has not asked for OpenMetrics.
- **An exemplar can only point at a *sampled* trace.** Under a low `OTEL_TRACES_SAMPLER_ARG`
  most buckets carry no exemplar. That is expected, not a gap. Raise the sampler argument if
  exemplar coverage matters to you more than collector volume; it is a sampling trade-off, not
  a defect.
- **With `OBS_TRACING_ENABLED` off, the histograms are unchanged and simply carry no
  exemplars.** Metrics do not depend on tracing being on: same series, same label keys, same
  bucket counts either way.
- **`OTEL_METRICS_EXEMPLAR_FILTER` overrides all of the above.** This server ships no default
  of its own and honors the standard OpenTelemetry SDK contract, whose default is
  `trace_based` — attach an exemplar when a sampled span is active, and otherwise not. That
  is the behavior the three points above describe. Setting `always_off` suppresses every
  exemplar even under full sampling, and it is the second thing to check when exemplars are
  missing. Setting `always_on` is **not recommended**: it attaches an exemplar even when no
  span was active, carrying an empty `trace_id` that links nowhere, which a Grafana panel
  renders as a dead link.

Exemplars add no new label keys and no new series.

The server emits the exemplars; turning them into clickable panel links is a dashboard
concern. You point your latency panels at a Tempo or Jaeger data source, and a panel with no
trace data source configured renders as a plain histogram.

### Go Runtime and Process Metrics

Standard `go_*` and `process_*` collectors from the Prometheus Go client library
(`collectors.NewGoCollector()` and `collectors.NewProcessCollector()`): goroutine count,
garbage-collection timing, memory stats, file descriptors, CPU. These are live whenever
`OBS_METRICS_ENABLED` is on, with no extra configuration.

**Naming.** These are upstream Prometheus conventions, not Solace-defined schema. A future
`client_golang` upgrade that renames them is not a breach of the additive-only commitment.

**Scrape-only.** These collectors are registered directly into the Prometheus registry, not
through the OTel meter API. They appear on `/metrics` but not on the OTLP metrics stream
(`OBS_METRICS_OTLP_ENABLED`). An OTLP-only consumer receives the `mcp_*` instruments and
resource attributes, but not `go_*` or `process_*`. If your OTLP pipeline shows no `go_*`
metrics, this is expected — scrape `/metrics` to get them.

**Absent from the OTLP push egress (SOL-152418, Story 46).** These two collectors register
directly against the Prometheus `client_golang` registry, never through the OTel meter provider
the `mcp_*` instruments share — so the OTLP reader, which only observes what passes through that
meter provider, never sees them. An OTLP-native APM ingesting this server's pushed metrics will
not show `go_*`/`process_*` panels; that gap is structural; not a bug to report.

---

## Audit Trail — [Interim — all record types except `broker_authz_denied`]

> _Status: **[Interim]** (SOL-152090, SOL-152096, SOL-152097). `operation` records for
> destructive tool calls are emitted today behind `OBS_AUDIT_LOG_ENABLED`, and the whole
> record schema below is enforced in code by one constructor. `auth_success`, `auth_failure`,
> `authz_denied`, and `broker_auth_retry` are also emitted today, behind the same flag
> (SOL-152097) — see [Authentication Events](#authentication-events). `audit_drop` is
> emitted. The one record type still unemitted is `broker_authz_denied`, landing with
> SOL-153332. Write your SIEM rules against the schema; expect that one record type to start
> appearing rather than to change shape._

One JSON event is emitted per **destructive** tool call (for example `disconnect-client`,
`delete-queue`, `delete-message-vpn`), at completion, with the outcome known. Read-only calls
are not audited. Authentication lifecycle events are also emitted (see [Authentication
Events](#authentication-events)). The stream is enabled with `OBS_AUDIT_LOG_ENABLED`.

> **The trigger is the `destructiveHint` annotation, not "did this change state".** Know the
> gap before you rely on this stream as your complete record of change. The emitter branches
> on a tool's `destructive` annotation (`internal/tools/manager.go`), and the `create-*` tools
> are annotated `destructive: false` because creating an object is additive. So these seven
> tools change broker state and emit **no** `operation` record:
>
> `create-message-vpn`, `create-queue`, `create-queue-subscription`, `create-topic-endpoint`,
> `create-rdp`, `clear-queue-stats`, `clear-client-stats`.
>
> An access review that needs object *creation* covered cannot get it from the audit stream
> today; join to the `tool invoked` operational log line for those tools instead. Widening the
> audit trigger from "destructive" to "all write tools" is a change to this schema's coverage,
> not to its shape, and is not made here.

> **The second gap: a call has to clear broker resolution and argument validation before it is
> audited.** The record is built only once the arguments hash is computable, which happens
> after the broker is resolved and the arguments pass schema validation. An unknown tool, a
> missing or unresolved broker, or a validation failure returns its error normally but writes
> no `operation` record — the call never reached the destructive gate. Unlike the annotation
> gap above, this is not a coverage decision; it is a call that never started, so there is
> nothing yet to audit.

Every event carries a top-level `"event": "audit"` tag so your log shipper can route the
audit sub-stream to a dedicated SIEM index.

**One `operation` record per call, not a started/completed pair.** The record is written when
the call finishes, so it carries the outcome and duration without your having to join two
records. A recovered panic still produces one, carrying `outcome: error`,
`error_type: panic`, and `panic_recovered: true` — a destructive handler that crashed may
already have changed the broker, so its absence must never read as "nothing was attempted".

**"One record per call" bounds the `operation` type, not the total.** A call the broker
denies at hop 2 on a **destructive** tool produces its `operation` record *and* a
`broker_authz_denied` record
(SOL-153332), because execution had already started. Match on `audit_event_type` rather than
counting records per `correlation_id`.

**With `OBS_AUDIT_LOG_ENABLED` off**, a destructive call instead logs the pre-flip
`executing destructive operation` WARN it always has, and no audit record is written. The
capability is inert when off, not degraded.

**Set `log_level` to `info` or lower.** An `operation` record is emitted at `INFO`, so on a
server running at `warn` or `error` it is filtered out before it reaches the log. The server
detects that and emits an `audit_drop` record instead. The drop is emitted at `ERROR`, the
highest level `log_level` accepts, so it survives **every** supported log level — including
the one that suppressed the record it is reporting. A stream of drops with no `operation`
records means your log level, not your flag.

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
| `error_type` | Why an operation failed; present on `outcome: error` only. Five of the twelve values reach an audit record, see [`error_type`](#error_type) | string (closed set) |
| `panic_recovered` | On `operation` only: present and `true` when a destructive handler crashed and was recovered (`outcome: error`, `error_type: panic`) | boolean |
| `arguments_hash` | SHA-256 over an RFC 8785 (JCS) canonicalization of the call arguments | hex string |
| `correlation_id` | Join key to logs, traces, and the broker-side entry | string |
| `reason` | Why authentication or authorization failed; present on `auth_failure`, `authz_denied`, and `broker_authz_denied` | string (closed set, one per record type) |
| `dropped_audit_event_type` | On `audit_drop` only: which `audit_event_type` could not be built or written | string (same closed set as `audit_event_type`) |
| `audit_schema_version` | The schema version, for query pinning | string (`1.1`) |

**`audit_event_type`** is a closed set of seven: `operation` (a destructive tool call),
`auth_success`, `auth_failure`, `authz_denied`, `broker_authz_denied`, `broker_auth_retry`,
and `audit_drop`.

**Which fields appear on which record.** Not every field is on every record, so a SIEM author
can tell record kinds apart from field presence alone.

`event`, `audit_event_type`, `timestamp_utc`, and `audit_schema_version` are on all seven.
`correlation_id` is on all seven **when the request carries one** — it is omitted rather than
written empty, so it is absent with `OBS_CORRELATION_ID_ENABLED` off and on records that do
not belong to a request.

Reading the table: **yes** means always present, **opt** means present when there is a value
(see the notes below each column's meaning of that), and **—** means never present.

| `audit_event_type` | `outcome` | `error_type` | `reason` | `tool` | `arguments_hash` | `started_at_utc`, `duration_ms` | `broker` | `principal.sub`, `agent_client_id` | `dropped_audit_event_type` | `panic_recovered` |
|---|---|---|---|---|---|---|---|---|---|---|
| `operation` | yes | on `error` only | — | yes | yes | yes | yes | opt | — | opt |
| `auth_success` | — | — | — | — | — | — | — | opt | — | — |
| `auth_failure` | — | — | yes | — | — | — | — | opt | — | — |
| `authz_denied` | — | — | yes | yes | — | — | — | opt | — | — |
| `broker_authz_denied` | — | — | yes | yes | — | — | yes | opt | — | — |
| `broker_auth_retry` | `success` or `error` | — | — | — | — | opt | yes | opt | — | — |
| `audit_drop` | — | — | — | opt | — | — | opt | — | opt | — |

- **The identity columns are `opt`, not `yes`.** `principal.sub` and `agent_client_id` are
  present whenever a principal was authenticated, and absent when there is none to name —
  with `mcp_client_auth.mode: disabled` no token is verified, so no record in that deployment
  carries them. **Do not use the absence of `principal.sub` to discriminate a record kind.**
  `agent_client_id` is additionally absent when the IdP issued no `client_id` claim.
- **`started_at_utc` and `duration_ms` are `opt` on `broker_auth_retry`.** They are permitted
  by the schema; the current emitter (SOL-152097) does not set them, since the retry policy
  does not track a per-attempt start time today.

This table is enforced, not merely documented. A single constructor
(`internal/observability/audit`.`NewEvent`) builds every record and rejects any combination
outside the table, so two emission sites cannot produce two shapes of the same record kind.

- **`auth_success` and `auth_failure` carry no `outcome`.** The record type already says what
  happened, so one predicate does the job of two.
- **On `auth_failure` the principal is unknown by definition**, since authentication is what
  failed. `principal.sub` and `agent_client_id` appear only when the token's signature, issuer,
  audience, and expiry all verified and a claim-level check *after* that point is what rejected
  it (for example, a malformed `scope` claim on an otherwise-valid token). An expired,
  audience-mismatched, wrong-issuer, malformed, or signature-invalid token carries neither
  field: each of those is rejected before any claim is readable server-side, so there is
  nothing yet to attribute the record to.
- **`authz_denied` and `broker_authz_denied` are the two hops of the same question.**
  `authz_denied` is this server refusing an authenticated caller the tool they asked for.
  `broker_authz_denied` is the broker refusing the exchanged identity, so it also names the
  `broker` that refused — the only column in which the two rows differ. On a destructive tool
  a hop-2 denial **coexists** with the call's `operation` record rather than suppressing it,
  because execution had already started by the time the broker refused. On a non-destructive
  tool there is no `operation` record to coexist with, so the denial stands alone.
- **`audit_drop` is a notice, not an outcome.** It reports that a record could not be written.
  It carries no principal, `outcome`, or `arguments_hash` — there is no surviving record for it
  to describe, and it must never become a second place raw arguments could leak. It carries
  `tool`, `broker`, and `dropped_audit_event_type` when the call site knows them, so a
  reviewer working inside the audit stream (`event="audit"`) can attribute the gap without
  joining to the operational log line — necessary at `log_level: warn` or above, where a
  successful call's own `tool invoked` line is filtered out but the drop, at `ERROR`, is not.
  All three are optional: a drop can happen before any of them is known. It is emitted at
  `ERROR`, above every other record type, so that it survives the log level that suppressed
  whatever it is reporting.

**Time fields carry their zone or unit in the name:** `_utc` for an instant, `_ms` for a
duration. Hence `timestamp_utc` and `started_at_utc` alongside `duration_ms`. Names freeze at
GA, so the rule is stated here for any field added before then.

**`principal` is a nested object, and `principal.sub` is a path into it** — not a literal key
with a dot in it. A record carries `"principal": { "sub": "..." }`. It is the schema's only
nested field; every other field is flat and snake_case. The nesting is deliberate: it leaves
room for a second member without renaming a field, which is what makes deferring
`preferred_username` ([Q-013](#decided-since-the-first-draft)) a reversible choice. Collectors that flatten nested
objects render it as `principal.sub` regardless, which is why the preceding tables use the
dotted form.

**`principal` identity.** The audit event records only `principal.sub`, the opaque OIDC
subject of the human user. That subject is read once from the verified token and carried end
to end through token exchange, so the broker's own SEMP log records the same user. The full
claim set propagated for that exchange is `sub`, `scope`, `client_id`, `iss`, `jti`; of these,
only `sub` is written to the audit event. A human-readable username (`preferred_username`) is
**deliberately omitted in v1**: it is directly identifying PII that would land in an
append-only store. This is a settled decision, not an open question — see
[Q-013](#decided-since-the-first-draft) for the reasoning and for what would change it.
Adding the field later is a pure addition under a minor bump.

**`arguments_hash`.** Lowercase hex SHA-256 (FIPS 180-4) over the
[RFC 8785](https://www.rfc-editor.org/rfc/rfc8785) JSON Canonicalization Scheme (JCS) form of
the arguments (keys sorted recursively by UTF-16 code unit, ES6 number serialisation,
insignificant whitespace removed, array order and nulls preserved). The hash is deterministic,
so an auditor can recompute it from the same arguments to prove a recorded event corresponds
to a specific call, without the raw argument values ever being stored.

**Reproducing the hash.** The digest is over the `arguments` object of the `tools/call`
request with two changes applied first, both of which an auditor must reproduce to get the
same digest:

1. **`broker` is removed.** The parameter is auto-injected into every tool's schema
   (`internal/tools/register.go`) rather than being part of the tool's own arguments, and
   broker aliases resolve case-insensitively — so if `broker` stayed in the digest, a call
   sent as `"broker": "PROD"` and one sent as `"broker": "prod"` against the same broker would
   hash differently despite being the same operation. Removing it before hashing means the
   digest cannot be salted by the caller's casing; the record's own `broker` field (always the
   *configured* casing) is the place to look for which broker a call targeted.
2. **A value on the [Never-Log list](internal/secure-logging-rules.md) is replaced with the
   fixed placeholder `[REDACTED]`, recursively.** A key matches by the same case-insensitive
   substring test as the `ReplaceAttr` net on log output — `password`, `token`, `secret`,
   `authorization`, `credential`, `api_key`, `private_key` — because at least one composite
   tool (`update-message-vpn`) accepts a free-form config object with no schema constraint on
   its keys, and the bundled SEMPv2 spec exposes password fields on that exact operation. The
   digest also redacts three terms with no log-attribute equivalent — `certcontent`, `keytab`,
   `passphrase` — covering certificate/key *content* fields such as
   `replicationBridgeAuthenticationClientCertContent`, a PEM private key the same operation
   accepts. These three are audit-only: the seven-pattern list above stays a byte-identical
   mirror of `cmd/server`'s own redaction list, so diffing the two for drift stays meaningful.
   The placeholder is fixed rather than derived from the original value, so changing a redacted
   field's value does not change the digest — a digest that varied with a secret's value would
   itself be a comparison oracle over that secret.

A call that sends no `arguments` field, or one whose remaining arguments are empty after
`broker` is removed, hashes as the empty object `{}`. Any conformant RFC 8785 implementation
reproduces the rest — for example, in Python:

```python
# pip install rfc8785
import hashlib, rfc8785

SENSITIVE = ("password", "token", "secret", "authorization", "credential", "api_key",
             "private_key", "certcontent", "keytab", "passphrase")

def redact(value):
    if isinstance(value, dict):
        return {k: "[REDACTED]" if any(s in k.lower() for s in SENSITIVE) else redact(v)
                for k, v in value.items()}
    if isinstance(value, list):
        return [redact(v) for v in value]
    return value

arguments.pop("broker", None)
print(hashlib.sha256(rfc8785.dumps(redact(arguments))).hexdigest())
```

Join a record to its originating request on `correlation_id`; recomputing the hash from the
original `tools/call` arguments (after the two transforms above) confirms it corresponds to
that specific call.

The server uses [`github.com/gowebpki/jcs`](https://github.com/gowebpki/jcs) (Apache-2.0)
rather than a hand-rolled canonicaliser, deliberately. Number serialisation, Unicode
handling, and recursive key ordering are where a home-grown implementation goes subtly wrong,
and the failure mode is a hash your tooling cannot reproduce and our tests would not catch.

**The audit schema is additive-only within a MAJOR version.** Within a major version fields
are added, and the closed sets (`audit_event_type`, `outcome`, `error_type`, `reason`) only
grow. An additive change bumps the minor `audit_schema_version`. A rename or removal bumps the
major, and only after the announce-then-dual-emit cycle in [Compatibility and Deprecation
Policy](#compatibility-and-deprecation-policy) — you get at least two minor releases carrying
both the old and the new form before the old one can go. Pin your queries to
`audit_schema_version` and a new field will not break them.

### Authentication Events

Alongside destructive-operation events, the audit stream records authentication lifecycle
events. The following names are distinct **event types**, not values of the shared `outcome`
field:

- `auth_success`, carrying `principal` and `agent_client_id`.
- `auth_failure`, carrying `reason` (same closed set as `mcp_auth_failure_total`).
- `broker_auth_retry`, carrying `broker` and `outcome`, for a broker-side 401 and
  cookie-clear/re-auth retry. **`outcome` answers "did the credential problem get resolved?",
  not "did the call succeed?"** — a 401 that recovers into a broker overload (503) which then
  exhausts its own retry cap is `outcome: success` here even though the call itself ultimately
  fails; join on `correlation_id` to the call's own `operation` record (or its absence, for a
  read-only call) to see the call's actual disposition.

**Volume note: `auth_success` and `auth_failure` do not follow the "one record per
destructive call" rate above.** Each is emitted once per authenticated or rejected
request to `/mcp` — every JSON-RPC POST, not only the destructive ones — so size SIEM
ingest against request volume, not destructive-call volume. `auth_failure{reason=missing}`
in particular is emitted before any authentication succeeds, so an unauthenticated caller
(a load-balancer probe, a scripted request loop) can trigger it with no rate limit of its
own; size retention for that one `reason` value with that in mind.

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

**Broker-side denial has its own record type: `broker_authz_denied`.** Authorization is
checked twice on any call that reaches the broker, destructive or read-only. Hop 1 is this
server deciding whether the caller may use the tool. Hop 2 is the broker deciding whether the
exchanged human identity may perform the SEMP operation, and a refusal there emits
`audit_event_type: broker_authz_denied` carrying `tool`, `broker`, the principal, and a
single-value closed `reason` set:

| `reason` | Meaning |
|---|---|
| `permission_denied` | The broker refused the exchanged identity the SEMP operation behind this tool. |

The two hops read differently in a query, deliberately. A hop-1 denial means nothing was
attempted. A hop-2 denial means the call *ran* and the broker stopped it, so on a destructive
tool it comes with an `operation` record of its own carrying `outcome: error`. **Do not write
a query that assumes one audit record per call** — a hop-2 denial on a destructive tool
produces two, and both are correct. (On a non-destructive tool it produces one, since that
tool never writes an `operation` record at all.) Match on `audit_event_type` instead of
counting.

> **Not emitted yet.** The record type is part of the schema and the constructor accepts it,
> so a SIEM rule can be written against it today. The emission site ships with SOL-153332.
> Its metric counterpart, `mcp_broker_authz_denied_total`, is documented under [Broker-Side
> Authorization Denials](#broker-side-authorization-denials--not-yet-emitted) and ships in the
> same story.

### Canonical Audit Queries

Four queries cover most of what an access review, a SOC 2 walkthrough, or a security
investigation asks of this stream. Each is given as a field predicate over records already
routed by `event="audit"`, because that is the one filter every shipper applies; translate the
predicate into your own SIEM's syntax. A Splunk SPL rendering is shown for the first as a
worked example of the translation.

**Pin every rule to `audit_schema_version`, as [Schema Versioning](#schema-versioning) already
tells you to.** The four queries below do it explicitly, with the current version as a
placeholder **you must set to your deployment's actual `audit_schema` before the rule goes
live, and update when it bumps.** A pinned query that stops matching after an upgrade has
detected schema drift, which is the point. But a rule copied fresh *after* the bump, with no
before-and-after to notice, is different: it returns zero rows on every run and looks like a
clean estate rather than a stale pin — check the pinned version against the deployment's
actual `audit_schema_version` before concluding anything from an empty result, especially on
query 1, where zero rows reads as "no privileged changes" to whoever is running the review. An
unpinned query is worse in the other direction: it keeps matching and silently spans two
contracts.

**1. Destructive-operation review — who changed what.**

```
event="audit" AND audit_schema_version="1.1" AND audit_event_type="operation"
  → group by principal.sub
  → report tool, broker, outcome, timestamp_utc, correlation_id
```

In Splunk SPL:

```
index=<your_audit_index> event="audit" audit_schema_version="1.1" audit_event_type="operation"
| stats count, values(tool) as tools, values(broker) as brokers, values(outcome) as outcomes,
    values(timestamp_utc) as timestamps, values(correlation_id) as correlation_ids
    by principal.sub
```

Every **destructive** tool call, successful or failed, attributed to the human principal that
made it. Two limits to state before a reviewer treats this as a complete record of change:

- **It is not every state change.** The `create-*` tools and the `clear-*-stats` tools are
  annotated non-destructive and emit no `operation` record, so this query returns zero object
  creations. See the coverage note under [Audit
  Trail](#audit-trail--interim--all-record-types-except-broker_authz_denied).
- **`principal.sub` is absent** in a deployment running `mcp_client_auth.mode: disabled`, and
  resolving it to a named human is your IdP's job — see
  [Q-013](#decided-since-the-first-draft).

**2. Rejected credentials — who could not get in.**

```
event="audit" AND audit_schema_version="1.1" AND audit_event_type="auth_failure"
  → group by reason
```

`reason` is the five-value closed set (`invalid_token`, `expired`, `audience_mismatch`,
`signature_invalid`, `missing`). Two volume notes before you build a rule on this: it fires
once per rejected request to `/mcp`, not once per destructive call, and `reason="missing"` can
be driven by any unauthenticated caller with no rate limit of its own (see [Authentication
Events](#authentication-events)).

**3. Denied privileged attempts, hop 1 — this server refused the tool.**

```
event="audit" AND audit_schema_version="1.1" AND audit_event_type="authz_denied"
  → group by reason, tool
```

`reason` is `missing_claim` or `not_permitted`. These calls never reached a handler, so there
is no `operation` record and no tool-invocation metric sample for them.

**4. Denied privileged attempts, hop 2 — the broker refused the operation.**

```
event="audit" AND audit_schema_version="1.1" AND audit_event_type="broker_authz_denied"
  → group by reason, tool, broker
```

`reason` is `permission_denied`. Unlike hop 1, the call ran: expect a coexisting `operation`
record on the same `correlation_id` for a destructive tool.

> **Query 4 returns nothing today.** `broker_authz_denied` is designed and schema-accepted but
> not yet emitted (Story 49, SOL-153332). Write the rule now so it starts working when the
> story lands; do not read its silence as evidence of no broker-side denials.

#### A complete refusal picture needs queries 3 and 4 together

Authorization is checked at two hops, and each hop has its own record type. **A reviewer who
runs only query 3 silently misses every broker-side denial** — the query returns cleanly, with
no indication that a second class of refusal exists and was not counted. That is the failure
mode worth guarding against, because it looks like a complete answer.

| Question | Query |
|---|---|
| Was anyone refused a tool by this server? | 3 |
| Was anyone refused an operation by a broker? | 4 |
| Was anyone refused anything? | 3 **and** 4 |

The two are not redundant and neither subsumes the other. A caller can be fully authorized at
hop 1 and refused at hop 2, which is the case a hop-1-only review is blindest to: the MCP
server's own policy says yes, and the broker says no.

### Audit Delivery

Delivery is **non-blocking by design**: writing an audit event never stalls or fails the
broker operation. The event rides the server's structured JSON log stream on stderr, tagged
`"event": "audit"`; your log shipper filters on the tag and routes it.

If the local sink backpressures, the event is **dropped rather than buffered**, and every
drop is recorded as a JSON record carrying `"event": "audit"` and
`"audit_event_type": "audit_drop"`, so a gap is visible, never silent. Three things produce a
drop today: the log handler refusing the write, the record's level being filtered out by the
server's configured log level, and arguments that could not be canonicalized for hashing.

> **The `mcp_audit_events_dropped_total` counter is not registered yet.** The `audit_drop`
> *record* ships now, with no dependency; the counter that makes drops dashboard-visible
> waits on the audit instruments being registered against the meter provider. Alert on the
> record until then.

`"event"` stays constant at `"audit"` on every record, drops included, precisely so the one
filter your shipper routes on cannot miss them — a drop notice that fell outside the audit
stream would go unseen in exactly the situation it exists to report. Discriminate record
kinds on `audit_event_type`, never on `event`.

For guaranteed delivery, route the tagged stream to an acknowledged sink on your side (TCP
syslog per RFC 5424, or a SIEM ingest endpoint such as Splunk HEC with indexer
acknowledgement). **Retention and tamper-evident storage are properties of that destination,
which you own.** The server does not itself persist or sign events.

---

## Distributed Tracing — [Interim: request-path and per-attempt spans wired]

> _Status: **[Interim]** (SOL-152420, SOL-153333, SOL-152421, SOL-152422). The tracer provider,
> OTLP export, and self-observation counters are wired and live behind `OBS_TRACING_ENABLED`
> (Story 25). Story 26 (SOL-152421) adds the request-path spans — the HTTP boundary, the tool
> dispatcher, the composite executor, and one span per SEMP call — and installs the W3C Trace
> server. Story 27 (SOL-152422) adds the per-*attempt* spans below those: one `semp.attempt`
> per SEMP try and one `tokenexchange.attempt` per IdP try, each carrying the
> `retry.decision` the retry policy actually made and `retry.exhausted` on the attempt where
> the allowance ran out. A call that retried three times is now three attempt spans under one
> `semp.request`, not one opaque span. Enabling the flag today therefore exports a real
> five-or-more-span trace per tool call, not a single-span root. Trace exemplars linking the
> latency histogram buckets to these traces are live as well (Story 47, SOL-152419) — see
> [Trace Exemplars](#trace-exemplars--implemented). Span names and span kinds remain open items
> for pilot input (item 4) — the names shipped so far are listed under [Spans](#spans) and can
> still change on your feedback._

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

A successful end-to-end tool call produces four spans in one trace, nested in this order
(Story 26, SOL-152421):

| Span | `SpanKind` | One per |
|---|---|---|
| `POST /mcp` | Server | inbound HTTP request to the MCP endpoint |
| `tools.CallTool` | Internal | tool invocation dispatched to a handler |
| `composite.Execute` | Internal | composite-tool execution (absent for a native SEMPv1 tool) |
| `semp.request` | Client | SEMP call to the broker, **covering its whole retry chain** |

Plus one `semp.attempt` span under `semp.request` per attempt that call made — one on a call
that succeeded first time, three on a call that took three tries (Story 27, SOL-152422; see
below).

The entry span's name follows the OTel HTTP server convention `{method} {route}`, and its HTTP
attributes come from `otelhttp`'s own semantic-convention implementation. The other three follow
the `<package>.<Function>` shape `tokenexchange.Exchange` established.

**The SSE notification stream is deliberately not traced.** The MCP streamable transport opens a
long-lived `GET /mcp` for server-to-client messages, held open for the whole session. Spanning it
would produce a span lasting as long as the session — reported only when the session ends, lost
entirely if the pod is killed first, and long enough to swamp any latency view computed from
entry-span duration. So `GET` is filtered out and you will see no span for that stream. MCP
requests travel over `POST`, which is what the entry span covers; a `DELETE` session teardown is
short-lived and is traced.

Three further named spans:

- `tokenexchange.Exchange`: one per call to the OAuth token exchange (Story 50, SOL-153333) —
  a cache hit, a singleflight follower, and the singleflight winner triggering a live IdP round
  trip each get their own span. Child of the `semp.request` span whose `AddAuth` triggered it.
- `semp.attempt` (Client): one per SEMP request *attempt* (Story 27, SOL-152422), child of the
  `semp.request` span covering that chain. This is the span that distinguishes a call that
  succeeded first time from one that succeeded on its third attempt: read `attempt`,
  `http.response.status_code`, `retry.decision` and `retry.exhausted` down the siblings and the
  shape of a retry storm is legible at a glance. **`semp.request` is deliberately not named
  `semp.attempt`:** naming a whole retry chain "attempt" would mislabel it.
- `tokenexchange.attempt` (Client): one per HTTP attempt of a live IdP token exchange
  (Story 27, SOL-152422), child of the `tokenexchange.Exchange` span of the caller that
  actually ran the exchange. **Two limitations to know about, both consequences of the
  exchange running detached from any one caller so that a cancellation cannot abort work
  others are waiting on:**
  - A **deduped caller sees no attempt spans.** Concurrent identical exchanges collapse into
    one IdP round trip, and the attempts hang off the winner's span. A follower's
    `tokenexchange.Exchange` span has no attempt children; follow its
    `singleflight_role="follower"`, its `winner_trace_id` / `winner_span_id`, or its span
    `Link` to reach the trace that holds them.
  - `correlation_id` **on these spans is the winner's**, so it identifies the request that
    triggered the exchange, not necessarily the request you are looking at. This is weaker
    than the SEMP path, where every attempt carries that request's own ID.

Span names, and span kinds, remain open items in this review (see
[Open Items for This Review](#open-items-for-this-review), item 4) — including the four above.
They are what ships today, not a commitment frozen ahead of your feedback.

**Every tool dispatch produces a `tools.CallTool` span, including the tools that do not run
through the tool manager.** `list-brokers` and `describe-semp-schema` are registered directly
against the MCP server, and an unparseable `arguments` payload is rejected before dispatch —
all three bypass the tool manager and so emit their own audit record, metric, and span. They
carry the same `tool` / `outcome` / `error_type` / `broker` attributes as any other dispatch,
which is what keeps the metric-to-trace pivot total: without it `list-brokers` would appear in
every dashboard and in no trace, and `bad_request` and `not_found` would be metric-only values
of a vocabulary this document describes as shared by all three signals.

**A hop-1 denial produces only the entry span.** When this server's own authorization refuses a
call, it short-circuits before dispatch — authorization is composed outside the tool-dispatch
instrumentation — so there is no `tools.CallTool` span. That is by design, not a gap: the denial
is recorded as an `authz_denied` audit event instead (Story 23). Do not read a lone `POST /mcp`
span with a 403 as a broken trace.

**A hop-2 denial looks completely different, and that is correct.** When the *broker* refuses the
SEMP request (Story 49, SOL-153332), dispatch has already happened, so you get the full
hierarchy — entry, dispatcher, executor, and the `semp.request` span that carries the failure.
The two denial cases are distinguishable in a trace by shape alone: one span versus four. That
distinction is the point, since they have different causes and different remedies.

### Span Attributes

| Attribute | Meaning | Basis |
|---|---|---|
| `correlation_id` | The shared request ID, joining the trace to logs and audit | Solace |
| `outcome` | The result; the same three values used as a metric label and an audit field | Solace |
| `error_type` | Why the call failed; present on `outcome: error` only, the same [`error_type`](#error_type) vocabulary — the whole set, including the values raised by the dispatch paths that do not run through the tool manager (`bad_request`, `not_found`) | Solace |
| `tool` | The tool name, on `tools.CallTool` and `composite.Execute`; same value as the `tool` metric label | Solace |
| `broker` | The broker, on `tools.CallTool`; the **same canonical label the `broker` metric label uses** — the configured alias in its configured casing, or the `none`/`unknown` sentinel when no broker was named or the named one is not configured. Deliberately not the raw value the caller sent, which would be unbounded, untrusted input and would break the metric-to-trace join on casing alone | Solace |
| `semp.version` | `v1` or `v2`, on `semp.request` | Solace |
| `semp.operation` | The SEMPv2 operationId (e.g. `getMsgVpnQueue`), on `semp.request` for v2 only — SEMPv1 has no operationId | Solace |
| `composite.steps` | Declared step count, on `composite.Execute` | Solace |
| `attempt` | The 1-based try number, on `semp.attempt` and `tokenexchange.attempt`. On `semp.attempt` it is the **same value as the `attempt` label** on `mcp_semp_request_total`, read from one counter so the two cannot drift; the token-exchange attempts have no counterpart metric | Solace |
| `http.response.status_code` | The status that attempt got, on `semp.attempt` and `tokenexchange.attempt`. Absent — never zero — when the attempt got no response at all (a connection error) | Solace |
| `retry.decision` | Whether the retry policy chose to retry after this attempt, on `semp.attempt` and `tokenexchange.attempt`. **This is the decision the server acted on, not a re-reading of the status code**, so it can legitimately be `false` on a 503: a request the caller declared non-idempotent, or one on a non-idempotent method, is never replayed. A `true` alongside `retry.exhausted` means the policy wanted to retry and had nothing left | Solace |
| `retry.exhausted` | `true` on the final attempt when a retry allowance ran out; absent otherwise. It means one thing: the policy stopped because something it was counting was already spent. On `semp.attempt` that covers all four allowances — the configured `semp.retries`, the internal 429/503 sub-cap (which fires well below `semp.retries`, and is how a real broker-overload episode usually ends), the once-only replay of a non-429/503 5xx, and the once-only 401 re-auth. **Absent when nothing ran out**, even though the call still failed: a replay the policy refused because the caller declared the request non-idempotent, a context that ended, a status never retried at all, or an auth mode that could not recover the first 401. Those need a different remedy from a bigger budget, which is why they are distinguishable. **On `tokenexchange.attempt` it means only that the IdP client's configured retry count (`MaxRetries`) was spent** — that path has no sub-cap, no non-idempotency guard, and no 401 re-auth allowance, so the four SEMP allowances and the four SEMP exclusions above do not apply. Raise the IdP retry setting, not `semp.retries` | Solace |
| `cache_hit` | `tokenexchange.Exchange` only: true when served from cache, false when a live IdP round trip was needed (or waited on). **Isolating actual live round trips needs `singleflight_role="winner"` too** — a follower also reports `cache_hit=false` despite doing no IdP work itself, so filtering on `cache_hit` alone counts one winner plus every follower waiting on it | Solace |
| `singleflight_role` | `tokenexchange.Exchange` only, absent on a cache hit: `winner` (this call ran the live IdP round trip) or `follower` (this call shared another's result) | Solace |
| `winner_trace_id` / `winner_span_id` | `tokenexchange.Exchange` only, present on a `follower` span only: the winner's own IDs, so an operator can pivot from a follower's span to the trace that actually did the IdP work. The follower span also carries a span `Link` to the same span | Solace |

`outcome` and `error_type` carry the **same vocabulary here as on the metric labels and the
audit record**, which is the point of a single vocabulary: filter a dashboard by
`error_type="broker_init_error"` and you can carry that predicate into the trace backend and
the SIEM unchanged, with no translation table.

This is enforced, not merely intended, and across all four signals rather than the two most
obviously paired. One test drives a real tool call and asserts that all four shared keys —
`tool`, `broker`, `outcome`, `error_type` — hold identical values on the span, on the
`mcp_tool_invocation_total` series, and on the `tool invoked` log line that call produced; a
second does the same for the `audit_event_type=operation` record, across a success, a failure
and a recovered panic. Nothing in the type system couples any of them — the span writes an
attribute, the metric writes a Prometheus label, the log line and the audit record write slog
attrs, all from different call sites, and the audit record's `outcome` is its own typed
vocabulary that merely happens to be spelled like the metric's — so they can drift while each
surface still looks healthy on its own. `correlation_id` is span-only by design: it is
per-request, and as a metric label it would be unbounded.

Two exclusions are deliberate. The log line's `broker` is **not** joined: it carries the raw
caller-supplied alias for diagnostics, while the metric label and the span attribute are
canonicalized, so for a broker that cannot be resolved they differ by design. And a call that
fails broker resolution produces no `operation` audit record at all — the record is written
past the destructive gate, which that call never reaches.

**The two attempt spans deliberately carry no `outcome`.** An attempt is not a call: a 503 that
was retried and then succeeded is a normal step of a healthy call, so tagging it
`outcome: error` would place an error span under a successful `semp.request` on every retried
call and inflate any error view a backend builds from that filter. `retry.decision` and
`http.response.status_code` describe an attempt; the call's outcome is on the parent span. For
the same reason their span status is left `Unset`.

**Which spans carry which.** Every span in the table above **except the entry span** carries
`outcome`. `POST /mcp` is produced by `otelhttp` and carries the HTTP semantic-convention
attributes plus `correlation_id`; there is no `outcome` on it. **A trace-backend filter of
`outcome = "error"` therefore misses every failure that produced only an entry span** — the 403
cross-origin rejection, the 413 body-limit rejection, and a hop-1 authorization denial (see
below), none of which reach the tool dispatcher. Select those on
`http.response.status_code` instead, and **not** on the span status: following the OTel server
convention, `otelhttp` sets the status to `Error` only for 5xx, so a 403 or 413 entry span has
status `Unset`. Only
`tools.CallTool` carries `error_type`: the twelve-value set is scoped to tool-invocation outcomes,
and the executor and SEMP layers have no value in it that describes an orchestration or
transport failure — the same reasoning that exempts `tokenexchange.Exchange` below. Those spans
report `outcome: error` and an `Error` span status, and the classification for the call as a
whole sits on the dispatch span above them. `error_type` is absent, never empty, on a
successful call, so a filter on it cannot match a success.

**A panicked call is never reported as a success.** Go does not populate named return
values when a panic unwinds a frame, so a span that derives its `outcome` from the returned
error would close as `outcome: success` on the way out of a panic — pointing an investigation
in exactly the wrong direction at exactly the worst moment. Every span on the request path
therefore recovers in its own deferred close, records `outcome: error`, and re-panics so the
failure still reaches the recovery layer above it (`tools.CallTool` gets there differently: its
audit defer already infers a panic from both the result and the error being nil, and rewrites
`error_type` to `panic`, which the span defer then reports because it runs afterwards). Pinned
by test in each layer.

**Span status carries no error text.** A failed span is marked with status code `Error` and an
**empty description**, and no exception event is recorded. This is deliberate: a broker error can
quote the response body, and a span travels to whatever collector
`OTEL_EXPORTER_OTLP_ENDPOINT` names — which may sit outside this deployment's residency
boundary. `error_type` carries everything needed to classify the failure; the full detail stays
in the audit record, inside your own log pipeline. (`tokenexchange.Exchange` is the exception and
does record its exception verbatim — see the warning at the end of this section.)

**Exception: `tokenexchange.Exchange` never sets `error_type`, even on `outcome: error`.** The
twelve-value `error_type` set above is scoped to tool-invocation outcomes and has no value
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

**Enabling `OBS_TRACING_ENABLED` exports client network identity to your collector.** The entry
span carries `otelhttp`'s standard OTel HTTP server attributes, and two of them —
`client.address` and `network.peer.address` — are the **caller's IP address**, alongside
`user_agent.original`. An IP address is personal data under GDPR and comparable regimes, so this
is a category of export worth naming rather than discovering: it now travels to wherever
`OTEL_EXPORTER_OTLP_ENDPOINT` points, on a per-request basis, and your collector's retention
becomes its retention. What is **not** exported: the `Authorization` header, cookies, and the URL query string (the
span records `url.path`, never `url.query`). That is enforced by test rather than asserted in
prose — a request carrying a bearer token, a session cookie and a secret-looking query
parameter is driven through the middleware, and the entry span's attribute keys are checked
against an explicit allowlist, so a future `otelhttp` bump that widens the set fails here
rather than reaching your collector. Drop `client.address` and `network.peer.address` at your
collector if your data-flow review would rather not hold them.

**Two address attributes, only one of them attested.** `client.address` is taken from the first
element of `X-Forwarded-For` verbatim, with no validation and no trusted-proxy handling, so any
caller can set it to any value. `network.peer.address` is the real transport peer. Use
`network.peer.address` for anything forensic and treat `client.address` as a hint.

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

### Resource Attributes — [Implemented]

> _Status: **[Implemented]** (SOL-152425, Story 34). Constructed once by
> `internal/observability/resource` and shared by the metrics meter provider (SOL-152091) and
> the tracer provider (SOL-152420) — one construction site, so the two cannot disagree. Also
> mirrored (the `service.name` / `deployment.environment.name` / `cloud.region` subset only)
> onto every log line from immediately after config loads onward — the handful of log lines
> emitted before config loads (the process banner and the config-load attempt itself) have no
> identity to attach, since it's derived from config. The **OTLP push** query guidance below
> describes that egress (SOL-152418, Story 46) — both it and the scrape (`target_info`) path
> are live today, sharing this same resource by construction (both readers attach to the one
> meter provider Story 14 built)._

Set from server configuration on **both** metrics and spans, so an aggregated dashboard can
tell instances apart without a label duplicated onto every series. All five follow the
OpenTelemetry resource semantic conventions
(https://opentelemetry.io/docs/specs/semconv/resource/).

| Attribute | Source | Config key |
|---|---|---|
| `service.name` | config, default `solace-broker-mcp` | `observability.service_name` |
| `service.version` | build-time injection | — |
| `service.instance.id` | config, else the pod name (Kubernetes downward API), else the process hostname | `observability.service_instance_id` |
| `deployment.environment.name` | config, when set | `observability.deployment_environment` |
| `cloud.region` | config, when set | `observability.cloud_region` |

An earlier draft called this attribute `region` and flagged the OTel name as a possible
change. **It is now `cloud.region`**: where OTel publishes a convention we adopt it, and
`cloud.region` is what a multi-broker aggregator expects to pivot on.

**`service_instance_id` is an override for the (uncommon) case where neither the pod name nor
the hostname identifies the instance usefully** — for example, several bare-metal instances
sharing a hostname. Most Kubernetes deployments need neither this nor `POD_NAME`
configuration: `deploy/kubernetes/deployment.yaml` already wires `POD_NAME` via the downward
API.

**The process hostname is what gets exported when neither override is set.** Outside
Kubernetes (or with `POD_NAME` unset), `service.instance.id` falls all the way through to
`os.Hostname()` — which can carry internal topology (a bare-metal or VM name your network team
recognizes) that now travels off-box on every span and appears on `target_info`. Set
`observability.service_instance_id` explicitly if that's not a value you want to export.

**Known limitation:** `service_name` and `service_instance_id` always win over the standard
`OTEL_SERVICE_NAME` / `OTEL_RESOURCE_ATTRIBUTES` environment variables, because config always
has a value for both (a real one, or the stated default) by the time the shared resource is
built, and this package's own attributes take precedence in the merge. `deployment_environment`
and `cloud_region` do **not** have this problem — config leaves them genuinely empty when
unset, so the standard `OTEL_RESOURCE_ATTRIBUTES` entries for those two reach the resource
unopposed. An operator who wants `OTEL_SERVICE_NAME` or an `OTEL_RESOURCE_ATTRIBUTES`
`service.instance.id` honored should use `observability.service_name` /
`observability.service_instance_id` instead, for now.

**`deployment.environment.name`, not the FD's original `deployment.environment`.** OTel
renamed the semantic-convention key ahead of this story landing; the SDK's own
`resource.Default()` (which this attribute set is merged with) is already built against the
renamed key, so keeping the older name here would have shipped an attribute the SDK's own
semantic-convention package no longer recognizes on day one. Disclosed as a deliberate
deviation from the FD text, not a silent one — see the SOL-152425 PR description.

**The old and new spellings can both reach the resource at once.** This package only ever
writes `deployment.environment.name`, but `resource.Default()`'s own environment-variable
detection still honors `OTEL_RESOURCE_ATTRIBUTES=deployment.environment=...` under the *old*
key — nothing rejects it. Set that variable under the old spelling and the merged resource
carries both keys with independent values; `target_info` shows both, and `SlogAttrs` mirrors
only the new one, so logs and metrics can disagree about which environment a pod is in. Use
`observability.deployment_environment` instead of the environment variable to avoid the
ambiguity entirely.

**How to query them, per egress.** These are resource attributes, not per-series labels, so
they arrive differently on each of the two metric egresses:

- **Prometheus scrape.** The exporter publishes them on the `target_info` series, with `.`
  translated to `_`, so they read as `service_name`, `cloud_region`, and so on. Join to it,
  for example
  `mcp_tool_invocation_total * on (instance, job) group_left(service_name, cloud_region) target_info`.
- **OTLP push.** Resource attributes are **not promoted to labels by default**. If you ingest
  our OTLP metrics straight into Prometheus, set `promote_resource_attributes` to include
  `service.name`, `service.instance.id`, `deployment.environment.name`, and `cloud.region`, or
  the same dashboard will show empty variable drop-down lists.

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

Present only on `outcome: error`, drawn from a closed set of twelve values:

| Value | Meaning |
|---|---|
| `panic` | An unexpected failure was caught by the recovery layer and returned as a clean error. |
| `unknown_tool` | The requested tool is not registered. |
| `missing_broker` | No broker was named on a call that requires one. |
| `unknown_broker` | The named broker is not configured. |
| `broker_init_error` | The broker is configured but could not be initialized. |
| `bad_request` | The request itself was malformed — unparseable `arguments`, or a missing or invalid parameter on a tool that validates its own input. |
| `validation_error` | The arguments failed input validation. |
| `not_found` | The requested item does not exist (for example, an unknown SEMP operation passed to `describe-semp-schema`). |
| `execution_error` | The tool ran and failed. |
| `nil_result` | The tool returned no result. |
| `output_validation_error` | The tool's output failed schema validation. |
| `marshal_error` | The result could not be serialized. |

**Only five of these reach an audit record.** The twelve values above are the full vocabulary
for the tool-invocation **metric** and for the `tool invoked` log line. An `operation` audit
record is written only for a call that actually reached the tool, so only the failures that
can happen at or after dispatch appear on one:

| Reaches an `operation` audit record | Never appears on an audit record |
|---|---|
| `execution_error`, `nil_result`, `output_validation_error`, `marshal_error`, `panic` | `unknown_tool`, `missing_broker`, `unknown_broker`, `broker_init_error`, `validation_error`, `bad_request`, `not_found` |

The right-hand column is every way a call is rejected **before** anything is attempted
against a broker: an unregistered tool, an absent or unresolvable broker, arguments that
failed schema validation, or a request whose `arguments` were not valid JSON. Those are
recorded as an ERROR `tool invoked` line in the operational log, not in the audit stream,
because no state changed and there is nothing for a compliance reviewer to review. A SIEM
rule matching `event="audit"` with `error_type: validation_error` will never fire; query the
operational stream for those.

Notes:

- **If you saw an earlier draft listing ten values, this supersedes it.** `bad_request` and
  `not_found` are emitted by the two tools that handle their own dispatch — `list-brokers` and
  `describe-semp-schema` — plus the argument-parsing guard ahead of every tool. They were
  omitted from earlier drafts that enumerated only the main tool-dispatch path. Nothing about
  the emitted records changed; the list was incomplete.

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
  A call denied at hop 1 produces no `operation` record at all. A call denied by the *broker*
  at hop 2 is different: it produces a `broker_authz_denied` record **and**, on a destructive
  tool, an `operation` record, because execution had already started. See
  [Authentication Events](#authentication-events).
- **Load-shedding / saturation is not an `outcome` value** either; it is a separate signal.
  It ships today as log lines (see
  [Load and Saturation Visibility](#load-and-saturation-visibility--interim--logs-only))
  and is planned as a metric in a later release (see
  [Planned for a Later Release](#planned-for-a-later-release-not-frozen-in-this-review)).
- **A desired-state noop (SOL-153341) reads `outcome=success`, plus a separate
  `desired_state` field — never a fourth `outcome` value.** Creating an object that already
  exists, or deleting one that's already gone, is success by this ticket's own definition, not
  a distinct kind of outcome — so the "tool invoked" log line for these two cases carries
  `outcome=success` like any other success, with `desired_state` (`already_exists` or
  `already_absent`) alongside it distinguishing "this was a no-op" from an ordinary fresh
  create/delete. `desired_state` is deliberately not named `outcome`: the write tool's own
  structured *result* also has a field literally called `outcome` with these same two values
  (plus `changed` and, for `already_exists`, `attributes_verified` — see the tool
  descriptions), but that is a different namespace — the tool's own output schema, not this
  shared telemetry vocabulary — and the two must not be confused when grepping logs versus
  reading a tool result. A monitor that alerted on `ERROR`-level volume for these two cases
  before this ticket should switch to a rule on `desired_state` instead; see the CHANGELOG
  entry for the full operator-visible effect.
- **The `operation` audit record does not carry `desired_state` — only the "tool invoked" log
  line does.** A desired-state noop against a destructive tool (`delete-message-vpn` against a
  VPN that is already gone, for example) still writes an `operation` record with
  `outcome: success` and no field distinguishing it from a genuine deletion — `audit.Fields`
  (`internal/observability/audit/event.go`) has no `desired_state`/`changed` field today, and
  `emitOperationAudit` (`internal/tools/manager.go`) branches only on whether the call errored,
  which a noop deliberately does not. For the durable, schema-enforced compliance trail this
  means "I deleted it" and "it was already gone" render identically: a reviewer reconstructing
  what an agent actually changed from the audit stream alone cannot tell the two apart. If you
  need that distinction, join to the "tool invoked" log line for the same `correlation_id` and
  read its `desired_state` field instead — the audit record alone is not enough. This gap is
  tracked, not fixed here (SOL-153341); a future revision may add the field to `audit.Fields`,
  which would be a schema-version bump like any other.

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

1. ~~**SEMP duration histogram buckets.**~~ **Decided.** Buckets are committed as
   `0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10` seconds and are frozen.
   Pilot feedback is welcome but cannot change the shipped values.
2. ~~**The no-response case on SEMP metrics.**~~ **Decided.** `http_response_status_code`
   is the empty string when no response arrives. Story 41 can revisit if pilots need the
   failure reason as its own label.
3. **Can your access review resolve `principal.sub` to a human?** The `sub`-only identity
   decision itself is settled — see [Q-013](#decided-since-the-first-draft). What is still
   genuinely open is whether that is workable for you: at review time, can your tooling
   resolve an opaque OIDC subject to a named person, **including for a user who has since been
   deprovisioned**? If it cannot, say so and we will add `principal.preferred_username` in a
   later minor. This is the one question that would change the answer, which is why it stays
   here rather than moving with the decision.
4. **Trace span names and span kinds.** Story 26 has now shipped four names —
   `POST /mcp` (Server), `tools.CallTool` (Internal), `composite.Execute` (Internal), and
   `semp.request` (Client) — listed under [Spans](#spans). They ship so you have something
   concrete to react to, not because they are frozen: renaming a span is cheap now and expensive
   after the freeze. If your trace backend or trace-based SLOs key off specific span names or
   `SpanKind` values, tell us what you expect. Two specifics we would most like checked: whether
   `semp.request` covering a whole retry chain, with per-attempt `semp.attempt` spans nested
   inside it (Story 27, now shipped), matches how you would query retries, and whether you
   expect `tools.CallTool` to be `Internal` or `Server`.
5. **The `outcome` / `error_type` split.** We have settled on three `outcome` values with the
   cause in a separate `error_type` of twelve values, rather than folding causes into `outcome`.
   Does that split match how your SIEM queries distinguish failures, and do the twelve
   `error_type` values cover how you classify them? If you would separate something we have
   merged — a `timeout` distinct from other errors, say — now is the time.
6. **Authorization denials — the signal is decided, the vocabulary is what we want checked.**
   Denials get their own record, `audit_event_type: authz_denied`, with `reason` drawn
   from `missing_claim` and `not_permitted`, plus a matching
   `mcp_authz_denied_total{tool,reason}` counter. Does that two-value `reason` set match how
   your access reviews classify a refusal, or do you distinguish cases we have merged? And is
   the single-predicate query the shape you need? One caveat worth knowing: denial history
   begins at the release that first emitted the record (SOL-152097) and the counter
   (SOL-152099) and cannot be back-filled.
### Decided Since the First Draft

Four items that appeared as open questions in earlier drafts are now settled, so you do not
need to spend review time on them. Where a decision has a cross-reference tag (`Q-nnn`), that
tag is stable and safe to cite in your own review notes.

- **Q-013 — audit identity is `principal.sub` only, and no readable username.** The audit
  event records the opaque OIDC subject of the human user, and deliberately **not**
  `preferred_username`. A readable username helps an access review, but it is directly
  identifying PII landing in an append-only store, which conflicts with erasure obligations
  under GDPR and PIPL. The decision is the reversible one in both directions that matter:
  adding `principal.preferred_username` later is a pure addition this schema permits under a
  minor bump, while removing it later would need a major version and a deprecation cycle. The
  `principal` field is a nested object precisely so a second member can be added without
  renaming anything — see [Event Fields](#event-fields). One related question is still open
  and stays in [Open Items](#open-items-for-this-review) as item 3: whether your access review
  can resolve `sub` to a human, including for a deprovisioned user.
- **`server_address` and `broker` on SEMP metrics: we keep both.** They answer different
  questions. `server_address` is the OTel-conventional host, which is what correlates this
  service with everything else OTel-instrumented in your estate; `broker` is your configured
  alias, which is what dashboards and alerts group by. Neither is redundant.
- **`region` is now `cloud.region`.** See [Resource Attributes](#resource-attributes--implemented).
- **OTLP metrics push has its own flag, `OBS_METRICS_OTLP_ENABLED`** (ships with Story 46,
  SOL-152418 — see [Metrics](#metrics--planned-with-exceptions)). We considered activating
  push as soon as `OTEL_EXPORTER_OTLP_ENDPOINT` was set, which would be tidier and would match
  what your collectors already configure. We rejected it: that variable is frequently set
  cluster-wide for other services, so an upgrade could silently start egressing telemetry from
  a Solace pod that nobody asked to export. An explicit capability flag keeps the decision
  yours, and it keeps this signal inside the same `OBS_*` model as every other capability. See
  [Metrics](#metrics--planned-with-exceptions).

---

## Load and Saturation Visibility — [Interim — logs only]

> _Status: **[Interim]**. This ships as structured **log lines**, not as metrics. The
> `mcp_saturation_total` counter and the occupancy gauge described in
> [Planned for a Later Release](#planned-for-a-later-release-not-frozen-in-this-review)
> are still roadmap. Nothing in this section appears on `/metrics`: the saturation instruments
> themselves have not been written. (The meter provider and the OpenTelemetry dependency now
> exist — an earlier draft said otherwise, written before Story 14 landed — so the remaining
> work is the instruments, not the pipeline.)_

Support and SRE needed one question answered quickly: when a customer says "MCP feels
slow", is the server pacing or shedding requests to protect a broker, or is something else
slow? When this shipped, the metric form of that answer depended on a metrics pipeline the
build did not have, and standing one up here would have duplicated work already in flight
under SOL-150254. So the signal ships as logs. The pipeline has since landed; what is still
missing is the saturation instruments themselves, tracked under SOL-150254.

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

## Vendor Neutrality

Nothing here requires a Solace-supplied backend, and nothing here is a Solace-proprietary
telemetry format. The wire formats are the open ones your existing stack already speaks.

| Signal | Format | Transport | Status |
|---|---|---|---|
| Metrics | OpenTelemetry, plus **Prometheus text exposition additionally** for scrape-based stacks | `/metrics` scrape endpoint (`OBS_METRICS_ENABLED`) | Live |
| Metrics | The same OpenTelemetry instruments | OTLP push (`OBS_METRICS_OTLP_ENABLED`) | Live (Story 46, SOL-152418) |
| Traces | OpenTelemetry | OTLP over gRPC (`OBS_TRACING_ENABLED`) | Live |
| Audit trail | Structured JSON on stderr, tagged `"event": "audit"` | Your log shipper, to any sink you route it to | Live |

Three things follow, and each is a commitment rather than an accident of the current build:

- **Metrics are designed for two egresses, not one.** OTLP push suits an OTLP-native APM; the
  Prometheus scrape endpoint suits a scrape-based stack with no collector in the path. Both
  observe one instrument set, so the two cannot disagree, and neither is a second-class path.
  Both are live today, each behind its own flag, both off by default (door-closing policy) — see
  [Metrics](#metrics--planned-with-exceptions). One asymmetry stays even with both on: the
  `go_*`/`process_*` collectors are scrape-only — see [Go Runtime and Process
  Metrics](#go-runtime-and-process-metrics).
- **No Solace-proprietary telemetry format appears anywhere.** No custom exporter, no
  Solace-specific wire protocol, no agent you have to install. Where OpenTelemetry publishes
  a semantic convention we adopt it (see [Conventions](#conventions)); where it does not, we
  use a documented Solace-*named* metric or field, carried over the standard transport like
  every other one.
- **Grafana is a reference implementation, not a requirement.** This document mentions Grafana
  because it is the most common way to consume these signals, and a reference dashboard is
  planned (Story 37, SOL-152092). Nothing in the schema depends on it. Trace exemplars are the
  one place a dashboard feature is described, and they are an OpenMetrics feature any
  conforming backend can read — see [Trace Exemplars](#trace-exemplars--implemented).

**Backends in scope.** The four we design and check against are Prometheus with Grafana,
Grafana Tempo, Jaeger, and Datadog. Be precise about what "check against" means today, because
the two levels are different:

| Backend | What is verified today |
|---|---|
| Prometheus (with Grafana) | The scrape surface itself is pinned by test: golden-file exposition output, OpenMetrics negotiation, exemplar emission, and the suppressed `otel_scope_*` labels. Grafana is then an ordinary Prometheus data source. |
| Grafana Tempo, Jaeger, Datadog | Reached over standard OTLP, with no backend-specific code path in this server. Verified at the protocol level, not yet as an end-to-end matrix per backend. |

**A per-backend tested matrix, and a reference OTel collector deployment to sit in front of
it, land with Story 40 (SOL-152423).** Until then, treat the second row as "should work
because the format is standard" rather than as an attested integration. That list is a
starting point, not a compatibility boundary: any backend that ingests OTLP or scrapes
Prometheus text exposition should work, and we would rather hear about one that does not than
have you assume it is unsupported.

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
