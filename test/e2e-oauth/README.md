# E2E OAuth Token-Exchange Suite

End-to-end tests for the OAuth token-exchange path: **Hop 1** (agent → MCP server JWT
validation) and **Hop 2** (MCP server → broker RFC 8693 token exchange), against a real
Keycloak IdP and two real Solace brokers with OAuth profiles configured. Supersedes the
older `test/oauth/` suite, which only covered Hop 1 (`client_credentials` grant, single
broker, no token exchange, no group/audience mapping).

Builds on the shared scaffold in [`../e2e-common/`](../e2e-common/README.md) for broker
lifecycle, MCP server process management, assertions, and the test runner. Everything
OAuth-specific (TLS-aware MCP wire calls, the OAuth config writer, token minting,
token-exchange counting, broker OAuth-profile configuration) lives in this suite's own
`helpers.sh` — see the file's header comment for why none of it was added to the shared
`e2e-common/lib.sh` (short version: `write_config` and the static-token MCP wire helpers
are hardcoded to the other 4 suites' Basic/static-bearer shape, and this suite's shape is
different enough — TLS, per-user JWTs, `broker_oauth` — that forking locally beats
genericizing shared code for one consumer).

## Quickstart

```bash
# Full cycle (recommended)
make e2e-oauth-all

# Or step by step
make e2e-oauth-up      # certs, Keycloak+brokers up, OAuth profiles configured
make e2e-oauth         # build+start the MCP server, run the 6 scenarios
make e2e-oauth-down    # tear down
```

Prerequisites: Docker + Compose, `curl`, `jq`, `openssl`, Go (per `go.mod`). No
`broker-driver`/CGo — every scenario is SEMP/JWT/tool-call only, no messaging.

## Ports (distinct from every other e2e suite's `.env`)

| | prod-us (broker-a) | test-us (broker-b) |
|---|---|---|
| Container | `solace-e2e-oauth-a` | `solace-e2e-oauth-b` |
| SEMP port | `8102` | `8104` |
| SEMP-TLS port | `2943` | `2945` |
| Required audience | `solace-broker` | `solace-broker-second` |

Keycloak: `keycloak-e2e-oauth`, HTTPS `18543`, HTTP `18280` (offset +100 from the
`dev/oauth-token-exchange` manual dev stack's `18443`/`18180`, so both can run side by
side on a developer machine). MCP server: `9093`.

## Why SEMP-TLS ports exist and why brokers need a cert installed

OAuth-mode brokers must be addressed over `https://` (the server's config validation
enforces this under `auth.mode: oauth`). The standard Solace broker image listens for
SEMP-over-TLS on container port `1943` by default (`serviceSempTlsEnabled`), but ships
with **no server certificate installed** — `configure-oauth-profiles.sh`'s
`ensure_broker_tls_cert`/`install_broker_tls_cert` generates and PATCHes one in via
`tlsServerCertContent` before the MCP server ever connects. This was found by running the
stack fresh, not documented anywhere in the reference tooling this suite was built from.

## Realm

`realm-export.json` is a copy of `dev/oauth-token-exchange/realm-export.json`'s realm
(from Amit's reference branch `amorade/dev-oauth-manual-e2e` — not merged to `main`), with
two fixes made after a genuinely fresh import surfaced them:

1. An orphaned `roles.client["solace-broker-second"]` entry referenced a client that
   didn't exist in the export at all — import failed outright (`App doesn't exist in
   role definitions: solace-broker-second`). Removed (it was an empty role list, unused).
2. Keycloak's standard token-exchange grant requires its `audience` request parameter to
   resolve to an **actual registered client** — a separate mechanism from the
   custom-audience protocol mapper already on `mcp-server-client`. `solace-broker` had a
   real (bearer-only, resource-server) client backing it; `solace-broker-second` didn't,
   so exchanging for `test-us` failed with `invalid_client` / `"Audience not found"`.
   Fixed by cloning `solace-broker`'s client definition as `solace-broker-second`.

Both gaps only surfaced by actually running a fresh import — worth flagging back upstream
if `dev/oauth-token-exchange/` ever gets its own PR, since the manual flow's long-lived
local Keycloak instance likely never exercised a from-scratch import.

Test users (`test-admin-user`/`test-readonly-user`, password `password`, mapped to realm
roles `solace-admins`/`solace-readonly`) mint Hop-1 JWTs via a direct password grant
against `agentic-app-client` (public, `directAccessGrantsEnabled: true`) — no browser,
no Authorization Code + PKCE flow.

## Cache-hit observability

`count_token_exchanges()` greps the Keycloak container's own log for successful
`type="TOKEN_EXCHANGE"` events. This needed `KC_LOG_LEVEL:
"INFO,org.keycloak.events:DEBUG"` on the Keycloak service — at the default level,
Keycloak logs failed exchanges (`TOKEN_EXCHANGE_ERROR`, WARN) but not successful ones
(DEBUG). Verified live before being relied on for the cache-hit/cache-invalidation
scenario assertions.

## Test catalog

| # | Scenario | What it proves |
|---|---|---|
| 1 | Admin path | `test-admin-user` calls `list-vpns` on `prod-us` → succeeds (Hop 1 + Hop 2 both work for the happy path) |
| 2 | Cache hit | 2 consecutive calls by the same user → exactly 1 new `TOKEN_EXCHANGE` event (the 2nd call reuses the cached Hop-2 token) |
| 3 | Cache invalidation on 401 | Poison `prod-us`'s required audience via SEMP (invalidates the cached token's validity) → restore it → next call succeeds with a fresh exchange. Deliberately doesn't assert the poisoning call's own pass/fail, since whether the auth layer's in-flight retry succeeds silently is an implementation detail |
| 4 | Insufficient permission | `test-readonly-user` calls a write tool (`clear-queue-stats`) → `Authorization failed on broker "prod-us".`, no other detail leaked |
| 5 | Audience isolation | `test-admin-user` calls a tool on `test-us` (audience `solace-broker-second`) → succeeds, proving the per-broker custom-audience mapper selects the right audience |
| 6 | Wrong audience denied | A tool call against a deliberately-misconfigured `test-us-wrong-audience` broker alias (same URL as `test-us`, but configured with `prod-us`'s audience) → broker rejects the exchanged token. Exercises the real Hop-2 exchange code path rather than a raw curl bypassing the MCP server |

Measured wall-clock (warm image cache, local sandbox, 2026-07-15): **~43s** for the full
`make e2e-oauth-all` cycle (certs, bring-up, configure, build, run, teardown) — well under
the 3-minute AC. A cold CI runner will add first-time image-pull time for Keycloak and
Solace on top of that.

## Fixture

One disposable queue, `e2e-oauth-queue`, created on `prod-us` for scenario 4's write-tool
call. Swept on entry and on exit (trap), matching the sibling suites' per-test-ownership
convention.
