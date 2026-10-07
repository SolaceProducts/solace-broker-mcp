# Client Auth Mode — Design Spec

**Jira:** [SOL-149989](https://sol-jira.atlassian.net/browse/SOL-149989) — Consolidate client auth config: replace `development_mode` + `dev_token` with single `client_auth.mode` enum
**Supersedes:** [SOL-149921](https://sol-jira.atlassian.net/browse/SOL-149921) (closed; PR [#50](https://github.com/SolaceDev/solace-broker-mcp/pull/50) closed)
**Epic:** [SOL-149480](https://sol-jira.atlassian.net/browse/SOL-149480) — Broker MCP Server: Early Access Release
**Date:** 2026-05-20
**Author:** Andrea Ross

---

## Problem

The current client auth config has three fields that interact to pick the auth flow: `development_mode`, `client_auth.dev_token`, and the *absence* of the latter. This two-field truth table created the SOL-149921 auth-bypass vulnerability: `development_mode: true` + empty/missing `dev_token` silently disabled all client authentication, with only a WARN log to flag it.

PR #50 addressed the immediate security bug by requiring `dev_token` non-empty in dev mode. Team review surfaced two issues with that approach:

1. The no-auth dev mode was an intentional feature for quick local testing — removing it broke a real use case the team values.
2. The root problem was the entanglement itself, not just one combination of values. Patching the bug without fixing the structure leaves the same shape of footgun in place for the next misconfiguration.

## Design goal

A client auth config that is **simple to read, hard to confuse, and impossible to land in no-auth mode by omission.** Reading any config tells you the auth state from one line.

## Solution

Consolidate to a single required enum field `client_auth.mode` with three explicit values. Required peer fields follow from the mode. The legacy `development_mode` field is deprecated and ignored.

```yaml
client_auth:
  mode: disabled | static | oauth

  # required iff mode == static:
  dev_token: "..."

  # required iff mode == oauth:
  issuer:       "https://idp.example.com"
  audience:     "mcp-server"
  resource_url: "https://mcp.example.com/mcp"
```

**Operational profile is derived from the mode:**

- `mode: disabled` and `mode: static` → dev profile (`http://` allowed for broker URLs, self-signed certs allowed)
- `mode: oauth` → production profile (`https://` required everywhere)

This eliminates the need for a second `development_mode` toggle. One field, three values, no truth tables.

## Allowed combinations

| `mode`     | Operational profile | Required peer fields                          | Use case |
|------------|---------------------|-----------------------------------------------|----------|
| `disabled` | dev                 | —                                             | Quick local dev, no IdP setup |
| `static`   | dev                 | `dev_token`                                   | Local dev with realistic `Authorization` header; e2e tests without an IdP |
| `oauth`    | production          | `issuer`, `audience`, `resource_url`          | Production deployment, OIDC against a real IdP |

The three modes are tiered, not interleaved: `mode: static` and `mode: disabled` are not legal production deployments, and `mode: oauth` requires the full operational rigor (`https://`).

## Runtime behavior per mode

| Aspect | `mode: disabled` | `mode: static` | `mode: oauth` |
|---|---|---|---|
| Startup log | `WARN: client_auth.mode = disabled` banner | `WARN: client_auth.mode = static` banner | `INFO: client auth via OAuth/OIDC` |
| Client must send | Nothing required (any/no `Authorization` header accepted) | `Authorization: Bearer <dev_token>` on every request | `Authorization: Bearer <JWT from IdP>` on every request |
| Server validates | Nothing — passes every request to handler | Constant-time compare against `dev_token` | JWT signature, `iss`, `aud`, `exp`; refreshes JWKS from issuer |
| `/.well-known/oauth-protected-resource` (bare, advertised in `WWW-Authenticate`) and `/.well-known/oauth-protected-resource<resource_url path>` (RFC 9728 §3.1 canonical) | Not served (404) | Not served (404) | Served — same document at both paths; advertises issuer, audience, resource URL |
| Broker URL `http://` allowed | Yes | Yes | No — `https://` required |
| Self-signed TLS allowed | Yes (per-broker `insecure_skip_verify: true`) | Yes | Yes (per-broker; normally false in prod) |
| Token expiration | n/a | 24h synthetic (matches current static verifier) | Whatever the IdP issues (`exp`) |
| Tools available | All MCP tools | All MCP tools | All MCP tools |

## Validation rules (operator-facing errors)

Validator runs as a switch on `client_auth.mode` after lowercase normalization. All errors are collected via `errors.Join` so operators see every problem in one run.

| Config issue | Startup error |
|---|---|
| `client_auth` block omitted | `client_auth.mode is required (must be one of: disabled, static, oauth)` |
| `client_auth.mode` omitted | `client_auth.mode is required (must be one of: disabled, static, oauth)` |
| `client_auth.mode: "<other>"` | `client_auth.mode "<value>" is invalid (must be one of: disabled, static, oauth)` |
| `mode: static` missing/empty `dev_token` | `client_auth.dev_token is required when client_auth.mode is "static"` |
| `mode: oauth` missing `issuer` | `client_auth.issuer is required when client_auth.mode is "oauth"` |
| `mode: oauth` missing `audience` | `client_auth.audience is required when client_auth.mode is "oauth"` |
| `mode: oauth` missing `resource_url` | `client_auth.resource_url is required when client_auth.mode is "oauth"` |
| `mode: oauth` with `http://` issuer | `client_auth.issuer: url scheme must be https to protect credentials in transit (got "http://...")` |
| `mode: oauth` with `http://` broker URL | `broker "<alias>": url scheme must be https to protect credentials in transit (got "http://...")` |
| `mode: static` or `mode: disabled` with `http://` broker URL | Allowed — `http://` is fine in dev profiles |
| Legacy `development_mode: true` present in YAML | `WARN: development_mode is deprecated and ignored; auth profile is now derived from client_auth.mode` (parse continues; ignored value) |
| Mode value with mixed case (`Disabled`, `OAUTH`) | Normalized to lowercase, accepted |

**Stale-field handling:** leftover peer fields under a mode that doesn't require them (e.g., `issuer` present under `mode: static`) are **ignored, not rejected.** Rejecting them would punish operators for keeping old comments or migration scaffolding around. The validator only enforces required fields; extras are inert.

## Runtime signaling — boot banner

The signal that "this server is running in an insecure mode" must be:

1. **Independent of the auth code path** — survives refactors of `NewAuthMiddleware` or future transport changes.
2. **Visually loud** — won't get lost in request-log noise.
3. **Single emission point** — operators see it in one predictable place at boot.

### Implementation

New file: `internal/auth/banner.go` exporting `LogStartupBanner(cfg *config.ServerConfig)`. Called from `cmd/server/main.go` immediately after `config.Load()` returns successfully. The scattered `slog.Warn` calls inside `NewAuthMiddleware` (line 35) and `NewTokenVerifier` (line 66) are removed — the boot banner replaces them.

### Banner content

For `mode: disabled`:

```
============================================================
  INSECURE MODE: client_auth.mode = disabled
  Client authentication is DISABLED.
  All MCP requests pass through without verification.
  This is development mode — NOT FOR PRODUCTION USE.
============================================================
```

For `mode: static`:

```
============================================================
  INSECURE MODE: client_auth.mode = static
  Authentication uses a shared static dev token.
  This is development mode — NOT FOR PRODUCTION USE.
============================================================
```

For `mode: oauth`: a single `slog.Info("client auth: OAuth/OIDC", slog.String("issuer", ...))` line. No banner — production is the unremarkable case.

**Log level: WARN.** Operators deliberately opted into `disabled` or `static` by typing the mode value, so ERROR would dilute the meaning of ERROR elsewhere in the codebase. Log aggregators can pattern-match `INSECURE MODE` for alerting if dev mode appears somewhere it shouldn't.

## Migration map (operator-facing)

| Old config | New config |
|---|---|
| `development_mode: true` + `dev_token: "abc"` | `client_auth: { mode: static, dev_token: "abc" }` |
| `development_mode: true` + missing/empty `dev_token` | `client_auth: { mode: disabled }` |
| `development_mode: false` + OIDC fields | `client_auth: { mode: oauth, issuer, audience, resource_url }` |

**Backwards compatibility hedge:** the `DevelopmentMode` Go struct field is retained for one release with a `// Deprecated:` doc comment, accepting the YAML key so old configs parse without "unknown field" errors. If set in YAML, a deprecation warning logs at boot but the value is otherwise ignored. This ensures operators with old configs hit the helpful `client_auth.mode is required` error instead of a confusing YAML-level rejection.

This is a hard cutover for behavior — there is no period during which old configs work. The hedge exists purely so operators reach the helpful error message.

## Architecture

### File-level responsibilities

| File | Responsibility |
|---|---|
| `internal/config/config.go` | `ClientAuthConfig` struct (now with `Mode` field); mode constants; `IsProductionMode()` derivation helper; `validate()` switch on mode |
| `internal/auth/banner.go` (new) | `LogStartupBanner(cfg)` — pure function emitting the boot-time banner |
| `internal/auth/middleware.go` | `NewAuthMiddleware` switches on `cfg.ClientAuth.Mode`; `NewProtectedResourceMetadataHandler` gates on `mode == "oauth"` |
| `cmd/server/main.go` | Calls `auth.LogStartupBanner(cfg)` immediately after `config.Load()` — single emission point |

### Mode constants (Go)

```go
const (
    AuthModeDisabled = "disabled"
    AuthModeStatic   = "static"
    AuthModeOAuth    = "oauth"
)
```

YAML values match these strings exactly. The constants are referenced from validator, middleware, and banner — no string duplication.

### `IsProductionMode()` helper

```go
// IsProductionMode reports whether the server is configured for production
// (OAuth client auth). This is the single source of truth — do NOT reintroduce
// cfg.DevelopmentMode checks. Operational behaviors (require https://, reject
// self-signed by default) gate on this method.
func (c *ServerConfig) IsProductionMode() bool {
    return c.ClientAuth.Mode == AuthModeOAuth
}
```

All sites currently using `cfg.DevelopmentMode` (broker URL validation, issuer/resource_url validation) migrate to `!cfg.IsProductionMode()`. The `DevelopmentMode` field is no longer read for control flow — only kept for the deprecation warning.

## Code-surface summary

| File | Change |
|---|---|
| `internal/config/config.go` | Add `Mode` to `ClientAuthConfig`; add mode constants; rewrite `validate()` auth section as switch-on-mode; mark `DevelopmentMode` as `// Deprecated:`; add `IsProductionMode()`; replace `!cfg.DevelopmentMode` callers with `cfg.IsProductionMode()` |
| `internal/auth/middleware.go` | `NewAuthMiddleware` switches on `cfg.ClientAuth.Mode`; `NewProtectedResourceMetadataHandler` gates on `mode == AuthModeOAuth`; remove the two `slog.Warn` calls (moved to banner) |
| `internal/auth/banner.go` (new) | `LogStartupBanner(cfg)` |
| `cmd/server/main.go` | Call `auth.LogStartupBanner(cfg)` after config load with don't-move-this comment |
| `internal/config/config_test.go` | Update ~27 fixtures to use `mode: static`; add ~12 new validation tests |
| `internal/auth/middleware_test.go` | Add per-mode middleware behavior tests; remove `Test_AuthDisabled` (defended a behavior we no longer have); add metadata-handler-per-mode tests |
| `internal/auth/banner_test.go` (new) | Capture `slog` output, assert banner content per mode |
| `broker-config.example.yaml` | Show all three modes (commented), default example uses `mode: oauth`; add note about `development_mode` deprecation |
| `test/e2e/broker-config.yaml` | Use `mode: static` + `dev_token` |
| `test/e2e/helpers.sh` | Same `MCP_DEV_TOKEN` wiring PR #50 introduced (already correct) |
| `test/e2e/agent/main.go` | Same `bearerRoundTripper` PR #50 introduced (already correct) |
| `test/e2e/oauth/test-config.yaml` | Leave broken with explicit TODO + follow-up ticket reference; remove the SOL-149921 dead-test note added in PR #50 |
| `CHANGELOG.md` | Operator-visible breaking-change entry with the full migration map |

## Test strategy

TDD throughout. For each new validator rule and each new middleware behavior:

1. Write the failing test first.
2. Run it; confirm it fails for the expected reason.
3. Implement the minimal change.
4. Run; confirm green.
5. Revert-verify: stash the change → test fails; restore → passes. Confirms the test is actually coupled.

### New test inventory

**Validator (config_test.go):**

- `TestLoadConfig_AuthMode_Disabled` — happy path
- `TestLoadConfig_AuthMode_Static` — happy path
- `TestLoadConfig_AuthMode_OAuth` — happy path
- `TestLoadConfig_AuthMode_Missing` — error: mode required
- `TestLoadConfig_AuthMode_Invalid` — error: unknown mode value
- `TestLoadConfig_AuthMode_Static_NoToken` — error: static requires dev_token
- `TestLoadConfig_AuthMode_OAuth_MissingIssuer`
- `TestLoadConfig_AuthMode_OAuth_MissingAudience`
- `TestLoadConfig_AuthMode_OAuth_MissingResourceURL`
- `TestLoadConfig_AuthMode_OAuth_HTTPIssuer` — error: https required for issuer
- `TestLoadConfig_AuthMode_OAuth_HTTPBroker` — error: https required for broker URL
- `TestLoadConfig_AuthMode_Static_HTTPBroker` — allowed: http broker OK in dev profile
- `TestLoadConfig_AuthMode_CaseInsensitive` — `DISABLED`, `OAuth`, etc.
- `TestLoadConfig_DevelopmentModeDeprecationWarning` — old field present → warning log
- `TestLoadConfig_AuthMode_StaleFieldsIgnored` — extra peer fields under wrong mode are inert

**Middleware (middleware_test.go):**

- `Test_NewAuthMiddleware_Disabled` — every request reaches handler
- `Test_NewAuthMiddleware_Static_ValidToken` — passes
- `Test_NewAuthMiddleware_Static_InvalidToken` — 401
- `Test_NewAuthMiddleware_Static_MissingToken` — 401
- `Test_NewAuthMiddleware_OAuth` — constructs OIDC verifier against stub issuer
- `Test_PRMHandler_Disabled` — returns nil
- `Test_PRMHandler_Static` — returns nil
- `Test_PRMHandler_OAuth` — returns handler

**Banner (banner_test.go, new):**

- `Test_StartupBanner_Disabled` — banner emitted at WARN, content includes "client_auth.mode = disabled" and "NOT FOR PRODUCTION USE"
- `Test_StartupBanner_Static` — banner emitted at WARN, content includes "client_auth.mode = static"
- `Test_StartupBanner_OAuth` — single INFO log, no banner

### Existing fixture updates

Mechanical script (same approach PR #50 used) updates ~27 YAML fixture strings: `development_mode: true` → `client_auth: { mode: static, dev_token: test }`. The script skips the new failure-case tests via an explicit allow-list.

### Removed tests

- `Test_AuthDisabled` (already removed in PR #50; same rationale — it asserted `200 OK` for every request when `dev_token` was empty, which is the behavior we deliberately no longer have).

### CI gate

- `go build ./...` clean
- `go vet ./...` clean
- `go test ./...` green
- `/check-logs` clean on the diff (per repo CLAUDE.md)

## Out of scope (follow-ups, tracked in memory)

1. **Investigate lightweight local OAuth server for developer mode** (Mark's preferred direction) — long-term replacement for `mode: disabled` and `mode: static`. Decision/spike ticket; not implementation.
2. **Restore e2e OAuth test path: provision TLS for test Keycloak** — the test at `test/e2e/oauth/test-config.yaml` was already broken on `main` pre-refactor; under the new design `mode: oauth` requires `https://` so the test cannot work until the test Keycloak has TLS.

## Commenting commitments

- Each mode constant gets a doc comment explaining its effect on the auth path.
- The `validate()` mode switch leads with a comment explaining the matrix: why `disabled`/`static` are dev-only, why `oauth` requires `https://` everywhere.
- `IsProductionMode()` gets a doc comment explicitly warning callers not to reintroduce `cfg.DevelopmentMode` checks.
- `LogStartupBanner` gets a leading comment explaining the three reasons it lives in the boot path (single emission, refactor-robust, loud) so the next person tempted to "tidy this up into middleware" understands why it doesn't.
- The deprecated `DevelopmentMode` field gets a `// Deprecated:` Go doc comment pointing at `ClientAuth.Mode`.
- The Jira ticket description (SOL-149989) carries the full design rationale for permanent reference.

## Design decisions captured from team review

- **Naming `oauth` over `oidc`** — matches existing user-facing terminology in `broker-config.example.yaml` ("OAuth client authentication") and the protected-resource metadata endpoint. Internal OIDC library usage stays as-is. [Amit, Balazs]
- **Strict prod auth** — `mode: static` and `mode: disabled` are dev-only; `mode: oauth` is the only legal production mode. The three modes are tiered, not interleaved. [Wajiha, Balazs]
- **Boot banner over inline middleware logs** — refactor-robust, single emission point, visually prominent. [Balazs]
- **Dev mode as antipattern, deferred** — long-term direction is to eliminate dev mode entirely; acceptable as a stopgap per security review ("such development modes are relatively common, so long as it isn't the default and requires 'root' to change"). [Mark, Andrea]
- **Single `mode` field over two interacting fields** — `development_mode` doing double duty (auth selection + operational profile) was the structural cause of SOL-149921. Collapsing to one field and deriving operational profile from it eliminates the truth table. [Amit, Andrea]
