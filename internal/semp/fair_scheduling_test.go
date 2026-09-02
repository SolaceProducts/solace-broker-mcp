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

package semp_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/resilience"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

// These tests run against a real BrokerClient over real HTTP, which is the
// whole point of putting them here rather than in the scheduler's own package:
// the scheduler's unit tests prove the rotation, and these prove the wiring —
// that semp.fair_scheduling actually reaches the Senders, that both protocol
// clients share one scheduler, and that the caller key survives the trip from
// a context to an admission decision.
//
// Real time, so the assertions are floors and ceilings rather than exact
// counts. The defect each guards is large: without fair scheduling the steady
// caller's share collapses toward 1/(1+burstConcurrency), not to something
// marginally under half.

const (
	fairInterval    = 40 * time.Millisecond
	fairWindow      = 2 * time.Second
	burstConcurrent = 20
)

// fairSchedulingSEMPConfig builds a SEMP config with fair scheduling explicitly
// on or off, and with admission otherwise unbounded so nothing sheds mid-test.
func fairSchedulingSEMPConfig(enabled bool) *config.SEMPConfig {
	retries := 0
	minInterval := fairInterval
	noQueueBound := time.Duration(0)
	fair := enabled
	return &config.SEMPConfig{
		RequestTimeoutDuration: 5 * time.Second,
		Retries:                &retries,
		RequestMinInterval:     &minInterval,
		MaxQueueWait:           &noQueueBound,
		FairScheduling:         &fair,
		RetryMinInterval:       1 * time.Millisecond,
		RetryMaxInterval:       5 * time.Millisecond,
		// Generous: the in-flight cap must provably not be the constraint, so
		// any share observed comes from the pace rotation.
		MaxConcurrentPerBroker: 32,
	}
}

// newFairTestBroker returns a BrokerClient pointed at a trivial always-OK
// server, plus a counter of requests the broker actually saw.
func newFairTestBroker(t *testing.T, enabled bool) (*semp.BrokerClient, *atomic.Int64) {
	t.Helper()

	var served atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		if r.URL.Path == "/SEMP" {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<rpc-reply><rpc><show/></rpc><execute-result code="ok"/></rpc-reply>`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{},"meta":{}}`))
	}))
	t.Cleanup(server.Close)

	brokerCfg := &config.BrokerConfig{
		URL:  server.URL,
		Auth: config.AuthConfig{Mode: "basic", Username: "admin", Password: "secret"},
	}
	bc, err := semp.NewBrokerClient("fairness-test", brokerCfg, fairSchedulingSEMPConfig(enabled), nil)
	if err != nil {
		t.Fatalf("NewBrokerClient: %v", err)
	}
	t.Cleanup(bc.Close)
	return bc, &served
}

// driveCallers runs one burst caller at burstConcurrent concurrency and one
// steady caller at a concurrency of one, for fairWindow, and reports how many
// requests each completed.
func driveCallers(t *testing.T, bc *semp.BrokerClient) (burst, steady int64) {
	t.Helper()

	op := &sempv2.Operation{
		ID:     "getAbout",
		Method: http.MethodGet,
		Path:   "/SEMP/v2/monitor/about",
	}

	burstKey := resilience.CallerKey{Subject: "burst-agent", Session: "session-burst"}
	steadyKey := resilience.CallerKey{Subject: "operator", Session: "session-steady"}

	var burstCount, steadyCount atomic.Int64
	ctx, cancel := context.WithTimeout(context.Background(), fairWindow)
	defer cancel()

	var wg sync.WaitGroup
	worker := func(key resilience.CallerKey, counter *atomic.Int64) {
		defer wg.Done()
		callerCtx := resilience.WithCallerKey(ctx, key)
		for ctx.Err() == nil {
			resp, err := bc.SEMPv2().Execute(callerCtx, op, nil)
			if err != nil {
				// A cancellation at the end of the window is expected; any
				// other error is the test's problem, not the subject's.
				if ctx.Err() == nil {
					t.Errorf("Execute for %q: %v", key.Subject, err)
				}
				return
			}
			_ = resp
			counter.Add(1)
		}
	}

	for range burstConcurrent {
		wg.Add(1)
		go worker(burstKey, &burstCount)
	}
	wg.Add(1)
	go worker(steadyKey, &steadyCount)
	wg.Wait()

	return burstCount.Load(), steadyCount.Load()
}

// The ticket's acceptance criterion, end to end: caller A issuing a sustained
// burst must not push caller B's throughput below the agreed floor.
//
// Without fair scheduling both admission gates are FIFO, so B's share tends to
// 1/(1+burstConcurrent) — about 5% at the concurrency used here. Round-robin
// over per-caller subqueues makes it roughly 50%.
//
// The floor asserted is 30%, well below the 50% the design targets and well
// above the ~5% the defect produces. The gap absorbs real-clock noise without
// weakening the assertion into meaninglessness.
func TestBrokerClient_FairSchedulingHoldsASteadyCallersFloor(t *testing.T) {
	bc, served := newFairTestBroker(t, true)

	burst, steady := driveCallers(t, bc)
	total := burst + steady
	if total == 0 {
		t.Fatal("no requests completed")
	}

	share := float64(steady) / float64(total)
	if share < 0.30 {
		t.Errorf("the steady caller completed %d of %d requests (%.1f%%), want at least 30%%: "+
			"one caller's sustained burst is starving another caller of the same broker "+
			"(burst=%d, steady=%d)", steady, total, share*100, burst, steady)
	}

	// Sanity: the broker really did see this traffic, so the split above is
	// about admitted requests rather than an artifact of everything failing.
	if got := served.Load(); got < total {
		t.Errorf("broker served %d requests but callers counted %d completions", got, total)
	}
}

// Fairness reslices the broker's budget; it must not raise it. The aggregate
// pace has to stay at semp.request_min_interval across every caller combined,
// which is the ticket's other acceptance criterion and the thing a scheduler
// with its own queue could most easily get wrong.
func TestBrokerClient_FairSchedulingPreservesTheAggregateRate(t *testing.T) {
	bc, served := newFairTestBroker(t, true)

	start := time.Now()
	burst, steady := driveCallers(t, bc)
	elapsed := time.Since(start)

	total := burst + steady
	if total == 0 {
		t.Fatal("no requests completed")
	}

	// One admission per interval, plus the single tick the limiter buffers, plus
	// slack for requests already in flight when the window closed.
	const slack = 4
	ceiling := int64(elapsed/fairInterval) + 1 + slack
	if got := served.Load(); got > ceiling {
		t.Errorf("the broker saw %d requests in %v, above the configured ceiling of %d "+
			"(one per %v): fair scheduling is admitting more than semp.request_min_interval "+
			"allows in aggregate", got, elapsed, ceiling, fairInterval)
	}
}

// The kill switch must actually change behavior, and this is also where the
// "without fairness" figure quoted in CHANGELOG.md and the threat model comes
// from. Claiming a measured improvement without measuring the baseline would
// be an unbacked number in a document that gets quoted back.
//
// The bound is deliberately loose. FIFO admission gives the steady caller
// roughly 1/(1+burstConcurrent), about 5% at the concurrency used here, so
// asserting "under 25%" is far from the real value in the safe direction while
// still failing if the switch silently does nothing — the scheduled path
// delivers ~50%.
func TestBrokerClient_FairSchedulingDisabled_SteadyCallerIsCrowdedOut(t *testing.T) {
	bc, _ := newFairTestBroker(t, false)

	burst, steady := driveCallers(t, bc)
	total := burst + steady
	if total == 0 {
		t.Fatal("no requests completed")
	}

	share := float64(steady) / float64(total)
	t.Logf("fair_scheduling off: steady caller got %d of %d requests (%.1f%%); "+
		"burst caller %d across %d concurrent workers", steady, total, share*100, burst, burstConcurrent)

	if share > 0.25 {
		t.Errorf("with fair scheduling off the steady caller still got %.1f%% of the broker, "+
			"expected it to be crowded out well below 25%%: the kill switch is not actually "+
			"disabling the scheduler, so the fairness comparison this pins is meaningless",
			share*100)
	}
}

// semp.fair_scheduling: false must fall through to the pre-SOL-153441 gates and
// leave everything else working. This is the kill switch's whole purpose: it
// exists for blast radius, so the disabled path has to be provably intact
// rather than merely present.
func TestBrokerClient_FairSchedulingDisabledStillPacesAndServes(t *testing.T) {
	bc, served := newFairTestBroker(t, false)

	start := time.Now()
	burst, steady := driveCallers(t, bc)
	elapsed := time.Since(start)

	total := burst + steady
	if total == 0 {
		t.Fatal("no requests completed with fair scheduling disabled: the fallback path is broken")
	}

	const slack = 4
	ceiling := int64(elapsed/fairInterval) + 1 + slack
	if got := served.Load(); got > ceiling {
		t.Errorf("with fair scheduling off the broker saw %d requests in %v, above the "+
			"ceiling of %d", got, elapsed, ceiling)
	}
}

// A config that never went through applyDefaults must leave fair scheduling
// off, the same reading MaxQueueWait takes. Every direct-construction test in
// this repo builds a SEMPConfig by hand, so the alternative would silently
// change the admission path under all of them.
func TestBrokerClient_NilFairSchedulingLeavesTheFeatureOff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{},"meta":{}}`))
	}))
	defer server.Close()

	retries := 0
	minInterval := time.Millisecond
	sempCfg := &config.SEMPConfig{
		RequestTimeoutDuration: 5 * time.Second,
		Retries:                &retries,
		RequestMinInterval:     &minInterval,
		MaxConcurrentPerBroker: 4,
		RetryMinInterval:       time.Millisecond,
		RetryMaxInterval:       5 * time.Millisecond,
		// FairScheduling deliberately left nil.
	}
	brokerCfg := &config.BrokerConfig{
		URL:  server.URL,
		Auth: config.AuthConfig{Mode: "basic", Username: "admin", Password: "secret"},
	}

	bc, err := semp.NewBrokerClient("nil-fair", brokerCfg, sempCfg, nil)
	if err != nil {
		t.Fatalf("NewBrokerClient: %v", err)
	}
	defer bc.Close()

	op := &sempv2.Operation{ID: "getAbout", Method: http.MethodGet, Path: "/SEMP/v2/monitor/about"}
	if _, err := bc.SEMPv2().Execute(context.Background(), op, nil); err != nil {
		t.Fatalf("Execute with nil FairScheduling: %v", err)
	}
}

// Closing a broker client must stop its scheduler. The dispatcher is a
// goroutine per broker, and -race does not detect goroutine leaks, so nothing
// else in the suite would notice.
//
// Asserted through behavior rather than a goroutine count: after Close, a
// request must fail fast with the scheduler's shutdown error instead of
// blocking on a queue nobody is draining.
func TestBrokerClient_CloseStopsTheScheduler(t *testing.T) {
	bc, _ := newFairTestBroker(t, true)
	bc.Close()

	op := &sempv2.Operation{ID: "getAbout", Method: http.MethodGet, Path: "/SEMP/v2/monitor/about"}
	// Deliberately short. A generous deadline would let this pass on the ctx
	// timeout instead of on the scheduler's refusal, which is the same bug the
	// error-identity assertion below guards.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := bc.SEMPv2().Execute(resilience.WithCallerKey(ctx, resilience.CallerKey{}), op, nil)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a request after Close succeeded; the scheduler is still admitting")
		}
		// A drained request must come back as the "broker busy" shape —
		// never sent, safe to repeat, retryable — not as a context timeout and
		// not as an opaque internal error. Asserting merely "some error" would
		// pass even if the request had simply blocked until its own deadline,
		// which is the failure mode this test exists to catch.
		var busy *resilience.BrokerBusyError
		if !errors.As(err, &busy) {
			t.Errorf("request after Close returned %v (%T), want a *resilience.BrokerBusyError: "+
				"a request drained by shutdown must be reported as never-sent and retryable, "+
				"because a rolling restart is routine on the shipped two-replica deployment",
				err, err)
		}
	case <-ctx.Done():
		t.Fatal("a request after Close blocked until the test deadline: Close did not release " +
			"the admission queue, so a caller with no deadline would hang for the life of " +
			"the process")
	}
}
