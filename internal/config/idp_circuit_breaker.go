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

package config

import (
	"fmt"
	"time"
)

// IdPCircuitBreakerConfig is the operator-facing schema for the token-
// exchange circuit breaker. It nests under broker_oauth because the breaker
// has nothing to protect without OAuth token exchange. Every field is optional;
// an omitted field falls back to the shipped default, so `circuit_breaker: {}`
// is equivalent to omitting the block entirely (all defaults, enabled).
//
// This is the schema type; the runtime type (tokenexchange.CircuitBreakerConfig)
// is produced by translation in tokenexchange.FromConfig. Pointer fields let the
// translator tell "operator omitted this" from "operator set the zero value",
// which matters for validation (a set-to-zero threshold is an error; an omitted
// one takes the default).
type IdPCircuitBreakerConfig struct {
	// Enabled is the escape hatch. Nil or true keeps the breaker on; false
	// disables it (retries still run) and logs a WARN. Disabling is not
	// recommended in production.
	Enabled *bool `yaml:"enabled"`

	FailureRateWindow           *time.Duration `yaml:"failure_rate_window"`
	MinimumRequests             *uint32        `yaml:"minimum_requests"`
	FailureRateThresholdPercent *float64       `yaml:"failure_rate_threshold_percent"`
	ConsecutiveFailureThreshold *uint32        `yaml:"consecutive_failure_threshold"`
	OpenStateDuration           *time.Duration `yaml:"open_state_duration"`
	HalfOpenProbeRequests       *uint32        `yaml:"half_open_probe_requests"`
}

// Circuit-breaker validation ceilings. Exported as the single source of truth:
// the runtime validator in tokenexchange references these too (that package
// already imports config; config must not import it back, so the constants live
// here where both layers can reach them without a cycle).
//
// Only MaxIdPHalfOpenProbeRequests is a domain-meaningful limit — 10 matches
// the highest default any mainstream breaker uses (Resilience4j), and more would
// flood a recovering IdP. The rest are sanity guardrails, NOT recommended
// ranges: real deployments run windows of 10-30s, minimum-request counts of
// 5-100, open durations of 5-60s, and consecutive thresholds around 5. The caps
// exist only to reject absurd/typo values (a 999999h window, a minimum-requests
// count in the billions), not to imply those magnitudes are reasonable.
//
// MinIdPFailureRateWindow is the floor: the breaker derives its rolling
// bucket period as window/10, so a window under ~10 buckets' worth would round
// to a zero bucket period.
const (
	MinIdPFailureRateWindow                  = 10 * time.Millisecond
	MaxIdPFailureRateWindow                  = time.Hour
	MaxIdPMinimumRequests             uint32 = 1_000_000
	MaxIdPConsecutiveFailureThreshold uint32 = 1_000
	MaxIdPOpenStateDuration                  = time.Hour
	MaxIdPHalfOpenProbeRequests       uint32 = 10
)

// BreakerEnabled reports whether the breaker should be constructed. The default
// (nil block or nil Enabled) is true — the breaker is on unless an operator
// explicitly sets enabled: false.
func (b *BrokerOAuthConfig) BreakerEnabled() bool {
	if b == nil || b.CircuitBreaker == nil || b.CircuitBreaker.Enabled == nil {
		return true
	}
	return *b.CircuitBreaker.Enabled
}

// validateIdPCircuitBreaker checks the operator-set circuit-breaker fields.
// Only set (non-nil) fields are checked — an omitted field takes the shipped
// default, which is valid by construction. Bounds are the shared Min/Max*
// constants, so config validation catches a bad value at startup rather than at
// Exchanger construction. A nil block means "all defaults" and is always valid.
func validateIdPCircuitBreaker(cb *IdPCircuitBreakerConfig) []error {
	if cb == nil {
		return nil
	}
	var errs []error

	if cb.FailureRateWindow != nil && (*cb.FailureRateWindow < MinIdPFailureRateWindow || *cb.FailureRateWindow > MaxIdPFailureRateWindow) {
		errs = append(errs, fmt.Errorf("broker_oauth.circuit_breaker.failure_rate_window must be in [%v, %v], got %v", MinIdPFailureRateWindow, MaxIdPFailureRateWindow, *cb.FailureRateWindow))
	}
	if cb.MinimumRequests != nil && (*cb.MinimumRequests == 0 || *cb.MinimumRequests > MaxIdPMinimumRequests) {
		errs = append(errs, fmt.Errorf("broker_oauth.circuit_breaker.minimum_requests must be in [1, %d], got %d", MaxIdPMinimumRequests, *cb.MinimumRequests))
	}
	if cb.FailureRateThresholdPercent != nil && (*cb.FailureRateThresholdPercent <= 0 || *cb.FailureRateThresholdPercent > 100) {
		errs = append(errs, fmt.Errorf("broker_oauth.circuit_breaker.failure_rate_threshold_percent must be in (0, 100], got %v", *cb.FailureRateThresholdPercent))
	}
	// consecutive_failure_threshold: 0 is valid — it disables the rule. The
	// upper cap is a nonsense guard only (real values are ~5); it is NOT tied
	// to minimum_requests — the two trip rules are independent (a run of
	// consecutive failures vs. a failure rate over a sample), so coupling them
	// would break the low-traffic case the consecutive rule exists for.
	if cb.ConsecutiveFailureThreshold != nil && *cb.ConsecutiveFailureThreshold > MaxIdPConsecutiveFailureThreshold {
		errs = append(errs, fmt.Errorf("broker_oauth.circuit_breaker.consecutive_failure_threshold must not exceed %d, got %d", MaxIdPConsecutiveFailureThreshold, *cb.ConsecutiveFailureThreshold))
	}
	if cb.OpenStateDuration != nil && (*cb.OpenStateDuration <= 0 || *cb.OpenStateDuration > MaxIdPOpenStateDuration) {
		errs = append(errs, fmt.Errorf("broker_oauth.circuit_breaker.open_state_duration must be in (0, %v], got %v", MaxIdPOpenStateDuration, *cb.OpenStateDuration))
	}
	if cb.HalfOpenProbeRequests != nil && (*cb.HalfOpenProbeRequests < 1 || *cb.HalfOpenProbeRequests > MaxIdPHalfOpenProbeRequests) {
		errs = append(errs, fmt.Errorf("broker_oauth.circuit_breaker.half_open_probe_requests must be in [1, %d], got %d", MaxIdPHalfOpenProbeRequests, *cb.HalfOpenProbeRequests))
	}

	return errs
}
