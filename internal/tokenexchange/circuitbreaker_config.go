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
)

// CircuitBreakerConfig is the breaker's tuning surface. The breaker is always
// constructed from this struct — even while the only source of values is
// DefaultCircuitBreakerConfig — so the later YAML/environment plumbing plugs in
// by populating the struct, not by rewiring the constructor.
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
	// a row regardless of sample size. It exists for the low-traffic case where
	// the rate rule may never gather MinimumRequests during a full outage. Zero
	// disables the rule.
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

// Validate guards the bounds a later YAML/environment override could violate.
// The defaults always pass; the method exists now so the override path added
// later has a validation seam already in place rather than a refactor.
func (c CircuitBreakerConfig) Validate() error {
	if c.FailureRateWindow <= 0 {
		return fmt.Errorf("circuit breaker: failure_rate_window must be positive, got %v", c.FailureRateWindow)
	}
	// Derived bucket granularity is window/10 (see the breaker constructor); a
	// window under 10 buckets' worth would round to a zero bucket period.
	if c.FailureRateWindow < 10*time.Millisecond {
		return fmt.Errorf("circuit breaker: failure_rate_window must be at least 10ms, got %v", c.FailureRateWindow)
	}
	if c.MinimumRequests == 0 {
		return fmt.Errorf("circuit breaker: minimum_requests must be at least 1")
	}
	if c.FailureRateThresholdPercent <= 0 || c.FailureRateThresholdPercent > 100 {
		return fmt.Errorf("circuit breaker: failure_rate_threshold_percent must be in (0, 100], got %v", c.FailureRateThresholdPercent)
	}
	// ConsecutiveFailureThreshold == 0 is intentionally allowed: it disables the
	// consecutive-failure rule, leaving the rate rule as the only trip.
	if c.OpenStateDuration <= 0 {
		return fmt.Errorf("circuit breaker: open_state_duration must be positive, got %v", c.OpenStateDuration)
	}
	if c.HalfOpenProbeRequests < 1 || c.HalfOpenProbeRequests > maxHalfOpenProbeRequests {
		return fmt.Errorf("circuit breaker: half_open_probe_requests must be in [1, %d], got %d", maxHalfOpenProbeRequests, c.HalfOpenProbeRequests)
	}
	return nil
}

// maxHalfOpenProbeRequests bounds how much traffic a single recovery attempt
// can send at a still-fragile IdP. Most deployments want 1-3; the cap leaves
// headroom without allowing a probe burst.
const maxHalfOpenProbeRequests uint32 = 10
