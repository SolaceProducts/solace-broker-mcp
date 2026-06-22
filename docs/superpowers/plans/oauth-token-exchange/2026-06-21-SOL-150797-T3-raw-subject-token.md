# SOL-150797 (T3): Capture the raw subject token on ctx after SDK validation

**Status**: planned
**Date**: 2026-06-21
**Branch**: `amorade/SOL-150797`
**Parent epic**: SOL-150070 (OAuth token exchange / Hop 2)
**Predecessors**: SOL-150794 (Authenticator interface), SOL-150795 (SEMP clients consume Authenticator), SOL-150796 (broker_oauth config schema + Hop 1/Hop 2 alignment guard)

---

## Why this ticket exists

When `broker_oauth` is enabled, the MCP server performs RFC 8693 token exchange against the customer's IdP per request. The exchange request needs the user's original signed JWT as the `subject_token` parameter. That JWT arrives in the `Authorization: Bearer <jwt>` header of the incoming MCP request, but the MCP Go SDK's `sdkauth.RequireBearerToken` middleware validates the token and forwards only the parsed `*TokenInfo` on ctx — the raw signed string is dropped.

A later ticket will add the OAuth `semp/auth.Authenticator` implementation that calls the IdP. That implementation needs the raw JWT but has only `ctx` to work with. T3 closes this gap: a small middleware stashes the raw bearer token on ctx under an unexported key, and a typed accessor returns it.

---

## Key design decisions (locked)

### 1. Position in the middleware chain: **after** the SDK's `RequireBearerToken`

Chain order at runtime:

```
sdkauth.RequireBearerToken  →  InjectRawSubjectToken  →  next handler
       (validates)              (stashes raw JWT)         (MCP SDK)
```

**Rationale.** Placing `InjectRawSubjectToken` after SDK validation gives the value on ctx a stronger contract: *if a token is present on ctx, the SDK has already validated its signature, issuer, audience, and expiry*. Downstream code (the future OAuth Authenticator) can rely on this invariant without re-validating.

This is encoded in the wiring by wrapping `next` with our middleware first, then letting the SDK middleware wrap that:

```go
middleware := sdkauth.RequireBearerToken(verifier, &opts)
return middleware(InjectRawSubjectToken(next)), nil
```

At request time, the SDK's middleware runs first. If validation fails, it writes 4xx and returns — our middleware is unreachable. If validation succeeds, the SDK calls `handler.ServeHTTP`, which is our middleware. The ordering is guaranteed by Go's control flow in `RequireBearerToken`'s body, not by configuration.

### 2. No `Authenticator` wrapper refactor

We considered building an `auth.AuthResult` + `auth.Authenticator` wrapper around the SDK's auth surface so the raw token would be a field on `AuthResult` (collapsing T3 into validator output). We rejected this for now:

- The MCP SDK is a **framework** the server lives inside, not a **library** it calls. Standard "abstract at the boundary" advice is written for libraries. SDK types appearing in our code where the SDK delivers them is the normal mode of being inside an SDK.
- The actual leak is small: `*sdkauth.TokenInfo` appears in two non-auth files (`internal/tools/identity.go`, `internal/tools/register.go`), where it is immediately converted to our `Identity` type at the call to `NewIdentityFromTokenInfo`. That conversion *is* the boundary.
- The wrapper would still have to "shadow-write" `*TokenInfo` on ctx so the SDK's `StreamableHTTPHandler` session-affinity check (`streamable.go:306`) keeps working. The wrapper does not free us from the SDK's auth contract; it only renames it.
- Cost of the wrapper (≈900 lines touched across production and tests) is disproportionate to the benefit when there is no second consumer of `*sdkauth.TokenInfo` in sight.

If a third consumer of `*sdkauth.TokenInfo` appears outside `internal/auth/` in the future, that is the trigger to reconsider (rule of three). Until then, T3's small middleware is the right tool.

### 3. Naming: `rawSubjectToken`, not `rawToken`

The value's role is RFC 8693's `subject_token`. Naming it after that role makes the contract self-documenting and reserves room for other raw tokens (none today) without name collision.

- File: `internal/auth/raw_subject_token.go`
- Type: `type rawSubjectTokenKey struct{}` (unexported)
- Middleware: `func InjectRawSubjectToken(next http.Handler) http.Handler`
- Accessor: `func RawSubjectTokenFromContext(ctx context.Context) (string, bool)`

The ticket's title and prior commit history use `rawToken`; the PR description will note the rename and the reasoning.

### 4. No per-request logging; one startup log

- **No log of the token bytes at any level**, not even at DEBUG. Token never appears in any log line.
- **No per-request "token stashed" log.** A per-request log would be noise on a busy server, would constitute a side-channel ("this request carried a valid bearer header"), and the real failure mode (token missing on ctx when downstream needs it) is already loudly observable via the future OAuth Authenticator's error.
- **One DEBUG line per installation**: `"InjectRawSubjectToken middleware installed"`. Emitted from inside `InjectRawSubjectToken`'s constructor (not from `NewAuthMiddleware`), so the log is colocated with the thing it describes. Today this fires once at startup because there is exactly one call site; if a future configuration installs the middleware on a second endpoint, the log will correctly emit a second line. No `sync.Once`, no package-level state.

### 5. No demo or instrumentation hooks

The Hop 2 prototype carried a `BMS_DEMO_LOG_TOKENS` env-flag for demo logging. T3 does not port that flag or any equivalent. Per parent ticket SOL-150070 acceptance criteria.

### 6. Stateless, ctx-only data flow

- No package-level mutable state.
- No caches, sync primitives, or maps.
- All per-request data lives on the stack of the request goroutine and on the request's ctx.
- The unexported key type ensures no other package can collide with our slot.

---

## Files touched

### New

- `internal/auth/raw_subject_token.go` — middleware, accessor, key type, doc comments.
- `internal/auth/raw_subject_token_test.go` — full test suite (see test plan below).

### Modified

- `internal/auth/middleware.go` — one-line change at line 72 wrapping `next` with `InjectRawSubjectToken`, plus an explanatory comment and the one startup DEBUG log.

### Not touched

- `cmd/server/main.go` — wiring lives inside `NewAuthMiddleware`; main.go is unaffected.
- `internal/tools/*` — tool handlers continue to receive `ctx`, no consumer of `RawSubjectTokenFromContext` exists yet.
- `internal/semp/auth/*` — the future OAuth Authenticator (different ticket) will be the first consumer.
- Configs, schemas, CHANGELOG.

---

## Implementation plan

This section covers production-code work only. Test work is in the next section.

### Step 1 — Create `internal/auth/raw_subject_token.go`

**Contents, in order:**

1. **Package doc comment** at the top of the file explaining the purpose: the SDK validates and discards the raw bearer token, RFC 8693 token exchange needs it back, this file plugs that hole. Reference the parent architecture plan and this ticket.

2. **Unexported key type**:
   ```go
   type rawSubjectTokenKey struct{}
   ```
   Empty struct. Zero bytes. Matches the pattern used by `retryStateKey{}` and `retrySafeKey{}` in `internal/semp/resilience/retry.go`.

3. **Middleware** `InjectRawSubjectToken(next http.Handler) http.Handler`:
   - **At construction time** (before returning the handler): emit one `slog.Debug("InjectRawSubjectToken middleware installed")`. This fires when the constructor runs, which today is once at startup.
   - **At request time** (inside the returned `http.HandlerFunc`):
     - Read `r.Header.Get("Authorization")`.
     - Split with `strings.Fields`. The result must have exactly two fields.
     - Field 1 must be `"Bearer"` matched case-insensitively (`strings.EqualFold`). This matches the SDK's `strings.ToLower(fields[0]) != "bearer"` check.
     - Field 2 must be non-empty.
     - When all three conditions hold: derive a new ctx with `context.WithValue(r.Context(), rawSubjectTokenKey{}, token)` and call `next.ServeHTTP(w, r.WithContext(ctx))`.
     - When any condition fails: call `next.ServeHTTP(w, r)` unchanged. **No rejection, no error, no log.** The SDK middleware upstream already rejected this case if it was a security issue; we only run when the SDK approved.

4. **Accessor** `RawSubjectTokenFromContext(ctx context.Context) (string, bool)`:
   - `v := ctx.Value(rawSubjectTokenKey{})`.
   - If `v == nil` → return `("", false)`.
   - Type-assert to `string` with `v, ok := v.(string)`. If `!ok` → return `("", false)` (defensive; only this package writes the key).
   - If `s == ""` → return `("", false)`.
   - Otherwise return `(s, true)`.

5. **GoDoc comments** on each exported symbol explaining:
   - `InjectRawSubjectToken`: position constraint (must run after `RequireBearerToken`), what the contract on ctx guarantees, no-op behaviour on malformed headers.
   - `RawSubjectTokenFromContext`: when it returns true (token present and non-empty), when it returns false (key absent, non-string value, empty string), and the intended consumer (the future OAuth Authenticator).

### Step 2 — Modify `internal/auth/middleware.go`

Current code at lines 68–72:

```go
middleware := sdkauth.RequireBearerToken(verifier, &sdkauth.RequireBearerTokenOptions{
    ResourceMetadataURL: metadataURL,
})

return middleware(next), nil
```

Replace with:

```go
middleware := sdkauth.RequireBearerToken(verifier, &sdkauth.RequireBearerTokenOptions{
    ResourceMetadataURL: metadataURL,
})

// InjectRawSubjectToken runs AFTER the SDK validates, so any value
// present on ctx under rawSubjectTokenKey{} has been validated by the
// SDK (signature, issuer, audience, expiry). Downstream code can rely
// on this invariant without re-validating. See SOL-150797.
return middleware(InjectRawSubjectToken(next)), nil
```

The startup `slog.Debug` lives inside `InjectRawSubjectToken`'s constructor (see Step 1), not here. The wiring site stays a clean one-liner.

### Step 3 — Local verification before commit

- Run `make check` (build, vet, lint, race-enabled tests). Must pass cleanly.
- Run `/check-logs` against `internal/auth/raw_subject_token.go`. The only log in the file is the startup `slog.Debug("InjectRawSubjectToken middleware installed")` with no fields and no token content. Any findings beyond zero are a regression to investigate.

### Step 4 — Commit

Single commit on `amorade/SOL-150797`. Message:

```
SOL-150797: capture raw subject token on ctx after SDK auth validates

InjectRawSubjectToken runs after sdkauth.RequireBearerToken so the
raw bearer token is stashed on ctx only when SDK validation has
already succeeded. The future OAuth semp Authenticator reads it via
RawSubjectTokenFromContext to use as subject_token in RFC 8693
token exchange.

Key type is unexported (rawSubjectTokenKey struct{}); accessor
returns (string, bool) so callers branch once. No logging of token
bytes at any level. Stateless — per-request data flows only via ctx.
```

No `Co-Authored-By` line.

### Step 5 — Open PR

PR description covers:

- What and why in plain English.
- The position decision (after-validation): SDK validates first, we stash after, so presence-on-ctx implies validation.
- The naming decision (`rawSubjectToken`, not `rawToken`) and the reason.
- Why no wrapper refactor (SDK-vs-library distinction).
- Why no per-request logging (no signal, side-channel risk).
- The one startup DEBUG log and where it lives.
- Concurrency note (per-request ctx, stateless middleware).
- Test coverage summary.
- Link to this plan document.

### Step 6 — Self-review

Run `/review` on the PR before requesting human review. Address any findings.

---

## Testing plan

This section is intentionally separated from the implementation plan above. Tests live in `internal/auth/raw_subject_token_test.go`. One test file, five named test functions, ≈150–200 lines total, race-clean, expected to run in under 100ms.

### What we are pinning down

Tests should make a future change that breaks any of the following fail loudly:

- The middleware extracts the token correctly from every valid `Bearer` shape and ignores invalid shapes (parsing logic).
- Concurrent requests do not see each other's tokens (security-critical isolation).
- The middleware runs only after the SDK has validated the token (position contract).
- The middleware does not mutate the original request's header or the parent ctx (isolation invariants).
- The accessor returns the exact captured token, or `("", false)`, with no string-empty surprises (contract).

### What we are NOT testing

- **Log output.** No logs in the file under test; nothing to assert.
- **Performance / benchmarks.** Two function calls; benchmarks are theater.
- **The unexported key from outside the package.** Cannot be done; that is the point of the unexported type.
- **The SDK's own behaviour.** The SDK is its own package's responsibility.
- **End-to-end OAuth token exchange.** That is the future ticket's test scope.

### Test functions

#### `TestInjectRawSubjectToken_HeaderShapes`

Table-driven. Each row sends one request through `InjectRawSubjectToken` (without the SDK in front, to isolate parsing), captures what the next handler sees via `RawSubjectTokenFromContext`, and asserts the expected outcome.

| Case | Authorization header | Expected `(token, ok)` |
|---|---|---|
| Missing | (header absent) | `("", false)` |
| Empty | `""` | `("", false)` |
| Bearer + JWT | `Bearer eyJhbGc.payload.sig` | `("eyJhbGc.payload.sig", true)` |
| lowercase bearer | `bearer eyJhbGc.payload.sig` | `("eyJhbGc.payload.sig", true)` |
| UPPERCASE BEARER | `BEARER eyJhbGc.payload.sig` | `("eyJhbGc.payload.sig", true)` |
| MixedCase | `BeArEr eyJhbGc.payload.sig` | `("eyJhbGc.payload.sig", true)` |
| Bearer alone | `Bearer` | `("", false)` |
| Bearer with empty token | `Bearer ` | `("", false)` |
| Basic scheme | `Basic dXNlcjpw` | `("", false)` |
| DPoP scheme | `DPoP eyJhbGc.payload.sig` | `("", false)` |
| Token only, no scheme | `eyJhbGc.payload.sig` | `("", false)` |
| Three fields | `Bearer foo bar` | `("", false)` |
| Multiple spaces | `Bearer   eyJhbGc.payload.sig` | `("eyJhbGc.payload.sig", true)` (strings.Fields collapses) |
| Tab separator | `Bearer\teyJhbGc.payload.sig` | `("eyJhbGc.payload.sig", true)` |

#### `TestInjectRawSubjectToken_ConcurrentRequests`

The most important test in the suite. Spins up N goroutines (N = 1000), each sending one request with `Bearer token-<i>`. Inside the next handler, each goroutine captures whatever `RawSubjectTokenFromContext` returns and asserts it matches its own `token-<i>`. Run under `-race`. Any cross-contamination or race detector hit fails the test.

The test exists not because we expect a race — the design has no shared state, so a race is structurally impossible. It exists as a tripwire: if a future change introduces shared state (a package variable, a sync.Map cache, a wrapping of the key into a non-empty struct), this test fails. The absence of races is the exact property the security model depends on.

#### `TestInjectRawSubjectToken_PositionAfterSDK`

Wires `sdkauth.RequireBearerToken(verifier)(InjectRawSubjectToken(probe))` and exercises two scenarios:

- **Valid token**: verifier accepts. Assert the probe handler is reached, that `auth.TokenInfoFromContext` returns non-nil with the expected claims (SDK contract), and that `RawSubjectTokenFromContext` returns the exact original token string. This pins that both the SDK and our middleware ran, in the expected order.
- **Invalid token**: verifier returns `sdkauth.ErrInvalidToken`. The probe sets `invoked := true`; the test asserts `invoked` stays `false` after the request, proving our middleware was never reached because the SDK short-circuited. This is the load-bearing test for the position decision.

#### `TestInjectRawSubjectToken_RequestIsolation`

Three sub-cases:

- **Header not mutated**: read `r.Header.Get("Authorization")` after the middleware has run. The original header value must be unchanged.
- **Parent ctx not mutated**: capture the original `r.Context()` before passing to the middleware. After the middleware runs, calling `RawSubjectTokenFromContext` on the *original* parent ctx must return `("", false)`. Only the derived ctx (the one passed to `next`) should carry the token.
- **No-op passes the request through**: send a request with no `Authorization` header. Use a probe handler that sets `invoked := true`. Assert `invoked` is `true` after the middleware runs. We are not the auth gate; we always call `next`.

#### `TestRawSubjectTokenFromContext_AccessorContract`

Edge cases for the accessor, bypassing the middleware to construct ctxs directly:

- `context.Background()` → `("", false)`.
- ctx with `rawSubjectTokenKey{}` explicitly set to `nil` → `("", false)`.
- ctx with `rawSubjectTokenKey{}` set to a non-string value (e.g. an int) → `("", false)` (defensive type-assertion).
- ctx with `rawSubjectTokenKey{}` set to `""` (empty string) → `("", false)`. This is the contract that lets callers branch once.
- ctx with `rawSubjectTokenKey{}` set to `"eyJhbGc.payload.sig"` → `("eyJhbGc.payload.sig", true)`. Byte-for-byte round trip.

### Outside-the-file consideration

The wiring in `NewAuthMiddleware` is what makes T3 actually run in production. The existing `internal/auth/middleware_test.go` covers `NewAuthMiddleware` output. A small assertion added there — "the handler returned by `NewAuthMiddleware` for OAuth mode, when invoked with a valid token, produces a ctx where `RawSubjectTokenFromContext` returns the original token" — closes the gap that a future maintainer could otherwise create by accidentally removing the `InjectRawSubjectToken` wrap.

This is a minor addition (~15 lines) to an existing test file. It is in scope for T3.

### Local verification

- `go test ./internal/auth/... -race -v` must pass with zero races.
- `make check` must pass cleanly.
- `/check-logs` must report zero findings on the new and modified files.

---

## Out-of-scope reminders

To prevent scope drift during implementation:

- **No demo logging or instrumentation hooks** (no `BMS_DEMO_LOG_TOKENS` or equivalent).
- **No caching** of the raw token or any derived value. Deferred to a separate follow-up story.
- **No changes to the future OAuth Authenticator.** That implementation lives in its own ticket and is the first consumer of `RawSubjectTokenFromContext`.
- **No wrapper refactor.** Considered and rejected; reasons documented in the "Key design decisions" section.
- **No `cmd/server/main.go` edits.** The wiring is contained in `NewAuthMiddleware`.
- **No schema, config, or CHANGELOG changes.** T3 is plumbing internal to the auth layer.

---

## Open items at time of writing

None. All design decisions locked. Ready to implement.
