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
