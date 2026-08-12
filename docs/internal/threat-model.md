# Threat Model — Solace Broker MCP Server

This document applies STRIDE to the trust boundaries in
[`architecture.md`](architecture.md). It exists because threat-modeling a
design and recording it is a PRODUCTS-tier requirement on the
[Open Source Solace Software Checklist](https://sol-jira.atlassian.net/wiki/spaces/DC/pages/6616154181/Open+Source+Solace+Software+Checklist),
and this repository is a SolaceProducts repo. SOL-152900.

**Method.** Seven trust boundaries, each analyzed for Spoofing, Tampering,
Repudiation, Information Disclosure, Denial of Service, and Elevation of
Privilege. Every threat below carries either a mitigation with a `file:line`
reference — verified by reading the code, not by trusting an existing doc —
or an explicit **"No mitigation — accepted risk"** statement. Nothing here is
a design proposal; it describes the system as it exists.

**Read live against commit `158af9f` (main, 7 August 2026).** Re-run this
analysis before the SolaceProducts flip and whenever a boundary listed below
changes materially — configuration drifts and this document does not update
itself.

**Consolidated accepted risks** are collected in [§8](#8-consolidated-accepted-risks)
for exception sign-off. Everything else here has a working mitigation.

---

## 1. MCP client → server

**What crosses it:** tool invocations from an LLM, including a
prompt-injected one — an MCP client calling `ToolManager.CallTool`
(`internal/tools/manager.go:110`) via `RegisterWithServer`
(`internal/tools/register.go:132`).

| Threat (STRIDE) | Mitigation | Status |
|---|---|---|
| Malformed/malicious tool params reach a handler (Tampering) | JSON-Schema validation on input (`internal/tools/validation.go:33`) and output (`internal/tools/manager.go:208`) before/after every handler call; panics caught by `withRecovery` (`internal/tools/register.go:62`) | Mitigated — structural only, see below |
| Caller targets an unauthorized broker via `injectBrokerParam` (Elevation of Privilege) | Broker resolution is strict against the configured pool — no fallback to an arbitrary host (`internal/semp/pool.go:105-108`) | Mitigated for *targeting*; not for *authorization* — see §6 |
| Destructive tool invoked without genuine human confirmation (Elevation of Privilege) | `enable_write_tools` (default false) gates registration server-side (`internal/tools/register.go:157-161,196-198`) — a destructive tool literally cannot be called when off | Mitigated for the on/off switch |
| Destructive tool invoked *while write tools are on*, without confirmation | None. Confirmation is prose in the tool description; `manager.go:181-186` only logs a WARNING and proceeds | **No mitigation — accepted risk** |
| SEMP response content used to prompt-inject the calling LLM (Tampering via the LLM) | None. Raw JSON flows verbatim into `StructuredContent`/`TextContent` (`internal/composite/executor.go:763`, `internal/tools/manager.go:208-224`); output validation checks shape, never content | **No mitigation — accepted risk** |
| Chatty/malicious LLM exhausts server or broker resources (DoS) | Per-broker semaphore + rate limiter bound outbound load (`internal/semp/broker.go:75-76`); request body capped (`cmd/server/main.go:219-228`) | Mitigated per-broker |
| One client starves every other legitimate caller of the *same* broker (DoS) | None — no per-client/per-caller rate limit or concurrency cap exists anywhere in the codebase | **No mitigation — accepted risk** |
| Caller identity attribution for audit (Repudiation) | Every call logs `sub`/`iss`/`client_id`/`jti` via `Identity.LogValue` (`internal/tools/identity.go:72-82`), success and failure paths, including panics | Mitigated when auth is enabled |
| No client auth at all (Spoofing/Repudiation collapse) | `mcp_client_auth.mode: disabled` is a supported, operator-chosen server mode (`internal/config/config.go:82`) — no identity is ever recorded in that mode | **No mitigation — accepted risk, operator-chosen** |
| Authz-denied error leaking policy shape to a probing LLM (Info Disclosure) | Deny message is deliberately uninformative and identical whether the tool doesn't exist, the group lacks a grant, or the claim is missing (`internal/tools/authorization.go:36-38`) | Mitigated |

---

## 2. Server → broker (SEMPv2/SEMPv1)

**What crosses it:** broker credentials, write operations against a
production broker, the confirmation gate on write tools.

| Threat (STRIDE) | Mitigation | Status |
|---|---|---|
| Credential leak via logs (Info Disclosure) | `HTTPClient.LogValue()` exposes only `base_url` (`internal/semp/sempv1/client.go:49-53`, `internal/semp/sempv2/client.go:73-77`) | Mitigated |
| Credential leak via broker URL / error text (Info Disclosure) | Userinfo-in-URL rejected at config validation (`internal/config/config.go:1535-1538`); URLs sanitized before logging (`config.go:1549-1568`); agent-facing errors suppress broker text and scrub IPs/paths (`internal/tools/errors.go:359-365`) | Mitigated |
| Destructive write executes with no server-side confirmation gate (Tampering) | None. `manager.go:181-186` logs a WARNING and executes; the only control is prompt text on the tool description | **No mitigation — accepted risk** (same finding as §1) |
| Partial state from a failed multi-step write (Tampering) | None — the composite executor is fail-fast with no compensation (`internal/composite/executor.go:95-135`). Safe today only because every shipped write tool is single-step; a future multi-step write reintroduces this | **No mitigation — accepted risk, currently unreachable** |
| Retry replays a non-idempotent write (Tampering) | Method-based retry policy plus a caller-declared `idempotent:false` escape that suppresses replay except 401 re-auth (`internal/semp/resilience/retry.go:160-256`, `internal/composite/executor.go:111-113`) | Mitigated |
| MITM on the broker connection via disabled TLS verification (Spoofing) | `insecure_skip_verify` requires an explicit `allow_insecure_broker_tls: true` opt-in when the server is in production (oauth) mode (`internal/config/config.go:955-964`); both paths log a visible WARNING | Mitigated in production |
| Same, in non-production (basic/bearer) mode | No opt-in gate required | **No mitigation — accepted risk, dev-scoped** |
| SSRF via a caller-steered broker target or path (Tampering/Elevation) | Broker host is 100% static/config-sourced (`internal/semp/pool.go:87-119`); path template params are escaped and dot-segment-blocked (`internal/semp/sempv2/client.go:203-228`) | Mitigated — no vector found |
| This server overloads the broker (DoS) | Shared per-broker semaphore + rate limiter + capped transient retries (`internal/semp/resilience/semaphore.go`, `ratelimiter.go`, `retry.go:22`) | Mitigated |
| Unbounded goroutine/queue buildup on this server when a caller sets no context deadline (DoS) | None — callers block on `ctx.Done()` only (`internal/semp/resilience/sender.go:200-212`) | **No mitigation — accepted risk** |

---

## 3. OAuth token exchange

**What crosses it:** IdP trust, token caching (`internal/oauth/cache`), the
circuit breaker on that path.

> **Correction to `architecture.md`:** a circuit breaker on the token-exchange
> path is real and on by default (`internal/tokenexchange/circuitbreaker.go`,
> wired in `exchanger.go:96-102`, disabled only by an explicit operator opt-out
> that itself logs a WARNING — `internal/tokenexchange/from_config.go:91-119`).
> `architecture.md` doesn't mention it; it should. Tracked as a follow-up doc
> fix, not blocking this ticket.

| Threat (STRIDE) | Mitigation | Status |
|---|---|---|
| Spoofed/MITM IdP (Spoofing) | No config knob disables TLS verification for the IdP client (unlike the broker client) — cert + hostname verification always on (`internal/idpclient/client.go:69-93`) | Mitigated |
| Malformed/intercepted IdP response (Tampering) | Content-Type, token shape, `token_type`, `issued_token_type`, and `expires_in` bounds all checked (`internal/tokenexchange/response.go:59-166`) | Mitigated |
| IdP outage causes fast failure cascades (DoS) | Circuit breaker (above) + retry chain deadline + a cross-caller 429 gate that short-circuits before the breaker (`internal/tokenexchange/exchange.go:207-220`, `retry_after_gate.go`) | Mitigated |
| IdP outage that's slow rather than fast (each failure burning the full retry-chain deadline) never trips the breaker (DoS) | Resolved under SOL-152286: the consecutive-failure rule now uses an undecayed counter (`newIsBreakerSuccess`/`newReadyToTrip`, `internal/tokenexchange/circuitbreaker.go`) that resets only on an observed success, not on a rolling-window timer, so it trips regardless of failure spacing. Residual: a *partially* degraded IdP (interleaved successes) below `minimum_requests` still can't trip either rule — inherent to the sample floor, not fixed by this change | Mitigated |
| Cross-caller or cross-broker token cache confusion (Info Disclosure/Elevation) | Cache key is `sha256(SubjectToken \|\| 0x00 \|\| BrokerAlias)` (`internal/tokenexchange/dedup_key.go:56-62`) — collision requires the byte-identical inbound JWT, i.e. the same identity | Mitigated by construction |
| Token leak via cache logging (Info Disclosure) | `CachedCredential`/`GetResult` `LogValue()` explicitly omit the token value (`internal/oauth/cache/cache.go:30-40,76-78`) | Mitigated |
| Stale token served after the IdP revokes the inbound token upstream (Info Disclosure/Elevation) | None — invalidation is reactive only, triggered by a broker 401 (`internal/tokenexchange/exchange.go:291-299`), not by any revocation push/poll | **No mitigation — accepted risk** |
| Singleflight dedup hands caller B a token meant for caller A (Elevation of Privilege) | Singleflight key construction is identical to the cache key — collision requires the same bearer JWT | Mitigated by construction |
| Raw inbound JWT leaks via logs or context misuse (Info Disclosure) | Carried in an unexported context-key type (`internal/auth/raw_subject_token.go:47`); never logged; downstream types implement redacting `String()`/`LogValue()` (`internal/tokenexchange/types.go:192-198`) | Mitigated |
| Algorithm-confusion attack at hop-1 verification (`alg: none`, HMAC-as-secret) (Spoofing/Tampering) | `InsecureSkipSignatureCheck` never set; verification against the IdP's asymmetric JWKS only (`internal/auth/middleware.go:144-146`); confirmed against `go-oidc` vendor source | Mitigated |

---

## 4. Configuration and secrets

**What crosses it:** broker credentials at rest and in memory, what
`internal/config` accepts, the rules in
[`secure-logging-rules.md`](secure-logging-rules.md).

| Threat (STRIDE) | Mitigation | Status |
|---|---|---|
| Plaintext secret typed directly into the YAML config, no `${VAR}` indirection (Info Disclosure) | None — `${VAR}` substitution is a documented convention, not enforced by `validate()` (`internal/config/config.go:1240-1297`) | **No mitigation — accepted risk** |
| Config-level credential structs (`AuthConfig`, `BrokerConfig`, and so on) logged raw (Info Disclosure) | `LogValue()` implemented on every credential-bearing config type, pinned by CI unit tests (`internal/config/config.go:429,441,457,272,245,263`; tests at `config_test.go:1360,1388,1419`) | Mitigated |
| `BasicAuthenticator`/`BearerAuthenticator` logged raw (Info Disclosure) | Documented rule (secure-logging-rules.md), not implemented as `LogValuer` on these two types (`internal/semp/auth/basic.go:13-17`, `bearer.go:12-14`) | Partial — backstop only, see next row |
| Same, as a backstop | Global `ReplaceAttr` redacts any attr key matching `password/token/secret/authorization/credential/api_key/private_key` (`cmd/server/main.go:58-90`) | Mitigated as defense-in-depth, not as the documented control |
| A newly-added credential-bearing struct ships with no `LogValuer` and no test (Info Disclosure) | No linter or CI gate enforces the secure-logging rules generally — only `/check-logs`, a manual developer skill | **No mitigation — accepted risk, developer-discipline-dependent** |
| Config validation error echoes a secret value (Info Disclosure) | Field-name-only error text (`config.go:1270,1273,1278`); YAML decode errors sanitized before logging (`config.go:558-571`) | Mitigated |
| Secrets held unzeroed in memory for the process lifetime (Info Disclosure) | None — standard Go string immutability, no wipe/zero mechanism anywhere in the codebase | **No mitigation — accepted risk, standard Go limitation** |

---

## 5. Embedded OpenAPI specs (`describe-semp-schema`)

**What crosses it:** the `describe-semp-schema` tool surface, what it
reveals.

| Threat (STRIDE) | Mitigation | Status |
|---|---|---|
| Full private SEMPv2 schema exposed regardless of what's actually wired as an MCP tool (Info Disclosure) | None — `buildSempSchemaMap` indexes every operation in the embedded config (510 ops) and action (123 ops) specs unconditionally (`internal/tools/describe_semp_schema.go:49-111`), versus ~16 operations actually exposed as tools in `tools.yaml` | **No mitigation — accepted risk** |
| Reveals private-spec detail absent from Solace's public SEMP docs (`x-sensitive`, `x-writeOnly`, undocumented fields) (Info Disclosure) | None — `view=raw` returns the definition verbatim (`describe_semp_schema.go:151-152`) | **No mitigation — accepted risk** |
| Recon of the full write/action catalog by a caller with MCP access but no broker credential, even when `enable_write_tools=false` (Info Disclosure) | Still requires a valid MCP session under whatever `mcp_client_auth.mode` is configured — not fully anonymous | Partial — transport auth only |
| RBAC cannot restrict this tool (Elevation of Privilege) | By design: `describe-semp-schema` is structurally exempt from `tool_authorization` policy, same as `list-brokers` (`internal/tools/authorization.go:40-44,156-196`; `CLAUDE.md:29`) | **No mitigation — accepted risk, by design** |

---

## 6. Multi-broker isolation

**What crosses it:** the `broker` parameter injection, RBAC surface, whether
one broker's config or response data can leak into another's.

| Threat (STRIDE) | Mitigation | Status |
|---|---|---|
| Arbitrary/typo'd broker string reaches an unconfigured target or falls back to a default (Elevation/Info Disclosure) | Hard error, no fallback path exists anywhere in `tools/`, `semp/`, or `config/` (`internal/tools/manager.go:137-149`, `internal/semp/pool.go:105-108`) | Mitigated |
| Broker-not-found error lists every configured alias to any caller, including ones they can't target (Info Disclosure) | None | **No mitigation — accepted risk, minor** |
| Shared mutable state (TCP pool, credentials, rate limiter) bleeds across brokers (Info Disclosure) | Fully isolated per-broker `BrokerClient`, built fresh per alias (`internal/semp/broker.go:66-94`); asserted by a dedicated integration test, `TestCredentialsAreIsolatedPerBroker` (`test/integration/broker_credential_isolation_test.go`) | Mitigated |
| One OAuth broker's IdP throttling/circuit-breaker state degrades every other OAuth broker sharing the process (DoS) | None — the token-exchange circuit breaker and 429 gate are process-wide by explicit design (`internal/tokenexchange/exchanger.go:51-62`); the isolation test file itself flags this as untested pending a future T6 | **No mitigation — accepted risk, documented tradeoff** |
| **Any caller authorized for a tool can target any configured broker with it — no per-broker authorization exists at the MCP layer** (Elevation of Privilege) | None. Verified: `Policy.Authorize(groups, toolName)` (`internal/authz/authz.go:123`) and the underlying `Policy` struct (`authz.go:27-37`) carry no broker dimension at all — authorization is decided before the target broker is even resolved. Per-caller enforcement reaches the broker only when that specific broker runs `auth.mode: oauth` (the broker's own authz on the exchanged token), never as an MCP-layer control | **No mitigation — this is the current model, not a bug. Broker-level tenancy separation between callers sharing one server instance does not exist.** |
| Alias collision or case-mismatch resolves to the wrong broker (Elevation/Info Disclosure) | ASCII-only alias pattern (`internal/config/config.go:766`); startup fails hard on a case-collision, dropping the loser rather than merging (`config.go:793-853`) | Mitigated |
| Cross-request goroutine/context bleed inside the composite executor (Info Disclosure) | Per-call `ExecuteContext`, client passed as an explicit parameter, no shared mutable fields (`internal/composite/executor.go:36-38,95-135`); `broker` param stripped twice before any template evaluates (`internal/tools/manager.go:238-246`, `executor.go:96-103`) | Mitigated |

**This is the single highest-impact finding in this document.** Every other
boundary either has a working mitigation or a narrowly-scoped accepted risk.
This one is structural: the server has no concept of "caller X may only
target broker Y." Anyone who deploys this server for more than one tenant on
shared infrastructure is relying entirely on RBAC being tool-scoped being
"good enough," which it is not once two tenants' brokers are both configured
on the same instance. Worth a named decision — either accept it explicitly
for this repo's deployment model (single tenant per instance) or file it as
follow-on work; it is not something this document can wave through silently.

---

## 7. Supply chain

**What crosses it:** dependency closure, the release pipeline, attestations
(SOL-152543).

| Threat (STRIDE) | Mitigation | Status |
|---|---|---|
| Compromised/typosquatted Go dependency (Tampering) | Committed `go.sum`, default `GOSUMDB` verification, daily Dependabot (`go.mod`, `go.sum`, `.github/dependabot.yml`) | Mitigated |
| Tag-moved GitHub Action (Tampering/Elevation) | Every action across all 8 workflows pinned to a full commit SHA, including internal `SolaceDev/*` actions | Mitigated |
| Unscanned dependency reaches `main` (Elevation) | `Guardian scan gate` is a **live, required** status check on `main` (verified: `gh api .../rulesets/13942241`) | Mitigated |
| Undisclosed/incompatible-license component ships silently (Tampering) | `licenses-check.sh` verifies `THIRD_PARTY_LICENSES.md` against the real `go list -deps ./cmd/server` closure, with a self-test against a vacuous pass (prior incident: SOL-152414) | Check exists and runs — see next row |
| Same, but the check is bypassable | Resolved under SOL-152412. `Third-party licenses current` is now in the required-status-checks list on `main` (`gh api .../rulesets/13942241`), so the check blocks rather than merely reporting | Mitigated |
| Substituted/rebuilt release artifact after the fact (Tampering) | All 4 binary archives + the multi-arch image are attested via `actions/attest-build-provenance` (SOL-152543), verifiable with `gh attestation verify --signer-workflow` (`--repo` alone is insufficient and is called out as such in `RELEASING.md`) | Mitigated |
| Force-retagged release re-triggers the pipeline under a moved tag (Tampering) | None — explicitly `[Planned]` in `RELEASING.md`; tag immutability is not enforced today | **No mitigation — accepted risk, explicitly planned** |
| No cryptographic tie between a tagger and a release tag (Repudiation) | None — signed git tags are explicitly `[Planned]` in `RELEASING.md` | **No mitigation — accepted risk, explicitly planned** |
| Unattributable or forged commit authorship (Repudiation) | `DCO` is a live-required check (`gh api .../rulesets/13942241`), served by the CNCF `dco2` GitHub App. Because it is an App evaluating commits server-side rather than a workflow the pull request supplies, a pull request cannot neuter its own gate | Mitigated for human, non-merge commits |
| Bot-authored or merge commits enter unsigned (Repudiation) | None. `dco2`'s `should_skip_commit` exempts every bot-authored commit (`FromBot`) and every merge commit (`IsMerge`) before reading the message; no config key narrows either. Wider than the retired `dco.yaml`, which exempted one bot by login and recomputed merges. Accepted as the cost of server-side evaluation | **No mitigation — accepted risk** |
| A maintainer clears a failed `DCO` via the App's override button (Repudiation) | `.github/dco.yml` sets `allowOverrideAction: false`, so the `Set DCO to pass` button is not rendered. `dco2` reads that file from the default branch only, so a pull request cannot re-enable it for itself. Not a security boundary — write access implies config access — so it stops an accident, not an intent | Mitigated against accident; residual insider risk accepted |

---

## 8. Consolidated accepted risks

Every row below has **no code-level mitigation** and is carried here for
exception review. Per the checklist: *"An exception with no named approver is
a gap, not an exception."*

| # | Boundary | Risk | Scope |
|---|---|---|---|
| 1 | MCP client → server | Destructive tools execute with no server-side confirmation gate once `enable_write_tools` is on | All write deployments |
| 2 | MCP client → server | SEMP response content flows to the LLM with no content sanitization — indirect prompt-injection surface | All deployments |
| 3 | MCP client → server | No per-client/per-caller rate limit — one caller can starve all others of a shared broker | All deployments |
| 4 | MCP client → server | `mcp_client_auth.mode: disabled` records no identity at all | Operator-chosen, disabled-auth deployments |
| 5 | Server → broker | No compensation/rollback on a failed multi-step write | Currently unreachable (all write tools are single-step) |
| 6 | Server → broker | `insecure_skip_verify` needs no opt-in gate outside production (oauth) mode | Dev/non-oauth deployments |
| 7 | Server → broker | Unbounded goroutine/queue buildup if a caller sets no context deadline | All deployments |
| 8 | ~~OAuth token exchange~~ | ~~A slow/low-traffic IdP outage may never trip the circuit breaker (fails open)~~ Resolved under SOL-152286 | ~~oauth broker mode~~ |
| 9 | OAuth token exchange | Cached hop-2 token isn't revoked when the IdP revokes the inbound token upstream | oauth broker mode |
| 10 | Config and secrets | Nothing prevents a plaintext secret typed directly into the YAML | All deployments |
| 11 | Config and secrets | No CI/lint gate enforces `secure-logging-rules.md` generally; relies on `/check-logs` discipline | All deployments |
| 12 | Config and secrets | Secrets held unzeroed in memory for the process lifetime | All deployments (standard Go limitation) |
| 13 | Embedded OpenAPI specs | `describe-semp-schema` exposes the full private spec (633 ops) regardless of the ~16 actually wired as tools | All deployments |
| 14 | Embedded OpenAPI specs | RBAC cannot restrict `describe-semp-schema` — structurally exempt by design | All deployments with RBAC configured |
| 15 | Multi-broker isolation | Broker alias enumeration on a typo'd broker name | All multi-broker deployments |
| 16 | Multi-broker isolation | One OAuth broker's IdP throttling degrades every other OAuth broker on the same process | Multi-broker oauth deployments |
| 17 | **Multi-broker isolation** | **No per-broker authorization exists — any caller authorized for a tool can target any configured broker.** | **Any deployment with more than one tenant's broker configured on one instance** |
| 18 | Supply chain | `Third-party licenses current` check runs but isn't a required status check, despite documentation saying it should be | Repo-wide |
| 19 | Supply chain | Release tags aren't signed or made immutable | Repo-wide, explicitly `[Planned]` |

**Recommendation to the four-approver group:** #17 is the one item here that
looks like a design gap rather than a scoped tradeoff — it should get an
explicit decision (accept for this repo's single-tenant deployment model, or
open follow-on work), not a blanket sign-off alongside the others. #18 is a
one-line ruleset fix, not a design question, and is probably worth just
fixing rather than accepting.

---

## Follow-ups this document surfaced (not fixed here)

- `architecture.md` doesn't mention the token-exchange circuit breaker
  (`internal/tokenexchange/circuitbreaker.go`) — it's real and on by default.
  Worth a doc update.
- `Third-party licenses current` (accepted risk #18 above) is a ruleset
  configuration gap, not a code gap — likely a five-minute fix once someone
  owns it.
