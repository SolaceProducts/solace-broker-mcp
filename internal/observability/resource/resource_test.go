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

package resource

import (
	"os"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
)

// TestMain clears the ambient OTEL_* vars, then primes sdkresource.Default()
// against that cleared environment. Both halves matter: Default() caches
// behind a sync.Once, so clearing alone leaves the first New call — whichever
// test that is, and under -shuffle that varies — baking its own env into the
// cache and breaking the omitted-vs-present assertions below. The precedence
// tests are unaffected either way: New reads sdkresource.Environment(), which
// is not memoized.
func TestMain(m *testing.M) {
	os.Unsetenv("OTEL_RESOURCE_ATTRIBUTES")
	os.Unsetenv("OTEL_SERVICE_NAME")
	sdkresource.Default()
	os.Exit(m.Run())
}

// findAttr returns the string value of key on res, failing the test if
// key isn't present.
func findAttr(t *testing.T, res *sdkresource.Resource, key attribute.Key) string {
	t.Helper()
	for _, kv := range res.Attributes() {
		if kv.Key == key {
			return kv.Value.AsString()
		}
	}
	t.Fatalf("resource has no %q attribute (attributes: %v)", key, res.Attributes())
	return ""
}

// hasAttrKey reports whether res carries key at all.
func hasAttrKey(res *sdkresource.Resource, key attribute.Key) bool {
	for _, kv := range res.Attributes() {
		if kv.Key == key {
			return true
		}
	}
	return false
}

// TestNew_ServiceName_DefaultWhenNeitherSourceSet pins step 3 for
// service.name. Since SOL-154608 this is the only place that default is
// applied, so it is a production path, not the defense-in-depth case it was.
// It also pins that Default()'s "unknown_service:<binary>" placeholder never
// reaches the merged resource — an implementation that just omitted the
// attribute when both sources were empty would export
// "unknown_service:resource.test" here.
func TestNew_ServiceName_DefaultWhenNeitherSourceSet(t *testing.T) {
	res, _, err := New(config.ObservabilityConfig{}, "v1.2.3")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got := findAttr(t, res, "service.name"); got != "solace-broker-mcp" {
		t.Errorf("service.name = %q, want %q", got, "solace-broker-mcp")
	}
	if got := findAttr(t, res, "service.version"); got != "v1.2.3" {
		t.Errorf("service.version = %q, want %q", got, "v1.2.3")
	}
}

// TestNew_ConfiguredServiceName pins step 1 for service.name: an explicit
// YAML field overrides the built-in default.
func TestNew_ConfiguredServiceName(t *testing.T) {
	res, _, err := New(config.ObservabilityConfig{ServiceName: "my-mcp"}, "v1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got := findAttr(t, res, "service.name"); got != "my-mcp" {
		t.Errorf("service.name = %q, want %q", got, "my-mcp")
	}
}

// TestNew_InstanceID_ConfigTakesPriorityOverPodName pins that
// cfg.ServiceInstanceID — the explicit operator override for a deployment
// topology where neither the pod name nor the hostname identifies the
// instance usefully (e.g. bare-metal instances sharing a hostname) — wins
// over POD_NAME when both are set.
func TestNew_InstanceID_ConfigTakesPriorityOverPodName(t *testing.T) {
	t.Setenv("POD_NAME", "pod-name-should-lose")

	res, _, err := New(config.ObservabilityConfig{ServiceInstanceID: "explicit-instance-id"}, "v1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got := findAttr(t, res, "service.instance.id"); got != "explicit-instance-id" {
		t.Errorf("service.instance.id = %q, want the configured override", got)
	}
}

// TestNew_InstanceID_PodNameTakesPriorityOverHostname pins that POD_NAME (the
// Kubernetes downward-API env var — see deploy/kubernetes/deployment.yaml)
// wins when set and no config override is given, regardless of what the
// process's hostname happens to be.
func TestNew_InstanceID_PodNameTakesPriorityOverHostname(t *testing.T) {
	t.Setenv("POD_NAME", "solace-broker-mcp-7d8f9-abcde")

	res, _, err := New(config.ObservabilityConfig{}, "v1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got := findAttr(t, res, "service.instance.id"); got != "solace-broker-mcp-7d8f9-abcde" {
		t.Errorf("service.instance.id = %q, want the POD_NAME value", got)
	}
}

// TestNew_InstanceID_FallsBackToHostname pins that an unset POD_NAME (a
// non-Kubernetes deployment) still produces a non-empty instance id — an
// empty one would collapse every instance into one series in an aggregator,
// exactly the failure this story exists to prevent.
func TestNew_InstanceID_FallsBackToHostname(t *testing.T) {
	t.Setenv("POD_NAME", "")

	res, _, err := New(config.ObservabilityConfig{}, "v1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got := findAttr(t, res, "service.instance.id"); got == "" {
		t.Error("service.instance.id is empty; want a non-empty fallback (hostname or \"unknown\")")
	}
}

// TestNew_OptionalAttributes_OmittedWhenUnconfigured pins that
// deployment.environment.name and cloud.region are absent — not present with
// an empty string — when unconfigured, per the FD's "omitted, not defaulted"
// commitment (also asserted by SlogAttrs's own test below, since an absent
// resource attribute must also be absent from logs).
func TestNew_OptionalAttributes_OmittedWhenUnconfigured(t *testing.T) {
	res, _, err := New(config.ObservabilityConfig{}, "v1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if hasAttrKey(res, deploymentEnvironmentNameKey) {
		t.Error("deployment.environment.name is present with no configured value")
	}
	if hasAttrKey(res, "cloud.region") {
		t.Error("cloud.region is present with no configured value")
	}
}

// TestNew_OptionalAttributes_PresentWhenConfigured pins that both optional
// attributes appear, under the current semconv key
// (deployment.environment.name, not the FD's now-superseded
// deployment.environment — see this package's doc comment), when configured.
func TestNew_OptionalAttributes_PresentWhenConfigured(t *testing.T) {
	res, _, err := New(config.ObservabilityConfig{
		DeploymentEnvironment: "production",
		CloudRegion:           "us-east-1",
	}, "v1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got := findAttr(t, res, "deployment.environment.name"); got != "production" {
		t.Errorf("deployment.environment.name = %q, want %q", got, "production")
	}
	if got := findAttr(t, res, "cloud.region"); got != "us-east-1" {
		t.Errorf("cloud.region = %q, want %q", got, "us-east-1")
	}
}

// TestSlogAttrs_IncludesOnlyTheCommittedLogSubset pins the FD's exact
// commitment: "Every log line includes service.name and, when configured,
// deployment.environment and cloud.region" — service.version and
// service.instance.id, both present on the resource, must NOT leak onto log
// lines; nothing here commits logs to carrying them.
func TestSlogAttrs_IncludesOnlyTheCommittedLogSubset(t *testing.T) {
	res, _, err := New(config.ObservabilityConfig{
		ServiceName:           "my-mcp",
		DeploymentEnvironment: "staging",
		CloudRegion:           "eu-west-1",
	}, "v9.9.9")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	attrs := SlogAttrs(res)
	got := map[string]string{}
	for _, a := range attrs {
		got[a.Key] = a.Value.String()
	}

	want := map[string]string{
		"service.name":                "my-mcp",
		"deployment.environment.name": "staging",
		"cloud.region":                "eu-west-1",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("SlogAttrs()[%q] = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("SlogAttrs() returned %d attrs (%v), want exactly %d (service.version/service.instance.id must not leak onto logs)",
			len(got), got, len(want))
	}
}

// TestSlogAttrs_OmitsUnconfiguredOptionalAttrs pins that SlogAttrs mirrors
// New's own omission of unconfigured optional attributes — a log line must
// not gain a "deployment.environment.name": "" attribute either.
func TestSlogAttrs_OmitsUnconfiguredOptionalAttrs(t *testing.T) {
	res, _, err := New(config.ObservabilityConfig{}, "v1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	attrs := SlogAttrs(res)
	if len(attrs) != 1 {
		t.Fatalf("SlogAttrs() = %v, want exactly one attribute (service.name only)", attrs)
	}
	if attrs[0].Key != "service.name" {
		t.Errorf("SlogAttrs()[0].Key = %q, want %q", attrs[0].Key, "service.name")
	}
}

// --- Standard OTel environment variables (SOL-154608) ---
//
// The tests above cover step 1 (YAML set) and step 3 (neither set); these
// cover step 2 and the step interactions the old implementation got wrong.

// identityAttrs drives the precedence matrix. One table rather than
// per-attribute tests: the asymmetry SOL-154608 fixes (two attributes honored
// the standard vars, two silently did not) could otherwise come back one
// attribute at a time.
var identityAttrs = []struct {
	key     attribute.Key
	setYAML func(*config.ObservabilityConfig, string)
}{
	{key: "service.name", setYAML: func(o *config.ObservabilityConfig, v string) { o.ServiceName = v }},
	{key: "service.instance.id", setYAML: func(o *config.ObservabilityConfig, v string) { o.ServiceInstanceID = v }},
	{key: deploymentEnvironmentNameKey, setYAML: func(o *config.ObservabilityConfig, v string) { o.DeploymentEnvironment = v }},
	{key: "cloud.region", setYAML: func(o *config.ObservabilityConfig, v string) { o.CloudRegion = v }},
}

// TestNew_EnvHonouredWhenYAMLUnset pins step 2 for all four attributes.
//
// Against the pre-SOL-154608 implementation this fails all FOUR rows, not the
// two that were broken in production: the optional two were honored only via
// Default(), which TestMain primes clean, so that route was never observable
// from a test — which is why the asymmetry survived a release.
//
// POD_NAME is set throughout so the service.instance.id row also proves the
// env var outranks the pod-name fallback, not merely that it is read.
func TestNew_EnvHonouredWhenYAMLUnset(t *testing.T) {
	for _, a := range identityAttrs {
		t.Run(string(a.key), func(t *testing.T) {
			t.Setenv("POD_NAME", "pod-name-should-lose")
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", string(a.key)+"=from-env")

			res, _, err := New(config.ObservabilityConfig{}, "v1")
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if got := findAttr(t, res, a.key); got != "from-env" {
				t.Errorf("%s = %q, want %q from OTEL_RESOURCE_ATTRIBUTES", a.key, got, "from-env")
			}
		})
	}
}

// TestNew_YAMLWinsOverEnv pins step 1 over step 2 for all four attributes —
// the half of the contract that must NOT change, so a deployment relying on
// the YAML field keeps working across this story.
func TestNew_YAMLWinsOverEnv(t *testing.T) {
	for _, a := range identityAttrs {
		t.Run(string(a.key), func(t *testing.T) {
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", string(a.key)+"=from-env")

			var cfg config.ObservabilityConfig
			a.setYAML(&cfg, "from-yaml")

			res, _, err := New(cfg, "v1")
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if got := findAttr(t, res, a.key); got != "from-yaml" {
				t.Errorf("%s = %q, want the YAML value to win", a.key, got)
			}
		})
	}
}

// TestNew_ServiceName_OTelServiceNameVar pins the dedicated variable — the
// one the OpenTelemetry Operator injects — not just the
// OTEL_RESOURCE_ATTRIBUTES route the matrix uses.
func TestNew_ServiceName_OTelServiceNameVar(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "from-otel-service-name")

	res, _, err := New(config.ObservabilityConfig{}, "v1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got := findAttr(t, res, "service.name"); got != "from-otel-service-name" {
		t.Errorf("service.name = %q, want the OTEL_SERVICE_NAME value", got)
	}
}

// TestNew_ServiceName_OTelServiceNameBeatsResourceAttributes pins the spec's
// ordering between the two env routes. New gets this free by reading
// sdkresource.Environment(); the test catches a future rewrite that parses
// the variables by hand and drops it.
func TestNew_ServiceName_OTelServiceNameBeatsResourceAttributes(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "dedicated-var")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.name=resource-attributes-var")

	res, _, err := New(config.ObservabilityConfig{}, "v1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got := findAttr(t, res, "service.name"); got != "dedicated-var" {
		t.Errorf("service.name = %q, want OTEL_SERVICE_NAME to outrank the OTEL_RESOURCE_ATTRIBUTES entry", got)
	}
}

// TestNew_ServiceName_YAMLWinsOverOTelServiceNameVar pins "ignored when the
// YAML field is set" against the dedicated variable specifically.
func TestNew_ServiceName_YAMLWinsOverOTelServiceNameVar(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "from-env")

	res, _, err := New(config.ObservabilityConfig{ServiceName: "from-yaml"}, "v1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got := findAttr(t, res, "service.name"); got != "from-yaml" {
		t.Errorf("service.name = %q, want the YAML value to win", got)
	}
}

// TestNew_OptionalAttributes_StillOmittedWithUnrelatedEnvVar pins that an
// envAttr lookup returning "" is treated as absent, so New contributes no
// attribute.
//
// Narrow on purpose: this covers what New contributes. That the merged
// resource also omits an empty-valued attribute the SDK's own detector put
// on the merge base is a separate claim, covered by
// TestNew_EmptyEnvEntryDoesNotReachResource via the defaultResource seam.
func TestNew_OptionalAttributes_StillOmittedWithUnrelatedEnvVar(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "some.other.attribute=value")

	res, _, err := New(config.ObservabilityConfig{}, "v1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if hasAttrKey(res, deploymentEnvironmentNameKey) {
		t.Error("deployment.environment.name is present with neither source set")
	}
	if hasAttrKey(res, "cloud.region") {
		t.Error("cloud.region is present with neither source set")
	}
}

// TestSlogAttrs_ReflectsEnvResolvedIdentity pins that logs follow the same
// precedence as the resource: an operator setting OTEL_SERVICE_NAME must not
// see one service.name on metrics and another in logs.
func TestSlogAttrs_ReflectsEnvResolvedIdentity(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "env-named-service")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment.name=env-staging,cloud.region=env-west")

	res, _, err := New(config.ObservabilityConfig{}, "v1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	got := map[string]string{}
	for _, a := range SlogAttrs(res) {
		got[a.Key] = a.Value.String()
	}
	want := map[string]string{
		"service.name":                "env-named-service",
		"deployment.environment.name": "env-staging",
		"cloud.region":                "env-west",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("SlogAttrs()[%q] = %q, want %q", k, got[k], v)
		}
	}
}

// TestNew_MalformedResourceAttributesDoNotFailStartup: entries that parse are
// honored, the rest dropped with an SDK diagnostic, no error. A stray comma in
// a platform-injected variable must not be why a pod fails to start.
func TestNew_MalformedResourceAttributesDoNotFailStartup(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "no-equals-sign,service.name=still-parsed")

	res, _, err := New(config.ObservabilityConfig{}, "v1")
	if err != nil {
		t.Fatalf("New() error = %v; malformed OTEL_RESOURCE_ATTRIBUTES must not fail construction", err)
	}
	if got := findAttr(t, res, "service.name"); got != "still-parsed" {
		t.Errorf("service.name = %q, want the entry that parsed cleanly", got)
	}
}

// TestNew_EmptyYAMLFieldCountsAsUnset pins that an explicitly empty YAML field
// behaves as unset for all four attributes, so step 2 still runs. The
// zero-value config in TestNew_EnvHonouredWhenYAMLUnset reaches the same code
// path but cannot express the intent: this is the contract docs/observability.md
// states ("an empty field counts as unset, not as pin empty"), and writing ""
// is what an operator does when a ${VAR} expands to nothing.
func TestNew_EmptyYAMLFieldCountsAsUnset(t *testing.T) {
	for _, a := range identityAttrs {
		t.Run(string(a.key), func(t *testing.T) {
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", string(a.key)+"=from-env")

			var cfg config.ObservabilityConfig
			a.setYAML(&cfg, "")

			res, _, err := New(cfg, "v1")
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if got := findAttr(t, res, a.key); got != "from-env" {
				t.Errorf("%s = %q, want the env value; an empty YAML field must count as unset", a.key, got)
			}
		})
	}
}

// --- Value hygiene and resolution reporting (SOL-154727) ---

// withBaseResource substitutes the resource New merges its own attributes
// over, restoring the real one when the test ends.
//
// This is the only way to exercise the Default()-carries-env path from inside
// this package: Default() memoizes behind a sync.Once and TestMain must prime
// it against a cleared environment for the omitted-vs-present assertions
// above to be deterministic, so no amount of t.Setenv can make the real
// Default() carry an attribute here. Production always takes that path, which
// is how the leak below survived a release. See defaultResource.
func withBaseResource(t *testing.T, attrs ...attribute.KeyValue) {
	t.Helper()
	prev := defaultResource
	t.Cleanup(func() { defaultResource = prev })
	defaultResource = func() *sdkresource.Resource {
		return sdkresource.NewWithAttributes(prev().SchemaURL(), attrs...)
	}
}

// TestNew_EmptyEnvEntryDoesNotReachResource reproduces the production path
// for OTEL_RESOURCE_ATTRIBUTES=cloud.region= — an unset ${REGION} in a
// manifest — on both halves at once: the env var is set, so New's own chain
// sees the empty value, AND the merge base carries what the SDK's env
// detector really emits for it, attribute.String("cloud.region", "").
//
// Before the identityKeys strip in baseResource the two optional rows failed:
// New treated the empty value as absent and contributed nothing, so there was
// no collision for Merge to resolve and the base's empty value survived onto
// target_info and every log line. The two required rows passed even then,
// because New always re-adds them with a non-empty value and so always won
// the collision — they are in identityKeys, and in this matrix, so the rule
// stays one sentence ("this package owns these four keys") instead of four
// cases resting on which attribute happens to be unconditional today.
func TestNew_EmptyEnvEntryDoesNotReachResource(t *testing.T) {
	for _, a := range identityAttrs {
		t.Run(string(a.key), func(t *testing.T) {
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", string(a.key)+"=")
			withBaseResource(t, attribute.String(string(a.key), ""))

			res, identity, err := New(config.ObservabilityConfig{}, "v1")
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			// The two required attributes fall through to their built-in
			// default; the two optional ones are omitted outright. Neither
			// may be present with an empty value, and no log line may carry
			// one.
			for _, kv := range res.Attributes() {
				if kv.Key == a.key && kv.Value.AsString() == "" {
					t.Errorf("%s is present with an empty value; an empty env entry must count as unset", a.key)
				}
			}
			for _, got := range SlogAttrs(res) {
				if got.Key == string(a.key) && got.Value.String() == "" {
					t.Errorf("SlogAttrs() carries %s=\"\"; an empty value must not reach log lines", a.key)
				}
			}
			if src := sourceFor(t, identity, a.key); src == SourceEnv {
				t.Errorf("%s resolved with source %q; an empty env entry must not count as the winning source", a.key, src)
			}
		})
	}
}

// TestSlogAttrs_DropsEmptyValues covers the one live path that hands
// SlogAttrs a resource New did not build: cmd/server falls back to a bare
// sdkresource.Default() when New returns an error, and that is exactly the
// resource whose env detector emits the empty attributes above. The strip in
// baseResource cannot help there, so SlogAttrs guards independently.
func TestSlogAttrs_DropsEmptyValues(t *testing.T) {
	res := sdkresource.NewSchemaless(
		attribute.String("service.name", "real-service"),
		attribute.String("deployment.environment.name", ""),
		attribute.String("cloud.region", ""),
	)

	attrs := SlogAttrs(res)
	if len(attrs) != 1 {
		t.Fatalf("SlogAttrs() = %v, want only the non-empty service.name", attrs)
	}
	if attrs[0].Key != "service.name" || attrs[0].Value.String() != "real-service" {
		t.Errorf("SlogAttrs()[0] = %v, want service.name=real-service", attrs[0])
	}
}

// TestNew_WhitespaceOnlyYAMLFieldCountsAsUnset pins the config side of the
// trim for all four attributes: a ${VAR} that expanded to spaces must behave
// like an unset field, so the env var still has a gap to fall into.
//
// Before the trim in New the YAML value won and exported "   " —
// indistinguishable from a configured value to everything downstream, and
// worse than the default precisely because it looks deliberate.
func TestNew_WhitespaceOnlyYAMLFieldCountsAsUnset(t *testing.T) {
	for _, a := range identityAttrs {
		t.Run(string(a.key), func(t *testing.T) {
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", string(a.key)+"=from-env")

			var cfg config.ObservabilityConfig
			a.setYAML(&cfg, "   ")

			res, identity, err := New(cfg, "v1")
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if got := findAttr(t, res, a.key); got != "from-env" {
				t.Errorf("%s = %q, want the env value; a whitespace-only YAML field must count as unset", a.key, got)
			}
			if src := sourceFor(t, identity, a.key); src != SourceEnv {
				t.Errorf("%s source = %q, want %q", a.key, src, SourceEnv)
			}
		})
	}
}

// TestNew_WhitespaceOnlyYAMLFieldOmitsOptionalAttributes is the other half of
// the trim for the two optional attributes: with no env entry to fall
// through to, a whitespace-only field must leave the attribute absent, not
// present-and-blank.
func TestNew_WhitespaceOnlyYAMLFieldOmitsOptionalAttributes(t *testing.T) {
	res, identity, err := New(config.ObservabilityConfig{
		DeploymentEnvironment: "  ",
		CloudRegion:           "\t\n",
	}, "v1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if hasAttrKey(res, deploymentEnvironmentNameKey) {
		t.Error("deployment.environment.name is present for a whitespace-only field")
	}
	if hasAttrKey(res, "cloud.region") {
		t.Error("cloud.region is present for a whitespace-only field")
	}
	if identity.DeploymentEnvironment.Source != SourceUnset {
		t.Errorf("deployment.environment.name source = %q, want %q", identity.DeploymentEnvironment.Source, SourceUnset)
	}
	if identity.CloudRegion.Source != SourceUnset {
		t.Errorf("cloud.region source = %q, want %q", identity.CloudRegion.Source, SourceUnset)
	}
}

// TestNew_SurroundingWhitespaceIsTrimmed pins that the trim is a trim, not
// just an is-blank test: a value with padding is used, without it.
func TestNew_SurroundingWhitespaceIsTrimmed(t *testing.T) {
	res, _, err := New(config.ObservabilityConfig{ServiceName: "  my-mcp  "}, "v1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got := findAttr(t, res, "service.name"); got != "my-mcp" {
		t.Errorf("service.name = %q, want %q", got, "my-mcp")
	}
}

// TestNew_WhitespaceOnlyOTelServiceNameVarCountsAsUnset pins the SDK
// behaviour the config-side trim was added to match. It is the reference
// half of the asymmetry: if a future SDK stopped trimming
// OTEL_SERVICE_NAME, the two sources would disagree again and this test —
// not a production incident — is what says so.
func TestNew_WhitespaceOnlyOTelServiceNameVarCountsAsUnset(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "   ")

	res, identity, err := New(config.ObservabilityConfig{}, "v1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got := findAttr(t, res, "service.name"); got != "solace-broker-mcp" {
		t.Errorf("service.name = %q, want the built-in default", got)
	}
	if identity.ServiceName.Source != SourceDefault {
		t.Errorf("service.name source = %q, want %q", identity.ServiceName.Source, SourceDefault)
	}
}

// TestNew_PercentEncodedWhitespaceEnvValueCountsAsUnset covers the one
// whitespace case the SDK's own trim misses: it trims the raw
// OTEL_RESOURCE_ATTRIBUTES value before percent-decoding it, so %20 decodes
// to a space afterwards and arrives untrimmed. envAttr trims again for this.
func TestNew_PercentEncodedWhitespaceEnvValueCountsAsUnset(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "cloud.region=%20%20")

	res, identity, err := New(config.ObservabilityConfig{}, "v1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if hasAttrKey(res, "cloud.region") {
		t.Errorf("cloud.region = %q, want the attribute omitted", findAttr(t, res, "cloud.region"))
	}
	if identity.CloudRegion.Source != SourceUnset {
		t.Errorf("cloud.region source = %q, want %q", identity.CloudRegion.Source, SourceUnset)
	}
}

// sourceFor reads the Resolution for key off identity, keeping the
// identityAttrs matrix usable for the Identity assertions too.
func sourceFor(t *testing.T, identity Identity, key attribute.Key) Source {
	t.Helper()
	switch key {
	case "service.name":
		return identity.ServiceName.Source
	case "service.instance.id":
		return identity.ServiceInstanceID.Source
	case deploymentEnvironmentNameKey:
		return identity.DeploymentEnvironment.Source
	case "cloud.region":
		return identity.CloudRegion.Source
	}
	t.Fatalf("sourceFor: unhandled key %q", key)
	return ""
}

// TestNew_IdentityReportsConfigSource pins source reporting for step 1 on all
// four attributes.
func TestNew_IdentityReportsConfigSource(t *testing.T) {
	for _, a := range identityAttrs {
		t.Run(string(a.key), func(t *testing.T) {
			var cfg config.ObservabilityConfig
			a.setYAML(&cfg, "from-yaml")

			_, identity, err := New(cfg, "v1")
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if src := sourceFor(t, identity, a.key); src != SourceConfig {
				t.Errorf("%s source = %q, want %q", a.key, src, SourceConfig)
			}
		})
	}
}

// TestNew_IdentityReportsEnvSource pins source reporting for step 2 on all
// four attributes. POD_NAME is set so the service.instance.id row proves the
// reported source is the one that actually won, not merely the first
// non-empty step after config.
func TestNew_IdentityReportsEnvSource(t *testing.T) {
	for _, a := range identityAttrs {
		t.Run(string(a.key), func(t *testing.T) {
			t.Setenv("POD_NAME", "pod-name-should-lose")
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", string(a.key)+"=from-env")

			_, identity, err := New(config.ObservabilityConfig{}, "v1")
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if src := sourceFor(t, identity, a.key); src != SourceEnv {
				t.Errorf("%s source = %q, want %q", a.key, src, SourceEnv)
			}
		})
	}
}

// TestNew_IdentityReportsInstanceIDFallbackSources pins the two steps unique
// to service.instance.id. The pod_name row is the one the startup line exists
// for: it is what an operator compares against, so that a service.instance.id
// reading "env" on every replica of a Deployment is visibly wrong.
func TestNew_IdentityReportsInstanceIDFallbackSources(t *testing.T) {
	t.Run("pod_name", func(t *testing.T) {
		t.Setenv("POD_NAME", "solace-broker-mcp-7d8f9-abcde")

		_, identity, err := New(config.ObservabilityConfig{}, "v1")
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		if identity.ServiceInstanceID.Source != SourcePodName {
			t.Errorf("service.instance.id source = %q, want %q", identity.ServiceInstanceID.Source, SourcePodName)
		}
		if identity.ServiceInstanceID.Value != "solace-broker-mcp-7d8f9-abcde" {
			t.Errorf("service.instance.id = %q, want the POD_NAME value", identity.ServiceInstanceID.Value)
		}
	})

	t.Run("hostname", func(t *testing.T) {
		t.Setenv("POD_NAME", "")

		_, identity, err := New(config.ObservabilityConfig{}, "v1")
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		// SourceDefault ("unknown") only when os.Hostname() fails, which
		// cannot be forced here; either way the value must be non-empty and
		// the source must say which it was.
		switch identity.ServiceInstanceID.Source {
		case SourceHostname, SourceDefault:
		default:
			t.Errorf("service.instance.id source = %q, want %q or %q",
				identity.ServiceInstanceID.Source, SourceHostname, SourceDefault)
		}
		if identity.ServiceInstanceID.Value == "" {
			t.Error("service.instance.id is empty; want a non-empty fallback")
		}
	})
}

// TestNew_IdentityValuesMatchTheResource pins that the reported identity and
// the exported resource cannot drift: a startup line naming values the
// resource does not carry would be worse than no line at all.
func TestNew_IdentityValuesMatchTheResource(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "cloud.region=env-west")

	res, identity, err := New(config.ObservabilityConfig{
		ServiceName:           "my-mcp",
		ServiceInstanceID:     "instance-7",
		DeploymentEnvironment: "production",
	}, "v1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	want := map[attribute.Key]Resolution{
		"service.name":               identity.ServiceName,
		"service.instance.id":        identity.ServiceInstanceID,
		deploymentEnvironmentNameKey: identity.DeploymentEnvironment,
		"cloud.region":               identity.CloudRegion,
	}
	for key, got := range want {
		if found := findAttr(t, res, key); found != got.Value {
			t.Errorf("resource %s = %q, but Identity reports %q (source %q)", key, found, got.Value, got.Source)
		}
	}
}

// TestIdentity_LogAttrs pins the startup line's key naming and, more
// usefully, that every field maps to its own key pair. Each of the eight
// strings below is distinct, so a copy-paste swap between two attributes —
// invisible in review, and the kind of bug that makes an operator chase the
// wrong field — fails here.
func TestIdentity_LogAttrs(t *testing.T) {
	identity := Identity{
		ServiceName:           Resolution{Value: "name-value", Source: "name-source"},
		ServiceInstanceID:     Resolution{Value: "instance-value", Source: "instance-source"},
		DeploymentEnvironment: Resolution{Value: "env-value", Source: "env-source"},
		CloudRegion:           Resolution{Value: "region-value", Source: "region-source"},
	}

	got := map[string]string{}
	for _, a := range identity.LogAttrs() {
		if _, dup := got[a.Key]; dup {
			t.Errorf("LogAttrs() emits key %q twice", a.Key)
		}
		got[a.Key] = a.Value.String()
	}

	want := map[string]string{
		"service_name":                  "name-value",
		"service_name_source":           "name-source",
		"service_instance_id":           "instance-value",
		"service_instance_id_source":    "instance-source",
		"deployment_environment":        "env-value",
		"deployment_environment_source": "env-source",
		"cloud_region":                  "region-value",
		"cloud_region_source":           "region-source",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("LogAttrs()[%q] = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("LogAttrs() emitted %d keys (%v), want exactly %d", len(got), got, len(want))
	}
}

// TestIdentity_LogAttrs_ReportsUnsetOptionalAttributes pins that an omitted
// optional attribute still appears on the line, as an empty value under
// source "unset". A missing key would read as "the server did not consider
// cloud.region", which is a different statement from "nothing configured it".
func TestIdentity_LogAttrs_ReportsUnsetOptionalAttributes(t *testing.T) {
	_, identity, err := New(config.ObservabilityConfig{}, "v1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	got := map[string]string{}
	for _, a := range identity.LogAttrs() {
		got[a.Key] = a.Value.String()
	}
	for _, field := range []string{"deployment_environment", "cloud_region"} {
		if v, ok := got[field]; !ok || v != "" {
			t.Errorf("LogAttrs()[%q] = %q (present=%v), want an empty value", field, v, ok)
		}
		if got[field+"_source"] != string(SourceUnset) {
			t.Errorf("LogAttrs()[%q] = %q, want %q", field+"_source", got[field+"_source"], SourceUnset)
		}
	}
}

// TestNew_PreservesSDKDefaultAttributesAndSchemaURL guards the merge base
// itself. Merging with the SDK default is what supplies telemetry.sdk.*, and
// the schema URL is taken from that same default so Merge cannot fail with
// ErrSchemaURLConflict — both are load-bearing, and both would vanish
// silently if baseResource's rebuild (SOL-154727) ever returned an empty or
// schemaless resource. Every other test in this file would still pass.
func TestNew_PreservesSDKDefaultAttributesAndSchemaURL(t *testing.T) {
	def := sdkresource.Default()

	res, _, err := New(config.ObservabilityConfig{}, "v1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	for _, key := range []attribute.Key{"telemetry.sdk.language", "telemetry.sdk.name", "telemetry.sdk.version"} {
		want := findAttr(t, def, key)
		if got := findAttr(t, res, key); got != want {
			t.Errorf("%s = %q, want %q from sdkresource.Default()", key, got, want)
		}
	}
	if got := res.SchemaURL(); got != def.SchemaURL() {
		t.Errorf("SchemaURL() = %q, want %q (sdkresource.Default()'s)", got, def.SchemaURL())
	}
}

// TestBaseResource_StripsOnlyTheIdentityKeys pins that the strip is a filter,
// not a blanket wipe: a non-identity attribute the SDK's env detector puts on
// the default — host.name is the realistic one, and OTEL_RESOURCE_ATTRIBUTES
// can carry any key at all — must survive into the merged resource untouched,
// including with an empty value, which this package has no business editing.
func TestBaseResource_StripsOnlyTheIdentityKeys(t *testing.T) {
	withBaseResource(t,
		attribute.String("cloud.region", "stripped-identity-key"),
		attribute.String("host.name", "kept-non-identity-key"),
		attribute.String("some.other.attribute", ""),
	)

	res, _, err := New(config.ObservabilityConfig{}, "v1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got := findAttr(t, res, "host.name"); got != "kept-non-identity-key" {
		t.Errorf("host.name = %q, want the base value kept", got)
	}
	if !hasAttrKey(res, "some.other.attribute") {
		t.Error("some.other.attribute was dropped; only the four identity keys may be stripped")
	}
	if hasAttrKey(res, "cloud.region") {
		t.Errorf("cloud.region = %q, want it stripped from the base", findAttr(t, res, "cloud.region"))
	}
}
