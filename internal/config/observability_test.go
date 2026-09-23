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
		envObsMetricsScrapeEnabled,
		envObsAuditLogEnabled,
		envObsTracingEnabled,
		envObsSaturationEventsEnabled,
		envObsAuthFailureCounterEnabled,
		envObsMetricsOTLPEnabled,
		envObsMetricsEnabledRetired,
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
		{"MetricsScrapeEnabled", o.MetricsScrapeEnabled, false},
		{"AuditLogEnabled", o.AuditLogEnabled, false},
		{"TracingEnabled", o.TracingEnabled, false},
		{"SaturationEventsEnabled", o.SaturationEventsEnabled, false},
		{"AuthFailureCounterEnabled", o.AuthFailureCounterEnabled, false},
		{"MetricsOTLPEnabled", o.MetricsOTLPEnabled, false},
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
	t.Setenv("OBS_METRICS_SCRAPE_ENABLED", "true")

	cfg, err := LoadConfig(writeTemp(t, obsYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Observability.CorrelationIDEnabled {
		t.Error("OBS_CORRELATION_ID_ENABLED=false should turn correlation OFF")
	}
	if !cfg.Observability.MetricsScrapeEnabled {
		t.Error("OBS_METRICS_SCRAPE_ENABLED=true should turn metrics ON")
	}
}

// TestObservability_OTLPOnly_LoadsWithoutScrape pins SOL-154607's headline:
// OBS_METRICS_OTLP_ENABLED no longer requires OBS_METRICS_SCRAPE_ENABLED.
// Before the split this combination failed config load (the OTLP reader had
// no provider to attach to, because the one flag also gated the provider);
// now it is the configuration an OTLP-native shop runs, and it must load with
// the scrape egress — and so the scrape listener — off.
func TestObservability_OTLPOnly_LoadsWithoutScrape(t *testing.T) {
	clearObsEnv(t)
	t.Setenv(envObsMetricsOTLPEnabled, "true")

	cfg, err := LoadConfig(writeTemp(t, obsYAML))
	if err != nil {
		t.Fatalf("OTLP-only must load, got: %v", err)
	}
	if !cfg.Observability.MetricsOTLPEnabled {
		t.Error("OBS_METRICS_OTLP_ENABLED=true should turn the OTLP egress ON")
	}
	if cfg.Observability.MetricsScrapeEnabled {
		t.Error("OBS_METRICS_SCRAPE_ENABLED unset must leave the scrape egress OFF")
	}
}

// TestObservability_AuthFailureCounter_FollowsProviderWhenUnset proves the
// follow behavior across every flag combination: with its own var unset, the
// auth-failure counter tracks MetricsProviderEnabled — on whenever either
// egress is on. The OTLP-only row is the one SOL-154607 exists for: under the
// old single-parent rule it resolved to "counters off" and silently lost both
// security counters, and a missing counter is indistinguishable from one
// reading zero.
func TestObservability_AuthFailureCounter_FollowsProviderWhenUnset(t *testing.T) {
	boolEnv := map[bool]string{true: "true", false: "false"}
	tests := []struct {
		name         string
		scrape, otlp bool
		wantProvider bool
	}{
		{"neither egress", false, false, false},
		{"scrape only", true, false, true},
		{"OTLP only", false, true, true},
		{"both", true, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearObsEnv(t)
			t.Setenv(envObsMetricsScrapeEnabled, boolEnv[tc.scrape])
			t.Setenv(envObsMetricsOTLPEnabled, boolEnv[tc.otlp])

			cfg, err := LoadConfig(writeTemp(t, obsYAML))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := cfg.Observability.MetricsProviderEnabled(); got != tc.wantProvider {
				t.Errorf("MetricsProviderEnabled() = %v, want %v", got, tc.wantProvider)
			}
			if got := cfg.Observability.AuthFailureCounterEnabled; got != tc.wantProvider {
				t.Errorf("AuthFailureCounterEnabled = %v, want %v (follows the provider when its own var is unset)", got, tc.wantProvider)
			}
		})
	}
}

// TestObservability_RetiredMetricsFlag_WarnsAndIsIgnored pins SOL-154607's
// no-alias rename: OBS_METRICS_ENABLED is not read, so setting it turns
// nothing on — and because a deployment that still sets it would otherwise
// get metrics silently off, config load says so once, naming the var and its
// replacement, so the operator has one line to act on. Absent, nothing is
// logged about it.
func TestObservability_RetiredMetricsFlag_WarnsAndIsIgnored(t *testing.T) {
	t.Run("set: ignored and warned", func(t *testing.T) {
		clearObsEnv(t)
		t.Setenv(envObsMetricsEnabledRetired, "true")
		buf := captureSlog(t)

		cfg, err := LoadConfig(writeTemp(t, obsYAML))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Observability.MetricsScrapeEnabled || cfg.Observability.MetricsOTLPEnabled {
			t.Error("the retired OBS_METRICS_ENABLED must not turn any egress on (no alias)")
		}
		logged := buf.String()
		if !strings.Contains(logged, envObsMetricsEnabledRetired) || !strings.Contains(logged, envObsMetricsScrapeEnabled) {
			t.Errorf("expected a warning naming %s and its replacement %s; log was: %s",
				envObsMetricsEnabledRetired, envObsMetricsScrapeEnabled, logged)
		}
	})

	t.Run("unset: silent", func(t *testing.T) {
		clearObsEnv(t)
		buf := captureSlog(t)

		if _, err := LoadConfig(writeTemp(t, obsYAML)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if strings.Contains(buf.String(), envObsMetricsEnabledRetired) {
			t.Errorf("no warning expected when the retired var is unset; log was: %s", buf.String())
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
		t.Setenv("OBS_METRICS_SCRAPE_ENABLED", "false")
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
		t.Setenv("OBS_METRICS_SCRAPE_ENABLED", "true")
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
	t.Setenv(envObsMetricsScrapeEnabled, "maybe")  // default false

	buf := captureSlog(t)

	cfg, err := LoadConfig(writeTemp(t, obsYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !cfg.Observability.CorrelationIDEnabled {
		t.Error("unparseable OBS_CORRELATION_ID_ENABLED should fall back to default true")
	}
	if cfg.Observability.MetricsScrapeEnabled {
		t.Error("unparseable OBS_METRICS_SCRAPE_ENABLED should fall back to default false")
	}

	logged := buf.String()
	if !strings.Contains(logged, envObsCorrelationIDEnabled) {
		t.Errorf("expected a warning naming %s; log was: %s", envObsCorrelationIDEnabled, logged)
	}
	if !strings.Contains(logged, envObsMetricsScrapeEnabled) {
		t.Errorf("expected a warning naming %s; log was: %s", envObsMetricsScrapeEnabled, logged)
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
	// All four identity fields stay empty at this layer (SOL-154608): a value
	// defaulted here is indistinguishable from one the operator wrote, leaving
	// OTEL_SERVICE_NAME no gap to fall into. The "solace-broker-mcp" default
	// still exists, at the end of internal/observability/resource's chain.
	if o.ServiceName != "" {
		t.Errorf("ServiceName = %q, want empty (resolved in internal/observability/resource, not defaulted here)", o.ServiceName)
	}
	if o.ServiceInstanceID != "" {
		t.Errorf("ServiceInstanceID = %q, want empty (omitted, not defaulted)", o.ServiceInstanceID)
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
  service_instance_id: my-instance-1
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
	// The only surface where the yaml:"service_instance_id" tag itself is
	// exercised — every other test sets ServiceInstanceID directly on a
	// struct literal, so a typo'd or renamed tag would compile and pass
	// everywhere else while silently dropping an operator's override
	// (flagged by review).
	if o.ServiceInstanceID != "my-instance-1" {
		t.Errorf("ServiceInstanceID = %q, want %q", o.ServiceInstanceID, "my-instance-1")
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

// TestObservability_ServiceInstanceIDFromPodNameSubstitution pins the exact
// migration docs/observability.md and the SOL-154608 CHANGELOG hand to
// operators. TestObservability_NumericFromYAML already covers ${VAR}
// generally; this pins the documented incantation, so it fails a build rather
// than someone's cluster.
func TestObservability_ServiceInstanceIDFromPodNameSubstitution(t *testing.T) {
	clearObsEnv(t)
	t.Setenv("POD_NAME", "solace-broker-mcp-7d8f9-abcde")

	cfg, err := LoadConfig(writeTemp(t, obsYAML+`
observability:
  service_instance_id: "${POD_NAME}"
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Observability.ServiceInstanceID; got != "solace-broker-mcp-7d8f9-abcde" {
		t.Errorf("ServiceInstanceID = %q, want the substituted POD_NAME; the documented "+
			"`service_instance_id: \"${POD_NAME}\"` migration no longer works", got)
	}
}
