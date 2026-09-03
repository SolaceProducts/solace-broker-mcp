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
	"testing"
	"time"
)

// The tests in admission_test.go and saturation_test.go all run against the
// UNSCHEDULED admission path, because their Senders are built without a
// Scheduler. Fair scheduling replaces that path wholesale, so every contract
// those two files pin — the single shared max_queue_wait budget, the typed shed
// with its stage vocabulary, caller-deadline precedence, and exactly one
// saturation warning per request — has to be re-pinned here or SOL-153441
// silently regresses the two commits below it in this stack.
//
// Real time rather than synctest, matching the style of the files these
// mirror: the scheduler's own rotation is proven deterministically in
// scheduler_test.go, and what these need is the Sender wired to it.

// newScheduledSender builds a Sender whose admission goes through a Scheduler
// over the same sem and limiter, which is the production wiring.
func newScheduledSender(t *testing.T, maxQueueWait *time.Duration, limiter *RateLimiter, sem Semaphore, saturation bool) (*Sender, *Scheduler) {
	t.Helper()
	sched := NewScheduler(sem, limiter, cap(sem))
	t.Cleanup(sched.Stop)

	opts := []Option{WithScheduler(sched)}
	if saturation {
		opts = append(opts, WithSaturationEvents(slowAdmissionThreshold))
	}
	return newAdmissionSenderWithOpts(t, maxQueueWait, limiter, sem, opts...), sched
}

// occupyEverySlot fills the semaphore THROUGH the scheduler, on behalf of a
// caller distinct from the one under test.
//
// Deliberately not fullSemaphore(t), which pushes onto the channel directly:
// that would leave the scheduler's own in-flight accounting reading zero while
// the channel is full, so eligibility would believe capacity exists and the
// dispatcher's send would block with the mutex held. Any test that fills a
// semaphore the scheduler owns has to go through the scheduler.
func occupyEverySlot(t *testing.T, s *Scheduler, key CallerKey) []*Waiter {
	t.Helper()
	held := make([]*Waiter, 0, cap(s.sem))
	for range cap(s.sem) {
		w := s.Enqueue(key)
		select {
		case <-w.Granted():
			if err := w.Err(); err != nil {
				t.Fatalf("occupying a slot: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("could not occupy the semaphore within 5s")
		}
		held = append(held, w)
	}
	t.Cleanup(func() {
		for _, w := range held {
			s.Release(w)
		}
	})
	return held
}

// A caller with no deadline must still get a bounded, typed failure on the
// scheduled path. This is SOL-153442's core guarantee, and losing it would
// restore the indefinite hang that commit removed.
func TestAdmitScheduled_ShedsWithinBound(t *testing.T) {
	for _, budget := range []time.Duration{80 * time.Millisecond, 200 * time.Millisecond} {
		t.Run(budget.String(), func(t *testing.T) {
			limiter := NewRateLimiter(blockedInterval)
			t.Cleanup(limiter.Stop)
			sender, _ := newScheduledSender(t, durPtr(budget), limiter, NewSemaphore(1), false)

			start := time.Now()
			_, err := sender.admit(context.Background())
			waited := time.Since(start)

			var busy *BrokerBusyError
			if !errors.As(err, &busy) {
				t.Fatalf("admit() error = %v, want a *BrokerBusyError", err)
			}
			if busy.MaxWait != budget {
				t.Errorf("BrokerBusyError.MaxWait = %v, want the configured %v", busy.MaxWait, budget)
			}
			if busy.Stage != AdmissionStageRateLimit {
				t.Errorf("Stage = %q, want %q: nothing was in flight, so the pace was the "+
					"binding constraint", busy.Stage, AdmissionStageRateLimit)
			}
			if waited < budget {
				t.Errorf("shed after %v, before the %v budget elapsed", waited, budget)
			}
			if waited > budget+admissionSlack {
				t.Errorf("shed after %v, more than %v past the %v budget", waited, admissionSlack, budget)
			}
		})
	}
}

// A request held up by the in-flight cap rather than the pace must say so, in
// the same stage vocabulary SOL-153442 established. It is what points an
// operator at max_concurrent_per_broker instead of request_min_interval.
func TestAdmitScheduled_ShedAtConcurrencyReportsConcurrencyStage(t *testing.T) {
	budget := 120 * time.Millisecond
	// Pace disabled, so the ONLY thing that can hold a request up is the slot
	// gate. That isolates the stage attribution.
	limiter := NewRateLimiter(0)
	t.Cleanup(limiter.Stop)
	sender, sched := newScheduledSender(t, durPtr(budget), limiter, NewSemaphore(1), false)

	occupyEverySlot(t, sched, CallerKey{Subject: "occupier"})

	ctx := WithCallerKey(context.Background(), CallerKey{Subject: "victim"})
	_, err := sender.admit(ctx)

	var busy *BrokerBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("admit() error = %v, want a *BrokerBusyError", err)
	}
	if busy.Stage != AdmissionStageConcurrency {
		t.Errorf("Stage = %q, want %q: every in-flight slot was held, so concurrency was "+
			"the binding constraint", busy.Stage, AdmissionStageConcurrency)
	}
}

// A caller that sets its own deadline must see the context error, never a busy
// error. SOL-153442 got this right with a deadline comparison ahead of the
// ctx.Err() check, precisely because a timer and a context cancellation race;
// the scheduled path routes through the same shed() and must inherit it.
func TestAdmitScheduled_CallerDeadlineWinsOverAdmissionBound(t *testing.T) {
	budget := 2 * time.Second
	limiter := NewRateLimiter(blockedInterval)
	t.Cleanup(limiter.Stop)
	sender, _ := newScheduledSender(t, durPtr(budget), limiter, NewSemaphore(1), false)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	_, err := sender.admit(ctx)

	var busy *BrokerBusyError
	if errors.As(err, &busy) {
		t.Fatalf("admit() returned a BrokerBusyError for a caller whose own deadline expired "+
			"first: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("admit() error = %v, want context.DeadlineExceeded", err)
	}
}

// An explicit cancellation must surface as the context error too, and must not
// leave the withdrawn request in the scheduler's queue.
func TestAdmitScheduled_CancellationWithdrawsTheWaiter(t *testing.T) {
	limiter := NewRateLimiter(blockedInterval)
	t.Cleanup(limiter.Stop)
	sender, sched := newScheduledSender(t, nil, limiter, NewSemaphore(1), false)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := sender.admit(ctx)
		errCh <- err
	}()

	// Give the request time to enqueue, then withdraw it.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("admit() error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admit() did not return after its context was cancelled")
	}

	// The abandoned waiter must be gone, and with it its caller's state.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, n, _ := sched.stats()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, n, _ := sched.stats()
	t.Errorf("%d caller entries left after the only request was cancelled, want 0: an "+
		"abandoned waiter is still occupying its queue", n)
}

// A shed request must not leak the slot the scheduler might have been about to
// hand it. Asserted on the semaphore, because that is also what
// semp.BrokerClient.InFlight reports to operators: a leak here would show as
// permanent phantom load and would eventually wedge admission entirely.
func TestAdmitScheduled_ShedLeaksNoSlot(t *testing.T) {
	budget := 60 * time.Millisecond
	limiter := NewRateLimiter(blockedInterval)
	t.Cleanup(limiter.Stop)
	sem := NewSemaphore(2)
	sender, sched := newScheduledSender(t, durPtr(budget), limiter, sem, false)

	for i := range 5 {
		if _, err := sender.admit(context.Background()); err == nil {
			t.Fatalf("request %d was admitted; the pace was supposed to be blocked", i)
		}
	}

	if got := len(sem); got != 0 {
		t.Errorf("%d in-flight slots held after every request was shed, want 0", got)
	}
	_, callers, inFlight := sched.stats()
	if inFlight != 0 {
		t.Errorf("scheduler in-flight accounting = %d, want 0", inFlight)
	}
	if callers != 0 {
		t.Errorf("%d caller entries retained after every request was shed, want 0", callers)
	}
}

// The saturation warning must still fire exactly once per request on the
// scheduled path. SOL-153443 sets slow = nil after the first fire so a
// saturated broker cannot flood the log and get the signal switched off;
// admitScheduled has its own select and its own copy of that logic.
func TestAdmitScheduled_SlowAdmission_WarnsOnlyOnce(t *testing.T) {
	logs := captureLogs(t)
	limiter := NewRateLimiter(blockedInterval)
	t.Cleanup(limiter.Stop)
	sender, _ := newScheduledSender(t, nil, limiter, NewSemaphore(1), true)

	go func() { _, _ = sender.admit(context.Background()) }()

	waitForLogLine(t, logs, slowAdmissionMsg, 1)
	// Stay parked well past several more threshold periods.
	time.Sleep(6 * slowAdmissionThreshold)

	if n := len(logs.matching(t, slowAdmissionMsg)); n != 1 {
		t.Errorf("saturation warning fired %d times for one request, want exactly 1: a "+
			"re-firing signal floods the log of a saturated broker", n)
	}
}

// The warning must arrive WHILE the request waits, not when it resolves. Under
// max_queue_wait: 0 a resolution-time signal would never arrive at all, which
// is the case that most needs one.
func TestAdmitScheduled_SlowAdmission_WarnsDuringTheWait(t *testing.T) {
	logs := captureLogs(t)
	limiter := NewRateLimiter(blockedInterval)
	t.Cleanup(limiter.Stop)
	// Unbounded admission: nothing will ever resolve this request.
	sender, _ := newScheduledSender(t, nil, limiter, NewSemaphore(1), true)

	go func() { _, _ = sender.admit(context.Background()) }()

	waitForLogLine(t, logs, slowAdmissionMsg, 1)

	rec := logs.matching(t, slowAdmissionMsg)[0]
	if got := rec["stage"]; got != AdmissionStageRateLimit {
		t.Errorf("warning stage = %v, want %q", got, AdmissionStageRateLimit)
	}
	if _, ok := rec["waited"]; !ok {
		t.Error("warning carries no waited field")
	}
}

// max_queue_wait: 0 must restore unbounded waiting on the scheduled path too.
// It is a documented setting, and a scheduler that quietly bounded it anyway
// would be a behavior change nobody asked for.
func TestAdmitScheduled_ZeroBudgetWaitsIndefinitely(t *testing.T) {
	limiter := NewRateLimiter(blockedInterval)
	t.Cleanup(limiter.Stop)
	zero := time.Duration(0)
	sender, _ := newScheduledSender(t, &zero, limiter, NewSemaphore(1), false)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	_, err := sender.admit(ctx)

	var busy *BrokerBusyError
	if errors.As(err, &busy) {
		t.Fatalf("admit() shed with max_queue_wait: 0, which must mean unbounded: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("admit() error = %v, want the caller's own deadline to be the only bound", err)
	}
}

// With both throttling and the admission bound disabled, admission is gated on
// slots alone and must still work rather than deadlock. This is the
// configuration where the scheduler runs no paced dispatcher at all.
func TestAdmitScheduled_NoThrottleNoBoundStillAdmits(t *testing.T) {
	limiter := NewRateLimiter(0)
	t.Cleanup(limiter.Stop)
	zero := time.Duration(0)
	sender, _ := newScheduledSender(t, &zero, limiter, NewSemaphore(2), false)

	done := make(chan error, 1)
	go func() {
		w, err := sender.admit(context.Background())
		if err == nil {
			sender.releaseAdmission(w)
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("admit() error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admit() blocked with throttling and the admission bound both disabled")
	}
}

// An operator triaging "who is being throttled right now" needs the waiting
// caller's identity on the saturation warning itself (SOL-153441 review),
// which is only wired on the scheduled path — the plain gates in admit() have
// no caller key to report. Asserted against the raw Subject unchanged because
// it is well under sanitize.Claim's 256-byte cap; the truncation behavior
// itself is pinned in identity_test.go and callerkey_test.go, not re-proven
// here.
func TestAdmitScheduled_SlowAdmission_LogsCallerSubject(t *testing.T) {
	logs := captureLogs(t)
	limiter := NewRateLimiter(blockedInterval)
	t.Cleanup(limiter.Stop)
	sender, _ := newScheduledSender(t, nil, limiter, NewSemaphore(1), true)

	ctx := WithCallerKey(context.Background(), CallerKey{Subject: "victim-sub"})
	go func() { _, _ = sender.admit(ctx) }()

	waitForLogLine(t, logs, slowAdmissionMsg, 1)

	rec := logs.matching(t, slowAdmissionMsg)[0]
	if got := rec["caller_sub"]; got != "victim-sub" {
		t.Errorf("caller_sub = %v, want the waiting caller's subject", got)
	}
}

// The shed warning needs the same correlation, for the same reason: it is the
// other half of the pair the review named ("still waiting" / "admission bound
// exceeded"), and an incident can end in either one depending on timing.
func TestAdmitScheduled_Shed_LogsCallerSubject(t *testing.T) {
	logs := captureLogs(t)
	budget := 80 * time.Millisecond
	limiter := NewRateLimiter(blockedInterval)
	t.Cleanup(limiter.Stop)
	sender, _ := newScheduledSender(t, durPtr(budget), limiter, NewSemaphore(1), false)

	ctx := WithCallerKey(context.Background(), CallerKey{Subject: "shed-sub"})
	_, err := sender.admit(ctx)

	var busy *BrokerBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("admit() error = %v, want a *BrokerBusyError", err)
	}

	rec := logs.matching(t, "request shed: broker admission bound exceeded")[0]
	if got := rec["caller_sub"]; got != "shed-sub" {
		t.Errorf("caller_sub = %v, want the shed caller's subject", got)
	}
}

// A request with no caller key stamped — every request through the plain,
// unscheduled admit() path — must report the same "<absent>" sentinel the
// audit log uses for an empty claim, not an empty string that would make the
// field disappear from a query for it.
func TestAdmit_SlowAdmission_LogsAbsentCallerSubjectWhenUnstamped(t *testing.T) {
	logs := captureLogs(t)
	sender := newSaturationSender(t, nil, NewRateLimiter(blockedInterval), NewSemaphore(1))

	go func() { _, _ = sender.admit(context.Background()) }()

	waitForLogLine(t, logs, slowAdmissionMsg, 1)

	rec := logs.matching(t, slowAdmissionMsg)[0]
	if got := rec["caller_sub"]; got != "<absent>" {
		t.Errorf("caller_sub = %v, want the absent sentinel for an unstamped request", got)
	}
}
