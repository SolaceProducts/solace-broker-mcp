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
	"log/slog"
	"sync"
	"time"
)

// ErrSchedulerStopped is returned to a waiter that was still queued when the
// broker's Scheduler was stopped. It is deliberately distinct from
// context.Canceled: the caller's context is fine, the server is shutting the
// broker client down underneath it.
//
// Sender.admitScheduled does not surface this to callers directly — it converts
// it to a *BrokerBusyError, because from the caller's point of view a drained
// request has exactly the properties that type describes: it was never sent, so
// it is safe to repeat, and the right response is to retry. On the two-replica
// deployment this project ships, that retry lands on a pod that is not
// draining.
var ErrSchedulerStopped = errors.New("broker admission scheduler stopped")

// CallerKey identifies the caller a request is charged to for fair scheduling.
//
// Two levels, rotated independently — see Scheduler for the rotation itself.
// Subject is the outer level and receives an equal share of the broker's pace;
// Session divides that subject's own share among its sessions. Keying on
// Subject alone would collapse to a single bucket under an OAuth
// client_credentials grant, where the subject is a service account shared by
// every session an agent platform fronts, which is the deployment shape that
// ships. Keying on the pair as one flat identity would do the opposite: a
// caller could multiply its share by opening sessions.
//
// Both fields are opaque and may be empty. Empty Subject is the norm outside
// mcp_client_auth.mode: oauth, and empty Session is what a transport with no
// session ID yields; each empty value simply collapses that level to one shared
// bucket, which needs no special-casing anywhere in the scheduler.
type CallerKey struct {
	// Subject is the raw OIDC sub, taken from sdkauth.TokenInfo.UserID.
	//
	// Deliberately NOT tools.Identity's sub: that one is the audit-log
	// projection, truncated to 256 bytes by sanitize.Claim and collapsed to an
	// "<absent>" sentinel when empty. Two distinct subjects sharing a 256-byte
	// prefix would merge into one fairness bucket.
	Subject string

	// Session is the MCP session ID (mcp.ServerSession.ID()).
	Session string
}

// LogValue implements slog.LogValuer so a CallerKey can never reach the log
// stream as a raw struct.
//
// Nothing logs a CallerKey today, and that is the point: this exists so the
// first line that does is safe by construction rather than by review. The
// subject is emitted because it is already part of the audit schema
// (internal/tools/identity.go); the session ID is reported only as present or
// absent, because it is the `Mcp-Session-Id` routing handle and there is no
// diagnostic question here that its value answers.
//
// Note the subject is emitted RAW, unlike the audit schema's, which truncates
// at 256 bytes. That is deliberate for the key itself — truncating would merge
// distinct callers into one fairness bucket — but it means anything logging
// this is emitting an untruncated claim. Prefer logging Identity where an
// audit-shaped record is what you want.
func (k CallerKey) LogValue() slog.Value {
	session := "<absent>"
	if k.Session != "" {
		session = "<present>"
	}
	return slog.GroupValue(
		slog.String("sub", k.Subject),
		slog.String("session", session),
	)
}

// callerKeyContextKey is the context key under which the tools layer stamps the
// caller key. Unexported with an unexported type so nothing outside this
// package can collide with it.
type callerKeyContextKey struct{}

// WithCallerKey returns a context carrying the caller a request is charged to.
//
// Stamped once, in the tools/call closure in internal/tools/register.go, which
// is the single point every broker-reaching request passes through. Delivered
// by context rather than threaded as a parameter because the alternative would
// touch every native tool, the composite executor, and both protocol clients to
// carry one value that only the Sender reads — and context-carried request
// state reaching the Sender is already the established pattern here
// (OperationIDKey, retryStateKey, WithRetrySafe/WithRetryUnsafe).
func WithCallerKey(ctx context.Context, key CallerKey) context.Context {
	return context.WithValue(ctx, callerKeyContextKey{}, key)
}

// CallerKeyFrom reads the caller key stamped by WithCallerKey, reporting
// whether one was stamped at all.
//
// The boolean matters because an empty key is legitimate and common — every
// non-oauth mode produces an empty subject, and a transport with no session ID
// produces an empty session — so the zero CallerKey cannot itself distinguish
// "this caller has no identity" from "nothing stamped a key". Only the second
// is a defect: it would mean a broker-bound request reached the Sender by a
// path that bypasses the tools/call closure, and every such request would then
// share one bucket regardless of who sent it.
//
// Unstamped is handled rather than rejected: it still resolves to the shared
// bucket, which is the safe reading. Sender.admitScheduled logs a WARN so the
// condition is visible in production rather than only in a test.
func CallerKeyFrom(ctx context.Context) (key CallerKey, stamped bool) {
	k, ok := ctx.Value(callerKeyContextKey{}).(CallerKey)
	return k, ok
}

// Waiter is one request's place in the admission queue. Obtained from
// Scheduler.Enqueue, resolved by exactly one of Granted firing or Abandon being
// called.
type Waiter struct {
	key CallerKey

	// ready is closed exactly once, by the scheduler, when this waiter is
	// either granted admission or released by Stop. Closing rather than sending
	// removes the whole class of handoff races the design flagged as its main
	// risk: there is no send that can block, no grant that can be lost to a
	// departed waiter, and no need to recycle a failed handoff.
	ready chan struct{}

	// granted and err are written by the scheduler under Scheduler.mu before
	// ready is closed, and read by the waiter's own goroutine only after ready
	// fires. The close/read pair is the happens-before edge, so no separate
	// synchronization is needed for them.
	granted bool
	err     error

	// resolved records that this waiter's slot accounting is settled, by either
	// Abandon or Release. One flag for both because they are the same event
	// from the accounting's point of view: this waiter will never hold a slot
	// again.
	//
	// Sender.Do calls exactly one of them exactly once, so this is not load
	// bearing today. It is here because the failure mode if a future caller
	// gets it wrong is silent and bad: releaseLocked decrements the SESSION's
	// in-flight count, not this waiter's, so a second release on a session that
	// holds several slots frees one of its other requests' slots instead of
	// erroring, handing that caller phantom capacity and breaking the ceiling
	// for everyone else.
	resolved bool
}

// Granted returns a channel that fires when this waiter has been resolved. The
// caller MUST then check Err: the channel also fires when the scheduler is
// stopped, in which case no slot is held.
func (w *Waiter) Granted() <-chan struct{} { return w.ready }

// Err reports why the waiter was resolved. nil means admission was granted and
// an in-flight slot is held on this waiter's behalf; the caller owns releasing
// it via Scheduler.Release. Only valid to read after Granted has fired.
func (w *Waiter) Err() error { return w.err }

// rotor is a round-robin ring of string keys with a cursor. Both levels of the
// scheduler's rotation use one, so the insertion and removal rules that make
// the rotation fair live in exactly one place.
//
// All methods must be called with Scheduler.mu held.
type rotor struct {
	keys   []string
	cursor int
}

func (r *rotor) len() int { return len(r.keys) }

// at returns the key `offset` positions ahead of the cursor.
func (r *rotor) at(offset int) string { return r.keys[(r.cursor+offset)%len(r.keys)] }

// advancePast moves the cursor just past the entry at `offset`, so the next
// scan begins with the following one.
//
// Fairness is in this method. Without it a scan would restart at the same head
// every time and the caller there would take every tick.
func (r *rotor) advancePast(offset int) {
	r.cursor = (r.cursor + offset + 1) % len(r.keys)
}

// insert adds a newly active key immediately behind the cursor, so it is served
// last in the current rotation.
//
// The position is load-bearing. A caller that issues one request at a time
// empties its subqueue after every grant and is removed, so it re-enters
// constantly. Appending it wherever the slice happens to end, or letting map
// order decide, would systematically over- or under-serve it against a caller
// holding a deep standing queue — the opposite of the guarantee. Putting it
// behind the cursor gives it exactly one turn per lap, the same as everyone
// else.
func (r *rotor) insert(key string) {
	if r.cursor > len(r.keys) {
		r.cursor = len(r.keys)
	}
	r.keys = append(r.keys, "")
	copy(r.keys[r.cursor+1:], r.keys[r.cursor:])
	r.keys[r.cursor] = key
	r.cursor++
	if r.cursor >= len(r.keys) {
		r.cursor = 0
	}
}

// remove drops a key, keeping the cursor pointing at the same logical next
// entry.
func (r *rotor) remove(key string) {
	for i, k := range r.keys {
		if k != key {
			continue
		}
		r.keys = append(r.keys[:i], r.keys[i+1:]...)
		if i < r.cursor {
			r.cursor--
		}
		if len(r.keys) == 0 || r.cursor >= len(r.keys) {
			r.cursor = 0
		}
		return
	}
}

// sessionState is one MCP session's queue and slot holding.
type sessionState struct {
	queue    []*Waiter
	inFlight int
}

// active reports whether this session still needs to exist. Holding a slot
// counts even with an empty queue.
func (c *sessionState) active() bool { return len(c.queue) > 0 || c.inFlight > 0 }

// subjectState is one OIDC subject's sessions, rotated among themselves, plus
// the subject's total in-flight count.
//
// inFlight is summed at the subject level because the in-flight ceiling and the
// last-slot reservation are enforced per SUBJECT, not per session. Enforcing
// them per session would let a subject with many sessions hold many times the
// ceiling, which is the same multiplication the pace rotation exists to
// prevent.
type subjectState struct {
	sessions map[string]*sessionState
	rotor    rotor
	inFlight int
}

func (s *subjectState) hasQueued() bool {
	for _, cs := range s.sessions {
		if len(cs.queue) > 0 {
			return true
		}
	}
	return false
}

// active reports whether this subject still needs to exist.
func (s *subjectState) active() bool { return len(s.sessions) > 0 }

// hasStarvedSession reports whether some session other than except has work
// queued and holds no slots — i.e. whether the intra-subject last-slot
// reservation has a beneficiary. Called with Scheduler.mu held.
func (s *subjectState) hasStarvedSession(except *sessionState) bool {
	for _, cs := range s.sessions {
		if cs == except {
			continue
		}
		if cs.inFlight == 0 && len(cs.queue) > 0 {
			return true
		}
	}
	return false
}

// Scheduler is a per-broker admission scheduler that shares the broker's
// request pace fairly across callers.
//
// # What it replaces
//
// Without it, both admission gates in Sender.Do are plain Go channel
// operations, and Go's channel wait queues are FIFO. A single caller issuing a
// burst therefore puts every later caller behind its entire backlog. This is
// not literal starvation — the queue does drain — but the head-of-line delay is
// unbounded, and it needs no hostile client to happen: one legitimate list-*
// call fans out one SEMP request per row (internal/composite/executor.go), so
// a large VPN enqueues hundreds of requests and a concurrent status check waits
// behind all of them.
//
// # Two levels of rotation
//
// The outer rotation is over OIDC subjects and the inner one is over each
// subject's MCP sessions. A subject gets one turn per outer lap however many
// sessions it holds, and its sessions then take turns within it. That is what
// makes the share ungameable: opening more sessions subdivides a subject's own
// turn rather than buying extra turns.
//
// A single flat rotation over (subject, session) pairs would be simpler and
// wrong — a caller with N sessions would take N/(N+1) of the pace against a
// single-session caller.
//
// In mcp_client_auth.mode: static every caller shares the subject "dev-user",
// and in mode: disabled the subject is empty, so the outer level degenerates to
// one bucket and the session level does all the work. That needs no branch.
//
// # The two resources are not the same shape
//
// A pacer tick is a point cost: one tick admits exactly one request, so the
// quantum is uniform and round-robin over it is genuinely fair with no deficit
// accounting. An in-flight slot is a duration cost, held for a request's entire
// retry chain — up to roughly 16 minutes at default settings. Charging both
// against one quantum would score a caller running 30-second requests and a
// caller running 30-millisecond requests as equally served.
//
// So the pace is the fair-shared resource, and the in-flight cap is a separate
// safety gate with no fairness claim attached: a per-SUBJECT ceiling of
// max(1, ceil(limit/activeSubjects)) checked when a grant is about to be made,
// never revoked retroactively.
//
// # Work-conserving
//
// One active subject gets the whole configured rate and the whole in-flight
// cap. Fairness reslices the existing budget; it never lowers the broker's
// aggregate ceilings, which remain exactly semp.request_min_interval and
// semp.max_concurrent_per_broker.
//
// # Ownership
//
// The scheduler is the sole owner of the semaphore while it is installed: it
// performs both the send and the receive. If Sender.Do also sent to the
// semaphore, that send could block and channel FIFO would be back at the second
// gate, which is the thing this removes. Sharing the same Semaphore value
// rather than replacing it keeps semp.BrokerClient.InFlight (len/cap over that
// channel) reporting real occupancy.
type Scheduler struct {
	mu sync.Mutex

	// subjects holds per-subject state for subjects that are currently active.
	//
	// Lifecycle is create-on-enqueue, delete-when-idle at both levels: a
	// session exists only while it has queued waiters or holds slots, and a
	// subject only while it has sessions. So len(subjects) IS the active-subject
	// count and no TTL, sweeper, or LRU is needed. This works because the
	// fair-shared resource has a uniform quantum and carries no deficit across
	// an empty queue — forgetting a caller neither costs it anything nor grants
	// it anything.
	subjects map[string]*subjectState

	// rotor is the round-robin order over active subjects.
	rotor rotor

	// credit is the single held pace tick. A tick that arrives when no caller
	// can be served is held here rather than dropped (dropping it would push
	// aggregate throughput below the configured rate) and rather than counted
	// (accumulating credit would let a burst exceed it). At most one, which
	// matches RateLimiter's own documented "idle time is credit, capped at one
	// request".
	credit bool

	sem      Semaphore
	limit    int  // semp.max_concurrent_per_broker, clamped to cap(sem)
	inFlight int  // total slots held; mirrors len(sem), maintained under mu
	paced    bool // false when semp.request_min_interval is 0 (throttling off)

	ticks <-chan time.Time

	// wake carries dispatch requests from Enqueue and Release to the
	// dispatcher. Buffered at 1 and sent to non-blockingly: a pending wake
	// already means "re-evaluate", so coalescing is correct and never drops a
	// state change.
	wake chan struct{}

	stopOnce sync.Once
	stopped  chan struct{} // closed by Stop
	done     chan struct{} // closed by the dispatcher on exit
}

// NewScheduler builds a per-broker admission scheduler over the broker's shared
// semaphore and rate limiter.
//
// sem and limiter must be the same instances the broker's Senders were built
// with; the scheduler takes over both. limit is
// semp.max_concurrent_per_broker, used for the per-subject ceiling.
//
// A limiter with throttling disabled (semp.request_min_interval: 0) yields a
// CLOSED channel, so the scheduler must not select on it: a dispatcher looping
// on a closed channel would spin a core per broker forever. Detected here via
// RateLimiter.Enabled, after which admission is gated on slots alone and the
// dispatcher runs purely on enqueue and release events. Note that with no pace
// to share there is no pace fairness either — only the in-flight ceiling
// remains. That is documented as a limit in docs/configuration.md.
//
// The caller owns Stop, which must run for the dispatcher goroutine to exit.
// semp.BrokerClient.Close is that owner in production.
func NewScheduler(sem Semaphore, limiter *RateLimiter, limit int) *Scheduler {
	if sem == nil {
		panic("resilience.NewScheduler: sem must be non-nil")
	}
	if limiter == nil {
		panic("resilience.NewScheduler: limiter must be non-nil")
	}
	// Clamp to the semaphore's actual capacity rather than trusting the caller.
	// This is a deadlock guard, not tidiness: grantOneLocked sends to sem while
	// holding s.mu, and that send is non-blocking ONLY because eligibility
	// already proved a slot is free. A limit above cap(sem) would let
	// eligibility report free capacity the channel does not have, and the send
	// would block with the mutex held, wedging the dispatcher and every waiter
	// behind it for the life of the process.
	//
	// Reachable without the clamp: NewSemaphore silently substitutes
	// DefaultMaxConcurrentPerBroker when asked for n < 1, so a hand-built
	// config with MaxConcurrentPerBroker of 0 yields cap(sem) == 10 while
	// passing limit == 0 here. That case lands on the same branch, but the
	// clamp is what makes the invariant hold for any argument.
	if limit < 1 || limit > cap(sem) {
		limit = cap(sem)
	}
	s := &Scheduler{
		subjects: make(map[string]*subjectState),
		sem:      sem,
		limit:    limit,
		paced:    limiter.Enabled(),
		wake:     make(chan struct{}, 1),
		stopped:  make(chan struct{}),
		done:     make(chan struct{}),
	}
	if s.paced {
		s.ticks = limiter.C()
	}
	go s.dispatch()
	return s
}

// Enqueue puts a request into its session's subqueue and returns its Waiter.
// Never blocks. The caller must resolve the Waiter exactly once, by observing
// Granted or by calling Abandon.
func (s *Scheduler) Enqueue(key CallerKey) *Waiter {
	w := &Waiter{key: key, ready: make(chan struct{})}

	s.mu.Lock()
	if s.isStopped() {
		s.mu.Unlock()
		w.err = ErrSchedulerStopped
		close(w.ready)
		return w
	}

	subj, ok := s.subjects[key.Subject]
	if !ok {
		subj = &subjectState{sessions: make(map[string]*sessionState)}
		s.subjects[key.Subject] = subj
		s.rotor.insert(key.Subject)
	}
	cs, ok := subj.sessions[key.Session]
	if !ok {
		cs = &sessionState{}
		subj.sessions[key.Session] = cs
		subj.rotor.insert(key.Session)
	}
	cs.queue = append(cs.queue, w)
	s.mu.Unlock()

	s.signal()
	return w
}

// Abandon withdraws a waiter that is giving up — its context ended, or the
// admission budget expired. Safe to call after the waiter was granted: that is
// the ctx-cancel-versus-grant race, and it resolves by releasing the slot the
// grant reserved rather than leaking it.
func (s *Scheduler) Abandon(w *Waiter) {
	s.mu.Lock()
	if w.resolved {
		s.mu.Unlock()
		return
	}
	w.resolved = true

	if w.granted {
		// Lost the race: the dispatcher already reserved a slot for this
		// waiter. Hand it straight back, or it is leaked for the life of the
		// process.
		s.releaseLocked(w.key)
		s.mu.Unlock()
		s.signal()
		return
	}

	s.removeFromQueueLocked(w)
	s.mu.Unlock()
}

// Release returns the in-flight slot held on behalf of a granted waiter. Must
// be called exactly once for every waiter whose Err was nil. Extra calls, and a
// call after Abandon, are ignored rather than double-counted — see Waiter's
// resolved field for why that guard is not merely defensive.
// The !granted half of the guard matters as much as the resolved half.
// releaseLocked decrements by KEY, not by waiter, so releasing a waiter that
// never held a slot would hand back one of its session's live requests' slots
// instead of erroring — the caller gets phantom capacity and the ceiling breaks
// for everyone else. Not reachable from Sender.Do, which resolves each waiter
// exactly once on the granted path only, so this is contract hardening.
func (s *Scheduler) Release(w *Waiter) {
	s.mu.Lock()
	if w.resolved || !w.granted {
		s.mu.Unlock()
		return
	}
	w.resolved = true
	s.releaseLocked(w.key)
	s.mu.Unlock()
	s.signal()
}

// Stage reports which constraint is holding this waiter up right now, in
// BrokerBusyError.Stage's vocabulary.
//
// Derived from live state on every read rather than recorded as the dispatcher
// passes waiters over. A recorded value latches: a broker that is briefly
// slot-saturated would mark every waiting request "concurrency" and they would
// keep reporting it after the pace became the real constraint again, which
// points the operator at max_concurrent_per_broker when the knob that matters
// is request_min_interval.
func (s *Scheduler) Stage(w *Waiter) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	subj := s.subjects[w.key.Subject]
	if subj != nil && !s.eligibleLocked(subj) {
		return AdmissionStageConcurrency
	}
	return AdmissionStageRateLimit
}

// Stop shuts the dispatcher down and releases every queued waiter with
// ErrSchedulerStopped. Idempotent, and safe to call on a scheduler whose
// dispatcher has already exited.
//
// Releasing parked waiters matters: a stopped ticker does not close its
// channel, so without this a waiter would block until its own context ended,
// which for a caller that set no deadline is never.
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() { close(s.stopped) })
	<-s.done
}

// isStopped reports whether Stop has run. Called with mu held.
func (s *Scheduler) isStopped() bool {
	select {
	case <-s.stopped:
		return true
	default:
		return false
	}
}

// signal nudges the dispatcher to re-evaluate. Non-blocking: a wake already
// queued means the dispatcher has not yet looked at the current state, so it
// will see this change too.
func (s *Scheduler) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// dispatch is the scheduler's single goroutine. It wakes on a pace tick, on an
// enqueue, or on a release, and it is deliberately all three rather than the
// tick alone.
//
// Waking on release is not an optimization. Before this scheduler, a request
// that had already taken its tick parked directly on the semaphore and resumed
// the instant a slot freed. A tick-only dispatcher would lose that: a caller
// held up purely by the slot ceiling would wait for the next tick after a slot
// came free, which at semp.request_min_interval is a regression against the
// behavior this replaces.
func (s *Scheduler) dispatch() {
	defer close(s.done)
	for {
		select {
		case <-s.stopped:
			s.drain()
			return
		case <-s.ticks:
			s.mu.Lock()
			s.credit = true
			s.dispatchLocked()
			s.mu.Unlock()
		case <-s.wake:
			s.mu.Lock()
			s.dispatchLocked()
			s.mu.Unlock()
		}
	}
}

// dispatchLocked grants as much admission as the current budget allows. Called
// with mu held.
//
// When pacing is on, one held tick admits exactly one request, so at most one
// grant is made per pass. When pacing is off there is no per-tick budget at
// all, and the loop grants until the slot rules stop it.
func (s *Scheduler) dispatchLocked() {
	if !s.paced {
		for s.grantOneLocked() {
		}
		return
	}
	if !s.credit {
		return
	}
	if s.grantOneLocked() {
		s.credit = false
	}
}

// grantOneLocked admits a single request: it picks the next eligible subject by
// round-robin, then the next queued session within that subject, also by
// round-robin. Reports whether a grant was made. Called with mu held.
//
// Skipping an ineligible subject rather than stalling on it is what keeps
// aggregate throughput at the configured rate: a held tick spent on nobody
// would push the broker below semp.request_min_interval, which the ticket
// requires to still hold. A skipped subject keeps its rotor position, so the
// rotation order is undisturbed — it is passed over for this tick, not moved to
// the back.
//
// When no subject at all can be served, the tick stays in s.credit and this
// pass makes no grant; the next release or enqueue re-runs the scan with that
// credit still in hand.
func (s *Scheduler) grantOneLocked() bool {
	n := s.rotor.len()
	if n == 0 {
		return false
	}

	for i := range n {
		subjKey := s.rotor.at(i)
		subj := s.subjects[subjKey]
		if subj == nil || !subj.hasQueued() {
			continue
		}
		if !s.eligibleLocked(subj) {
			continue
		}

		w := s.dequeueLocked(subj)
		if w == nil {
			// hasQueued said otherwise, so this cannot happen; treat it as a
			// non-grant rather than a panic.
			continue
		}

		w.granted = true
		s.sem <- struct{}{}
		s.inFlight++
		subj.inFlight++
		subj.sessions[w.key.Session].inFlight++

		s.rotor.advancePast(i)
		s.pruneSessionLocked(subj, w.key)
		close(w.ready)
		return true
	}
	return false
}

// dequeueLocked takes the next waiter from this subject's session rotation,
// advancing that subject's own cursor so its sessions take turns. Sessions that
// have reached their own slot sub-ceiling are skipped, keeping their rotor
// position. Returns nil when no session of this subject may be served, which
// leaves the subject's turn to the next candidate. Called with mu held.
func (s *Scheduler) dequeueLocked(subj *subjectState) *Waiter {
	n := subj.rotor.len()
	for i := range n {
		sessKey := subj.rotor.at(i)
		cs := subj.sessions[sessKey]
		if cs == nil || len(cs.queue) == 0 {
			continue
		}
		if !s.sessionEligibleLocked(subj, cs) {
			continue
		}
		w := cs.queue[0]
		cs.queue = cs.queue[1:]
		subj.rotor.advancePast(i)
		return w
	}
	return nil
}

// subjectCeilingLocked is the in-flight allowance for one subject.
//
// Recomputed on every check rather than stored, because the active-subject
// count moves as callers arrive and go idle, and a stored ceiling would be
// stale in exactly the moment a new caller shows up.
func (s *Scheduler) subjectCeilingLocked() int {
	active := len(s.subjects)
	if active < 1 {
		active = 1
	}
	ceiling := (s.limit + active - 1) / active
	if ceiling < 1 {
		ceiling = 1
	}
	return ceiling
}

// eligibleLocked reports whether this SUBJECT may take a slot right now. Called
// with mu held.
//
// The subject level is the outer bound, sized against the number of active
// subjects, so a subject cannot multiply its in-flight share across sessions
// the way a per-session-only ceiling would let it. sessionEligibleLocked then
// divides that allowance among the subject's own sessions.
func (s *Scheduler) eligibleLocked(subj *subjectState) bool {
	free := s.limit - s.inFlight
	if free <= 0 {
		return false
	}
	if subj.inFlight >= s.subjectCeilingLocked() {
		return false
	}

	// Last-slot reservation.
	//
	// The final free slot is not given to a subject that already holds one
	// while some other subject has work queued and holds nothing. Without it, a
	// caller that arrives into a saturated broker waits behind a whole retry
	// chain — up to roughly 16 minutes at defaults — because every slot a peer
	// releases is immediately retaken by that peer.
	//
	// Conditioned on a starved peer actually existing, not merely on there
	// being more than one active subject (SOL-153441 decision A, made precise).
	// Reserving whenever two subjects are active idles a slot for nobody
	// whenever both of them are busy, which costs 10% of the configured cap at
	// the default of 10 and buys nothing. Requiring a real beneficiary gives
	// the same guarantee at no cost.
	//
	// Note what this does and does not promise. A caller that has not arrived
	// yet gets nothing reserved for it — that is the deliberate trade. What it
	// gets once it does arrive and enqueue is a bound of ONE completion: from
	// that moment the incumbent can no longer retake the slot it releases.
	if s.limit > 1 && free == 1 && subj.inFlight > 0 && s.hasStarvedSubjectLocked(subj) {
		return false
	}

	return true
}

// hasStarvedSubjectLocked reports whether some subject other than except has
// work queued and holds no slots — i.e. whether the last-slot reservation has a
// beneficiary. Called with mu held.
func (s *Scheduler) hasStarvedSubjectLocked(except *subjectState) bool {
	for _, subj := range s.subjects {
		if subj == except {
			continue
		}
		if subj.inFlight == 0 && subj.hasQueued() {
			return true
		}
	}
	return false
}

// sessionEligibleLocked reports whether one session of an already-eligible
// subject may take a slot. Called with mu held.
//
// Without this the slot rules would give NO protection between sessions of one
// subject, and that is the case that matters most rather than an edge: under an
// OAuth client_credentials grant there is exactly one subject, so the
// subject-level ceiling is the whole cap and the cross-subject reservation
// never fires. One session in a retry storm could then hold every slot and
// starve every sibling session for a full retry chain — roughly 16 minutes at
// defaults — which is the precise failure the ceiling and reservation were
// written to bound.
//
// Nested rather than replacing the subject bound: the sub-ceilings can sum to
// slightly more than the subject's allowance because of rounding, and the
// subject check is what still caps the total.
func (s *Scheduler) sessionEligibleLocked(subj *subjectState, cs *sessionState) bool {
	ceiling := s.subjectCeilingLocked()

	n := len(subj.sessions)
	if n < 1 {
		n = 1
	}
	sessionCeiling := (ceiling + n - 1) / n
	if sessionCeiling < 1 {
		sessionCeiling = 1
	}
	if cs.inFlight >= sessionCeiling {
		return false
	}

	// The same last-slot reservation, applied to the subject's own headroom so
	// one session cannot consume the last of it while a sibling session is
	// queued and holds none. Conditioned on a real beneficiary for the same
	// reason as the subject-level rule.
	headroom := ceiling - subj.inFlight
	if ceiling > 1 && headroom == 1 && cs.inFlight > 0 && subj.hasStarvedSession(cs) {
		return false
	}

	return true
}

// releaseLocked returns one slot held by key. Called with mu held.
func (s *Scheduler) releaseLocked(key CallerKey) {
	subj := s.subjects[key.Subject]
	if subj == nil {
		return
	}
	cs := subj.sessions[key.Session]
	if cs == nil || cs.inFlight == 0 {
		// Defensive: a release with no matching holding would mean a double
		// release, which would corrupt the in-flight accounting for every other
		// caller. Drop it rather than drive the counters negative.
		return
	}
	<-s.sem
	s.inFlight--
	subj.inFlight--
	cs.inFlight--
	s.pruneSessionLocked(subj, key)
}

// removeFromQueueLocked drops an abandoned waiter from its session's subqueue.
// Called with mu held.
func (s *Scheduler) removeFromQueueLocked(w *Waiter) {
	subj := s.subjects[w.key.Subject]
	if subj == nil {
		return
	}
	cs := subj.sessions[w.key.Session]
	if cs == nil {
		return
	}
	for i, q := range cs.queue {
		if q == w {
			cs.queue = append(cs.queue[:i], cs.queue[i+1:]...)
			break
		}
	}
	s.pruneSessionLocked(subj, w.key)
}

// pruneSessionLocked deletes a session with nothing queued and nothing in
// flight, and then the subject if that was its last session. Called with mu
// held.
//
// This is the whole of the memory story: bookkeeping scales with requests
// currently waiting or in flight, not with callers ever seen, so a long-running
// process accumulates nothing. Note what it does NOT bound — the depth of a
// subqueue. That is the pre-existing accepted risk of a caller with no context
// deadline, now bounded in time by semp.max_queue_wait (SOL-153442) rather than
// in depth.
func (s *Scheduler) pruneSessionLocked(subj *subjectState, key CallerKey) {
	cs := subj.sessions[key.Session]
	if cs == nil || cs.active() {
		return
	}
	delete(subj.sessions, key.Session)
	subj.rotor.remove(key.Session)

	if subj.active() {
		return
	}
	delete(s.subjects, key.Subject)
	s.rotor.remove(key.Subject)
}

// drain releases every queued waiter with ErrSchedulerStopped. Slots already
// held are left alone: their requests are in flight against the broker and
// their own Release calls still run.
func (s *Scheduler) drain() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for subjKey, subj := range s.subjects {
		for sessKey, cs := range subj.sessions {
			for _, w := range cs.queue {
				w.err = ErrSchedulerStopped
				close(w.ready)
			}
			cs.queue = nil
			if !cs.active() {
				delete(subj.sessions, sessKey)
				subj.rotor.remove(sessKey)
			}
		}
		if !subj.active() {
			delete(s.subjects, subjKey)
			s.rotor.remove(subjKey)
		}
	}
}

// stats reports live occupancy: how many subjects and sessions the scheduler is
// tracking and how many slots are held. Used by tests; a future metrics
// exporter is the natural second consumer.
func (s *Scheduler) stats() (subjects, sessions, inFlight int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, subj := range s.subjects {
		sessions += len(subj.sessions)
	}
	return len(s.subjects), sessions, s.inFlight
}

// sessionInFlight reports how many slots one session currently holds.
func (s *Scheduler) sessionInFlight(key CallerKey) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	subj := s.subjects[key.Subject]
	if subj == nil {
		return 0
	}
	cs := subj.sessions[key.Session]
	if cs == nil {
		return 0
	}
	return cs.inFlight
}
