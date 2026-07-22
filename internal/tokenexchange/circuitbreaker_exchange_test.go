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

	"github.com/SolaceDev/solace-broker-mcp/internal/idpclient"
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
	cfg.MinimumRequests = 1 << 30
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
	cfg.MinimumRequests = 1 << 30 // isolate the trip to the consecutive rule
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
