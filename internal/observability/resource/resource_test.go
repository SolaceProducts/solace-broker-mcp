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
	res, err := New(config.ObservabilityConfig{}, "v1.2.3")
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
	res, err := New(config.ObservabilityConfig{ServiceName: "my-mcp"}, "v1")
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

	res, err := New(config.ObservabilityConfig{ServiceInstanceID: "explicit-instance-id"}, "v1")
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

	res, err := New(config.ObservabilityConfig{}, "v1")
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

	res, err := New(config.ObservabilityConfig{}, "v1")
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
	res, err := New(config.ObservabilityConfig{}, "v1")
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
	res, err := New(config.ObservabilityConfig{
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
	res, err := New(config.ObservabilityConfig{
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
	res, err := New(config.ObservabilityConfig{}, "v1")
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

			res, err := New(config.ObservabilityConfig{}, "v1")
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

			res, err := New(cfg, "v1")
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

	res, err := New(config.ObservabilityConfig{}, "v1")
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

	res, err := New(config.ObservabilityConfig{}, "v1")
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

	res, err := New(config.ObservabilityConfig{ServiceName: "from-yaml"}, "v1")
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
// Narrow on purpose: this covers what New contributes, NOT that the merged
// resource omits empty-valued attributes. With OTEL_RESOURCE_ATTRIBUTES=
// cloud.region= (an unset ${REGION} in a manifest), Default()'s own detector
// emits cloud.region="", which survives unopposed precisely because New
// omitted the key. That predates SOL-154608 and is unchanged by it, and no
// test here can see it: TestMain primes Default() clean.
func TestNew_OptionalAttributes_StillOmittedWithUnrelatedEnvVar(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "some.other.attribute=value")

	res, err := New(config.ObservabilityConfig{}, "v1")
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

	res, err := New(config.ObservabilityConfig{}, "v1")
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

	res, err := New(config.ObservabilityConfig{}, "v1")
	if err != nil {
		t.Fatalf("New() error = %v; malformed OTEL_RESOURCE_ATTRIBUTES must not fail construction", err)
	}
	if got := findAttr(t, res, "service.name"); got != "still-parsed" {
		t.Errorf("service.name = %q, want the entry that parsed cleanly", got)
	}
}
