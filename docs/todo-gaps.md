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
