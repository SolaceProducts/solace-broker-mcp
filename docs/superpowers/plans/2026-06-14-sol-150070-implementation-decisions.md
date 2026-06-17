# SOL-150070 — Implementation Decisions Log

Status: Living document, updated as sub-tickets land.

This file records decisions made *during implementation* of SOL-150070 (OAuth Token Exchange Hop 2). It complements — and does not replace — the upstream architecture plan at [`docs/oauth/token-exchange-SOL-150070/architecture-plan.md`](../../oauth/token-exchange-SOL-150070/architecture-plan.md).

**Scope of this doc:** decisions that surface during coding and are too granular for the architecture plan but too significant to leave only in a commit message or a PR thread. Examples:

- Construction-site choices ("does X get built in A or B?")
- Test-design decisions that establish a precedent (e.g. how concurrent multi-component scenarios are tested)
- Trade-offs taken inside a single sub-ticket that future sub-tickets will inherit
- Deviations from the original decomposition, with reasoning

**Format:** one section per decision. Each section names the sub-ticket that raised the decision, the choice taken, and the *reason* (not just the outcome). The reason is what makes the entry useful in three months.

---

## T1b — Authenticator is constructed inside `NewBrokerClient`, not inside the pool

**Date:** 2026-06-14
**Sub-ticket:** [SOL-150795](https://sol-jira.atlassian.net/browse/SOL-150795)
**Architecture plan refs:** Decision 7 + addenda (Authenticator on `BrokerClient`, borrowed by protocol clients).

### The question

When a broker is first requested, *which layer* builds its `Authenticator`?

Two candidate sites:

1. **`pool.getOrCreate`** — the pool builds the Authenticator and passes it to `NewBrokerClient`.
2. **`semp.NewBrokerClient`** — the BrokerClient builds its own Authenticator from `brokerCfg.Auth` as part of its construction.

The architecture plan's wording is ambiguous on this — it talks about the Authenticator being "born in `pool.getOrCreate`," but it also says the Authenticator is "a field on `BrokerClient`." The Jira ticket says "Modify `semp.NewBrokerClient` … construct one Authenticator per broker."

### Decision

**The Authenticator is constructed inside `NewBrokerClient`**, alongside the other fields it composes (sempv1 client, sempv2 client). The pool calls `NewBrokerClient(cfg, ...)` and stores whatever comes back; it does not know how the Authenticator is built or what dependencies it needs.

### Why

Three reasons, in order of weight:

1. **Single-responsibility per layer.** The pool's job is registry/lifecycle ("is this broker's client built yet? if not, build one; hand it back"). The BrokerClient's job is "I represent one broker — I own everything that talks to that broker." The Authenticator is owned by the BrokerClient, so its *construction* is the BrokerClient's concern, not the pool's.

2. **Avoids leaking auth knowledge into the pool.** If the pool builds the Authenticator, the pool has to know which auth modes exist, what each one needs, and how to wire each one's dependencies. Today that's just `cfg.Auth`. T6 adds OAuth, which requires the pool to also hold a reference to the global `*Exchanger` and a per-broker cookie jar. The pool would accumulate auth-domain knowledge for no benefit. Keeping construction inside `NewBrokerClient` localizes that knowledge to one layer.

3. **Testability outside the pool.** Anyone who wants to construct a `BrokerClient` in a test — without spinning up a full pool — should not have to manually build an Authenticator first. The BrokerClient owns its own assembly. The pool is an optimization (caching/dedup), not a precondition for getting a working BrokerClient.

### Consequences

- `pool.getOrCreate` does **not** need an `auth.NewAuthenticator` call. It just resolves the broker config and calls `NewBrokerClient(cfg, sempCfg)` as before.
- `NewBrokerClient` calls `auth.NewAuthenticator(brokerCfg.Auth)` once, stores the result as a field, and passes the same pointer to both `sempv1.NewHTTPClient` and `sempv2.NewHTTPClient`.
- When T6 lands and OAuth becomes a real mode, `NewBrokerClient`'s signature will grow to accept the global `*Exchanger` (and per-broker cookie jar). The pool will pass those in. The pool still doesn't *build* the Authenticator; it just hands over the runtime dependencies the BrokerClient needs to do so.

### What this means for future sub-tickets

- **T6:** when extending `NewAuthenticator` to accept `*Exchanger` + jar, the call inside `NewBrokerClient` is the single update site for the dispatcher's new arguments. No pool changes.
- **T7a:** wires the Exchanger into `main` and passes it through the pool to `NewBrokerClient`. The pool becomes a courier for the Exchanger pointer, not its builder or its consumer.

---

## T1b — Protocol-client constructors take an `Authenticator`; nothing below `NewBrokerClient` builds one

**Date:** 2026-06-14
**Sub-ticket:** [SOL-150795](https://sol-jira.atlassian.net/browse/SOL-150795)
**Architecture plan refs:** Decision 7 addendum ("one Authenticator per broker, shared by SEMPv1 + SEMPv2 protocol clients via pointer").

### The question

After locking in that `NewBrokerClient` is the single builder of Authenticators (see prior entry), two cascading questions surfaced during the T1b refactor:

1. Does `sempv1.NewHTTPClient` / `sempv2.NewHTTPClient` accept an already-built `Authenticator`, or does it accept `brokerCfg` and build its own?
2. What signature should the test helper `newTestClientWith` use — `config.AuthConfig` (and build the Authenticator inside) or `auth.Authenticator` (caller builds)?

These look like two questions; they're really one.

### Decision

**Protocol-client constructors take an `auth.Authenticator` parameter.** They no longer read `brokerCfg.Auth`. They store the pointer the caller hands in.

**Test helpers take an `auth.Authenticator`** for the same reason. Tests construct one at the call site (with a thin per-test convenience function for the common case).

The signatures become:

```go
func NewHTTPClient(brokerCfg *config.BrokerConfig, sempCfg *config.SEMPConfig,
                   sem resilience.Semaphore, authn auth.Authenticator) (*HTTPClient, error)

func newTestClientWith(t *testing.T, srv *httptest.Server, authn auth.Authenticator) *HTTPClient
```

### Why

The decision is forced by the prior entry ("`NewBrokerClient` is the single builder"), not chosen independently:

1. **"One builder" propagates downward.** If the protocol clients built their own Authenticator from `brokerCfg.Auth`, they would be a *second* builder. Worse, in production each broker would end up with three Authenticator instances — one in `NewBrokerClient`, one inside the v1 client, one inside the v2 client — all reading the same config and behaving identically. That contradicts Decision 7's addendum, which says one Authenticator per broker is shared by both protocol clients *via pointer*.

2. **Same logic at the test layer.** A test helper that takes `AuthConfig` and calls `auth.NewAuthenticator` internally is a second builder for the test layer. It would have to track future signature changes to `NewAuthenticator` (T6 adds `*Exchanger` + jar parameters) in lock-step with `NewBrokerClient`. Taking an already-built Authenticator keeps the helper inert — it just passes through whatever the test constructs.

3. **Honest tests.** A protocol-client unit test's job is to exercise the protocol client's contract: "given these dependencies, do X correctly." Hiding the Authenticator construction behind a helper that takes `AuthConfig` blurs the contract — a reader can't tell at a glance what the protocol client needs. Explicit construction at the call site makes the dependency visible.

### Consequences

- **Production**: `NewBrokerClient` calls `auth.NewAuthenticator(brokerCfg.Auth)` once, stores the `*Authenticator` as a field on `BrokerClient`, and passes the same pointer to both `sempv1.NewHTTPClient` and `sempv2.NewHTTPClient`. One instance per broker, shared.
- **Protocol clients**: drop the `authCfg config.AuthConfig` field; add an `authenticator auth.Authenticator` field. Replace `auth.AddAuth(ctx, req, c.authCfg)` with `c.authenticator.AddAuth(ctx, req)`. They no longer reach into `brokerCfg.Auth` for any reason.
- **Tests**: `newTestClientWith` takes `auth.Authenticator`. Each call site builds an Authenticator inline (typically `auth.NewBasicAuthenticator("user", "pass")` — one line). A small per-package convenience helper (`basicAuthn(t, user, pass)`) is fine if it reduces ceremony at three or more call sites; otherwise inline construction is clearest.
- **Earlier lean rejected**: a previous reading suggested keeping `newTestClientWith`'s signature as `AuthConfig` to minimize test churn. That lean was wrong — it framed the test helper as an independent choice, but the construction-site decision forces it. Recording the rejection here so a future maintainer doesn't re-litigate it.

### What this means for future sub-tickets

- **T6:** the `NewAuthenticator` signature grows (`*Exchanger`, jar). The only production call site that changes is `NewBrokerClient`. Test call sites change individually as tests opt into OAuth — they pass an `OAuthAuthenticator` directly, no migration shim needed.
- **T7b:** the Sender's `authCfg` field is removed and replaced with an `auth.Authenticator` pointer borrowed from the BrokerClient. That refactor follows the same "no new builders" principle — the Sender does not build, it receives.

---

## T1b — Panic on nil Authenticator in protocol-client constructors; return error from `NewBrokerClient`

**Date:** 2026-06-14
**Sub-ticket:** [SOL-150795](https://sol-jira.atlassian.net/browse/SOL-150795)

### The question

After the prior entries, `sempv1.NewHTTPClient` and `sempv2.NewHTTPClient` both take an `auth.Authenticator` parameter. The constructor must do *something* if a caller hands it nil. Two questions arose during implementation:

1. Should the protocol-client constructors **panic** or **return an error** when `authn == nil`?
2. Should `NewBrokerClient` (the layer above) also panic, since its failures are arguably the same category?

### Decision

**`NewHTTPClient` panics on nil `Authenticator`. `NewBrokerClient` returns an error on its failure modes.** The two layers are categorically different and should not be unified.

### Why

The rule applied: **programmer-error preconditions panic; runtime failures return an error.** "Programmer error" means there is no realistic input or environment condition that produces the failure — only broken code can.

Applying that rule to the two layers:

**`NewHTTPClient`'s `authn == nil` is a programmer error.** Trace every path that reaches it:

- Production path: `NewBrokerClient` calls `auth.NewAuthenticator(brokerCfg.Auth)`. Every success path of `NewAuthenticator` returns a non-nil `Authenticator`; nil is returned only with a non-nil error. The only way nil reaches `NewHTTPClient` is if a caller ignored the error (e.g. `authn, _ := auth.NewAuthenticator(...)`). That's a bug in calling code.
- Test path: a test would have to type `NewHTTPClient(..., nil)` literally. Also a bug.
- No environment or input condition produces this. There is no `nil-authenticator.yaml` to misconfigure.

So `NewHTTPClient`'s nil-check guards against bugs, not runtime conditions. The right response is a panic with a clear message at construction time, not a returned error that callers might silently propagate up the stack.

**`NewBrokerClient`'s failure modes are runtime conditions.** Trace them:

- `auth.NewAuthenticator(brokerCfg.Auth)` returns an error when `brokerCfg.Auth.Mode` is an unrecognized string. That comes from `broker-config.yaml` — user-controlled input. A typo (`bsic` instead of `basic`) triggers it. Not a programmer error.
- `sempv1.NewHTTPClient` and `sempv2.NewHTTPClient` can fail due to cookie jar creation (OS state) or other genuine runtime failures.

Mixing these into a panic would be hostile to operators: a typo in YAML would crash the server. Worse, `NewBrokerClient` runs lazily on first request via `pool.getOrCreate` — so a single misconfigured broker would take down the whole process the first time anyone touched it, rather than producing a recoverable per-broker error.

Returning an error preserves the pool's existing "isolated failure per broker" semantics. The rest of the server keeps serving requests for healthy brokers.

### Defensive-coding caveat

The nil-check in `NewHTTPClient` is defensive — by construction (per prior decision), `NewBrokerClient` always passes a non-nil Authenticator in production. The check is kept because:

1. The Authenticator abstraction is new (T1a). The check documents the precondition for contributors who'll add new code paths during T6 and beyond.
2. Pulling the failure forward in time matters. Without the check, a nil Authenticator surfaces as `nil pointer dereference` inside `AddAuth` at request time — potentially thousands of requests deep, with a stack trace that points to the AddAuth call site rather than the construction bug. The check makes the failure happen where it is fixable.

After the abstraction is fully cooked (post-T6, post-T7b), the check may be a reasonable cleanup candidate. Not blocking; file as a `/should-i-ticket` follow-up if it ever feels like overhead.

### Why we check `authn` but not the other parameters

`NewHTTPClient` takes four parameters (`brokerCfg`, `sempCfg`, `sem`, `authn`). Only `authn` gets a nil-check. The principle isn't "panic on every nil-able parameter" — that path leads to ceremony on every public function and unprincipled inconsistency when parameters are added or removed. The principle is narrower:

> Panic when a nil value would be **stored and used later**, so the natural failure happens far from the cause. Trust the type system when a nil value would be **dereferenced immediately**, because the natural failure is already loud and well-located.

`brokerCfg` and `sempCfg` are read within the constructor body itself — a nil value crashes immediately at the line that touches it, with a stack trace pointing exactly at the constructor. Checking them adds noise without improving diagnosis. `sem` is handed to `resilience.New`, which already panics with its own message at that boundary — the check exists, just at the right layer.

`authn` is the only parameter that gets stored on the struct without being dereferenced at construction. A nil value would survive construction and surface as `nil pointer dereference` inside `AddAuth` on the first actual SEMP request — potentially thousands of requests deep, with a stack trace pointing at the call site rather than the construction bug. That's the failure mode the check exists to prevent.

This narrower rule keeps the model honest: the check exists because the failure would otherwise be deferred and confusing, not because every nil-able parameter deserves one.

### General principle (worth keeping in mind for the rest of SOL-150070)

When designing a constructor that could fail, ask: **among the realistic ways this can fail, are any caused by something other than broken code?**

- If yes → return error.
- If no → panic.

Mixing programmer-error preconditions and runtime failures under one `error` return obscures which is which. Callers end up writing the same `if err != nil` block for "cookie jar failed" (recoverable, log and continue at a higher layer) and "you passed nil" (your code is wrong, no recovery possible). Separating the two keeps the error surface honest.

Local precedent: `resilience.New` already panics on a nil `Semaphore` for exactly the same reason. T1b is consistent with that pattern.

---

## T1b — In-process integration tests live under `test/integration/`, one file per invariant

**Date:** 2026-06-15
**Sub-ticket:** [SOL-150795](https://sol-jira.atlassian.net/browse/SOL-150795)

### The question

The new multi-broker concurrent test composes seven internal components: `BrokerPool`, `BrokerClient`, `auth.NewAuthenticator`, `*BasicAuthenticator`, `*BearerAuthenticator`, `sempv1.HTTPClient`, `resilience.Sender`. That is qualitatively different from the surrounding tests in `internal/semp/pool_test.go`, which exercise one component each. Where should such cross-component tests live, and how should they be named, given that T6, T5, and T7a will add several more?

### Decision

A new directory `test/integration/` holds in-process Go integration tests. Each file owns **one qualitatively distinct invariant**, named after the invariant rather than the components composed or the test setup.

The first file is `test/integration/broker_credential_isolation_test.go`, holding `TestCredentialsAreIsolatedPerBroker` (the multi-broker concurrent test originally landed inside `pool_test.go`).

Future tickets add sibling files under the same directory: `oauth_user_isolation_test.go` (T5/T6), `oauth_broker_isolation_test.go` (T6/T7a), `oauth_token_lifecycle_test.go` (T6/T7a). The full rule and rationale lives in [`test/integration/README.md`](../../../test/integration/README.md).

### Why a separate directory and not a naming convention in the same package

Earlier in T1b a weaker proposal (Option 2+3 from the design discussion) suggested keeping the integration tests inside `internal/semp/` with a `*_integration_test.go` filename and `TestIntegration_` prefix. That was rejected for three reasons:

1. **Discipline rots.** A naming convention is enforced only by reviewer attention. The next contributor adding an integration test will not necessarily know the rule; a unit-test file in the same directory is a tempting place to drop "one quick cross-component check."
2. **Directory structure enforces what conventions only suggest.** A separate `test/integration/` directory makes the layer obvious at a directory listing and impossible to accidentally violate without crossing a directory boundary that is itself a review signal.
3. **The project already segregates non-unit tests under `test/`.** `test/e2e-basic-mcp/`, `test/oauth/`, and `test/e2e-monitoring/` all exist. Adding `test/integration/` as a sibling places the new tier exactly where the project has already decided non-unit testing infrastructure lives.

The cost of the separate package (no access to unexported identifiers) is a benefit here — true integration tests should assert through the public API, not implementation details. The test in question already does this.

### Why one file per invariant, not per feature or per component

A file named after components (`pool_auth_test.go`) attracts every test that touches those components, regardless of what it asserts. The result is a grab-bag file with mixed scope that no one wants to read.

A file named after a feature (`oauth_test.go`) attracts every OAuth-related integration scenario. Two qualitatively different invariants — *user isolation* (different users on the same broker) and *broker isolation* (different brokers using OAuth) — share the file even though they test different machinery and require different setups.

A file named after an invariant (`broker_credential_isolation_test.go`) has sharp scope. When a new isolation property appears, it gets a new file. The folder, not the file, holds the breadth.

### Why static-credential isolation and OAuth isolation are different invariants

The T1b test covers basic and bearer modes only. OAuth was deferred to its own file rather than added as a third mode here. The reason: OAuth's isolation question is qualitatively harder.

- For basic/bearer, every `*BasicAuthenticator` and `*BearerAuthenticator` instance is self-contained. There is no shared state between brokers' Authenticators. "Broker isolation" is structurally true; the only failure mode is the pool routing the wrong client to a request, which is a routing bug. The current test proves this with a small, focused setup.
- For OAuth, every `*OAuthAuthenticator` instance shares the same global `*Exchanger`, the same `TokenCache` (partitioned by cache key), the same singleflight group, and a per-broker cookie jar. Broker isolation depends on cache-key construction, singleflight-key construction, and cookie-jar separation. None of that machinery exists for basic/bearer.

Forcing both into one file would either (a) blur two different invariants under one test name, or (b) make T6 rewrite the file's setup to accommodate a fake IdP, token-exchange flow, and per-user request fanning. Either is worse than separate files.

### Not in this ticket: build-tag tier separation

`test/integration/` runs as part of `go test ./...` today. There is no `//go:build integration` tag and no `make integration` target. The directory itself is the marker; the build-tag tier is a future cleanup.

The upgrade path is one-way and cheap: when the runtime cost of integration tests starts mattering in CI, the files already live in the right directory and only need the build tag and a make target. No structural changes required.

This is captured as future-work in `test/integration/README.md`. A follow-up ticket should land it when the suite is large enough to justify the friction — order-of-magnitude five tests, not one.

### What this means for future sub-tickets

- **T5, T6, T7a:** integration tests for OAuth machinery go in `test/integration/`, in new files named after the invariant being tested. Do not extend `broker_credential_isolation_test.go`.
- **T1b PR review precedent:** when a reviewer asks "is this a unit test or an integration test?", the answer is in the file path. `internal/<pkg>/*_test.go` → unit. `test/integration/*_test.go` → integration. `test/e2e-*/` → E2E.
- **No build tag yet.** Anyone tempted to add `//go:build integration` should file a follow-up ticket instead — there is more rigor to the move than a single line, and we should not pay that cost piecemeal.

---

## T2 — Hop 2 OAuth config schema requires explicit `idp_token_url`; OIDC Discovery deferred

**Date:** 2026-06-16
**Sub-ticket:** [SOL-150796](https://sol-jira.atlassian.net/browse/SOL-150796)
**Architecture plan refs:** Decision 1 (one IdP per deployment), Decision 9 (no live IdP probing at startup).

### The question

`BrokerOAuthConfig` has three universal fields: `idp_token_url`, `mcp_server_client_id`, `client_secret`. Should `idp_token_url` be required?

Two real operator personas pull in opposite directions:

- **Modern-IdP operator** — uses Keycloak / Okta / Auth0 / Entra. Their IdP exposes an OIDC Discovery endpoint at `<issuer>/.well-known/openid-configuration`. They'd prefer to configure `issuer_url` once and let the MCP server fetch `idp_token_url` (and any other endpoint coordinates) from discovery.
- **Legacy / non-OIDC operator** — uses an older or custom OAuth server with no discovery endpoint. They must configure `idp_token_url` explicitly.

The natural "flexible" answer is: accept either `issuer_url` OR `idp_token_url`, with a validator rule that exactly one must be set. This is the same pattern as the `client_auth_method` discriminator we settled on earlier in T2.

### Decision

**T2's schema requires `idp_token_url` explicitly.** `issuer_url` is not added in this ticket. OIDC Discovery support — and the accompanying `issuer_url` field — is deferred to its own follow-up ticket, post-T2.

### Why

Three reasons, in order of weight:

1. **Discovery is runtime work, and T2 is schema-only.** The ticket scope is explicit: schema and validation, no runtime behavior change. Adding `issuer_url` to the schema without a runtime that supports it would mean either rejecting the field in the validator (operator-confusing — they see the field in the example YAML and can't use it) or accepting it and silently failing later. Neither is honest. Discovery belongs in the ticket where its runtime lands.

2. **Discovery is non-trivial runtime work that conflicts with Decision 9.** Implementing OIDC Discovery means deciding: when to fetch (lazy vs. eager), whether to cache (and how to honor cache headers), how to handle the discovery endpoint being temporarily unavailable, what to do if the document is malformed, and how this interacts with the architecture plan's Decision 9 commitment of "no live IdP probing at startup." Lazy-fetch on first request is the most natural fit for Decision 9, but it adds a first-request latency tax and a new failure mode for the cold path. These are real design questions that deserve their own ticket and design conversation.

3. **The deferral is non-breaking.** Adding `issuer_url` later does not require any operator who configured `idp_token_url` in V1 to migrate. The follow-up ticket extends the schema additively: `issuer_url` becomes an optional field; validator rule grows to "exactly one of `issuer_url` or `idp_token_url` must be set"; runtime grows discovery logic. Existing configs using `idp_token_url` keep working untouched.

### What the validator does today

- `idp_token_url` is required (non-empty, valid URL).
- No `issuer_url` field exists in the schema yet.

### What the deferred follow-up ticket will add

- A new optional `issuer_url` field on `BrokerOAuthConfig`.
- Validator rule: exactly one of `issuer_url` or `idp_token_url` must be set; both is an error; neither is an error.
- Runtime: OIDC Discovery client that fetches `<issuer_url>/.well-known/openid-configuration`, extracts `token_endpoint`, and uses it in place of `idp_token_url`. Caching, error handling, and fetch timing decided in that ticket.
- Operators on modern IdPs gain the convenience of single-field IdP configuration. Operators on legacy IdPs are unaffected.

### What this means for the V1 schema commitment

`idp_token_url` is part of the V1 schema and stays required forever. Operators who configure it in V1 will never be forced to migrate to `issuer_url`. The discovery-based path is an *additional* way to configure the same coordinate, not a replacement.

### Honest caveat

This is the third place in T2 where we've leaned on the "schema is forward-compatible, validator is strict about V1's implementation" pattern (the others being `client_auth_method` and `grant_type`). For T2's narrower scope of `idp_token_url` vs. `issuer_url`, we made the opposite call — keep the schema narrow now, extend later — because:

- `client_auth_method` and `grant_type` are discriminators with a single field already present (the method/type string itself). The schema cost is one string field plus an allowlist. Cheap.
- `issuer_url` vs. `idp_token_url` is a *capability*, not a discriminator. Adding `issuer_url` to the schema requires either (a) accepting it and silently failing at runtime, or (b) rejecting it in the validator with an "unsupported" error. Both are worse than not having the field.

The line between "discriminator pattern (cheap, schema-flexible)" and "feature placeholder (confusing, schema-flexible)" is the runtime cost of supporting the alternative value. Discriminators where the runtime treats both values nearly identically (e.g., `client_secret_basic` vs. `client_secret_post` differ only in transport) are cheap to keep in schema. Discriminators that gate substantially different runtime paths (Discovery vs. explicit endpoint) are not.

---

## T2 — `client_auth_method` discriminator: schema-flexible, validator-strict

**Date:** 2026-06-16
**Sub-ticket:** [SOL-150796](https://sol-jira.atlassian.net/browse/SOL-150796)

### The question

How does the MCP server authenticate itself to the IdP when calling the token endpoint? RFC 6749 §2.3 + RFC 7523 + RFC 8705 + OIDC Core §9 collectively define several standard methods: `client_secret_basic`, `client_secret_post`, `client_secret_jwt`, `private_key_jwt`, `tls_client_auth`, `self_signed_tls_client_auth`, `none`. Different IdPs require different methods. FAPI-tier deployments (banks, regulated enterprises) typically *forbid* the `client_secret_*` methods.

### Decision

Add a `client_auth_method` discriminator field. V1 supports two values: `client_secret_basic` (default) and `client_secret_post`. Other registered method strings are not implemented in V1 — the validator rejects them with a clear "not yet supported in this version" error. A separate follow-up ticket will add `private_key_jwt` and `tls_client_auth`.

### Why

1. **Operators must be free to match their IdP's requirements.** Keycloak prefers `client_secret_basic`; some older Okta configs require `client_secret_post`. We do not pick for them and we do not restrict them — within the methods we support.

2. **The discriminator is forward-compatible.** When the follow-up ticket lands, it expands `validClientAuthMethods` and adds the corresponding runtime; existing operator configs using `client_secret_basic` keep working untouched.

3. **The default is RFC 6749 §2.3.1's preferred method.** Operators who omit the field get the safer-on-the-wire transport (Basic header vs. form body). Operators who know their IdP override the default.

### What the schema does today

- `client_auth_method` is an optional string in `BrokerOAuthConfig`.
- Empty → defaults to `client_secret_basic`.
- Value in `validClientAuthMethods` (`{client_secret_basic, client_secret_post}`) → accepted.
- Any other value → rejected at startup with a clear error.

### What the deferred follow-up will add

- Expand `validClientAuthMethods` to include `private_key_jwt`, `tls_client_auth`, and (likely) `self_signed_tls_client_auth`.
- Add the per-method conditional fields needed by each (e.g., `private_key_file`, `key_id`, `client_cert_file`, `client_key_file`).
- Add the runtime that builds the right token-exchange request body and parses the response per method.
- Schema shape (flat vs. nested per-method sub-blocks) is the follow-up's design call; nothing is locked in by T2.

### Per-method required-field validation (contract for follow-up tickets)

The validator enforces required fields **per `client_auth_method`**, not unconditionally. The `client_secret` check is inside a `switch cfg.BrokerOAuth.ClientAuthMethod { ... }` whose `client_secret_basic` and `client_secret_post` case requires the secret. Other methods get other cases when they land.

This structure is the contract for follow-up tickets adding new methods:

> **Adding a method to `validClientAuthMethods` MUST come with a new case in the per-method switch in `validateBrokerOAuthConfig` that declares the new method's required fields.** Existing cases stay untouched. The switch is intentionally exhaustive in its dispatch to make this hard to forget — if a method is added to the allowlist without a corresponding case, the validator silently accepts configs that should fail.

Why the per-method switch rather than per-field if-statements:

- **Co-location**: each method's required-field rules live next to its name. A reader scrolling the switch sees "method X needs Y" in one place per method.
- **Additive extension**: adding a new method is one new case, full stop. No edits to existing cases. No risk of accidentally tightening or loosening V1's rules.
- **Hard to forget the gate**: an if-statement style ("if method == X or Y, check Z") scatters per-method rules across the function and makes "did I gate this check?" a real review burden every time someone adds a method. The switch makes the gating structural.

The V1 case is currently:

```go
switch cfg.BrokerOAuth.ClientAuthMethod {
case ClientAuthMethodSecretBasic, ClientAuthMethodSecretPost:
    if cfg.BrokerOAuth.ClientSecret == "" { ... }
}
```

Future follow-up cases will look like:

```go
case ClientAuthMethodPrivateKeyJWT:
    if cfg.BrokerOAuth.PrivateKeyFile == "" { ... }
    if cfg.BrokerOAuth.KeyID == "" { ... }
case ClientAuthMethodTLSClientAuth:
    if cfg.BrokerOAuth.ClientCertFile == "" { ... }
    if cfg.BrokerOAuth.ClientKeyFile == "" { ... }
```

Universal required fields (`idp_token_url`, `mcp_server_client_id`, and the three discriminator allowlist checks) stay outside the switch — they apply to every method.

---

## T2 — `grant_type` discriminator: schema-flexible, V1 supports RFC 8693 only

**Date:** 2026-06-16
**Sub-ticket:** [SOL-150796](https://sol-jira.atlassian.net/browse/SOL-150796)

### The question

Different IdPs implement different OAuth grant-type variations. RFC 8693 defines `urn:ietf:params:oauth:grant-type:token-exchange` for token-exchange flows. Microsoft Entra ID does *not* implement RFC 8693 — it uses its own On-Behalf-Of flow with `urn:ietf:params:oauth:grant-type:jwt-bearer` and a different endpoint structure. If a meaningful share of Solace customers use Entra (which is plausible), our config schema must not lock us into RFC 8693 alone.

### Decision

Add a `grant_type` discriminator field. V1 supports one value: `urn:ietf:params:oauth:grant-type:token-exchange` (RFC 8693, also the default). Other grant-type strings are not implemented in V1 — the validator rejects them with a clear "not yet supported in this version" error. A separate spike/investigation ticket will assess whether to add Entra OBO support (`jwt-bearer`) and what runtime work that requires.

### Why

1. **Adding a config field later is cheap; adding a config field after operators write configs is a migration.** Putting `grant_type` in the schema today, even with one supported value, means future extension (Entra OBO, client_credentials for any future use case) is a one-line allowlist change plus runtime work — never a schema migration.

2. **The discriminator is consistent with `client_auth_method`.** Two discriminators in the same block, same pattern, same operator mental model: "this string picks which protocol variant you're using; the validator tells you which values are supported now."

3. **The Entra concern is real.** Microsoft Entra ID is the dominant enterprise IdP in many Solace customer accounts, and it does not implement RFC 8693. The schema must not presuppose a choice the runtime hasn't made yet about which IdPs we support natively.

### Honest caveat

`grant_type`'s existence in the schema does *not* mean Entra OBO works in V1. It only means the schema doesn't *block* Entra OBO from being added later. The runtime work to support `jwt-bearer` (different request body shape, different response handling, different endpoint conventions) is substantial and lives in a separate ticket.

This is the same "schema is forward-compatible, validator is strict about V1's implementation" pattern as `client_auth_method`. We accept the extra field in the schema today in exchange for keeping the door open without ceremony.

---

## T2 — Per-broker `audience` and `scopes`: both optional, grounded in Solace broker validation toggles

**Date:** 2026-06-16
**Sub-ticket:** [SOL-150796](https://sol-jira.atlassian.net/browse/SOL-150796)

### The question

When a broker uses `auth.mode: oauth`, what should the MCP server tell the IdP to mint? RFC 8693 §2.1 allows the MCP server to specify a per-request `audience` (target audience claim) and `scope` (requested scopes). Both are marked OPTIONAL in the RFC. Should our config require them when `mode: oauth`?

### Decision

Both fields are **optional** in our schema. The MCP server includes them in the token-exchange request only when set. When empty, the field is omitted from the request entirely, and the IdP's default behavior applies.

### Why

The Solace broker's global `OauthProfile` (from the SEMP v2 spec) exposes per-field validation toggles:

- `resourceServerValidateAudienceEnabled` (boolean): controls whether the broker validates the token's `aud` claim. When disabled, the broker accepts tokens with any (or no) audience. When enabled, the broker requires `resourceServerRequiredAudience` to match.
- `resourceServerValidateScopeEnabled` (boolean): controls whether the broker validates the token's `scope` claim. When disabled, the broker accepts tokens with any (or no) scopes. When enabled, the broker requires every value in `resourceServerRequiredScope` to be present in the token's scope claim.

The MCP server has no way to know the broker's toggle state without probing the broker's SEMP config (which Decision 9 forbids; we do not do live IdP/broker introspection at startup). So the operator — who knows their broker's profile config — decides per-broker whether to set these fields. Requiring them at the MCP-server level would force operators to set values that may not matter for their broker, and may even conflict with their broker's actual policy.

This also matches RFC 8693 §2.1, which marks both fields as OPTIONAL request parameters. Our schema honors the RFC's intent: the request shape is what the operator says it is.

### Runtime behavior

When building the token-exchange request body, the MCP server:

- Includes `audience=<value>` if and only if `audience` is non-empty.
- Includes `scope=<space-joined values>` if and only if `scopes` is non-empty.
- Sends neither otherwise.

If the IdP rejects the request because *it* requires `audience` (some do, even when the broker does not), the operator sees a clear IdP error and configures the field. Not a silent failure.

### Note on shape: `scopes` is a YAML list, not a space-separated string

Solace's `resourceServerRequiredScope` field is a single space-separated string. Our schema uses a YAML list (`scopes: [a, b, c]`). This is purely a presentation choice — YAML lists are more natural to write and review than space-separated strings, and we join with spaces when building the wire-level request body.

---

## T2 — `requested_token_type`: not included in the schema (always access token)

**Date:** 2026-06-16
**Sub-ticket:** [SOL-150796](https://sol-jira.atlassian.net/browse/SOL-150796)

### The question

RFC 8693 §2.1 defines an OPTIONAL `requested_token_type` parameter that lets the client ask the IdP for a specific token type (access token, refresh token, JWT, SAML2, etc.). Default per the RFC is `urn:ietf:params:oauth:token-type:access_token`. Should our config expose this as a field?

### Decision

Not included in V1. The MCP server always asks for (or relies on the IdP's default of) an access token. No config field.

### Why

Our use case is fixed: the MCP server presents the minted token to a Solace broker as a Bearer credential at the SEMP endpoint. Only an access token works for that. No realistic Solace deployment needs a refresh token, a SAML assertion, or a JWT-shaped token-of-tokens at this layer.

Adding a config field for a setting that has exactly one correct value is a knob nobody turns. If a customer ever needs a non-access-token (hard to imagine in this architecture), we add the field then. Until then it would be schema noise.

### Runtime behavior

T6 either omits `requested_token_type` from the token-exchange request (relying on the RFC default) or hardcodes `urn:ietf:params:oauth:token-type:access_token`. Both produce the same outcome with our IdPs.

---

## T2 — Final field list and operator-facing schema

**Date:** 2026-06-16
**Sub-ticket:** [SOL-150796](https://sol-jira.atlassian.net/browse/SOL-150796)

### The complete schema operators see

```yaml
# Hop 1: how agents authenticate to the MCP server.
mcp_client_auth:
  mode: oauth                    # or "disabled" / "static"
  issuer: "https://idp.example.com"
  audience: "mcp-server"
  resource_url: "https://mcp.example.com/mcp"

# Hop 2: the IdP coordinates the MCP server uses to obtain tokens for brokers.
# Required when at least one broker has auth.mode: oauth.
broker_oauth:
  idp_token_url: "https://idp.example.com/oauth/token"
  mcp_server_client_id: "mcp-server"
  mcp_server_client_auth:                                       # discriminated union: populate exactly one sub-block
    client_secret_basic:
      secret: "${MCP_CLIENT_SECRET}"
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange" # required, no default
  audience_parameter_name: "audience"                           # required, no default — one of: audience | scope | resource

brokers:
  prod-east:
    url: "https://broker.example.com:943"
    auth:
      mode: oauth                                # basic | bearer | oauth
      audience: "solace-broker-prod"             # oauth: optional
      scopes:                                    # oauth: optional
        - "semp:read"
        - "semp:write"
```

### The exhaustive field table

#### Global block (`broker_oauth`)

| Field | Required | Default | Validator rule | Source of truth |
|---|---|---|---|---|
| `idp_token_url` | yes | — | non-empty, valid URL, `http`/`https` scheme | operator-supplied (Discovery deferred to a follow-up) |
| `mcp_server_client_id` | yes | — | non-empty | operator-supplied (the MCP server's client_id registered at the IdP) |
| `mcp_server_client_auth` | yes | — | discriminated union: exactly one sub-block populated. V1 supports `client_secret_basic` and `client_secret_post`, both of which require a non-empty `secret:` (after `${VAR}` substitution). Other registered methods are rejected with "not yet supported in this version" | operator-supplied (method chosen per the IdP's requirements) |
| `grant_type` | yes | — (no default) | must be in the V1 allowlist (`urn:ietf:params:oauth:grant-type:token-exchange`); other grant-types rejected with "not yet supported in this version" | operator-supplied; the allowlist expands as future grant-types (e.g. Entra OBO's `jwt-bearer`) gain runtime support |
| `audience_parameter_name` | yes | — (no default) | must be one of `audience`, `scope`, `resource` | operator-supplied (the parameter name the IdP expects to carry the per-broker audience identifier in the token-exchange request) |

#### Per-broker (`brokers.<alias>.auth`, when `mode: oauth`)

| Field | Required | Default | Validator rule | Source of truth |
|---|---|---|---|---|
| `mode` | yes | — | must be in `validAuthModes` (`{basic, bearer, oauth}`) | operator-supplied |
| `audience` | no | — (omitted from request when empty) | non-empty if set | operator decides based on the broker's `resourceServerValidateAudienceEnabled` and the IdP's requirements |
| `scopes` | no | — (omitted from request when empty) | each entry non-whitespace if set | operator decides based on the broker's `resourceServerValidateScopeEnabled` and `resourceServerRequiredScope` |

#### Constants added to existing code

| Constant | Value | Where it lives | Purpose |
|---|---|---|---|
| `AuthModeOAuth` | `"oauth"` | `internal/config/config.go` (alongside `AuthModeBasic`/`AuthModeBearer`) | Adds `oauth` as an allowed per-broker auth mode. Already exists for Hop 1's `validAuthClientModes`; we're reusing the same string for `validAuthModes`. |
| `ClientAuthMethodSecretBasic` | `"client_secret_basic"` | new | Standard IANA value; the V1 default for `client_auth_method`. |
| `ClientAuthMethodSecretPost` | `"client_secret_post"` | new | Standard IANA value; alternative `client_auth_method`. |
| `GrantTypeTokenExchange` | `"urn:ietf:params:oauth:grant-type:token-exchange"` | new | RFC 8693 grant type string; the V1 default for `grant_type`. |

### The "what is the operator actually required to type?" answer

A new operator deploying with all V1 defaults writes:

```yaml
broker_oauth:
  idp_token_url: "..."
  mcp_server_client_id: "..."
  client_secret: "${...}"

brokers:
  my-broker:
    url: "..."
    auth:
      mode: oauth
      # audience and scopes only if your broker's profile validates them
```

That's the minimum. Everything else (`client_auth_method`, `grant_type`, `audience`, `scopes`) is either defaulted or genuinely optional per the broker's profile.

### Reasoning thread for "why these fields and no others"

A reader asking "why is field X not in the schema?" should find their answer here:

- **`issuer_url` / OIDC Discovery** — deferred per the earlier T2 entry. Runtime work, not schema work.
- **`private_key_file`, `client_cert_file`, `client_key_file`, `key_id`** — fields needed by `private_key_jwt` and `tls_client_auth`. Deferred to the `client_auth_method` follow-up ticket along with their runtime support.
- **`requested_token_type`** — always access_token in our use case. Knob nobody turns.
- **`resource`** (RFC 8693 §2.1 OPTIONAL parameter, similar to `audience`) — not added because Solace brokers use the `aud` claim for resource binding, not RFC 8707 resource indicators. If a customer uses an IdP that requires `resource` parameter, we add it then. So far, no demand.
- **Refresh-token handling, jti tracking, token caching policy** — runtime concerns (T4/T5). Not schema concerns.

### What is *not* committed to V1 by this schema

- The Discovery / `issuer_url` path. Add later, additively.
- The `private_key_jwt` / `tls_client_auth` methods and their fields. Add later, additively. The follow-up ticket has full design license on flat-vs-nested shape because no operator has written those fields yet.
- Entra OBO support. Investigation ticket separate.

### V1 commitment

These are the operator-facing field names. They do not change without a deprecation cycle:

`idp_token_url`, `mcp_server_client_id`, `client_auth_method`, `client_secret`, `grant_type` (under `broker_oauth`).
`mode`, `audience`, `scopes` (under per-broker `auth`).

Optional fields can be added; required fields cannot be added without breaking existing configs. Field renames are migrations and require a deprecation cycle. The schema is the contract.

---

## T2 — Cross-IdP audit (Keycloak / Okta / Auth0 / Entra)

**Date:** 2026-06-16
**Sub-ticket:** [SOL-150796](https://sol-jira.atlassian.net/browse/SOL-150796)

### Why this audit exists

The schema needs to support several different IdPs that real Solace customers use. Rather than designing the schema on assumptions about how each IdP behaves, we surveyed each one to confirm how it actually handles the request parameters that carry the broker's identity (`audience`, `resource`, `scope`). The audit drove the `audience_parameter_name` discriminator decision below.

### Findings (sourced from each IdP's official docs)

| IdP | RFC 8693 support | `audience` parameter | `resource` parameter | `scope` carries resource? | What the broker's identity flows in |
|---|---|---|---|---|---|
| **Keycloak** (26.2+) | Yes (standard token exchange) | Yes — must be a registered `client_id`, not a URI | No (not implemented as of 26.x) | No | `audience=<client_id>` |
| **Okta** (Custom Authorization Server) | Yes | Yes — logical name or URI, matches the AS's configured Audience | Not documented | No | `audience=<value>` |
| **Auth0** (Custom Token Exchange) | Yes | Yes — defaults; opaque API Identifier (URL or string) | Opt-in only via "Resource Parameter Compatibility Profile" (off by default); `audience` wins on conflict | No | `audience=<value>` |
| **Entra ID v2 OBO** | No — uses proprietary OBO with `grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer` | No (silently ignored on v2) | No (rejected when combined with v2 `scope`) | **Yes** — encoded as `<app-id-uri>/<scope-name>` per scope value | `scope=<app-id-uri>/<scope-name>` (audience prefixed onto every scope) |
| **Entra ID v1** (legacy) | N/A | No | **Yes** — `resource=<app-id>` | No | `resource=<app-id>` |

Sources (each row verified against the named IdP's docs in June 2026):

- Keycloak: [Configuring and using token exchange](https://www.keycloak.org/securing-apps/token-exchange); [Standard Token Exchange GA in 26.2](https://www.keycloak.org/2025/05/standard-token-exchange-kc-26-2).
- Okta: [Set up OAuth 2.0 On-Behalf-Of Token Exchange](https://developer.okta.com/docs/guides/set-up-token-exchange/main/); [Set up AI agent token exchange](https://developer.okta.com/docs/guides/ai-agent-token-exchange/-/main/).
- Auth0: [Resource Parameter Compatibility Profile](https://auth0.com/ai/docs/mcp/guides/resource-param-compatibility-profile); [The Many Faces of OAuth 2.0 Token Exchange](https://auth0.com/blog/the-many-faces-of-oauth2-token-exchange/).
- Entra v2 OBO: [Microsoft identity platform and OAuth 2.0 On-Behalf-Of flow](https://learn.microsoft.com/en-us/entra/identity-platform/v2-oauth2-on-behalf-of-flow); [Scopes and permissions in the Microsoft identity platform](https://learn.microsoft.com/en-us/entra/identity-platform/scopes-oidc).

### What the audit told us

Three concrete things drove the schema decisions that follow:

1. **There is no single wire-level parameter that all IdPs use.** Three of four major IdPs use `audience`. Entra v2 uses `scope` (with audience embedded). Entra v1 uses `resource`. Auth0 supports `resource` opt-in but prefers `audience`. The schema must be agnostic to which one of these the operator's IdP wants.

2. **The operator's mental model can stay uniform.** Across all four IdPs, the operator is configuring "the broker's identity at the IdP." That value goes into the minted token's `aud` claim regardless of how the IdP encodes the request-side parameter. So the operator-facing field name (`audience`) accurately describes the operator's intent even when the wire format differs.

3. **Wire-format encoding is per-IdP-family, not per-IdP-vendor.** Once an IdP's grant-type and audience-carrying parameter are known, the wire format is determined. The schema only needs to expose those two choices (`grant_type` and `audience_parameter_name`), not enumerate vendors.

These three observations are what the `audience_parameter_name` discriminator's existence is justified by.

---

## T2 — `audience_parameter_name` discriminator: schema-flexible, runtime deferred per IdP family

**Date:** 2026-06-16
**Sub-ticket:** [SOL-150796](https://sol-jira.atlassian.net/browse/SOL-150796)

### The question

The cross-IdP audit established that different IdPs carry the broker's identity in different request parameters: `audience` (Keycloak/Okta/Auth0), `scope` (Entra v2), or `resource` (Entra v1; Auth0 opt-in; future RFC 8707-aligned IdPs). The schema must let the operator pick which parameter their IdP expects without locking us out of IdPs we haven't surveyed.

The earlier draft of T2 proposed deriving the parameter from `grant_type` (token-exchange ⇒ `audience`, jwt-bearer ⇒ `scope`). This was rejected after recognizing that the OAuth specs do not require a 1:1 grant-type ↔ wire-encoding mapping — a future IdP could legitimately implement a different pairing. Silent derivation would force a runtime branch (or break) on such an IdP.

### Decision

Add an explicit `audience_parameter_name` field to `BrokerOAuthConfig`. Allowed values: `audience`, `scope`, `resource`. Default: `audience`.

The runtime uses this value to determine which OAuth parameter name carries the per-broker `audience` value on the wire. The runtime implementation lives in T6 (and any IdP-family-specific follow-up tickets); T2 only commits to the schema shape.

### Why explicit over derived

1. **OAuth specs do not require a fixed grant-type ↔ wire-encoding mapping.** Asserting one would constrain us to currently-observed behavior of currently-surveyed IdPs. New IdPs could be standards-compliant but not match our derivation rule. Explicit is robust to future IdPs we haven't audited.

2. **It removes a hidden coupling.** With derivation, an operator reading their YAML cannot tell which wire parameter carries the audience without reading our docs. With an explicit field, the YAML is self-describing.

3. **The footgun is acceptable.** An operator could in theory set `grant_type: token-exchange` with `audience_parameter_name: scope` and fail at first request. The validator emits a warning (not error) for unusual pairings. The IdP returns a clear error on the failed request. Compared to silent derivation, where the same misconfiguration produces an identical failure with no in-YAML evidence of why, explicit is strictly safer.

### Why `resource` stays in the allowlist even though no V1 IdP uses it as primary

The audit showed that Auth0 supports `resource` opt-in, and Entra v1 used it. The next-generation MCP-aligned IdPs may also adopt it (RFC 8707 has growing momentum in the MCP authorization spec). The cost of keeping it in the allowlist is one string in the validator; the cost of removing it and later adding it back is a schema change with operator-facing migration notes. The cost asymmetry favors keeping it.

We will not *recommend* `resource` in V1 docs. We will not block it.

### Naming

`audience_parameter_name` was chosen over `audience_in`, `audience_request_param`, `audience_send_as`, `audience_target`, `audience_encoding`. Rationale: short enough to type, OAuth-vocabulary-aligned ("OAuth request parameter"), and the values themselves (`audience`/`scope`/`resource`) are all OAuth parameter names, so a field named `_param` accurately describes what the value is.

### What the validator does today

- `audience_parameter_name` is an optional string in `BrokerOAuthConfig`.
- Empty → defaults to `audience`.
- Value in `validAudienceParams` (`{audience, scope, resource}`) → accepted.
- Any other value → rejected with a clear error listing the allowed values.

### What the runtime does today (T2's scope)

Nothing. T2 is schema-only. T6 (or a follow-up ticket per IdP family) implements the wire-format composition per `audience_parameter_name` value.

### How operator-facing fields stay consistent across IdPs

Operators write the same per-broker fields regardless of IdP:

```yaml
brokers:
  prod:
    auth:
      mode: oauth
      audience: "<broker-identifier-at-the-IdP>"
      scopes:
        - "<bare-scope-name>"
```

`audience` is the broker's identity at the IdP — sent verbatim to the IdP, ends up in the minted token's `aud` claim. `scopes` are bare permission names — not prefixed with the audience.

The runtime composes the wire request based on `audience_parameter_name`. For Entra (`audience_parameter_name: scope`), the runtime composes one combined `scope=<audience>/<scope>` per scope entry. For Keycloak/Okta/Auth0 (`audience_parameter_name: audience`), the runtime sends `audience` and `scope` as two separate parameters. Operators do not encode the audience into each scope by hand.

---

## T2 — Schema is Entra-ready; runtime composition deferred to the Entra epic

**Date:** 2026-06-16
**Sub-ticket:** [SOL-150796](https://sol-jira.atlassian.net/browse/SOL-150796)

### The question

Microsoft Entra ID does not implement RFC 8693. It uses its proprietary On-Behalf-Of flow with `grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer` and encodes the broker's identity inside `scope` rather than as a top-level `audience` or `resource` parameter. A meaningful share of Solace customers use Entra. The schema must be ready to support Entra without breaking changes once that work is prioritized.

### Decision

T2's schema (`idp_token_url`, `mcp_server_client_id`, `client_auth_method`, `client_secret`, `grant_type`, `audience_parameter_name` globally; `audience`, `scopes` per broker) is **intentionally designed to support Entra OBO as a future, additive runtime feature.** T2 does not implement Entra OBO. T2 does not commit to any runtime composition rules — neither for RFC 8693 IdPs nor for Entra. The runtime work for each IdP family lives in T6 or in IdP-family-specific follow-up tickets (e.g., a dedicated Entra OBO epic).

### Why T2 is explicitly *not* implementing the Entra composition rules

The earlier conversation worked out concrete runtime composition rules (e.g., for `audience_parameter_name: scope`, the runtime sends `scope=<audience>/<scope>` per entry, joined with spaces). These rules are correct per Microsoft's documentation but they are *runtime behavior*. T2 is a schema-only ticket. Locking the rules in here would either:

- Force T2's scope to grow to include runtime work, contradicting the ticket's stated scope-only nature, or
- Document rules that nothing in V1 enforces or applies, leading to schema-and-runtime drift if the Entra epic later decides to compose differently.

Both are worse than deferring the composition rules entirely to the ticket that implements them.

### What T2 *does* commit to (Entra-readiness shape)

Three schema commitments make Entra implementation purely additive when the Entra epic happens:

1. **`audience_parameter_name` is a discriminator with `scope` in its allowlist.** When the Entra epic ships, no schema change is needed — operators just set `audience_parameter_name: scope`.

2. **`grant_type` is a discriminator that can accept additional values via allowlist expansion.** When the Entra epic ships, `urn:ietf:params:oauth:grant-type:jwt-bearer` joins `validGrantTypes` — no new fields, no migration.

3. **Per-broker `audience` and `scopes` describe operator intent, not wire format.** Operators write the broker's identity in `audience` and bare scope names in `scopes`. The runtime is responsible for any per-IdP composition. The operator-facing contract does not vary by IdP. This is the load-bearing property: an existing Solace deployment configured for Keycloak today does not have to rewrite its `audience` and `scopes` fields when migrating to Entra later — the runtime handles the difference.

### What the Entra epic will own when it happens

The Entra epic — when prioritized — owns:

- The runtime composition rules per `audience_parameter_name` value (including the `<audience>/<scope>` prefixing for Entra and the `.default` semantics for empty-scopes).
- Validator warnings for unusual `grant_type` + `audience_parameter_name` pairings (not in V1).
- Operator-facing documentation for Entra-specific gotchas (`accessTokenAcceptedVersion`, app-id-URI conventions, broker `resourceServerRequiredAudience` alignment).
- E2E tests against a live Entra instance.

None of those land in T2. T2 lands the schema shape they need to be implementable additively.

### What this commits the schema to forever

The four operator-facing names involved in this design — `audience_parameter_name`, `grant_type`, `audience`, `scopes` — are V1 contract surface. They will not be renamed or removed without a deprecation cycle. Their *meanings* (operator intent, not wire format) are also part of the contract. The runtime's internal interpretation of these fields can evolve; the schema cannot.

---

## T2 — Validator rejects `auth.mode: oauth` until the OAuth runtime is wired

**Date:** 2026-06-16
**Sub-ticket:** [SOL-150796](https://sol-jira.atlassian.net/browse/SOL-150796)

### The question

T2 adds the `oauth` value to the per-broker auth mode allowlist and adds `broker_oauth` as a top-level config block. Nothing in V1 actually *reads* these at runtime — the OAuth dispatcher, exchanger, token cache, and protocol-client wiring all land in later tickets (T3 / T4 / T5 / T6 / T7a). What happens if an operator writes `auth.mode: oauth` between T2's release and the runtime tickets landing?

Without intervention, the failure mode is: config validation passes, server starts cleanly, first request to the OAuth-mode broker triggers `auth.NewAuthenticator`'s `default` branch (since no `oauth` case exists yet), returning an "unsupported auth mode" error that surfaces at request time rather than startup time. Bad UX — late, hidden, hard to debug.

### Decision

**Until the OAuth runtime is wired, the validator rejects any broker with `auth.mode: oauth` at startup with a clear, actionable error.** The same applies to the global `broker_oauth` block: it is accepted and validated as a schema artifact, but with a startup `INFO` log noting that no OAuth runtime is active in this version yet. Per-broker `mode: oauth` is the hard fail; standalone `broker_oauth` is only informational.

No feature flag is introduced.

### Why no feature flag

Feature flags exist for behavior an operator might want to toggle. Examples: experimental retry logic, new caching strategy, optional canary code path. Here there is no operator choice — until the runtime is wired, OAuth simply does not work. A flag would imply "you could enable this if you wanted," which is false.

Flags also tend to outlive their purpose. Once OAuth is fully wired, the flag becomes dead code that someone has to remember to remove. The natural "remove the rejection when the runtime lands" path doesn't have that problem — the rejection is part of the validator and is replaced by full validation when T6 ships.

### Why this enforcement is the right choice

1. **The contract is honest.** The schema *describes* OAuth (Entra-readiness, forward-compatibility). The runtime *does not yet implement* it. The validator reflects reality: "this configuration is recognized but not functional in this version." That's more honest than silent acceptance (which lies about V1 supporting OAuth) or a feature flag (which suggests a choice that doesn't exist).

2. **Failures land at startup, not at request time.** An operator who tries `mode: oauth` sees the rejection immediately when starting the server — not on the first SEMP request hours later. Cheap, fast diagnostic loop.

3. **The placeholder removes itself cleanly.** When the OAuth runtime is wired (T5 or T6), the rejection in `validateBroker` is replaced by full per-mode validation. No flag to flip, no dead code. The natural progression of the codebase removes the placeholder. This avoids the "we forgot to enable the feature" failure mode that plagues flag-gated work.

### What the validator does today (after T2)

For per-broker `auth.mode: oauth`:

```go
case AuthModeOAuth:
    // SCHEMA: validate OAuth-specific fields per the schema (audience non-empty
    // if set, scopes contains no whitespace-only entries, etc.).
    // ...

    // RUNTIME: reject until the OAuth runtime is wired in.
    // This rejection is removed by the sub-ticket that wires `auth.NewAuthenticator`'s
    // OAuth case (currently planned as part of T6).
    errs = append(errs, fmt.Errorf(
        "broker %q: auth.mode %q is recognized but not yet supported in this version "+
            "(tracked in SOL-150070); use basic or bearer for now",
        alias, AuthModeOAuth))
```

For top-level `broker_oauth`:

- If the block is set, validate its structural fields (`idp_token_url` is a valid URL, `client_secret` non-empty after env substitution, `client_auth_method` and `grant_type` and `audience_parameter_name` are in their respective allowlists).
- If validation passes, emit a startup `INFO` log noting the block is accepted in schema but not yet consumed by any runtime in this version.
- Do not reject. An operator may legitimately be staging configuration ahead of the runtime support landing.

### Rejection order: schema validation first, then the rejection

The rejection runs *after* schema-level validation for the oauth mode (audience field shape, scopes shape, etc.). This means an operator who configures `mode: oauth` with a malformed `audience` gets *both* errors:

- "broker X: audience must be non-empty when set"
- "broker X: auth.mode oauth is recognized but not yet supported in this version"

This ordering is deliberate: when T6 lands and the rejection is removed, the schema-level validators are already field-tested by operators who tried oauth too early. Errors-aggregated-via-`errors.Join` (the existing project pattern) makes both visible.

### What the sub-ticket that wires the runtime owns

Whichever sub-ticket lands `auth.NewAuthenticator`'s OAuth case (currently planned in T6) is responsible for **removing the rejection block from `validateBroker`**. This is a one-line removal — the schema validators above the rejection stay; the unconditional `errs = append(errs, ...)` line goes. The decisions doc and the sub-ticket's PR description should both note this removal explicitly.

### What this commits us to

- The rejection error message is operator-facing — it must stay actionable. Future edits to this rejection (if the tracking ticket changes, or if the supported modes change) should keep the format: name what was configured, why it failed, and what the operator can do today instead.
- The rejection is a *protection*, not a contract. It will be removed when the runtime lands. No deprecation cycle needed for the rejection itself.

### Operator visibility: standalone banner alongside the joined error

The per-broker rejection error sits inside the joined `errors.Join` result that `LoadConfig` returns. In a deployment that has multiple unrelated config problems (typos, bad URLs, invalid log levels, etc.), the OAuth rejection is one bullet among many inside a single ERROR log line's `error=` field. The load-bearing problem — "this entire feature is not available in this build" — gets visually equal weight to "you typed `trace` instead of `info`."

To fix the UX without changing the validator's error-aggregation contract, `validate()` emits a standalone operator-facing banner via `slog.Error` as a SEPARATE log line, **just before** returning the joined error. The banner fires once when one or more brokers are configured with `auth.mode: oauth`; the count is reported with correct singular/plural ("1 broker" or "N brokers"), but the banner does not list broker names — those live in the joined error below, so the banner scales to any number of affected brokers without blowing out.

Banner wording is operator-language: states the limitation, names the count, prescribes the action for today, names the feature as planned in a future release. No internal ticket references in the banner (operators are not our audience for our Jira IDs), no apologetic "please report" framing.

The structure operators see in startup logs:

1. `INFO server starting`
2. `INFO loaded .env file`
3. `ERROR <multi-line banner>` ← the loud headline, fires once
4. `ERROR failed to load config error="validating config: ..."` ← comprehensive, includes broker names

Code: `oauthNotSupportedBanner` const + `logOAuthNotSupportedBanner(n int)` helper in `internal/config/config.go`. The per-broker rejection error inside `validateBroker`'s `case AuthModeOAuth:` is unchanged — the banner is purely additive.

Test coverage: `TestLoadConfig_EmitsOAuthNotSupportedBanner` exercises three cases: a single oauth broker (singular wording), multiple oauth brokers (plural wording), and a config with no oauth brokers (banner must not fire).

---

## T2 — `mcp_server_client_auth` is a discriminated union (named sub-blocks), not a flat method field with conditional siblings

**Date:** 2026-06-16
**Sub-ticket:** [SOL-150796](https://sol-jira.atlassian.net/browse/SOL-150796)

### The question

The earlier T2 drafts treated `client_auth_method` as a flat discriminator string with the method-specific fields (`client_secret`, future `private_key_file`, etc.) as flat siblings under `broker_oauth`. That shape works for V1's one supported method family, but:

- It does not communicate the method/credential coupling visually in the YAML — operators have to know the per-field rules.
- It allows operators to write fields in invalid combinations (e.g., `client_auth_method: private_key_jwt` paired with `client_secret: "..."`), which the validator catches at startup but the schema does not structurally prevent.
- It commits us to "flat fields under `broker_oauth`" as the V1 contract, which becomes structurally hard to evolve once the follow-up adds `private_key_jwt`, `tls_client_auth`, and `self_signed_tls_client_auth` (5+ flat conditional fields at the top level becomes a grab bag).

Reversing the flat shape later would be a breaking schema change for every existing operator config — exactly what the V1 schema commitment is supposed to prevent.

### Decision

`mcp_server_client_auth` is restructured as a **discriminated union with named per-method sub-blocks**. The block name *is* the method choice. Exactly one sub-block is populated at any time. Each sub-block contains only the fields its method needs. No standalone `client_auth_method:` string field.

V1 ships two sub-blocks: `client_secret_basic` and `client_secret_post` (each containing a single `secret:` field). The follow-up ticket adds `private_key_jwt`, `tls_client_auth`, and `self_signed_tls_client_auth` as new sibling sub-blocks — purely additive, no existing operator config breaks.

### What this looks like in YAML

```yaml
broker_oauth:
  idp_token_url: "https://idp.example.com/oauth/token"
  mcp_server_client_id: "mcp-server"
  mcp_server_client_auth:
    client_secret_basic:
      secret: "${MCP_CLIENT_SECRET}"
    # Or, for client_secret_post (V1 supported):
    # client_secret_post:
    #   secret: "${MCP_CLIENT_SECRET}"
    # Future (follow-up tickets):
    # private_key_jwt:
    #   private_key_file: "/etc/mcp/oauth-key.pem"
    #   key_id: "kid-abc"
    # tls_client_auth:
    #   cert_file: "/etc/mcp/oauth-client.crt"
    #   key_file: "/etc/mcp/oauth-client.key"
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: audience
```

### Why this shape, grounded in production-grade precedent

The decision is informed by what production YAML-driven config systems actually do when they need to support multiple alternative configurations of the same concept over time. Three precedents drove the choice:

1. **Kubernetes CRDs and cert-manager.** cert-manager's `ClusterIssuer` `solvers` array uses exactly this pattern: each solver is a discriminated union with `http01:` or `dns01:` (and inside `dns01:`, named provider sub-blocks like `cloudflare:`, `route53:`, `cloudDNS:`). The Kubernetes ecosystem is the largest community of production YAML operators, and this pattern is what they read and write daily. Operators who recognize the shape from Kubernetes recognize it immediately here.

2. **OpenAPI's `discriminator` + `oneOf`.** The formal spec for polymorphic schemas in OpenAPI uses a discriminator property to select which schema validates the structure. Our `mcp_server_client_auth` is the OpenAPI-canonical shape of this pattern: the sub-block name *is* the discriminator value, and the sub-block's contents are the per-type schema.

3. **Envoy proxy.** Envoy's `validation_context` / `validation_context_sds_secret_config` / `combined_validation_context` are three sibling sub-blocks at the same level, with an explicit "only one may be set" constraint. Same pattern.

The common thread: *as the number of alternatives grows beyond one, flat conditional siblings become unreadable. Discriminated unions with named sub-blocks scale.*

### Why this is worth the V1 cost

The cost: V1's minimum config is two YAML lines longer than the flat shape would be (an extra `mcp_server_client_auth:` parent key and `client_secret_basic:` sub-key, both with their indentation). Operators on the simplest setup write seven lines of `broker_oauth` instead of five.

The benefits, in order of impact:

1. **Structural impossibility of misconfigured states.** An operator cannot configure `client_secret_basic` *and* `private_key_jwt` simultaneously — the YAML structure does not allow it (the validator enforces "exactly one populated" as a structural rule). They cannot write `private_key_file:` for `client_secret_basic` — the field does not exist on that sub-block. The schema *itself* communicates the coupling that the flat shape only encoded in the validator.

2. **The V1 contract holds forever.** When the follow-up adds `private_key_jwt`, an operator's `mcp_server_client_auth: { client_secret_basic: { secret: ... } }` still parses identically — the new method is a *sibling* sub-block they did not write. No migration, no deprecation cycle, no breaking change. This is exactly the "no drift" promise the V1 schema commitment was meant to provide.

3. **Operator vocabulary is unambiguous.** The method names (`client_secret_basic`, `private_key_jwt`, `tls_client_auth`) are IANA-registered OAuth method strings. Operators reading the YAML who already know OAuth — and operators who will look up an unfamiliar method — find authoritative documentation immediately because the keys *are* the IANA names.

4. **It matches the codebase's stated principle.** Earlier T2 decisions emphasized "design for extensibility now, accept the cost of forward-compatibility in V1." Flat-with-discriminator was the smaller-cost option that *partially* satisfied this principle — it kept the door open for new methods but committed the schema to flat top-level fields. The discriminated union is the larger-cost option that *fully* satisfies it, including for the operator-facing shape.

### Why not flat (the rejected option)

The flat shape was rejected after this analysis because:

- **It commits the V1 schema to a shape that does not scale.** Three flat conditional fields (`client_secret`, `private_key_file`, `client_cert_file`) at the same level as `client_auth_method` is already a grab bag at the moment the follow-up ships. Adding more methods later makes it worse.

- **The "validator-enforced coupling" argument is insufficient.** Validator errors fire at startup, but the schema itself does not communicate the coupling to operators reading the file. The schema is the user-facing contract; the validator is the implementation. Embedding contract semantics in the validator alone is leaky.

- **Reversing the choice post-V1 would be a breaking change.** The schema we ship is the schema operators write configs against. If V1 ships flat and the follow-up wants nested (which it would, by exactly the same logic that informs this decision), every operator config breaks at parse time when the follow-up lands.

### Validator semantics

The validator's contract for `mcp_server_client_auth`:

1. **Exactly one of the sub-blocks under `mcp_server_client_auth:` may be populated.** Zero populated → error ("at least one mcp_server_client_auth method is required"). More than one populated → error ("only one mcp_server_client_auth method may be configured at a time"). The error message names which sub-blocks were populated.

2. **The populated sub-block's required fields must be non-empty.** For V1, both `client_secret_basic` and `client_secret_post` require `secret:` non-empty (after `${VAR}` substitution). Each sub-block owns its own field validation in the validator; new sub-blocks added by follow-up tickets add their own field-validation rules.

3. **The populated sub-block's name is the method.** Inside the config package, the validator uses an internal `selectedMethod()` helper that returns the populated method name (along with any structural errors). Runtime code added by follow-up tickets dispatches on the populated sub-block directly — there is no public `Method()` accessor on the struct (see the "Follow-up cleanup: removing `Method()`" section later in this doc).

### Discriminator fields no longer have defaults

With this restructuring, the three discriminator fields (`grant_type`, `audience_parameter_name`, and the now-removed standalone `client_auth_method`) are all **required, no defaults**. The reasoning from earlier in T2 applies: defaults paper over choices that operators should be explicit about, and the cost is one line of typing on day one.

Specifically:

- The method choice is now implicit in *which sub-block under `mcp_server_client_auth:` is populated*. There is no separate `client_auth_method:` field to default. Operators choose by structure.
- `grant_type` and `audience_parameter_name` are required string fields. Validator rejects empty values with a clear "X is required" error. Allowlist membership is checked next.

This makes the contract uniformly explicit: every protocol choice is operator-acknowledged in the YAML.

### V1 contract commitment

These names and shapes are the V1 user-facing contract:

- Top-level `broker_oauth:` fields: `idp_token_url`, `mcp_server_client_id`, `mcp_server_client_auth`, `grant_type`, `audience_parameter_name`. All required.
- `mcp_server_client_auth:` sub-blocks: `client_secret_basic`, `client_secret_post`. Exactly one populated.
- `client_secret_basic` / `client_secret_post` fields: `secret`. Required.

None of the above may be renamed or removed without a deprecation cycle. Future methods land as **new sibling sub-blocks under `mcp_server_client_auth:`** — additive only. The follow-up ticket that adds `private_key_jwt` does not touch V1's two sub-blocks.

### Why this is the final answer

T2's `BrokerOAuthConfig` design has gone through several drafts. This one is grounded in:

- Direct evidence from production-grade systems facing the same problem (cert-manager, Envoy, Kubernetes ecosystem broadly).
- The formal OpenAPI pattern for polymorphic schemas.
- The "V1 schema is forever" commitment recorded earlier in this document.
- The "design for extensibility" principle stated repeatedly in T2's earlier decisions.

The cost (two extra YAML lines in the minimum case) is small. The benefit (forward-compatible schema, structural enforcement of method-credential coupling, zero migration cost for the follow-up) compounds over the project's lifetime. This is the shape the codebase commits to.

### Follow-up cleanup: `Method()` removed, `selectedMethod()` is the single resolver

The initial discriminated-union refactor shipped with three helpers: `populatedMethods()` (returned the list of populated sub-blocks), `Method()` (collapsed the list down to a single name or empty), and `validateBrokerClientAuth` (called `populatedMethods` and dispatched on the count).

`Method()` was anticipatory — it was added on the assumption that T6's runtime would dispatch on a method name string returned by this helper. T6 doesn't exist yet, and the only consumer of `Method()` in production code was `BrokerOAuthConfig.LogValue`. The validator went through `populatedMethods()` directly. Two different shapes serving two different consumers.

The cleanup collapses both helpers into a single `selectedMethod() (string, []error)` method on `BrokerClientAuth`. It returns the resolved method name on the happy path and structural errors otherwise. The validator and both `LogValue` implementations call it directly:

- The validator propagates the errors into its error-aggregation chain.
- `LogValue` discards the errors (logging is best-effort; the validator surfaces the real issue separately).

What removed: `Method()` and `populatedMethods()`. What stays: `allowedClientAuthMethods()` (used in error messages). When T6 needs runtime dispatch, it will define the right-shaped API for its actual consumers — likely returning a constructed `Authenticator`, not a string the runtime then has to switch on. Adding `Method()` now committed us to that string-returning shape before knowing it was the right fit.

Naming choice: `selectedMethod` over alternatives like `resolveMethod` (too generic — which method?) and `resolveClientAuthMethod` (redundant — receiver type already says "client auth"). `selectedMethod` reads cleanly at call sites: *"the BrokerClientAuth's selected method, with any errors with that selection."*

---

## T2 — Hop 1 client-auth block renamed: `client_auth` → `mcp_client_auth` (and Go type `ClientAuthConfig` → `MCPClientAuthConfig`)

**Date:** 2026-06-17
**Sub-ticket:** [SOL-150796](https://sol-jira.atlassian.net/browse/SOL-150796)

### The question

The earlier T2 design landed two blocks that both contain the word `client_auth`:

- The Hop 1 top-level block: `client_auth:` — *how agents authenticate to the MCP server*. The MCP server is the OAuth resource server here. Pre-existing in the codebase before T2.
- The Hop 2 nested sub-block: `broker_oauth.mcp_server_client_auth:` — *how the MCP server authenticates to the IdP*. The MCP server is the OAuth client here. Introduced by T2's discriminated-union refactor.

Both are correctly named for what they configure in isolation, but together they invite confusion. An operator skimming the YAML sees `client_auth` in two places and has to read carefully to understand which "client" is being authenticated.

### Decision

The Hop 1 block is renamed:

- YAML key: `client_auth` → `mcp_client_auth`.
- Go type: `ClientAuthConfig` → `MCPClientAuthConfig`.
- Go field on `ServerConfig` and `yamlConfig`: `ClientAuth` → `MCPClientAuth`.

The Hop 2 nested sub-block (`broker_oauth.mcp_server_client_auth`, of type `BrokerClientAuth`) stays as-is. Its context — "inside `broker_oauth:`" — already disambiguates what kind of client is being authenticated.

### Why this is worth doing

1. **The Hop 1 block is the one whose name needs the qualifier.** In context, "the MCP server's client auth config" answers the question *whose client* is being authenticated — it's the MCP client (the agent) authenticating to the MCP server. `mcp_client_auth` makes that explicit. Without the qualifier, an operator coming to the file fresh sees `client_auth` and has to figure out which "client" it refers to.

2. **The Hop 2 sub-block's context disambiguates it.** `broker_oauth.mcp_server_client_auth` is read as "broker-OAuth client-auth" — the MCP server's client identity at the IdP, used for broker OAuth. The `broker_oauth:` parent already establishes "this is about authenticating to the IdP for broker access," so `mcp_server_client_auth` inside that context unambiguously means "how the MCP server identifies itself *as a client* of the IdP." No rename needed.

3. **The asymmetry is a feature, not a bug.** The Hop 1 block is at the top level of the YAML — there's no parent block to provide context. It needs the qualifier embedded in its own name. The Hop 2 sub-block lives inside `broker_oauth:`, which provides the context. Symmetric renaming (e.g., renaming both to `mcp_*`) would over-qualify the nested one.

### What this commits us to

- `mcp_client_auth:` is the V1 contract for the Hop 1 block. It will not be renamed again without a deprecation cycle.
- The Go type `MCPClientAuthConfig` and field `MCPClientAuth` are internal API surface but the naming is now stable.
- Existing internal operator deployments using `client_auth:` must update their YAML. We are pre-V1, so this is a tolerable break — but it is the kind of break that informed our other "design once, commit forever" decisions in T2.

### Pre-V1 license — used deliberately

This rename uses some of our pre-V1 schema-change budget. We did so because:

- The disambiguation pays off forever in operator UX.
- Doing it now (before V1) costs only internal redeployment of operator YAML; doing it post-V1 would cost public deprecation and migration tooling.
- The cost ratio is small now, large later.

That is exactly the cost-curve calculation the V1 contract commitment exists to capture. We spent the budget where it bought the most.

### Files touched (for posterity)

- `internal/config/config.go` — type, fields, YAML tag, validator error messages.
- `internal/auth/{banner,middleware}.go` and their `_test.go` siblings — field-access references and operator-facing banner/error strings.
- `internal/config/config_test.go`, `internal/config/yaml_error_test.go`, `internal/tools/manager_test.go`, `internal/semp/pool_test.go`, `test/integration/broker_credential_isolation_test.go`, `cmd/server/main_test.go` — inline YAML literals and struct/field references.
- `broker-config.example.yaml`, `deploy/kubernetes/configmap.yaml`, `test/oauth/test-config.yaml`, `test/rbac-prototype/broker-config-{static,oauth}.yaml` — operator-facing config files.
- `README.md`, `docs/configuration.md`, `docs/user-guide.md`, `docs/authentication.md` — operator-facing documentation.

Historical decision-log documents (`docs/superpowers/specs/2026-05-20-client-auth-mode-design.md`, `docs/superpowers/plans/2026-05-20-client-auth-mode.md`, etc.) intentionally retain their original `client_auth` references — they are records of past decisions, not current operator guidance.

---

## T2 — Naming principle: explicit names by default, reserve generic names for future use

**Date:** 2026-06-17
**Sub-ticket:** [SOL-150796](https://sol-jira.atlassian.net/browse/SOL-150796)

### The question

T2 made a series of rename decisions for fields inside `broker_oauth:`:

- `token_url` → `idp_token_url`
- `client_id` → `mcp_server_client_id`
- `client_auth` (nested discriminated-union block) → `mcp_server_client_auth`
- `audience_param` → `audience_parameter_name`
- `grant_type` — kept
- `secret` (inside `client_secret_basic`) — kept

The first round of reasoning was case-by-case: *"is this an OAuth-spec term every IdP uses verbatim, or a coined term operators can't look up?"* That rule worked for `audience_param` (coined → rename) and for `grant_type` (universal OAuth-spec term → keep), but it under-weighted two structural concerns that became visible only when the renames stacked. This section records the fuller principle so future schema decisions don't re-litigate it.

### Decision

The naming principle for V1 schema fields, in priority order:

1. **Use explicit, qualified names by default** — names that answer *"whose?"* or *"what for?"* in the field name itself.
2. **Keep generic/terse names only when** (a) the surrounding context fully answers the disambiguation question, or (b) the name is so locked by external specs (RFC, IANA registry) that it cannot realistically be misread.
3. **Treat generic names as a finite reservation** — every generic name we use today is one we cannot use for a future concept without a deprecation cycle.

### Why explicit by default

Two reinforcing reasons. Both compound over time, which is what makes the principle worth committing to.

**1. AI-assisted reading is now part of every operator workflow.** Configs are read and reasoned about by LLMs as much as by humans — operators paste their YAML into Claude / ChatGPT / Copilot to ask "why doesn't this work" or "generate a config for this scenario." For an LLM, *field names are the strongest signal*. A name like `client_id` forces the model to infer "whose?" from surroundings, often wrong. A name like `mcp_server_client_id` lets the model answer the question without inference, making AI-generated suggestions about our config more accurate and AI-driven debugging more reliable. This benefit applies on every interaction, indefinitely.

**2. Year-two clarity.** First-deploy operators have docs open. Year-two operators don't — they're upgrading, troubleshooting, or copying their old YAML into a new context. Explicit field names mean operators don't need docs nearby to understand what each field is for. This is the same principle behind self-documenting code in general, but it bites harder for config files than for code: code is read by people who chose to dig in; config is read by people who just want it to work.

### Why "reserve generic names for future use"

This is the structural argument the case-by-case rule missed. Once we use `client_id` to mean *"the MCP server's OAuth client_id at the IdP"*, we've committed to that meaning forever. We can't later use bare `client_id` for some other concept (the broker's identifier, a different OAuth flow's client_id, a non-OAuth use of "client_id") without naming collision and a deprecation cycle.

By taking the *qualified* name today (`mcp_server_client_id`), we **reserve the unqualified `client_id` for a future use case**. If a V2 feature introduces some other client_id-shaped concept, we can name it cleanly without colliding with V1. Generic names are scarce real estate; once spoken for, they're locked.

Same applies to:
- `token_url` (kept generic would lock it to the IdP token endpoint forever) → use `idp_token_url`, reserve `token_url`
- `client_auth` (top-level Hop 1 and nested Hop 2 already collided this way once) → use `mcp_server_client_auth`, reserve `client_auth`
- `audience_param` (was coined, also generic) → use `audience_parameter_name`, reserve any future `*_param` family

### What stays generic, and why

The principle's escape hatches:

- **`grant_type`** stays because it's an RFC 6749 / RFC 8693 spec term used verbatim by every IdP and library. It's not realistically going to mean anything else inside an OAuth context. The spec locks it; there's no future concept that would want to claim the name.
- **`secret`** stays because it's *inside* a named sub-block (`client_secret_basic`, `client_secret_post`) whose own name fully disambiguates. The context answers "whose secret?" before the field is even read.
- **`scopes`, `audience`** at the per-broker level stay because they're scoped under `brokers.<alias>.auth:`, where the parent path establishes "this is auth config for *this* broker." No further qualification needed.

### The trade-off the principle accepts

Explicit names are longer. Operators type more characters per config; reviewers read longer lines. We accept this cost because:

1. The typing happens once (first deploy); the reading happens forever.
2. AI-generated configs don't get tired typing longer names.
3. Reserved generic names are worth their weight when a future feature needs them.

### What this commits us to for V1 and beyond

- All four `broker_oauth:` field renames stand: `idp_token_url`, `mcp_server_client_id`, `mcp_server_client_auth`, `audience_parameter_name`.
- The principle (explicit by default; generic names reserved) applies to every future schema decision. If a future field is genuinely ambiguous without a qualifier, the qualifier goes in the name.
- A field name change inside V1's schema would be a breaking schema change. Therefore: when in doubt during design, lean explicit. Generic-to-explicit is hard later; explicit-to-generic is impossible.
