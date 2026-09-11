# Product architect — SOL-154210

Date: 2026-09-11

Link: https://sol-jira.atlassian.net/browse/SOL-154210
Parent epic: SOL-153075 (Broker MCP GA Readiness)

Story: log advertised OAuth Protected Resource Metadata (PRM), the 401 metadata URL, and registered well-known paths once at startup.

This is an operator-visibility product. It does not change OAuth discovery on the wire.

## Persona

Operator (and support reading their logs) of an MCP server with `mcp_client_auth.mode: oauth`.

Not the LLM, not the IdP admin, not the broker admin.

Job: a client (Claude Code, Agent Mesh, and similar) cannot complete OAuth discovery. The operator needs to answer: did **this process** advertise what we think we configured?

The issuer banner and `config loaded` line do not answer that. Today the only related log is `registered OAuth protected resource metadata endpoint` with no fields. Confirming the advertised document otherwise means curling the box or reading Go.

## Outcome (done when)

After oauth startup that registers PRM, **one INFO line** is a complete, truthful snapshot. Operators can grep it. They do not curl the box or read Go.

That line must show:

1. `resource` and issuer (`authorization_servers`) as they appear in the PRM JSON
2. the URL placed on 401 `WWW-Authenticate` `resource_metadata`
3. GET paths on **this process** that serve that JSON (bare `/.well-known/oauth-protected-resource` vs the RFC 9728 §3.1 path suffix taken from `resource_url`)
4. `scopes_supported` and `bearer_methods_supported` (today hardcoded `openid` / `header`)

Static or disabled: **no this line** (PRM routes are not registered).

The snapshot is truthful when it matches what this process actually put on the wire and on `mux.Handle`, including the known split: 401 `resource_metadata` is always the bare `{scheme}://{host}/.well-known/oauth-protected-resource` form; an extra registered path exists only when `resource_url` has a path. Logging both makes that split visible.

## Locked product decisions

- Advertise the snapshot **once** at oauth startup. Extend the existing `registered OAuth protected resource metadata endpoint` line with fields. Do not add a second event type for the same fact.
- Do **not** log the advertised PRM / `resource_metadata` URL again on each 401. 401s already have a different job: failure reason (`auth_failure` / `mcp_auth_failure_total`). The 401 header URL does not vary per request; repeating it is noise. If the header ever disagreed with startup, that is a bug in the single formula, not a reason for a second log.
- Do **not** log on each PRM GET. The payload does not change per request.
- Do **not** change what is advertised (JSON, header URL, which paths).

Logged URLs stay sanitized like other operator-facing URLs.

## Correlation IDs

Do **not** attach `correlation_id` to the PRM registration log.

That line fires in `registerMetadataRoutes` at process startup, after `mux.Handle` and before `ListenAndServe`. There is no HTTP request, no client, and no `X-Correlation-ID`. Correlation IDs are stamped only on the `/mcp` chain (`buildMCPEndpoint` → `correlation.Middleware`). Well-known PRM routes are registered on the mux without that middleware; recovery wraps the whole mux later and does not invent an ID.

The slog wrapper already omits `correlation_id` when `correlation.From(ctx)` is empty (startup / non-request). Forcing a fake or empty ID would invent a join key that looks like a request and mislead operators grepping one ID across `auth_failure`.

## Not this ticket

- Changing RFC 9728 behavior, JSON fields, 401 header URL, or which well-known paths are registered
- Per-request access logs for PRM GET or `/mcp` 401s
- SIEM-specific new event types (security can consume the same startup line later; that is a second consumer, not a second feature)

## Docs that ship with this change

- `docs/internal/secure-logging-rules.md`: the Always-Log table owns the required slog keys for this event.
- `docs/observability.md`: this registration INFO has no `correlation_id` because it runs at startup, outside a request.
- `docs/authentication.md`: the startup log is the first troubleshooting check; the existing curl check remains valid.

## Why this is the product

Support tickets for MCP OAuth discovery fail when advertised `resource` / issuer / well-known path disagree with the URL the client used. Startup logs are where operators confirm that. This ticket makes the advertisement inspectable without changing it.
