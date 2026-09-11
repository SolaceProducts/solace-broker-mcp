#!/usr/bin/env bash
# E2E tracing test: verifies a full trace (HTTP entry → tool dispatch → SEMP attempt)
# lands in Tempo after a real tool call to a live broker.
# All services (mcp-server, otel-collector, tempo, solace) are started by
# docker compose before this script runs.

set -euo pipefail

MCP_URL="${MCP_URL:-http://localhost:9095}"
TEMPO_URL="${TEMPO_URL:-http://localhost:3201}"
MCP_DEV_TOKEN="${MCP_DEV_TOKEN:-e2e-dev-token}"
BROKER_ALIAS="solace"

pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

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
response=$(curl -sf -D - -X POST "$MCP_URL/mcp" \
    -H "Content-Type: application/json" \
    -H "Accept: application/json, text/event-stream" \
    -H "Authorization: Bearer $MCP_DEV_TOKEN" \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"e2e-tracing","version":"1.0.0"}}}')

session_id=$(echo "$response" | grep -i "mcp-session-id" | awk '{print $2}' | tr -d '\r')
[ -z "$session_id" ] && fail "No Mcp-Session-Id in initialize response"
pass "Session initialized: $session_id"

# ── 3. Call get-broker-status (produces full span hierarchy incl. semp.request) ──
tool_response=$(curl -sf -X POST "$MCP_URL/mcp" \
    -H "Content-Type: application/json" \
    -H "Accept: application/json, text/event-stream" \
    -H "Authorization: Bearer $MCP_DEV_TOKEN" \
    -H "Mcp-Session-Id: $session_id" \
    -d "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"get-broker-status\",\"arguments\":{\"broker\":\"$BROKER_ALIAS\"}}}")

echo "$tool_response" | grep -q '"result"' || fail "Tool call did not return a result"
pass "get-broker-status succeeded"

# ── 4. Wait for batch flush (collector batches for up to 5s) ─────────────────
echo "Waiting for trace batch flush..."
sleep 10

# ── 5. Assert trace landed in Tempo ─────────────────────────────────────────
tempo_response=$(curl -sf "$TEMPO_URL/api/search?tags=service.name%3Dsolace-broker-mcp")
trace_count=$(echo "$tempo_response" | python3 -c "import sys,json; d=json.load(sys.stdin); print(len(d.get('traces', [])))")

[ "$trace_count" -gt 0 ] || fail "No traces found in Tempo for service solace-broker-mcp"
pass "Found $trace_count trace(s) in Tempo"

# ── 6. Assert semp.request span is present across all traces ─────────────────
found_semp=false
while IFS= read -r trace_id; do
    spans=$(curl -sf "$TEMPO_URL/api/traces/$trace_id" | python3 -c "
import sys, json
d = json.load(sys.stdin)
names = [s.get('name','') for b in d.get('batches',[]) for ss in b.get('scopeSpans',[]) for s in ss.get('spans',[])]
print('\n'.join(names))
")
    if echo "$spans" | grep -q "semp.request"; then
        found_semp=true
        pass "semp.request span present in trace $trace_id"
        break
    fi
done < <(echo "$tempo_response" | python3 -c "import sys,json; d=json.load(sys.stdin); [print(t['traceID']) for t in d['traces']]")

[ "$found_semp" = true ] || fail "semp.request span not found in any trace"

echo ""
echo "=== All tracing E2E checks passed ==="
