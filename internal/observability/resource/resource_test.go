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

// TestMain clears the ambient OTEL_RESOURCE_ATTRIBUTES / OTEL_SERVICE_NAME
// env vars before any test in this package runs. sdkresource.Default()
// memoizes its result behind a sync.Once, so whichever value it reads from
// these vars on its first call in the process is permanent — t.Setenv on an
// individual test cannot undo it. Without this, a developer's or CI runner's
// ambient environment could make the omitted-vs-present assertions below
// (TestNew_OptionalAttributes_OmittedWhenUnconfigured,
// TestSlogAttrs_OmitsUnconfiguredOptionalAttrs) fail nondeterministically.
func TestMain(m *testing.M) {
	os.Unsetenv("OTEL_RESOURCE_ATTRIBUTES")
	os.Unsetenv("OTEL_SERVICE_NAME")
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

// TestNew_DefaultServiceName pins the fallback when ServiceName is empty —
// exercises New directly against a zero-value ObservabilityConfig, the
// defense-in-depth path serviceName's doc comment describes; production
// config loading has already defaulted this by the time New is called.
func TestNew_DefaultServiceName(t *testing.T) {
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

// TestNew_ConfiguredServiceName pins that an explicit ServiceName overrides
// the default.
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
