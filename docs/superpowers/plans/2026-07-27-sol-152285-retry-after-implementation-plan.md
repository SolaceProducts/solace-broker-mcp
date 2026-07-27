# SOL-152285 — Honor Retry-After on IdP 429 Responses — Implementation Plan

Status: All 5 commits implemented and verified (parsing, gate mechanism,
operator logging, config surface, CHANGELOG). Ready for `make check` and PR.
See §9 for implementation-time findings not anticipated in the original
design, including §9.5 on the zero-value config bug found and fixed during
commit 4.
Ticket: [SOL-152285](https://sol-jira.atlassian.net/browse/SOL-152285)
Related: SOL-151600 (PR #205 — circuit breaker, 429 exclusion policy, the pinned
test `TestBreaker_HalfOpenExcludedProbeRefundsSlot` this ticket closes the gap on).

This document records the design arrived at through Q&A before implementation,
the reasoning behind each decision (not just the outcome), and the concerns
still open. It complements the ticket description — mechanism placement and
tradeoffs live here; intent/acceptance criteria stay in Jira.

---

## 1. The actual gap (restated precisely)

429 is deliberately excluded from circuit-breaker failure counting (SOL-151600):
a throttling IdP is up, and counting 429s would let one tenant's rate limit trip
the shared breaker. That policy is **settled and unchanged by this ticket.**

The gap: `Retry-After` is honored *only inside a single logical `Exchange()`
call's own retry chain* (`idpclient/retrying.go`'s `RateLimitLinearJitterBackoff`).
That chain's memory of "the IdP told us to wait" dies the instant `Exchange()`
returns. Every other caller — concurrent or subsequent, same broker or a
different one hitting the same shared IdP — has no way to know the IdP just
said "back off," so each independently rediscovers the same throttling via its
own doomed round-trip. The IdP's 429 is a statement about *itself* (one shared
resource), but today it's being honored *per-caller*, not *per-IdP*.

The half-open unbounded-probe scenario (pinned by
`TestBreaker_HalfOpenExcludedProbeRefundsSlot`) is the sharpest symptom of this
gap — gobreaker refunds the probe slot for excluded outcomes, so a string of
429s during half-open never spends the probe budget and nothing paces the
retries — but the gap exists identically in the **closed** state under
gradual throttling, which is arguably the more common real-world case.

---

## 2. Core mechanism: a single shared time-gate

**Not per dedup-key, not per-broker.** The breaker itself is process-wide,
guarding one shared IdP (`breakerName` doc: "one breaker per process guarding
the one IdP"). The gate must have the same scope — a 429 from one broker's
exchange is evidence about the IdP, not about that broker, so the gate
protects every caller, including ones hitting a different audience against the
same IdP. **This is a deliberate blast-radius choice — document it inline so a
reviewer doesn't have to ask "why does throttling on broker A block broker
B's unrelated exchange?"**

### State

One field on `*Exchanger`, same lifetime as `e.breaker`:

```go
// gatedUntil is a shared, process-wide backoff: while now < gatedUntil, no
// caller attempts the IdP at all. Set only when a 429 chain exhausts with a
// usable Retry-After (see classifyRetryOutcome). Unix nanos per e.nowFunc;
// zero means "not gated". Deliberately NOT per-key: the breaker it sits in
// front of is process-wide (one shared IdP), and a 429 is a statement about
// the IdP, not about whichever broker's exchange happened to trigger it.
gatedUntil atomic.Int64
```

Use `e.nowFunc()` (the existing injectable clock seam used elsewhere in this
package) for all "now" reads/writes here — not `time.Now()` directly — so
tests can advance the gate deterministically instead of sleeping real wall
time (see §6).

### Where it's checked

`runProtectedExchange` ([exchange.go:108](../../../internal/tokenexchange/exchange.go#L108)),
*before* `e.breaker.Execute(...)`:

```go
func (e *Exchanger) runProtectedExchange(key string, input ExchangeInput) (*Token, error) {
    if until := e.gatedUntil.Load(); until > 0 && e.nowFunc().UnixNano() < until {
        return nil, &ExchangeError{
            Sentinel: ErrExchangeRateLimited, // new sentinel — see §5
            Message:  "token exchange rate limited: honoring IdP Retry-After, not attempting",
        }
    }
    if e.breaker == nil {
        return e.runExchangeOnce(key, input)
    }
    return e.breaker.Execute(func() (*Token, error) {
        return e.runExchangeOnce(key, input)
    })
}
```

A gated call fails fast — no HTTP round-trip, no breaker bookkeeping touched
at all (not `Execute`, not `IsExcluded`, not the probe-budget counters). This
runs *inside* `e.group.Do(...)` (singleflight), so concurrent identical
callers (same key) correctly share the one gate-rejection result — no special
handling needed, confirmed by inspection of the call order in `Exchange`
([exchange.go:63](../../../internal/tokenexchange/exchange.go#L63)).

**Unconditional on breaker state.** No `e.breaker.State()` check anywhere.
Applies identically whether closed, half-open, open, or the breaker disabled
(`e.breaker == nil`, a legal config per `Params.CircuitBreaker`). It's a
no-op when the breaker is already open (everything's rejected anyway) and
load-bearing exactly when it matters (closed, half-open). This also avoids
adding a dependency on gobreaker's `State()`/`Counts()` query surface, which
the codebase already treats as something to avoid calling from hot paths
(see `logBreakerStateChange`'s comment on holding the internal mutex).

### Where it's set

**Only at chain exhaustion**, in `classifyRetryOutcome`
([exchange.go:215](../../../internal/tokenexchange/exchange.go#L215)) — NOT on
the first 429 seen mid-chain in `parseIdPResponse`. A single 429 might resolve
on the very next retry attempt; only a chain that exhausted with 429s on
(potentially) every attempt is strong evidence the IdP is still throttling
*right now*. Setting the gate earlier would preemptively block every other
caller off a single data point the triggering caller itself hasn't finished
acting on.

Use the **last** attempt's `Retry-After` value. Parse both forms per
RFC 9110 §10.2.3:
- delta-seconds (`Retry-After: 120`)
- HTTP-date (`Retry-After: Wed, 21 Oct 2026 07:28:00 GMT`)

**Clock-skew floor:** HTTP-date is the IdP's absolute wall-clock time. If the
IdP's clock is skewed relative to ours, `parsedTime.Sub(now)` can be negative
or absurdly large. Floor at zero (treat a negative delta as "no gate", not as
a negative-duration gate) — never let a skewed IdP clock invert the gate
into a no-op-that-looks-set or, worse, propagate a huge accidental value
through the *cap* check before it's caught.

**Write via max, not overwrite:**

```go
func (e *Exchanger) raiseGate(newUntil int64) {
    for {
        cur := e.gatedUntil.Load()
        if newUntil <= cur {
            return
        }
        if e.gatedUntil.CompareAndSwap(cur, newUntil) {
            return
        }
    }
}
```

Lock-free CAS loop — a shorter Retry-After from one exhausted chain must not
clobber a longer one already in effect from a concurrent chain. No mutex
needed: this is a single int64 with no larger invariant to protect (see §6.3
in the Q&A — mutex would be over-engineering for one atomic value).

---

## 3. The cap: operator-configurable, not hardcoded

This codebase has an established pattern for exactly this shape of tunable —
`CircuitBreakerConfig`: shipped default → operator YAML override (pointer
field, nil = "use default") → sanity-ceiling constant that guards typos, not
a "recommended range" (see `IdPCircuitBreakerConfig` /
`MaxIdPHalfOpenProbeRequests` for the precedent). The cap here follows the
identical shape — a hardcoded Go constant would be the one tunable in this
whole subsystem that doesn't follow the established pattern, and would take a
decision away from the operator who actually knows their IdP's throttling
behavior (some IdPs legitimately need minutes; others should never be trusted
past a few seconds).

### New config surface

`internal/config/idp_circuit_breaker.go` (or a small sibling file, e.g.
`idp_retry_after.go` — naming TBD, keep it near the breaker config since both
are `broker_oauth`-scoped IdP-resilience knobs):

```yaml
broker_oauth:
  retry_after:
    max_honored_duration: 60s   # optional; shipped default applies if omitted
```

```go
type IdPRetryAfterConfig struct {
    MaxHonoredDuration *time.Duration `yaml:"max_honored_duration"`
}

const (
    // Sanity ceiling only — guards a typo'd YAML value (60h instead of 60s),
    // not a recommended range.
    MaxIdPRetryAfterHonoredDuration = time.Hour
)
```

Runtime side (`tokenexchange`), mirroring `resolveCircuitBreakerConfig`:

```go
// defaults.go
const defaultMaxHonoredRetryAfter = 60 * time.Second // starting point; confirm against
                                                        // OpenStateDuration default (30s) —
                                                        // TBD whether these should be related.
```

### Clamp behavior

If the IdP's Retry-After (after parsing, before writing to `gatedUntil`)
exceeds the operator's configured (or default) cap, clamp to the cap and log
(§4.2) — explicit acceptance criterion.

---

## 4. Logging — operator-facing states (its own commit, see §7)

The core question this section answers: **what does an operator staring at
logs during an IdP incident actually need to know?** Not just "a 429
happened" (that's already inferable from existing transport/failure-class
logging) — specifically, *what the system is now doing as a result*, because
that determines their next action (nothing to do / raise with IdP owner /
tune the cap).

Three states, mutually exclusive per exhausted 429 chain — exactly one fires
per chain exhaustion event:

### 4.1 Gate set (Retry-After present, within cap) — the feature working as intended

```go
slog.Warn("token exchange rate limited: honoring IdP Retry-After",
    slog.String("broker", input.BrokerAlias),
    slog.Duration("retry_after", honored),
    slog.Time("gated_until", gatedUntilTime))
```

Operator story: *"IdP told us to wait, so we're pacing every caller back on
its behalf — this is the system self-correcting, no action needed."*
`broker` is informational only (whoever happened to trigger it) — the gate
itself is shared IdP-wide, so the message text should make that scope
explicit rather than implying it's broker-specific.

### 4.2 Gate set but clamped (Retry-After present, exceeds configured cap)

```go
slog.Warn("token exchange Retry-After exceeded configured cap, clamping",
    slog.String("broker", input.BrokerAlias),
    slog.Duration("requested", parsed),
    slog.Duration("clamped_to", cap))
```

Operator story: *"IdP asked for longer than we're configured to honor — either
raise `max_honored_duration` if this is expected/legitimate for this IdP, or
this may indicate the IdP is behaving unusually."*

### 4.3 Gate NOT set (Retry-After absent or unparseable)

```go
slog.Warn("token exchange rate limited: IdP sent no usable Retry-After, cannot pace subsequent callers",
    slog.String("broker", input.BrokerAlias),
    slog.Int("attempts", attempts),
    slog.String("retry_after_raw", capString(rawHeaderValue, maxRetryAfterRawLogLen))) // only if present-but-unparseable; omit if truly absent
```

Operator story: *"IdP is rate-limiting us but gave us nothing to act on — the
shared-backoff protection can't engage this time, every caller is still
independently retrying. This isn't something our config can fix; consider
raising it with the IdP owner, or lean on the circuit breaker's consecutive-
failure/rate thresholds instead since this safety net won't trigger for this
IdP's 429s."*

Worth distinguishing **truly absent** vs. **present but malformed** — the
latter is a more interesting signal (misbehaving/misconfigured IdP response)
than the former (IdP just doesn't send the optional header, per RFC 6585 §4
`MAY` / RFC 9110, confirmed neither RFC makes it mandatory anywhere in HTTP).
If logging the raw malformed value, cap its length the same way
`classifyClientError` already caps the OAuth error code
(`maxErrorCodeLen`) before logging — same principle: don't let unbounded
IdP-supplied content bloat or pollute logs.

---

## 5. New sentinel: `ErrExchangeRateLimited`

Do **not** reuse `ErrExchangeCircuitOpen` for gate rejections. Reasoning:

- `ExchangeError.LogAttrs()` already emits `breaker_state=open` specifically
  for `ErrExchangeCircuitOpen` ([errors.go:100](../../../internal/tokenexchange/errors.go#L100))
  — reusing it would make a gate-rejection look identical to "the breaker
  tripped due to failures" in logs/dashboards/alerting, which is a different
  operational story requiring a different operator action.
- Any existing alerting on `errors.Is(err, ErrExchangeCircuitOpen)` would
  silently start firing for a semantically different condition if we
  overloaded it.

Add to `types.go`:

```go
// ErrExchangeRateLimited — a shared, process-wide gate is honoring the IdP's
// own Retry-After instruction from a prior exhausted 429 chain; this call
// was rejected before any IdP round-trip was attempted. Distinct from
// ErrExchangeCircuitOpen (breaker tripped on failures) — this fires
// regardless of breaker state and means the IdP is reachable but has asked
// us to wait. Transient-class: identical handling to ErrExchangeCircuitOpen
// from the agent's perspective (back off, don't retry immediately).
ErrExchangeRateLimited = errors.New("token exchange rate limited")
```

**Must also update `AgentMessage`** ([errors.go:67](../../../internal/tokenexchange/errors.go#L67)):
add `ErrExchangeRateLimited` to the transient-class branch (alongside
`ErrExchangeTransport`, `ErrExchangeRetriesExhausted`, `ErrExchangeCircuitOpen`)
so it gets the shared "identity provider is not responding" message rather
than falling through to the generic per-broker "server-side issue" message —
naming one broker for a shared-IdP-wide throttle would be actively misleading,
the same anti-pattern the existing comment there already warns against.

**Must also update `LogAttrs`** — add an analogous marker to the
`breaker_state` one, e.g. `slog.String("gate", "retry_after")`, so
log-based alerting can tell the two rejection reasons apart at a glance.

---

## 6. Test-plan concerns to close explicitly

### 6.1 CORRECTION (during implementation): `TestBreaker_HalfOpenExcludedProbeRefundsSlot` does NOT need an update — it structurally cannot exercise the gate

Originally this section predicted the test would start failing once the
gate landed, since it sends a 429 then immediately calls `Exchange()` again
expecting the IdP to be reached. **That prediction was wrong**, discovered
by actually running the test against the implemented gate: it still passes
unmodified, and for a reason worth recording rather than silently
"noticing it still works."

The test builds its exchanger via `newBreakerTestExchangerPlain`
([circuitbreaker_exchange_test.go:49](../../../internal/tokenexchange/circuitbreaker_exchange_test.go#L49)),
which wires a **plain, non-retrying** `http.Client{}` deliberately — its own
doc says this is "so one 5xx is one fast logical failure with no retry
backoff." `idpclient.WithAttemptsCounter`'s counter is only ever incremented
by `attemptsRecorder`, which `NewRetryingHTTPClient` installs — a plain
client never increments it, so `attempts()` is always 0 for every call this
test makes. `classifyRetryOutcome`'s very first line is
`if attempts < 1 { return err }` — with a plain client this guard fires on
every call, so the raw `ErrExchangeTransport` is returned unchanged, **the
`ErrExchangeRetriesExhausted` rewrap never happens**, and
`raiseGateOnExhaustedRateLimit` (which keys specifically on that sentinel)
correctly never fires. The gate is never raised in this test regardless of
whether its 429 response carries a `Retry-After` header — there's nothing
to add that would change that.

This is consistent, not a loophole: the gate is deliberately scoped to
"a chain gave up after retrying," and a plain client by construction never
retries, so it never produces that outcome. The test's own stated intent
("a 429 during half-open is excluded... must not consume a probe slot") is
still fully valid and still correctly exercised — it is simply testing a
scenario (single-attempt, no retry chain) that the gate mechanism was never
meant to touch.

**Real gap this surfaced:** none of the *existing* tests use a genuinely
retrying client during half-open with a 429+Retry-After response, so
nothing proves the gate correctly blocks a subsequent half-open probe in
the retrying-client case the ticket is actually motivated by. A **new** test
is needed for that — see §6.3's added item — built on `newBreakerTestExchanger`
(the retrying variant), not a modification of the existing pinned test.

**Sweep of every other existing test for the same risk (done, not assumed):**
grepped every `_test.go` file under `internal/tokenexchange/` and
`internal/idpclient/` for `StatusTooManyRequests` and cross-referenced against
every `e.Exchange(...)` call site in `circuitbreaker_exchange_test.go`. Result:

- **`TestBreaker_RateLimitExcludedNotCounted`**
  ([circuitbreaker_exchange_test.go:128](../../../internal/tokenexchange/circuitbreaker_exchange_test.go#L128))
  and **`TestExchange_RetriesExhaustedOn429`**
  ([exchange_test.go:1506](../../../internal/tokenexchange/exchange_test.go#L1506))
  are the only other two tests that use an actual 429 response. Both call
  `Exchange()` exactly once and inspect state/error afterward — no follow-up
  call exists for the gate to block. Unaffected as written.
- Every other multi-call test in `circuitbreaker_exchange_test.go`
  (`TestBreaker_OpenStateFailsFast`, `TestBreaker_RecoveryClosesAfterConsecutiveProbeSuccesses`,
  `TestBreaker_HalfOpenProbeFailureReopens`, `TestBreaker_HalfOpenRejectsBeyondProbeBudget`,
  `TestBreaker_OpenBreakerRejectsStampedeCleanly`, etc.) drives its test
  server with `503`/connection failures, never `429`. Since the gate is only
  ever raised on a 429-chain exhaustion, these cannot trigger it regardless
  of how many times they call `Exchange()`.
- `failure_class_test.go` and `internal/idpclient/retrying_test.go` test
  below the `Exchanger`/gate layer entirely (pure classification, and the
  retry-transport layer respectively) — no `Exchange()` calls, not in scope.

So `TestBreaker_HalfOpenExcludedProbeRefundsSlot` is confirmed to need NO
change (§6.1 correction) — but a *new* test is required to cover the
retrying-client half-open case it doesn't reach; see §6.3's added item.

### 6.2 Time control

Confirm `e.nowFunc()` is the existing clock seam (referenced in
`response.go`); wire the gate's reads/writes through it, not `time.Now()`.
Tests need to advance a fake clock to exercise gate-expiry without real
sleeps — same need `recoveryBreakerConfig`'s 250ms real wait currently
works around for the breaker's own `OpenStateDuration`; don't introduce a
second real-sleep-based timing test if the fake-clock seam already exists.

### 6.3 Required unit tests (from acceptance criteria + this design)

- Delta-seconds Retry-After sets the gate; next call before expiry is
  rejected with `ErrExchangeRateLimited`; call after expiry proceeds.
- HTTP-date Retry-After parses and sets the gate correctly.
- Absent header: gate not set, current behavior preserved, §4.3 log line
  emitted.
- Unparseable header: same as absent, but distinguished in the log
  (§4.3, malformed-value branch).
- Retry-After exceeding configured cap: clamps, gate set to capped value,
  §4.2 log line emitted.
- HTTP-date in the past / clock-skew-negative case: treated as no-gate
  (floored), not a negative-duration gate.
- Gate is set only at chain exhaustion, not on an intermediate 429 that a
  later retry in the same chain resolves.
- Concurrent exhausted chains: max-wins CAS behavior (shorter Retry-After
  doesn't clobber a longer one already in effect).
- Gate check is a true no-op when breaker is `nil` (disabled) and when
  breaker is already open (existing open-state rejection path still exercised
  and unaffected).
- **New** (added per §6.1 correction): half-open + retrying client + 429
  with Retry-After — built on `newBreakerTestExchanger` (the retrying
  variant, not `newBreakerTestExchangerPlain`), so `attempts()` actually
  exceeds 0 and the chain can genuinely exhaust to
  `ErrExchangeRetriesExhausted`. Confirms the gate is raised and a
  subsequent probe attempt during that window is rejected with
  `ErrExchangeRateLimited` before the fake clock advances, and proceeds to
  a real IdP round-trip after it does. This is the test that actually
  proves the ticket's motivating half-open scenario — the existing pinned
  test does not and structurally cannot (§6.1).
- 429 remains excluded from breaker failure counting — existing breaker
  tests stay green with no changes to `isBreakerExcluded`/`isBreakerSuccess`
  (confirmed: `TestBreaker_HalfOpenExcludedProbeRefundsSlot` needed zero
  changes, §6.1).

---

## 7. Suggested commit sequence

Split so (a) the logging/operator-visibility work is reviewable on its own,
and (b) the operator-facing config surface lands last, once the mechanism it
configures already exists and is tested against a shipped-default constant.
Every commit in this sequence builds and passes tests on its own — none
depends on a later commit.

1. **Retry-After parsing** — delta-seconds + HTTP-date parsing in
   `response.go`, unit tests for parsing alone (no gate wiring yet, no config
   surface yet — pure parsing of a header string into a `time.Duration`).
2. **Gate mechanism** — `gatedUntil` field, `raiseGate`, the check in
   `runProtectedExchange`, wiring in `classifyRetryOutcome`/`runExchangeOnce`
   to raise it on exhaustion, new `ErrExchangeRateLimited` sentinel +
   `AgentMessage`/`LogAttrs` updates. The cap is a **hardcoded shipped-default
   constant** in `defaults.go` at this stage (`defaultMaxHonoredRetryAfter`)
   — no YAML surface yet, so the mechanism is complete and fully testable on
   its own. `TestBreaker_HalfOpenExcludedProbeRefundsSlot` needs NO change
   here — confirmed by running it, not assumed (§6.1 correction): it uses a
   plain non-retrying client, which never produces the
   `ErrExchangeRetriesExhausted` outcome the gate keys on. Add the new
   retrying-client half-open test from §6.3 instead.
3. **Operator logging** — the three log states from §4, plus `LogAttrs`'s
   `gate` marker. This is the commit that directly answers "what does the
   operator see" — keep it isolated so it's reviewable purely on log-message
   quality/completeness without re-litigating the mechanism.
4. **Config surface (last)** — `IdPRetryAfterConfig` schema type in
   `internal/config`, its validator, and `FromConfig` wiring to overlay the
   operator's YAML value onto the commit-2 default (same overlay shape as
   `resolveCircuitBreakerConfig`). This is the only commit that touches
   `internal/config/` — deliberately last, since it's additive plumbing on
   top of an already-complete, already-tested mechanism, not a prerequisite
   for it.
5. **CHANGELOG entry** (per project convention — this touches
   `internal/config/` and `internal/tokenexchange/` production surface).

---

## 8. Explicitly out of scope (unchanged from ticket)

- Changing the breaker's 429-excluded classification.
- Retry-After on non-429 responses (503, deferred).
- Per-tenant rate limiting.
- **Fallback/default gate duration when Retry-After is absent** — raised
  during design Q&A as a real, non-redundant gap (an IdP that throttles hard
  but never sends the header is unprotected by *both* the gate, no signal,
  and the breaker, 429 excluded from counting by design) — but it inverts
  this ticket's core property ("only ever act on what the IdP told us") and
  changes the "absent header preserves current behavior" acceptance
  criterion. Recommended as a **separate follow-up ticket**
  (`broker_oauth.retry_after.fallback_duration`, off/zero by default so no
  operator inherits invented backoff they didn't opt into).

---

## 9. Implementation-time findings (things the design Q&A did not anticipate)

Recorded as they were hit, during commits 1–2, so a future reader does not
have to rediscover them by reading a confusing diff or a flaky test.

### 9.1 `TestBreaker_HalfOpenExcludedProbeRefundsSlot` needed NO change — corrected in §6.1

Already folded into §6.1 above; the short version: that test uses a plain
(non-retrying) client, which can never produce the
`ErrExchangeRetriesExhausted` outcome the gate keys on, so it was never at
risk despite the original prediction. A **new** test,
`TestBreaker_HalfOpenRetryAfterGateBlocksNextProbe`
([circuitbreaker_exchange_test.go](../../../internal/tokenexchange/circuitbreaker_exchange_test.go)),
was added instead, using the retrying-client helper
(`newBreakerTestExchanger`) to actually exercise the ticket's motivating
half-open scenario.

### 9.2 A large test Retry-After value can blow the chain deadline via the EXISTING intra-chain honoring — a real interaction, not just a test artifact

While writing the half-open gate test, setting `Retry-After: 30` on the
mock IdP's 429 response caused the trip call to fail with a **network-class**
error (reopening the breaker) instead of exhausting as rate-limited. Root
cause, confirmed by re-reading `idpclient/retrying.go`'s own doc: 429 is
retried, and `RateLimitLinearJitterBackoff` honors the response's
`Retry-After` **uncapped** for the wait *between attempts within the same
chain* (this is pre-existing behavior, untouched by this ticket — see
`NewRetryingHTTPClient`'s doc on `checkRetry`). A `Retry-After: 30` meant the
retry loop itself tried to sleep 30 real seconds before attempt 2, blowing
through the ~19s chain deadline (`ComputeChainDeadline`'s formula) well
before a second attempt could land — producing `context.DeadlineExceeded`,
classified `FailureClassNetwork` (a genuine breaker failure, not excluded),
rather than the intended exhausted-429/`FailureClassRateLimited` path.

**This is not merely a test-authoring gotcha — it is a real, pre-existing
production interaction worth being aware of independent of this ticket:**
an IdP that returns a large `Retry-After` (say, 60s) on its very first 429
will have that value honored uncapped by the intra-chain retry loop before
this ticket's gate (or its cap) ever gets a chance to act — the chain will
either blow its own deadline (reclassifying as a network failure, which
DOES count against the breaker) or, if the chain deadline is generous
enough, simply take a long time to exhaust. **The cap this ticket
introduces (`defaultMaxHonoredRetryAfter` / the future
`max_honored_duration` YAML) only bounds the GATE's window — it does
nothing to bound the intra-chain wait, which remains uncapped exactly as it
was before this ticket, per `idpclient`'s existing, deliberate design.**
Flagging this as worth a decision by whoever owns `idpclient`'s retry
policy: whether the intra-chain honoring should ALSO be capped (symmetric
with this ticket's gate cap) is a legitimate question this ticket's scope
does not cover and does not change today — recorded here so it isn't lost.

Practical consequence for the test: the mock IdP's `Retry-After` header
value must stay comfortably under the chain deadline (used `2`, not `30`)
to actually exercise the exhausted-429 path this ticket's gate depends on;
the *gate's own* honored window in that test is controlled independently
via the pinned `nowFunc`, decoupled from the header's literal value.

### 9.3 Threading `Retry-After`'s parsed value through the exhaustion rewrap required a new `ExchangeError` field

The design anticipated using "the last attempt's Retry-After value" at
exhaustion time, but `classifyRetryOutcome` only ever received the already-
classified `*ExchangeError` (with `HTTPStatus`/`FailureClass`), not the raw
`*http.Response` — `parseIdPResponse` already discards the response after
extracting what it needs, one call frame earlier. Resolved by adding a
`RetryAfterResult *retryAfterResult` field to `ExchangeError`
([errors.go](../../../internal/tokenexchange/errors.go)), populated in
`parseIdPResponse`'s 429 branch and copied forward through the
`ErrExchangeRetriesExhausted` rewrap in `classifyRetryOutcome` — the exact
same "survive the rewrap" treatment already given to `FailureClass`, for
the same structural reason (the sentinel changes on rewrap; a sibling field
is how anything about the underlying cause survives that).

### 9.4 Commit-2 scope boundary: `raiseGateOnExhaustedRateLimit` takes no `brokerAlias` parameter yet

To keep commit 2 (mechanism) and commit 3 (logging) genuinely independent —
not just nominally split while secretly coupled — `raiseGateOnExhaustedRateLimit`
in [retry_after_gate.go](../../../internal/tokenexchange/retry_after_gate.go)
does not thread `brokerAlias` through today; it is added when commit 3 wires
in the actual log calls. A `TODO(SOL-152285 logging commit)` comment marks
the exact seam. (Superseded once commit 3 landed — the parameter was added
exactly as planned, no surprises here.)

### 9.5 Commit-4 bug found during implementation: an explicit `max_honored_duration: 0` was silently indistinguishable from an omitted field

The original commit-4 draft (§3) treated `MaxHonoredDuration *time.Duration`
purely as "pointer lets the translator tell omitted from set", the same as
every field on `IdPCircuitBreakerConfig`. That pattern quietly breaks for
this specific field, because of an asymmetry with `Params.MaxHonoredRetryAfter`
(the runtime landing spot, a **plain** `time.Duration`, not a pointer):
that field's own zero value already carries meaning — "operator didn't
override, use `defaultMaxHonoredRetryAfter`" (see its doc in
[types.go](../../../internal/tokenexchange/types.go)). So an operator who
wrote `max_honored_duration: 0` intending "honor no Retry-After at all"
would have that `0` pass validation, flow through
`resolveMaxHonoredRetryAfter` unchanged, land on `Params.MaxHonoredRetryAfter`
as `0`, and then `Exchanger.clampRetryAfter` would silently read it as "use
the shipped 60s default" — the OPPOSITE of what was configured, with no
error and no log line to reveal the mismatch.

Caught while writing `TestValidateIdPRetryAfter`'s table (originally
included a "zero valid, honors nothing" case) — realized the claimed
behavior didn't match what the runtime code actually does with that value.

**Fix:** `validateIdPRetryAfter`
([idp_retry_after.go](../../../internal/config/idp_retry_after.go)) rejects
zero explicitly (bound changed from `[0, cap]` to `(0, cap]`), with the
reasoning captured in both the validator's doc and
`IdPRetryAfterConfig`'s own doc comment. There is no supported way to
configure "honor nothing" in this ticket's scope — introducing one would
need a second sentinel value (e.g. a distinct negative marker) purely to
distinguish "explicit zero" from "omitted", which is unwarranted complexity
for a case nothing in the ticket asks for. If a future ticket needs that
capability, it is a deliberate scope decision to make then, not an
oversight to silently patch around now.

**Why this is worth flagging as a finding, not just a fixed bug:** it is a
general trap in this codebase's own pointer-field pattern
(`*T` distinguishes omitted-from-set at the CONFIG layer) whenever the
RUNTIME layer's corresponding field is a plain, non-pointer `T` whose zero
value is *also* semantically meaningful. `IdPCircuitBreakerConfig`'s
fields mostly avoid this because their runtime counterparts
(`CircuitBreakerConfig`'s fields) either reject zero already
(`OpenStateDuration <= 0` is invalid) or zero is a legitimate, unambiguous
value in its own right (`ConsecutiveFailureThreshold == 0` genuinely means
"disable this rule", not "use some other default"). This field is the
first one in this area where the two zero-meanings actually collide, and
it will collide again for any future config knob shaped the same way —
worth checking explicitly next time rather than assuming the pointer
pattern is automatically safe.

### 9.6 Two bugs caught by Copilot's automated PR review — both real, both fixed

Copilot's review of PR #221 flagged two issues, both confirmed by re-reading
the actual code before fixing (not accepted on faith):

**Whitespace not trimmed before parsing (`response.go`).** `parseRetryAfter`
fed the raw header value directly to `strconv.ParseInt` and `http.ParseTime`
without trimming. RFC 9110 §5.6.3 allows optional surrounding whitespace
(OWS) around field values, and neither Go's header parsing nor
`go-retryablehttp`'s own `parseRetryAfterHeader` (checked directly, no
trimming there either) guarantees it's stripped before reaching us. A
header like `"120 "` or `" Wed, 21 Oct ..."` would have been treated as
unparseable, silently falling into the "gate not set" path — exactly the
outcome the ticket says an IdP should NOT get punished with when it sent a
perfectly valid header. Fixed by trimming into a separate `trimmed` local
used only for the two parse attempts; `raw` (used for logging) stays
untrimmed so the "gate not set — unparseable" log line still shows exactly
what the IdP sent, unmodified.

**`raiseGate` unconditionally logged "gate set"/"clamped" even when nothing
was raised (`retry_after_gate.go`).** Two distinct ways this fired
incorrectly: (1) a usable Retry-After that floors to a non-positive delay
(`Retry-After: 0`, or a past HTTP-date `parseRetryAfter` already floored to
zero) — `raiseGate`'s own `delay <= 0` guard made it a no-op, but
`raiseGateOnExhaustedRateLimit` logged `logGateSet`/`logGateClamped`
regardless; (2) a genuinely positive delay that loses the CAS-max race to a
concurrent chain's already-later `gatedUntil` — the log line reported this
chain's own (shorter, non-winning) delay and a locally recomputed
`gated_until` as if it had taken effect, when the actual gate state came
from the other chain. Both are logging-correctness bugs, not gating bugs —
`raiseGate` itself was always doing the right thing; only the log calls sitting
downstream of it were wrong. Fixed by changing `raiseGate`'s signature to
return `(effectiveUntil time.Time, raised bool)` and gating the two log
calls on `raised`, so "gate set"/"clamped" only ever fires when this call
is what actually changed `gatedUntil`.

Confirmed both fixes are covered by new tests, not just asserted: read
every existing test in `retry_after_gate_test.go` and the `TestParseRetryAfter`
table in `response_test.go` first to check none of them already exercised
these paths (they didn't — the existing zero-delay test called `raiseGate`
directly, never through `raiseGateOnExhaustedRateLimit`'s logging path, and
no existing case combined a positive delay with a losing CAS). Added
`TestParseRetryAfter_RawPreservesOriginalWhitespace` plus two new
whitespace cases in the parsing table, and
`TestRaiseGateOnExhaustedRateLimit_ZeroDelayLogsNothing` /
`TestRaiseGateOnExhaustedRateLimit_LosingCASLogsNothing` for the logging fix
— both fail against the pre-fix code (verified by reading the diff), so
they're real regression guards, not vacuous additions.

### 9.7 A fourth review round found `New()` accepted a negative `MaxHonoredRetryAfter` unvalidated

After the integer-overflow fix (§9.6) and a clean independent line-by-line
review, a further review comment caught one more real gap: `New()`
validates `HTTPClient`, `Cache`, and `CircuitBreaker`, but copied
`Params.MaxHonoredRetryAfter` straight onto the `Exchanger` with no check
at all. `clampRetryAfter`'s existing `ceiling <= 0` fallback (deliberately
`<= 0`, not `== 0`, precisely because several existing tests construct
`&Exchanger{...}` literals directly, bypassing `New()` entirely — checked
via grep before deciding *not* to tighten it) means a negative value is
silently treated the same as the documented "use the default" zero
sentinel. The production config path can never produce this
(`validateIdPRetryAfter` already rejects `<= 0` at the YAML layer,
confirmed by re-reading it), so the actual exposure is narrower than it
first sounds: a caller building `Params{}` directly — today, only tests;
potentially a future non-config caller — could pass a negative value
and never learn their input was nonsensical, because it would just quietly
behave like zero.

**Fix:** `New()` now rejects `MaxHonoredRetryAfter < 0` outright (zero
still accepted, since it's the documented sentinel), mirroring the existing
pattern of validating what the config layer's validator cannot see. Two new
tests pin both sides: `TestNew_MaxHonoredRetryAfterNegativeRejected` and
`TestNew_MaxHonoredRetryAfterZeroAccepted`, placed alongside the existing
`TestNew_HTTPClientNilRejected`/`TestNew_CacheNilRejected` pair in
`exchanger_test.go` rather than in the Retry-After-specific test files,
since this is fundamentally a `New()` constructor-validation concern, not a
gate-mechanism one.
