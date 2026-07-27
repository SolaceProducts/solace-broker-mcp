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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestGateCheck_NoGateSetIsNeverBlocked: the gate is opt-in.
func TestGateCheck_NoGateSetIsNeverBlocked(t *testing.T) {
	t.Parallel()
	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e.gateCheck() {
		t.Fatal("gateCheck() = true on a freshly constructed Exchanger, want false")
	}
}

// TestGateCheck_BlocksBeforeExpiryAllowsAfter: the core gate-timing
// contract, isolated from the breaker/retry machinery.
func TestGateCheck_BlocksBeforeExpiryAllowsAfter(t *testing.T) {
	t.Parallel()
	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.nowFunc = func() time.Time { return now }

	e.raiseGate(10 * time.Second)

	if !e.gateCheck() {
		t.Fatal("gateCheck() = false immediately after raiseGate(10s), want true")
	}

	now = now.Add(5 * time.Second)
	if !e.gateCheck() {
		t.Fatal("gateCheck() = false at +5s of a 10s gate, want true (still gated)")
	}

	now = now.Add(6 * time.Second) // total +11s, past the 10s window
	if e.gateCheck() {
		t.Fatal("gateCheck() = true at +11s of a 10s gate, want false (gate expired)")
	}
}

func TestRaiseGate_ZeroOrNegativeDelayIsNoOp(t *testing.T) {
	t.Parallel()
	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.nowFunc = func() time.Time { return now }

	e.raiseGate(0)
	if e.gateCheck() {
		t.Fatal("gateCheck() = true after raiseGate(0), want false")
	}

	e.raiseGate(-5 * time.Second)
	if e.gateCheck() {
		t.Fatal("gateCheck() = true after raiseGate(negative), want false")
	}
}

// TestRaiseGate_MaxWinsUnderConcurrency fires concurrent raiseGate calls
// with varying delays and asserts the gate lands at the longest one.
func TestRaiseGate_MaxWinsUnderConcurrency(t *testing.T) {
	t.Parallel()
	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.nowFunc = func() time.Time { return now }

	const goroutines = 50
	longest := time.Duration(goroutines) * time.Second

	var wg sync.WaitGroup
	for i := 1; i <= goroutines; i++ {
		wg.Add(1)
		go func(seconds int) {
			defer wg.Done()
			e.raiseGate(time.Duration(seconds) * time.Second)
		}(i)
	}
	wg.Wait()

	got := e.gatedUntil.Load()
	want := now.Add(longest).UnixNano()
	if got != want {
		t.Errorf("gatedUntil = %d, want %d (the single longest delay of %v)", got, want, longest)
	}
}

// TestRaiseGate_ShorterDelayDoesNotClobberLongerAlreadySet is the
// deterministic counterpart to the concurrency test above.
func TestRaiseGate_ShorterDelayDoesNotClobberLongerAlreadySet(t *testing.T) {
	t.Parallel()
	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.nowFunc = func() time.Time { return now }

	e.raiseGate(30 * time.Second)
	longUntil := e.gatedUntil.Load()

	e.raiseGate(5 * time.Second) // shorter — must not lower the gate
	if got := e.gatedUntil.Load(); got != longUntil {
		t.Errorf("gatedUntil = %d after a shorter raiseGate, want unchanged %d", got, longUntil)
	}

	e.raiseGate(60 * time.Second) // longer — must raise it further
	if got := e.gatedUntil.Load(); got == longUntil {
		t.Error("gatedUntil unchanged after a longer raiseGate, want it raised")
	}
}

// Both use an Exchanger with maxHonoredRetryAfter unset, exercising the
// fallback to defaultMaxHonoredRetryAfter — see
// TestClampRetryAfter_OperatorOverride for the configured-cap path.
func TestClampRetryAfter_UnderCapPassesThroughUnclamped(t *testing.T) {
	t.Parallel()
	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, wasClamped := e.clampRetryAfter(10 * time.Second)
	if wasClamped {
		t.Error("wasClamped = true for a value under the cap, want false")
	}
	if got != 10*time.Second {
		t.Errorf("clamped = %v, want unchanged 10s", got)
	}
}

func TestClampRetryAfter_OverCapClamps(t *testing.T) {
	t.Parallel()
	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	over := defaultMaxHonoredRetryAfter + time.Hour
	got, wasClamped := e.clampRetryAfter(over)
	if !wasClamped {
		t.Error("wasClamped = false for a value over the cap, want true")
	}
	if got != defaultMaxHonoredRetryAfter {
		t.Errorf("clamped = %v, want the default cap %v", got, defaultMaxHonoredRetryAfter)
	}
}

// TestClampRetryAfter_OperatorOverride: an operator cap replaces the
// shipped default in both directions (looser and tighter).
func TestClampRetryAfter_OperatorOverride(t *testing.T) {
	t.Parallel()
	p := validParams(t)
	p.MaxHonoredRetryAfter = 5 * time.Minute // larger than defaultMaxHonoredRetryAfter (60s)
	e, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, wasClamped := e.clampRetryAfter(2 * time.Minute)
	if wasClamped {
		t.Error("wasClamped = true for a value under the operator's larger cap, want false")
	}
	if got != 2*time.Minute {
		t.Errorf("clamped = %v, want unchanged 2m", got)
	}

	p2 := validParams(t)
	p2.MaxHonoredRetryAfter = 5 * time.Second
	e2, err := New(p2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got2, wasClamped2 := e2.clampRetryAfter(30 * time.Second)
	if !wasClamped2 {
		t.Error("wasClamped = false for a value over the operator's smaller cap, want true")
	}
	if got2 != 5*time.Second {
		t.Errorf("clamped = %v, want the operator's cap 5s", got2)
	}
}

func TestRaiseGateOnExhaustedRateLimit_OnlyExhaustedRateLimitedRaisesGate(t *testing.T) {
	t.Parallel()

	newExchanger := func(t *testing.T) *Exchanger {
		t.Helper()
		e, err := New(validParams(t))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		e.nowFunc = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
		return e
	}

	t.Run("exhausted rate-limited with usable Retry-After raises the gate", func(t *testing.T) {
		t.Parallel()
		e := newExchanger(t)
		err := &ExchangeError{
			Sentinel:         ErrExchangeRetriesExhausted,
			FailureClass:     FailureClassRateLimited,
			RetryAfterResult: &retryAfterResult{delay: 5 * time.Second, ok: true},
		}
		e.raiseGateOnExhaustedRateLimit(err, "test-broker")
		if !e.gateCheck() {
			t.Error("gateCheck() = false, want true — exhausted+rate-limited+usable Retry-After must raise the gate")
		}
	})

	t.Run("non-exhausted error does not raise the gate", func(t *testing.T) {
		t.Parallel()
		e := newExchanger(t)
		err := &ExchangeError{
			Sentinel:         ErrExchangeTransport,
			FailureClass:     FailureClassRateLimited,
			RetryAfterResult: &retryAfterResult{delay: 5 * time.Second, ok: true},
		}
		e.raiseGateOnExhaustedRateLimit(err, "test-broker")
		if e.gateCheck() {
			t.Error("gateCheck() = true, want false — a non-exhausted (mid-chain) 429 must not raise the gate")
		}
	})

	t.Run("exhausted but different FailureClass does not raise the gate", func(t *testing.T) {
		t.Parallel()
		e := newExchanger(t)
		err := &ExchangeError{
			Sentinel:     ErrExchangeRetriesExhausted,
			FailureClass: FailureClassUpstream5xx,
		}
		e.raiseGateOnExhaustedRateLimit(err, "test-broker")
		if e.gateCheck() {
			t.Error("gateCheck() = true, want false — an exhausted 5xx must not raise the rate-limit gate")
		}
	})

	t.Run("exhausted rate-limited with unusable Retry-After does not raise the gate", func(t *testing.T) {
		t.Parallel()
		e := newExchanger(t)
		err := &ExchangeError{
			Sentinel:         ErrExchangeRetriesExhausted,
			FailureClass:     FailureClassRateLimited,
			RetryAfterResult: &retryAfterResult{ok: false},
		}
		e.raiseGateOnExhaustedRateLimit(err, "test-broker")
		if e.gateCheck() {
			t.Error("gateCheck() = true, want false — absent/unparseable Retry-After must preserve current behavior (no gate)")
		}
	})

	t.Run("exhausted rate-limited with nil RetryAfterResult does not raise the gate", func(t *testing.T) {
		t.Parallel()
		e := newExchanger(t)
		err := &ExchangeError{
			Sentinel:     ErrExchangeRetriesExhausted,
			FailureClass: FailureClassRateLimited,
		}
		e.raiseGateOnExhaustedRateLimit(err, "test-broker")
		if e.gateCheck() {
			t.Error("gateCheck() = true, want false — a nil RetryAfterResult must not raise the gate")
		}
	})

	t.Run("a plain (non-ExchangeError) error does not raise the gate", func(t *testing.T) {
		t.Parallel()
		e := newExchanger(t)
		// errors.As must fail cleanly here, not panic.
		e.raiseGateOnExhaustedRateLimit(ErrExchangeRetriesExhausted, "test-broker")
		if e.gateCheck() {
			t.Error("gateCheck() = true, want false — a sentinel with no ExchangeError wrapper must not raise the gate")
		}
	})
}

// TestRunProtectedExchange_GateBlocksBeforeBreakerConsultedRegardlessOfState
// proves the gate works with the breaker disabled — it doesn't require one.
func TestRunProtectedExchange_GateBlocksBeforeBreakerConsultedRegardlessOfState(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	t.Run("breaker disabled (nil)", func(t *testing.T) {
		t.Parallel()
		p := validParams(t)
		p.TokenURL = srv.URL
		p.HTTPClient = &http.Client{}
		e, err := New(p)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		e.nowFunc = func() time.Time { return now }
		e.raiseGate(time.Minute)

		_, err = e.Exchange(context.Background(), inputWithSubject("nil-breaker-gated"))
		if !errors.Is(err, ErrExchangeRateLimited) {
			t.Errorf("err = %v, want ErrExchangeRateLimited (gate must apply even with breaker disabled)", err)
		}
		if got := hits.Load(); got != 0 {
			t.Errorf("server hits = %d, want 0 — a gated call must never reach the IdP", got)
		}
	})
}

// TestRunProtectedExchange_GatedCallNeverReachesBreakerBookkeeping keeps the
// gate a pure orthogonal addition — breaker counters must stay untouched.
func TestRunProtectedExchange_GatedCallNeverReachesBreakerBookkeeping(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cfg := DefaultCircuitBreakerConfig()
	p := validParams(t)
	p.TokenURL = srv.URL
	p.HTTPClient = &http.Client{}
	p.CircuitBreaker = &cfg
	e, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.nowFunc = func() time.Time { return now }
	e.raiseGate(time.Minute)

	countsBefore := e.breaker.Counts()

	_, err = e.Exchange(context.Background(), inputWithSubject("gated-no-breaker-bookkeeping"))
	if !errors.Is(err, ErrExchangeRateLimited) {
		t.Fatalf("err = %v, want ErrExchangeRateLimited", err)
	}

	countsAfter := e.breaker.Counts()
	if countsAfter != countsBefore {
		t.Errorf("breaker Counts changed from %+v to %+v — a gated call must not touch breaker bookkeeping at all", countsBefore, countsAfter)
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("server hits = %d, want 0", got)
	}
}

// TestExchange_RateLimitedSentinelDistinctFromCircuitOpen guards against
// alerting keyed on one sentinel silently starting to fire for the other.
func TestExchange_RateLimitedSentinelDistinctFromCircuitOpen(t *testing.T) {
	t.Parallel()

	rateLimited := &ExchangeError{Sentinel: ErrExchangeRateLimited, Message: "rate limited"}
	circuitOpen := &ExchangeError{Sentinel: ErrExchangeCircuitOpen, Message: "circuit open"}

	if errors.Is(rateLimited, ErrExchangeCircuitOpen) {
		t.Error("errors.Is(rateLimited, ErrExchangeCircuitOpen) = true, want false")
	}
	if errors.Is(circuitOpen, ErrExchangeRateLimited) {
		t.Error("errors.Is(circuitOpen, ErrExchangeRateLimited) = true, want false")
	}

	rlAttrs := attrMap(rateLimited.LogAttrs())
	if _, ok := rlAttrs["gate"]; !ok {
		t.Error(`LogAttrs() for ErrExchangeRateLimited missing "gate" marker`)
	}
	if _, ok := rlAttrs["breaker_state"]; ok {
		t.Error(`LogAttrs() for ErrExchangeRateLimited must NOT carry "breaker_state" — that would conflate it with a circuit-open rejection`)
	}

	coAttrs := attrMap(circuitOpen.LogAttrs())
	if _, ok := coAttrs["breaker_state"]; !ok {
		t.Error(`LogAttrs() for ErrExchangeCircuitOpen missing "breaker_state" marker`)
	}
	if _, ok := coAttrs["gate"]; ok {
		t.Error(`LogAttrs() for ErrExchangeCircuitOpen must NOT carry "gate"`)
	}

	const broker = "some-broker"
	if got := rateLimited.AgentMessage(broker); got != circuitOpen.AgentMessage(broker) {
		t.Errorf("AgentMessage differs between ErrExchangeRateLimited (%q) and ErrExchangeCircuitOpen (%q), want identical shared-IdP phrasing", got, circuitOpen.AgentMessage(broker))
	}
}

func attrMap(attrs []slog.Attr) map[string]slog.Value {
	m := make(map[string]slog.Value, len(attrs))
	for _, a := range attrs {
		m[a.Key] = a.Value
	}
	return m
}

// findLog returns the first captured record whose message equals want, or nil.
func findLog(records []logRecord, want string) *logRecord {
	for i := range records {
		if records[i].Message == want {
			return &records[i]
		}
	}
	return nil
}

// Not run with t.Parallel(): captureLogs mutates the process-global slog
// default, same convention as exchange_test.go's log-capturing tests.
func TestRaiseGateOnExhaustedRateLimit_LogsGateSet(t *testing.T) {
	records, restore := captureLogs(t)
	defer restore()

	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.nowFunc = func() time.Time { return now }

	exchErr := &ExchangeError{
		Sentinel:         ErrExchangeRetriesExhausted,
		FailureClass:     FailureClassRateLimited,
		RetryAfterResult: &retryAfterResult{delay: 10 * time.Second, ok: true},
	}
	e.raiseGateOnExhaustedRateLimit(exchErr, "broker-a")

	if !e.gateCheck() {
		t.Fatal("gateCheck() = false, want true — the gate must be raised")
	}

	rec := findLog(records(), "token exchange rate limited: honoring IdP Retry-After for all callers")
	if rec == nil {
		t.Fatal("expected a \"honoring IdP Retry-After\" log line, found none")
	}
	if rec.Level != slog.LevelWarn {
		t.Errorf("level = %v, want Warn", rec.Level)
	}
	if got := rec.Attrs["broker"]; got != "broker-a" {
		t.Errorf(`attrs["broker"] = %q, want "broker-a"`, got)
	}
	if got := rec.Attrs["retry_after"]; got != (10 * time.Second).String() {
		t.Errorf(`attrs["retry_after"] = %q, want %q`, got, (10 * time.Second).String())
	}
	if _, ok := rec.Attrs["gated_until"]; !ok {
		t.Error(`attrs["gated_until"] missing`)
	}
	if findLog(records(), "token exchange Retry-After exceeded configured cap, clamping") != nil {
		t.Error("unexpectedly also logged the clamped-cap line")
	}
	if findLog(records(), "token exchange rate limited: IdP sent no Retry-After, cannot pace subsequent callers") != nil {
		t.Error("unexpectedly also logged the gate-not-set line")
	}
}

func TestRaiseGateOnExhaustedRateLimit_LogsGateClamped(t *testing.T) {
	records, restore := captureLogs(t)
	defer restore()

	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.nowFunc = func() time.Time { return now }

	requested := defaultMaxHonoredRetryAfter + time.Hour
	exchErr := &ExchangeError{
		Sentinel:         ErrExchangeRetriesExhausted,
		FailureClass:     FailureClassRateLimited,
		RetryAfterResult: &retryAfterResult{delay: requested, ok: true},
	}
	e.raiseGateOnExhaustedRateLimit(exchErr, "broker-b")

	if !e.gateCheck() {
		t.Fatal("gateCheck() = false, want true — the gate must still be raised, at the clamped value")
	}
	wantUntil := now.Add(defaultMaxHonoredRetryAfter).UnixNano()
	if got := e.gatedUntil.Load(); got != wantUntil {
		t.Errorf("gatedUntil = %d, want %d (clamped to defaultMaxHonoredRetryAfter)", got, wantUntil)
	}

	rec := findLog(records(), "token exchange Retry-After exceeded configured cap, clamping")
	if rec == nil {
		t.Fatal("expected a \"exceeded configured cap, clamping\" log line, found none")
	}
	if rec.Level != slog.LevelWarn {
		t.Errorf("level = %v, want Warn", rec.Level)
	}
	if got := rec.Attrs["broker"]; got != "broker-b" {
		t.Errorf(`attrs["broker"] = %q, want "broker-b"`, got)
	}
	if got := rec.Attrs["requested"]; got != requested.String() {
		t.Errorf(`attrs["requested"] = %q, want %q`, got, requested.String())
	}
	if got := rec.Attrs["clamped_to"]; got != defaultMaxHonoredRetryAfter.String() {
		t.Errorf(`attrs["clamped_to"] = %q, want %q`, got, defaultMaxHonoredRetryAfter.String())
	}
	if findLog(records(), "token exchange rate limited: honoring IdP Retry-After for all callers") != nil {
		t.Error("unexpectedly also logged the plain gate-set line")
	}
}

func TestRaiseGateOnExhaustedRateLimit_LogsGateNotSet_Absent(t *testing.T) {
	records, restore := captureLogs(t)
	defer restore()

	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e.nowFunc = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	exchErr := &ExchangeError{
		Sentinel:         ErrExchangeRetriesExhausted,
		FailureClass:     FailureClassRateLimited,
		RetryAfterResult: &retryAfterResult{ok: false, raw: ""},
	}
	e.raiseGateOnExhaustedRateLimit(exchErr, "broker-c")

	if e.gateCheck() {
		t.Fatal("gateCheck() = true, want false — an absent Retry-After must not raise the gate")
	}

	rec := findLog(records(), "token exchange rate limited: IdP sent no Retry-After, cannot pace subsequent callers")
	if rec == nil {
		t.Fatal("expected the absent-header log line, found none")
	}
	if got := rec.Attrs["broker"]; got != "broker-c" {
		t.Errorf(`attrs["broker"] = %q, want "broker-c"`, got)
	}
	if _, ok := rec.Attrs["retry_after_raw"]; ok {
		t.Error(`attrs["retry_after_raw"] present for a truly absent header, want absent (nothing to show)`)
	}
}

func TestRaiseGateOnExhaustedRateLimit_LogsGateNotSet_Unparseable(t *testing.T) {
	records, restore := captureLogs(t)
	defer restore()

	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e.nowFunc = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	exchErr := &ExchangeError{
		Sentinel:         ErrExchangeRetriesExhausted,
		FailureClass:     FailureClassRateLimited,
		RetryAfterResult: &retryAfterResult{ok: false, raw: "not-a-valid-value"},
	}
	e.raiseGateOnExhaustedRateLimit(exchErr, "broker-d")

	if e.gateCheck() {
		t.Fatal("gateCheck() = true, want false — an unparseable Retry-After must not raise the gate")
	}

	rec := findLog(records(), "token exchange rate limited: IdP sent an unparseable Retry-After, cannot pace subsequent callers")
	if rec == nil {
		t.Fatal("expected the unparseable-header log line, found none")
	}
	if got := rec.Attrs["broker"]; got != "broker-d" {
		t.Errorf(`attrs["broker"] = %q, want "broker-d"`, got)
	}
	if got := rec.Attrs["retry_after_raw"]; got != "not-a-valid-value" {
		t.Errorf(`attrs["retry_after_raw"] = %q, want %q`, got, "not-a-valid-value")
	}
}

func TestLogGateNotSet_RawValueCappedInLogs(t *testing.T) {
	records, restore := captureLogs(t)
	defer restore()

	oversized := strings.Repeat("x", maxRetryAfterRawLogLen*2)
	logGateNotSet("broker-e", oversized)

	rec := findLog(records(), "token exchange rate limited: IdP sent an unparseable Retry-After, cannot pace subsequent callers")
	if rec == nil {
		t.Fatal("expected the unparseable-header log line, found none")
	}
	got := rec.Attrs["retry_after_raw"]
	if len(got) > maxRetryAfterRawLogLen+len("...(truncated)") {
		t.Errorf("logged raw value length = %d, want capped at %d + truncation marker", len(got), maxRetryAfterRawLogLen)
	}
	if got == oversized {
		t.Error("logged raw value equals the full oversized input, want it truncated")
	}
}
