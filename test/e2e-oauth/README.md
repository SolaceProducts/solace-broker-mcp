# E2E OAuth Token-Exchange Suite

End-to-end tests for the OAuth token-exchange path: **Hop 1** (agent → MCP server JWT
validation) and **Hop 2** (MCP server → broker RFC 8693 token exchange), against a real
Keycloak IdP and two real Solace brokers with OAuth profiles configured. Supersedes the
older `test/oauth/` suite, which only covered Hop 1 (`client_credentials` grant, single
broker, no token exchange, no group/audience mapping).

Also covers **claims-based tool RBAC** (SOL-151440) end to end — Keycloak issues a token
with a `groups` claim, the server verifies it, resolves the claim, and `policy.Authorize`
allows or denies the `tools/call`. See [Tool RBAC](#tool-rbac) below; read that section
before changing anything in `test-rbac-scenarios.sh`, because the policy it applies is
chosen for a specific reason that is easy to undo by accident.

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
make e2e-oauth         # build+start the MCP server, run all 15 scenarios
make e2e-oauth-down    # tear down
```

Prerequisites: Docker + Compose, `curl`, `jq`, `openssl`, Go (per `go.mod`). No
`broker-driver`/CGo — every scenario is SEMP/JWT/tool-call only, no messaging.

Any Docker-API-compatible daemon works — nothing in the suite uses Docker-specific
behaviour. Verified against rootful Podman (`podman machine init --rootful`, then
`export DOCKER_HOST=unix://$(podman machine inspect --format '{{.ConnectionInfo.PodmanSocket.Path}}')`);
the brokers' `shm_size: 1g` and `nofile: 1048576` are honoured there. Rootless Podman is
untested and likely caps that `nofile` limit.

> **Changing `realm-export.json`? Run `make e2e-oauth-down` first.** Keycloak imports the
> realm only when its *container is created* (`start-dev --import-realm`, no volume), so
> `make e2e-oauth-up` against a container from an earlier branch silently keeps the old
> realm. The symptom is `mint_token` failing with `invalid_client`, or RBAC scenarios
> denying for no apparent reason. `mint_token` prints a hint when it sees this. CI is
> unaffected — it always starts from a fresh runner.

`run-all.sh` runs three phases against two server configurations, restarting the MCP server
in between. Every phase runs even if an earlier one fails, so one regression does not mask
the others; the run exits non-zero if any phase failed.

| Phase | `tool_authorization` | Scenarios |
|---|---|---|
| 1 | disabled (policy present) | `test-oauth-scenarios.sh` (hop-2 token exchange, 6) |
| 2 | disabled (policy present) | `test-rbac-scenarios.sh disabled` (RBAC no-op, 1) |
| 3 | **enabled** | `test-rbac-scenarios.sh enabled` (tool RBAC, 8) |

Both configurations carry the *same* `access_level_groups`; only the `enabled` flag
differs. A disabled phase running an empty policy could not catch "the flag is ignored but
the policy is still consulted", because there would be nothing to consult.

Each phase begins with a positive control (`assert_server_rbac_mode`) confirming the server
really is in the expected mode. Several assertions turn on the *absence* of audit records,
and absence proves nothing unless you know the scrape works and the phase is pointed at the
right server. The run ends with a per-phase verdict table naming which phase failed.

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

`test-admin-user` additionally holds the realm role **`Ops`**, and there are two Hop-1
clients. Both exist for the tool-RBAC scenarios — see [Tool RBAC](#tool-rbac).

| Hop-1 client | `groups` claim on its tokens | Used by |
|---|---|---|
| `agentic-app-client` | yes (`realm-roles-to-groups` mapper) | every scenario except the missing-claim one |
| `agentic-app-client-nogroups` | **no** (same client, mapper omitted) | the missing-claim deny scenario only |

## Tool RBAC

### The property this suite exists to pin down

There are **two** tokens in this system, and tool RBAC reads only one of them:

| Token | Hop | Consumed by |
|---|---|---|
| Subject token | agent → MCP server | **MCP server tool RBAC** |
| Exchanged token | MCP server → broker | Solace broker access levels |

Both carry a claim named `groups`. Both layers read it. They are nonetheless
**independent authorization decisions with independent vocabularies**, and confusing them
is the single easiest way to write an RBAC test that proves nothing.

Two things settle which token `withAuthorization` sees:

- **Ordering.** `withAuthorization` runs at `tools/call` dispatch. The exchanged token is
  not minted until the tool handler makes its first SEMP call, inside `next(...)`. At
  decision time the exchanged token does not exist. It must therefore be the subject token.
- **Observable.** On a denied call, Keycloak's `TOKEN_EXCHANGE` counter never moves
  (`count_token_exchanges`) — the exchange never happened, yet a decision was made.

Which claims a token carries depends on **which client requested it**: a Keycloak client's
dedicated protocol mappers only fire when that client is the requesting client. The
subject token is requested by `agentic-app-client`; the exchanged token is requested by
`mcp-server-client`. That one rule explains the whole layout — including why subject
tokens originally carried no `groups` claim at all while the brokers worked fine, since
the only realm-role→`groups` mappers lived on `mcp-server-client` and the two
`solace-broker*` clients. The mapper on `agentic-app-client` was added for this reason.

### Why the policy grants on `Ops`

The policy (`TOOL_AUTHZ_RBAC` in `helpers.sh`) grants exactly one tool to one group:

```yaml
access_level_groups:
  Ops:
    - list-vpns
```

`Ops` is a group **the MCP server grants on and no broker maps** —
`configure-oauth-profiles.sh` configures only `solace-admins` and `solace-readonly` as
broker access-level groups, and a scenario asserts `Ops` is absent from both brokers.

To be precise about "MCP-only": `Ops` *does* travel to the broker in the exchanged token,
because `mcp-server-client` carries its own realm-role→`groups` mapper. It is simply
unmapped there, so it grants nothing broker-side. The claim reaches both consumers; only
one of them acts on it.

This is deliberate, and it is the design constraint of the whole suite. If the grant were
on `solace-admins` — a name the broker also honours — a passing test could not distinguish
*"the MCP server authorized this call"* from *"the broker would have allowed it anyway"*.
It would keep passing with the RBAC layer deleted. Granting on a group only the MCP server
recognises isolates the layer under test and demonstrates the two layers are genuinely
independent.

`test-admin-user` holds **both** `solace-admins` and `Ops`, which sharpens it further: the
allow scenario asserts `matched_groups` is exactly `["Ops"]`. If `solace-admins` ever shows
up there, the policy has drifted onto a broker-shared group and the suite has quietly
stopped proving anything.

**If you change the policy, do not grant on a group any broker also honours.**

### Why the assertions read the server log, not the response

A tool-authorization denial is **not** an HTTP 401 and **not** a JSON-RPC error.
Authentication failures are handled well before the tool layer; a denial is an ordinary
`200` carrying a tool-level error result (`isError: true`, `structuredContent.error`).

Worse for testing: the two deny reasons return a **byte-identical** caller-facing message
on purpose (`authzDeniedMessage` / `authzMissingClaimMessage` in
`internal/tools/authorization.go`) so a caller cannot learn whether an admin failed to
grant them or excluded them deliberately. One scenario asserts exactly that
indistinguishability, modulo the per-request `_meta.correlation_id`.

So the deny reason lives only on the server-side audit event, and every decision assertion
goes through `assert_authz_decision` (`helpers.sh`), which reads JSON slog records from
`$MCP_SERVER_LOG`:

| Field | Meaning |
|---|---|
| `decision` | `allowed` / `denied` |
| `decision_reason` | `not_permitted` (had groups, none granted) / `missing_claim` (no groups claim) |
| `expected_claim` | on `missing_claim` only — the claim name the server looked for |
| `matched_groups` | which groups produced the grant. Populated on allow; present but empty (`[]`) on a `not_permitted` deny, and omitted entirely on `missing_claim` |

The caller's own groups are deliberately **absent** from deny records; one scenario asserts
that too. `log_mark` + `authz_records_since` scope each assertion to the records appended
by the call under test. No polling is needed: `withAuthorization` logs before returning and
Go's file writes are unbuffered syscalls, so the record is on disk before the HTTP response
lands — unlike `count_token_exchanges`, which round-trips through the container log driver.

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

### Tool RBAC scenarios (`test-rbac-scenarios.sh`)

| # | Scenario | What it proves |
|---|---|---|
| 1 | Allow names the granting group | `test-admin-user` (holds `solace-admins` **and** `Ops`) calls `list-vpns` → succeeds with real data, and the audit record's `matched_groups` is exactly `["Ops"]`. The grant came from the MCP-only group, not the broker-shared one |
| 2 | Grant is tool-scoped | The same caller, same group, calls a tool `Ops` does **not** grant → denied. Without this, a `Policy.Authorize` that ignored the tool name entirely — "holds any configured group, so allow" — would pass every other scenario here |
| 3 | Broker authorizes independently | `Ops` is absent from both brokers' `accessLevelGroups` (asserted against a *positive control*, so a failed SEMP read can't pass as "absent"), yet scenario 1's call still succeeded — so the broker authorized on `solace-admins`, which the MCP policy never mentions |
| 4 | Deny short-circuits before the broker | A denied call triggers **zero** token exchanges. Otherwise invisible: a gate that returned the denial *and* still ran the handler would produce an identical response and an identical audit record. This is the empirical half of the ordering argument above |
| 5 | Deny: groups present, none permit | `test-readonly-user` calls `list-vpns` → tool-level error (not a 401, not a JSON-RPC error), audit `decision_reason: not_permitted`. Also asserts the caller's own groups never reach the deny record |
| 6 | Deny: no groups claim at all | Same user as scenario 1, but the token is minted via `agentic-app-client-nogroups` → audit `decision_reason: missing_claim` with `expected_claim: groups`. It is the claim set being denied, not the identity |
| 7 | Deny reasons indistinguishable to caller | Scenarios 5 and 6 produce byte-identical caller-visible results (modulo the per-request `_meta.correlation_id`). Both responses are first asserted to *be* denials — equality alone would also be satisfied by two identical successes. Pins the deliberate non-disclosure, and is why 5 and 6 assert on the audit log |
| 8 | Exempt tools bypass the gate | A caller the policy grants nothing can still call **every** structurally exempt tool — `list-brokers` and `describe-semp-schema` — and **no** audit record is emitted for any of them. Both reach exemption the same way: `RegisterListBrokers` and `RegisterDescribeSempSchema` take no policy argument, so the gate is never wrapped around them. The set is cross-checked against the `*ToolName` constants in `internal/tools/`, so a third exempt tool fails this scenario instead of going silently uncovered |
| 9 | RBAC disabled is a no-op (phase 2) | With `enabled: false` but the same grants present, the exact call denied in scenario 5 succeeds, and the gate emits no records at all |

These assertions are load-bearing, not decorative: moving the grant from `Ops` to
`solace-admins` (the mistake the design guards against) makes scenario 1 fail with
`matched_groups=["solace-admins"], want ["Ops"]` and the suite exit non-zero — verified by
mutation before this landed.

Measured wall-clock (warm image cache, local sandbox, 2026-07-15): **~43s** for the full
`make e2e-oauth-all` cycle (certs, bring-up, configure, build, run, teardown) — well under
the 3-minute AC. A cold CI runner will add first-time image-pull time for Keycloak and
Solace on top of that.

## Fixture

One disposable queue, `e2e-oauth-queue`, created on `prod-us` for scenario 4's write-tool
call. Swept on entry and on exit (trap), matching the sibling suites' per-test-ownership
convention.
