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

package main

import (
	"context"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/resource"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/tracing"
)

// TestSharedResource_BothProvidersPreserveTheResourceTheyAreGiven is a
// narrower claim than "main() cannot build the resource twice" — that
// invariant is structural (verified by code review: main() has exactly one
// resource.New call site, and the same res variable reaches both
// startMetricsEndpoint and tracing.New — see main()'s "Shared identity
// resource" section) and isn't something a unit test can couple to without
// either calling main() itself or extracting its wiring into a helper, which
// this story doesn't do. What this test DOES prove, and would catch: that
// neither provider quietly re-merges, copies, or otherwise alters the
// resource.Resource it's handed — passing the exact same value in and
// confirming both report back that exact same value (by identity, and by
// attribute content as a second check).
func TestSharedResource_BothProvidersPreserveTheResourceTheyAreGiven(t *testing.T) {
	res, err := resource.New(config.ObservabilityConfig{
		ServiceName:           "test-service",
		DeploymentEnvironment: "test-env",
		CloudRegion:           "test-region",
	}, "v1.2.3")
	if err != nil {
		t.Fatalf("resource.New() error = %v", err)
	}

	mp, err := metrics.New("v1.2.3", res)
	if err != nil {
		t.Fatalf("metrics.New() error = %v", err)
	}
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	tp, err := tracing.New(config.ObservabilityConfig{TracingEnabled: true, MetricsEnabled: true}, mp.MeterProvider(), res)
	if err != nil {
		t.Fatalf("tracing.New() error = %v", err)
	}
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	if mp.Resource() != res {
		t.Error("metrics.Provider.Resource() is not the exact resource passed to New — it was copied or replaced")
	}
	if tp.Resource() != res {
		t.Error("tracing.Provider.Resource() is not the exact resource passed to New — it was copied or replaced")
	}

	metricsAttrs := mp.Resource().Attributes()
	tracingAttrs := tp.Resource().Attributes()
	if len(metricsAttrs) != len(tracingAttrs) {
		t.Fatalf("metrics resource has %d attributes, tracing resource has %d — want identical sets\nmetrics: %v\ntracing: %v",
			len(metricsAttrs), len(tracingAttrs), metricsAttrs, tracingAttrs)
	}
	metricsSet := make(map[string]string, len(metricsAttrs))
	for _, kv := range metricsAttrs {
		metricsSet[string(kv.Key)] = kv.Value.String()
	}
	for _, kv := range tracingAttrs {
		want, ok := metricsSet[string(kv.Key)]
		if !ok {
			t.Errorf("tracing resource has attribute %q that metrics resource lacks", kv.Key)
			continue
		}
		if got := kv.Value.String(); got != want {
			t.Errorf("attribute %q: metrics = %q, tracing = %q — the two providers disagree", kv.Key, want, got)
		}
	}
}
