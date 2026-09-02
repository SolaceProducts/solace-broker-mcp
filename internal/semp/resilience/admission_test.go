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

package resilience

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
)

// admissionSlack is how much longer than the configured bound a shed may take
// before the test calls it a failure. Generous enough for a loaded CI runner,
// far tighter than the defect being guarded: without the bound these requests
// do not fail late, they never return at all.
const admissionSlack = 2 * time.Second

// blockedInterval is a rate-limiter interval long enough that no tick can
// arrive during any test here, which is what "the limiter is saturated" means
// from a caller's point of view.
//
// Preferred over a goroutine racing the sender to drain each tick: that
// approach makes the test's own scheduling decide whether a tick leaks through,
// so it would flake on exactly the machine (a loaded runner) where flakes cost
// the most. An interval no tick can beat produces the same wait with no race.
const blockedInterval = time.Hour

// newAdmissionSender builds a Sender wired for admission tests. A nil
// maxQueueWait models a config that never went through applyDefaults, which
// must leave the bound off.
func newAdmissionSender(t *testing.T, maxQueueWait *time.Duration, limiter *RateLimiter, sem Semaphore, brokerURL string) *Sender {
	t.Helper()
	retries := 0
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		Retries:                &retries,
		RequestMinInterval:     &minInterval,
		MaxQueueWait:           maxQueueWait,
		RequestTimeoutDuration: 30 * time.Second,
		RetryMinInterval:       1 * time.Millisecond,
		RetryMaxInterval:       10 * time.Millisecond,
	}
	return New(&http.Client{}, sempCfg, bearerAuth(t), brokerURL, sem, limiter)
}

// fullSemaphore returns a Semaphore of capacity 1 with its only slot already
// taken, modelling a broker whose in-flight cap is fully occupied.
func fullSemaphore(t *testing.T) Semaphore {
	t.Helper()
	sem := NewSemaphore(1)
	sem <- struct{}{}
	return sem
}

func durPtr(d time.Duration) *time.Duration { return &d }

// A caller with no deadline must get a bounded, typed failure at the
// rate-limiter gate. This is the core of SOL-153442: before it, this exact call
// blocked forever, because nothing upstream of Sender.Do sets a deadline
// (cmd/server/main.go leaves http.Server.WriteTimeout at zero).
//
// Table-driven over two bounds so the failure is shown to track the configured
// value rather than some fixed internal constant.
func TestSenderDo_RateLimitGate_ShedsWithinBound(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		bound time.Duration
	}{
		{name: "50ms bound", bound: 50 * time.Millisecond},
		{name: "250ms bound", bound: 250 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			limiter := NewRateLimiter(blockedInterval)
			defer limiter.Stop()
			d := newAdmissionSender(t, durPtr(tc.bound), limiter, NewSemaphore(10), "http://test-broker")

			req := newGetRequest(t, "http://test-broker/x")
			start := time.Now()
			//nolint:bodyclose // the request is shed before it is sent; resp is always nil here
			resp, err := d.Do(context.Background(), req)
			elapsed := time.Since(start)

			if resp != nil {
				t.Fatalf("got a response; a shed request must never reach the broker")
			}

			var busy *BrokerBusyError
			if !errors.As(err, &busy) {
				t.Fatalf("got err %v (%T), want *BrokerBusyError", err, err)
			}
			if busy.Stage != AdmissionStageRateLimit {
				t.Errorf("Stage = %q, want %q", busy.Stage, AdmissionStageRateLimit)
			}
			if busy.MaxWait != tc.bound {
				t.Errorf("MaxWait = %s, want the configured %s", busy.MaxWait, tc.bound)
			}
			if busy.Waited < tc.bound {
				t.Errorf("Waited = %s, want at least the configured bound %s", busy.Waited, tc.bound)
			}
			if elapsed < tc.bound {
				t.Errorf("shed after %s, want no sooner than the configured bound %s", elapsed, tc.bound)
			}
			if elapsed > tc.bound+admissionSlack {
				t.Errorf("shed after %s, want within %s of the configured bound %s", elapsed, admissionSlack, tc.bound)
			}
		})
	}
}

// The semaphore gate must be bounded too, and this is the case that actually
// bites in production. The rate limiter drains at a fixed pace; a semaphore
// slot is held for the whole retry chain, up to roughly 16 minutes at default
// settings. Bounding only the limiter would move the indefinite hang from one
// select to the next rather than removing it.
func TestSenderDo_ConcurrencyGate_ShedsWithinBound(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		bound time.Duration
	}{
		{name: "50ms bound", bound: 50 * time.Millisecond},
		{name: "250ms bound", bound: 250 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Throttling off, so the limiter admits instantly and the only gate
			// that can shed is the in-flight cap.
			limiter := NewRateLimiter(0)
			defer limiter.Stop()
			d := newAdmissionSender(t, durPtr(tc.bound), limiter, fullSemaphore(t), "http://test-broker")

			req := newGetRequest(t, "http://test-broker/x")
			start := time.Now()
			//nolint:bodyclose // the request is shed before it is sent; resp is always nil here
			resp, err := d.Do(context.Background(), req)
			elapsed := time.Since(start)

			if resp != nil {
				t.Fatalf("got a response; a shed request must never reach the broker")
			}

			var busy *BrokerBusyError
			if !errors.As(err, &busy) {
				t.Fatalf("got err %v (%T), want *BrokerBusyError", err, err)
			}
			if busy.Stage != AdmissionStageConcurrency {
				t.Errorf("Stage = %q, want %q", busy.Stage, AdmissionStageConcurrency)
			}
			if busy.MaxWait != tc.bound {
				t.Errorf("MaxWait = %s, want the configured %s", busy.MaxWait, tc.bound)
			}
			if elapsed < tc.bound {
				t.Errorf("shed after %s, want no sooner than the configured bound %s", elapsed, tc.bound)
			}
			if elapsed > tc.bound+admissionSlack {
				t.Errorf("shed after %s, want within %s of the configured bound %s", elapsed, admissionSlack, tc.bound)
			}
		})
	}
}

// The bound is one budget across both gates, not one per gate. A request that
// spends most of the budget waiting for a tick must shed at the concurrency
// gate on the remainder, so the caller-visible ceiling stays the configured
// value rather than double it.
//
// The interval is set to most of the bound: the tick arrives with budget left
// (so the limiter gate does not shed), and the full semaphore then consumes
// what remains.
func TestSenderDo_AdmissionBudgetIsSharedAcrossBothGates(t *testing.T) {
	t.Parallel()

	// These three values are load-bearing and were chosen against a specific
	// defect: a budget restarted at the second gate. That variant sheds at
	// interval+bound = 3s, so the assertion threshold has to sit below 3s while
	// staying clear of the 2s a correct implementation takes.
	//
	//	shared budget (correct):  sheds at ~2s
	//	per-gate budget (defect): sheds at ~3s
	//	threshold:                2.4s
	//
	// The earlier version of this test used bound=400ms / interval=250ms
	// against the shared 2s admissionSlack, which put the threshold at 2.4s and
	// passed the per-gate defect at 650ms. The discriminator must exceed the
	// slack, so this test carries its own tighter one.
	const bound = 2 * time.Second
	const interval = 1 * time.Second
	const sharedBudgetSlack = 400 * time.Millisecond

	limiter := NewRateLimiter(interval)
	defer limiter.Stop()
	d := newAdmissionSender(t, durPtr(bound), limiter, fullSemaphore(t), "http://test-broker")

	req := newGetRequest(t, "http://test-broker/x")
	start := time.Now()
	//nolint:bodyclose // the request is shed before it is sent; resp is always nil here
	_, err := d.Do(context.Background(), req)
	elapsed := time.Since(start)

	var busy *BrokerBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("got err %v (%T), want *BrokerBusyError", err, err)
	}
	// The first tick lands at 1s with 1s of budget still to run, so a correct
	// implementation always clears the limiter gate and sheds at the in-flight
	// cap. The 1s of headroom is what keeps this from flaking on a loaded
	// runner.
	if busy.Stage != AdmissionStageConcurrency {
		t.Fatalf("Stage = %q, want %q — the %s tick should have been admitted well inside the %s budget, "+
			"leaving the in-flight cap to shed", busy.Stage, AdmissionStageConcurrency, interval, bound)
	}
	if elapsed > bound+sharedBudgetSlack {
		t.Errorf("shed after %s, want within %s of the single %s budget; "+
			"a budget restarted at the second gate would shed at about %s",
			elapsed, sharedBudgetSlack, bound, interval+bound)
	}
}

// A caller that sets its own deadline keeps the pre-SOL-153442 contract: it
// sees the context error, never a BrokerBusyError. Covers both gates, since
// each has its own ctx.Done() case.
func TestSenderDo_CallerDeadlineWinsOverAdmissionBound(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		limiter *RateLimiter
		sem     Semaphore
	}{
		{name: "rate limit gate", limiter: NewRateLimiter(blockedInterval), sem: NewSemaphore(10)},
		{name: "concurrency gate", limiter: NewRateLimiter(0), sem: nil}, // sem filled below
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer tc.limiter.Stop()

			sem := tc.sem
			if sem == nil {
				sem = fullSemaphore(t)
			}
			// A bound far longer than the caller's deadline, so whichever fires
			// first is unambiguous.
			d := newAdmissionSender(t, durPtr(10*time.Second), tc.limiter, sem, "http://test-broker")

			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()

			req := newGetRequest(t, "http://test-broker/x")
			//nolint:bodyclose // the request is shed before it is sent; resp is always nil here
			_, err := d.Do(ctx, req)

			var busy *BrokerBusyError
			if errors.As(err, &busy) {
				t.Fatalf("got *BrokerBusyError; a caller's own deadline must surface as a context error")
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("got err %v, want context.DeadlineExceeded", err)
			}
		})
	}
}

// A caller whose deadline lands essentially level with the admission bound must
// still get the context error, not a busy error.
//
// This is the case a plain ctx.Err() check gets wrong: a timer fires straight
// from the runtime, whereas a context deadline has to propagate through
// cancelCtx.cancel before Err() reports it, so at these margins Err() still
// reads nil when the shed path looks. Reading ctx.Deadline() and comparing it
// against the clock is what makes it deterministic.
//
// Run at several margins on both sides of the bound, repeatedly, because the
// defect is probabilistic — a single run at any one margin passes by luck often
// enough to be worthless.
func TestSenderDo_CallerDeadlineAtBoundBoundary_ReportsContextError(t *testing.T) {
	t.Parallel()

	const bound = 100 * time.Millisecond

	for _, margin := range []time.Duration{
		5 * time.Millisecond,
		2 * time.Millisecond,
		1 * time.Millisecond,
		200 * time.Microsecond,
		0,
	} {
		t.Run(margin.String()+" before the bound", func(t *testing.T) {
			t.Parallel()

			for i := range 20 {
				limiter := NewRateLimiter(blockedInterval)
				d := newAdmissionSender(t, durPtr(bound), limiter, NewSemaphore(10), "http://test-broker")

				ctx, cancel := context.WithTimeout(context.Background(), bound-margin)
				req := newGetRequest(t, "http://test-broker/x")
				//nolint:bodyclose // the request is shed before it is sent; resp is always nil here
				_, err := d.Do(ctx, req)
				cancel()
				limiter.Stop()

				var busy *BrokerBusyError
				if errors.As(err, &busy) {
					t.Fatalf("run %d: got *BrokerBusyError for a caller whose own deadline expired %s "+
						"before the %s bound; the caller's deadline must win", i, margin, bound)
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("run %d: got err %v, want context.DeadlineExceeded", i, err)
				}
			}
		})
	}
}

// Neither shed path may consume a semaphore slot. A slot leaked here is never
// returned — Do's release defer is only registered once admission succeeds — so
// the broker would lose an in-flight slot permanently on every shed and
// eventually deadlock. Verified by reading, and pinned here because that
// regression is silent until a broker stops accepting work entirely.
func TestSenderDo_ShedDoesNotConsumeASemaphoreSlot(t *testing.T) {
	t.Parallel()

	t.Run("rate limit gate", func(t *testing.T) {
		t.Parallel()
		limiter := NewRateLimiter(blockedInterval)
		defer limiter.Stop()
		sem := NewSemaphore(2)
		d := newAdmissionSender(t, durPtr(50*time.Millisecond), limiter, sem, "http://test-broker")

		//nolint:bodyclose // the request is shed before it is sent; resp is always nil here
		_, err := d.Do(context.Background(), newGetRequest(t, "http://test-broker/x"))
		var busy *BrokerBusyError
		if !errors.As(err, &busy) {
			t.Fatalf("got err %v, want *BrokerBusyError", err)
		}
		if got := len(sem); got != 0 {
			t.Errorf("semaphore holds %d slot(s) after a shed at the rate-limit gate, want 0", got)
		}
	})

	t.Run("concurrency gate", func(t *testing.T) {
		t.Parallel()
		limiter := NewRateLimiter(0)
		defer limiter.Stop()
		// Capacity 2 with one slot taken, so a leak shows as 2 rather than
		// being masked by a full channel.
		sem := NewSemaphore(2)
		sem <- struct{}{}
		d := newAdmissionSender(t, durPtr(50*time.Millisecond), limiter, sem, "http://test-broker")

		// Fill the remaining slot so the gate genuinely blocks.
		sem <- struct{}{}

		//nolint:bodyclose // the request is shed before it is sent; resp is always nil here
		_, err := d.Do(context.Background(), newGetRequest(t, "http://test-broker/x"))
		var busy *BrokerBusyError
		if !errors.As(err, &busy) {
			t.Fatalf("got err %v, want *BrokerBusyError", err)
		}
		if busy.Stage != AdmissionStageConcurrency {
			t.Fatalf("Stage = %q, want %q", busy.Stage, AdmissionStageConcurrency)
		}
		if got := len(sem); got != 2 {
			t.Errorf("semaphore holds %d slot(s) after a shed at the concurrency gate, want the 2 "+
				"the test put there — a shed must not acquire one", got)
		}
	})
}

// A nil MaxQueueWait must leave the bound off entirely, preserving the old
// behavior for a hand-constructed config. Proven by showing the request still
// waits on the caller's context rather than shedding: with a bound in force it
// would be a BrokerBusyError instead.
func TestSenderDo_NilMaxQueueWaitLeavesAdmissionUnbounded(t *testing.T) {
	t.Parallel()

	limiter := NewRateLimiter(blockedInterval)
	defer limiter.Stop()
	d := newAdmissionSender(t, nil, limiter, NewSemaphore(10), "http://test-broker")

	if d.maxQueueWait != 0 {
		t.Fatalf("maxQueueWait = %s, want 0 (unbounded) for a nil config field", d.maxQueueWait)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	req := newGetRequest(t, "http://test-broker/x")
	//nolint:bodyclose // the request never gets past admission; resp is always nil here
	_, err := d.Do(ctx, req)

	var busy *BrokerBusyError
	if errors.As(err, &busy) {
		t.Fatalf("got *BrokerBusyError; a nil max_queue_wait must not bound admission")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got err %v, want context.DeadlineExceeded", err)
	}
}

// The common case must be untouched: a request admitted inside the window
// completes normally, with no deadline of its own and a bound configured.
func TestSenderDo_WithinAdmissionWindow_SucceedsUnchanged(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	// A real pacing interval, well inside a 5s bound.
	limiter := NewRateLimiter(10 * time.Millisecond)
	defer limiter.Stop()

	retries := 0
	minInterval := 10 * time.Millisecond
	sempCfg := &config.SEMPConfig{
		Retries:                &retries,
		RequestMinInterval:     &minInterval,
		MaxQueueWait:           durPtr(5 * time.Second),
		RequestTimeoutDuration: 30 * time.Second,
		RetryMinInterval:       1 * time.Millisecond,
		RetryMaxInterval:       10 * time.Millisecond,
	}
	d := New(server.Client(), sempCfg, bearerAuth(t), server.URL, NewSemaphore(10), limiter)

	// Several sequential requests, so the pacing gate is genuinely exercised
	// rather than satisfied once by the ticker's buffered first tick.
	for i := range 3 {
		req := newGetRequest(t, server.URL)
		resp, err := d.Do(context.Background(), req)
		if err != nil {
			t.Fatalf("request %d: unexpected error %v; a request inside the admission window must be unaffected", i, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200", i, resp.StatusCode)
		}
	}
}

// The error text is what an operator reads in the server log, so pin the facts
// it has to carry: the bound, the gate, and that nothing was sent.
func TestBrokerBusyError_Message(t *testing.T) {
	t.Parallel()

	err := &BrokerBusyError{
		Stage:   AdmissionStageConcurrency,
		Waited:  31 * time.Second,
		MaxWait: 30 * time.Second,
	}
	got := err.Error()
	for _, want := range []string{"broker busy", "30s", "concurrency", "31s", "was not sent"} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q, want it to mention %q", got, want)
		}
	}
}
