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

	"github.com/SolaceProducts/solace-broker-mcp/internal/defaults"
)

// ObservabilityConfig holds the feature flags and tunables for the
// observability capabilities (correlation IDs, metrics, audit log, tracing,
// saturation events, auth-failure counter). The flags are loaded and surfaced
// here; correlation IDs are now wired into the request path (SOL-151279), while
// metrics, audit, tracing, and saturation remain surfaced-only until their
// stories consume them. (Panic recovery is unconditional and is no longer a
// flag on this struct.)
//
// Two distinct loading channels, deliberately split:
//
//   - Capability flags (the bool fields) come from OBS_* environment
//     variables, applied in applyObservabilityEnv. Env vars (not YAML) keep
//     the operator's on/off switches in one obvious place and let a deployment
//     toggle a capability without editing the mounted config file. The v1
//     "door-closing" defaults ship correlation ON and everything else OFF.
//
//   - Numeric tunables (the int fields, YAML-tagged) parse from the YAML
//     config like every other field, so they inherit the existing ${VAR}
//     substitution for free (substituteEnvVars runs over the raw bytes before
//     decode). Defaults are applied in applyDefaults alongside the rest.
type ObservabilityConfig struct {
	// Capability flags. Loaded from OBS_* env vars in applyObservabilityEnv,
	// not from YAML. The yaml:"-" tags are intentional: they exclude these
	// fields from YAML decoding so env vars stay the single source for flags.
	CorrelationIDEnabled    bool `yaml:"-"`
	MetricsEnabled          bool `yaml:"-"`
	AuditLogEnabled         bool `yaml:"-"`
	TracingEnabled          bool `yaml:"-"`
	SaturationEventsEnabled bool `yaml:"-"`
	// AuthFailureCounterEnabled follows MetricsEnabled unless its own env var
	// (OBS_AUTH_FAILURE_COUNTER_ENABLED) is explicitly set. The auth-failure
	// counter is a metric, so it makes no sense to emit it while metrics are
	// off — but an operator can still force it independently if they set the
	// var directly.
	AuthFailureCounterEnabled bool `yaml:"-"`

	// YAML tunables. Parsed from YAML (inheriting ${VAR} substitution);
	// defaults applied in applyDefaults.
	SaturationThresholdMs     int `yaml:"saturation_threshold_ms"`
	ProgressSignalThresholdMs int `yaml:"progress_signal_threshold_ms"`
	OTelSelfStatsIntervalS    int `yaml:"otel_self_stats_interval_s"`
	// MetricsBindAddress is the address the Prometheus /metrics listener binds
	// to (e.g. ":9091" or "0.0.0.0:9091"). Defaulted to DefaultMetricsBindAddress when empty.
	MetricsBindAddress string `yaml:"metrics_bind_address"`
	// ShutdownDrainDelayS is the propagation window, in seconds, the server
	// waits after flipping /readyz to 503 on SIGTERM before it begins graceful
	// HTTP shutdown. It gives the orchestrator time to deregister the pod from
	// its endpoint set so no new traffic is routed to a draining pod (SOL-151288).
	// Defaulted to DefaultShutdownDrainDelayS; a non-positive value re-defaults.
	ShutdownDrainDelayS int `yaml:"shutdown_drain_delay_s"`

	// Identity fields (SOL-152425, Story 34): wired into the single OTel
	// resource.Resource shared by the meter provider (Story 14) and the
	// tracer provider (Story 25), and into the default slog attributes. See
	// internal/observability/resource. ServiceName defaults to
	// "solace-broker-mcp" when empty. ServiceInstanceID falls back to the
	// Kubernetes-downward-API pod name, then the process hostname, when
	// empty — an explicit override for the (uncommon) case where neither
	// identifies the instance usefully (e.g. bare-metal instances that share
	// a hostname). DeploymentEnvironment and CloudRegion are omitted from the
	// resource (and from logs) when empty — there is no meaningful default
	// for either, and an empty value is not the same question as "not
	// configured".
	ServiceName           string `yaml:"service_name"`
	ServiceInstanceID     string `yaml:"service_instance_id"`
	DeploymentEnvironment string `yaml:"deployment_environment"`
	CloudRegion           string `yaml:"cloud_region"`
}

// Observability env var names. Capability on/off switches; the v1 defaults
// follow the "door-closing" policy — correlation IDs on, everything else off
// until an operator opts in.
const (
	envObsCorrelationIDEnabled      = "OBS_CORRELATION_ID_ENABLED"
	envObsMetricsEnabled            = "OBS_METRICS_ENABLED"
	envObsAuditLogEnabled           = "OBS_AUDIT_LOG_ENABLED"
	envObsTracingEnabled            = "OBS_TRACING_ENABLED"
	envObsSaturationEventsEnabled   = "OBS_SATURATION_EVENTS_ENABLED"
	envObsAuthFailureCounterEnabled = "OBS_AUTH_FAILURE_COUNTER_ENABLED"
)

// applyObservabilityEnv populates the capability flags on cfg from the OBS_*
// environment variables, using the v1 "door-closing" defaults. Called from
// applyEnvOverrides so it runs in the same phase as the other env-driven
// overrides (MCP_SERVER_PORT). Numeric tunables are NOT touched here — those
// come from YAML and are defaulted in applyDefaults.
func applyObservabilityEnv(cfg *ServerConfig) {
	o := &cfg.Observability

	o.CorrelationIDEnabled = envBool(envObsCorrelationIDEnabled, true, "observability")
	o.MetricsEnabled = envBool(envObsMetricsEnabled, false, "observability")
	o.AuditLogEnabled = envBool(envObsAuditLogEnabled, false, "observability")
	o.TracingEnabled = envBool(envObsTracingEnabled, false, "observability")
	o.SaturationEventsEnabled = envBool(envObsSaturationEventsEnabled, false, "observability")

	// Auth-failure counter follows metrics unless its own var is explicitly
	// set. LookupEnv distinguishes "unset" (follow metrics) from "set to
	// false" (operator forced it off even though metrics are on).
	if _, ok := os.LookupEnv(envObsAuthFailureCounterEnabled); ok {
		// Inside this branch the var IS set, so envBool parses the operator's
		// explicit value; the o.MetricsEnabled default is unreachable here (it
		// would only apply if the value were unparseable) — the follow-metrics
		// behavior lives entirely in the else branch below.
		o.AuthFailureCounterEnabled = envBool(envObsAuthFailureCounterEnabled, o.MetricsEnabled, "observability")
	} else {
		o.AuthFailureCounterEnabled = o.MetricsEnabled
	}
}

// applyObservabilityDefaults fills the numeric tunables that the operator left
// at zero with their defaults. Called from applyDefaults so it sits beside the
// SEMP/port/log-level defaulting. A non-positive value (zero, or a stray
// negative coming from YAML) means "omitted" for these fields: none has a
// meaningful value <= 0 (a 0ms or negative saturation threshold, or a 0s/
// negative self-stats interval, is nonsensical). Re-defaulting rather than
// letting such a value survive prevents propagating a nonsensical tunable to
// the capabilities that will later consume it.
func applyObservabilityDefaults(cfg *ServerConfig) {
	o := &cfg.Observability
	if o.SaturationThresholdMs <= 0 {
		o.SaturationThresholdMs = defaults.DefaultSaturationThresholdMs
	}
	if o.ProgressSignalThresholdMs <= 0 {
		o.ProgressSignalThresholdMs = defaults.DefaultProgressSignalThresholdMs
	}
	if o.OTelSelfStatsIntervalS <= 0 {
		o.OTelSelfStatsIntervalS = defaults.DefaultOTelSelfStatsIntervalS
	}
	if o.ShutdownDrainDelayS <= 0 {
		o.ShutdownDrainDelayS = defaults.DefaultShutdownDrainDelayS
	}
	if o.MetricsBindAddress == "" {
		o.MetricsBindAddress = defaults.DefaultMetricsBindAddress
	}
	if o.ServiceName == "" {
		o.ServiceName = defaults.DefaultServiceName
	}
	// DeploymentEnvironment and CloudRegion are deliberately NOT defaulted:
	// empty means "omit this attribute", not "use a placeholder value".
}
