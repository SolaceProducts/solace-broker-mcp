// Copyright 2024-2026 Solace Corporation. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tokenexchange

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/idpclient"
	"github.com/sony/gobreaker/v2"
)

// fastTripBreakerConfig trips after a single consecutive failure and stays
// open long enough that a follow-up call in the same test is rejected without
// racing the recovery timer. Small numbers keep the tests fast and
// deterministic.
func fastTripBreakerConfig() CircuitBreakerConfig {
	c := DefaultCircuitBreakerConfig()
	c.ConsecutiveFailureThreshold = 1
	c.OpenStateDuration = time.Hour
	return c
}

// newBreakerTestExchangerPlain builds a breaker-enabled exchanger backed by a
// PLAIN (non-retrying) *http.Client, so one 5xx is one fast logical failure
// with no retry backoff. Used by the concurrency tests, where the point is the
// breaker's shared-counter behavior under contention, not retry timing —
// dragging each goroutine through the 1-2s backoff would only slow the test
// without exercising anything new (retry-collapse is covered elsewhere).
func newBreakerTestExchangerPlain(t *testing.T, serverURL string, cbCfg CircuitBreakerConfig) *Exchanger {
	t.Helper()
	p := validParams(t)
	p.TokenURL = serverURL
	p.HTTPClient = &http.Client{}
	p.CircuitBreaker = &cbCfg
	e, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// newBreakerTestExchanger builds a retrying exchanger with the breaker enabled
// from the given config. Per-attempt timeout is short; retry knobs use the
// package defaults so a persistent 5xx exhausts to a single logical failure.
func newBreakerTestExchanger(t *testing.T, serverURL string, cbCfg CircuitBreakerConfig) *Exchanger {
	t.Helper()
	client, err := idpclient.NewRetryingHTTPClient(
		idpclient.RetryOptions{
			MaxRetries:   DefaultMaxRetries,
			RetryWaitMin: DefaultRetryWaitMin,
			RetryWaitMax: DefaultRetryWaitMax,
		},
		idpclient.WithTimeout(500*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewRetryingHTTPClient: %v", err)
	}
	p := validParams(t)
	p.TokenURL = serverURL
	p.HTTPClient = client
	p.CircuitBreaker = &cbCfg
	e, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// TestBreaker_OneLogicalOperationCountsOnce is the headline guarantee: a single
// Exchange that retries 5xx three times before giving up must register as ONE
// breaker failure, not three. This is what makes the configured thresholds mean
// "logical operations" rather than "HTTP attempts".
func TestBreaker_OneLogicalOperationCountsOnce(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	// High threshold so the breaker does NOT trip — we want to read the raw
	// count, not observe an open circuit.
	cfg := DefaultCircuitBreakerConfig()
	cfg.ConsecutiveFailureThreshold = 100
	e := newBreakerTestExchanger(t, srv.URL, cfg)

	_, err := e.Exchange(context.Background(), validInput())
	if !errors.Is(err, ErrExchangeRetriesExhausted) {
		t.Fatalf("err = %v, want ErrExchangeRetriesExhausted", err)
	}

	if got := hits.Load(); got != 3 {
		t.Errorf("server hits = %d, want 3 (retry loop tried all attempts)", got)
	}
	counts := e.breaker.Counts()
	if counts.TotalFailures != 1 {
		t.Errorf("breaker TotalFailures = %d, want 1 (one logical operation, not per-attempt)", counts.TotalFailures)
	}
	if counts.Requests != 1 {
		t.Errorf("breaker Requests = %d, want 1", counts.Requests)
	}
}

// TestBreaker_RateLimitExcludedNotCounted asserts a 429 outcome is excluded:
// it counts as neither success nor failure, so throttling cannot trip the
// shared breaker.
func TestBreaker_RateLimitExcludedNotCounted(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	cfg := DefaultCircuitBreakerConfig()
	cfg.ConsecutiveFailureThreshold = 1 // would trip on a single COUNTED failure
	e := newBreakerTestExchanger(t, srv.URL, cfg)

	_, err := e.Exchange(context.Background(), validInput())
	if !errors.Is(err, ErrExchangeRetriesExhausted) {
		t.Fatalf("err = %v, want ErrExchangeRetriesExhausted", err)
	}

	counts := e.breaker.Counts()
	if counts.TotalFailures != 0 {
		t.Errorf("breaker TotalFailures = %d, want 0 (429 must be excluded)", counts.TotalFailures)
	}
	if counts.TotalExclusions != 1 {
		t.Errorf("breaker TotalExclusions = %d, want 1", counts.TotalExclusions)
	}
	if e.breaker.State() != gobreaker.StateClosed {
		t.Errorf("breaker State = %v, want closed (a 429 must not trip it)", e.breaker.State())
	}
}

// TestBreaker_ConfigFaultExcludedNotCounted is the end-to-end proof of the
// spec decision that an endpoint MISCONFIGURATION must not trip the breaker.
// An httptest TLS server presents a cert signed by an unknown authority; the
// plain client does not trust it, so httpClient.Do fails with an
// x509.UnknownAuthorityError — exactly the "invalid/expired TLS certificate"
// case situations.md says must NOT count. Even with the breaker set to trip on
// a single counted failure, repeated attempts must leave it CLOSED, because a
// config fault is excluded: one operator's bad cert must never fast-fail every
// tenant.
func TestBreaker_ConfigFaultExcludedNotCounted(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // never reached — TLS handshake fails first
	}))
	defer srv.Close()

	// Plain client (no custom RootCAs) → the server's self-signed cert is
	// untrusted → x509.UnknownAuthorityError on every attempt.
	cfg := DefaultCircuitBreakerConfig()
	cfg.ConsecutiveFailureThreshold = 1 // would trip immediately on a COUNTED failure
	e := newBreakerTestExchangerPlain(t, srv.URL, cfg)

	const attempts = 5
	for i := 0; i < attempts; i++ {
		in := validInput()
		in.SubjectToken = fmt.Sprintf("cfg-fault-%d", i)
		_, err := e.Exchange(context.Background(), in)
		if !errors.Is(err, ErrExchangeTransport) {
			t.Fatalf("attempt %d: err = %v, want ErrExchangeTransport", i, err)
		}
		var exchErr *ExchangeError
		if errors.As(err, &exchErr) && exchErr.FailureClass != FailureClassConfig {
			t.Fatalf("attempt %d: FailureClass = %v, want Config (untrusted TLS cert)", i, exchErr.FailureClass)
		}
	}

	counts := e.breaker.Counts()
	if counts.TotalFailures != 0 {
		t.Errorf("breaker TotalFailures = %d, want 0 (a TLS config fault must be excluded)", counts.TotalFailures)
	}
	if counts.TotalExclusions != attempts {
		t.Errorf("breaker TotalExclusions = %d, want %d", counts.TotalExclusions, attempts)
	}
	if e.breaker.State() != gobreaker.StateClosed {
		t.Errorf("breaker State = %v, want closed (a config fault must never trip the shared breaker)", e.breaker.State())
	}
}

// TestBreaker_OpenStateFailsFast asserts that once the breaker trips open, the
// next Exchange is rejected with ErrExchangeCircuitOpen WITHOUT reaching the
// IdP (server hit count does not increase).
func TestBreaker_OpenStateFailsFast(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	e := newBreakerTestExchanger(t, srv.URL, fastTripBreakerConfig())

	// First call trips the breaker (1 consecutive failure).
	_, err := e.Exchange(context.Background(), validInput())
	if !errors.Is(err, ErrExchangeRetriesExhausted) {
		t.Fatalf("first call err = %v, want ErrExchangeRetriesExhausted", err)
	}
	if e.breaker.State() != gobreaker.StateOpen {
		t.Fatalf("breaker State = %v, want open after a tripping failure", e.breaker.State())
	}
	hitsAfterTrip := hits.Load()

	// Second call must be rejected by the open breaker without hitting the IdP.
	_, err = e.Exchange(context.Background(), validInput())
	if !errors.Is(err, ErrExchangeCircuitOpen) {
		t.Errorf("second call err = %v, want ErrExchangeCircuitOpen", err)
	}
	if got := hits.Load(); got != hitsAfterTrip {
		t.Errorf("server hits rose from %d to %d; open breaker must not call the IdP", hitsAfterTrip, got)
	}
}

// TestBreaker_OpenStateErrorIsTransientAndEnriched confirms the open-state
// error is a fully enriched *ExchangeError classified transient (so the agent
// backs off rather than being told a specific broker is broken).
func TestBreaker_OpenStateErrorIsTransientAndEnriched(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	e := newBreakerTestExchanger(t, srv.URL, fastTripBreakerConfig())
	_, _ = e.Exchange(context.Background(), validInput()) // trip it

	_, err := e.Exchange(context.Background(), validInput())
	var exchErr *ExchangeError
	if !errors.As(err, &exchErr) {
		t.Fatalf("open-state err is not *ExchangeError: %v", err)
	}
	if exchErr.BrokerAlias != validInput().BrokerAlias {
		t.Errorf("BrokerAlias = %q, want enriched %q", exchErr.BrokerAlias, validInput().BrokerAlias)
	}
	if exchErr.TokenEndpoint != e.tokenURL {
		t.Errorf("TokenEndpoint = %q, want enriched %q", exchErr.TokenEndpoint, e.tokenURL)
	}
	// Transient class: not broker-named, "try again" message.
	const wantTransient = "Authentication is unavailable — the identity provider is not responding."
	if got := exchErr.AgentMessage(validInput().BrokerAlias); got != wantTransient {
		t.Errorf("AgentMessage = %q, want transient %q", got, wantTransient)
	}
	// The structured breaker_state marker must survive enrichment, so an
	// operator can filter for breaker-fast-failed calls without parsing the
	// message. No failure_class: no IdP call was attempted.
	if !hasLogAttr(exchErr.LogAttrs(), "breaker_state", "open") {
		t.Errorf("LogAttrs missing breaker_state=open on enriched open-state error; got %v", exchErr.LogAttrs())
	}
}

// TestBreaker_DisabledCallsIdPAndRetries asserts the escape hatch: with the
// breaker disabled (nil config), repeated failures never fast-fail — every
// call reaches the IdP and retries run.
func TestBreaker_DisabledCallsIdPAndRetries(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	// newRetryingTestExchanger leaves CircuitBreaker nil → disabled.
	e := newRetryingTestExchanger(t, srv.URL)
	if e.breaker != nil {
		t.Fatal("expected nil breaker when CircuitBreaker config is nil")
	}

	for i := 0; i < 2; i++ {
		_, err := e.Exchange(context.Background(), validInput())
		if !errors.Is(err, ErrExchangeRetriesExhausted) {
			t.Fatalf("call %d err = %v, want ErrExchangeRetriesExhausted (never circuit-open)", i, err)
		}
	}
	// 2 logical calls × 3 attempts each = 6 IdP hits; nothing was short-circuited.
	if got := hits.Load(); got != 6 {
		t.Errorf("server hits = %d, want 6 (disabled breaker never fast-fails)", got)
	}
}

// TestBreaker_SingleflightBurstCountsOnce asserts that a burst of concurrent
// identical exchanges — collapsed by singleflight into one IdP call — registers
// exactly one breaker outcome, not one per caller.
func TestBreaker_SingleflightBurstCountsOnce(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		<-release // hold the winner open so late callers join the same flight
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := DefaultCircuitBreakerConfig()
	cfg.ConsecutiveFailureThreshold = 100 // don't trip; we want to read counts
	e := newBreakerTestExchanger(t, srv.URL, cfg)

	const callers = 8
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			_, _ = e.Exchange(context.Background(), validInput())
		}()
	}
	// Give the goroutines a moment to coalesce on the singleflight key, then
	// release the handler. (The handler blocks on the first hit, so waiters
	// that arrive after join the in-flight call rather than starting their own.)
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()

	counts := e.breaker.Counts()
	if counts.Requests != 1 {
		t.Errorf("breaker Requests = %d, want 1 (singleflight collapses the burst into one outcome)", counts.Requests)
	}
	if counts.TotalFailures != 1 {
		t.Errorf("breaker TotalFailures = %d, want 1", counts.TotalFailures)
	}
}

// TestBreaker_ConcurrentDistinctKeysCountExactlyOncePerCall is the shared-
// counter integrity test for the "multiple users, multiple requests" case.
//
// Unlike the singleflight-burst test (one key → collapsed to one outcome),
// every goroutine here uses a DISTINCT subject token, so each produces a
// distinct dedup key, misses singleflight, and independently calls
// breaker.Execute. That drives N goroutines onto the one process-wide breaker
// at the same instant — the scenario where a lost update or a data race in the
// shared counter would show up. The breaker threshold is set high enough that
// it never trips, so every one of the N failures must be recorded (none
// rejected early), and the count must be EXACTLY N: not N-k (a dropped update)
// nor >N (double count). Run under -race (make check) so a torn write is caught
// even if the arithmetic happened to land right.
func TestBreaker_ConcurrentDistinctKeysCountExactlyOncePerCall(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	// Pin the breaker so NO trip rule can fire — the test needs every one of
	// the N outcomes recorded, none rejected by a partial open:
	//   - consecutive rule disabled (0),
	//   - rate rule starved (minimum requests unreachable),
	//   - threshold pinned high too, so no default can surprise the test.
	// And pin the counting window far longer than the test runs: if the
	// rolling window rotated mid-test, old buckets would age out and Counts()
	// could read < N with no bug in the breaker — a latent flake. 1h guarantees
	// a single window for the whole run.
	cfg := DefaultCircuitBreakerConfig()
	cfg.ConsecutiveFailureThreshold = 0
	cfg.MinimumRequests = 1_000_000 // the max allowed; far above N so the rate rule never trips
	cfg.FailureRateThresholdPercent = 100
	cfg.FailureRateWindow = time.Hour
	e := newBreakerTestExchangerPlain(t, srv.URL, cfg)

	const callers = 50
	var (
		wg    sync.WaitGroup
		start = make(chan struct{}) // start barrier: release all goroutines at once
	)
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			in := validInput()
			// Distinct subject token per goroutine → distinct dedup key →
			// no singleflight collapse → its own breaker outcome.
			in.SubjectToken = fmt.Sprintf("subject-token-%d", i)
			<-start // block until every goroutine is spawned and ready
			_, err := e.Exchange(context.Background(), in)
			if !errors.Is(err, ErrExchangeTransport) {
				t.Errorf("caller %d: err = %v, want ErrExchangeTransport (breaker stayed closed)", i, err)
			}
		}(i)
	}
	// All goroutines are parked on <-start; releasing here makes them contend
	// on the shared breaker simultaneously rather than trickling through (a
	// plain launch loop can let goroutine 0 finish before 49 is even spawned,
	// so "concurrent" would be aspirational without this barrier).
	close(start)
	wg.Wait()

	counts := e.breaker.Counts()
	if counts.Requests != callers {
		t.Errorf("breaker Requests = %d, want %d (one per distinct-key logical operation)", counts.Requests, callers)
	}
	if counts.TotalFailures != callers {
		t.Errorf("breaker TotalFailures = %d, want %d (every concurrent failure recorded exactly once)", counts.TotalFailures, callers)
	}
	if counts.TotalSuccesses != 0 {
		t.Errorf("breaker TotalSuccesses = %d, want 0 (all calls failed)", counts.TotalSuccesses)
	}
	if e.breaker.State() != gobreaker.StateClosed {
		t.Errorf("breaker State = %v, want closed (thresholds set so it never trips)", e.breaker.State())
	}
	if got := hits.Load(); got != callers {
		t.Errorf("server hits = %d, want %d (distinct keys must not collapse; one plain attempt each)", got, callers)
	}
}

// ---------- SOL-151600: half-open recovery coverage ----------
//
// The tests below exercise the open -> half-open -> closed recovery arc,
// which TestBreaker_OpenStateFailsFast and friends above never touch (they
// pin OpenStateDuration to an hour specifically to stay open). gobreaker
// v2.4.0 has no fake-clock hook, so recovery must be observed through a real
// wall-clock wait; recoveryBreakerConfig keeps that wait short (250ms) while
// starving every OTHER trip/decay rule so the only thing moving the breaker
// is the consecutive-failure rule and the half-open probe budget.

// recoveryBreakerConfig trips on a single failure and opens for only 250ms,
// so tests can observe the half-open transition without a long sleep. The
// rate rule and bucket decay are starved/frozen (MinimumRequests far above
// anything a test drives, FailureRateWindow an hour) so only the consecutive
// rule and the half-open probe budget are in play — nothing else can trip or
// heal the breaker out from under an assertion.
func recoveryBreakerConfig() CircuitBreakerConfig {
	c := DefaultCircuitBreakerConfig()
	c.ConsecutiveFailureThreshold = 1
	c.MinimumRequests = 1_000_000
	c.FailureRateWindow = time.Hour
	c.OpenStateDuration = 250 * time.Millisecond
	c.HalfOpenProbeRequests = 2
	return c
}

// waitForBreakerState polls State() until it equals want or the deadline
// expires. Polling is safe here specifically because the open -> half-open
// transition in gobreaker v2.4.0 is LAZY and LATCHING: it is evaluated
// inside State()/beforeRequest() only when the open timer has elapsed, and
// half-open does not expire on its own (there is no background timer). So
// there is no window in which the transition can fire and be missed between
// polls — a slow machine only delays observing it, it cannot skip past it.
func waitForBreakerState(t *testing.T, cb interface{ State() gobreaker.State }, want gobreaker.State) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := cb.State(); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("breaker State = %v after 10s, want %v", cb.State(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// inputWithSubject returns a validInput() clone with a distinct SubjectToken.
// Every logical probe in the tests below must use its own token: the token
// cache would otherwise serve a repeat SubjectToken straight from cache
// (never reaching the breaker), and singleflight would collapse concurrent
// calls sharing a token into one breaker outcome.
func inputWithSubject(s string) ExchangeInput {
	in := validInput()
	in.SubjectToken = s
	return in
}

// TestBreaker_RecoveryClosesAfterConsecutiveProbeSuccesses pins the full
// recovery arc: an open breaker only closes after HalfOpenProbeRequests
// consecutive successful probes, not after the first one.
func TestBreaker_RecoveryClosesAfterConsecutiveProbeSuccesses(t *testing.T) {
	t.Parallel()
	var (
		hits    atomic.Int32
		healthy atomic.Bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("recovered-tok", 3600))
	}))
	defer srv.Close()

	cfg := recoveryBreakerConfig()
	e := newBreakerTestExchangerPlain(t, srv.URL, cfg)

	// Unhealthy: one failure trips the breaker (ConsecutiveFailureThreshold=1).
	_, err := e.Exchange(context.Background(), inputWithSubject("recovery-trip"))
	if !errors.Is(err, ErrExchangeTransport) {
		t.Fatalf("trip call err = %v, want ErrExchangeTransport", err)
	}
	if e.breaker.State() != gobreaker.StateOpen {
		t.Fatalf("breaker State = %v after tripping failure, want open", e.breaker.State())
	}

	// IdP recovers; wait out the open timer for the lazy/latching transition.
	healthy.Store(true)
	waitForBreakerState(t, e.breaker, gobreaker.StateHalfOpen)

	// Probe 1 of 2: must succeed and reach the IdP, but must NOT close the
	// breaker on its own — HalfOpenProbeRequests=2 requires two in a row.
	hitsBeforeProbe1 := hits.Load()
	_, err = e.Exchange(context.Background(), inputWithSubject("recovery-probe-1"))
	if err != nil {
		t.Fatalf("probe 1 err = %v, want nil (IdP is healthy)", err)
	}
	if got := hits.Load(); got != hitsBeforeProbe1+1 {
		t.Errorf("hits after probe 1 = %d, want %d (probe must reach the IdP)", got, hitsBeforeProbe1+1)
	}
	if got := e.breaker.State(); got != gobreaker.StateHalfOpen {
		t.Errorf("breaker State after 1 probe success = %v, want half-open (needs 2 consecutive)", got)
	}
	if got := e.breaker.Counts().ConsecutiveSuccesses; got != 1 {
		t.Errorf("ConsecutiveSuccesses after probe 1 = %d, want 1", got)
	}

	// Probe 2 of 2: second consecutive success closes the breaker.
	_, err = e.Exchange(context.Background(), inputWithSubject("recovery-probe-2"))
	if err != nil {
		t.Fatalf("probe 2 err = %v, want nil", err)
	}
	if got := e.breaker.State(); got != gobreaker.StateClosed {
		t.Errorf("breaker State after 2 consecutive probe successes = %v, want closed", got)
	}

	// Post-recovery: an ordinary call succeeds and reaches the IdP normally.
	hitsBeforePostRecovery := hits.Load()
	_, err = e.Exchange(context.Background(), inputWithSubject("recovery-post"))
	if err != nil {
		t.Fatalf("post-recovery call err = %v, want nil", err)
	}
	if got := hits.Load(); got != hitsBeforePostRecovery+1 {
		t.Errorf("hits after post-recovery call = %d, want %d", got, hitsBeforePostRecovery+1)
	}
}

// TestBreaker_HalfOpenProbeFailureReopens asserts that a single failed probe
// in half-open reopens the breaker immediately (it does not tolerate one
// failure the way the consecutive-success rule tolerates partial recovery).
func TestBreaker_HalfOpenProbeFailureReopens(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable) // unhealthy throughout
	}))
	defer srv.Close()

	cfg := recoveryBreakerConfig()
	e := newBreakerTestExchangerPlain(t, srv.URL, cfg)

	_, err := e.Exchange(context.Background(), inputWithSubject("reopen-trip"))
	if !errors.Is(err, ErrExchangeTransport) {
		t.Fatalf("trip call err = %v, want ErrExchangeTransport", err)
	}
	if e.breaker.State() != gobreaker.StateOpen {
		t.Fatalf("breaker State = %v after tripping failure, want open", e.breaker.State())
	}

	waitForBreakerState(t, e.breaker, gobreaker.StateHalfOpen)

	hitsBeforeProbe := hits.Load()
	probeStart := time.Now()
	_, err = e.Exchange(context.Background(), inputWithSubject("reopen-probe"))
	if !errors.Is(err, ErrExchangeTransport) {
		t.Fatalf("probe err = %v, want ErrExchangeTransport", err)
	}
	if got := hits.Load(); got != hitsBeforeProbe+1 {
		t.Errorf("hits after probe = %d, want %d (probe must genuinely reach the IdP, not fast-fail)", got, hitsBeforeProbe+1)
	}

	// Two-tier reopen assertion. The failed probe both reopens the breaker
	// AND restarts the open timer, so a slow/stalled test goroutine could in
	// principle observe the timer having already re-expired back to
	// half-open by the time we check. That is still a healthy outcome (not a
	// bug), so we only require StateOpen exactly when we know we checked
	// well within the fresh OpenStateDuration window; otherwise the only hard
	// requirement is that it did NOT close — closed would mean the failed
	// probe was wrongly treated as a success or failed to reopen the breaker.
	s := e.breaker.State()
	if s == gobreaker.StateClosed {
		t.Fatalf("breaker State = closed after a failed half-open probe, want open (or half-open if the timer already re-expired)")
	}
	if time.Since(probeStart) < cfg.OpenStateDuration {
		if s != gobreaker.StateOpen {
			t.Errorf("breaker State = %v within the fresh open window, want open (failed probe must reopen immediately)", s)
		}
	}
}

// TestBreaker_HalfOpenRejectsBeyondProbeBudget asserts the half-open probe
// budget is enforced under concurrency: with HalfOpenProbeRequests=2, a third
// concurrent caller is rejected without reaching the IdP while two probes are
// already in flight, and the breaker still closes once those two succeed.
func TestBreaker_HalfOpenRejectsBeyondProbeBudget(t *testing.T) {
	t.Parallel()
	var (
		hits    atomic.Int32
		healthy atomic.Bool
		entered = make(chan struct{}, 2)
		release = make(chan struct{})
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			hits.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		entered <- struct{}{}
		<-release
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("budget-tok", 3600))
	}))
	defer srv.Close()

	cfg := recoveryBreakerConfig()
	e := newBreakerTestExchangerPlain(t, srv.URL, cfg)

	_, err := e.Exchange(context.Background(), inputWithSubject("budget-trip"))
	if !errors.Is(err, ErrExchangeTransport) {
		t.Fatalf("trip call err = %v, want ErrExchangeTransport", err)
	}

	healthy.Store(true)
	waitForBreakerState(t, e.breaker, gobreaker.StateHalfOpen)

	var (
		wg         sync.WaitGroup
		errA, errB error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errA = e.Exchange(context.Background(), inputWithSubject("budget-probe-a"))
	}()
	go func() {
		defer wg.Done()
		_, errB = e.Exchange(context.Background(), inputWithSubject("budget-probe-b"))
	}()

	// Deterministic barrier: both probe slots are provably held once we've
	// received twice from entered. No sleeps — a sleep-based "give it time to
	// start" wait would be both slower and capable of flaking.
	<-entered
	<-entered

	hitsBeforeThird := hits.Load()
	_, errC := e.Exchange(context.Background(), inputWithSubject("budget-probe-c"))
	if !errors.Is(errC, ErrExchangeCircuitOpen) {
		t.Fatalf("third caller err = %v, want ErrExchangeCircuitOpen (probe budget of 2 already spent)", errC)
	}
	if got := hits.Load(); got != hitsBeforeThird {
		t.Errorf("hits rose from %d to %d; the rejected third caller must never reach the IdP", hitsBeforeThird, got)
	}

	close(release)
	wg.Wait()

	if errA != nil {
		t.Errorf("probe A err = %v, want nil", errA)
	}
	if errB != nil {
		t.Errorf("probe B err = %v, want nil", errB)
	}
	if got := e.breaker.State(); got != gobreaker.StateClosed {
		t.Errorf("breaker State after both probes succeeded = %v, want closed", got)
	}
}

// TestBreaker_HalfOpenExcludedProbeRefundsSlot asserts a 429 during half-open
// is excluded rather than counted: it must not reopen the breaker and must
// not consume a probe slot, so the next two genuine successes still close it.
func TestBreaker_HalfOpenExcludedProbeRefundsSlot(t *testing.T) {
	t.Parallel()
	var mode atomic.Int32 // 0=unhealthy(503), 1=rate-limited(429), 2=healthy(200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch mode.Load() {
		case 1:
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, successJSON("refund-tok", 3600))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	cfg := recoveryBreakerConfig()
	e := newBreakerTestExchangerPlain(t, srv.URL, cfg)

	_, err := e.Exchange(context.Background(), inputWithSubject("refund-trip"))
	if !errors.Is(err, ErrExchangeTransport) {
		t.Fatalf("trip call err = %v, want ErrExchangeTransport", err)
	}

	mode.Store(1)
	waitForBreakerState(t, e.breaker, gobreaker.StateHalfOpen)

	_, err = e.Exchange(context.Background(), inputWithSubject("refund-429"))
	if err == nil {
		t.Fatal("429 probe err = nil, want a rejection error")
	}
	if got := e.breaker.State(); got != gobreaker.StateHalfOpen {
		t.Errorf("breaker State after excluded 429 probe = %v, want half-open (exclusion must not reopen)", got)
	}
	if got := e.breaker.Counts().TotalExclusions; got != 1 {
		t.Errorf("TotalExclusions after 429 probe = %d, want 1", got)
	}

	// The excluded probe must not have consumed budget: two genuine
	// successes now still close the breaker exactly as in the plain
	// recovery case.
	mode.Store(2)
	_, err = e.Exchange(context.Background(), inputWithSubject("refund-probe-1"))
	if err != nil {
		t.Fatalf("healthy probe 1 err = %v, want nil", err)
	}
	if got := e.breaker.State(); got != gobreaker.StateHalfOpen {
		t.Errorf("breaker State after 1 healthy probe = %v, want half-open (still needs 2 consecutive)", got)
	}

	_, err = e.Exchange(context.Background(), inputWithSubject("refund-probe-2"))
	if err != nil {
		t.Fatalf("healthy probe 2 err = %v, want nil", err)
	}
	if got := e.breaker.State(); got != gobreaker.StateClosed {
		t.Errorf("breaker State after 2 consecutive healthy probes = %v, want closed", got)
	}
}

// Uses a RETRYING client (unlike TestBreaker_HalfOpenExcludedProbeRefundsSlot's
// plain client, which never exhausts — see that test's doc).
func TestBreaker_HalfOpenRetryAfterGateBlocksNextProbe(t *testing.T) {
	t.Parallel()
	var mode atomic.Int32 // 0=unhealthy(503), 1=rate-limited(429 + Retry-After), 2=healthy(200)
	var rateLimitedHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch mode.Load() {
		case 1:
			rateLimitedHits.Add(1)
			// Small on purpose: idpclient's retry loop honors Retry-After
			// UNCAPPED between attempts, so a larger value here would blow
			// the ~19s chain deadline before exhaustion and misclassify as
			// a real breaker failure instead of rate-limited.
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, successJSON("gate-tok", 3600))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	cfg := recoveryBreakerConfig()
	e := newBreakerTestExchanger(t, srv.URL, cfg)

	// Only the gate's clock is pinned; idpclient's own inter-attempt backoff
	// has no injectable clock and still runs on real time.
	gateNow := time.Now()
	e.nowFunc = func() time.Time { return gateNow }

	_, err := e.Exchange(context.Background(), inputWithSubject("gate-trip"))
	// Retrying client, so a persistent 503 exhausts to ErrExchangeRetriesExhausted.
	if !errors.Is(err, ErrExchangeRetriesExhausted) {
		t.Fatalf("trip call err = %v, want ErrExchangeRetriesExhausted", err)
	}

	mode.Store(1)
	waitForBreakerState(t, e.breaker, gobreaker.StateHalfOpen)

	_, err = e.Exchange(context.Background(), inputWithSubject("gate-429-probe"))
	if !errors.Is(err, ErrExchangeRetriesExhausted) {
		t.Fatalf("429 probe err = %v, want ErrExchangeRetriesExhausted", err)
	}
	if got := rateLimitedHits.Load(); got < 2 {
		t.Fatalf("rate-limited hits = %d, want the chain to have retried at least once before exhausting", got)
	}
	if got := e.breaker.State(); got != gobreaker.StateHalfOpen {
		t.Errorf("breaker State after exhausted 429 probe = %v, want half-open (excluded, must not reopen)", got)
	}

	hitsBeforeGatedCall := rateLimitedHits.Load()
	_, err = e.Exchange(context.Background(), inputWithSubject("gate-blocked"))
	if !errors.Is(err, ErrExchangeRateLimited) {
		t.Fatalf("gated call err = %v, want ErrExchangeRateLimited", err)
	}
	if got := rateLimitedHits.Load(); got != hitsBeforeGatedCall {
		t.Errorf("rate-limited hits changed from %d to %d — gated call must not reach the IdP", hitsBeforeGatedCall, got)
	}
	if got := e.breaker.State(); got != gobreaker.StateHalfOpen {
		t.Errorf("breaker State after gated rejection = %v, want unchanged half-open (gate check runs before the breaker)", got)
	}

	gateNow = gateNow.Add(3 * time.Second)
	mode.Store(2)

	_, err = e.Exchange(context.Background(), inputWithSubject("gate-probe-1"))
	if err != nil {
		t.Fatalf("post-gate probe 1 err = %v, want nil", err)
	}
	if got := e.breaker.State(); got != gobreaker.StateHalfOpen {
		t.Errorf("breaker State after 1 healthy probe = %v, want half-open (still needs 2 consecutive)", got)
	}

	_, err = e.Exchange(context.Background(), inputWithSubject("gate-probe-2"))
	if err != nil {
		t.Fatalf("post-gate probe 2 err = %v, want nil", err)
	}
	if got := e.breaker.State(); got != gobreaker.StateClosed {
		t.Errorf("breaker State after 2 consecutive healthy probes = %v, want closed", got)
	}
}

// TestBreaker_OpenBreakerRejectsStampedeCleanly checks the OTHER concurrency
// risk: many goroutines hitting an ALREADY-OPEN breaker at once. The rejection
// path (breaker.Execute short-circuits → mapCircuitOpen converts the library
// error → enrichment) runs concurrently for every caller; a data race or a
// bypass there would surface here.
//
// The breaker is tripped FIRST, serially, then the stampede is released against
// the open state. This is deliberate: trying to trip the breaker *during* the
// stampede is not deterministic — with a fast client the whole flood can pass
// the closed-state check before the first failures register, so "some got
// rejected" cannot be guaranteed. Concurrent RECORDING while closed is already
// covered by the exact-count test; this isolates concurrent REJECTION while
// open, which is what can be asserted without flake.
//
// Once open, EVERY caller must be rejected identically: error is
// ErrExchangeCircuitOpen, never a raw gobreaker error (proves mapCircuitOpen
// holds under contention), never nil; and the IdP is not touched at all. Run
// under -race (make check) so a race in the reject path is caught even when the
// counts happen to line up.
func TestBreaker_OpenBreakerRejectsStampedeCleanly(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cfg := DefaultCircuitBreakerConfig()
	cfg.ConsecutiveFailureThreshold = 3
	cfg.MinimumRequests = 1_000_000 // max allowed; isolates the trip to the consecutive rule
	cfg.OpenStateDuration = time.Hour
	e := newBreakerTestExchangerPlain(t, srv.URL, cfg)

	// Phase 1: trip the breaker serially with distinct-key failures.
	for i := 0; i < 3; i++ {
		in := validInput()
		in.SubjectToken = fmt.Sprintf("trip-%d", i)
		_, _ = e.Exchange(context.Background(), in)
	}
	if e.breaker.State() != gobreaker.StateOpen {
		t.Fatalf("breaker State = %v after 3 consecutive failures, want open", e.breaker.State())
	}
	hitsAfterTrip := hits.Load()

	// Phase 2: stampede the now-open breaker.
	const callers = 50
	var (
		wg          sync.WaitGroup
		rawLeak     atomic.Int32
		nilErr      atomic.Int32
		circuitOpen atomic.Int32
		other       atomic.Int32
	)
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			in := validInput()
			in.SubjectToken = fmt.Sprintf("stampede-%d", i)
			_, err := e.Exchange(context.Background(), in)
			switch {
			case err == nil:
				nilErr.Add(1)
			case errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests):
				rawLeak.Add(1) // raw library error escaped mapCircuitOpen
			case errors.Is(err, ErrExchangeCircuitOpen):
				circuitOpen.Add(1)
			default:
				other.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if got := rawLeak.Load(); got != 0 {
		t.Errorf("raw gobreaker error leaked to %d caller(s); mapCircuitOpen must convert every open-state rejection", got)
	}
	if got := nilErr.Load(); got != 0 {
		t.Errorf("%d caller(s) got a nil error; an open breaker must reject with an error", got)
	}
	if got := other.Load(); got != 0 {
		t.Errorf("%d caller(s) got an unexpected error class", got)
	}
	if got := circuitOpen.Load(); got != callers {
		t.Errorf("ErrExchangeCircuitOpen count = %d, want %d (every caller must be rejected by the open breaker)", got, callers)
	}
	if got := hits.Load(); got != hitsAfterTrip {
		t.Errorf("server hits rose from %d to %d during the stampede; an open breaker must not call the IdP", hitsAfterTrip, got)
	}
}

// ---------- SOL-152286: undecayed consecutive-failure counter ----------
//
// gobreaker's own ConsecutiveFailures decays as its rolling window's buckets
// age out, so a slow, low-traffic outage can leave it permanently below
// threshold even though every exchange failed. The tests below drive
// failures spaced wide enough (relative to a deliberately tiny window) that
// the OLD decayed counter would never reach the threshold, proving the new
// counter is what actually trips the breaker.

// sparseSpacingBreakerConfig uses a 100ms window (10ms buckets) so failures
// spaced ~30ms apart cross 3+ bucket boundaries between each one — wide
// enough that gobreaker's own bucket-decayed ConsecutiveFailures peaks at 4,
// below the threshold of 5 (confirmed by simulating gobreaker's
// rolling-bucket arithmetic at this exact geometry; sleep jitter only widens
// the spacing, which decays the old counter harder, so the margin is safe in
// the only direction timing can drift). MinimumRequests is starved so only
// the consecutive rule can trip.
func sparseSpacingBreakerConfig() CircuitBreakerConfig {
	c := DefaultCircuitBreakerConfig()
	c.ConsecutiveFailureThreshold = 5
	c.MinimumRequests = 1_000_000
	c.FailureRateWindow = 100 * time.Millisecond
	c.OpenStateDuration = time.Hour
	return c
}

// TestBreaker_SparseSpacingStillTrips is the money test for SOL-152286: five
// failures spaced wider than the rolling window's bucket period must still
// trip the consecutive rule, even though gobreaker's own decayed counter
// would plateau below threshold at this spacing.
func TestBreaker_SparseSpacingStillTrips(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	e := newBreakerTestExchangerPlain(t, srv.URL, sparseSpacingBreakerConfig())

	const spacedFailures = 5
	for i := 0; i < spacedFailures; i++ {
		_, err := e.Exchange(context.Background(), inputWithSubject(fmt.Sprintf("sparse-%d", i)))
		if !errors.Is(err, ErrExchangeTransport) {
			t.Fatalf("failure %d: err = %v, want ErrExchangeTransport", i, err)
		}
		time.Sleep(30 * time.Millisecond)
	}

	if got := e.breaker.State(); got != gobreaker.StateOpen {
		t.Fatalf("breaker State = %v after %d sparsely-spaced failures, want open", got, spacedFailures)
	}

	hitsBeforeNext := hits.Load()
	_, err := e.Exchange(context.Background(), inputWithSubject("sparse-rejected"))
	if !errors.Is(err, ErrExchangeCircuitOpen) {
		t.Errorf("post-trip call err = %v, want ErrExchangeCircuitOpen", err)
	}
	if got := hits.Load(); got != hitsBeforeNext {
		t.Errorf("server hits rose from %d to %d; open breaker must not call the IdP", hitsBeforeNext, got)
	}
}

// TestBreaker_SuccessResetsConsecutiveCounter asserts a single success
// between failures resets the undecayed counter, so 4 failures + 1 success +
// 4 failures must NOT trip a threshold-5 rule — only a 5th consecutive
// failure after the success does.
func TestBreaker_SuccessResetsConsecutiveCounter(t *testing.T) {
	t.Parallel()
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if healthy.Load() {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, successJSON("reset-tok", 3600))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cfg := DefaultCircuitBreakerConfig()
	cfg.ConsecutiveFailureThreshold = 5
	cfg.MinimumRequests = 1_000_000
	e := newBreakerTestExchangerPlain(t, srv.URL, cfg)

	for i := 0; i < 4; i++ {
		_, err := e.Exchange(context.Background(), inputWithSubject(fmt.Sprintf("reset-fail-a-%d", i)))
		if !errors.Is(err, ErrExchangeTransport) {
			t.Fatalf("pre-reset failure %d: err = %v, want ErrExchangeTransport", i, err)
		}
	}

	healthy.Store(true)
	if _, err := e.Exchange(context.Background(), inputWithSubject("reset-success")); err != nil {
		t.Fatalf("reset success call err = %v, want nil", err)
	}
	healthy.Store(false)

	for i := 0; i < 4; i++ {
		_, err := e.Exchange(context.Background(), inputWithSubject(fmt.Sprintf("reset-fail-b-%d", i)))
		if !errors.Is(err, ErrExchangeTransport) {
			t.Fatalf("post-reset failure %d: err = %v, want ErrExchangeTransport", i, err)
		}
	}
	if got := e.breaker.State(); got != gobreaker.StateClosed {
		t.Fatalf("breaker State = %v after 4+success+4 failures, want closed (success must reset the streak)", got)
	}

	_, err := e.Exchange(context.Background(), inputWithSubject("reset-fail-fifth"))
	if !errors.Is(err, ErrExchangeTransport) {
		t.Fatalf("5th post-reset failure: err = %v, want ErrExchangeTransport", err)
	}
	if got := e.breaker.State(); got != gobreaker.StateOpen {
		t.Errorf("breaker State = %v after a fresh run of 5, want open", got)
	}
}

// TestBreaker_ExcludedDoesNotResetConsecutiveCounter asserts an excluded
// outcome (429) neither increments nor resets the counter: 4 failures, a
// 429, then 1 more failure must still trip a threshold-5 rule (4+1=5, the
// 429 is invisible to the counter).
func TestBreaker_ExcludedDoesNotResetConsecutiveCounter(t *testing.T) {
	t.Parallel()
	var rateLimited atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if rateLimited.Load() {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cfg := DefaultCircuitBreakerConfig()
	cfg.ConsecutiveFailureThreshold = 5
	cfg.MinimumRequests = 1_000_000
	e := newBreakerTestExchangerPlain(t, srv.URL, cfg)

	for i := 0; i < 4; i++ {
		_, err := e.Exchange(context.Background(), inputWithSubject(fmt.Sprintf("excl-fail-%d", i)))
		if !errors.Is(err, ErrExchangeTransport) {
			t.Fatalf("failure %d: err = %v, want ErrExchangeTransport", i, err)
		}
	}

	rateLimited.Store(true)
	_, err := e.Exchange(context.Background(), inputWithSubject("excl-429"))
	if !errors.Is(err, ErrExchangeTransport) {
		t.Fatalf("429 call: err = %v, want ErrExchangeTransport", err)
	}
	rateLimited.Store(false)
	if got := e.breaker.State(); got != gobreaker.StateClosed {
		t.Fatalf("breaker State = %v after a 429; must stay closed (excluded, not counted)", got)
	}

	_, err = e.Exchange(context.Background(), inputWithSubject("excl-fail-fifth"))
	if !errors.Is(err, ErrExchangeTransport) {
		t.Fatalf("5th failure: err = %v, want ErrExchangeTransport", err)
	}
	if got := e.breaker.State(); got != gobreaker.StateOpen {
		t.Errorf("breaker State = %v after 4+429+1 failures, want open (429 must not reset the streak)", got)
	}
}

// TestBreaker_RecoveryNeedsFreshFailuresToRetrip pins that closing the
// breaker via recovery restarts the consecutive counter at 0: a single
// ordinary failure right after recovery must NOT immediately re-trip a
// threshold-5 rule.
func TestBreaker_RecoveryNeedsFreshFailuresToRetrip(t *testing.T) {
	t.Parallel()
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if healthy.Load() {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, successJSON("retrip-tok", 3600))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cfg := recoveryBreakerConfig()
	cfg.ConsecutiveFailureThreshold = 5
	e := newBreakerTestExchangerPlain(t, srv.URL, cfg)

	for i := 0; i < 5; i++ {
		_, err := e.Exchange(context.Background(), inputWithSubject(fmt.Sprintf("retrip-trip-%d", i)))
		if !errors.Is(err, ErrExchangeTransport) {
			t.Fatalf("trip failure %d: err = %v, want ErrExchangeTransport", i, err)
		}
	}
	if got := e.breaker.State(); got != gobreaker.StateOpen {
		t.Fatalf("breaker State = %v after 5 failures, want open", got)
	}

	healthy.Store(true)
	waitForBreakerState(t, e.breaker, gobreaker.StateHalfOpen)
	if _, err := e.Exchange(context.Background(), inputWithSubject("retrip-probe-1")); err != nil {
		t.Fatalf("probe 1 err = %v, want nil", err)
	}
	if _, err := e.Exchange(context.Background(), inputWithSubject("retrip-probe-2")); err != nil {
		t.Fatalf("probe 2 err = %v, want nil", err)
	}
	if got := e.breaker.State(); got != gobreaker.StateClosed {
		t.Fatalf("breaker State = %v after 2 consecutive probe successes, want closed", got)
	}

	healthy.Store(false)
	_, err := e.Exchange(context.Background(), inputWithSubject("retrip-single-failure"))
	if !errors.Is(err, ErrExchangeTransport) {
		t.Fatalf("post-recovery failure: err = %v, want ErrExchangeTransport", err)
	}
	if got := e.breaker.State(); got != gobreaker.StateClosed {
		t.Errorf("breaker State = %v after 1 failure post-recovery, want closed (recovery must restart the counter at 0)", got)
	}
}

// TestBreaker_RateRuleTripsOnPartialDegradation covers the rate rule's trip
// end-to-end — the path every other exchange-level test deliberately starves
// — using the shape the consecutive rule is blind to: interleaved successes
// reset the streak while the failure rate holds above 50%. Must stay closed
// at 9 evaluated outcomes (55%, but under the MinimumRequests floor) and
// trip on the 11th call — the first failure checked after the floor is met
// (the 10th outcome is a success; the rate rule is only checked on failures).
func TestBreaker_RateRuleTripsOnPartialDegradation(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Odd-numbered requests fail, even-numbered succeed: F S F S ... —
		// sequential distinct-key calls, so the parity is deterministic.
		if hits.Add(1)%2 == 0 {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, successJSON("degraded-tok", 3600))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cfg := DefaultCircuitBreakerConfig() // MinimumRequests=10, threshold 50%
	cfg.ConsecutiveFailureThreshold = 0  // isolate the rate rule
	// Freeze the window far past the test's runtime so bucket decay cannot
	// age outcomes out mid-test (same rationale as the exact-count test).
	cfg.FailureRateWindow = time.Hour
	cfg.OpenStateDuration = time.Hour
	e := newBreakerTestExchangerPlain(t, srv.URL, cfg)

	// 9 evaluated outcomes, alternating F S F S... (hits 1,3,5,7,9 fail;
	// 2,4,6,8 succeed): 5 failures of 9 evaluated is already 55%, but
	// 9 < MinimumRequests, so the floor must hold the breaker closed.
	for i := 0; i < 9; i++ {
		_, err := e.Exchange(context.Background(), inputWithSubject(fmt.Sprintf("degraded-%d", i)))
		if i%2 == 0 && !errors.Is(err, ErrExchangeTransport) {
			t.Fatalf("call %d: err = %v, want ErrExchangeTransport (odd hit fails)", i, err)
		}
		if i%2 == 1 && err != nil {
			t.Fatalf("call %d: err = %v, want nil (even hit succeeds)", i, err)
		}
	}
	if got := e.breaker.State(); got != gobreaker.StateClosed {
		t.Fatalf("breaker State = %v at 9 evaluated outcomes, want closed (below the MinimumRequests floor)", got)
	}

	// 10th evaluated outcome is a success (hit 10, even) — the rate rule only
	// runs its check on failures, so an 11th call (hit 11, odd → failure)
	// delivers the tripping evaluation: 6 failures of 11 evaluated = 54.5%,
	// floor met, threshold crossed.
	if _, err := e.Exchange(context.Background(), inputWithSubject("degraded-9")); err != nil {
		t.Fatalf("10th call: err = %v, want nil (even hit succeeds)", err)
	}
	_, err := e.Exchange(context.Background(), inputWithSubject("degraded-10"))
	if !errors.Is(err, ErrExchangeTransport) {
		t.Fatalf("11th call: err = %v, want ErrExchangeTransport", err)
	}
	if got := e.breaker.State(); got != gobreaker.StateOpen {
		t.Fatalf("breaker State = %v after 6 failures of 11 evaluated, want open (rate rule must trip despite interleaved successes)", got)
	}

	// Fail-fast proof, same as the consecutive-rule tests: rejected without
	// an IdP round-trip.
	hitsBeforeNext := hits.Load()
	_, err = e.Exchange(context.Background(), inputWithSubject("degraded-rejected"))
	if !errors.Is(err, ErrExchangeCircuitOpen) {
		t.Errorf("post-trip call err = %v, want ErrExchangeCircuitOpen", err)
	}
	if got := hits.Load(); got != hitsBeforeNext {
		t.Errorf("server hits rose from %d to %d; open breaker must not call the IdP", hitsBeforeNext, got)
	}
}

// TestBreaker_ConcurrentConsecutiveCounterRaceFree drives many concurrent
// Exchange calls so the counter is read/written from many goroutines at
// once. -race catches a torn access; serialization is gobreaker's own
// guarantee (afterRequest takes cb.mutex before any callback), not proven
// here. What this test proves instead: no update is silently LOST. The
// threshold (callers+1) is unreachable by the concurrent phase alone, so the
// breaker is guaranteed closed afterward; a deterministic follow-up run
// then must trip at EXACTLY that threshold — a lost increment would trip
// late or never. Concurrent tripping itself is covered elsewhere
// (TestBreaker_ConcurrentDistinctKeysCountExactlyOncePerCall,
// TestBreaker_OpenBreakerRejectsStampedeCleanly).
func TestBreaker_ConcurrentConsecutiveCounterRaceFree(t *testing.T) {
	t.Parallel()
	var counter atomic.Int32
	var phase atomic.Int32 // 0=concurrent (mixed outcomes), 1=baseline (success), 2=follow-up (fail)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch phase.Load() {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, successJSON("race-tok", 3600))
			return
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		// Concurrent phase: deterministic per-request outcome (odd => fail)
		// so the mix is reproducible without a shared mutable toggle racing
		// the goroutines themselves.
		if counter.Add(1)%2 == 0 {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, successJSON("race-tok", 3600))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	const callers = 50
	const threshold = callers + 1 // unreachable by the concurrent phase alone
	cfg := DefaultCircuitBreakerConfig()
	cfg.ConsecutiveFailureThreshold = threshold
	cfg.MinimumRequests = 1_000_000
	e := newBreakerTestExchangerPlain(t, srv.URL, cfg)

	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			_, _ = e.Exchange(context.Background(), inputWithSubject(fmt.Sprintf("race-%d", i)))
		}(i)
	}
	wg.Wait()

	if got := e.breaker.State(); got != gobreaker.StateClosed {
		t.Fatalf("breaker State = %v after the concurrent phase, want closed (threshold is unreachable by 50 calls alone)", got)
	}

	// One guaranteed success resets the counter to a known 0 before the
	// deterministic run (the concurrent phase's last outcome is unspecified).
	phase.Store(1)
	if _, err := e.Exchange(context.Background(), inputWithSubject("race-baseline")); err != nil {
		t.Fatalf("baseline call err = %v, want nil", err)
	}

	phase.Store(2)
	for i := 0; i < threshold; i++ {
		_, err := e.Exchange(context.Background(), inputWithSubject(fmt.Sprintf("race-followup-%d", i)))
		if !errors.Is(err, ErrExchangeTransport) {
			t.Fatalf("follow-up failure %d: err = %v, want ErrExchangeTransport", i, err)
		}
	}
	if got := e.breaker.State(); got != gobreaker.StateOpen {
		t.Fatalf("breaker State = %v after %d follow-up failures from a known baseline, want open (a lost update during contention would trip early or never)", got, threshold)
	}
}

// TestBreaker_StateChangeLogCarriesConsecutiveFailures pins the trip WARN's
// consecutive_failures attribute — the operator's signal for which rule
// opened the breaker. NOT parallel: captureLogs swaps the global logger.
func TestBreaker_StateChangeLogCarriesConsecutiveFailures(t *testing.T) {
	records, restore := captureLogs(t)
	defer restore()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cfg := DefaultCircuitBreakerConfig()
	cfg.ConsecutiveFailureThreshold = 3
	cfg.MinimumRequests = 1_000_000 // isolate the trip to the consecutive rule
	cfg.OpenStateDuration = time.Hour
	e := newBreakerTestExchangerPlain(t, srv.URL, cfg)

	for i := 0; i < 3; i++ {
		_, _ = e.Exchange(context.Background(), inputWithSubject(fmt.Sprintf("log-trip-%d", i)))
	}
	if got := e.breaker.State(); got != gobreaker.StateOpen {
		t.Fatalf("breaker State = %v after 3 failures, want open", got)
	}

	// Filter to the closed→open transition rather than assuming capture order.
	for _, rec := range records() {
		if rec.Message != "token exchange circuit breaker state change" ||
			rec.Attrs["from"] != "closed" || rec.Attrs["to"] != "open" {
			continue
		}
		if got := rec.Attrs["consecutive_failures"]; got != "3" {
			t.Errorf("trip WARN consecutive_failures = %q, want %q (must carry the tripping count)", got, "3")
		}
		return
	}
	t.Error("no closed→open state-change WARN captured")
}
