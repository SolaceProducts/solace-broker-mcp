# Grafana dashboard

A starter dashboard, `solace-broker-mcp-overview.json`: tool RED, active
requests, SEMP RED-per-attempt, an auth-failure rate broken out by reason, an
`error_type` breakdown, Go runtime, and build info. Import it as-is — no
panel authoring required.

Full documentation, including exemplar setup and the OTLP-ingestion caveats
below, lives in
[`docs/observability.md` → Grafana Dashboard](../../docs/observability.md#grafana-dashboard--implemented).
This file is a quick-start pointer, not a substitute for that section.

## Import

**Dashboards → New → Import**, upload `solace-broker-mcp-overview.json`.
Grafana prompts for one input — the Prometheus data source to bind the
dashboard's `${DS_PROMETHEUS}` variable to. Requires Grafana 9 or later.

## Before you assume a panel is broken

- **Go runtime panels show no data if your Prometheus ingests this server's
  OTLP metrics push rather than scraping `/metrics`.** `go_*`/`process_*` are
  scrape-only by design — see
  [`docs/observability.md` → Go Runtime and Process Metrics](../../docs/observability.md#go-runtime-and-process-metrics).
- **The `$service_name`/`$cloud_region` variable dropdowns are empty on the
  OTLP path unless `promote_resource_attributes` is configured on that
  Prometheus.** And that path needs an OTel Collector in front of it — this
  server's OTLP metrics exporter is gRPC-only, and a bare Prometheus's native
  OTLP receiver only speaks OTLP/HTTP, so a direct push does not work. See
  the "Works against both ingestion paths" subsection in
  [`docs/observability.md`](../../docs/observability.md#grafana-dashboard--implemented)
  for what's required.
- **The two latency panels show no exemplar links** unless: tracing is
  enabled on the server, your Prometheus was started with
  `--enable-feature=exemplar-storage`, and your Grafana Prometheus data
  source has an Exemplars mapping pointed at a Tempo/Jaeger/other trace data
  source. Missing any one of these renders a plain histogram, not an error.

## CI

`cmd/server/grafana_dashboard_test.go` checks every panel's metric name and
label keys against Story 14's golden file
(`internal/observability/metrics/testdata/metrics_golden.txt`) — the
authority on what those actually are. Edit this dashboard's queries and that
test enforces they still match the real schema.
