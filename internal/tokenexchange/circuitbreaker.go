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
	"sync/atomic"

	"github.com/sony/gobreaker/v2"
)

// breakerName labels the breaker in logs. Internal only — there is
// one breaker per process guarding the one IdP, so operators never need to
// change it.
const breakerName = "idp-token-exchange"

// newTokenExchangeCircuitBreaker builds the process-wide breaker from config.
// The success type is *Token because that is what Exchange's protected section
// returns. BucketPeriod is derived from the window (never operator-set) so the
// failure rate is computed over ~10 rolling buckets.
//
// consecutiveFailures is a local, not an Exchanger field — only the two
// closures built here ever touch it, so a wider home would only widen its
// blast radius for no benefit. See newReadyToTrip for why it exists.
func newTokenExchangeCircuitBreaker(cfg CircuitBreakerConfig) *gobreaker.CircuitBreaker[*Token] {
	var consecutiveFailures atomic.Uint32
	return gobreaker.NewCircuitBreaker[*Token](gobreaker.Settings{
		Name:          breakerName,
		Interval:      cfg.FailureRateWindow,
		BucketPeriod:  cfg.FailureRateWindow / 10,
		Timeout:       cfg.OpenStateDuration,
		MaxRequests:   cfg.HalfOpenProbeRequests,
		ReadyToTrip:   newReadyToTrip(cfg, &consecutiveFailures),
		IsSuccessful:  newCountingIsBreakerSuccess(&consecutiveFailures),
		IsExcluded:    isBreakerExcluded,
		OnStateChange: newLogBreakerStateChange(&consecutiveFailures),
	})
}

// newReadyToTrip builds the trip predicate. Two independent rules: a
// consecutive-failure count, and a failure-rate rule gated by a minimum
// sample so a couple of failures cannot trip on noise. The denominator
// excludes excluded outcomes (429, cancellations) so throttling cannot
// dilute or drive the rate.
//
// The consecutive rule reads consecutiveFailures, an undecayed counter this
// package owns, instead of gobreaker's own counts.ConsecutiveFailures:
// gobreaker's version decays as its rolling window's buckets age out, so a
// slow, low-traffic outage (failures spaced wider than a bucket period) can
// leave it permanently below threshold even though every exchange failed —
// and too sparse for MinimumRequests to save via the rate rule. This
// counter never decays on time, only on an observed success, because
// Exchange sits behind a token cache: a quiet gap usually means "nothing
// needed a token," not "the IdP healed."
func newReadyToTrip(cfg CircuitBreakerConfig, consecutiveFailures *atomic.Uint32) func(gobreaker.Counts) bool {
	return func(counts gobreaker.Counts) bool {
		if cfg.ConsecutiveFailureThreshold > 0 &&
			consecutiveFailures.Load() >= cfg.ConsecutiveFailureThreshold {
			return true
		}

		evaluated := counts.TotalSuccesses + counts.TotalFailures
		if evaluated < cfg.MinimumRequests {
			return false
		}

		failureRate := float64(counts.TotalFailures) / float64(evaluated) * 100
		return failureRate >= cfg.FailureRateThresholdPercent
	}
}

// newCountingIsBreakerSuccess wraps isBreakerSuccess to maintain
// consecutiveFailures (see newReadyToTrip) — the name says so because this
// is the one callback slot below that is NOT a pure function of its error
// argument; see the block comment above isBreakerExcluded for why that
// matters. gobreaker only calls IsSuccessful for outcomes IsExcluded already
// let through, so an excluded outcome never reaches the counter either —
// matching the rate rule's denominator. The write is a lock-free atomic op,
// so it stays within the "fast and side-effect-free" constraint despite not
// being pure.
func newCountingIsBreakerSuccess(consecutiveFailures *atomic.Uint32) func(error) bool {
	return func(err error) bool {
		ok := isBreakerSuccess(err)
		if ok {
			consecutiveFailures.Store(0)
		} else {
			consecutiveFailures.Add(1)
		}
		return ok
	}
}

// newLogBreakerStateChange builds the OnStateChange callback. It runs UNDER
// the breaker's internal mutex (gobreaker v2.4.0 fires it from afterRequest
// with cb.mutex held), so keep it to cheap logging only — no blocking work,
// no State()/Counts() calls (those re-take the lock; the atomic Load below
// does not). WARN because transitions are operationally important.
//
// consecutive_failures tells the operator which rule opened the breaker: on
// closed→open it equals the threshold when the consecutive rule fired, and
// sits below it when the rate rule did. (The counter keeps incrementing and
// resetting even with consecutive_failure_threshold: 0 — that comparison
// just has no meaning once the rule is disabled.)
func newLogBreakerStateChange(consecutiveFailures *atomic.Uint32) func(name string, from, to gobreaker.State) {
	return func(name string, from, to gobreaker.State) {
		slog.Warn("token exchange circuit breaker state change",
			slog.String("breaker", name),
			slog.String("from", from.String()),
			slog.String("to", to.String()),
			slog.Uint64("consecutive_failures", uint64(consecutiveFailures.Load())))
	}
}

// The circuit breaker protects one shared IdP, so its counters must reflect
// IdP availability and nothing else. These three functions translate a Layer 2
// exchange outcome into one of gobreaker's three verdicts — excluded, failure,
// or success — and own the policy for which outcomes mean "the IdP is
// unhealthy". They are pure functions of their error argument, and must stay
// that way: no logging, no State()/Counts() calls, no blocking work. (The
// IsSuccessful slot actually installed is newCountingIsBreakerSuccess, a thin
// wrapper that is deliberately NOT pure — see its own doc.)
//
// Correctness here leans on three gobreaker v2.4.0 behaviors that are not
// documented contract: IsExcluded runs before IsSuccessful; IsSuccessful runs
// before ReadyToTrip sees its effect; and a stale outcome (started before a
// trip, finished after recovery) is discarded by the generation check before
// either callback runs. The first two are covered end-to-end — a bump that
// reordered them would fail the sparse-spacing and exclusion tests in
// circuitbreaker_exchange_test.go. The third is not test-covered, which is
// why gobreaker is excluded from the go-minor-patch Dependabot group
// (.github/dependabot.yml): a bump needs a human reading its changelog, not
// an auto-merge that could silently let stale results pollute the counter.

// isBreakerExcluded reports outcomes that must count as neither success nor
// failure. Excluding them (rather than counting a success) keeps the failure
// rate honest: a caller cancelling, or the server rejecting a bad token, says
// nothing about whether the IdP is up. gobreaker consults this first — an
// excluded outcome never reaches isBreakerSuccess.
func isBreakerExcluded(err error) bool {
	if err == nil {
		return false // a real success is not an exclusion
	}

	// Not an availability signal at all: the caller went away, or the failure
	// originates inside this process rather than at the IdP.
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrExchangeRequestBuild) ||
		errors.Is(err, ErrExchangeMissingSubject) {
		return true
	}

	// A rate limit means the IdP is reachable and deliberately throttling;
	// a config fault (bad TLS trust/hostname, DNS name-not-found) means the
	// endpoint is misconfigured, not down. Neither is an availability signal,
	// so both are excluded — and excluded regardless of whether retries were
	// exhausted, which is why the class must survive the exhaustion rewrap.
	// Counting a config fault would let one operator typo trip the shared
	// breaker for every tenant, and it would never heal.
	switch failureClassOf(err) {
	case FailureClassRateLimited, FailureClassConfig:
		return true
	}

	return false
}

// isBreakerSuccess reports whether an outcome counts as a success for breaker
// purposes. gobreaker calls this only for outcomes isBreakerExcluded did not
// exclude, so "not a counted failure" is the same as success here — including
// a rejection (errno at the IdP proves it is up).
func isBreakerSuccess(err error) bool {
	return !isBreakerFailure(err)
}

// isBreakerFailure reports the outcomes that indicate the IdP itself is
// unhealthy: no usable response (network), a server-side 5xx, or a transport
// interruption while reading the response body. A rejection is deliberately
// absent — the IdP answered, so it is available.
func isBreakerFailure(err error) bool {
	if err == nil {
		return false
	}
	switch failureClassOf(err) {
	case FailureClassNetwork, FailureClassUpstream5xx, FailureClassBodyRead:
		return true
	default:
		return false
	}
}

// failureClassOf extracts the FailureClass from an error's ExchangeError, or
// FailureClassNone if the error is not (or does not wrap) an *ExchangeError.
// Reaching through the wrapping chain is what lets classification survive the
// ErrExchangeRetriesExhausted rewrap.
func failureClassOf(err error) FailureClass {
	var exchErr *ExchangeError
	if errors.As(err, &exchErr) {
		return exchErr.FailureClass
	}
	return FailureClassNone
}
