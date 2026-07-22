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
	"fmt"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
)

// CircuitBreakerConfig is the breaker's tuning surface. The breaker is always
// constructed from this struct: DefaultCircuitBreakerConfig provides the base
// values, and operator config (broker_oauth.circuit_breaker) overlays them via
// tokenexchange.FromConfig — so config plugs in by populating the struct, not
// by rewiring the constructor.
//
// The fields describe the conceptual window and thresholds, not gobreaker's
// bucket granularity: that is derived internally so operators never configure a
// library implementation detail.
type CircuitBreakerConfig struct {
	// FailureRateWindow is how much recent history the failure rate is computed
	// over. Long enough to ignore a brief blip, short enough that a resolved
	// outage stops influencing the breaker quickly.
	FailureRateWindow time.Duration

	// MinimumRequests is how many classified operations must exist in the
	// window before the rate rule may open the breaker — it stops a tiny sample
	// (2 failures out of 2) from tripping on noise.
	MinimumRequests uint32

	// FailureRateThresholdPercent is the failure percentage (of counted
	// successes+failures, excluding excluded outcomes) that opens the breaker
	// once MinimumRequests is met.
	FailureRateThresholdPercent float64

	// ConsecutiveFailureThreshold opens the breaker after this many failures in
	// a row, without waiting for the rate rule's MinimumRequests sample. The
	// count is window-bound (gobreaker decays it as the rolling window's buckets
	// age out), so it trips fast-failing outages; failures spaced out by long
	// retry chains may never accumulate — see newReadyToTrip's regime note.
	// Zero disables the rule.
	ConsecutiveFailureThreshold uint32

	// OpenStateDuration is how long the breaker stays fully open before allowing
	// recovery probes. Too short re-probes an IdP that is still down; too long
	// keeps failing fast after it has recovered.
	OpenStateDuration time.Duration

	// HalfOpenProbeRequests is how many probes are allowed through while testing
	// recovery. Kept small so a recovering IdP is not hit with a burst.
	HalfOpenProbeRequests uint32
}

// DefaultCircuitBreakerConfig returns the production-safe defaults. It is a
// function, not a package var, so each caller gets a fresh value and cannot
// mutate the defaults seen by others.
func DefaultCircuitBreakerConfig() CircuitBreakerConfig {
	return CircuitBreakerConfig{
		FailureRateWindow:           defaultCircuitBreakerFailureRateWindow,
		MinimumRequests:             defaultCircuitBreakerMinimumRequests,
		FailureRateThresholdPercent: defaultCircuitBreakerFailureRateThresholdPercent,
		ConsecutiveFailureThreshold: defaultCircuitBreakerConsecutiveFailureThreshold,
		OpenStateDuration:           defaultCircuitBreakerOpenStateDuration,
		HalfOpenProbeRequests:       defaultCircuitBreakerHalfOpenProbeRequests,
	}
}

// Validate guards the bounds an operator override (broker_oauth.circuit_breaker)
// could violate. The defaults always pass; FromConfig calls this as the final
// gate after overlaying operator values onto the defaults.
func (c CircuitBreakerConfig) Validate() error {
	// Bounds are the shared config.*IdP* constants — the single source of truth
	// also used by the config-layer YAML validator (config.validateIdPCircuitBreaker),
	// so a value rejected here is rejected there and vice versa. Derived bucket
	// granularity is window/10, so the floor keeps the bucket period non-zero;
	// the ceilings are sanity guardrails, not recommended ranges (see the
	// constants' doc in config/idp_circuit_breaker.go).
	if c.FailureRateWindow < config.MinIdPFailureRateWindow || c.FailureRateWindow > config.MaxIdPFailureRateWindow {
		return fmt.Errorf("circuit breaker: failure_rate_window must be in [%v, %v], got %v", config.MinIdPFailureRateWindow, config.MaxIdPFailureRateWindow, c.FailureRateWindow)
	}
	if c.MinimumRequests == 0 || c.MinimumRequests > config.MaxIdPMinimumRequests {
		return fmt.Errorf("circuit breaker: minimum_requests must be in [1, %d], got %d", config.MaxIdPMinimumRequests, c.MinimumRequests)
	}
	if c.FailureRateThresholdPercent <= 0 || c.FailureRateThresholdPercent > 100 {
		return fmt.Errorf("circuit breaker: failure_rate_threshold_percent must be in (0, 100], got %v", c.FailureRateThresholdPercent)
	}
	// ConsecutiveFailureThreshold == 0 is intentionally allowed: it disables the
	// consecutive-failure rule, leaving the rate rule as the only trip. The cap
	// is a nonsense guard only and is deliberately NOT tied to MinimumRequests —
	// the two trip rules are independent (see newReadyToTrip).
	if c.ConsecutiveFailureThreshold > config.MaxIdPConsecutiveFailureThreshold {
		return fmt.Errorf("circuit breaker: consecutive_failure_threshold must not exceed %d, got %d", config.MaxIdPConsecutiveFailureThreshold, c.ConsecutiveFailureThreshold)
	}
	if c.OpenStateDuration <= 0 || c.OpenStateDuration > config.MaxIdPOpenStateDuration {
		return fmt.Errorf("circuit breaker: open_state_duration must be in (0, %v], got %v", config.MaxIdPOpenStateDuration, c.OpenStateDuration)
	}
	if c.HalfOpenProbeRequests < 1 || c.HalfOpenProbeRequests > config.MaxIdPHalfOpenProbeRequests {
		return fmt.Errorf("circuit breaker: half_open_probe_requests must be in [1, %d], got %d", config.MaxIdPHalfOpenProbeRequests, c.HalfOpenProbeRequests)
	}
	return nil
}
