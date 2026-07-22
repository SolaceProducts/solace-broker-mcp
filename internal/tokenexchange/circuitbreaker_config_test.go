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
	"testing"
	"time"
)

// TestDefaultCircuitBreakerConfig_Values pins the shipped defaults. A change
// here is operationally visible, so the exact values are asserted rather than
// just "non-zero".
func TestDefaultCircuitBreakerConfig_Values(t *testing.T) {
	t.Parallel()
	got := DefaultCircuitBreakerConfig()
	want := CircuitBreakerConfig{
		FailureRateWindow:           30 * time.Second,
		MinimumRequests:             10,
		FailureRateThresholdPercent: 50,
		ConsecutiveFailureThreshold: 5,
		OpenStateDuration:           30 * time.Second,
		HalfOpenProbeRequests:       2,
	}
	if got != want {
		t.Errorf("DefaultCircuitBreakerConfig() = %+v, want %+v", got, want)
	}
}

// TestDefaultCircuitBreakerConfig_Validates guards that the shipped defaults
// never fail their own validation — the whole "start with defaults" premise
// depends on it.
func TestDefaultCircuitBreakerConfig_Validates(t *testing.T) {
	t.Parallel()
	if err := DefaultCircuitBreakerConfig().Validate(); err != nil {
		t.Errorf("DefaultCircuitBreakerConfig().Validate() = %v, want nil", err)
	}
}

// TestDefaultCircuitBreakerConfig_ReturnsFreshValue proves the function returns
// an independent value each call, not a shared mutable one — the reason it is a
// function and not a package var. Mutating one result must not affect another.
func TestDefaultCircuitBreakerConfig_ReturnsFreshValue(t *testing.T) {
	t.Parallel()
	a := DefaultCircuitBreakerConfig()
	a.FailureRateThresholdPercent = 99
	b := DefaultCircuitBreakerConfig()
	if b.FailureRateThresholdPercent != 50 {
		t.Errorf("mutating one config changed another: b.FailureRateThresholdPercent = %v, want 50", b.FailureRateThresholdPercent)
	}
}

func TestCircuitBreakerConfig_Validate(t *testing.T) {
	t.Parallel()

	// mutate applies a tweak to a fresh valid default so each case differs from
	// the baseline by exactly one field.
	mutate := func(f func(*CircuitBreakerConfig)) CircuitBreakerConfig {
		c := DefaultCircuitBreakerConfig()
		f(&c)
		return c
	}

	cases := []struct {
		name    string
		cfg     CircuitBreakerConfig
		wantErr bool
	}{
		{"defaults valid", DefaultCircuitBreakerConfig(), false},

		{"zero window", mutate(func(c *CircuitBreakerConfig) { c.FailureRateWindow = 0 }), true},
		{"negative window", mutate(func(c *CircuitBreakerConfig) { c.FailureRateWindow = -time.Second }), true},
		{"sub-bucket window", mutate(func(c *CircuitBreakerConfig) { c.FailureRateWindow = time.Millisecond }), true},

		{"zero minimum requests", mutate(func(c *CircuitBreakerConfig) { c.MinimumRequests = 0 }), true},

		{"zero threshold", mutate(func(c *CircuitBreakerConfig) { c.FailureRateThresholdPercent = 0 }), true},
		{"negative threshold", mutate(func(c *CircuitBreakerConfig) { c.FailureRateThresholdPercent = -1 }), true},
		{"over-100 threshold", mutate(func(c *CircuitBreakerConfig) { c.FailureRateThresholdPercent = 100.1 }), true},
		{"threshold exactly 100 ok", mutate(func(c *CircuitBreakerConfig) { c.FailureRateThresholdPercent = 100 }), false},

		{"zero consecutive disables rule (allowed)", mutate(func(c *CircuitBreakerConfig) { c.ConsecutiveFailureThreshold = 0 }), false},

		{"zero open duration", mutate(func(c *CircuitBreakerConfig) { c.OpenStateDuration = 0 }), true},
		{"negative open duration", mutate(func(c *CircuitBreakerConfig) { c.OpenStateDuration = -time.Second }), true},

		{"zero probes", mutate(func(c *CircuitBreakerConfig) { c.HalfOpenProbeRequests = 0 }), true},
		{"probes at cap ok", mutate(func(c *CircuitBreakerConfig) { c.HalfOpenProbeRequests = maxHalfOpenProbeRequests }), false},
		{"probes over cap", mutate(func(c *CircuitBreakerConfig) { c.HalfOpenProbeRequests = maxHalfOpenProbeRequests + 1 }), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.cfg.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate() = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}
