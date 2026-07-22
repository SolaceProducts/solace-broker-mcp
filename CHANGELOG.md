# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- New MCP tools `list-bridges` and `get-bridge-status` for Bridge monitoring — the same list-then-drill-down pattern as `list-rdps`/`get-rdp-status`. `list-bridges` returns enabled state, inbound/outbound connection state, and last inbound failure reason for every bridge in a VPN (default 100, max 500 via `maxResults`), with a summary block (`downCount`, `disabledCount`, `byInboundFailureReason`). `get-bridge-status` returns additional per-bridge diagnostic detail — connection establisher, failure category, and client name — for one bridge, identified by `msgVpnName` + `bridgeName` + `bridgeVirtualRouter` (bridges are the only object in this server keyed by two names rather than one, since a bridge's config can differ between a broker's primary and backup virtual router in an HA pair). Tracked under SOL-152124.

- New top-level `broker_oauth:` configuration block for upcoming OAuth authentication from the MCP server to brokers. Schema-only in this release — the OAuth runtime is not yet wired, and any broker with `auth.mode: oauth` is rejected at startup with a standalone error banner explaining the limitation and a per-broker validation error in the joined config error. The block holds the global IdP coordinates the MCP server will use to obtain broker-bound tokens: `idp_token_endpoint`, `mcp_server_client_id`, an `mcp_server_client_auth` discriminated union (one named sub-block per IANA "OAuth Token Endpoint Authentication Methods" identifier — V1 supports `client_secret_basic` and `client_secret_post`), `grant_type` (allowlisted to RFC 8693's token-exchange URN), and `audience_parameter_name` (allowlisted to `audience` | `scope` | `resource`). The discriminated-union shape structurally prevents misconfigured method/credential pairings — operators choose a method by populating its named sub-block; the validator enforces "exactly one sub-block populated." Per-broker `auth.mode: oauth` accepts an optional `audience` field whose value is forwarded to the IdP when set. The validator also enforces a permanent structural invariant: if any broker uses `auth.mode: oauth`, `mcp_client_auth.mode` must also be `oauth` (the MCP server cannot obtain a broker token without the agent's token from the client-auth side). Operator-facing banners and errors live in a new `internal/banner` package so future banners land in one canonical home. Tracked under SOL-150796.

- Per-invocation caller identity on tool-invocation log lines (`sub`, `iss`, `client_id`, `jti`) in `oauth` and `static` client-auth modes. Missing optional claims surface as the `<absent>` sentinel so log consumers see a stable schema; `disabled` mode emits no identity fields. Tracked under SOL-149606.
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

- **BREAKING**: A broker configured with `insecure_skip_verify: true` is now refused at startup under `mcp_client_auth.mode: oauth` (production), unless a new top-level `allow_insecure_broker_tls: true` opt-in is also set. Previously this combination started successfully with only a `slog.Warn`, leaving TLS certificate verification disabled while the server still sent the broker admin credential over the connection on every SEMP request — an exploitable man-in-the-middle path. Mirrors the existing `allow_remote_unauthenticated` guard. `allow_insecure_broker_tls` is server-wide, not per-broker: it lifts the check for every broker in the config, not just the one being onboarded. Dev/static modes are unchanged and continue to allow self-signed brokers without the opt-in. Migration: add `allow_insecure_broker_tls: true` to accept the risk, or install a trusted certificate on the broker. Tracked under SOL-151517.

- `ToolManager.CallTool` now takes a trailing `Identity` argument carrying per-invocation audit identity. This is an internal Go API (the package is `internal/tools`); on-the-wire MCP tool schemas and operator-visible config are unchanged. Tracked under SOL-149606.

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

- Config loader now rejects unknown YAML fields at startup instead of silently ignoring them. Previously, a typo like `developmnet_mode` or `insecure_skip_verfy` was accepted and the operator's intended override became a no-op. The loader now fails fast with an error naming the offending field. Existing configs with stale or misspelled keys will fail to start until the typo is corrected; configs with only valid keys are unaffected. Tracked under SOL-149927.

- Replaced the package-level `auth.AddAuth(ctx, req, cfg)` dispatcher with an `auth.Authenticator` interface and per-broker instances. `NewBrokerClient` is the single builder: it constructs one `Authenticator` per broker from `brokerCfg.Auth` and passes the same pointer to both the SEMPv1 and SEMPv2 protocol clients. The clients no longer read `brokerCfg.Auth`; they store the Authenticator on the struct and call `c.authenticator.AddAuth(ctx, req)` per request. No behavior change for existing `basic` and `bearer` auth modes — same Authorization headers, same retry/timeout posture, same config schema. Internal Go API: `sempv1.NewHTTPClient` and `sempv2.NewHTTPClient` signatures gained an `auth.Authenticator` parameter; these are only called from `semp.NewBrokerClient` and tests in the same module. Enables the upcoming OAuth Token Exchange (Hop 2) support without further protocol-client branching. Tracked under SOL-150794 and SOL-150795.

- **BREAKING**: Top-level client-authentication block renamed from `client_auth:` to `mcp_client_auth:`. The Go type renames to `MCPClientAuthConfig`, and the field on `ServerConfig` renames to `MCPClientAuth`. The rename disambiguates the operator-facing schema now that the new `broker_oauth:` block introduces a separate `broker_oauth.mcp_server_client_auth:` nested sub-block — the top-level `client_auth:` name was ambiguous between client authentication on the inbound side (agents authenticating to the MCP server) and on the outbound side (the MCP server authenticating to the IdP). Migration: rename the top-level `client_auth:` key in every config to `mcp_client_auth:`. No field semantics change; only the block name. Tracked under SOL-150796.

- Validator now trims whitespace before checking emptiness for basic-auth `username`/`password` and bearer-auth `token`. Configs whose credentials resolve to a whitespace-only string (e.g., `token: " "` or a `${VAR}` substitution that yields only whitespace) are now rejected at startup with a clear "required for X auth" error rather than passing validation and failing every SEMP request with a 401 at runtime. Tracked under SOL-150796.

### Removed

- Per-broker `brokers.<alias>.auth.scopes` field. The Hop-2 OAuth runtime is gated behind `ENABLE_UNRELEASED_BROKER_OAUTH` and was never activated with the field populated in the wild, so this removes surface rather than user-visible behavior. Rationale: scope entitlement is a property of the user (carried on the subject token), not of the broker — a single static per-broker list either hard-fails less-privileged users (strict IdPs reject `invalid_scope`) or under-scopes admins. The exchange request now omits the RFC 6749 §3.3 `scope` parameter, so the IdP grants its per-client / per-user default scopes. Config decoding is strict (`KnownFields(true)`), so any config still declaring `auth.scopes` fails loudly at startup — this is intentional and covered by a test. Follow-up SOL-151816 will design a per-user scope mechanism; if per-call scopes ever vary for the same subject token, they must also join the token-exchange dedup key (`internal/tokenexchange/dedup_key.go`).

### Fixed

- OIDC token verifier now bounds the HTTP client used by go-oidc for both startup discovery and lazy JWKS refresh (10s per-request timeout). Previously, the verifier fell back to `http.DefaultClient` (zero timeout), and a slow or hung identity provider during key rotation could wedge the JWKS-refresh goroutine indefinitely and stall per-request token verification past the inbound MCP request's own server-side deadlines. The existing 30s discovery deadline is preserved. Operators running an IdP that legitimately takes longer than 10s to serve `/jwks` will see auth fail closed; document the timeout if your environment requires tuning. Tracked under SOL-150219.

- OAuth broker requests now recover from an expired token in-flight. When a broker returns `401 Unauthorized` under `auth.mode: oauth`, the SEMP transport evicts the cached broker token and retries the request once with a freshly exchanged token, instead of failing the call immediately. The recovery decision belongs to the authenticator: `HandleAuthFailure` returns whether to retry and whether the retry must re-authenticate, so static modes (basic, bearer) are unaffected. The re-auth is capped at one attempt per request, so a persistently rejected credential surfaces as a `401` rather than looping. If the token exchange itself fails during the retry (e.g. the IdP is unreachable), the failure is now logged with broker and operation and carries the last observed `401` status. Tracked under SOL-151624.

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

- [Unreleased]: https://github.com/SolaceDev/solace-broker-mcp/compare/v0.1.0...HEAD
- [0.1.0]: https://github.com/SolaceDev/solace-broker-mcp/releases/tag/v0.1.0
- [0.0.1]: https://github.com/SolaceDev/solace-broker-mcp/releases/tag/v0.0.1
