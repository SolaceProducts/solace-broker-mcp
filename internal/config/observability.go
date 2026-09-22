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
	"log/slog"
	"os"

	"github.com/SolaceProducts/solace-broker-mcp/internal/defaults"
)

// ObservabilityConfig holds the feature flags and tunables for the
// observability capabilities (correlation IDs, metrics, audit log, tracing,
// saturation events, auth-failure counter). Every flag here now gates a real
// consumer: correlation IDs in the request path (SOL-151279), the metrics
// provider and /metrics listener (cmd/server/main.go), the audit log on the
// tool and SEMP paths (internal/tools, internal/semp), the tracer provider,
// and the saturation signal on the broker admission path (internal/semp/pool.go).
// (Panic recovery is unconditional and is no longer a flag on this struct.)
//
// The v1 defaults and the written flip-condition behind each one are recorded
// in docs/observability.md, "Flag Defaults at GA". Change a default there and
// here together, and update the default-assertion table in observability_test.go.
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
	CorrelationIDEnabled bool `yaml:"-"`
	// MetricsScrapeEnabled (OBS_METRICS_SCRAPE_ENABLED) turns on the Prometheus
	// scrape egress: the client_golang registry, the Go/process collectors, the
	// Prometheus exporter, and the /metrics listener on MetricsBindAddress. It
	// is one of the two metrics egress flags (see MetricsProviderEnabled).
	// Before SOL-154607 it was OBS_METRICS_ENABLED and also gated the meter
	// provider the OTLP egress pushes from, so an OTLP-only deployment could
	// not avoid binding an unauthenticated scrape listener nothing read.
	MetricsScrapeEnabled    bool `yaml:"-"`
	AuditLogEnabled         bool `yaml:"-"`
	TracingEnabled          bool `yaml:"-"`
	SaturationEventsEnabled bool `yaml:"-"`
	// MetricsOTLPEnabled (OBS_METRICS_OTLP_ENABLED) turns on the OTLP push
	// egress (SOL-152418, Story 46). Independent of MetricsScrapeEnabled:
	// either flag alone builds the shared meter provider, and with both set
	// the two readers observe one instrument set rather than a second set of
	// them.
	MetricsOTLPEnabled bool `yaml:"-"`
	// AuthFailureCounterEnabled follows MetricsProviderEnabled (either egress
	// flag) unless its own env var (OBS_AUTH_FAILURE_COUNTER_ENABLED) is
	// explicitly set. It gates both security counters, mcp_auth_failure_total
	// and mcp_authz_denied_total (SOL-152099). They are metrics, so it makes
	// no sense to emit them while no egress is on — but an operator can still
	// force the flag independently if they set the var directly.
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
	// internal/observability/resource, which resolves each through one chain:
	// this field, then OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES, then a
	// built-in default (SOL-154608). All four are left empty here when unset,
	// so setting a field overrides the env var and leaving it unset honors it.
	// The defaults, applied only when neither source is set: "solace-broker-mcp";
	// the downward-API pod name then the hostname (ServiceInstanceID is an
	// override for the uncommon case where neither identifies the instance,
	// e.g. bare-metal instances sharing a hostname); and, for the last two,
	// omitting the attribute entirely — empty is not the same question as
	// "not configured".
	ServiceName           string `yaml:"service_name"`
	ServiceInstanceID     string `yaml:"service_instance_id"`
	DeploymentEnvironment string `yaml:"deployment_environment"`
	CloudRegion           string `yaml:"cloud_region"`
}

// MetricsProviderEnabled reports whether a metrics meter provider is built at
// all: true when either egress flag is on. It is the one definition of that
// rule — the auth-failure counter's derived default (applyObservabilityEnv)
// and cmd/server's provider gate (metrics.Enabled) both read it, so the two
// cannot drift. Neither flag set means no provider, no instruments, and no
// listener (SOL-154607).
func (o ObservabilityConfig) MetricsProviderEnabled() bool {
	return o.MetricsScrapeEnabled || o.MetricsOTLPEnabled
}

// Observability env var names. Capability on/off switches; the v1 defaults
// follow the "door-closing" policy — correlation IDs on, everything else off
// until an operator opts in.
const (
	envObsCorrelationIDEnabled      = "OBS_CORRELATION_ID_ENABLED"
	envObsMetricsScrapeEnabled      = "OBS_METRICS_SCRAPE_ENABLED"
	envObsAuditLogEnabled           = "OBS_AUDIT_LOG_ENABLED"
	envObsTracingEnabled            = "OBS_TRACING_ENABLED"
	envObsSaturationEventsEnabled   = "OBS_SATURATION_EVENTS_ENABLED"
	envObsAuthFailureCounterEnabled = "OBS_AUTH_FAILURE_COUNTER_ENABLED"
	envObsMetricsOTLPEnabled        = "OBS_METRICS_OTLP_ENABLED"
)

// envObsMetricsEnabledRetired is the pre-SOL-154607 name of
// envObsMetricsScrapeEnabled. It is never read — the rename is deliberate and
// has no alias, while the config surface is still uncommitted — but a
// deployment that still sets it would otherwise get metrics silently off,
// and a missing counter is indistinguishable from one reading zero.
// applyObservabilityEnv warns when it is present so the operator has one log
// line to act on.
const envObsMetricsEnabledRetired = "OBS_METRICS_ENABLED"

// applyObservabilityEnv populates the capability flags on cfg from the OBS_*
// environment variables, using the v1 "door-closing" defaults. Called from
// applyEnvOverrides so it runs in the same phase as the other env-driven
// overrides (MCP_SERVER_PORT). Numeric tunables are NOT touched here — those
// come from YAML and are defaulted in applyDefaults.
func applyObservabilityEnv(cfg *ServerConfig) {
	o := &cfg.Observability

	o.CorrelationIDEnabled = envBool(envObsCorrelationIDEnabled, true, "observability")
	o.MetricsScrapeEnabled = envBool(envObsMetricsScrapeEnabled, false, "observability")
	o.AuditLogEnabled = envBool(envObsAuditLogEnabled, false, "observability")
	o.TracingEnabled = envBool(envObsTracingEnabled, false, "observability")
	o.SaturationEventsEnabled = envBool(envObsSaturationEventsEnabled, false, "observability")
	o.MetricsOTLPEnabled = envBool(envObsMetricsOTLPEnabled, false, "observability")

	// Names only, never the value: the retired var is not read, so its value
	// has nothing to say, and the operator's fix is the same either way. The
	// message states the consequence and the action because this line is the
	// only thing that distinguishes "renamed and forgotten" from "metrics
	// deliberately off" — docs/observability.md's runbook quotes it verbatim.
	if _, ok := os.LookupEnv(envObsMetricsEnabledRetired); ok {
		slog.Warn("retired observability flag is set and ignored: it enables nothing (no meter provider, no /metrics listener, no security counters); rename it to the replacement",
			slog.String("var", envObsMetricsEnabledRetired),
			slog.String("replacement", envObsMetricsScrapeEnabled))
	}

	// Auth-failure counter follows the metrics provider (either egress flag)
	// unless its own var is explicitly set. LookupEnv distinguishes "unset"
	// (follow) from "set to false" (operator forced it off even though a
	// provider is built).
	if _, ok := os.LookupEnv(envObsAuthFailureCounterEnabled); ok {
		// Inside this branch the var IS set, so envBool parses the operator's
		// explicit value; the MetricsProviderEnabled default is unreachable
		// here (it would only apply if the value were unparseable) — the
		// follow behavior lives entirely in the else branch below.
		o.AuthFailureCounterEnabled = envBool(envObsAuthFailureCounterEnabled, o.MetricsProviderEnabled(), "observability")
	} else {
		o.AuthFailureCounterEnabled = o.MetricsProviderEnabled()
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
	// None of the four identity fields is defaulted here. A value defaulted at
	// this layer is indistinguishable from one the operator wrote, leaving the
	// standard env vars no gap to fall into — exactly why OTEL_SERVICE_NAME
	// never took effect before SOL-154608. internal/observability/resource
	// owns the chain. DeploymentEnvironment and CloudRegion additionally have
	// nothing to default TO: empty means "omit this attribute".
}
