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
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Every timing test here runs inside a synctest bubble, so "time" is the
// bubble's virtual clock: it advances only when every goroutine is durably
// blocked, and it advances exactly. A tick interval is therefore an exact
// quantum rather than a racy approximation, which is what makes assertions like
// "exactly 20 admissions in 20 intervals" safe to write on a loaded CI runner.
//
// The alternative — real time plus sleeps and tolerances — is what a concurrent
// state machine like this normally degenerates into, and it is why the ticker
// is injected through RateLimiter rather than constructed inside the scheduler.

const testInterval = 10 * time.Millisecond

// newTestScheduler builds a scheduler with pacing on. Must be called inside a
// synctest bubble so the ticker belongs to the bubble's clock.
func newTestScheduler(t *testing.T, limit int) *Scheduler {
	t.Helper()
	limiter := NewRateLimiter(testInterval)
	t.Cleanup(limiter.Stop)
	s := NewScheduler(NewSemaphore(limit), limiter, limit)
	t.Cleanup(s.Stop)
	return s
}

// grant enqueues one request and blocks until it is admitted, failing the test
// if the scheduler stops first.
func grant(t *testing.T, s *Scheduler, key CallerKey) *Waiter {
	t.Helper()
	w := s.Enqueue(key)
	<-w.Granted()
	if err := w.Err(); err != nil {
		t.Fatalf("Enqueue(%+v): unexpected error %v", key, err)
	}
	return w
}

// tryGrant reports whether the SLOT rules admit a request for key. Used to
// assert that a per-caller ceiling or the last-slot reservation is refusing
// admission.
//
// The sleep is load-bearing and not a timing fudge. Without it the check races
// the pace: a waiter that no tick has reached yet looks identical to one the
// slot rules refused, so every "was refused" assertion would pass for the wrong
// reason and the slot rules would be untested. Sleeping past a full interval
// guarantees a tick has arrived and is held as credit, so a waiter still
// unresolved after it was refused on slots. The virtual clock makes "past a
// full interval" exact.
func tryGrant(s *Scheduler, key CallerKey) (*Waiter, bool) {
	w := s.Enqueue(key)
	time.Sleep(2 * testInterval)
	synctest.Wait()
	select {
	case <-w.Granted():
		return w, w.Err() == nil
	default:
		return w, false
	}
}

// A burst from one caller must not crowd out a caller issuing one request at a
// time. This is the ticket's headline contract and the single most important
// test in this file.
//
// The burst caller runs 50 concurrent requests, modelling a composite fan-out
// (internal/composite/executor.go issues one SEMP call per row at up to 32-way
// concurrency, each paginating). The steady caller runs exactly one at a time,
// modelling an operator's status check.
//
// Under the plain FIFO gates this replaces, admission order is arrival order,
// so the steady caller would take roughly 1/51 of the pace. Round-robin over
// per-caller subqueues makes it 1/2 regardless of how much either caller has
// outstanding.
//
// The deep-queue-versus-single-request shape is also what proves the ring
// re-insertion rule: the steady caller empties its subqueue after every grant
// and is pruned from the rotation each time, so if it were re-inserted anywhere
// but behind the cursor it would be systematically over- or under-served.
func TestScheduler_BurstCallerCannotCrowdOutSteadyCaller(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Generous cap: the in-flight gate must provably not be the constraint,
		// so any share observed comes from the pace rotation.
		s := newTestScheduler(t, 200)

		burst := CallerKey{Subject: "burst-agent", Session: "s1"}
		steady := CallerKey{Subject: "operator", Session: "s2"}

		var mu sync.Mutex
		admitted := map[CallerKey]int{}
		record := func(k CallerKey) {
			mu.Lock()
			admitted[k]++
			mu.Unlock()
		}

		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup

		worker := func(key CallerKey) {
			defer wg.Done()
			for {
				w := s.Enqueue(key)
				select {
				case <-w.Granted():
					if w.Err() != nil {
						return
					}
					record(key)
					s.Release(w)
				case <-ctx.Done():
					s.Abandon(w)
					return
				}
				if ctx.Err() != nil {
					return
				}
			}
		}

		const burstConcurrency = 50
		for range burstConcurrency {
			wg.Add(1)
			go worker(burst)
		}
		wg.Add(1)
		go worker(steady)

		// 200 intervals of virtual time. Exact, because the clock only moves
		// when every goroutine is blocked.
		const intervals = 200
		time.Sleep(intervals * testInterval)
		cancel()
		wg.Wait()

		mu.Lock()
		gotBurst, gotSteady := admitted[burst], admitted[steady]
		mu.Unlock()

		total := gotBurst + gotSteady
		if total == 0 {
			t.Fatal("no requests were admitted at all")
		}

		// The contract is an equal share of the pace. Allow a modest band for
		// the handful of admissions in flight when the clock stops; the defect
		// this guards produces roughly 1/51, not 1/2.
		steadyShare := float64(gotSteady) / float64(total)
		if steadyShare < 0.40 || steadyShare > 0.60 {
			t.Errorf("steady caller got %d of %d admissions (%.1f%%), want ~50%%: "+
				"one caller's burst is crowding out a caller issuing one request at a time "+
				"(burst=%d, steady=%d)",
				gotSteady, total, steadyShare*100, gotBurst, gotSteady)
		}
	})
}

// Fairness has to hold across more than two callers, and specifically when
// intermittent callers keep leaving and re-entering the rotation.
//
// This is the case the ring-insertion rule exists for. A caller issuing one
// request at a time empties its subqueue after every grant and is pruned, so it
// re-enters the ring constantly, while a caller with a deep standing queue never
// leaves it. Where a returning caller is placed decides whether it gets one turn
// per lap or is repeatedly served early or skipped. Two callers cannot show this
// — the rotation is symmetric — so this uses four.
func TestScheduler_FairShareHoldsAcrossManyIntermittentCallers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestScheduler(t, 200)

		deep := CallerKey{Subject: "deep"}
		steady := []CallerKey{
			{Subject: "steady-1"},
			{Subject: "steady-2"},
			{Subject: "steady-3"},
		}

		var mu sync.Mutex
		admitted := map[CallerKey]int{}

		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		worker := func(key CallerKey) {
			defer wg.Done()
			for {
				w := s.Enqueue(key)
				select {
				case <-w.Granted():
					if w.Err() != nil {
						return
					}
					mu.Lock()
					admitted[key]++
					mu.Unlock()
					s.Release(w)
				case <-ctx.Done():
					s.Abandon(w)
					return
				}
				if ctx.Err() != nil {
					return
				}
			}
		}

		// The deep caller keeps 20 requests outstanding at all times.
		for range 20 {
			wg.Add(1)
			go worker(deep)
		}
		// The intermittent callers hold exactly one at a time, so each is
		// pruned from the ring and re-inserted after every single grant.
		for _, k := range steady {
			wg.Add(1)
			go worker(k)
		}

		time.Sleep(400 * testInterval)
		cancel()
		wg.Wait()

		mu.Lock()
		snapshot := make(map[CallerKey]int, len(admitted))
		total := 0
		for k, v := range admitted {
			snapshot[k] = v
			total += v
		}
		mu.Unlock()

		if total == 0 {
			t.Fatal("no requests were admitted")
		}
		const callers = 4
		want := 1.0 / callers
		for _, k := range append([]CallerKey{deep}, steady...) {
			share := float64(snapshot[k]) / float64(total)
			if share < want-0.06 || share > want+0.06 {
				t.Errorf("caller %q got %d of %d admissions (%.1f%%), want ~%.0f%%: "+
					"a caller that leaves and re-enters the rotation after every request "+
					"is not getting one turn per lap (all shares: %v)",
					k.Subject, snapshot[k], total, share*100, want*100, snapshot)
			}
		}
	})
}

// Fairness must not cost throughput. With a single active caller the scheduler
// has to be fully work-conserving: the broker still sees exactly the configured
// rate, not a fraction of it reserved for callers that do not exist.
//
// Asserted as an exact count, which the virtual clock makes possible: in N
// intervals a correct scheduler admits exactly N requests.
func TestScheduler_SingleCallerGetsTheWholeConfiguredRate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestScheduler(t, 50)
		only := CallerKey{Subject: "solo"}

		var mu sync.Mutex
		count := 0

		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					w := s.Enqueue(only)
					select {
					case <-w.Granted():
						if w.Err() != nil {
							return
						}
						mu.Lock()
						count++
						mu.Unlock()
						s.Release(w)
					case <-ctx.Done():
						s.Abandon(w)
						return
					}
					if ctx.Err() != nil {
						return
					}
				}
			}()
		}

		const intervals = 100
		time.Sleep(intervals*testInterval + testInterval/2)
		cancel()
		wg.Wait()

		mu.Lock()
		got := count
		mu.Unlock()

		// One tick admits one request. The limiter buffers a single tick, so
		// the very first request is admitted immediately, giving intervals+1.
		if got < intervals || got > intervals+2 {
			t.Errorf("admitted %d requests in %d intervals, want ~%d: a single active caller "+
				"must receive the whole configured rate, not a reserved fraction of it",
				got, intervals, intervals+1)
		}
	})
}

// The last-slot reservation is conditioned on contention (SOL-153441 decision
// A), so a lone caller must be able to occupy every in-flight slot.
//
// An unconditional reservation would leave one slot permanently idle, costing
// 10% of semp.max_concurrent_per_broker at its default of 10 in the
// single-agent deployment that is most common today. It would also break
// TestSenderDo_SharedSemaphoreEnforcesPerBrokerCap, which asserts a cap of 3 is
// reachable by one caller.
func TestScheduler_SingleCallerReachesTheFullInFlightCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const limit = 3
		s := newTestScheduler(t, limit)
		only := CallerKey{Subject: "solo"}

		held := make([]*Waiter, 0, limit)
		for i := range limit {
			w := grant(t, s, only)
			held = append(held, w)
			if got := len(s.sem); got != i+1 {
				t.Fatalf("after %d grants, in-flight is %d, want %d", i+1, got, i+1)
			}
		}

		// The cap itself must still hold: a fourth request waits.
		if w, ok := tryGrant(s, only); ok {
			t.Errorf("a %dth request was admitted past the cap of %d", limit+1, limit)
			s.Release(w)
		} else {
			s.Abandon(w)
		}

		for _, w := range held {
			s.Release(w)
		}
	})
}

// With more than one caller active, the final free slot is withheld from a
// caller that already holds one, so a newly arrived caller's wait is bounded by
// a single completion rather than by the other caller's whole retry chain.
//
// The scenario is the one that motivated the rule: a slot is held for an entire
// retry chain, up to roughly 16 minutes at default settings, so a caller that
// takes every slot during a degraded episode would otherwise block everyone
// else for that long.
func TestScheduler_LastSlotIsWithheldUnderContention(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const limit = 4
		s := newTestScheduler(t, limit)
		hog := CallerKey{Subject: "hog"}
		other := CallerKey{Subject: "other"}

		// The occupancy here is chosen so the RESERVATION is the only thing
		// that can refuse, which the obvious arrangement does not achieve. With
		// three subjects active the per-subject ceiling is ceil(4/3) = 2, so a
		// subject already holding 2 is refused by the ceiling and the
		// reservation is never consulted — a test built that way passes with
		// the reservation deleted, which is no test at all.
		//
		// So: hog holds 1 (under its ceiling) and other holds 2, leaving
		// exactly one slot free. The ceiling permits hog another; only the
		// reservation stands in the way.
		hogW := grant(t, s, hog)
		held := []*Waiter{hogW, grant(t, s, other), grant(t, s, other)}

		if free := limit - len(s.sem); free != 1 {
			t.Fatalf("test setup: %d slots free, want exactly 1", free)
		}

		// A third caller with work queued and no slots is what the reservation
		// is FOR, and its presence is what arms the rule. Without a starved
		// peer the last slot would be idling for nobody, which is a capacity
		// cost with no benefit — so the rule deliberately does not fire then,
		// and this test would be asserting the wrong thing without this waiter.
		starved := CallerKey{Subject: "starved"}
		starvedW := s.Enqueue(starved)
		synctest.Wait()

		// The hog holds one slot and is under its ceiling, but the last free
		// slot is reserved for the caller holding none.
		if w, ok := tryGrant(s, hog); ok {
			t.Errorf("the caller already holding slots took the last free slot while another "+
				"caller was queued with none; that caller would then wait for a whole retry "+
				"chain (in-flight %d of %d)", len(s.sem), limit)
			s.Release(w)
		} else {
			s.Abandon(w)
		}

		// And the starved caller is the one that gets it.
		select {
		case <-starvedW.Granted():
			if err := starvedW.Err(); err != nil {
				t.Fatalf("starved caller: %v", err)
			}
			held = append(held, starvedW)
		default:
			t.Error("the caller holding no slots never received the reserved slot; the " +
				"reservation exists precisely to keep it available to that caller")
			s.Abandon(starvedW)
		}

		for _, h := range held {
			s.Release(h)
		}
	})
}

// With no starved peer, the last slot must NOT be withheld.
//
// This is the other half of the reservation's contract and the reason it is
// conditioned on a beneficiary rather than on mere contention. Reserving
// whenever two callers are active idles one of the ten default slots any time
// both are busy — a 10% capacity loss that protects nobody, because the caller
// it is being held for does not exist.
func TestScheduler_LastSlotIsGrantedWhenNoPeerIsStarved(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const limit = 4
		s := newTestScheduler(t, limit)
		a := CallerKey{Subject: "a"}
		b := CallerKey{Subject: "b"}

		// Both subjects active and both holding slots: a holds 1, b holds 2.
		held := []*Waiter{grant(t, s, a), grant(t, s, b), grant(t, s, b)}
		if free := limit - len(s.sem); free != 1 {
			t.Fatalf("test setup: %d slots free, want exactly 1", free)
		}

		// Nobody is queued with zero slots, so the last one is usable.
		w, ok := tryGrant(s, a)
		if !ok {
			t.Error("the last free slot was withheld with no starved caller to hold it for: " +
				"that idles capacity permanently under contention and protects nobody")
			s.Abandon(w)
		} else {
			held = append(held, w)
		}

		for _, h := range held {
			s.Release(h)
		}
	})
}

// A caller in a retry storm holds each slot for the whole chain. It must not be
// able to accumulate every slot, or a second caller is blocked for the full
// retry budget.
//
// The per-caller ceiling is what bounds this: with two callers active it is
// half the cap, so the storming caller tops out well short of the whole.
func TestScheduler_RetryStormCallerCannotHoldEverySlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const limit = 8
		s := newTestScheduler(t, limit)
		storm := CallerKey{Subject: "storming"}
		victim := CallerKey{Subject: "victim"}

		// The victim is active from the start, which is what makes the ceiling
		// bite. It holds one slot and never releases it during the storm.
		victimW := grant(t, s, victim)

		// The storming caller grabs everything it can.
		var stormHeld []*Waiter
		for range limit * 2 {
			w, ok := tryGrant(s, storm)
			if !ok {
				s.Abandon(w)
				break
			}
			stormHeld = append(stormHeld, w)
		}

		ceiling := (limit + 1) / 2 // ceil(8/2)
		if len(stormHeld) > ceiling {
			t.Errorf("the storming caller holds %d of %d slots, above its ceiling of %d: "+
				"one caller in a retry storm can pin the broker for every other caller",
				len(stormHeld), limit, ceiling)
		}
		if len(stormHeld) == limit {
			t.Errorf("the storming caller took every one of the %d slots", limit)
		}

		// The victim must still be able to make progress.
		w, ok := tryGrant(s, victim)
		if !ok {
			t.Error("the victim could not be admitted while the other caller was storming")
			s.Abandon(w)
		} else {
			s.Release(w)
		}

		s.Release(victimW)
		for _, h := range stormHeld {
			s.Release(h)
		}
	})
}

// Every slot the scheduler hands out must come back. The accounting is checked
// against the semaphore itself, which is also what semp.BrokerClient.InFlight
// reports, so a leak here would show up to operators as permanent phantom load
// and would eventually wedge the broker's admission entirely.
//
// Three resolution shapes are mixed deliberately, including abandon AFTER a
// grant — which is the one that actually reaches the slot-handback branch.
//
// Abandoning without waiting does NOT reach it: inside a synctest bubble the
// clock only advances once every goroutine is durably blocked, so an
// Enqueue-then-Abandon pair with no blocking point between them can never have
// a tick arrive in the middle, and the waiter is always withdrawn before any
// grant. Only a goroutine that waits for Granted and then abandons exercises
// the handback.
func TestScheduler_EverySlotGrantedIsReleased(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const limit = 6
		s := newTestScheduler(t, limit)

		var wg sync.WaitGroup
		for i := range 40 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				key := CallerKey{Subject: "caller", Session: string(rune('a' + i%4))}
				w := s.Enqueue(key)
				switch i % 3 {
				case 0:
					// Withdrawn before any grant can land.
					s.Abandon(w)
				case 1:
					// Granted, then released normally.
					<-w.Granted()
					if w.Err() == nil {
						s.Release(w)
					}
				default:
					// Granted, THEN abandoned: the handback path.
					<-w.Granted()
					if w.Err() == nil {
						s.Abandon(w)
					}
				}
			}()
		}
		wg.Wait()
		synctest.Wait()

		if got := len(s.sem); got != 0 {
			t.Errorf("%d in-flight slots still held after every request resolved: "+
				"a grant that raced a context cancellation leaked its slot", got)
		}

		_, sessions, inFlight := s.stats()
		callers := sessions
		if inFlight != 0 {
			t.Errorf("scheduler in-flight accounting is %d, want 0", inFlight)
		}
		if callers != 0 {
			t.Errorf("%d caller entries survived, want 0", callers)
		}
	})
}

// Per-caller state must not accumulate over the life of the process. There is
// no TTL, sweeper, or LRU by design: state is created on enqueue and deleted
// once a caller has nothing queued and nothing in flight, so the bookkeeping
// scales with concurrent work rather than with callers ever seen.
//
// A thousand distinct callers is well past any realistic session count and
// leaves nothing behind.
func TestScheduler_PerCallerStateIsReclaimedWhenCallersGoIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestScheduler(t, 4)

		for i := range 1000 {
			key := CallerKey{Subject: "sub", Session: string(rune(i))}
			w := grant(t, s, key)
			s.Release(w)
		}
		synctest.Wait()

		subjects, callers, _ := s.stats()
		ring := subjects

		if callers != 0 {
			t.Errorf("%d caller entries retained after 1000 callers went idle, want 0: "+
				"per-caller state grows without bound over a long-running process", callers)
		}
		if ring != 0 {
			t.Errorf("%d round-robin ring entries retained, want 0", ring)
		}
	})
}

// A slot coming free must admit a waiting request immediately, not on the next
// pace tick (SOL-153441 decision C).
//
// Before the scheduler, a request that had already taken its tick parked
// directly on the semaphore and resumed the instant a slot freed. A dispatcher
// woken only by the ticker would lose that and add up to one full interval to
// every slot-blocked request, which is a regression against the code this
// replaces rather than a new cost of fairness.
func TestScheduler_ReleaseAdmitsAWaiterWithoutWaitingForTheNextTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const limit = 1
		s := newTestScheduler(t, limit)
		a := CallerKey{Subject: "a"}
		b := CallerKey{Subject: "b"}

		held := grant(t, s, a)

		// b cannot be admitted: the only slot is taken.
		w := s.Enqueue(b)
		synctest.Wait()
		select {
		case <-w.Granted():
			t.Fatal("b was admitted while the only in-flight slot was held")
		default:
		}

		// Let a full pace interval pass so a credit is definitely available and
		// the only thing still missing is the slot.
		time.Sleep(2 * testInterval)
		synctest.Wait()

		before := time.Now()
		s.Release(held)
		<-w.Granted()
		if err := w.Err(); err != nil {
			t.Fatalf("b: %v", err)
		}
		if waited := time.Since(before); waited != 0 {
			t.Errorf("b waited %v after the slot freed, want 0: the dispatcher is not "+
				"woken by a release, so a slot-blocked request pays a full pace interval", waited)
		}
		s.Release(w)
	})
}

// A tick that arrives when nobody can be served must be held, not dropped
// (SOL-153441 decision B). Dropping it would push the broker below the
// configured semp.request_min_interval, which the ticket requires to still hold
// in aggregate.
//
// The held credit is capped at one, matching RateLimiter's own contract that
// idle time is credit worth at most a single request. Several intervals of
// nobody-eligible must therefore still yield exactly one immediate admission
// when a slot frees, not a burst.
func TestScheduler_HeldTickIsSpentButNotAccumulated(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// limit = 2 and BOTH slots released together, deliberately.
		//
		// With limit = 1 this test passes without the credit cap at all: once
		// the single slot is retaken, `free == 0` refuses everything, so the
		// in-flight cap does the work the credit cap is supposed to be doing
		// and accumulated credit produces the identical observation. Freeing
		// two slots at once removes slots as the binding constraint, so the
		// only thing that can hold the second admission back is the credit
		// having been capped at one.
		const limit = 2
		s := newTestScheduler(t, limit)
		a := CallerKey{Subject: "a"}
		b := CallerKey{Subject: "b"}

		// a is alone, so it reaches the full cap.
		heldA := []*Waiter{grant(t, s, a), grant(t, s, a)}

		// Queue three requests for b, then let several intervals pass with
		// every slot occupied so ticks arrive with nobody eligible.
		queued := []*Waiter{s.Enqueue(b), s.Enqueue(b), s.Enqueue(b)}
		time.Sleep(5 * testInterval)
		synctest.Wait()

		// Free both slots. Exactly one of b's requests may be admitted: the one
		// held credit, spent once. Five idle intervals must not have banked
		// five.
		s.Release(heldA[0])
		s.Release(heldA[1])
		synctest.Wait()

		granted := 0
		for _, w := range queued {
			select {
			case <-w.Granted():
				granted++
			default:
			}
		}
		if granted != 1 {
			t.Errorf("%d requests admitted the instant two slots freed, want exactly 1: "+
				"five idle intervals accumulated credit, which lets a burst exceed "+
				"semp.request_min_interval and changes the profile the broker sees", granted)
		}

		for _, w := range queued {
			select {
			case <-w.Granted():
				if w.Err() == nil {
					s.Release(w)
				}
			default:
				s.Abandon(w)
			}
		}
	})
}

// One session of a subject must not be able to hold every in-flight slot and
// starve its sibling sessions.
//
// This is the case that matters most rather than an edge. Under an OAuth
// client_credentials grant there is exactly ONE subject, so the subject-level
// ceiling is the whole cap and the cross-subject last-slot reservation can
// never fire. Without a per-session sub-ceiling inside the subject's allowance,
// the slot rules give zero protection in precisely the deployment shape the
// fairness work exists for, and one session in a retry storm pins the broker
// for every other session for a full retry chain.
func TestScheduler_OneSessionCannotStarveSiblingSessionsOfSlots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const limit = 4
		s := newTestScheduler(t, limit)

		// One service-account subject, as client_credentials produces.
		const subject = "service-account"
		greedy := CallerKey{Subject: subject, Session: "sess-greedy"}
		sibling := CallerKey{Subject: subject, Session: "sess-sibling"}

		// Make both sessions known to the scheduler before the greedy one
		// starts taking slots, so the sub-ceiling is computed against two.
		primer := grant(t, s, sibling)

		var greedyHeld []*Waiter
		for range limit * 2 {
			w, ok := tryGrant(s, greedy)
			if !ok {
				s.Abandon(w)
				break
			}
			greedyHeld = append(greedyHeld, w)
		}

		if len(greedyHeld) >= limit {
			t.Errorf("one session took %d of %d slots: sessions of a single subject get no "+
				"slot protection from each other, which is exactly the client_credentials "+
				"shape this feature targets", len(greedyHeld), limit)
		}

		// And the sibling must still be able to make progress.
		w, ok := tryGrant(s, sibling)
		if !ok {
			t.Error("the sibling session could not be admitted while the other session of the " +
				"same subject was taking slots")
			s.Abandon(w)
		} else {
			s.Release(w)
		}

		s.Release(primer)
		for _, h := range greedyHeld {
			s.Release(h)
		}
	})
}

// rotor is used at both rotation levels now, so a regression in its cursor
// bookkeeping is a silent one-turn fairness skip at whichever level it hits.
//
// The compensation this pins — decrementing the cursor when an entry ahead of
// it is removed — is invisible to the end-to-end fairness tests, which stay
// green without it. A caller that leaves and re-enters is the common case in
// this scheduler, so removal happens constantly.
func TestRotor_RemovalKeepsTheCursorOnTheSameNextEntry(t *testing.T) {
	// Cursor sits on "c"; removing an entry BEFORE it must not skip "c".
	r := rotor{keys: []string{"a", "b", "c"}, cursor: 2} // "c" is next

	r.remove("a")
	if got := r.at(0); got != "c" {
		t.Errorf("after removing an entry ahead of the cursor, next = %q, want \"c\": the "+
			"cursor was not compensated, so a caller loses a turn", got)
	}

	// Removing the entry the cursor points AT must move on to the following one.
	r = rotor{keys: []string{"a", "b", "c"}, cursor: 1}
	r.remove("b")
	if got := r.at(0); got != "c" {
		t.Errorf("after removing the entry at the cursor, next = %q, want \"c\"", got)
	}

	// Removing an entry after the cursor leaves it alone.
	r = rotor{keys: []string{"a", "b", "c"}, cursor: 0}
	r.remove("c")
	if got := r.at(0); got != "a" {
		t.Errorf("after removing an entry behind the cursor, next = %q, want \"a\"", got)
	}

	// Removing the last entry resets the cursor rather than leaving it out of
	// range.
	r = rotor{keys: []string{"a"}, cursor: 0}
	r.remove("a")
	if r.len() != 0 || r.cursor != 0 {
		t.Errorf("after removing the only entry: len=%d cursor=%d, want 0 and 0", r.len(), r.cursor)
	}
}

// A newcomer must be served last in the current lap, at every insertion
// position including the two edges the fairness tests never reach.
func TestRotor_InsertPlacesNewcomerBehindTheCursor(t *testing.T) {
	// Into an empty rotor: the newcomer is the only entry and is next.
	var r rotor
	r.insert("a")
	if r.len() != 1 || r.at(0) != "a" {
		t.Fatalf("insert into an empty rotor: len=%d next=%q", r.len(), r.at(0))
	}

	// The incumbent keeps its turn; the newcomer waits a lap.
	r.insert("b")
	if got := r.at(0); got != "a" {
		t.Errorf("next = %q, want \"a\": the newcomer jumped ahead of the incumbent", got)
	}
	if got := r.at(1); got != "b" {
		t.Errorf("second = %q, want \"b\"", got)
	}

	// A full lap must visit each entry exactly once.
	seen := map[string]int{}
	for i := range r.len() {
		seen[r.at(i)]++
	}
	for _, k := range []string{"a", "b"} {
		if seen[k] != 1 {
			t.Errorf("entry %q appears %d times in one lap, want exactly 1", k, seen[k])
		}
	}
}

// semp.request_min_interval: 0 is a documented setting meaning "no throttle",
// and NewRateLimiter returns a CLOSED channel for it. A dispatcher looping on
// that channel would spin a full core per broker for the life of the process.
//
// With pacing off the scheduler must still work — admitting on the slot rules
// alone — and must not burn the CPU doing it. The test asserts the behavior
// (admission proceeds, bounded by the cap) rather than the CPU, but the
// scheduler reaching this state at all is what the paced flag exists for.
func TestScheduler_DisabledPacerAdmitsOnSlotsAloneWithoutSpinning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := NewRateLimiter(0)
		defer limiter.Stop()
		if limiter.Enabled() {
			t.Fatal("NewRateLimiter(0) must report throttling disabled")
		}

		const limit = 3
		s := NewScheduler(NewSemaphore(limit), limiter, limit)
		defer s.Stop()

		if s.paced {
			t.Fatal("scheduler must not run a paced dispatcher against a closed ticker channel")
		}

		key := CallerKey{Subject: "unthrottled"}
		var held []*Waiter
		for range limit {
			held = append(held, grant(t, s, key))
		}

		// The in-flight cap still applies with pacing off.
		if w, ok := tryGrant(s, key); ok {
			t.Error("the in-flight cap was not enforced with throttling disabled")
			s.Release(w)
		} else {
			s.Abandon(w)
		}

		for _, w := range held {
			s.Release(w)
		}
	})
}

// Stop must release everything parked in the queue with a definite error.
//
// A stopped ticker does not close its channel, so without this a waiter blocks
// until its own context ends — and a caller that set no deadline has none, so
// the request would hang for the life of the process during a drain.
func TestScheduler_StopReleasesQueuedWaitersWithADefiniteError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const limit = 1
		limiter := NewRateLimiter(testInterval)
		defer limiter.Stop()
		s := NewScheduler(NewSemaphore(limit), limiter, limit)

		held := grant(t, s, CallerKey{Subject: "holder"})

		queued := []*Waiter{
			s.Enqueue(CallerKey{Subject: "a"}),
			s.Enqueue(CallerKey{Subject: "b"}),
			s.Enqueue(CallerKey{Subject: "c"}),
		}
		synctest.Wait()

		s.Stop()

		for i, w := range queued {
			select {
			case <-w.Granted():
			default:
				t.Fatalf("queued waiter %d was left blocked after Stop; a caller with no "+
					"deadline would hang for the life of the process", i)
			}
			if !errors.Is(w.Err(), ErrSchedulerStopped) {
				t.Errorf("queued waiter %d resolved with %v, want ErrSchedulerStopped", i, w.Err())
			}
		}

		// A request arriving after Stop is refused rather than queued forever.
		late := s.Enqueue(CallerKey{Subject: "late"})
		select {
		case <-late.Granted():
			if !errors.Is(late.Err(), ErrSchedulerStopped) {
				t.Errorf("post-Stop enqueue resolved with %v, want ErrSchedulerStopped", late.Err())
			}
		default:
			t.Error("a request enqueued after Stop was left blocked")
		}

		// Stop is idempotent: BrokerClient.Close may run more than once.
		s.Stop()

		// The in-flight request is untouched: it is live against the broker and
		// its own Release still runs.
		s.Release(held)
	})
}

// Stopping a scheduler must not leave its dispatcher behind. Every broker
// client builds one, and every test broker would otherwise leak a goroutine —
// which -race does not detect, so nothing else in the suite would catch it.
func TestScheduler_StopTerminatesTheDispatcher(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := NewRateLimiter(testInterval)
		defer limiter.Stop()
		s := NewScheduler(NewSemaphore(4), limiter, 4)

		s.Stop()

		select {
		case <-s.done:
		default:
			t.Fatal("Stop returned while the dispatcher goroutine was still running")
		}
	})
}

// The caller key is hierarchical, and both levels have to be live: two sessions
// of one subject are distinct buckets, and so are two subjects.
//
// The subject level alone is not enough. Under an OAuth client_credentials
// grant the subject is a service account shared by every session an agent
// platform fronts, so keying on it alone would collapse the whole platform into
// one bucket and leave the scheduler inert in the deployment shape that ships.
func TestScheduler_SessionsOfOneSubjectAreDistinctBuckets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestScheduler(t, 200)

		// One service-account subject, two sessions. Fair scheduling must split
		// between them exactly as it would between two subjects.
		busy := CallerKey{Subject: "service-account", Session: "session-busy"}
		quiet := CallerKey{Subject: "service-account", Session: "session-quiet"}

		var mu sync.Mutex
		admitted := map[CallerKey]int{}

		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		worker := func(key CallerKey) {
			defer wg.Done()
			for {
				w := s.Enqueue(key)
				select {
				case <-w.Granted():
					if w.Err() != nil {
						return
					}
					mu.Lock()
					admitted[key]++
					mu.Unlock()
					s.Release(w)
				case <-ctx.Done():
					s.Abandon(w)
					return
				}
				if ctx.Err() != nil {
					return
				}
			}
		}
		for range 20 {
			wg.Add(1)
			go worker(busy)
		}
		wg.Add(1)
		go worker(quiet)

		time.Sleep(100 * testInterval)
		cancel()
		wg.Wait()

		mu.Lock()
		gotBusy, gotQuiet := admitted[busy], admitted[quiet]
		mu.Unlock()

		total := gotBusy + gotQuiet
		if total == 0 {
			t.Fatal("no requests were admitted")
		}
		share := float64(gotQuiet) / float64(total)
		if share < 0.40 || share > 0.60 {
			t.Errorf("the quiet session got %d of %d admissions (%.1f%%), want ~50%%: "+
				"sessions of one subject are collapsing into a single bucket, which is "+
				"exactly the client_credentials shape the hierarchy exists for",
				gotQuiet, total, share*100)
		}
	})
}

// A caller must not be able to enlarge its share by opening MCP sessions.
//
// This is the property the two-level rotation exists for, and the one the
// threat model and docs assert. Sessions are freely mintable: the server runs
// the stateful streamable-HTTP handler, so every `initialize` yields a new
// session ID, and nothing caps sessions per subject. If the rotation were flat
// over (subject, session) pairs — the obvious and wrong implementation — a
// subject with N sessions would take N/(N+1) of the pace against a
// single-session subject, which at N=10 is 91%.
//
// The outer rotation is over SUBJECTS, so a subject gets one turn per lap
// however many sessions it holds.
func TestScheduler_ExtraSessionsDoNotEnlargeASubjectsShare(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestScheduler(t, 200)

		const greedySessions = 10
		greedy := make([]CallerKey, 0, greedySessions)
		for i := range greedySessions {
			greedy = append(greedy, CallerKey{
				Subject: "greedy-subject",
				Session: string(rune('a' + i)),
			})
		}
		honest := CallerKey{Subject: "honest-subject", Session: "only"}

		var mu sync.Mutex
		bySubject := map[string]int{}

		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		worker := func(key CallerKey) {
			defer wg.Done()
			for {
				w := s.Enqueue(key)
				select {
				case <-w.Granted():
					if w.Err() != nil {
						return
					}
					mu.Lock()
					bySubject[key.Subject]++
					mu.Unlock()
					s.Release(w)
				case <-ctx.Done():
					s.Abandon(w)
					return
				}
				if ctx.Err() != nil {
					return
				}
			}
		}

		// The greedy subject keeps every one of its ten sessions busy.
		for _, k := range greedy {
			wg.Add(1)
			go worker(k)
		}
		wg.Add(1)
		go worker(honest)

		time.Sleep(300 * testInterval)
		cancel()
		wg.Wait()

		mu.Lock()
		gotGreedy, gotHonest := bySubject["greedy-subject"], bySubject["honest-subject"]
		mu.Unlock()

		total := gotGreedy + gotHonest
		if total == 0 {
			t.Fatal("no requests were admitted")
		}
		share := float64(gotHonest) / float64(total)
		if share < 0.40 || share > 0.60 {
			t.Errorf("the single-session subject got %d of %d admissions (%.1f%%), want ~50%%: "+
				"a subject is multiplying its share by opening sessions (greedy=%d across %d "+
				"sessions, honest=%d across 1). Flat keying on (subject, session) would give "+
				"the honest caller ~%.0f%%",
				gotHonest, total, share*100, gotGreedy, greedySessions, gotHonest,
				100.0/float64(greedySessions+1))
		}
	})
}

// The same must hold for in-flight slots: a subject with many sessions must not
// hold many times its share of the concurrency cap.
//
// The ceiling and the last-slot reservation are therefore enforced per SUBJECT,
// summing across its sessions. Enforced per session they would multiply exactly
// like the pace would.
func TestScheduler_ExtraSessionsDoNotEnlargeASubjectsInFlightShare(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const limit = 8
		s := newTestScheduler(t, limit)

		honest := CallerKey{Subject: "honest", Session: "only"}
		// Make the honest subject active first so the ceiling is computed
		// against two subjects from the start.
		honestHeld := grant(t, s, honest)

		// The greedy subject now tries to take everything, spreading its
		// requests over many sessions.
		var greedyHeld []*Waiter
		for i := range limit * 3 {
			key := CallerKey{Subject: "greedy", Session: string(rune('a' + i))}
			w, ok := tryGrant(s, key)
			if !ok {
				s.Abandon(w)
				continue
			}
			greedyHeld = append(greedyHeld, w)
		}

		ceiling := (limit + 1) / 2 // ceil(8/2) with two active subjects
		if len(greedyHeld) > ceiling {
			t.Errorf("the greedy subject holds %d of %d slots across %d sessions, above its "+
				"per-subject ceiling of %d: sessions are multiplying its in-flight share",
				len(greedyHeld), limit, len(greedyHeld), ceiling)
		}

		s.Release(honestHeld)
		for _, w := range greedyHeld {
			s.Release(w)
		}
	})
}

// Distinct subjects are distinct buckets, and an empty subject or session is a
// legitimate shared bucket rather than an error — that is what every non-oauth
// mode produces, and it needs no special-casing.
func TestScheduler_EmptyKeyFieldsCollapseToASharedBucket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestScheduler(t, 4)

		// The zero key is what a context with no stamped identity yields.
		anon := CallerKey{}
		w1 := grant(t, s, anon)
		w2 := grant(t, s, anon)

		_, callers, _ := s.stats()
		if callers != 1 {
			t.Errorf("two requests with the zero caller key produced %d buckets, want 1", callers)
		}

		s.Release(w1)
		s.Release(w2)
	})
}

// The context plumbing is the only route a caller key takes from the tools
// layer to the Sender, so its round trip and its absent case both need pinning.
func TestCallerKeyContextRoundTrip(t *testing.T) {
	want := CallerKey{Subject: "sub-123", Session: "sess-abc"}
	got, stamped := CallerKeyFrom(WithCallerKey(context.Background(), want))
	if !stamped {
		t.Error("a stamped context reported no caller key")
	}
	if got != want {
		t.Errorf("CallerKeyFrom round trip = %+v, want %+v", got, want)
	}

	// An empty key is legitimate — it is what every non-oauth mode produces —
	// so it must still report as stamped. Only that distinction lets a test
	// catch a future call path that bypasses the stamping site entirely.
	empty, stamped := CallerKeyFrom(WithCallerKey(context.Background(), CallerKey{}))
	if !stamped {
		t.Error("an explicitly stamped empty key reported as unstamped; that collapses the " +
			"legitimate anonymous bucket and a missing stamp into the same observation")
	}
	if empty != (CallerKey{}) {
		t.Errorf("stamped empty key = %+v, want the zero key", empty)
	}

	bare, stamped := CallerKeyFrom(context.Background())
	if stamped {
		t.Error("an unstamped context reported a caller key")
	}
	if bare != (CallerKey{}) {
		t.Errorf("CallerKeyFrom on an unstamped context = %+v, want the zero key", bare)
	}
}

// The per-caller ceiling arithmetic must never believe there is more capacity
// than the semaphore actually has.
//
// grantOneLocked sends to the semaphore while holding the scheduler mutex, and
// that send is non-blocking only because eligibility already proved a slot is
// free. A limit above cap(sem) would break that proof and the send would block
// with the mutex held, wedging the dispatcher and every waiter behind it.
//
// The path is reachable by accident, not just by a bad argument: NewSemaphore
// substitutes a default capacity when asked for n < 1, so a hand-built config
// can produce a semaphore whose capacity does not match the number the
// scheduler was handed.
func TestNewScheduler_ClampsLimitToSemaphoreCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := NewRateLimiter(testInterval)
		defer limiter.Stop()

		sem := NewSemaphore(2)
		s := NewScheduler(sem, limiter, 99) // absurdly above cap(sem)
		defer s.Stop()

		if s.limit != cap(sem) {
			t.Fatalf("limit = %d, want it clamped to cap(sem) = %d", s.limit, cap(sem))
		}

		// Prove the dispatcher survives: fill the semaphore, then confirm a
		// further request is refused rather than wedging the scheduler.
		key := CallerKey{Subject: "solo"}
		held := []*Waiter{grant(t, s, key), grant(t, s, key)}

		if w, ok := tryGrant(s, key); ok {
			t.Error("a request was admitted past the semaphore's real capacity")
			s.Release(w)
		} else {
			s.Abandon(w)
		}

		// The scheduler is still live, which is the property that would be lost
		// to a mutex-held blocking send.
		s.Release(held[0])
		next := grant(t, s, key)
		s.Release(next)
		s.Release(held[1])
	})
}

// A semaphore built from a non-positive cap falls back to a default capacity,
// so the scheduler's limit must follow the semaphore rather than the argument.
func TestNewScheduler_TracksSemaphoreDefaultCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := NewRateLimiter(testInterval)
		defer limiter.Stop()

		sem := NewSemaphore(0) // substitutes DefaultMaxConcurrentPerBroker
		s := NewScheduler(sem, limiter, 0)
		defer s.Stop()

		if s.limit != cap(sem) {
			t.Errorf("limit = %d, want cap(sem) = %d", s.limit, cap(sem))
		}
	})
}

// CallerKey must never reach the log stream as a raw struct. Nothing logs one
// today; the LogValuer exists so the first line that does is safe by
// construction, and this pins what it emits.
func TestCallerKeyLogValueRedactsTheSessionID(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	logger.Info("probe", slog.Any("caller", CallerKey{
		Subject: "auth0|abc",
		Session: "0199-secret-session-handle",
	}))
	out := buf.String()

	if strings.Contains(out, "0199-secret-session-handle") {
		t.Errorf("the raw MCP session ID reached the log stream: %s", out)
	}
	if !strings.Contains(out, "auth0|abc") {
		t.Errorf("the subject is part of the audit schema and should be logged: %s", out)
	}
	if !strings.Contains(out, "<present>") {
		t.Errorf("a session ID that exists should be reported as present: %s", out)
	}

	buf.Reset()
	logger.Info("probe", slog.Any("caller", CallerKey{Subject: "s"}))
	if !strings.Contains(buf.String(), "<absent>") {
		t.Errorf("a missing session ID should be reported as absent: %s", buf.String())
	}
}

// A double release must not corrupt the accounting. It should never happen —
// Sender.Do releases exactly once — but driving the in-flight counter negative
// would silently relax the cap for every other caller, so the guard is worth
// pinning.
func TestScheduler_DoubleReleaseIsIgnored(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestScheduler(t, 2)
		key := CallerKey{Subject: "a"}

		w := grant(t, s, key)
		s.Release(w)
		s.Release(w)
		synctest.Wait()

		s.mu.Lock()
		inFlight := s.inFlight
		s.mu.Unlock()
		if inFlight != 0 {
			t.Errorf("in-flight accounting is %d after a double release, want 0", inFlight)
		}
		if got := len(s.sem); got != 0 {
			t.Errorf("semaphore occupancy is %d after a double release, want 0", got)
		}
	})
}

// A second release of ONE waiter must not free a DIFFERENT request's slot.
//
// This is the case the resolved flag exists for, and the one-slot double-release
// test above cannot show: releaseLocked decrements the caller's in-flight count,
// not the waiter's, so when a caller holds several slots an extra release on one
// waiter silently frees another of its live requests' slots. The caller then
// holds more real in-flight work than the scheduler believes, which relaxes its
// ceiling and the last-slot reservation for everyone else.
func TestScheduler_ExtraReleaseDoesNotFreeAnotherRequestsSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestScheduler(t, 4)
		key := CallerKey{Subject: "multi"}

		first := grant(t, s, key)
		second := grant(t, s, key)
		if got := len(s.sem); got != 2 {
			t.Fatalf("test setup: in-flight is %d, want 2", got)
		}

		// Release the first waiter twice. The second call must be ignored, not
		// charged against the still-live second request.
		s.Release(first)
		s.Release(first)
		synctest.Wait()

		_, _, inFlight := s.stats()
		callerInFlight := s.sessionInFlight(key)

		if inFlight != 1 || callerInFlight != 1 {
			t.Errorf("after releasing one of two held slots twice: scheduler in-flight = %d, "+
				"caller in-flight = %d, want 1 and 1 — the extra release freed the other "+
				"request's slot", inFlight, callerInFlight)
		}
		if got := len(s.sem); got != 1 {
			t.Errorf("semaphore occupancy = %d, want 1", got)
		}

		s.Release(second)
	})
}

// Releasing a waiter that was never granted must not free a live sibling
// request's slot.
//
// The resolved flag alone does not stop this: releaseLocked decrements by KEY,
// so a still-queued waiter passed to Release charges its session's counter and
// hands back a slot belonging to one of that session's in-flight requests. The
// caller then holds more real work than the scheduler believes, which relaxes
// its ceiling and the last-slot reservation for everyone else.
//
// Not reachable from Sender.Do, which resolves each waiter exactly once and
// only on the granted path — this pins the contract so a future caller cannot
// break it silently.
func TestScheduler_ReleasingANeverGrantedWaiterIsIgnored(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const limit = 1
		s := newTestScheduler(t, limit)
		key := CallerKey{Subject: "a"}

		live := grant(t, s, key)

		// A second request for the same session cannot be admitted: the only
		// slot is held. It stays queued.
		queued := s.Enqueue(key)
		synctest.Wait()
		select {
		case <-queued.Granted():
			t.Fatal("test setup: the second request was admitted past the cap of 1")
		default:
		}

		// Releasing the queued waiter must do nothing at all.
		s.Release(queued)
		synctest.Wait()

		_, _, inFlight := s.stats()
		if inFlight != 1 {
			t.Errorf("scheduler in-flight = %d after releasing a never-granted waiter, want 1: "+
				"the live request's slot was handed back under it", inFlight)
		}
		if got := len(s.sem); got != 1 {
			t.Errorf("semaphore occupancy = %d, want 1", got)
		}

		s.Abandon(queued)
		s.Release(live)
	})
}

// Release after Abandon on the same waiter must be a no-op, for the same reason
// as the test above: both settle the same waiter, so the second one would be
// charged against a different live request of that caller.
func TestScheduler_ReleaseAfterAbandonIsIgnored(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestScheduler(t, 4)
		key := CallerKey{Subject: "multi"}

		first := grant(t, s, key)
		second := grant(t, s, key)

		s.Abandon(first) // granted, so this hands the slot back
		s.Release(first) // must be ignored
		synctest.Wait()

		s.mu.Lock()
		inFlight := s.inFlight
		s.mu.Unlock()
		if inFlight != 1 {
			t.Errorf("scheduler in-flight = %d after Abandon then Release on one waiter, "+
				"want 1 (the other request is still live)", inFlight)
		}

		s.Release(second)
	})
}

// Abandon must be safe to call twice: Sender.admit calls it on exactly one
// path, but a second call must not double-release a slot and hand another
// caller phantom capacity.
func TestScheduler_DoubleAbandonIsIgnored(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestScheduler(t, 2)
		key := CallerKey{Subject: "a"}

		w := grant(t, s, key)
		s.Abandon(w)
		s.Abandon(w)
		synctest.Wait()

		s.mu.Lock()
		inFlight := s.inFlight
		s.mu.Unlock()
		if inFlight != 0 {
			t.Errorf("in-flight accounting is %d after a double abandon, want 0", inFlight)
		}
	})
}

// The stage a shed reports must name the gate that is actually the constraint,
// because it is what points an operator at either semp.request_min_interval or
// semp.max_concurrent_per_broker.
func TestScheduler_StageNamesTheBindingGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const limit = 1
		s := newTestScheduler(t, limit)

		// Nothing in flight: a fresh waiter is held up by the pace alone.
		fresh := s.Enqueue(CallerKey{Subject: "fresh"})
		if got := s.Stage(fresh); got != AdmissionStageRateLimit {
			t.Errorf("stage for a waiter with slots free = %q, want %q", got, AdmissionStageRateLimit)
		}
		<-fresh.Granted()

		// Now the only slot is held and a second caller is active, so the
		// binding constraint is concurrency.
		blocked := s.Enqueue(CallerKey{Subject: "blocked"})
		time.Sleep(3 * testInterval)
		synctest.Wait()
		if got := s.Stage(blocked); got != AdmissionStageConcurrency {
			t.Errorf("stage for a waiter blocked on the in-flight cap = %q, want %q",
				got, AdmissionStageConcurrency)
		}

		s.Abandon(blocked)
		s.Release(fresh)
	})
}
