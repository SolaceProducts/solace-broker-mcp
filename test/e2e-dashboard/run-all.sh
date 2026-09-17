#!/usr/bin/env bash
# E2E dashboard test: proves the published deploy/grafana dashboard and the
# published deploy/otel-collector metrics pipeline both actually deliver data,
# on both metrics ingestion paths, against a live broker. Services
# (mcp-server, prometheus-scrape, prometheus-otlp, otel-collector, grafana,
# solace) are started by docker compose before this script runs.
set -euo pipefail

MCP_URL="${MCP_URL:-http://localhost:9096}"
PROM_SCRAPE_URL="${PROM_SCRAPE_URL:-http://localhost:9092}"
PROM_OTLP_URL="${PROM_OTLP_URL:-http://localhost:9093}"
GRAFANA_URL="${GRAFANA_URL:-http://localhost:3000}"
GRAFANA_PASSWORD="${GRAFANA_PASSWORD:-admin}"
MCP_DEV_TOKEN="${MCP_DEV_TOKEN:-e2e-dev-token}"
BROKER_ALIAS="solace"
DASHBOARD_JSON="../../deploy/grafana/solace-broker-mcp-overview.json"

pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

mcp_call() {
    curl -sf -X POST "$MCP_URL/mcp" \
        -H "Content-Type: application/json" \
        -H "Accept: application/json, text/event-stream" \
        -H "Authorization: Bearer $MCP_DEV_TOKEN" \
        -H "Mcp-Session-Id: $session_id" \
        -d "$1"
}

# ── 1. Wait for MCP server ───────────────────────────────────────────────────
echo "Waiting for MCP server..."
for i in $(seq 1 30); do
    if curl -sf "$MCP_URL/readyz" > /dev/null 2>&1; then
        break
    fi
    [ "$i" -eq 30 ] && fail "MCP server did not become ready in time"
    sleep 2
done
pass "MCP server ready"

# ── 2. Initialize MCP session ────────────────────────────────────────────────
init_response=$(curl -sf -D - -X POST "$MCP_URL/mcp" \
    -H "Content-Type: application/json" \
    -H "Accept: application/json, text/event-stream" \
    -H "Authorization: Bearer $MCP_DEV_TOKEN" \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"e2e-dashboard","version":"1.0.0"}}}')

session_id=$(echo "$init_response" | grep -i "mcp-session-id" | awk '{print $2}' | tr -d '\r')
[ -z "$session_id" ] && fail "No Mcp-Session-Id in initialize response"
pass "Session initialized: $session_id"

mcp_call '{"jsonrpc":"2.0","method":"notifications/initialized"}' > /dev/null

# ── 3. Drive real traffic: successes (tool + SEMP RED) and one deliberate ────
#    error (error_type breakdown). mcp_tool_invocation_total,
#    mcp_semp_request_total, and mcp_http_active_requests are OTel-SDK
#    instruments that do not appear on a scrape until they have recorded at
#    least one data point (verified in SOL-152092) — this traffic is what
#    makes those panels have anything to assert against at all.
echo "Driving real tool traffic..."
for i in 1 2 3; do
    tool_response=$(mcp_call "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"get-broker-status\",\"arguments\":{\"broker\":\"$BROKER_ALIAS\"}}}")
    echo "$tool_response" | grep -q '"result"' || fail "get-broker-status did not return a result: $tool_response"
    if echo "$tool_response" | grep -q '"isError"[[:space:]]*:[[:space:]]*true'; then
        fail "get-broker-status returned a tool error: $tool_response"
    fi
done
pass "get-broker-status succeeded 3x"

error_response=$(mcp_call '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get-broker-status","arguments":{"broker":"nonexistent-broker"}}}')
echo "$error_response" | grep -q '"isError"[[:space:]]*:[[:space:]]*true' || fail "expected a tool error calling a nonexistent broker, got: $error_response"
pass "deliberate error call produced isError:true (populates the error_type breakdown panel)"

# A nonexistent BROKER fails at broker resolution, before any SEMP call is
# ever attempted — it does nothing for the "SEMP error rate by status code"
# panel, which needs a real non-2xx response FROM the broker. A nonexistent
# VPN gets past resolution and reaches SEMP, but only for a single-resource
# lookup: verified directly against this broker that a *collection* query
# for a nonexistent VPN's queues returns 200 with an empty array (ordinary
# REST collection-endpoint behavior), not an error — list-queues would not
# have produced one. get-vpn-status is a single-resource GET, confirmed
# returning 400 (SEMP's own NOT_FOUND-as-400 convention) for a VPN that
# does not exist.
semp_error_response=$(mcp_call "{\"jsonrpc\":\"2.0\",\"id\":4,\"method\":\"tools/call\",\"params\":{\"name\":\"get-vpn-status\",\"arguments\":{\"broker\":\"$BROKER_ALIAS\",\"msgVpnName\":\"nonexistent-vpn\"}}}")
echo "$semp_error_response" | grep -q '"isError"[[:space:]]*:[[:space:]]*true' || fail "expected a tool error querying a nonexistent VPN's status, got: $semp_error_response"
pass "deliberate SEMP-level error call produced isError:true (populates the SEMP error-rate panel)"

# Polls the actual rate() form every panel query uses, not just the raw
# counter's presence: rate() needs the SAME series scraped at least twice to
# compute anything, so a wait that only checks "has this metric appeared at
# all" can break as soon as the success series' first sample lands, well
# before the error series (recorded in the same batch, a moment later) has
# had its own second scrape — which is exactly what happened the first time
# this suite ran end to end: the error-outcome panels intermittently failed
# with "expected non-empty data, got none" even though the raw counter was
# already visible. One function, parameterized on the Prometheus URL, used
# for both paths below — the two were originally byte-identical copies that
# only closed over a different URL variable.
prom_rate_nonempty() {
    local url="$1" query="$2" result count
    result=$(curl -sf "$url/api/v1/query" --data-urlencode "query=$query" 2>/dev/null) || return 1
    count=$(echo "$result" | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('data',{}).get('result',[])))" 2>/dev/null) || return 1
    [ "${count:-0}" -gt 0 ]
}

# ── 4. Wait for Path A (scrape) to have >=2 samples, then verify every panel,
#    every template variable, and the exemplar, straight against Prometheus ──
echo "Waiting for the scrape-path Prometheus to have collected traffic..."
for i in $(seq 1 12); do
    sleep 5
    prom_rate_nonempty "$PROM_SCRAPE_URL" 'sum(rate(mcp_tool_invocation_total{outcome="error"}[5m]))' \
        && prom_rate_nonempty "$PROM_SCRAPE_URL" 'sum(rate(mcp_semp_request_total{http_response_status_code!~"2.."}[5m]))' \
        && break
    [ "$i" -eq 12 ] && fail "scrape-path Prometheus never computed a rate for both the tool-error and SEMP-error series after 60s"
done
pass "scrape-path Prometheus has collected traffic and can compute rate() over both error series"

python3 verify-panels.py \
    --prometheus-url "$PROM_SCRAPE_URL" \
    --dashboard "$DASHBOARD_JSON" \
    --check-variables \
    --check-exemplar \
    || fail "Path A (scrape) panel/variable/exemplar verification failed — see output above"
pass "Path A (Prometheus scrape): every panel, every variable, and the exemplar check passed"

# ── 5. Import the real dashboard JSON into Grafana via the API — the only ───
#    mechanism that resolves its ${DS_PROMETHEUS} templating (see
#    grafana-provisioning/datasources/prometheus.yaml) — and assert it
#    produced no provisioning errors. Path A's traffic generation and wait
#    loop above already take 60s+, which in practice is long past Grafana's
#    own startup — but that was implicit timing luck, not a real wait, so
#    poll its health endpoint explicitly rather than rely on it.
echo "Waiting for Grafana..."
for i in $(seq 1 30); do
    if curl -sf "$GRAFANA_URL/api/health" > /dev/null 2>&1; then
        break
    fi
    [ "$i" -eq 30 ] && fail "Grafana did not become ready in time"
    sleep 2
done
pass "Grafana ready"

echo "Importing the dashboard into Grafana..."
# Dashboard path and datasource name passed via argv, not interpolated into
# the Python source, matching uncomment-metrics-pipeline.sh's convention —
# these are fixed local values today, not attacker input, but building the
# payload from argv rather than string-embedding is the same habit either
# way and costs nothing.
import_payload=$(python3 - "$DASHBOARD_JSON" "Prometheus-Scrape" <<'PYEOF'
import json, sys
dashboard_path, datasource_name = sys.argv[1], sys.argv[2]
dash = json.load(open(dashboard_path))
payload = {
    "dashboard": dash,
    "overwrite": True,
    "inputs": [{"name": "DS_PROMETHEUS", "type": "datasource", "pluginId": "prometheus", "value": datasource_name}],
}
print(json.dumps(payload))
PYEOF
)
import_response=$(curl -sf -u "admin:$GRAFANA_PASSWORD" -X POST "$GRAFANA_URL/api/dashboards/import" \
    -H 'Content-Type: application/json' -d "$import_payload") || fail "Grafana import request failed"

echo "$import_response" | python3 -c "
import json, sys
d = json.load(sys.stdin)
if not d.get('imported'):
    sys.exit(f'import response did not report imported:true: {d}')
if d.get('removed'):
    sys.exit(f'import response reported removed:true, unexpected: {d}')
" || fail "dashboard import reported a problem: $import_response"

dashboard_uid=$(echo "$import_response" | python3 -c "import json,sys; print(json.load(sys.stdin)['uid'])")
fetch_response=$(curl -sf -u "admin:$GRAFANA_PASSWORD" "$GRAFANA_URL/api/dashboards/uid/$dashboard_uid") \
    || fail "could not fetch the imported dashboard back from Grafana"

echo "$fetch_response" | python3 -c "
import json, sys
d = json.load(sys.stdin)
if 'dashboard' not in d:
    sys.exit(f'fetched response has no dashboard key: {d}')
meta = d.get('meta', {})
if not meta.get('url'):
    sys.exit(f'fetched dashboard has no meta.url — provisioning did not complete cleanly: {meta}')
panels = d['dashboard'].get('panels', [])
if not panels:
    sys.exit('fetched dashboard has zero panels — the import silently dropped content')
" || fail "imported dashboard failed the post-import sanity check: $fetch_response"
pass "dashboard imported into Grafana with no provisioning errors ($dashboard_uid)"

# ── 6. Wait for Path B (OTLP push through the collector, uncommented from ───
#    the real committed otelcol.yaml — see uncomment-metrics-pipeline.sh),
#    then re-run the same panel/variable checks, asserting go_*/process_*
#    come back EMPTY (the documented structural gap).
echo "Waiting for the OTLP-ingesting Prometheus to receive pushed metrics..."
# Same rate()-needs-two-samples reasoning as the scrape-path wait above,
# applied on the OTLP-ingesting side: Prometheus still needs two ingested
# data points of a series to compute a rate over it, regardless of how that
# data arrived. Same prom_rate_nonempty function, just the other URL.
for i in $(seq 1 12); do
    sleep 5
    prom_rate_nonempty "$PROM_OTLP_URL" 'sum(rate(mcp_tool_invocation_total{outcome="error"}[5m]))' \
        && prom_rate_nonempty "$PROM_OTLP_URL" 'sum(rate(mcp_semp_request_total{http_response_status_code!~"2.."}[5m]))' \
        && break
    [ "$i" -eq 12 ] && fail "OTLP-ingesting Prometheus never computed a rate for both error series after 60s — the published collector metrics pipeline did not deliver metrics"
done
pass "OTLP-ingesting Prometheus has received pushed metrics — the published collector config works"

python3 verify-panels.py \
    --prometheus-url "$PROM_OTLP_URL" \
    --dashboard "$DASHBOARD_JSON" \
    --check-variables \
    --expect-empty-prefixes go_,process_ \
    || fail "Path B (OTLP push) panel/variable verification failed — see output above"
pass "Path B (OTLP push): every non-Go panel and every variable resolved; Go runtime panels correctly empty"

echo ""
echo "=== All dashboard E2E checks passed on both metrics ingestion paths ==="
