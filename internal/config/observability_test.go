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
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/defaults"
)

// obsYAML is a minimal valid config used by the observability tests. It
// configures one basic-auth broker and static client auth so LoadConfig
// reaches the end (defaults + env overrides applied, validation passes)
// without any broker/auth noise.
const obsYAML = `
mcp_client_auth:
  mode: static
  dev_token: test
brokers:
  prod:
    url: "https://broker.example.com:1943"
    auth:
      mode: basic
      username: admin
      password: secret
`

// TestObservability_FlagDefaults pins the v1 "door-closing" defaults: when no
// OBS_* env var is set, correlation IDs and panic recovery are ON and every
// other capability is OFF, with the auth-failure counter following metrics
// (OFF).
func TestObservability_FlagDefaults(t *testing.T) {
	cfg, err := LoadConfig(writeTemp(t, obsYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	o := cfg.Observability
	checks := []struct {
		name string
		got  bool
		want bool
	}{
		{"CorrelationIDEnabled", o.CorrelationIDEnabled, true},
		{"PanicRecoveryEnabled", o.PanicRecoveryEnabled, true},
		{"MetricsEnabled", o.MetricsEnabled, false},
		{"AuditLogEnabled", o.AuditLogEnabled, false},
		{"TracingEnabled", o.TracingEnabled, false},
		{"SaturationEventsEnabled", o.SaturationEventsEnabled, false},
		{"AuthFailureCounterEnabled", o.AuthFailureCounterEnabled, false},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("default %s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestObservability_EnvOverridesBothDirections flips the two defaults in
// opposite directions at once: correlation (default true) forced false, metrics
// (default false) forced true. Proves env overrides win over defaults in both
// directions.
func TestObservability_EnvOverridesBothDirections(t *testing.T) {
	t.Setenv("OBS_CORRELATION_ID_ENABLED", "false")
	t.Setenv("OBS_METRICS_ENABLED", "true")

	cfg, err := LoadConfig(writeTemp(t, obsYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Observability.CorrelationIDEnabled {
		t.Error("OBS_CORRELATION_ID_ENABLED=false should turn correlation OFF")
	}
	if !cfg.Observability.MetricsEnabled {
		t.Error("OBS_METRICS_ENABLED=true should turn metrics ON")
	}
}

// TestObservability_AuthFailureCounter_FollowsMetricsWhenUnset proves the
// follow-metrics behavior: with its own var unset, the auth-failure counter
// tracks OBS_METRICS_ENABLED.
func TestObservability_AuthFailureCounter_FollowsMetricsWhenUnset(t *testing.T) {
	t.Run("follows metrics on", func(t *testing.T) {
		t.Setenv("OBS_METRICS_ENABLED", "true")
		cfg, err := LoadConfig(writeTemp(t, obsYAML))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !cfg.Observability.AuthFailureCounterEnabled {
			t.Error("auth-failure counter should follow metrics ON when its own var is unset")
		}
	})

	t.Run("follows metrics off", func(t *testing.T) {
		t.Setenv("OBS_METRICS_ENABLED", "false")
		cfg, err := LoadConfig(writeTemp(t, obsYAML))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Observability.AuthFailureCounterEnabled {
			t.Error("auth-failure counter should follow metrics OFF when its own var is unset")
		}
	})
}

// TestObservability_AuthFailureCounter_ExplicitOverridesMetrics proves the
// explicit-set escape hatch: when OBS_AUTH_FAILURE_COUNTER_ENABLED is set, it
// wins regardless of metrics — including the tricky case of forcing it OFF
// while metrics are ON (the reason the implementation uses LookupEnv rather
// than a plain default).
func TestObservability_AuthFailureCounter_ExplicitOverridesMetrics(t *testing.T) {
	t.Run("explicit on while metrics off", func(t *testing.T) {
		t.Setenv("OBS_METRICS_ENABLED", "false")
		t.Setenv("OBS_AUTH_FAILURE_COUNTER_ENABLED", "true")
		cfg, err := LoadConfig(writeTemp(t, obsYAML))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !cfg.Observability.AuthFailureCounterEnabled {
			t.Error("explicit OBS_AUTH_FAILURE_COUNTER_ENABLED=true should win over metrics OFF")
		}
	})

	t.Run("explicit off while metrics on", func(t *testing.T) {
		t.Setenv("OBS_METRICS_ENABLED", "true")
		t.Setenv("OBS_AUTH_FAILURE_COUNTER_ENABLED", "false")
		cfg, err := LoadConfig(writeTemp(t, obsYAML))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Observability.AuthFailureCounterEnabled {
			t.Error("explicit OBS_AUTH_FAILURE_COUNTER_ENABLED=false should win over metrics ON")
		}
	})
}

// TestObservability_NumericDefaults pins the numeric tunable defaults applied
// when the observability block is omitted from YAML.
func TestObservability_NumericDefaults(t *testing.T) {
	cfg, err := LoadConfig(writeTemp(t, obsYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	o := cfg.Observability
	if o.SaturationThresholdMs != defaults.DefaultSaturationThresholdMs {
		t.Errorf("SaturationThresholdMs = %d, want %d", o.SaturationThresholdMs, defaults.DefaultSaturationThresholdMs)
	}
	if o.ProgressSignalThresholdMs != defaults.DefaultProgressSignalThresholdMs {
		t.Errorf("ProgressSignalThresholdMs = %d, want %d", o.ProgressSignalThresholdMs, defaults.DefaultProgressSignalThresholdMs)
	}
	if o.OTelSelfStatsIntervalS != defaults.DefaultOTelSelfStatsIntervalS {
		t.Errorf("OTelSelfStatsIntervalS = %d, want %d", o.OTelSelfStatsIntervalS, defaults.DefaultOTelSelfStatsIntervalS)
	}
}

// TestObservability_NumericFromYAML proves the numeric tunables parse from the
// YAML observability block (overriding the defaults) and that ${VAR}
// substitution reaches them — the substitution runs over raw bytes before
// decode, so an int field gets it for free.
func TestObservability_NumericFromYAML(t *testing.T) {
	t.Setenv("OTEL_INTERVAL", "120")

	yamlBody := obsYAML + `
observability:
  saturation_threshold_ms: 25
  progress_signal_threshold_ms: 8000
  otel_self_stats_interval_s: ${OTEL_INTERVAL}
`
	cfg, err := LoadConfig(writeTemp(t, yamlBody))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	o := cfg.Observability
	if o.SaturationThresholdMs != 25 {
		t.Errorf("SaturationThresholdMs = %d, want 25", o.SaturationThresholdMs)
	}
	if o.ProgressSignalThresholdMs != 8000 {
		t.Errorf("ProgressSignalThresholdMs = %d, want 8000", o.ProgressSignalThresholdMs)
	}
	if o.OTelSelfStatsIntervalS != 120 {
		t.Errorf("OTelSelfStatsIntervalS = %d, want 120 (from ${OTEL_INTERVAL})", o.OTelSelfStatsIntervalS)
	}
}
