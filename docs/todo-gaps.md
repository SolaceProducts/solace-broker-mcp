# Outstanding Implementation Gaps

Tracked gaps from Jira stories that are deferred or pending further discussion.

## SOL-148423 — Implement SEMP Client Foundation with Connection Pool

### Integration tests against live broker

**Status:** Deferred

**Story requirement:** Under "Testing Approach" and "Definition of Done":
- Integration test: Authenticate to test broker with basic auth, GET
  /SEMP/v2/monitor/broker, verify 200 response
- Integration test: Authenticate to test broker with bearer token, verify 200
  response
- Integration test: Test with invalid credentials, verify 401 error returned
- Integration test succeeds against live broker (both auth modes)
- Session cookies handled automatically (verified via integration test)

**Reason deferred:** Requires a test broker environment and decision on test
gating strategy (env-var skip vs. build tags). To be implemented when CI broker
infrastructure is ready.

## SOL-148427 — Implement Tool Manager Foundation

### LLM-optimized response filtering

**Status:** Deferred

Full SEMP responses are returned unfiltered to AI agents — all fields, regardless
of relevance to the query. This wastes tokens and reduces LLM response quality.

**Filtering approaches:**
- **SEMPv2:** `select` query parameter available for server-side field filtering
  (already supported in step args, not yet used by any tool)
- **SEMPv1 / other sources:** No server-side filtering available — the MCP server
  must filter/transform responses post-retrieval

**Required work:**
- A general-purpose MCP-server-side transformation layer to handle both cases
- Per-step data transformation in composite tool definitions
- Result strategies beyond "collect" (merge, unwrap) — designed but not implemented

**Reason deferred:** Requires design discussion on transformation DSL, field
selection conventions, and per-tool output contracts. To be addressed when
tool output quality becomes a measurable concern.
