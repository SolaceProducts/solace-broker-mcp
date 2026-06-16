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
