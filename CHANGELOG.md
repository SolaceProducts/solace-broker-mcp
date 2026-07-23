# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

> **Maintaining this file:** draft entries per PR under `[Unreleased]` (the
> `/changelog` skill drafts one from your diff), and the release process promotes
> `[Unreleased]` into a dated version block at tag time — see `RELEASING.md`. Entries
> for released versions below were reconstructed from the per-tag GitHub Release notes;
> the concise ones cite their SOL ticket, the detailed paragraphs were authored at the
> time of the change.

## [Unreleased]

### Added
- New MCP tools `list-kafka-receivers`, `get-kafka-receiver-status`, `list-kafka-senders`, and `get-kafka-sender-status` for Kafka Receiver/Sender monitoring — the same list-then-drill-down pattern as `list-rdps`/`get-rdp-status` and `list-bridges`/`get-bridge-status`. A Kafka Receiver pulls messages from an external Kafka cluster into a Message VPN; a Kafka Sender pushes messages from a VPN's queues out to an external Kafka cluster. Unlike bridges' two-directional connection-state model, both objects use a single `up`/`enabled`/`failureReason` health shape (the same shape as RDPs) and a single-name identifier (`kafkaReceiverName`/`kafkaSenderName`, not a compound key). `list-kafka-receivers`/`list-kafka-senders` return up to 100 by default (max 500 via `maxResults`), with a summary block (`downCount`, `disabledCount`, `byFailureReason`); `get-kafka-receiver-status`/`get-kafka-sender-status` return additional per-object detail including topic-binding health (`topicBindingUpCount`/`topicBindingCount`) or queue-binding health (`queueBindingUpCount`/`queueBindingCount`). Read-only monitoring only; write tools and live-broker E2E fixture coverage are deferred to follow-up tickets. Unlike bridges, this implementation is spec-derived rather than lab-verified: neither lab appliance available at implementation time (`sempVersion 100.0main.0.7305` and `10.26.0.8586`) exposes the `kafkaReceivers`/`kafkaSenders` SEMP paths at all, so whether `failureReason` reliably populates on a real down receiver/sender — versus staying empty the way bridges' `inboundFailureReason` did — remains unconfirmed; a follow-up should verify against a broker with the feature licensed/enabled. Tracked under SOL-152328.
- New MCP tools `list-bridges` and `get-bridge-status` for Bridge monitoring — the same list-then-drill-down pattern as `list-rdps`/`get-rdp-status`. `list-bridges` returns enabled state, inbound/outbound connection state, and last inbound failure reason for every bridge in a VPN (default 100, max 500 via `maxResults`), with a summary block (`downCount`, `disabledCount`, `byInboundFailureReason`). `get-bridge-status` returns additional per-bridge diagnostic detail — connection establisher, failure category, and client name — for one bridge, identified by `msgVpnName` + `bridgeName` + `bridgeVirtualRouter` (bridges are the only object in this server keyed by two names rather than one, since a bridge's config can differ between a broker's primary and backup virtual router in an HA pair). Tracked under SOL-152124.
- Process-level circuit breaker in front of RFC 8693 token exchange to the IdP, so a sustained IdP outage fails fast instead of driving every broker's requests into the full retry/timeout budget. One breaker per process guards the single shared IdP; because the deployment has one IdP, one client credential, and one shared availability dependency, all brokers and tenants share it. A "request" for the breaker is one complete logical exchange *after* the retry loop has finished — a chain that retries a 5xx three times before giving up records one failure, not three. Failure classification is owned by the server, not the operator: transient network errors, upstream 5xx, and response-body-read failures count toward the breaker; HTTP 429 is excluded (the IdP is reachable and throttling, so counting it would let one tenant's rate-limit trip the shared breaker); token rejections, caller cancellations, and local request-build errors are excluded (they say nothing about IdP availability). Endpoint *misconfiguration* — an untrusted/expired TLS certificate, a hostname mismatch, or a DNS name-not-found for the configured IdP — is also excluded: it is a permanent operator fault, not an outage, and counting it would let one bad config value trip the shared breaker for every tenant with no chance to heal (a DNS *timeout*, by contrast, is transient and does count). Once open, exchanges are rejected immediately with a new `ErrExchangeCircuitOpen` sentinel — classified transient like a transport failure but distinct in the audit line ("we did not try" vs. retries-exhausted's "we tried and gave up"); state transitions log at WARN. Configured under a new optional `broker_oauth.circuit_breaker:` block (nested there because the breaker only protects OAuth token exchange, which exists only when `broker_oauth` does): `enabled` (default true; `false` is an operational escape hatch that disables the breaker while leaving retries intact and logs a WARN), `failure_rate_window`, `minimum_requests`, `failure_rate_threshold_percent`, `consecutive_failure_threshold` (fast-trips a fast-failing outage without waiting for the rate rule's sample; the count is window-bound, so failures spaced out by long retry chains may not accumulate; `0` disables it), `open_state_duration`, and `half_open_probe_requests`. Every field is optional and falls back to a production-safe default, so omitting the block (or the whole thing) yields the shipped defaults with the breaker on; out-of-bounds values are rejected at config load alongside every other `broker_oauth` error. The internal bucket granularity and breaker name are derived, not operator-configurable. Rides entirely under the `broker_oauth` runtime, which remains gated behind `ENABLE_UNRELEASED_BROKER_OAUTH` and unactivated in the wild, so no existing deployment is affected. Tracked under SOL-151600.
- Server-side retry loop for RFC 8693 token exchange to the IdP. Previously a single 5xx, HTTP 429, or connection error to the IdP failed the tool call outright; the loop now transparently retries up to three attempts total against transient signals (5xx, 429, and connection-level errors — DNS, TLS handshake, body-read partials), honoring the IdP's `Retry-After` header uncapped between attempts via jittered linear backoff. 4xx client errors (400, 401, 403) are deliberately not retried — they will not fix themselves on repeat. The whole retry loop is fenced by a chain-deadline (`context.WithTimeout` derived from the retry knobs — currently 19s worst case with the shipped defaults of 5s per attempt, 2 retries, and 1–2s backoff), which also bounds a hostile `Retry-After` value. When all attempts fail, a new `ErrExchangeRetriesExhausted` sentinel distinguishes "we tried and gave up" from a single-shot transport failure; the last attempt's HTTP status and IdP endpoint survive on the same `*ExchangeError` envelope for the audit line. JWKS refresh and OIDC discovery paths deliberately keep the non-retrying HTTP client — they are read-only lookups where a single failure is the correct signal. Tracked under SOL-151520.
- New top-level `tool_authorization:` configuration block for per-tool authorization based on the caller's group or role memberships as carried by a configurable OIDC claim. Required under `mcp_client_auth.mode: oauth` and refused under any other mode — the `enabled` field must be set explicitly to `true` or `false`, forcing every oauth deployment to make a deliberate choice on tool-level access control. When `enabled: true`, the block also carries `groups_claim_name` (name of the OIDC claim carrying the memberships; defaults to `"groups"`) and `access_level_groups` (a map from group name to the list of MCP tool names that group grants; union semantics — a caller is allowed to invoke a tool when at least one of their groups grants it). `list-brokers` is structurally exempt — every authenticated caller can invoke it regardless of the policy, so a caller can always discover configured broker aliases before invoking any other tool; listing it under a group in `access_level_groups` is inert. Authorization decisions live in a new `internal/authz` package; its `Decision` type implements `slog.LogValuer` to emit only an `allowed` flag and a matched-group count, never the group names themselves, so the sanctioned logging path cannot leak group membership. Tracked under SOL-151875, SOL-151907, and SOL-152293.
- Apache 2.0 LICENSE file for open source compliance
- CONTRIBUTING.md with comprehensive contribution guidelines including DCO requirements
- CODE_OF_CONDUCT.md with Contributor Covenant 2.1
- SECURITY.md with vulnerability disclosure policy and security best practices
- GitHub issue templates for bug reports and feature requests
- GitHub pull request template with comprehensive checklist
- Copyright headers to all Go source files
- Status badges in README.md (build, license, Go version, code of conduct)
- Contributing and License sections in README.md

### Changed

- **BREAKING**: Under `mcp_client_auth.mode: oauth` (production) the server now refuses to start with a plaintext listener — i.e. when neither `tls_cert_file`/`tls_key_file` nor a new top-level `tls_terminated_upstream: true` opt-in is set. Previously such a config started silently and transmitted client bearer tokens and tool results in cleartext while validating as production. TLS termination at an upstream proxy/ingress is a supported pattern, so the fix is enforcement with an explicit opt-out: set `tls_terminated_upstream: true` to acknowledge that TLS is terminated upstream — the server then serves plaintext and logs a startup WARN naming the acknowledgment. Providing `tls_cert_file`/`tls_key_file` terminates TLS at the server as before. Mirrors the existing `allow_remote_unauthenticated` / `allow_insecure_broker_tls` guards; the field is ignored in the `disabled`/`static` dev modes. Migration: an `oauth` deployment without server-side TLS certs must set `tls_terminated_upstream: true` (or configure certs). Because the config parser rejects unknown fields, an older binary will fail to start if the field is present — so deploy the new binary and the updated config together (the field and the binary that understands it move as one change), and when rolling back to an older binary, remove the field in the same change. Completes the server-listener side of the production-mode transport posture begun in SOL-149665. Tracked under SOL-150692.

- **BREAKING**: MCP tool `get-vpn-health` renamed to `get-vpn-status`. Matches the `get-broker-health` → `get-broker-status` precedent (SOL-150707): the tool reports raw VPN state (enabled, connection count, per-service up/down), not a health verdict — whether a VPN is "healthy" depends on deployment intent the server doesn't have. Input parameters and output shape are unchanged; only the tool name and step key (`vpnHealth` → `vpnStatus`) changed. Migration: any client invoking the tool by name must switch to `get-vpn-status`. Tracked under SOL-151742.

- SEMP permission-denied (code 72) errors are now tagged with the calling tool and operation, so an operator can see which tool call hit a broker authorization limit. Tracked under SOL-151718.

- Disambiguated the `clear-queue-stats` and `delete-queue-messages` tool descriptions so agents stop conflating "reset counters" with "purge messages." Tracked under SOL-151850.

- Added a one-line pointer from each of `list-vpns`, `list-queues`, and `list-clients` to its list-then-drill-down "show details" counterpart (`get-vpn-status`, `get-queue-metrics`, `get-client-details` respectively), so an agent that only reads the list tool's description still discovers the detail tool exists — matching the pointer `list-rdps`/`list-bridges` already had to `get-rdp-status`/`get-bridge-status`. No behavior, parameter, or output-shape change; description text only. Tracked under SOL-152122.

- When a write tool is given a body attribute the embedded SEMP schema doesn't recognize, the client-side rejection now names all three possible causes: typo, tool-only param that should be declared as path/query/header, or a broker newer than the embedded schema. The embedded schema version is logged via `slog.Debug` for operator correlation. Previously the message framed the only cause as a naming mistake, hiding the legitimate case where the target broker is newer than the spec the server was built against. Tracked under SOL-152207.

- Removed the dangling "See the SEMP `<Type>` schema for the full list." pointer from the `create-message-vpn`, `update-message-vpn`, `create-queue`, `update-queue`, `create-topic-endpoint`, `update-topic-endpoint`, `create-rdp`, and `update-rdp` write-tool descriptions. The MCP server does not expose the SEMP schema, so the sentence sent agents chasing a resource they cannot reach; the attribute examples already inline in each description remain. Tracked under SOL-152203.

- Reshaped the agent-facing surface for token-exchange failures. The `error` string a tool call returns for a token-exchange failure now collapses from six sentinel-specific messages into two categories: transient (`ErrExchangeTransport` and `ErrExchangeRetriesExhausted`) surface as `"Authentication is unavailable — the identity provider is not responding."` (deliberately not broker-named because a shared-IdP outage affects every broker at once); everything else surfaces as `"Authentication failed for broker \"X\". This is a server-side issue."` with the broker alias interpolated so a multi-broker operator can grep the audit line for context. No IdP-generated content is embedded in these strings, so an IdP that leaks sensitive material in an `error_description` field cannot bleed it into the agent surface. Related: the `retryable` flag on the tool result — which agents may branch on — now returns `false` for `ErrExchangeRetriesExhausted` (we already gave up, retrying doesn't help), for a 4xx response whose body is not a standard OAuth `error` JSON (previously `true` — a proxy/WAF interception page will not become a valid OAuth response on retry), and for an oversized OAuth error code (previously `true`). Sentinel-specific detail (endpoint, HTTP status, elapsed) still lands on the audit log line via `LogAttrs`, which is the right audience for it. Tracked under SOL-151520.

- Updated the embedded SEMPv2 OpenAPI specs to the 10.26.2 rolling release (`10.26.2.9715`), so tool schemas track current broker attributes. The three spec files are renamed to `semp-v2-swagger-{action,config,monitor}.10.26.2.json`. Loading is unaffected — specs are picked up by the embed glob and typed from their `basePath`, not their filename. Tracked under SOL-152206.

- SEMP `429 Too Many Requests` and `503 Service Unavailable` responses are now retried at most 3 times, independently of the configured `retries` cap (default 10). A 429/503 signals an overloaded or unavailable broker, and these retries bypass the per-broker rate limiter, so retrying up to 10 times amplified load precisely when the broker could least handle it. The cap bounds a transient episode to 4 requests (original + 3 retries); once reached, the call fails with the same retries-exhausted error it returned before, only sooner. Other retry behavior is unchanged: the single retry for other 5xx, 401 re-authentication, connection-error retries, and the overall retry-chain deadline all still honor the full `retries` budget. Distinct from the retry-chain deadline (SOL-151518), which bounds duration rather than the number of attempts. Tracked under SOL-152209.

### Removed

- Per-broker `brokers.<alias>.auth.scopes` field. The Hop-2 OAuth runtime is gated behind `ENABLE_UNRELEASED_BROKER_OAUTH` and was never activated with the field populated in the wild, so this removes surface rather than user-visible behavior. Rationale: scope entitlement is a property of the user (carried on the subject token), not of the broker — a single static per-broker list either hard-fails less-privileged users (strict IdPs reject `invalid_scope`) or under-scopes admins. The exchange request now omits the RFC 6749 §3.3 `scope` parameter, so the IdP grants its per-client / per-user default scopes. Config decoding is strict (`KnownFields(true)`), so any config still declaring `auth.scopes` fails loudly at startup — this is intentional and covered by a test. Follow-up SOL-151816 will design a per-user scope mechanism; if per-call scopes ever vary for the same subject token, they must also join the token-exchange dedup key (`internal/tokenexchange/dedup_key.go`).

### Fixed

- OAuth broker requests now recover from an expired token in-flight. When a broker returns `401 Unauthorized` under `auth.mode: oauth`, the SEMP transport evicts the cached broker token and retries the request once with a freshly exchanged token, instead of failing the call immediately. The recovery decision belongs to the authenticator: `HandleAuthFailure` returns whether to retry and whether the retry must re-authenticate, so static modes (basic, bearer) are unaffected. The re-auth is capped at one attempt per request, so a persistently rejected credential surfaces as a `401` rather than looping. If the token exchange itself fails during the retry (e.g. the IdP is unreachable), the failure is now logged with broker and operation and carries the last observed `401` status. Tracked under SOL-151624.

- `list-vpns` aggregation no longer reports a dead `zeroConnectionCount`; the count now reflects actual per-VPN connection state. Tracked under SOL-151771.

### Security

- `Token` and `Exchange` types now implement `LogValue`/`String`/`GoString` guards so raw OAuth tokens cannot leak through `slog`, `fmt`, or `%#v` rendering. Tracked under SOL-151649.
- SEMPv2 path-parameter values are now rejected before URL construction when they are empty or a dot segment (`.` or `..`). `buildURL` escaped `/` but left dot segments intact, so a value like `..` could, on a broker or fronting proxy that normalizes dot segments, collapse a request onto an unintended (e.g. parent) path — most consequentially on the destructive `config/deleteMsgVpn`, `config/deleteMsgVpnQueue`, and `action/doMsgVpnQueueDeleteMsgs` operations. The guard rejects the value with a clear error naming the parameter and covers read and destructive operations at the single substitution choke point. Tracked under SOL-152208.

## [0.5.0] - 2026-07-09

### Added
- Action tools for queues and clients (queue actions and client actions). Tracked under SOL-148462.
- Core management tools for VPN and queue administration. Tracked under SOL-148460.
- RDP (REST Delivery Point) write tools. Tracked under SOL-148461.
- `/livez` liveness endpoint, with `/health` retained as an alias. Tracked under SOL-151283.
- `/readyz` readiness endpoint reflecting MCP-server readiness, decoupled from broker reachability. Tracked under SOL-151284.
- Kubernetes split liveness/readiness/startup probes; `/ready` aliased to `/readyz`. Tracked under SOL-151285.
- Correlation-ID HTTP middleware: the ID is forwarded to SEMPv1/v2 broker requests, returned in the MCP response header and `CallToolResult.Meta`, and emitted as a `slog` attribute on every request-scoped log line. Tracked under SOL-151279, SOL-151280, SOL-151281, SOL-151282.
- Whole-mux HTTP panic-recovery middleware. Tracked under SOL-151286.
- SIGTERM in-process drain (`/readyz` flips to 503 → drain → graceful shutdown); a second signal forces immediate shutdown. Tracked under SOL-151288 and SOL-151437.
- `listen_address` server config with a loopback-only default, so the server does not bind a public interface unless explicitly configured. Tracked under SOL-150690.
- RFC 8693 OAuth token-exchange runtime (broker-OAuth "Hop 2"), gated behind `ENABLE_UNRELEASED_BROKER_OAUTH`. Tracked under SOL-150799, SOL-150800, SOL-150801.
- Aggregations for `list-vpns`, `list-rdps`, `list-slow-subscribers`, `list-queue-discards`, and `get-rdp-status`. Tracked under SOL-151313, SOL-151314, SOL-151315, SOL-151316, SOL-151343.
- Accurate live queue-depth reporting. Tracked under SOL-150260.
- New top-level `broker_oauth:` configuration block for upcoming OAuth authentication from the MCP server to brokers. Schema-only in this release — the OAuth runtime is not yet wired, and any broker with `auth.mode: oauth` is rejected at startup with a standalone error banner explaining the limitation and a per-broker validation error in the joined config error. The block holds the global IdP coordinates the MCP server will use to obtain broker-bound tokens: `idp_token_endpoint`, `mcp_server_client_id`, an `mcp_server_client_auth` discriminated union (one named sub-block per IANA "OAuth Token Endpoint Authentication Methods" identifier — V1 supports `client_secret_basic` and `client_secret_post`), `grant_type` (allowlisted to RFC 8693's token-exchange URN), and `audience_parameter_name` (allowlisted to `audience` | `scope` | `resource`). The discriminated-union shape structurally prevents misconfigured method/credential pairings — operators choose a method by populating its named sub-block; the validator enforces "exactly one sub-block populated." Per-broker `auth.mode: oauth` accepts an optional `audience` field whose value is forwarded to the IdP when set. The validator also enforces a permanent structural invariant: if any broker uses `auth.mode: oauth`, `mcp_client_auth.mode` must also be `oauth` (the MCP server cannot obtain a broker token without the agent's token from the client-auth side). Operator-facing banners and errors live in a new `internal/banner` package so future banners land in one canonical home. Tracked under SOL-150796.

### Changed

- **BREAKING**: Top-level client-authentication block renamed from `client_auth:` to `mcp_client_auth:`. The Go type renames to `MCPClientAuthConfig`, and the field on `ServerConfig` renames to `MCPClientAuth`. The rename disambiguates the operator-facing schema now that the new `broker_oauth:` block introduces a separate `broker_oauth.mcp_server_client_auth:` nested sub-block — the top-level `client_auth:` name was ambiguous between client authentication on the inbound side (agents authenticating to the MCP server) and on the outbound side (the MCP server authenticating to the IdP). Migration: rename the top-level `client_auth:` key in every config to `mcp_client_auth:`. No field semantics change; only the block name. Tracked under SOL-150796.

- **BREAKING**: A broker configured with `insecure_skip_verify: true` is now refused at startup under `mcp_client_auth.mode: oauth` (production), unless a new top-level `allow_insecure_broker_tls: true` opt-in is also set. Previously this combination started successfully with only a `slog.Warn`, leaving TLS certificate verification disabled while the server still sent the broker admin credential over the connection on every SEMP request — an exploitable man-in-the-middle path. Mirrors the existing `allow_remote_unauthenticated` guard. `allow_insecure_broker_tls` is server-wide, not per-broker: it lifts the check for every broker in the config, not just the one being onboarded. Dev/static modes are unchanged and continue to allow self-signed brokers without the opt-in. Migration: add `allow_insecure_broker_tls: true` to accept the risk, or install a trusted certificate on the broker. Tracked under SOL-151517.

- Validator now trims whitespace before checking emptiness for basic-auth `username`/`password` and bearer-auth `token`. Configs whose credentials resolve to a whitespace-only string (e.g., `token: " "` or a `${VAR}` substitution that yields only whitespace) are now rejected at startup with a clear "required for X auth" error rather than passing validation and failing every SEMP request with a 401 at runtime. Tracked under SOL-150796.

- Lifted `SafeCookieJar` to the broker level and delegated 401 recovery to the `Authenticator`. Tracked under SOL-151468.
- Unified `downCount` semantics across the `list-*` aggregating tools. Tracked under SOL-151552.

### Fixed
- Bound broker-controlled error text at capture, so an oversized broker error can't blow up memory or logs. Tracked under SOL-151516.
- Recover panics in `errgroup` worker goroutines instead of crashing the server. Tracked under SOL-151514.
- Bound the overall SEMP retry chain with a deadline. Tracked under SOL-151518.

## [0.4.0] - 2026-06-16

### Added
- Error translation and output sanitization for tool responses. Tracked under SOL-148434.
- `get-hardware-details` step for appliance platforms. Tracked under SOL-150708.

### Changed
- **BREAKING**: MCP tool `get-broker-health` renamed to `get-broker-status`. The tool reports raw broker state, not a health verdict. Migration: any client invoking the tool by name must switch to `get-broker-status`. Tracked under SOL-150707.

- Replaced the package-level `auth.AddAuth(ctx, req, cfg)` dispatcher with an `auth.Authenticator` interface and per-broker instances. `NewBrokerClient` is the single builder: it constructs one `Authenticator` per broker from `brokerCfg.Auth` and passes the same pointer to both the SEMPv1 and SEMPv2 protocol clients. The clients no longer read `brokerCfg.Auth`; they store the Authenticator on the struct and call `c.authenticator.AddAuth(ctx, req)` per request. No behavior change for existing `basic` and `bearer` auth modes — same Authorization headers, same retry/timeout posture, same config schema. Internal Go API: `sempv1.NewHTTPClient` and `sempv2.NewHTTPClient` signatures gained an `auth.Authenticator` parameter; these are only called from `semp.NewBrokerClient` and tests in the same module. Enables the upcoming OAuth Token Exchange (Hop 2) support without further protocol-client branching. Tracked under SOL-150794 and SOL-150795.

- Enabled retries for read-only SEMPv1 `show` commands. Tracked under SOL-150664.
- Enforced a per-broker connection bound via `MaxConnsPerHost` and a shared in-flight semaphore. Tracked under SOL-150665 and SOL-150116.

### Fixed
- Truncate broker-controlled error text at capture. Tracked under SOL-150663.
- Cap inbound `/mcp` request body at 4 MiB. Tracked under SOL-150660.
- Close the response body when SEMP retries are exhausted. Tracked under SOL-150661.
- Recover tool-handler panics instead of crashing the server. Tracked under SOL-150685.

### Security
- Keep secrets out of YAML config parse errors in startup logs. Tracked under SOL-150666.

## [0.3.0] - 2026-06-03

### Added
- Per-invocation caller identity on tool-invocation log lines (`sub`, `iss`, `client_id`, `jti`) in `oauth` and `static` client-auth modes. Missing optional claims surface as the `<absent>` sentinel so log consumers see a stable schema; `disabled` mode emits no identity fields. Tracked under SOL-149606.
- Basic health endpoints. Tracked under SOL-148426.
- Advanced monitoring tools: replication, discards, and slow subscribers. Tracked under SOL-148432.

### Changed

- **BREAKING**: Client auth config consolidated into single required `client_auth.mode` enum (`disabled` | `static` | `oauth`). The legacy `development_mode` flag is deprecated and ignored — its presence in YAML logs a deprecation warning at startup. The previous "development_mode + empty dev_token = silent no-auth" path (SOL-149921) is replaced by the explicit `mode: disabled`. Migration:

  | Old config | New config |
  |---|---|
  | `development_mode: true` + `dev_token: "abc"` | `client_auth: { mode: static, dev_token: "abc" }` |
  | `development_mode: true` + missing/empty `dev_token` | `client_auth: { mode: disabled }` |
  | `development_mode: false` + OIDC fields | `client_auth: { mode: oauth, issuer, audience, resource_url }` |

  `mode: oauth` is the only legal production mode and enforces `https://` on broker URLs, issuer, and resource_url. `mode: disabled` and `mode: static` are development-only and allow `http://`. A prominent WARN-level boot banner fires for `disabled` and `static` modes. Tracked under SOL-149989.

- **BREAKING**: Broker aliases must now satisfy a contract: 1–63 characters, only letters/digits/hyphens, must start and end alphanumeric, compared case-insensitively. Configs that previously loaded silently will now be rejected at startup if they contain: empty aliases, whitespace, underscores, dots, embedded special characters, leading or trailing hyphens, aliases longer than 63 characters, or case-only collisions (e.g. `Prod` and `prod` in the same config). Original casing is preserved in all user-facing output (logs, `list-brokers`, error messages); tool calls resolve case-insensitively so any casing of a configured alias works. Migration:

  | Old alias | New alias |
  |---|---|
  | `prod_east` | `prod-east` |
  | `Prod` + `prod` (collision) | rename one of them |

  Tracked under SOL-149789.

- `ToolManager.CallTool` now takes a trailing `Identity` argument carrying per-invocation audit identity. This is an internal Go API (the package is `internal/tools`); on-the-wire MCP tool schemas and operator-visible config are unchanged. Tracked under SOL-149606.

- Config loader now rejects unknown YAML fields at startup instead of silently ignoring them. Previously, a typo like `developmnet_mode` or `insecure_skip_verfy` was accepted and the operator's intended override became a no-op. The loader now fails fast with an error naming the offending field. Existing configs with stale or misspelled keys will fail to start until the typo is corrected; configs with only valid keys are unaffected. Tracked under SOL-149927.

- Skip environment-variable substitution inside YAML comments. Tracked under SOL-149904.

### Deprecated
- The `development_mode` config flag is deprecated in favor of `client_auth.mode` (see the consolidation entry under Changed). It is still parsed but ignored, and logs a deprecation warning at startup. Tracked under SOL-149989.

### Removed
- **BREAKING**: `get-dmr-status` tool removed. Migration: clients invoking `get-dmr-status` must stop; DMR state is available through the remaining monitoring tools. Tracked under SOL-150316.

### Fixed
- OIDC token verifier now bounds the HTTP client used by go-oidc for both startup discovery and lazy JWKS refresh (10s per-request timeout). Previously, the verifier fell back to `http.DefaultClient` (zero timeout), and a slow or hung identity provider during key rotation could wedge the JWKS-refresh goroutine indefinitely and stall per-request token verification past the inbound MCP request's own server-side deadlines. The existing 30s discovery deadline is preserved. Operators running an IdP that legitimately takes longer than 10s to serve `/jwks` will see auth fail closed; document the timeout if your environment requires tuning. Tracked under SOL-150219.
- Wrap the cookie jar in an atomic `SafeCookieJar` to remove a 401 race. Tracked under SOL-149922.
- Cap broker response body at 16 MiB. Tracked under SOL-149920.
- Set granular timeouts on the SEMP HTTP transport, and `ReadTimeout`/`IdleTimeout` on the MCP HTTP server. Tracked under SOL-149925 and SOL-149914.
- Close the broker pool on graceful shutdown. Tracked under SOL-149926.

### Security
- Redact userinfo credentials in broker-URL validation errors. Tracked under SOL-149923.
- Warn on `insecure_skip_verify: true` in production mode. Tracked under SOL-149928.

## [0.2.0] - 2026-05-15

### Added
- SEMPv1 SEMP API client foundation. Tracked under SOL-148424.
- VPN-level monitoring tools: health, list, queues, and clients. Tracked under SOL-148429.
- Remaining monitoring tools: DMR, message rates, and RDP. Tracked under SOL-148433.
- `get-broker-health` tool. Tracked under SOL-148428.
- `select=` field filtering across the monitoring tools to reduce the response size returned to the LLM. Tracked under SOL-149404.
- Pagination page-count cap to prevent infinite loops. Tracked under SOL-149750.
- CODEOWNERS file. Tracked under SOL-149790.

### Changed
- **BREAKING**: Reject `http://` broker and auth URLs in production mode. Migration: use `https://` for broker and auth URLs in production, or run in a development auth mode. Tracked under SOL-149665.
- Rate limiting, retry logic, and MCP error translation. Tracked under SOL-148425.
- Skip retry for non-idempotent HTTP methods (POST/PATCH). Tracked under SOL-149746.
- Reject unknown fields in the composite YAML loader. Tracked under SOL-149670.
- Tune the HTTP transport connection pool (`MaxIdleConnsPerHost`). Tracked under SOL-149748.
- Fix misleading `list-*` tool descriptions that claimed to return all results. Tracked under SOL-149343.

### Fixed
- `.env` parser: strip quoted values and warn on an unreadable file. Tracked under SOL-149664.
- Add `sync.RWMutex` to `ToolManager` to guard concurrent handlers-map access. Tracked under SOL-149671.
- SEMPv2 HTTP headers: omit `Content-Type` on bodyless requests, add `Accept`. Tracked under SOL-149672.
- `buildURL` errors on an unfilled path-parameter placeholder. Tracked under SOL-149749.
- Replace `os.Exit(1)` in the `ListenAndServe` goroutine with an error channel. Tracked under SOL-149747.
- Fix server startup/shutdown logging gaps. Tracked under SOL-149595.

## [0.1.0] - 2026-04-24

### Added
- Initial release of Solace Broker MCP Server
- **Tool Manager Foundation**
  - Generic tool registration and routing infrastructure
  - Parameter and output validation against JSON Schema
  - Broker resolution and connection pooling
  - Structured logging for all tool invocations
- **Composite Tool Engine**
  - YAML-driven multi-step tool definitions
  - Go template-based argument resolution
  - Parallel and sequential step execution
  - Configurable result strategies (collect, merge, unwrap)
- **SEMP Client Layer**
  - HTTP client with Basic Auth and Bearer token support
  - OpenAPI spec parser (799 operations from Monitor, Config, Action APIs)
  - Lazy broker connection pooling with thread-safe double-checked locking
  - Per-broker HTTP transport and connection pooling
- **Configuration Management**
  - YAML config file with environment variable substitution (`${VAR_NAME}`)
  - `.env` file loading for local development
  - Validation for broker URLs, auth modes, ports, TLS pairing
  - Multiple broker support with independent credentials
- **Authentication & Security**
  - OAuth/JWT token validation with OIDC provider integration
  - Development mode with optional static dev token
  - Automatic JWKS key rotation
  - Scope-based access control (optional)
  - OAuth 2.0 Protected Resource Metadata endpoint (RFC 9728)
- **Secure Logging**
  - Structured JSON logging with `log/slog`
  - Credential redaction via `slog.LogValuer` pattern
  - `ReplaceAttr` safety net for defense-in-depth
  - Configurable log levels (debug, info, warn, error)
  - Never logs passwords, tokens, or authorization headers
- **Testing Infrastructure**
  - Unit tests across all packages with `-race` detector
  - Integration tests for tool manager and handlers
  - E2E test suite with two-broker Docker Compose setup
  - OAuth integration tests with Keycloak
  - Comprehensive test coverage (config, semp, composite, tools, auth)
- **CI/CD Pipeline**
  - GitHub Actions workflow for lint, build, test, E2E
  - golangci-lint with security checks (gosec, bodyclose, noctx)
  - E2E tests run automatically on all PRs
  - OAuth E2E tests with Terraform-managed Keycloak
- **Production Deployment**
  - Dockerfile with multi-stage build and non-root user
  - Kubernetes manifests (Deployment, Service, ConfigMap, Secret)
  - GitHub Actions release workflow with multi-platform binaries
  - Health check endpoint (`/health`)
  - Graceful shutdown with 120-second timeout
- **Documentation**
  - Comprehensive README with quickstart guide
  - Architecture documentation with component diagrams and request flow
  - Secure logging rules with examples
  - E2E testing guide
  - Packaging and release documentation

### Changed
- Upgraded MCP Go SDK to v1.5.0

### Security
- All credentials redacted from logs by default
- Constant-time comparison for dev token validation (prevents timing attacks)
- TLS certificate verification enabled by default (`insecure_skip_verify: false`)
- HTTP server ReadHeaderTimeout set to prevent Slowloris attacks

## [0.0.1] - 2026-02-15

### Added
- Initial proof-of-concept implementation
- Basic SEMP client
- Simple config loading

---

## Versioning

This project uses [Semantic Versioning](https://semver.org/):
- **MAJOR** version for incompatible API changes
- **MINOR** version for new functionality in a backward-compatible manner
- **PATCH** version for backward-compatible bug fixes

## Release Process

1. Update this CHANGELOG with all changes in `[Unreleased]`
2. Move unreleased changes to a new version section with date
3. Create git tag: `git tag -a v0.2.0 -m "Release v0.2.0"`
4. Push tag: `git push origin v0.2.0`
5. GitHub Actions automatically builds binaries and creates GitHub Release

## Links

- [Unreleased]: https://github.com/SolaceDev/solace-broker-mcp/compare/v0.5.0...HEAD
- [0.5.0]: https://github.com/SolaceDev/solace-broker-mcp/compare/v0.4.0...v0.5.0
- [0.4.0]: https://github.com/SolaceDev/solace-broker-mcp/compare/v0.3.0...v0.4.0
- [0.3.0]: https://github.com/SolaceDev/solace-broker-mcp/compare/v0.2.0...v0.3.0
- [0.2.0]: https://github.com/SolaceDev/solace-broker-mcp/compare/v0.1.0...v0.2.0
- [0.1.0]: https://github.com/SolaceDev/solace-broker-mcp/compare/v0.0.1...v0.1.0
- [0.0.1]: https://github.com/SolaceDev/solace-broker-mcp/releases/tag/v0.0.1
