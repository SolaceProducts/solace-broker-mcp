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
// will override the config struct, not these constants. Values are tuned for a
// single shared IdP; see the accompanying field docs on CircuitBreakerConfig for
// the reasoning behind each.
const (
	defaultCircuitBreakerFailureRateWindow = 30 * time.Second

	defaultCircuitBreakerMinimumRequests uint32 = 10

	defaultCircuitBreakerFailureRateThresholdPercent float64 = 50

	defaultCircuitBreakerConsecutiveFailureThreshold uint32 = 5

	defaultCircuitBreakerOpenStateDuration = 30 * time.Second

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
