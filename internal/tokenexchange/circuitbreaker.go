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
func newTokenExchangeCircuitBreaker(cfg CircuitBreakerConfig) *gobreaker.CircuitBreaker[*Token] {
	return gobreaker.NewCircuitBreaker[*Token](gobreaker.Settings{
		Name:          breakerName,
		Interval:      cfg.FailureRateWindow,
		BucketPeriod:  cfg.FailureRateWindow / 10,
		Timeout:       cfg.OpenStateDuration,
		MaxRequests:   cfg.HalfOpenProbeRequests,
		ReadyToTrip:   newReadyToTrip(cfg),
		IsSuccessful:  isBreakerSuccess,
		IsExcluded:    isBreakerExcluded,
		OnStateChange: logBreakerStateChange,
	})
}

// newReadyToTrip builds the trip predicate. Two rules: a consecutive-failure
// count, and a failure-rate rule gated by a minimum sample so a couple of
// failures cannot trip on noise. The denominator excludes excluded outcomes
// (429, cancellations) so throttling cannot dilute or drive the rate.
//
// Regime gap, deliberate: gobreaker's ConsecutiveFailures counter is
// window-bound — it decays as the rolling window's buckets (window/10) age
// out — so the consecutive rule only trips when counted failures arrive in
// quick succession, i.e. a fast-failing outage such as connection refused.
// A slow outage whose failures each burn the full retry-chain deadline
// (~19s) spaces them too far apart to ever accumulate, and at traffic too
// low for MinimumRequests the rate rule cannot trip either, so in that
// low-traffic slow-failure regime the breaker stays closed (fails open):
// exchanges still run their normal retry/timeout budget.
func newReadyToTrip(cfg CircuitBreakerConfig) func(gobreaker.Counts) bool {
	return func(counts gobreaker.Counts) bool {
		if cfg.ConsecutiveFailureThreshold > 0 &&
			counts.ConsecutiveFailures >= cfg.ConsecutiveFailureThreshold {
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

// logBreakerStateChange runs UNDER the breaker's internal mutex — gobreaker
// (v2.4.0) fires OnStateChange from afterRequest while cb.mutex is held — so
// while it executes, all breaker traffic is serialized behind it. Transitions
// are rare and the work here is a single WARN log, which is acceptable; keep
// this callback to cheap logging only (no blocking work, and no State()/
// Counts() calls — those re-take the same lock). Transitions are
// operationally important (the IdP just became unreachable, or recovered),
// hence WARN.
func logBreakerStateChange(name string, from, to gobreaker.State) {
	slog.Warn("token exchange circuit breaker state change",
		slog.String("breaker", name),
		slog.String("from", from.String()),
		slog.String("to", to.String()))
}

// The circuit breaker protects one shared IdP, so its counters must reflect
// IdP availability and nothing else. These three functions translate a Layer 2
// exchange outcome into one of gobreaker's three verdicts — excluded, failure,
// or success — and own the policy for which outcomes mean "the IdP is
// unhealthy". They must stay fast and side-effect-free: gobreaker invokes them
// while holding the lock that guards its internal counters, so no logging, no
// State()/Counts() calls, no blocking work.

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
