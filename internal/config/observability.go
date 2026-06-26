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
	"strconv"

	"github.com/SolaceDev/solace-broker-mcp/internal/defaults"
)

// ObservabilityConfig holds the feature flags and tunables for the
// observability capabilities (correlation IDs, panic recovery, metrics, audit
// log, tracing, saturation events, auth-failure counter). It is a skeleton
// (SOL-151278): the flags are loaded and surfaced, but no behavior is wired
// into the request path yet — later stories consume these values.
//
// Two distinct loading channels, deliberately split:
//
//   - Capability flags (the bool fields) come from OBS_* environment
//     variables, applied in applyObservabilityEnv. Env vars (not YAML) keep
//     the operator's on/off switches in one obvious place and let a deployment
//     toggle a capability without editing the mounted config file. The v1
//     "door-closing" defaults ship correlation + panic recovery ON and
//     everything else OFF.
//
//   - Numeric tunables (the int fields, YAML-tagged) parse from the YAML
//     config like every other field, so they inherit the existing ${VAR}
//     substitution for free (substituteEnvVars runs over the raw bytes before
//     decode). Defaults are applied in applyDefaults alongside the rest.
type ObservabilityConfig struct {
	// Capability flags. Loaded from OBS_* env vars in applyObservabilityEnv,
	// NOT from YAML — hence no yaml tags. See the type doc for why.
	CorrelationIDEnabled    bool `yaml:"-"`
	PanicRecoveryEnabled    bool `yaml:"-"`
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

	// Numeric tunables. Parsed from YAML (inheriting ${VAR} substitution);
	// defaults applied in applyDefaults.
	SaturationThresholdMs     int `yaml:"saturation_threshold_ms"`
	ProgressSignalThresholdMs int `yaml:"progress_signal_threshold_ms"`
	OTelSelfStatsIntervalS    int `yaml:"otel_self_stats_interval_s"`
}

// Observability env var names. Capability on/off switches; the v1 defaults
// follow the "door-closing" policy — correlation IDs and panic recovery on,
// everything else off until an operator opts in.
const (
	envObsCorrelationIDEnabled      = "OBS_CORRELATION_ID_ENABLED"
	envObsPanicRecoveryEnabled      = "OBS_PANIC_RECOVERY_ENABLED"
	envObsMetricsEnabled            = "OBS_METRICS_ENABLED"
	envObsAuditLogEnabled           = "OBS_AUDIT_LOG_ENABLED"
	envObsTracingEnabled            = "OBS_TRACING_ENABLED"
	envObsSaturationEventsEnabled   = "OBS_SATURATION_EVENTS_ENABLED"
	envObsAuthFailureCounterEnabled = "OBS_AUTH_FAILURE_COUNTER_ENABLED"
)

// envBool reads name from the environment and parses it as a boolean. When the
// var is unset (or set to a value strconv.ParseBool rejects), it returns def.
// This keeps an operator typo from silently flipping a capability — a bad value
// falls back to the documented default rather than to Go's zero value.
func envBool(name string, def bool) bool {
	v, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

// applyObservabilityEnv populates the capability flags on cfg from the OBS_*
// environment variables, using the v1 "door-closing" defaults. Called from
// applyEnvOverrides so it runs in the same phase as the other env-driven
// overrides (MCP_SERVER_PORT). Numeric tunables are NOT touched here — those
// come from YAML and are defaulted in applyDefaults.
func applyObservabilityEnv(cfg *ServerConfig) {
	o := &cfg.Observability

	o.CorrelationIDEnabled = envBool(envObsCorrelationIDEnabled, true)
	o.PanicRecoveryEnabled = envBool(envObsPanicRecoveryEnabled, true)
	o.MetricsEnabled = envBool(envObsMetricsEnabled, false)
	o.AuditLogEnabled = envBool(envObsAuditLogEnabled, false)
	o.TracingEnabled = envBool(envObsTracingEnabled, false)
	o.SaturationEventsEnabled = envBool(envObsSaturationEventsEnabled, false)

	// Auth-failure counter follows metrics unless its own var is explicitly
	// set. LookupEnv distinguishes "unset" (follow metrics) from "set to
	// false" (operator forced it off even though metrics are on).
	if _, ok := os.LookupEnv(envObsAuthFailureCounterEnabled); ok {
		// Inside this branch the var IS set, so envBool parses the operator's
		// explicit value; the o.MetricsEnabled default is unreachable here (it
		// would only apply if the value were unparseable) — the follow-metrics
		// behavior lives entirely in the else branch below.
		o.AuthFailureCounterEnabled = envBool(envObsAuthFailureCounterEnabled, o.MetricsEnabled)
	} else {
		o.AuthFailureCounterEnabled = o.MetricsEnabled
	}
}

// applyObservabilityDefaults fills the numeric tunables that the operator left
// at zero with their defaults. Called from applyDefaults so it sits beside the
// SEMP/port/log-level defaulting. Zero means "omitted in YAML" for these
// fields — none of the three has a meaningful zero value (a 0ms saturation
// threshold or 0s self-stats interval would be nonsensical), so zero-as-omitted
// is safe here.
func applyObservabilityDefaults(cfg *ServerConfig) {
	o := &cfg.Observability
	if o.SaturationThresholdMs == 0 {
		o.SaturationThresholdMs = defaults.DefaultSaturationThresholdMs
	}
	if o.ProgressSignalThresholdMs == 0 {
		o.ProgressSignalThresholdMs = defaults.DefaultProgressSignalThresholdMs
	}
	if o.OTelSelfStatsIntervalS == 0 {
		o.OTelSelfStatsIntervalS = defaults.DefaultOTelSelfStatsIntervalS
	}
}
