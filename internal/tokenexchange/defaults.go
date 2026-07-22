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

import "time"

// Package-local timing knobs for the retry policy. Kept here (rather
// than in internal/defaults) because only this package consumes them —
// idpclient accepts them as arguments, and no config validator or other
// package references them today. Centralizing them in one file lets us
// keep every timing decision — per-attempt, backoff bounds, retry count,
// and the derived chain deadline — visible together.
//
// Numbers chosen for a healthy token endpoint on-network to same-region.
// See PR SOL-151520 for the evidence trail (Keycloak p99, Auth0 defaults,
// gRPC / SRE guidance).
const (
	// DefaultPerAttemptTimeout bounds one HTTP round-trip to the IdP.
	// Set on the inner *http.Client's Timeout, so it applies uniformly
	// to every retry attempt.
	DefaultPerAttemptTimeout = 5 * time.Second

	// DefaultMaxRetries is the number of retries AFTER the first
	// attempt. Total attempts = MaxRetries + 1. Matches gRPC's
	// recommended cap and Google SRE guidance ("cap retries at 3").
	DefaultMaxRetries = 2

	// DefaultRetryWaitMin and DefaultRetryWaitMax bound the jittered
	// backoff between attempts. `RateLimitLinearJitterBackoff` samples
	// uniformly in [min, max]; `Retry-After` overrides the sample when
	// present.
	DefaultRetryWaitMin = 1 * time.Second
	DefaultRetryWaitMax = 2 * time.Second
)

// Circuit-breaker defaults. Unexported because only DefaultCircuitBreakerConfig
// reads them — no other package touches these numbers, and a future YAML surface
// will override the config struct, not these constants.
//
// Each value is anchored to the defaults mainstream breakers ship, then adjusted
// for this deployment's shape: one shared IdP, protected by one process-wide
// breaker, at token-exchange volumes (lower than a general RPC path). Reference
// points: Hystrix, Resilience4j, Polly, Envoy outlier detection, sony/gobreaker.
const (
	// 30s: long enough to ride out a brief blip, short enough that a resolved
	// outage stops influencing the breaker within ~half a minute. Deliberately
	// longer than mainstream defaults (Hystrix 10s, Polly 30s, Envoy 10s)
	// because token-exchange traffic is lower-volume, so a slightly longer
	// window gathers a usable sample.
	defaultCircuitBreakerFailureRateWindow = 30 * time.Second

	// 10: minimum classified operations before the rate rule may trip, so a
	// 2-out-of-2 blip cannot open the breaker. Lower than Hystrix's 20 and
	// Resilience4j's 100 on purpose — those protect high-volume RPC paths;
	// token exchange sees far less traffic, and the consecutive-failure rule
	// covers the low-traffic outage case the higher minimums would miss.
	defaultCircuitBreakerMinimumRequests uint32 = 10

	// 50%: half of counted (non-excluded) operations failing is a clear outage
	// signal. Mirrors Resilience4j's and Polly's failure-rate defaults exactly.
	defaultCircuitBreakerFailureRateThresholdPercent float64 = 50

	// 5: consecutive failures that trip immediately regardless of sample size —
	// the fast path for a full outage at low traffic. Matches gobreaker's and
	// Envoy's consecutive-5xx default of 5.
	defaultCircuitBreakerConsecutiveFailureThreshold uint32 = 5

	// 30s open before probing recovery. Between Hystrix's 5s and Resilience4j's/
	// gobreaker's 60s. Long enough not to hammer a still-down IdP, short enough
	// that recovery is detected promptly; a multi-replica fleet may raise it so
	// replicas don't all probe at once.
	defaultCircuitBreakerOpenStateDuration = 30 * time.Second

	// 2: recovery probes admitted in half-open. Requires two consecutive
	// successes to close (stronger evidence than a single probe) without
	// bursting a fragile IdP. Between the common "1 trial call" (Hystrix, Polly,
	// gobreaker) and Resilience4j's 10.
	defaultCircuitBreakerHalfOpenProbeRequests uint32 = 2
)

// ComputeChainDeadline returns the total time budget for one exchange's
// retry loop, applied via `context.WithTimeout` around the singleflight
// function. The value bounds every attempt AND every backoff together —
// if it fires mid-loop, the loop returns `context.DeadlineExceeded`,
// which `classifyRetryOutcome` rewraps as `ErrExchangeRetriesExhausted`.
//
// Formula, when `override <= 0`:
//
//	(maxRetries + 1) * perAttempt   // worst-case attempt cost
//	+ maxRetries * retryWaitMax     // worst-case backoff cost
//
// With this package's defaults (5s, 2, 2s) the formula yields
// 3*5s + 2*2s = 19s.
//
// The override parameter exists as an extensibility seam: a future YAML
// path could surface a user-visible `broker_oauth.retry.chain_deadline`
// key and thread it here. Today no caller passes a positive override,
// so the formula wins. Keeping it in the signature costs nothing and
// avoids the "extract override support later" refactor.
//
// Rationale for a formula rather than a hardcoded constant: retry knobs
// compose — changing PerAttemptTimeout or MaxRetries without adjusting
// the chain deadline yields incoherent policy (per-attempt could exceed
// the chain). Deriving the chain from the other knobs keeps them in sync.
func ComputeChainDeadline(
	override time.Duration,
	perAttempt time.Duration,
	retryWaitMax time.Duration,
	maxRetries int,
) time.Duration {
	if override > 0 {
		return override
	}
	return time.Duration(maxRetries+1)*perAttempt +
		time.Duration(maxRetries)*retryWaitMax
}
