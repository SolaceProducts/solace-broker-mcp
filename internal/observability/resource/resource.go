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
// serviceVersion (passed in, so this package has no dependency on
// internal/version).
//
// All four identity attributes resolve the same way (SOL-154608): the YAML
// field, else the standard OTel env var (OTEL_SERVICE_NAME, or an
// OTEL_RESOURCE_ATTRIBUTES entry under the attribute's own key), else the
// built-in default. Before SOL-154608 config always had a value for
// service.name and service.instance.id, so those two always won the merge and
// OTEL_SERVICE_NAME could never take effect — silently, which is why it
// survived a release.
//
// Step 2 reads sdkresource.Environment(), not the vars directly: that applies
// the SDK's own parsing and its OTEL_SERVICE_NAME-over-OTEL_RESOURCE_ATTRIBUTES
// ordering, and unlike Default() it is not memoized, which is what makes the
// precedence testable with t.Setenv. Cost: a malformed OTEL_RESOURCE_ATTRIBUTES
// is reported twice at startup, once here and once by Default()'s detector.
//
// Merged with Default() for its telemetry.sdk.* attributes; Merge's second
// argument wins collisions. Resolving service.name and service.instance.id to
// a non-empty value unconditionally is what keeps Default()'s own guesses out:
// its "unknown_service:<binary>" placeholder, and the random UUID it generates
// for service.instance.id under the experimental OTEL_GO_X_RESOURCE flag.
func New(cfg config.ObservabilityConfig, serviceVersion string) (*sdkresource.Resource, error) {
	env := sdkresource.Environment()

	attrs := []attribute.KeyValue{
		semconv.ServiceName(serviceName(cfg, env)),
		semconv.ServiceVersion(serviceVersion),
		semconv.ServiceInstanceID(instanceID(cfg, env)),
	}
	if v := firstNonEmpty(cfg.DeploymentEnvironment, envAttr(env, deploymentEnvironmentNameKey)); v != "" {
		attrs = append(attrs, deploymentEnvironmentNameKey.String(v))
	}
	if v := firstNonEmpty(cfg.CloudRegion, envAttr(env, semconv.CloudRegionKey)); v != "" {
		attrs = append(attrs, semconv.CloudRegion(v))
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

// envAttr returns the value env (sdkresource.Environment()) carries for key,
// or "" when absent.
func envAttr(env *sdkresource.Resource, key attribute.Key) string {
	for _, kv := range env.Attributes() {
		if kv.Key == key {
			return kv.Value.AsString()
		}
	}
	return ""
}

// firstNonEmpty returns the first non-empty value, or "". Callers pass sources
// in priority order, so each precedence chain reads off its own call site.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// serviceName resolves service.name. Config no longer defaults this field —
// applyObservabilityDefaults leaves it empty so the env var has a gap to fall
// into (SOL-154608) — so the default here is the only one.
func serviceName(cfg config.ObservabilityConfig, env *sdkresource.Resource) string {
	return firstNonEmpty(cfg.ServiceName, envAttr(env, semconv.ServiceNameKey), defaults.DefaultServiceName)
}

// instanceID resolves service.instance.id: the config override (the FD's
// "config, or the pod name" commitment, for topologies where neither pod name
// nor hostname identifies the instance — e.g. bare-metal instances sharing a
// hostname), else an OTEL_RESOURCE_ATTRIBUTES entry, else the downward-API pod
// name (deploy/kubernetes/deployment.yaml), else the hostname. Always returns
// something rather than propagating an os.Hostname error: an empty id
// collapses every instance into one series, and identity is best-effort, not
// worth failing startup over.
func instanceID(cfg config.ObservabilityConfig, env *sdkresource.Resource) string {
	var hostname string
	if host, err := os.Hostname(); err == nil {
		hostname = host
	}
	return firstNonEmpty(
		cfg.ServiceInstanceID,
		envAttr(env, semconv.ServiceInstanceIDKey),
		os.Getenv("POD_NAME"),
		hostname,
		"unknown",
	)
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
