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
	"os"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/defaults"
)

// clearObsEnv unsets every OBS_* capability flag for the duration of the test so
// a runner with these vars exported in its environment cannot leak into a test
// that asserts the door-closing DEFAULTS. Each var is restored on cleanup. Use
// at the top of any test that asserts default flag values.
//
// Numeric tunables (saturation_threshold_ms, etc.) are NOT env-driven — they
// load from YAML — so there are no numeric OBS_* override vars to clear here.
func clearObsEnv(t *testing.T) {
	t.Helper()
	vars := []string{
		envObsCorrelationIDEnabled,
		envObsMetricsEnabled,
		envObsAuditLogEnabled,
		envObsTracingEnabled,
		envObsSaturationEventsEnabled,
		envObsAuthFailureCounterEnabled,
	}
	for _, name := range vars {
		if prev, ok := os.LookupEnv(name); ok {
			if err := os.Unsetenv(name); err != nil {
				t.Fatalf("os.Unsetenv(%q): %v", name, err)
			}
			t.Cleanup(func() { _ = os.Setenv(name, prev) })
		}
	}
}

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
// OBS_* env var is set, correlation IDs are ON and every other capability is
// OFF, with the auth-failure counter following metrics (OFF).
func TestObservability_FlagDefaults(t *testing.T) {
	clearObsEnv(t)

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

// TestObservability_BadBoolFallsBackToDefaultAndWarns proves a set-but-
// unparseable OBS_* value falls back to the documented default (not Go's zero
// value) and is not silent: envBool emits a slog.Warn naming the var. We force a
// garbage value on a default-true flag (correlation) and a default-false flag
// (metrics); each must keep its default and produce a warning.
func TestObservability_BadBoolFallsBackToDefaultAndWarns(t *testing.T) {
	clearObsEnv(t)
	t.Setenv(envObsCorrelationIDEnabled, "yebbut") // default true
	t.Setenv(envObsMetricsEnabled, "maybe")        // default false

	buf := captureSlog(t)

	cfg, err := LoadConfig(writeTemp(t, obsYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !cfg.Observability.CorrelationIDEnabled {
		t.Error("unparseable OBS_CORRELATION_ID_ENABLED should fall back to default true")
	}
	if cfg.Observability.MetricsEnabled {
		t.Error("unparseable OBS_METRICS_ENABLED should fall back to default false")
	}

	logged := buf.String()
	if !strings.Contains(logged, envObsCorrelationIDEnabled) {
		t.Errorf("expected a warning naming %s; log was: %s", envObsCorrelationIDEnabled, logged)
	}
	if !strings.Contains(logged, envObsMetricsEnabled) {
		t.Errorf("expected a warning naming %s; log was: %s", envObsMetricsEnabled, logged)
	}
}

// TestObservability_NumericDefaults pins the numeric tunable defaults applied
// when the observability block is omitted from YAML.
func TestObservability_NumericDefaults(t *testing.T) {
	clearObsEnv(t)

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
	if o.ShutdownDrainDelayS != defaults.DefaultShutdownDrainDelayS {
		t.Errorf("ShutdownDrainDelayS = %d, want %d", o.ShutdownDrainDelayS, defaults.DefaultShutdownDrainDelayS)
	}
	if o.MetricsBindAddress != defaults.DefaultMetricsBindAddress {
		t.Errorf("MetricsBindAddress = %q, want %q", o.MetricsBindAddress, defaults.DefaultMetricsBindAddress)
	}
	if o.ServiceName != defaults.DefaultServiceName {
		t.Errorf("ServiceName = %q, want %q", o.ServiceName, defaults.DefaultServiceName)
	}
	if o.DeploymentEnvironment != "" {
		t.Errorf("DeploymentEnvironment = %q, want empty (omitted, not defaulted)", o.DeploymentEnvironment)
	}
	if o.CloudRegion != "" {
		t.Errorf("CloudRegion = %q, want empty (omitted, not defaulted)", o.CloudRegion)
	}
}

// TestObservability_NumericFromYAML proves the numeric tunables parse from the
// YAML observability block (overriding the defaults) and that ${VAR}
// substitution reaches them — the substitution runs over raw bytes before
// decode, so an int field gets it for free.
func TestObservability_NumericFromYAML(t *testing.T) {
	t.Setenv("OTEL_INTERVAL", "120")
	t.Setenv("METRICS_ADDR", "0.0.0.0:9099")

	yamlBody := obsYAML + `
observability:
  saturation_threshold_ms: 25
  progress_signal_threshold_ms: 8000
  otel_self_stats_interval_s: ${OTEL_INTERVAL}
  shutdown_drain_delay_s: 7
  metrics_bind_address: ${METRICS_ADDR}
  service_name: my-mcp
  deployment_environment: production
  cloud_region: us-east-1
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
	if o.ShutdownDrainDelayS != 7 {
		t.Errorf("ShutdownDrainDelayS = %d, want 7", o.ShutdownDrainDelayS)
	}
	if o.MetricsBindAddress != "0.0.0.0:9099" {
		t.Errorf("MetricsBindAddress = %q, want 0.0.0.0:9099 (from ${METRICS_ADDR})", o.MetricsBindAddress)
	}
	if o.ServiceName != "my-mcp" {
		t.Errorf("ServiceName = %q, want %q", o.ServiceName, "my-mcp")
	}
	if o.DeploymentEnvironment != "production" {
		t.Errorf("DeploymentEnvironment = %q, want %q", o.DeploymentEnvironment, "production")
	}
	if o.CloudRegion != "us-east-1" {
		t.Errorf("CloudRegion = %q, want %q", o.CloudRegion, "us-east-1")
	}
}

// TestObservability_NonPositiveNumericsAreReDefaulted proves a stray
// non-positive value in YAML (e.g. -1) is treated as "omitted" and re-defaulted
// rather than surviving as a nonsensical tunable. None of these fields has a
// meaningful value <= 0.
func TestObservability_NonPositiveNumericsAreReDefaulted(t *testing.T) {
	yamlBody := obsYAML + `
observability:
  saturation_threshold_ms: -1
  progress_signal_threshold_ms: 0
  otel_self_stats_interval_s: -42
  shutdown_drain_delay_s: -3
`
	cfg, err := LoadConfig(writeTemp(t, yamlBody))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	o := cfg.Observability
	if o.SaturationThresholdMs != defaults.DefaultSaturationThresholdMs {
		t.Errorf("SaturationThresholdMs = %d, want default %d (negative re-defaulted)", o.SaturationThresholdMs, defaults.DefaultSaturationThresholdMs)
	}
	if o.ProgressSignalThresholdMs != defaults.DefaultProgressSignalThresholdMs {
		t.Errorf("ProgressSignalThresholdMs = %d, want default %d (zero re-defaulted)", o.ProgressSignalThresholdMs, defaults.DefaultProgressSignalThresholdMs)
	}
	if o.OTelSelfStatsIntervalS != defaults.DefaultOTelSelfStatsIntervalS {
		t.Errorf("OTelSelfStatsIntervalS = %d, want default %d (negative re-defaulted)", o.OTelSelfStatsIntervalS, defaults.DefaultOTelSelfStatsIntervalS)
	}
	if o.ShutdownDrainDelayS != defaults.DefaultShutdownDrainDelayS {
		t.Errorf("ShutdownDrainDelayS = %d, want default %d (negative re-defaulted)", o.ShutdownDrainDelayS, defaults.DefaultShutdownDrainDelayS)
	}
}
