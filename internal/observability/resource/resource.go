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

// Package resource builds the single OTel resource.Resource shared by the
// metrics meter provider (Story 14, SOL-152091) and the tracer provider
// (Story 25, SOL-152420), plus the matching default slog attributes
// (SOL-152425, Story 34). One construction site, so metrics, traces, and
// logs cannot disagree about which instance emitted them — the anti-drift
// guarantee this story exists to provide.
package resource

import (
	"log/slog"
	"os"

	"go.opentelemetry.io/otel/attribute"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/defaults"
)

// deploymentEnvironmentNameKey names the current OTel semantic-convention key
// for the deployment-environment resource attribute. The FD's committed
// attribute table (Story 34, SOL-152425) names the now-superseded
// "deployment.environment"; semconv renamed it to "deployment.environment.name"
// ahead of this story landing, and the SDK's own resource.Default() (which
// this package merges with) is already built against that renamed key. Using
// the FD's literal, now-stale name here would ship an attribute the SDK's own
// semantic-convention package no longer recognizes on day one — the opposite
// of ADR-007's "OTel semantic conventions wherever they apply" — so this
// package follows the current semconv name instead. Disclosed rather than
// silently deviated from the FD text; see the PR description and CHANGELOG.
const deploymentEnvironmentNameKey = semconv.DeploymentEnvironmentNameKey

// New builds the shared resource.Resource from cfg's identity fields plus
// serviceVersion (the build-time version — passed in rather than imported
// directly, so this package has no dependency on internal/version) and the
// instance id (cfg.ServiceInstanceID, else the pod name, else the hostname —
// see instanceID).
//
// Merged with resource.Default() so the SDK's own automatic attributes
// (telemetry.sdk.*, and its own service.name/service.instance.id guesses when
// this package's values were somehow empty) are still present; Merge's
// documented behavior is that the resource passed as the second argument wins
// on any key collision, so the identity fields here always take precedence
// over the SDK's defaults.
//
// Known limitation, disclosed rather than fixed here: because this package's
// values always win the merge, the two attributes cfg always has a real
// value for — service.name (defaulted by config to "solace-broker-mcp") and
// service.instance.id (defaulted here to the pod name or hostname) — can
// never be overridden by the standard OTEL_SERVICE_NAME or an
// OTEL_RESOURCE_ATTRIBUTES service.instance.id entry, unlike
// deployment.environment.name and cloud.region, which cfg leaves empty when
// unconfigured and so DO fall through to whatever resource.Default() detects
// from those same env vars. An operator who wants either standard env var
// honored must currently use this package's own config fields
// (observability.service_name, observability.service_instance_id) instead.
// Unifying the precedence across all five attributes is tracked as a
// follow-up, not attempted in this story.
func New(cfg config.ObservabilityConfig, serviceVersion string) (*sdkresource.Resource, error) {
	attrs := []attribute.KeyValue{
		semconv.ServiceName(serviceName(cfg)),
		semconv.ServiceVersion(serviceVersion),
		semconv.ServiceInstanceID(instanceID(cfg)),
	}
	if cfg.DeploymentEnvironment != "" {
		attrs = append(attrs, deploymentEnvironmentNameKey.String(cfg.DeploymentEnvironment))
	}
	if cfg.CloudRegion != "" {
		attrs = append(attrs, semconv.CloudRegion(cfg.CloudRegion))
	}

	// sdkresource.Default().SchemaURL(), not the pinned semconv.SchemaURL:
	// Merge fails with ErrSchemaURLConflict when the two resources' schema
	// URLs differ and neither is empty (flagged by review). Building this
	// resource's schema URL from the SDK's own default removes the implicit
	// "this pin must track the SDK's internal semconv version" coupling
	// entirely, rather than relying on it happening to match today.
	res, err := sdkresource.Merge(
		sdkresource.Default(),
		sdkresource.NewWithAttributes(sdkresource.Default().SchemaURL(), attrs...),
	)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// serviceName returns cfg's configured service name. Config loading
// (internal/config) already defaults this to defaults.DefaultServiceName
// when empty; the empty-string fallback here is defense in depth for a
// Resource built directly against a zero-value ObservabilityConfig (e.g. in
// a test that skips config loading), not a path production traffic takes.
func serviceName(cfg config.ObservabilityConfig) string {
	if cfg.ServiceName != "" {
		return cfg.ServiceName
	}
	return defaults.DefaultServiceName
}

// instanceID returns, in order: cfg.ServiceInstanceID (an explicit operator
// override — the FD's "config, or the pod name" commitment, for a deployment
// topology where neither the pod name nor the hostname identifies the
// instance usefully, e.g. bare-metal instances sharing a hostname); the pod
// name (set via the Kubernetes downward API — see
// deploy/kubernetes/deployment.yaml); or, when both are unset (bare-metal,
// docker-compose, or a non-Kubernetes deployment with no override
// configured), the process's hostname. An empty instance ID would collapse
// every instance into one series in an aggregator, which is exactly the
// failure this story exists to prevent, so this always returns SOMETHING
// rather than propagating an os.Hostname error into resource construction: a
// resource attribute is best-effort identity, not a value worth failing
// server startup over.
func instanceID(cfg config.ObservabilityConfig) string {
	if cfg.ServiceInstanceID != "" {
		return cfg.ServiceInstanceID
	}
	if pod := os.Getenv("POD_NAME"); pod != "" {
		return pod
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return "unknown"
}

// SlogAttrs returns the subset of res's attributes that belong on every log
// line: service.name always, deployment.environment.name and cloud.region
// when configured. service.version and service.instance.id are deliberately
// excluded — the FD's own commitment ("Every log line includes service.name
// and, when configured, deployment.environment and cloud.region") names only
// these three for logs, not the full resource set metrics and traces carry.
//
// Returns slog.Attr, not attribute.KeyValue: the only caller wires these
// straight into a slog.Handler.WithAttrs call (cmd/server/main.go), and every
// attribute this package puts in the resource is string-valued, so the
// conversion is total.
func SlogAttrs(res *sdkresource.Resource) []slog.Attr {
	var out []slog.Attr
	for _, kv := range res.Attributes() {
		switch kv.Key {
		case semconv.ServiceNameKey, deploymentEnvironmentNameKey, semconv.CloudRegionKey:
			out = append(out, slog.String(string(kv.Key), kv.Value.AsString()))
		}
	}
	return out
}
