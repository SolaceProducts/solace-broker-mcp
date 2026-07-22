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

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
)

func ptr[T any](v T) *T { return &v }

// TestResolveCircuitBreakerConfig_NilUsesDefaults asserts that no operator
// config yields the full shipped defaults with the breaker enabled.
func TestResolveCircuitBreakerConfig_NilUsesDefaults(t *testing.T) {
	t.Parallel()
	got := resolveCircuitBreakerConfig(nil)
	if got == nil {
		t.Fatal("resolveCircuitBreakerConfig(nil) = nil, want defaults (breaker enabled)")
	}
	if *got != DefaultCircuitBreakerConfig() {
		t.Errorf("resolveCircuitBreakerConfig(nil) = %+v, want defaults %+v", *got, DefaultCircuitBreakerConfig())
	}
}

// TestResolveCircuitBreakerConfig_DisabledReturnsNil asserts the escape hatch:
// enabled: false yields a nil runtime config, which New reads as "breaker off".
func TestResolveCircuitBreakerConfig_DisabledReturnsNil(t *testing.T) {
	t.Parallel()
	got := resolveCircuitBreakerConfig(&config.IdPCircuitBreakerConfig{Enabled: ptr(false)})
	if got != nil {
		t.Errorf("resolveCircuitBreakerConfig(enabled=false) = %+v, want nil", *got)
	}
}

// TestResolveCircuitBreakerConfig_EnabledTrueUsesDefaults asserts that an
// explicit enabled: true with no other fields is the same as the defaults.
func TestResolveCircuitBreakerConfig_EnabledTrueUsesDefaults(t *testing.T) {
	t.Parallel()
	got := resolveCircuitBreakerConfig(&config.IdPCircuitBreakerConfig{Enabled: ptr(true)})
	if got == nil || *got != DefaultCircuitBreakerConfig() {
		t.Errorf("resolveCircuitBreakerConfig(enabled=true) = %v, want defaults", got)
	}
}

// TestResolveCircuitBreakerConfig_PartialOverlay asserts that only the fields
// the operator set are overlaid; the rest keep their defaults.
func TestResolveCircuitBreakerConfig_PartialOverlay(t *testing.T) {
	t.Parallel()
	got := resolveCircuitBreakerConfig(&config.IdPCircuitBreakerConfig{
		FailureRateThresholdPercent: ptr(75.0),
		OpenStateDuration:           ptr(90 * time.Second),
	})
	if got == nil {
		t.Fatal("got nil, want overlaid config")
	}

	want := DefaultCircuitBreakerConfig()
	want.FailureRateThresholdPercent = 75.0
	want.OpenStateDuration = 90 * time.Second
	if *got != want {
		t.Errorf("partial overlay = %+v, want %+v", *got, want)
	}
}

// TestResolveCircuitBreakerConfig_FullOverlay asserts every field overlays, and
// the result differs from the defaults on each.
func TestResolveCircuitBreakerConfig_FullOverlay(t *testing.T) {
	t.Parallel()
	got := resolveCircuitBreakerConfig(&config.IdPCircuitBreakerConfig{
		FailureRateWindow:           ptr(60 * time.Second),
		MinimumRequests:             ptr(uint32(20)),
		FailureRateThresholdPercent: ptr(80.0),
		ConsecutiveFailureThreshold: ptr(uint32(3)),
		OpenStateDuration:           ptr(45 * time.Second),
		HalfOpenProbeRequests:       ptr(uint32(1)),
	})
	want := CircuitBreakerConfig{
		FailureRateWindow:           60 * time.Second,
		MinimumRequests:             20,
		FailureRateThresholdPercent: 80.0,
		ConsecutiveFailureThreshold: 3,
		OpenStateDuration:           45 * time.Second,
		HalfOpenProbeRequests:       1,
	}
	if got == nil || *got != want {
		t.Errorf("full overlay = %v, want %+v", got, want)
	}
}
