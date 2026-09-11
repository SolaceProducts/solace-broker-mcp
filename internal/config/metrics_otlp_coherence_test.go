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
	"strings"
	"testing"
)

// OBS_METRICS_OTLP_ENABLED=true with metrics themselves off is refused at
// config load (SOL-152418, Story 46): the OTLP reader attaches to the same
// meter provider Story 14 builds, and there is no provider to attach to with
// metrics disabled.
func TestValidate_MetricsOTLPRequiresMetricsEnabled(t *testing.T) {
	tests := []struct {
		name           string
		metricsEnabled bool
		otlpEnabled    bool
		wantErr        bool
	}{
		{
			name:           "OTLP on, metrics on: coherent",
			metricsEnabled: true,
			otlpEnabled:    true,
			wantErr:        false,
		},
		{
			name:           "OTLP on, metrics off: rejected",
			metricsEnabled: false,
			otlpEnabled:    true,
			wantErr:        true,
		},
		{
			name:           "OTLP off, metrics off: fine (both off)",
			metricsEnabled: false,
			otlpEnabled:    false,
			wantErr:        false,
		},
		{
			name:           "OTLP off, metrics on: fine (Prometheus-only)",
			metricsEnabled: true,
			otlpEnabled:    false,
			wantErr:        false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearObsEnv(t)
			if tc.metricsEnabled {
				t.Setenv("OBS_METRICS_ENABLED", "true")
			}
			if tc.otlpEnabled {
				t.Setenv("OBS_METRICS_OTLP_ENABLED", "true")
			}

			_, err := LoadConfig(writeTemp(t, obsYAML))
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected a coherence error, got nil")
				}
				if !strings.Contains(err.Error(), "OBS_METRICS_OTLP_ENABLED") {
					t.Errorf("error should name OBS_METRICS_OTLP_ENABLED so an operator can act on it, got: %v", err)
				}
				if !strings.Contains(err.Error(), "OBS_METRICS_ENABLED") {
					t.Errorf("error should name OBS_METRICS_ENABLED as the fix, got: %v", err)
				}
			} else if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
		})
	}
}
