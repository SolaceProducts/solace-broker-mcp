# Architecture Plan — Token Exchange (SOL-150070)

Status: Draft, in progress.
Scope of this document: Architectural decisions for the **Exchanger** and **TokenCache** components and the contract between them. Other components (InjectRawToken, Authenticator, IdPClient, dispatcher, config) will be added in follow-up sections as we make decisions about them.

This document captures *what we decided* and *why* — not just the final shape, but the reasoning that led there. The "why" is the part that's load-bearing during reviews and when revisiting decisions later.

---

## Context: The production reality we're designing for

The MCP server is deployed by enterprise customers — banks, airlines, regulated industries. These customers expect:

- **Per-component health, metrics, and logging** — SREs need to know exactly which component failed, not "something in the token path."
- **Blast-radius isolation** — an issue in caching should not require touching exchange code, and vice versa.
- **Independent rollback** — different components ship and roll back independently.
- **Auditability** — every action attributable to a specific component for compliance.

These operational guarantees, not aesthetics, are what drive the componentization decisions below. Monolithic "the Exchanger does everything" architecture fails every one of these tests.

---

## Decision 1: One IdP per deployment, not per broker

The prototype (`broker-mcp-poc/oauth-hop2`) implements one Exchanger per broker, on the assumption that different brokers may point to different IdPs. We are **not** adopting that.

**Decision:** A single IdP serves the whole deployment. The IdP's `token_url`, `client_id`, and `client_secret` are global config. Per-broker config holds only what differs per broker (audience, scopes).

**Why:**
- In practice, enterprise customers run one IdP for the deployment. Multi-IdP is a theoretical flexibility that adds config surface and operational complexity for a use case that doesn't exist in our target customer base.
- One IdP means one HTTP client, one connection pool, one TLS config, one set of timeouts — all configured once instead of per-broker.
- Simplifies wiring: one Exchanger singleton instead of N per-broker instances.

**Consequence:** The Exchanger becomes a **process-singleton**, same lifecycle story as `BrokerPool`. Per-broker concerns (audience, scopes) move from Exchanger state into per-call parameters supplied by the per-broker Authenticator.

---

## Decision 2: The Exchanger's duties

The Exchanger sits between "I have an agent's JWT" and "I have a broker-scoped token I can use." That is its only job. Concretely, its duties are:

1. **Build a valid RFC 8693 token-exchange HTTP request** — correct grant type, subject token, subject token type, client credentials, audience, scopes, requested token type.
2. **Authenticate to the IdP as the MCP server** — using `client_secret_post` (form body) — a single deployment-wide MCP identity.
3. **Send the request, parse the response** — honor `ctx`, read body, parse JSON, extract token + `expires_in`.
4. **Classify failures** into sentinel errors the upper layers can map to HTTP codes:
   - IdP-structured OAuth rejection → `ErrExchangeRejected` (401)
   - Network / timeout / 5xx → `ErrExchangeTransport` (503)
   - 2xx but unparseable → `ErrInvalidResponse` (502)
5. **Cache exchanged tokens** — delegate to a `TokenCache` component (see Decision 4).
6. **Deduplicate concurrent identical exchanges** with `singleflight` to prevent cold-cache stampedes against the IdP.
7. **Honor context** — every IdP call uses `http.NewRequestWithContext(ctx, ...)`. Caller cancellation propagates.
8. **Never leak secrets** — no raw `subject_token`, no `access_token`, no `client_secret` in any log line, error message, or metric label.

### What the Exchanger explicitly does *not* do

- Does **not** decide who is allowed to exchange — that's the IdP.
- Does **not** know about brokers — it takes `audience` and `scopes` as parameters.
- Does **not** validate the exchanged token — the broker does that on receipt.
- Does **not** know about sessions or user lifecycle.
- Does **not** retry IdP failures — out of scope (owned by SOL-147583).
- Does **not** refresh tokens proactively — re-exchange on cache miss is the model.
- **Does not own freshness logic.** See Decision 3.

---

## Decision 3: The Exchanger does NOT own caching policy

This is the most important architectural call in this document.

The prototype combines exchange logic and cache logic in one struct: the Exchanger holds the cache, builds keys, compares `expiresAt` against the clock with a skew, decides what counts as fresh, and writes back on success. **We are not adopting that.**

**Decision:** Cache concerns and exchange concerns are owned by two different components. The Exchanger holds the `TokenCache` by interface and calls only `Get`/`Put`/`Delete` on it. The Exchanger never compares timestamps, never decides freshness, never knows about eviction.

### Why this split (the reasoning, in full)

The two responsibilities have **fundamentally different lifecycle complexity**:

| Concern | Exchanger | Cache |
|---|---|---|
| Builds RFC 8693 request | ✅ | ❌ |
| Talks to IdP | ✅ | ❌ |
| Classifies IdP errors | ✅ | ❌ |
| Storage | ❌ | ✅ |
| Freshness / expiry | ❌ | ✅ |
| Eviction (forced or automatic) | ❌ | ✅ |
| Background sweepers, TTL, LRU, multi-tier | ❌ | ✅ |
| Metrics: hits, misses, size, pressure, eviction reasons | ❌ | ✅ |
| Health: backend reachability | ❌ | ✅ |

These columns change **for different reasons**. Changing the RFC version, the IdP error format, or the singleflight strategy touches the Exchanger. Changing the eviction policy, adding a sweeper, swapping in-memory for Redis, adding cache metrics, or tuning expiry skew touches the Cache. **Single Responsibility Principle says: different reasons to change ⇒ different components.**

### The future SOL-150052 cache will own real lifecycle behavior

SOL-150052 is producing the production cache. It will define:
- Eviction policy (LRU with max size)
- Scope-aware cache keys
- Background sweepers (possibly)
- Cache stats / metrics
- Possibly multi-tier (in-memory L1 + Redis L2)

**None of these should touch the Exchanger.** That's the test for whether our boundary is correct — and that's what motivates putting freshness behind the cache interface today, in the placeholder, even though the placeholder is simple.

### The blast-radius argument

Enterprise customers need to fix one component without touching others. If the Exchanger owns expiry math:

- Tuning the skew → modify Exchanger → re-test exchange logic.
- Adding a sweeper → modify Exchanger → re-test exchange logic.
- Switching to Redis → modify Exchanger → re-test exchange logic.

Every cache change ripples through exchange code, which means every cache change requires re-validating that we still do RFC 8693 correctly. That's not acceptable for a security-critical path under enterprise SLAs.

With the boundary in place:

- Tuning skew → Cache impl change. Exchanger untouched.
- Adding a sweeper → Cache impl change. Exchanger untouched.
- Switching to Redis → Cache impl change. Exchanger untouched.
- Changing RFC 8693 request format → Exchanger change. Cache untouched.

That's a real boundary.

### Where my earlier reasoning was wrong

In our initial discussion I argued the BrokerPool combines create+store and works fine, so combining exchange+cache should also be fine. **That comparison was wrong.** BrokerPool entries never expire, never get evicted, have no lifecycle. The pool is a *registry*, not a *cache*. Once you introduce expiry, force-eviction, background lifecycle, and a parallel team designing the eviction policy, the combined-component pattern breaks. The token cache is fundamentally different from a connection pool.

---

## Decision 4: The TokenCache interface (committed for SOL-150052 to honor)

This interface is the contract the Exchanger depends on **and** the contract SOL-150052 must implement. It must be right today, because changing it later breaks downstream consumers.

```go
type Token struct {
    Value     string
    ExpiresAt time.Time
    // Other fields (scope, token_type, etc.) added only if downstream code needs them.
}

type TokenCache interface {
    // Get returns a token only if one is stored AND still fresh by the cache's policy.
    // found=false means "no usable token here" — caller does not distinguish missing vs expired.
    // err is for backend failures (e.g., Redis down), not for "not found".
    Get(ctx context.Context, key string) (token Token, found bool, err error)

    // Put stores a token. The cache uses token.ExpiresAt to decide freshness on future Gets.
    Put(ctx context.Context, key string, token Token) error

    // Delete forcibly evicts a key. Used for known-bad token paths (e.g., broker 401 — future scope).
    Delete(ctx context.Context, key string) error
}
```

### Design choices baked into this interface, and why

1. **`ctx` on every method.** A Redis or other network-backed implementation needs cancellation/deadline support. The in-memory placeholder ignores `ctx` harmlessly. Adding `ctx` later means every caller signature changes — must commit now.

2. **`error` on every method.** Same reasoning. Network caches fail. The placeholder returns `nil` errors but the type is there.

3. **`Get` returns only fresh tokens.** `found=false` covers both "never stored" and "stored but expired" — caller does not care which. This is the *single most important* line of the contract: it puts freshness entirely behind the interface. Once the caller can't ask "is this expired?", the caller can't make freshness decisions, and the boundary holds.

4. **`Put` accepts a `Token` that carries its own `ExpiresAt`.** The cache reads `ExpiresAt` and applies its own skew/policy. The Exchanger does not compute "now + expires_in - skew" — it just hands off what the IdP told it.

5. **`Delete` is exposed** so a future code path (broker 401, future story) can force eviction. The Exchanger itself does not call `Delete` in v1 — but the contract supports it cleanly.

### What is deliberately NOT on the interface

- **No `IsExpired(token)` helper.** The moment a caller can ask this, they'll start making freshness decisions themselves and the boundary collapses.
- **No `ExpiresAt()` getter.** Same reason.
- **No iteration / `Keys()` / `Size()`.** Out of scope for callers. Cache impl may expose this for its own metrics, but not on the public interface.
- **No batch operations (`GetMulti`, etc.).** YAGNI. Add only if a real caller needs it.

### Coordination with SOL-150052

This interface is what Wajiha must honor. Before the placeholder lands, this interface should be reviewed with her (or at minimum surfaced as the contract she's expected to implement). If she has design constraints that affect the interface shape (e.g., scope-aware keys requiring caller participation), we need to know now, not after the swap.

---

## Decision 5: The Exchanger's shape, post-decisions

Given the above, the Exchanger struct is:

```go
type Exchanger struct {
    // Deployment-global config — set once at construction, never written.
    tokenURL           string
    clientID           string
    clientSecret       string
    requestedTokenType string

    // Shared dependencies, injected at construction.
    httpClient *http.Client
    cache      TokenCache
    group      *singleflight.Group
}
```

Its public method:

```go
func (e *Exchanger) Exchange(
    ctx context.Context,
    subjectToken string,
    audience string,
    scopes []string,
) (Token, error)
```

Per-call parameters (`audience`, `scopes`) come from the per-broker Authenticator that calls the Exchanger. The Exchanger holds nothing broker-specific.

### Cache key construction

Pure function, single source of truth, deterministic:

```go
func buildKey(subjectToken, audience string, scopes []string, requestedTokenType string) string {
    // sha256(subjectToken) || audience || sorted-joined(scopes) || requestedTokenType
}
```

**Every field that affects the IdP's response must be in the key.** The prototype keys on `sha256(token) || audience` only — that's a bug. If two callers ask for the same `(token, audience)` but different scopes, they should get different exchanged tokens; a key that ignores scopes would return the wrong cached entry.

The singleflight key is the **same** cache key. They must not drift.

### Internal organization (decomposition for readability, not concerns)

Inside `Exchange`, factor for readability:

- `buildKey(...)` — pure function, package-private
- `e.buildIdPRequest(ctx, ...) (*http.Request, error)` — RFC 8693 form construction
- `e.parseIdPResponse(resp) (Token, error)` — JSON parse + error classification
- `e.callIdP(ctx, ...) (Token, error)` — orchestrates buildIdPRequest → httpClient.Do → parseIdPResponse

These are **private methods on the same struct** — refactoring for readability, not separation of concerns. They all stay inside the Exchanger because they all change for the same reason ("how we talk to the IdP").

`Exchange` itself is then:

```go
func (e *Exchanger) Exchange(ctx, subjectToken, audience, scopes) (Token, error) {
    key := buildKey(subjectToken, audience, scopes, e.requestedTokenType)

    if tok, ok, err := e.cache.Get(ctx, key); err == nil && ok {
        return tok, nil
    }

    v, err, _ := e.group.Do(key, func() (any, error) {
        if tok, ok, err := e.cache.Get(ctx, key); err == nil && ok {
            return tok, nil // double-check after winning singleflight slot
        }
        tok, err := e.callIdP(ctx, subjectToken, audience, scopes)
        if err != nil {
            return nil, err
        }
        _ = e.cache.Put(ctx, key, tok)
        return tok, nil
    })
    if err != nil {
        return Token{}, err
    }
    return v.(Token), nil
}
```

Three responsibilities visible: build key, try cache → singleflight → IdP, store on success. The Exchanger does not check expiry, compare timestamps, evict, or know what "fresh" means. It trusts the cache.

---

## Decision 6: Observability is part of every component's contract

This is not a "later" concern. Enterprise customers require per-component observability from day one. For the two components covered in this document:

### Exchanger metrics (minimum)

- `mcp_oauth_exchange_total{audience, result}` — counter; `result` ∈ {success, rejected, transport_error, invalid_response}
- `mcp_oauth_exchange_duration_seconds{audience}` — histogram, IdP round-trip wall time
- `mcp_oauth_exchange_singleflight_dedup_total{audience}` — counter, callers who joined an in-flight exchange

### Exchanger logging

- One log line per exchange attempt with correlation ID — never logs `subjectToken`, `access_token`, `client_secret`.
- Errors logged at WARN (rejected) or ERROR (transport, invalid_response) with sentinel error type.

### Cache metrics (minimum, on the placeholder)

- `mcp_oauth_cache_get_total{result}` — `result` ∈ {hit, miss}
- `mcp_oauth_cache_put_total`
- `mcp_oauth_cache_delete_total`
- `mcp_oauth_cache_size` — gauge

(The placeholder will be minimal but emit these so dashboards built today work unchanged when SOL-150052's real cache lands.)

### Cache logging

- Eviction reasons (when SOL-150052's cache lands).
- Backend health transitions (when a network-backed cache lands).
- Placeholder logs nothing per-operation — would be noise.

### Convention

Match the project's existing convention (slog structured logging — see `docs/internal/secure-logging-rules.md`). Verify whether the project emits Prometheus metrics today; if not, this becomes part of scope or a follow-up.

---

## Summary diagram (Exchanger ↔ Cache boundary)

```
                  ┌─────────────────────────────────┐
                  │  Per-broker Authenticator       │
                  │  (audience, scopes, ref to Ex.) │
                  └────────────────┬────────────────┘
                                   │ Exchange(ctx, subjectToken, audience, scopes)
                                   ▼
        ┌──────────────────────────────────────────────────────┐
        │  Exchanger (singleton)                               │
        │  ───────────────────────────────────────────────     │
        │  • buildKey  — pure function                         │
        │  • cache.Get(ctx, key) — trusts cache for freshness  │
        │  • singleflight.Do(key, fn)                          │
        │  • callIdP(ctx, …) — RFC 8693 over HTTP              │
        │  • cache.Put(ctx, key, token) — on success           │
        │                                                      │
        │  Owns: RFC 8693 mechanics, error classification,     │
        │        singleflight, IdP HTTP client                 │
        │  Doesn't own: anything about freshness or eviction   │
        └──────────────────────────────┬───────────────────────┘
                                       │ TokenCache interface
                                       │   Get(ctx, key) → (Token, found, err)
                                       │   Put(ctx, key, Token) → err
                                       │   Delete(ctx, key) → err
                                       ▼
        ┌──────────────────────────────────────────────────────┐
        │  TokenCache (interface)                              │
        │  ───────────────────────────────────────────────     │
        │  v1: Placeholder in-memory impl (this story)         │
        │  v2: Production impl (SOL-150052) — LRU, eviction,   │
        │      possibly sweeper, possibly Redis, etc.          │
        │                                                      │
        │  Owns: storage, freshness, expiry, eviction,         │
        │        background lifecycle, cache metrics           │
        │  Exchanger never sees the difference between v1/v2.  │
        └──────────────────────────────────────────────────────┘
```

---

## Open items (to be resolved before/during implementation)

1. **Scopes: per-broker or global?** Decision pending. Default to per-broker (superset of global) unless deliberate reason otherwise.
2. **Cache contract review with Wajiha (SOL-150052 owner).** The `TokenCache` interface above is what she must honor. Surface it to her before the placeholder lands.
3. **Project metrics convention** — verify Prometheus vs OTel vs slog-only. Affects metric names above.
4. **Project health-endpoint pattern** — does it exist? If yes, both components should be capable of contributing readiness.
5. **Context deadline propagation** — verify that the per-request `ctx` arriving at the Exchanger already has a deadline set by the MCP layer; if not, the Exchanger needs its own timeout config.
6. **Components not yet decided in this doc:** InjectRawToken middleware, IdPClient (whether to split from Exchanger), AddAuth dispatcher, config schema. To be added in follow-up sections.

---

## Decision 7: The Authenticator — per-broker strategy object, constructed once

The `Authenticator` is the per-broker object that knows how to attach the right `Authorization` header to an outbound SEMP request. It is the seam between the SEMP transport layer and whatever auth scheme a given broker uses — the SEMP code never branches on auth mode; it just calls `authenticator.AddAuth(ctx, req)` and polymorphism does the rest.

### The contract (already exists in production for basic/bearer)

```go
type Authenticator interface {
    AddAuth(ctx context.Context, req *http.Request) error
}
```

One method. "Mutate this request to be authenticated, or return an error."

Three implementations the dispatcher returns:

| Implementation | What it does | Fields held |
|---|---|---|
| `BasicAuthenticator` | `req.SetBasicAuth(username, password)` | username, password |
| `BearerAuthenticator` | sets `Authorization: Bearer <token>` | static token |
| `OAuthAuthenticator` (this story) | calls `Exchanger.Exchange(...)`, attaches result | audience, scopes, `*Exchanger` ptr |

Same lifecycle, same interface, different implementations. Adding `oauth` is a **non-invasive change** — the SEMP client doesn't change. Only a new branch in the dispatcher and a new struct.

### Decision: One Authenticator per broker

**Decision:** Each broker gets exactly one Authenticator instance, constructed once when that broker's `BrokerClient` is constructed, attached as a field on the `BrokerClient`, reused for the life of the process.

Construction happens lazily — at the moment a broker is first requested via `pool.getOrCreate(alias)`, the pool calls `NewBrokerClient`, which builds the Authenticator alongside the protocol clients. The Authenticator is a sibling of the broker client: born together, attached, cached together. Same lifecycle story as everything else in §8 of the walkthrough — "long-lived service objects, short-lived request objects."

**Why per-broker, not per-call:**

- Per-broker config (audience, scopes) is **stable for the life of the process**. There is nothing to reconstruct per call.
- Per-request allocations are wasted work in the hot path. Construct once, reuse millions of times.
- Born-once construction is where startup validation happens — "this broker says `auth_mode: oauth` but no audience" → fail at construction, not at request 1000.

**Why per-broker, not global:**

- Different brokers can have different audiences and (likely) different scopes. The Authenticator holds those values.
- The dispatcher returns *one* of three concrete implementations based on each broker's config — a global Authenticator would have to know all brokers' configs, which collapses the polymorphism.

### Decision: The Authenticator holds runtime-wired state, NOT the config struct

The broker config (`BrokerConfig.Auth.OAuth.Audience`, `.Scopes`) already holds these values. The Authenticator is **not duplicating** that data — it holds a runtime-ready, construction-time-captured view alongside its wired-up dependencies.

```go
type OAuthAuthenticator struct {
    exchanger *Exchanger  // wired runtime dependency — NOT in config
    audience  string      // captured from config at construction
    scopes    []string    // captured from config at construction
}
```

**Why this is not redundant with the config:**

- **Different layers see different things.** The SEMP client only knows the `Authenticator` interface. It does not (and should not) know about the config layer, YAML, or how OAuth works. The Authenticator is what bridges "broker config has these values" and "SEMP code calls `AddAuth`."
- **The Authenticator holds more than just config values.** The `*Exchanger` pointer is **not** in the config — it's a runtime dependency wired up at startup. The Authenticator is the **composition point** where per-broker config and the global Exchanger come together.
- **Polymorphism breaks without it.** Without the Authenticator absorbing OAuth-specific work, the SEMP client would have to know every auth mode, hold every auth mode's dependencies (Exchanger, possibly Cache), and branch on mode at every call site. The Authenticator pattern keeps OAuth knowledge out of the SEMP transport layer entirely.

This is the canonical "config vs. wired objects" two-stage pattern:

1. **Config layer** — passive structs loaded from YAML. Pure data. No behavior.
2. **Wired layer** — runtime objects built from config + shared dependencies. Behavior. Reused by the rest of the system.

The config is the blueprint. The Authenticator is the machine built from the blueprint. The SEMP client is the worker. The worker doesn't read the blueprint; the worker operates the machine.

### Decision: The OAuthAuthenticator is concurrency-safe even though OAuth work is user-specific

This was the most subtle decision point. The Authenticator is **shared across all concurrent users hitting the same broker** — but each user gets their own per-user broker token. This works because:

```go
func (a *OAuthAuthenticator) AddAuth(ctx context.Context, req *http.Request) error {
    subjectToken, ok := auth.RawTokenFromContext(ctx)  // per-request, from ctx
    if !ok { return ErrNoAgentToken }
    brokerToken, err := a.exchanger.Exchange(ctx, subjectToken, a.audience, a.scopes)
    if err != nil { return fmt.Errorf("oauth authenticator: %w", err) }
    req.Header.Set("Authorization", "Bearer "+brokerToken.Value)  // per-request mutation
    return nil
}
```

**The key insight: the Authenticator holds no per-user state.**

| Field | Stable for the process? |
|---|---|
| `*Exchanger` (pointer) | ✅ Same singleton always |
| `audience` (string) | ✅ Fixed for this broker |
| `scopes` ([]string) | ✅ Fixed for this broker |

The **user-specific** input (the raw JWT) does NOT live on the Authenticator. It lives in `ctx`, which is passed in fresh on every call. The Authenticator can serve 1000 concurrent users from 1000 different goroutines because the user information flows *through* the method call, not *into* the object's fields.

By the §17 / §18 framework from the concurrency walkthrough:

| What | Category | Synchronization |
|---|---|---|
| `AddAuth` machine code | Hardware-immutable (`.text`) | None needed |
| Authenticator struct fields | Effectively immutable (set at construction, never written) | None needed |
| Per-call locals (`subjectToken`, `brokerToken`, `err`) | Per-call private (stack or fresh heap) | None needed |
| `a.exchanger.Exchange(...)` call | Delegated to Exchanger — see Decision 5 | Exchanger handles via its own discipline |

Every memory access falls into a safe category. The OAuthAuthenticator is goroutine-safe **by construction**, not by mutex.

### Decision: Singleflight at the Exchanger correctly partitions by user

The OAuthAuthenticator delegates to the Exchanger, which uses singleflight keyed on `sha256(subjectToken) || audience || sorted(scopes) || requestedTokenType`. This means:

- Concurrent requests from the **same** user to the **same** broker share one singleflight slot → one IdP round-trip, result fanned out to all callers.
- Concurrent requests from **different** users to the **same** broker have **different keys** → independent singleflight slots → independent IdP round-trips → no cross-user contamination.

There is no global "one goroutine wins" — there is one election *per key*, happening independently in parallel. User A's exchange result cannot be returned to user B because their cache keys (and therefore their singleflight slots) are different, derived from `sha256` of distinct subject tokens.

This is the **security invariant** that lets a single shared Authenticator + Exchanger safely serve multiple users at once. The cache key's inclusion of `sha256(subjectToken)` is not an optimization — it is what makes per-user correctness possible.

### Decision: Per-broker observability is the Authenticator's job

Per-component metrics from the architecture map (`component-map.md`):

- `mcp_auth_attempts_total{broker, result}` — counter; `result` ∈ {success, no_subject_token, exchange_rejected, exchange_transport_error}
- `mcp_auth_attempts_duration_seconds{broker}` — histogram (includes time spent in Exchanger)

Why per-broker labels here (and per-audience labels at the Exchanger):

- The Authenticator's `broker` label answers operational questions like "which broker is causing auth failures?"
- The Exchanger's `audience` label answers IdP-side questions like "is the IdP rejecting this audience?"
- Different dimensions, both useful, neither duplicates the other.

The Authenticator never logs the subject token or the exchanged token. Errors are logged at WARN (rejected) or ERROR (transport) with sentinel error types only.

### The Authenticator's responsibilities, named

1. **Hold broker-specific OAuth config** (audience, scopes) captured at construction.
2. **Pull the raw JWT from ctx** — bridges Hop 1 (`InjectRawToken`'s ctx value) to Hop 2 (Exchanger's input).
3. **Invoke the Exchanger** with the right per-broker parameters.
4. **Attach the resulting token** as `Authorization: Bearer <token>` on the outbound request.
5. **Map "no agent token in ctx" to a clean sentinel error** so the SEMP layer can fail cleanly.

That is all. It is deliberately thin. Anything more belongs in the Exchanger (exchange mechanics), the Cache (storage policy), the dispatcher (wiring), or the config (declaration).

### What the OAuthAuthenticator explicitly does *not* do

- Does **not** own its own cache or HTTP client. It borrows the Exchanger (which owns those).
- Does **not** retry — out of scope (Exchanger maps errors, SEMP layer decides response).
- Does **not** know about users — it knows about `ctx`. Per-user behavior emerges from per-request `ctx`.
- Does **not** validate that the exchanged token is "good." The broker validates on receipt.
- Does **not** hold a reference to `BrokerConfig`. It captures the strings it needs at construction and forgets the source.

### Lifecycle summary

```
t0   Process starts. Exchanger constructed (singleton). Pool empty.
t1   First request for broker-c arrives. pool.getOrCreate runs under write lock:
       client := NewBrokerClient(alias, brokerCfg, sempCfg)
         // inside NewBrokerClient: authn := auth.NewAuthenticator(brokerCfg.Auth)
         // (when T6 lands, NewAuthenticator also takes the *Exchanger)
       pool.clients["broker-c"] = client
t2…  Every subsequent request for broker-c reuses the same client and the same
     authn. authn.AddAuth(ctx, req) is called concurrently from many G6's;
     each gets its own ctx (its own user's raw JWT), each ends up with its own
     exchanged broker token attached to its own req. Authenticator state is
     never mutated.
tEnd Process exits. Authenticator garbage-collected with its BrokerClient.
```

### Principle captured

> **An object can be a singleton (or per-broker reusable) and still produce per-call results, as long as the per-call inputs are passed in, not stored. Sharing the *object* is sharing the *behavior*, not the *state*. Per-user safety comes from never letting per-user data become a field.**

This is the same principle that makes `*http.Client`, `*sql.DB`, and `CompositeToolHandler` (walkthrough §11–12) safe to share. The Authenticator joins that family.

### Addendum: One Authenticator per broker, shared by v1 and v2 protocol clients

Surfaced during production recon (no `Authenticator` interface exists today; auth is a package-level function `auth.AddAuth(ctx, req, cfg)` called from both `sempv1.HTTPClient` and `sempv2.HTTPClient`). The recon raised the question: when we introduce the interface, do v1 and v2 each construct and hold their own Authenticator, or do they share one?

**Decision: one `Authenticator` instance per broker, shared by that broker's SEMPv1 and SEMPv2 protocol clients via pointer.**

**Why sharing is correct:**

- The Authenticator's concern is "how to authenticate to *this broker*." Both protocol clients on the same broker have identical authentication requirements. Two Authenticator instances would be functionally identical but allocated twice — wasteful and asymmetry-prone.
- Per-broker (not per-protocol-client) is the correct unit of an Authenticator's existence. The unit of an object should match the unit of its concern.
- Sharing strengthens the goroutine-safety story: one object, no per-user state, used concurrently by both protocol clients' request paths.

### Addendum: The Authenticator field lives on `BrokerClient`, protocol clients borrow a reference

**Decision: the `Authenticator` is a field on `BrokerClient`. SEMPv1 and SEMPv2 protocol clients receive a borrowed reference (pointer) via their constructors.**

```go
type BrokerClient struct {
    sempV1Client  *sempv1.HTTPClient
    sempV2Client  *sempv2.HTTPClient
    authenticator auth.Authenticator   // ← OWNED HERE; shared with v1 + v2 by pointer
    alias         string
}
```

**Why on `BrokerClient` rather than only on the protocol clients:**

- **Discoverability** — reading `BrokerClient`'s definition immediately shows what auth scheme this broker uses, without having to drill into either protocol client.
- **Single source of truth for "this broker's auth"** — there is no ambiguity about which copy is authoritative because there is no copy.
- **Future protocol clients** — if a third SEMP variant (or a new protocol entirely) is added later, it borrows the same `authenticator` field. No reshape needed.
- **Symmetry with construction** — the Authenticator is born inside `NewBrokerClient` alongside the protocol clients, on the lazy path triggered by `pool.getOrCreate`. It belongs to the BrokerClient's lifecycle. Putting it as a field on the BrokerClient matches that lifecycle exactly.

**Why protocol clients still hold a reference rather than reaching up to the parent:**

- **Loose coupling** — `sempv1.HTTPClient` and `sempv2.HTTPClient` do not need to know they have a "parent" `BrokerClient`. They only need an `auth.Authenticator`. Constructor injection keeps the protocol clients ignorant of the broader assembly.
- **Testability** — protocol clients can be tested with a hand-built `Authenticator` (e.g., a fake) without constructing a full `BrokerClient`.
- **Standard Go style** — dependencies are passed in via constructor parameters, not read from a parent pointer at runtime.

### Construction site (where the Authenticator is born)

The Authenticator is constructed inside `internal/semp/broker.go`'s `NewBrokerClient`, alongside the per-broker semaphore and the protocol clients themselves. The pool's `getOrCreate` is the caller that triggers construction (lazily, on first touch of a broker alias), but `NewBrokerClient` is the **single builder** — no other code path calls `auth.NewAuthenticator`.

```go
// internal/semp/broker.go
func NewBrokerClient(alias string, brokerCfg *config.BrokerConfig, sempCfg *config.SEMPConfig) (*BrokerClient, error) {
    // Single builder: one Authenticator per broker, shared by both protocol clients.
    authn, err := auth.NewAuthenticator(brokerCfg.Auth)
    if err != nil {
        return nil, fmt.Errorf("creating authenticator for broker %q: %w", alias, err)
    }

    sem := resilience.NewSemaphore(sempCfg.MaxConcurrentPerBroker)
    sempV1Client, err := sempv1.NewHTTPClient(brokerCfg, sempCfg, sem, authn)
    if err != nil { return nil, err }
    sempV2Client, err := sempv2.NewHTTPClient(brokerCfg, sempCfg, sem, authn)
    if err != nil { return nil, err }

    return &BrokerClient{
        sempV1Client:  sempV1Client,
        sempV2Client:  sempV2Client,
        authenticator: authn, // pre-staged for the T7b Sender migration
        alias:         alias,
    }, nil
}
```

The pool's role is unchanged from before this refactor: it caches one BrokerClient per alias under a write lock, calling `NewBrokerClient` once on first touch. The pool does not know about Authenticators — it just hands `brokerCfg` to `NewBrokerClient` and stores the result.

When T6 (OAuth) lands, `NewAuthenticator` will grow additional parameters (`*Exchanger`, per-broker cookie jar). The only production call site that changes is the one in `NewBrokerClient`; the pool stays clean.

`NewBrokerClient` passes the same `authn` pointer to both `sempv1.NewHTTPClient` and `sempv2.NewHTTPClient`. Both protocol clients store the pointer in their struct's `authenticator` field. The per-request hot path becomes:

```go
// in sempv1/client.go and sempv2/client.go, before sender.Do(...)
if err := c.authenticator.AddAuth(ctx, req); err != nil {
    return nil, fmt.Errorf("applying auth for %s: %w", op.ID, err)
}
```

Symmetric, uniform, one object across both protocol layers.

### Ownership and lifetime invariants (the verbalized contract)

| Invariant | Why it matters |
|---|---|
| Exactly one `Authenticator` is constructed per broker | No duplication, no risk of one drifting from the other |
| Construction happens inside `NewBrokerClient`, never anywhere else | Single source of "which auth scheme for this broker" — pool and protocol clients never call `auth.NewAuthenticator` |
| The `Authenticator` is owned by the `BrokerClient` (struct field) | Lifetime tied to the broker; dies with the BrokerClient |
| Protocol clients (`sempv1`, `sempv2`) hold a borrowed reference (pointer) | Loose coupling, easy testing, future protocols compose cleanly |
| The Authenticator's fields are never written after construction (Decision 7's safety invariant) | Goroutine-safety remains structural, not by mutex |
| Per-user data never lives on the Authenticator; it flows via `ctx` on each `AddAuth` call | The whole concurrency model depends on this — see Decision 8 |

These six invariants are the code-review checklist for any PR that touches Authenticator wiring.

---

## Decision 8: Edge cases and concurrency invariants explored

This section captures the edge cases and "is this actually safe?" questions raised during the Authenticator design discussion. Each is recorded with the concern, the resolution, and the principle that makes the resolution sound. These are not hypothetical — they are the actual scenarios that need to be valid for the architecture to hold under enterprise concurrent load.

### Edge case 1: "OAuth Authenticator looks different from Basic/Bearer — should it be per-request instead of per-broker?"

**Concern raised:** Basic and bearer auth use static credentials, so an Authenticator created once at broker-client construction time is fine. But OAuth produces a *per-user* exchanged token. Doesn't that mean the OAuthAuthenticator must be created per request?

**Resolution:** No. **Per-request work ≠ per-request object.** The OAuthAuthenticator object is constructed once per broker. Its `AddAuth` method does per-request work because it reads `ctx` (which carries the per-user subject token) on every call. The object has no per-user fields.

**Why this works:**

| Field on OAuthAuthenticator | Stable for the process? |
|---|---|
| `*Exchanger` (pointer) | ✅ Same singleton always |
| `audience` (string) | ✅ Fixed for this broker |
| `scopes` ([]string) | ✅ Fixed for this broker |

Per-user data (subject token) does NOT live on the Authenticator — it lives in `ctx` and flows in via the method signature. The Authenticator is a place to hang the constants between calls. The actual auth happens entirely in the method invocation, with per-call inputs.

If the OAuthAuthenticator had `lastUser`, `lastToken`, or any per-user mutable field, this design would be unsafe — and we would have had to construct one per request. **The design choice that makes per-broker construction safe is: hold no per-user state.**

**Principle:**

> An object can be shared and still produce per-call results, as long as per-call inputs are passed in (not stored). Sharing the *object* is sharing the *behavior*, not the *state*.

This is the same property that makes `*http.Client` and `*sql.DB` safe to share across goroutines.

### Edge case 2: "Is `Exchanger.Exchange` actually safe to call concurrently from many G6's?"

**Concern raised:** The same `Exchanger.Exchange` is called from every per-request goroutine (G6). Even though the instructions are the same, is there real hardware-level isolation between concurrent calls?

**Resolution:** Yes. By the §17 framework from the concurrency walkthrough:

| What | Category | Why safe |
|---|---|---|
| `Exchange` machine code | Hardware-immutable (`.text`) | Kernel-enforced read-only page; CPU cannot write |
| Exchanger struct fields (`tokenURL`, `clientID`, `clientSecret`, `requestedTokenType`, `httpClient`, `cache`, `group`) | Effectively immutable | Written once at construction, never written again; concurrent reads of stable memory are always safe |
| Per-call locals (`key`, `tok`, `err`, the singleflight closure) | Per-call private (stack or fresh heap) | Each call gets its own allocation; no aliasing across goroutines |
| `e.cache.Get/Put/Delete` | Delegated | Cache impl's responsibility |
| `e.group.Do` | Delegated | `golang.org/x/sync/singleflight` is documented goroutine-safe |
| `e.httpClient.Do` | Delegated | stdlib documents `*http.Client` as goroutine-safe |

Every memory access falls into a safe category. **Exchanger is goroutine-safe by construction**, not by mutex on its own fields.

### Edge case 3: "Per-G6 stack isolation — does the model hold for `Exchange`?"

**Concern raised:** Each G6 has its own stack. When G6_1 and G6_2 both call `Exchange`, each gets its own stack frame for that call. Are the locals (key, tok, etc.) genuinely isolated by virtue of being on different stacks?

**Resolution:** Yes — with one refinement around escape analysis.

The naive model says: "G6_1's `key` and G6_2's `key` live on different stacks at different memory addresses. No interference possible." That model is correct for the simple case.

**Refinement — escape analysis:** Not every local stays on the stack. The Go compiler runs *escape analysis* and decides per-variable whether it can remain on the stack or must move to the heap. A variable escapes if any pointer to it outlives the function — for example, the closure passed to `singleflight.Group.Do` escapes because singleflight stores it internally until completion. Captures of that closure (`key`, `ctx`, `subjectToken`, `audience`, `scopes`) escape with it.

**Does escape break the isolation argument?** No, but it refines the *mechanism*:

> Per-call locals live somewhere — stack if they don't escape, heap if they do — but in either case, they are freshly allocated for each call and not aliased by any other goroutine. Isolation is preserved; it is enforced by "fresh allocation per call," not strictly by "stack frame per call."

This is exactly the "per-call private" row in §17's table. Hardware-level isolation for non-escaping locals; heap-allocation isolation for escaping ones. Either way, no goroutine can see another's per-call state.

### Edge case 4: "Singleflight elects one goroutine — does that randomly pick a winner globally and cause cross-user contamination?"

**Concern raised:** When 10 requests with subject token T1 arrive at the same time as 20 requests with subject token T2 (both for the same broker), the "singleflight elects one goroutine to do the work" description sounds like a global lottery. If so, could user T1's exchange result be handed to user T2's request?

**Resolution:** No. **Singleflight is scoped by the key you give it, not globally.**

The cache key (and the singleflight key, which is the same key) is:

```
sha256(subjectToken) || audience || sorted(scopes) || requestedTokenType
```

Two callers see the same key if and only if **every** field above is the same. Different users have different subject tokens → different `sha256` results → different keys → different singleflight slots, completely independent.

**Walkthrough of the scenario:**

```
t=0  10 G6's start Exchange with T1; 20 G6's start with T2. All 30 compute keys:
       - First 10  → K1 = sha256(T1) || aud(B) || sorted(S) || access_token
       - Other 20  → K2 = sha256(T2) || aud(B) || sorted(S) || access_token
     K1 ≠ K2 (different sha256 hashes).

t=1  All 30 hit cache.Get. Both K1 and K2 are cold → all 30 cache misses.

t=2  Singleflight phase. Two SEPARATE elections happen in parallel:
       - Of the 10 with K1, one G6 (G6_a) wins K1's slot. The other 9 block on K1.
       - Of the 20 with K2, one G6 (G6_b) wins K2's slot. The other 19 block on K2.
     G6_a and G6_b run CONCURRENTLY. They are in different singleflight slots.
     The IdP receives TWO independent token-exchange POSTs simultaneously.

t=3  IdP responds:
       - G6_a gets tokenForT1, writes cache[K1] = tokenForT1, returns tokenForT1.
       - G6_b gets tokenForT2, writes cache[K2] = tokenForT2, returns tokenForT2.

t=4  The 9 waiters on K1 unblock with tokenForT1.
     The 19 waiters on K2 unblock with tokenForT2.

t=5  All 30 requests proceed downstream with the correct user-specific token.
     User T1's 10 requests use tokenForT1.
     User T2's 20 requests use tokenForT2.
     They never see each other's tokens. Ever.
```

The phrase "singleflight elects one goroutine" means **one per key**, not one globally. 50 different keys = 50 independent elections happening in parallel = 50 chosen goroutines all running concurrently. No global coordinator.

Mental model of singleflight: a map of `key → (in-flight work, waiters list)`. Two goroutines with different keys never even contend on the same entry. They take the map's internal lock briefly to insert, then operate on independent state.

### Edge case 5: The three layers of cross-user isolation

The architecture has **defense in depth** against any path where user A's exchanged token could leak to user B's request. All three layers would have to fail simultaneously for cross-user contamination — and each layer alone is sufficient.

| Layer | Mechanism | What it isolates |
|---|---|---|
| 1 | **Cache key includes `sha256(subjectToken)`** | Different users → different keys → different cache entries. Hardware-level isolation via SHA-256 collision resistance. |
| 2 | **Singleflight slots are per-key** | Two pending closures keyed K_A and K_B are separate entries in singleflight's internal map. The goroutine running K_A's closure has no access to K_B's waiters. Isolation by data structure. |
| 3 | **Closure captures are per-call** | Each singleflight slot's closure captures *its own copy* of `subjectToken`, `ctx`, etc. Closures are isolated by Go's closure-capture semantics + heap escape behavior. |

**Conclusion:** There is no plausible code path where one user's request receives another user's exchanged token. The cache key's inclusion of `sha256(subjectToken)` is not an optimization — it is the **security invariant** that makes shared singleton components (Exchanger, Authenticator) safe under multi-user concurrent load.

### Edge case 6: What does the IdP load actually look like?

Given the architecture, the IdP load is bounded by **distinct `(user, broker, scopes)` tuples currently being exchanged**, not by total request volume. Cache hits eliminate most of this in steady state.

| Scenario | IdP load |
|---|---|
| One user, 100 concurrent requests, one broker | 1 exchange call (singleflight collapses) |
| 100 users, 1 request each, one broker | 100 exchange calls (different keys, no dedup possible — and none desired; they're genuinely different exchanges) |
| One user, 100 concurrent requests across 5 brokers (20 per broker) | 5 exchange calls (one per broker, same user) |
| 100 users × 5 brokers, all concurrent, cold cache | Up to 500 concurrent IdP calls (modulo HTTP connection pool limits in the shared `*http.Client`) |
| Steady state, warm cache | Approaches 0 — most calls are cache hits |

This shape is acceptable for the target deployment. If the IdP ever becomes a bottleneck, the connection-pool config in the shared `*http.Transport` is the first knob to tune. Circuit-breaking and IdP failover are out of scope for SOL-150070 (owned by SOL-147583).

### Edge case 7: Singleflight cancellation footgun (known consideration, not blocking)

**The sharp edge:** The G6 that wins a singleflight slot is the one that executes the closure (and therefore the IdP HTTP call). If that G6's `ctx` is canceled (e.g., its originating client disconnects) mid-IdP-call, the IdP call returns a context error → the closure returns that error → **all the other waiters on that key receive the same context error**, even though their own `ctx` values are perfectly healthy.

**Consequence:** A single canceling client can fail the in-progress exchange for everyone else waiting on the same key. Those waiters' next attempts will succeed (one of them becomes the new singleflight winner), but the current batch fails.

**Why this is not blocking SOL-150070:**

- Cache hits are the common case in steady state; singleflight only matters on cold misses.
- The blast radius is bounded: only callers waiting on the **same** key at the **same moment** are affected. Different users (different keys) are unaffected.
- The next requests recover automatically.

**Future mitigation (if ever observed in production):** wrap the singleflight closure to run with a context derived from the singleton (not tied to any single caller's ctx). This decouples in-flight work from a single client's cancellation. Out of scope for now — recorded so reviewers and future maintainers know the consideration exists.

### Edge case 8: Config vs. Authenticator — is the Authenticator duplicating the broker config?

**Concern raised:** `BrokerConfig.Auth.OAuth.Audience` and `.Scopes` already exist in config. Doesn't holding those same values on the OAuthAuthenticator duplicate state?

**Resolution:** No. The Authenticator is **not** duplicating data; it is holding a runtime-ready, construction-time-captured view of the relevant subset, alongside its wired-up dependencies. Already covered in Decision 7 — recorded here as an explicit edge case for completeness:

- **Config = blueprint.** Passive data, declarative, no behavior.
- **Authenticator = machine built from the blueprint.** Runtime object with wired dependencies and behavior.
- **SEMP client = worker.** Uses the machine, never touches the blueprint.

This is the canonical "config vs. wired objects" two-stage pattern. Mixing them ("just pass `BrokerConfig` everywhere") would force every consumer to re-construct the runtime objects, tightly couple the SEMP client to the config schema, and break the interface-based polymorphism.

### Summary: the invariants that make the architecture sound

Every edge case above resolves cleanly *because* the design holds the following invariants:

1. **No per-user state on shared objects.** Authenticator fields are process-stable; per-user data flows via `ctx`.
2. **Cache and singleflight keys include `sha256(subjectToken)`.** This is what partitions concurrent work by user.
3. **Effectively-immutable struct fields.** Set at construction, never written again — concurrent reads are always safe.
4. **Per-call private state lives on stacks or fresh heap allocations.** Never aliased across goroutines.
5. **Synchronization is delegated** to the components that need it (Cache, singleflight, `*http.Transport`), not implemented on the Authenticator or Exchanger directly.

If any of these invariants is violated in implementation — for example, by adding a `lastToken` field to the Authenticator, or by computing a singleflight key that omits the subject token — the safety argument collapses. **Code review must verify these invariants hold.**

---

## Decision 9: Config validation strategy — eager at startup, but bounded

When a broker is misconfigured for OAuth (missing audience, expired client secret, unreachable IdP, etc.), we want to surface the problem as early and as locally as possible — ideally at deploy time, not at 3am when the first request hits a stale config. But "validate everything at startup" is not free. This decision locks in *which* validations happen at startup and *which* are deliberately deferred.

### The validation taxonomy

Validation can happen at five distinct levels, with different costs and payoffs:

| Level | What it checks | When | Cost | Locked decision |
|---|---|---|---|---|
| 1 — Structural | YAML parses, field types correct | Startup | Free (already done) | ✅ Yes, at startup |
| 2 — Required fields | If `mode: oauth`, required fields are present and non-empty | Startup | Free (struct inspection) | ✅ Yes, at startup |
| 3 — Semantic | URLs parse, no whitespace in audiences, scopes well-formed | Startup | Free (string checks) | ✅ Yes, at startup |
| 4 — Liveness / reachability | Real HTTP call to IdP to verify it's reachable / creds valid | Startup | Real cost — couples boot to remote dep | ❌ Deferred to future ticket |
| 5 — End-to-end functional | Real RFC 8693 exchange | Runtime / E2E tests | Definitionally per-request | N/A — not a startup concern |

### What we DO at startup (Levels 1–3)

A new `validateOAuthConfig(cfg)` function runs as part of the existing `validate()` chain in `internal/config/config.go`. It performs purely struct-shape checks — zero network calls, microsecond-scale runtime cost.

**Per-broker checks (when `auth.mode: oauth`):**

- `auth.oauth.audience` is set and non-empty.
- `auth.oauth.scopes`, if present, contains only non-empty trimmed strings (no duplicates, no whitespace-only entries).
- `auth.oauth.requested_token_type`, if present, is a recognized URI (default to `urn:ietf:params:oauth:token-type:access_token` if omitted).

**Global IdP block checks (required if any broker uses oauth mode):**

- `oauth.token_url` is set, parses as a URL, and uses `https://` (or `http://` only with an explicit dev-mode opt-in flag).
- `oauth.client_id` is set and non-empty.
- `oauth.client_secret` is set and non-empty **after `${VAR}` substitution** — catches "the env var didn't resolve" at boot.
- `oauth.client_id` and `oauth.audience` follow common IdP conventions (alphanumeric + `-_/` typically; reject obvious garbage like embedded newlines or shell metacharacters).

**Cross-cutting checks:**

- If any broker has `auth.mode: oauth`, the global `oauth:` block **must** be present. Hard error if missing.
- If the global `oauth:` block is present but no broker uses oauth, log a WARN (config drift) but do not fail boot.

**Sketch:**

```go
func validateOAuthConfig(cfg *ServerConfig) error {
    var brokersUsingOAuth []string
    for alias, brokerCfg := range cfg.brokers {
        if brokerCfg.Auth.Mode == AuthModeOAuth {
            brokersUsingOAuth = append(brokersUsingOAuth, alias)
        }
    }

    if len(brokersUsingOAuth) == 0 {
        if cfg.OAuth != nil {
            slog.Warn("oauth IdP config provided but no broker uses oauth mode")
        }
        return nil
    }

    if cfg.OAuth == nil {
        return fmt.Errorf("brokers %v use oauth mode but no global oauth config is set", brokersUsingOAuth)
    }

    if cfg.OAuth.TokenURL == "" {
        return errors.New("oauth.token_url is required")
    }
    if _, err := url.Parse(cfg.OAuth.TokenURL); err != nil {
        return fmt.Errorf("oauth.token_url is not a valid URL: %w", err)
    }
    if cfg.OAuth.ClientID == "" {
        return errors.New("oauth.client_id is required")
    }
    if cfg.OAuth.ClientSecret == "" {
        return errors.New("oauth.client_secret is required (did ${VAR} fail to resolve?)")
    }

    for _, alias := range brokersUsingOAuth {
        b := cfg.brokers[alias]
        if b.Auth.OAuth == nil || b.Auth.OAuth.Audience == "" {
            return fmt.Errorf("broker %q: oauth.audience is required when auth.mode is oauth", alias)
        }
        for i, s := range b.Auth.OAuth.Scopes {
            if strings.TrimSpace(s) == "" {
                return fmt.Errorf("broker %q: oauth.scopes[%d] is empty", alias, i)
            }
        }
    }
    return nil
}
```

### What we DO NOT do at startup (Level 4)

We explicitly **do not** make any network call to the IdP at startup — no discovery doc fetch, no JWKS endpoint probe, no test exchange.

**Why this is deferred:**

- **Boot coupling.** Performing an IdP call at startup means the MCP server's boot becomes dependent on the IdP's availability. In a multi-replica Kubernetes deployment, a rolling restart during even a short IdP brownout could take the whole fleet offline. The MCP server has no business being unhealthy because the IdP is briefly down — its job is to serve broker traffic, with OAuth as one of several auth modes.
- **What you can validate is limited.** RFC 8693 needs a real subject token to do an actual exchange, which doesn't exist at boot. So a "live probe" would be limited to a discovery doc fetch or a JWKS endpoint hit — both prove HTTPS reachability but neither proves the `client_id`/`client_secret` are valid. You'd validate less than the intuition suggests.
- **The 401-on-first-request risk is mitigated by other means.** Observability + good logging on the first OAuth-mode request will surface "client_secret invalid" within seconds of traffic, not silently for minutes. Pre-deploy smoke tests (which use real tokens) are a better place to catch this.

**The hard policy decision being locked here:** *the MCP server boots even if the IdP is unreachable.* OAuth-mode brokers will fail their first request with a transport error until the IdP recovers, but other brokers (and the rest of the server) are unaffected. We choose availability over coupling.

### What we accept as the trade-off

A misconfigured `client_secret` (e.g., rotated in the IdP but not in our config) will be detected only on the first real OAuth-mode request. The detection is immediate — the IdP returns 401 — and the failure mode is well-defined (the request fails cleanly with a sentinel error, doesn't poison the cache, and is observable via Authenticator/Exchanger metrics).

This is acceptable because:

- Catching it at deploy time would require Level 4 validation, which has worse downsides.
- Pre-deploy smoke tests (typically part of CD pipeline) can do an end-to-end check with a real token that would catch this same class of bug, *without* coupling production boot to the IdP.
- Observability surfaces the failure within seconds, and the blast radius is one broker (others unaffected).

### Future option (not in scope for SOL-150070)

A separate ticket could later introduce a **background readiness probe** that:

- Fetches the IdP's discovery doc on a timer (e.g., every 30s).
- Affects `/ready` (so traffic isn't routed until the IdP is reachable) but **not** `/health` (so the pod isn't killed by k8s).
- Logs WARN on first failure, ERROR on sustained failure.

This gives operators a clear signal without coupling boot. It's not in SOL-150070's scope. Recording the future option here so it's discoverable when the time comes.

### Where this validation runs

The validation function is called from `LoadConfig`'s existing `validate()` chain. A misconfigured OAuth setup causes `LoadConfig` to return an error, which causes `main()` to exit with a non-zero status before the HTTP server starts listening. Same failure surface as any other config error today.

### Where Authenticator construction stays lazy

Because Levels 1–3 already happened at startup, `NewBrokerClient` (invoked lazily via `pool.getOrCreate`) can trust the config is well-formed when it constructs the `OAuthAuthenticator` on first request. No re-validation needed at construction time. The first-request hot path stays fast.

**Slogan:** eager config validation, lazy object construction.

### Principle captured

> **Validate at the earliest point you can do so without paying for capabilities you don't have.** Structural, schema, and semantic validation cost nothing and catch real bugs at deploy time. Liveness validation costs a remote dependency and catches a narrower class of bugs — that trade is rarely worth it for v1, and is better served by a separate readiness probe with its own failure semantics.

---

## Decision 10: Refactor visibility and commit hygiene

This decision is about **how the work lands**, not about what the work is. It belongs in the architecture plan because the way a security-critical change is staged for review is itself an architectural concern — a clean technical design that arrives as a 2,000-line opaque PR can be rejected (or merged unread) for the wrong reasons. The goal is for every reviewer to be able to convince themselves of correctness from one focused commit at a time.

### The framing: this is a refactor + a feature, not just a feature

The original Jira story (SOL-150070) describes the work as "extend the existing `basic`/`bearer` dispatcher with an `oauth` case." That framing came from the prototype's perspective. The production recon revealed it is more accurate to describe the work as:

1. **A behavior-preserving refactor** — introducing an `Authenticator` interface where none existed, replacing the package-level `auth.AddAuth(ctx, req, cfg)` function dispatcher with per-broker `Authenticator` instances, and migrating both `sempv1.HTTPClient` and `sempv2.HTTPClient` to consume the interface.

2. **Plus a feature addition** — adding the global IdP config block, `InjectRawToken` middleware, `TokenCache` placeholder, `Exchanger`, `OAuthAuthenticator`, the `oauth` branch in the dispatcher, and the end-to-end wiring.

**This distinction must be visible** in commit messages, PR descriptions, the changelog, and reviewer comms. Treating the whole change as "just adding OAuth" misrepresents the blast radius and obscures the deliberate behavior-preservation discipline applied to the refactor portion.

### Decision: each refactor and each feature increment is its own commit

The Authenticator refactor lands as a **standalone, behavior-preserving commit**. A reviewer looking at that single commit's diff should be able to convince themselves of two claims:

- **Behavior is identical.** No new auth modes, no changes to what `basic` or `bearer` requests do over the wire. Every existing test passes unchanged.
- **Shape changed for a stated reason.** The interface enables OAuth to carry an `*Exchanger` dependency that the function dispatcher couldn't carry cleanly.

If the refactor is mixed with feature additions in the same commit, neither claim is independently auditable.

### Required properties of the refactor commit

The refactor commit (introducing `Authenticator` interface + migrating v1/v2) must:

- **Preserve observable behavior** — every existing unit, integration, and E2E test passes unchanged. If any test breaks, the refactor is not behavior-preserving and must be revised.
- **Add direct tests for the new types** — `BasicAuthenticator.AddAuth(...)` does what `auth.AddAuth(..., AuthModeBasic, ...)` did; same for `BearerAuthenticator`. These are net-new tests on net-new types; they do not replace the existing dispatcher tests, which remain valid (against the deprecated/removed function) or are migrated cleanly to the new interface tests.
- **Pass `-race`** — the goroutine-safety story (Decision 7's invariants) is verified, not assumed.
- **Touch the agreed code paths and no others** — the diff is bounded to `internal/semp/auth/`, `internal/semp/sempv1/`, `internal/semp/sempv2/`, `internal/semp/pool.go`, `internal/semp/broker.go`, and test files for those packages. If the diff strays outside that boundary, the commit is doing more than it claims.

### Commit sequence on the feature branch

The token-exchange work lands on a long-lived `token-exchange` feature branch (per the Jira story). Within that branch, the natural commit sequence is:

| # | Commit | Behavior change? | Notes |
|---|---|---|---|
| 1 | `refactor(auth): introduce Authenticator interface; migrate sempv1+sempv2 to per-broker instances` | None — pure refactor | Strict behavior-preservation. Must pass all existing tests unchanged. |
| 2 | `feat(config): add global oauth IdP block and per-broker oauth fields with startup validation` | None at runtime — config schema + Levels 1–3 validation only | No code path reads these fields yet. |
| 3 | `feat(auth): add InjectRawToken middleware and RawTokenFromContext accessor` | New ctx value present on all requests; nothing else reads it yet | Net-additive. Existing flows unaffected. |
| 4 | `feat(oauth): add TokenCache interface and in-memory placeholder implementation` | None — no callers yet | Standalone, testable in isolation. |
| 5 | `feat(oauth): add Exchanger (singleton) for RFC 8693 token exchange` | None — no callers yet | Depends on #2 (IdP config) and #4 (cache). |
| 6 | `feat(auth): add OAuthAuthenticator and oauth branch in NewAuthenticator dispatcher` | `auth_mode: oauth` brokers become functional | Depends on #1 (interface), #3 (raw token), #5 (Exchanger). |
| 7 | `feat(server): wire Exchanger in main; pool constructs Authenticator per broker` | Token exchange now end-to-end | The "thing turns on" commit. Depends on #5 + #6. |
| 8 | `test: add Keycloak-based E2E test for agent token → broker token exchange` | None — test only | Validates the entire flow. |
| 9 | `chore(logs): run /check-logs and fix any flagged issues` | None — log hygiene only | Per CLAUDE.md. Story acceptance criterion. |

Properties of this sequence:

- **Commits 1–5 are individually behavior-neutral or strictly additive.** No production caller is affected until commit 6.
- **Bisection is trivially useful.** If something regresses after the feature lands, `git bisect` between commits 6, 7, 8 immediately localizes the cause.
- **Each commit is reviewable in isolation.** None depends on understanding the others to be evaluated for correctness.
- **The "feature turns on" moment is identifiable** as a single commit (#7). Easy to revert if needed.

This sequence is a starting point. The actual commit boundaries during implementation may shift slightly (e.g., #3 and #4 may swap, #4 may split into "interface" and "impl"), but the **principle** is: each refactor and each feature increment is its own commit, behavior changes are identified and bounded, the "turn on" moment is explicit.

### Decision: small PRs to the feature branch, one merge PR to `main`

Per the Jira story, the feature branch lives until Phase 2 is release-ready. Within that lifetime, the right pattern is:

- **Each commit (or tight pair of commits) is its own small PR onto the feature branch.** Tight focus, easy review, clean changelog entry per PR.
- **The eventual feature-branch → `main` PR** is then mostly a formality — the work has already been individually reviewed.

This is the standard "long-lived feature branch with internal PR hygiene" pattern. The alternative — one giant PR at the end — defeats the purpose of incremental review and makes the final merge an all-or-nothing event with no leverage for course correction.

### Required content for each PR description / commit message

For every PR landing on the feature branch, the description must answer four questions:

1. **What does this commit do?** One sentence at the top of the message body. Concrete and observable.
2. **Why is this needed?** The motivation in two to four sentences. For the refactor commit, this is "preparing for OAuth, which needs per-broker state beyond `AuthConfig`; the function dispatcher couldn't carry an `*Exchanger` cleanly." Link to `architecture-plan.md` for the long-form rationale.
3. **What does *not* change?** Especially for the refactor commit — explicitly call out that observable behavior for `basic` and `bearer` modes is unchanged. For feature commits, call out which existing flows are untouched.
4. **How was this verified?** Tests added or changed; CI runs of `make check`, `-race`, `/check-logs`. For the refactor commit specifically: "all existing tests pass unchanged."

### Changelog discipline

The `CHANGELOG.md` (project-level) gets one entry per substantive PR. The refactor commit's entry is **not** "added OAuth support" — that comes later. The refactor entry reads more like:

> *refactor: replaced the package-level `auth.AddAuth(ctx, req, cfg)` dispatcher with an `Authenticator` interface and per-broker instances. Both SEMPv1 and SEMPv2 protocol clients now consume a borrowed `Authenticator` reference constructed once per broker inside `NewBrokerClient` (invoked lazily via `pool.getOrCreate`). No behavior change for existing `basic` and `bearer` auth modes. Enables upcoming RFC 8693 token-exchange support (SOL-150070).*

The OAuth-feature commit's entry comes later and says what the feature does.

### Compliance with existing project conventions

The repo's in-tree `CLAUDE.md` already mandates:

- Run `/check-logs` before committing — to scan for logging security violations. **Required for every commit in this sequence that touches auth, exchange, or cache code.** Fix all CRITICAL and HIGH issues before pushing.
- Run `make check` (build, vet, lint, race-enabled tests) before pushing. **CI is the source of truth** — if the Makefile drifts, CI wins. We follow that.
- E2E suites are run by CI (`test/e2e-basic-mcp`, oauth, monitoring). Our new Keycloak-based E2E test must integrate with the existing harness, not parallel to it.
- Tool naming uses kebab-case. (Not directly relevant to this work, but mentioned for completeness — confirms the project has codified conventions we follow.)

These are non-negotiable. Every commit in the sequence above must satisfy them.

### What this decision does NOT do

- It does not specify exact wording for commit messages or PR titles — only the content they must convey.
- It does not enforce a specific number of commits — only that refactor and feature increments are separated.
- It does not prescribe a review process beyond what the project already has — only that the commits are *reviewable* in isolation.

These are intentional. The decision is about discipline at the commit level; specifics within that discipline are at the implementer's judgment.

### Principle captured

> **The reviewer's mental model is the artifact, not just the code.** A change that is correct but unreadable is worse than a change that is half-finished but reviewable. Stage commits so each one stands alone for review; never mix refactor with feature; always state explicitly what does *not* change. Behavior-preservation is a property to be claimed and defended in the commit message, not assumed silently.

---

## Decision 12: Solace broker OAuth session model and cookie jar scope

This decision exists because of a real concern raised during architecture review: *does the existing per-broker `SafeCookieJar` model correctly isolate OAuth requests across users?* The concern was specifically about whether the broker might issue per-user session cookies under OAuth, which — if it happened — would defeat the per-user identity isolation that the rest of the architecture works hard to guarantee. The Sender's own struct-comment (`sender.go:43-44`) flagged this exact scenario as a future risk:

> *NOTE: if per-user broker sessions are introduced, jar replacement will need to be scoped per user.*

That comment was an honest warning from the original author, and we owed it a real answer rather than an assumption.

### The contract, verified from official Solace documentation

Three statements from Solace's official docs, quoted directly:

- **"Sessions are not created when authenticating with an OAuth token or tokens using HTTP Bearer authentication."**
- **"If a session cookie is provided, it is ignored."**
- **"The bearer token in the Authorization header must be provided on every request."**

Sources:

- [SEMP Authentication and Authorization](https://docs.solace.com/Admin/SEMP/SEMP-Security.htm)
- [SEMP API Architecture](https://docs.solace.com/SEMP/SEMP-API-Archit.htm)
- [Configuring OAuth Authentication](https://docs.solace.com/Admin/Configuring-OAuth-for-Management-Access.htm)
- [Management User Authentication / Authorization Overview](https://docs.solace.com/Overviews/Mgmt-User-Authen-Auth-Overview.htm)

### What these statements jointly establish

These three statements, taken together, define a **stronger isolation contract than we were originally assuming**:

1. **No broker-side session is created** for OAuth-authenticated SEMP requests. The broker authenticates the request entirely from the Bearer token in the `Authorization` header, validates the token against its configured OAuth profile, and treats the request as a standalone authenticated operation. There is no "session continuation" state on the broker side.

2. **The broker does not issue session cookies** in response to OAuth-authenticated requests. There is nothing for our cookie jar to accumulate from an OAuth-mode interaction.

3. **The broker actively ignores any session cookie** that happens to be present on an OAuth-authenticated request. Even in the pathological case where a stale cookie from a prior basic-auth interaction with the same broker ended up in our jar, the broker would discard it and authenticate purely from the Bearer token. Identity cannot flow through cookies for OAuth.

4. **Every request must carry the Bearer token.** The Bearer token *is* the entire authentication artifact for each request, every time. This matches RFC 8693's stateless-by-design model and is what makes per-request, per-user authentication safe under a shared connection pool.

### Why this is the right design (and why our concern was substantive but resolved)

The concern was substantive because, if Solace had done what some lesser-implemented OAuth integrations do — issuing a session cookie after the first OAuth-authenticated request and then *using* that cookie for subsequent requests — the entire per-user isolation chain would collapse:

- User Alice's request arrives, broker issues `Set-Cookie: session_id=ABC`.
- The jar captures the cookie.
- User Bob's request goes out next; `http.Client.Do` auto-attaches the cookie.
- Broker sees Bob's Bearer token *and* Alice's session cookie. Behavior is broker-dependent and almost certainly wrong.

Solace explicitly avoids this trap. The OAuth design is stateless by intent — the Bearer token carries identity, expiry, and audience as self-contained claims; no broker-side state is needed; cookies are unnecessary noise; and they're actively rejected if present. **This is what OAuth was designed to be**, and Solace honors it.

### What this means for our architecture

#### The existing per-broker `SafeCookieJar` model is correct for all three auth modes

| Mode | Broker behavior re cookies | Jar's role | Correct? |
|---|---|---|---|
| `basic` | Issues a session cookie; subsequent requests authenticate via cookie | Jar accumulates and presents the cookie | ✅ Yes — shared cookie is correct because all callers share one broker-side identity |
| `bearer` | Effectively same as basic for cookie semantics (static credential, one shared broker-side identity) | Jar accumulates and presents | ✅ Yes — same reasoning |
| `oauth` | Does not issue cookies; ignores any cookie sent | Jar is a functional no-op for OAuth requests | ✅ Yes — harmlessly correct, even if a stale cookie were present |

The existing `SafeCookieJar` does not need to change. Its per-broker scope is correct for all three modes, for **mode-specific reasons** — basic/bearer because the broker-side identity is shared and the cookie reflects shared session state; OAuth because cookies don't carry any state the broker cares about.

#### The 401-handling refactor (Decision 11) becomes simpler

For OAuth-mode 401 responses, this contract means:

- A 401 cannot be a "session expired" error, because OAuth doesn't establish sessions.
- A 401 cannot be a "cookie went stale" error, because OAuth doesn't use cookies.
- A 401 means specifically: **the broker rejected the Bearer token itself.** Likely causes: token logically expired (we trusted a stale `expires_in`), audience mismatch, IdP trust broken, user permissions revoked since the token was minted, or clock skew with the IdP.

For each of these, retrying with the same cached token will produce the same 401. The correct action is:

- **Evict the cached token for this `(user, broker, scopes)` key** so the next request from the same user does a fresh exchange.
- **Do not retry this request.** Fail fast.
- **Do not touch the cookie jar** — it has no role in OAuth's failure mode.

`OAuthAuthenticator.HandleAuthFailure` (or whatever we name the method in Decision 11) is thus a one-liner: evict the cache key and return `retry: false`. Cookie jar is not in scope for OAuth's failure handling.

#### The Sender's existing warning comment remains as future-proofing

The comment at `sender.go:43-44` correctly anticipated a class of bug. Our work does **not** trigger it because Solace does not introduce per-user broker sessions for OAuth. **The comment remains valid as a forward-looking warning** — if Solace ever changes this behavior (which would require violating OAuth's stateless design, so it's unlikely), a future maintainer will see the comment and know to revisit this decision.

We deliberately do not delete the comment; we *strengthen* it by adding the contract this decision names, so the future maintainer knows what assumption is being relied on.

### The named contract (load-bearing for all OAuth correctness in SOL-150070)

> **Contract — Solace broker OAuth session model:** Solace PubSub+ brokers do not establish broker-side sessions, do not issue session cookies, and actively ignore any session cookie present on requests authenticated via OAuth Bearer tokens. The Bearer token in the `Authorization` header is the complete and required authentication artifact for every SEMP request authenticated via OAuth.
>
> This contract is established by Solace's official documentation (links above). The token-exchange architecture in this repository relies on this contract for per-user isolation across a shared HTTP connection pool and a shared cookie jar. If this contract ever changes — if Solace ever introduces broker-side sessions or session cookies for OAuth-authenticated traffic — this decision and Decisions 7, 8, and 11 must be re-evaluated.

This contract is now **reviewable, not assumed**. A future maintainer, reviewer, or auditor can verify it independently by following the citation links.

### What this decision does NOT do

- It does not change anything about the `SafeCookieJar`'s implementation or scope.
- It does not change the construction site or lifecycle of the cookie jar.
- It does not affect basic or bearer mode behavior in any way.
- It does not promise anything about how Solace handles cookies in *non-SEMP* contexts (e.g., the Broker Manager web UI uses different patterns).
- It does not promise anything about future Solace broker behavior — only that today's documented behavior is correct for our use, and the assumption is explicit.

### Principle captured

> **Load-bearing assumptions deserve citations, not folklore.** When a design relies on the behavior of a remote system — especially one we don't control — that behavior must be named, documented, and made verifiable. Future maintainers will not have access to the conversation that produced the design; they will only have the code and the architecture document. The document is where assumptions become contracts.

---

## Decision 13: Test surface and concurrency-test discipline

This decision exists because **testability is a property of the architecture, not an afterthought**, and the architecture we have produces an unusually clean test surface. Every architectural decision in this plan was made for correctness reasons, but each one also produces a testability win: small components with one or two collaborators, immutable post-construction state, dependencies passed by interface, per-call data flowing via `ctx` or method parameters. That shape is not a coincidence — it is what falls out when separation of concerns is genuine. This decision locks in the discipline required to *realize* that testability in the codebase, not just observe it on paper.

The downstream consequence: **the concurrency invariants named in Decision 8 stop being reviewer discipline and become CI-enforced contracts.** A future PR that violates them produces a red build, not a missed code-review comment.

### Per-component testing strategy

Each component has a narrow public surface and a small set of dependencies. Tests follow that shape — each component is unit-tested in isolation with at most one or two test doubles for its collaborators.

| Component | Public surface (what we test) | Test doubles used | Concurrency angle |
|---|---|---|---|
| `InjectRawToken` middleware | Header parsing; ctx accessor returns the captured token | Stub `http.Handler` to capture ctx | None per-call — ctx is fresh per request |
| `TokenCache` (placeholder impl) | `Get`/`Put`/`Delete` semantics; freshness contract (expired entries return `found=false`) | None — direct construction | 100 goroutines on disjoint keys under `-race` |
| `Exchanger` | Cache-hit path; cache-miss path; singleflight dedup; error classification (rejected vs. transport vs. invalid response); cache key includes every input that affects the response | Fake `TokenCache` (start empty, observe writes); fake IdP via `httptest.NewServer` | The mandatory concurrency tests listed below |
| `BasicAuthenticator` | `AddAuth` sets correct basic header; `HandleAuthFailure` clears cookie jar once | Fake `SafeCookieJar` | Field immutability under concurrent `AddAuth` |
| `BearerAuthenticator` | `AddAuth` sets correct bearer header; `HandleAuthFailure` returns `retry: false` | None | Field immutability under concurrent `AddAuth` |
| `OAuthAuthenticator` | `AddAuth` reads raw token from ctx, calls Exchanger, sets bearer header; `HandleAuthFailure` evicts cache and returns `retry: false` | Fake `Exchanger` | Field immutability under concurrent `AddAuth` with distinct ctx |
| `Sender` (post-Decision 11) | Rate limiting; semaphore acquisition/release; retry policy for 429/503/other-5xx; retry exhaustion; idempotency guard; delegates 401 to `Authenticator.HandleAuthFailure` | Stub `Authenticator`; `httptest.NewServer` for the broker side | Existing test surface; new test that 401 → Authenticator is called |
| `validateOAuthConfig` | Table-driven validation: missing fields, malformed URL, env-var-unresolved | None — pure function | None — pure data |

The visible pattern: **every component tests with one or two fakes.** That is the test-coverage sweet spot. If we ever find ourselves writing a test for a component that needs three or more fakes set up just to exercise one method, that's a signal the component has accreted too many responsibilities and should be revisited.

### Mandatory concurrency tests (these define correctness for SOL-150070)

These tests are not nice-to-have. Each one corresponds directly to an invariant from Decision 8. If any one fails, the architecture's safety claim is invalidated.

#### Test 1 — Per-user isolation (the security invariant)

The most important test in the whole feature. Proves that no user ever sees another user's exchanged token.

```
Setup:
  real Exchanger
  fake TokenCache (in-memory)
  fake IdP that returns token = "exchanged-for-" + subject_token

Run:
  1000 goroutines, half with subjectToken="alice-token", half with "bob-token",
  same broker (same audience and scopes), all concurrent.

Assert:
  Every goroutine that asked for alice-token got "exchanged-for-alice-token".
  Every goroutine that asked for bob-token  got "exchanged-for-bob-token".
  No goroutine got the other user's token.

Flags: -race
```

This is the test that *proves* the cache-key partitioning works as designed. It must pass.

#### Test 2 — Singleflight stampede prevention

Proves singleflight actually deduplicates identical concurrent exchanges.

```
Setup:
  real Exchanger
  empty fake TokenCache
  fake IdP that counts how many requests it sees

Run:
  500 goroutines, all with the same (subjectToken, audience, scopes), all concurrent.

Assert:
  Fake IdP saw exactly 1 request (not 500).
  All 500 goroutines got the same token back.

Flags: -race
```

If this test ever fails, the singleflight wiring is broken (likely the singleflight key drifted from the cache key, or the closure captured per-call state incorrectly).

#### Test 3 — Cache key partitioning completeness

Proves that *every* component of the cache key actually contributes to partitioning. Catches "we forgot to put scopes in the key" bugs.

```
For each pair of (subjectToken, audience, scopes, requestedTokenType) tuples
that differ in exactly one field:
  Send one request with tuple A, one with tuple B (same subjectToken if testing
  audience/scopes/type, different subjectToken if testing the token field).

Assert: fake IdP saw 2 requests. Two distinct cache entries were created.

Flags: -race
```

#### Test 4 — Concurrent 401-eviction (post Decision 11)

Proves per-user cache eviction works under concurrent access and doesn't affect other users.

```
Setup:
  Exchanger with cache pre-populated for (Alice, broker, scopes) and (Bob, broker, scopes).
  Fake broker that returns 401 to OAuth requests.

Run:
  Alice's request goes through Authenticator.AddAuth → broker returns 401 →
  Sender calls Authenticator.HandleAuthFailure → cache entry should be evicted.

Assert:
  Cache entry for (Alice, broker, scopes) is gone.
  Cache entry for (Bob,   broker, scopes) is untouched.

Flags: -race
```

#### Test 5 — Authenticator struct immutability under concurrent calls

Proves that `AddAuth` does not mutate the Authenticator's fields, no matter how many goroutines call it concurrently with different ctx.

```
Setup:
  authn := NewOAuthAuthenticator(testExchanger, "aud-1", []string{"read", "write"})
  capture initial field values

Run:
  100 goroutines, each calls authn.AddAuth(ctx, req) with its own distinct ctx
  containing its own distinct subject token.

Assert: every field on authn has its original value at the end.

Flags: -race
```

This is the test that *enforces* Decision 8's "no per-user state on shared objects" invariant. The struct comment names the invariant; this test fails any PR that violates it.

### Race-flag discipline

**Every test added by this feature runs under `go test -race`.** No exceptions.

The architecture deliberately produces a no-shared-mutable-state-in-hot-path design (per-call data on stacks or in fresh heap allocations; shared data effectively immutable after construction; synchronization delegated to the components that own mutable state). The race detector should therefore report **zero** issues even under the heaviest concurrent test load.

If it ever reports a race in a test for this feature, **that race is real, not a false positive.** It indicates either:

- A genuine bug in the implementation (some field mutates after construction).
- A genuine bug in the design (we missed a synchronization boundary).
- A test that is itself racy (shared test setup mutated by parallel subtests — fixable, but worth understanding).

In all three cases, the race must be diagnosed and fixed, not silenced.

The project's `make check` already runs tests with `-race` (per `CLAUDE.md`). Our tests inherit this. We do not add any test that would fail with `-race`.

### Goroutine leak detection

Tests for the `Exchanger` (especially singleflight tests) launch goroutines transparently — singleflight's closure, possibly an HTTP request, possibly the fake IdP's handler goroutine. Every test that exercises code paths capable of launching goroutines ends with a leak check:

```go
t.Cleanup(func() {
    goleak.VerifyNone(t)
})
```

(Or equivalent — the project may already have a pattern; if so, we match it.)

This catches the entire class of "I launched something I never awaited" bugs that singleflight, retryablehttp, and asynchronous HTTP make easy to introduce.

### Invariants from Decision 8 become enforced, not just documented

Decision 8 names five invariants the design relies on. Each gets at least one named test that fails if the invariant is violated:

| Decision 8 invariant | Test that enforces it |
|---|---|
| No per-user state on shared objects | Test 5 — Authenticator struct immutability |
| Cache and singleflight keys include `sha256(subjectToken)` | Test 1 — Per-user isolation |
| Effectively-immutable struct fields | `-race` discipline; Test 5 |
| Per-call private state lives on stacks or fresh heap allocations | Goleak + `-race` (no shared state to race on) |
| Synchronization is delegated to the components that own mutable state | Test 2 — Singleflight stampede; concurrent TokenCache test |

A future PR that breaks an invariant fails CI. The invariants stop being review-discipline and become contracts.

### Per-commit test obligations (binds Decision 10's commit sequence)

Decision 10 specified that the work lands as a sequence of focused, behavior-bounded commits. This decision specifies the **test obligation** for each:

| # | Commit | Test obligation |
|---|---|---|
| 1 | Authenticator interface refactor | All existing tests pass unchanged; new direct unit tests on `BasicAuthenticator` and `BearerAuthenticator`; `-race` clean |
| 2 | Config schema + validation | Table-driven `validateOAuthConfig` tests covering Levels 1–3 from Decision 9 |
| 3 | `InjectRawToken` middleware | Header-parsing tests, ctx-accessor tests, `-race` clean |
| 4 | `TokenCache` placeholder | Get/Put/Delete tests; freshness contract test; 100-goroutine concurrent-access test under `-race` |
| 5 | `Exchanger` | All 5 mandatory concurrency tests above; error-classification tests; `-race` + goleak |
| 6 | `OAuthAuthenticator` + `oauth` dispatcher branch | `AddAuth` with fake Exchanger; `HandleAuthFailure` cache-eviction test; struct-immutability under concurrent calls |
| 7 | `main` wiring | Integration test: real Exchanger + fake IdP + fake cache + real `BrokerPool` constructing the wiring end-to-end |
| 8 | Keycloak E2E | Full agent-token → broker-token flow against real Keycloak in Docker, integrated with existing `test/oauth/` harness |
| 9 | `/check-logs` hygiene | No test code; verification step, per Decision 10 |

Every commit must pass its own test obligation before being pushed. Every commit must pass `make check` (which includes `-race`). CI is the source of truth.

### What this decision does NOT do

- It does not specify exact test file names or test function names — those are the implementer's judgment.
- It does not require 100% line coverage — coverage targets are project-level decisions.
- It does not specify a mocking library — Go's interfaces + handwritten fakes are sufficient and idiomatic; no testify mocks needed unless the existing codebase uses them.
- It does not impose tests on code unrelated to this feature.

These are intentional. The decision is about the **non-negotiable test surface** for SOL-150070's correctness claims; specifics within that surface are at the implementer's judgment.

### Why this discipline matters specifically for this feature

Token exchange is a security-critical path serving multiple users concurrently with per-user identity guarantees. The blast radius of a regression is large: a per-user isolation bug could be a credential leak; a singleflight bug could be an IdP stampede; a 401-eviction bug could be a cache-poisoning vector. These are not theoretical — they are the failure modes the architecture was designed to prevent.

Discipline at the test level is how we ensure those design protections survive contact with future maintenance. **Decisions 7, 8, 11, and 12 describe what the design promises; this decision describes how those promises are kept under change.**

### Principle captured

> **Testability is a property of the architecture, and discipline is what realizes it.** A clean architecture makes good tests possible; only discipline makes them inevitable. For a security-critical feature, the discipline must be visible in the architecture document — what tests are mandatory, what flags they run under, what invariants they enforce — so future maintainers know what to preserve, not just what to build.

---

## Updated open items (replacing the earlier item 6)

6. **Components not yet decided in this doc:** `InjectRawToken` middleware design details (ctx key shape, middleware-chain position relative to SDK's `RequireBearerToken`, observability surface), `IdPClient` (whether to split from Exchanger or keep inline), exact YAML/struct shape of the new config sections, full mechanics of the `Authenticator` interface migration (struct field renames in `sempv1.HTTPClient` and `sempv2.HTTPClient`, the deprecated `auth.AddAuth` function's removal vs. internal use), **and Decision 11 (Sender as detector / Authenticator as handler) which builds on Decision 12 and is now ready to be written**.

---

## Principles captured by these decisions

> **State that doesn't change per call shouldn't be re-supplied per call. State that varies per call shouldn't be held as a field.** — Decision 1 (Exchanger as singleton with per-call audience/scopes).

> **Single Responsibility Principle is about reasons to change, not number of fields. A class has one responsibility if all its fields and methods change together when the underlying behavior changes.** — Decision 3 (Exchanger ≠ Cache).

> **Componentization is the price you pay for the right to fail partially.** Enterprise customers need to fix one component without touching others — that operational requirement, not aesthetic preference, drives the boundaries. — Decision 6 (observability per component) and the broader split.

> **The interface a component depends on is the contract its implementer must honor — get it right at the moment of commitment, not when you swap implementations.** — Decision 4 (TokenCache interface committed now, for SOL-150052 to honor later).
