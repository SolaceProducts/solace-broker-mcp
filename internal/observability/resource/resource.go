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
	"strings"

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

// identityKeys are the four attributes this package owns outright. Membership
// has one consequence, in baseResource: the merge base is stripped of these
// keys, so the precedence chain in New is the only thing that can set them.
//
// Without that strip, a value the chain deliberately rejected can still reach
// the resource through the back door. OTEL_RESOURCE_ATTRIBUTES=cloud.region=
// (an unset ${REGION} in a manifest) is the realistic case: the SDK's own env
// detector emits attribute.String("cloud.region", "") for it, New treats the
// empty value as absent and contributes no attribute, and with no collision
// for Merge to resolve the empty value survives unopposed — onto target_info
// and, before the guard in SlogAttrs, onto every log line (SOL-154727).
//
// Only the two optional attributes can actually leak that way: New always
// re-adds service.name and service.instance.id with a non-empty value, so
// those two always win the collision. They are listed anyway, so the rule is
// "this package owns these four keys" rather than four separate cases resting
// on which attribute happens to be unconditional today.
var identityKeys = map[attribute.Key]bool{
	semconv.ServiceNameKey:       true,
	semconv.ServiceInstanceIDKey: true,
	deploymentEnvironmentNameKey: true,
	semconv.CloudRegionKey:       true,
}

// defaultResource is sdkresource.Default, indirected so tests can substitute
// a base that carries what Default() carries in production.
//
// The seam is not cosmetic. Default() memoizes behind a sync.Once, so a test
// binary gets exactly one environment's worth of it; resource_test.go's
// TestMain must therefore prime it against a cleared environment for the
// omitted-vs-present assertions to be deterministic. The cost of that, until
// this seam existed, was that no test in this package could observe the
// Default()-carries-env path that production always takes — which is why the
// empty-attribute leak above went unnoticed through a release (SOL-154727).
var defaultResource = sdkresource.Default

// baseResource returns the resource New merges its own attributes over: the
// SDK default (for its telemetry.sdk.* attributes) with the four identity
// keys removed. See identityKeys for why they are removed.
func baseResource() *sdkresource.Resource {
	def := defaultResource()
	attrs := def.Attributes()
	kept := make([]attribute.KeyValue, 0, len(attrs))
	for _, kv := range attrs {
		if !identityKeys[kv.Key] {
			kept = append(kept, kv)
		}
	}
	return sdkresource.NewWithAttributes(def.SchemaURL(), kept...)
}

// Source names the step of the precedence chain that supplied a value.
type Source string

// The precedence-chain steps, in the order New tries them. Not every chain
// uses every step: only service.instance.id can reach SourcePodName or
// SourceHostname, and only the two optional attributes can end at
// SourceUnset.
const (
	SourceConfig   Source = "config"
	SourceEnv      Source = "env"
	SourcePodName  Source = "pod_name"
	SourceHostname Source = "hostname"
	SourceDefault  Source = "default"
	SourceUnset    Source = "unset"
)

// Resolution is one identity attribute's resolved value and the step that
// supplied it. Value is empty exactly when Source is SourceUnset, which for
// deployment.environment.name and cloud.region means the attribute is
// omitted from the resource entirely.
type Resolution struct {
	Value  string
	Source Source
}

// Identity records how each of the four identity attributes resolved.
// Returned alongside the resource so cmd/server can report it at startup:
// nothing else announces which of the competing inputs won, and since
// SOL-154608 an OTEL_RESOURCE_ATTRIBUTES service.instance.id outranks the
// downward-API pod name — so a shared, non-per-pod injection silently
// collapses every replica onto one instance id. service.instance.id is also
// deliberately excluded from log attributes (see SlogAttrs), leaving no way
// to tie a log line back to the pod that produced it. One startup line
// naming the values and their sources makes both visible at deploy time
// (SOL-154727).
type Identity struct {
	ServiceName           Resolution
	ServiceInstanceID     Resolution
	DeploymentEnvironment Resolution
	CloudRegion           Resolution
}

// LogAttrs renders the resolution for the startup identity line, one
// value/source pair per attribute.
//
// The keys are the YAML field names (service_name, not service.name) for two
// reasons: they name the field an operator edits to change the answer, and
// the dotted attribute keys are already bound to every log line by
// SlogAttrs, so reusing them here would emit each one twice on this line.
//
// The two optional attributes are reported even when unset, as an empty
// value paired with source "unset" — on this line, unlike on the resource,
// "not configured" is the answer being reported rather than a value
// masquerading as one.
//
// Lives here rather than at the call site so that adding a fifth identity
// attribute means updating the struct and this method together, instead of
// leaving a startup line that quietly reports four of five.
func (i Identity) LogAttrs() []slog.Attr {
	return []slog.Attr{
		slog.String("service_name", i.ServiceName.Value),
		slog.String("service_name_source", string(i.ServiceName.Source)),
		slog.String("service_instance_id", i.ServiceInstanceID.Value),
		slog.String("service_instance_id_source", string(i.ServiceInstanceID.Source)),
		slog.String("deployment_environment", i.DeploymentEnvironment.Value),
		slog.String("deployment_environment_source", string(i.DeploymentEnvironment.Source)),
		slog.String("cloud_region", i.CloudRegion.Value),
		slog.String("cloud_region_source", string(i.CloudRegion.Source)),
	}
}

// New builds the shared resource.Resource from cfg's identity fields plus
// serviceVersion (passed in, so this package has no dependency on
// internal/version), and reports how each identity attribute resolved.
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
// Merged with baseResource() for its telemetry.sdk.* attributes; Merge's
// second argument wins collisions. Resolving service.name and
// service.instance.id to a non-empty value unconditionally is what keeps the
// SDK's own guesses out: its "unknown_service:<binary>" placeholder, and the
// random UUID it generates for service.instance.id under the experimental
// OTEL_GO_X_RESOURCE flag.
func New(cfg config.ObservabilityConfig, serviceVersion string) (*sdkresource.Resource, Identity, error) {
	// Trim the config side so both sources agree on what counts as a value.
	// The SDK already trims the environment side (sdk/resource/env.go), so
	// before this an operator whose ${VAR} expanded to spaces got a
	// whitespace-only service.name from YAML but the built-in default from
	// OTEL_SERVICE_NAME — one input, two answers, and the YAML one is worse
	// than the default because it looks configured. cfg is a value copy, so
	// these assignments are local to New (SOL-154727).
	cfg.ServiceName = strings.TrimSpace(cfg.ServiceName)
	cfg.ServiceInstanceID = strings.TrimSpace(cfg.ServiceInstanceID)
	cfg.DeploymentEnvironment = strings.TrimSpace(cfg.DeploymentEnvironment)
	cfg.CloudRegion = strings.TrimSpace(cfg.CloudRegion)

	env := sdkresource.Environment()

	identity := Identity{
		ServiceName:       serviceName(cfg, env),
		ServiceInstanceID: instanceID(cfg, env),
		DeploymentEnvironment: resolve(
			candidate{cfg.DeploymentEnvironment, SourceConfig},
			candidate{envAttr(env, deploymentEnvironmentNameKey), SourceEnv},
		),
		CloudRegion: resolve(
			candidate{cfg.CloudRegion, SourceConfig},
			candidate{envAttr(env, semconv.CloudRegionKey), SourceEnv},
		),
	}

	attrs := []attribute.KeyValue{
		semconv.ServiceName(identity.ServiceName.Value),
		semconv.ServiceVersion(serviceVersion),
		semconv.ServiceInstanceID(identity.ServiceInstanceID.Value),
	}
	if identity.DeploymentEnvironment.Source != SourceUnset {
		attrs = append(attrs, deploymentEnvironmentNameKey.String(identity.DeploymentEnvironment.Value))
	}
	if identity.CloudRegion.Source != SourceUnset {
		attrs = append(attrs, semconv.CloudRegion(identity.CloudRegion.Value))
	}

	// base.SchemaURL(), not the pinned semconv.SchemaURL: Merge fails with
	// ErrSchemaURLConflict when the two resources' schema URLs differ and
	// neither is empty (flagged by review). Building this resource's schema
	// URL from the SDK's own default removes the implicit "this pin must
	// track the SDK's internal semconv version" coupling entirely, rather
	// than relying on it happening to match today.
	base := baseResource()
	res, err := sdkresource.Merge(
		base,
		sdkresource.NewWithAttributes(base.SchemaURL(), attrs...),
	)
	if err != nil {
		return nil, Identity{}, err
	}
	return res, identity, nil
}

// envAttr returns the value env (sdkresource.Environment()) carries for key,
// or "" when absent.
//
// Trimmed for the same reason the config values are, and for one case the
// SDK's own trim misses: it trims the raw OTEL_RESOURCE_ATTRIBUTES value
// before percent-decoding it, so cloud.region=%20 decodes to a space
// afterwards and arrives here untrimmed.
func envAttr(env *sdkresource.Resource, key attribute.Key) string {
	for _, kv := range env.Attributes() {
		if kv.Key == key {
			return strings.TrimSpace(kv.Value.AsString())
		}
	}
	return ""
}

// candidate is one step of a precedence chain: the value that step offers
// (empty when it has nothing to offer) and the name reported for it.
type candidate struct {
	value  string
	source Source
}

// resolve returns the first candidate offering a non-empty value, or a
// SourceUnset zero Resolution when none does. Callers pass the chain in
// priority order, so each precedence chain reads off its own call site.
func resolve(chain ...candidate) Resolution {
	for _, c := range chain {
		if c.value != "" {
			return Resolution{Value: c.value, Source: c.source}
		}
	}
	return Resolution{Source: SourceUnset}
}

// serviceName resolves service.name. Config no longer defaults this field —
// applyObservabilityDefaults leaves it empty so the env var has a gap to fall
// into (SOL-154608) — so the default here is the only one.
func serviceName(cfg config.ObservabilityConfig, env *sdkresource.Resource) Resolution {
	return resolve(
		candidate{cfg.ServiceName, SourceConfig},
		candidate{envAttr(env, semconv.ServiceNameKey), SourceEnv},
		candidate{defaults.DefaultServiceName, SourceDefault},
	)
}

// instanceID resolves service.instance.id: the config override (the FD's
// "config, or the pod name" commitment, for topologies where neither pod name
// nor hostname identifies the instance — e.g. bare-metal instances sharing a
// hostname), else an OTEL_RESOURCE_ATTRIBUTES entry, else the downward-API pod
// name (deploy/kubernetes/deployment.yaml), else the hostname. Always returns
// something rather than propagating an os.Hostname error: an empty id
// collapses every instance into one series, and identity is best-effort, not
// worth failing startup over.
//
// POD_NAME and the hostname are not trimmed, unlike the config and env
// values above: POD_NAME carries a Kubernetes object name and the hostname a
// DNS label, neither of which can contain whitespace. Trimming them would be
// code for an input that cannot occur.
func instanceID(cfg config.ObservabilityConfig, env *sdkresource.Resource) Resolution {
	var hostname string
	if host, err := os.Hostname(); err == nil {
		hostname = host
	}
	return resolve(
		candidate{cfg.ServiceInstanceID, SourceConfig},
		candidate{envAttr(env, semconv.ServiceInstanceIDKey), SourceEnv},
		candidate{os.Getenv("POD_NAME"), SourcePodName},
		candidate{hostname, SourceHostname},
		candidate{"unknown", SourceDefault},
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
			// Empty values are dropped, not logged as "". A resource from
			// New cannot carry one — baseResource strips these keys and New
			// re-adds them only when non-empty — but cmd/server falls back
			// to a bare sdkresource.Default() when New fails, and that is
			// exactly the resource whose env detector emits cloud.region=""
			// for OTEL_RESOURCE_ATTRIBUTES=cloud.region= (SOL-154727).
			if v := kv.Value.AsString(); v != "" {
				out = append(out, slog.String(string(kv.Key), v))
			}
		}
	}
	return out
}
