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

package metrics

import (
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
)

// TestEnabled pins that Enabled is the OR of the two egress flags
// (SOL-154607): true for every provider-bearing configuration and false only
// when neither egress is on — a reflection of both flags, not of one.
func TestEnabled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		scrape, otlp bool
		want         bool
	}{
		{"neither", false, false, false},
		{"scrape only", true, false, true},
		{"OTLP only", false, true, true},
		{"both", true, true, true},
	}
	for _, tc := range tests {
		cfg := config.ObservabilityConfig{MetricsScrapeEnabled: tc.scrape, MetricsOTLPEnabled: tc.otlp}
		if got := Enabled(cfg); got != tc.want {
			t.Errorf("%s: Enabled() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestAuthFailureCounterEnabled pins that the accessor reflects the resolved
// AuthFailureCounterEnabled flag — both directions.
func TestAuthFailureCounterEnabled(t *testing.T) {
	t.Parallel()
	for _, want := range []bool{true, false} {
		cfg := config.ObservabilityConfig{AuthFailureCounterEnabled: want}
		if got := AuthFailureCounterEnabled(cfg); got != want {
			t.Errorf("AuthFailureCounterEnabled() = %v, want %v", got, want)
		}
	}
}
