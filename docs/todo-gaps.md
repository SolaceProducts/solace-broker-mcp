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

## SOL-147161 Story 21 — Production Deployment Packaging

### systemd service template

**Status:** Deferred

**Story requirement:** Under "Acceptance Criteria" and "Work Breakdown":
- systemd service template created (broker-mcp-server.service)
- Includes: ExecStart, Restart policy, Environment file reference
- User/Group configuration for security
- Documentation for installation on bare metal/VMs

**Reason deferred:** Bare metal operators can run the binary directly or write
their own service file. The statically-linked binary has no dependencies and
handles SIGTERM/SIGINT natively. Add a systemd template (and optionally a
launchd plist for macOS) when operators request it.

### stdio transport support

**Status:** Deferred (future enhancement)

**Motivation:** Streamable HTTP requires the server to be running before the
MCP client connects. For laptop users (e.g., Claude Desktop), stdio transport
enables auto-start — the client spawns the server as a subprocess on launch,
removing the need for manual startup or OS-level service configuration.

**Feasibility:** Straightforward. The `mcp.Server` instance (`server := mcp.NewServer(...)`
in `cmd/server/main.go`) is already transport-agnostic. Steps 1-8 in `main()` (config, OpenAPI parsing,
broker pool, tool registration) have no HTTP dependency. Adding stdio requires:
- A flag or config option to select transport (`--transport stdio|http`)
- Calling the SDK's stdio transport instead of `mcp.NewStreamableHTTPHandler`
- Skipping auth middleware, health endpoint, TLS, and port binding in stdio mode
- Allowing config without `client_auth` when not in HTTP mode

**Estimated scope:** ~30 lines in `main()` or a small refactor extracting
shared initialization into a reusable function.

### Helm chart for Kubernetes

**Status:** Deferred

**Story requirement:** Under "Acceptance Criteria" and "Work Breakdown":
- Helm chart created with Chart.yaml, values.yaml, templates/
- Templates: Deployment, Service, ConfigMap, Secret
- Configurable values: broker URLs, OAuth config, rate limiting, replicas,
  resources
- Default values secure (development_mode: false)

**Reason deferred:** Example Kubernetes manifests are provided in
`deploy/kubernetes/` for copy-paste-edit deployment. A full Helm chart adds
templating, rollback, and multi-instance convenience but is not required for
initial release. Add when Kubernetes becomes a primary deployment target or
early adopters request it.

### Kubernetes deployment section in user documentation

**Status:** Pending

**Requirement:** The upcoming user-facing documentation should include a
Kubernetes deployment guide covering the example manifests in
`deploy/kubernetes/`, how to configure the ConfigMap and Secret, health probe
setup, and security context. Reference `docs/packaging-release.md` for the
technical details.

## SOL-148425 — Implement Rate Limiting and Retry Logic

### Error translation for AI agent consumption

**Status:** Resolved

**Story requirement:** Translate broker errors to human-readable messages with
retryable field and guidance for AI agents.

**Implementation:** Rather than a separate AgentError type, tool execution
errors are returned as MCP-compliant `CallToolResult` with `IsError: true`
per the MCP spec (tool execution errors SHOULD be visible to the LLM, not
opaque protocol errors). The result carries:
- `Content[0].text` — human-readable message using the broker's own error
  descriptions (SEMPv2 `meta.error.description`, SEMPv1 `Error.Message`)
- `StructuredContent` — machine-readable fields: `error`, `retryable`,
  `status`, plus protocol-specific data (`operation`, `sempStatus`,
  `sempCode` for SEMPv2; `kind`, `reasonCode` for SEMPv1; `attempts` for
  retries exhausted)

Only `RetriesExhaustedError` (429/503/5xx after internal retry exhaustion)
is marked `retryable: true`. All client errors and SEMPv1 envelope errors
are `retryable: false`.

SEMPv2 `meta.error` is now parsed into structured `SEMPError` fields
(`Description`, `SEMPCode`, `SEMPStatus`) instead of being discarded as a
raw body string.

See: `internal/tools/manager.go` (`buildErrorResult`, `buildErrorMessage`,
`isRetryable`) and `internal/semp/sempv2/client.go` (`parseSEMPError`).

### Connecting MCP clients to the server

**Status:** Pending

**Requirement:** The user-facing documentation should explain how to connect
MCP clients to the running server. This is the missing link between "server is
deployed" and "I can use it from my AI assistant." Cover:

- **Claude Desktop**: connecting via Custom Connectors (Settings > Connectors >
  Add custom connector) pointing to the `/mcp` endpoint, or using the
  `mcp-remote` npm bridge in `claude_desktop_config.json` for stdio-style
  configuration
- **Claude Code**: configuring the server URL in MCP settings with the `/mcp`
  endpoint
- **OAuth setup**: the `/mcp` endpoint is behind auth middleware; clients must
  be configured with valid OAuth credentials when `development_mode` is false
- **Discovery endpoint**: the server exposes
  `/.well-known/oauth-protected-resource` (RFC 9728) for automatic
  authorization server discovery by MCP clients
- **Development mode**: using `development_mode: true` with a static dev token
  for local testing without a full OAuth setup
