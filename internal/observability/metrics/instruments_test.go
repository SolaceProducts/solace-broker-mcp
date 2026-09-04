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

package metrics

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
)

// TestToolMetrics_RecordSuccessLabels pins the label set on a successful tool
// invocation: exactly one counter series, with error_type empty.
func TestToolMetrics_RecordSuccessLabels(t *testing.T) {
	p, err := New(testVersion, sdkresource.Default())
	if err != nil {
		t.Fatal(err)
	}
	tm, err := p.ToolMetrics()
	if err != nil {
		t.Fatal(err)
	}

	tm.Record(context.Background(), "test-tool", "dev", "success", "", 42*time.Millisecond)

	const want = `
# HELP mcp_tool_invocation_total Number of tool invocations.
# TYPE mcp_tool_invocation_total counter
mcp_tool_invocation_total{broker="dev",error_type="",outcome="success",tool="test-tool"} 1
`
	if err := testutil.GatherAndCompare(p.registry, strings.NewReader(want), "mcp_tool_invocation_total"); err != nil {
		t.Error(err)
	}
}

// TestToolMetrics_HistogramBuckets pins the duration histogram's bucket
// boundaries to exactly the nine the ticket requires. A 42ms sample lands in the
// 0.05s bucket and above, so every bucket from le="0.05" up is cumulative 1.
func TestToolMetrics_HistogramBuckets(t *testing.T) {
	p, err := New(testVersion, sdkresource.Default())
	if err != nil {
		t.Fatal(err)
	}
	tm, err := p.ToolMetrics()
	if err != nil {
		t.Fatal(err)
	}

	tm.Record(context.Background(), "test-tool", "dev", "success", "", 42*time.Millisecond)

	const want = `
# HELP mcp_tool_invocation_duration_seconds Duration of tool invocation in seconds
# TYPE mcp_tool_invocation_duration_seconds histogram
mcp_tool_invocation_duration_seconds_bucket{broker="dev",error_type="",outcome="success",tool="test-tool",le="0.001"} 0
mcp_tool_invocation_duration_seconds_bucket{broker="dev",error_type="",outcome="success",tool="test-tool",le="0.005"} 0
mcp_tool_invocation_duration_seconds_bucket{broker="dev",error_type="",outcome="success",tool="test-tool",le="0.01"} 0
mcp_tool_invocation_duration_seconds_bucket{broker="dev",error_type="",outcome="success",tool="test-tool",le="0.05"} 1
mcp_tool_invocation_duration_seconds_bucket{broker="dev",error_type="",outcome="success",tool="test-tool",le="0.1"} 1
mcp_tool_invocation_duration_seconds_bucket{broker="dev",error_type="",outcome="success",tool="test-tool",le="0.5"} 1
mcp_tool_invocation_duration_seconds_bucket{broker="dev",error_type="",outcome="success",tool="test-tool",le="1"} 1
mcp_tool_invocation_duration_seconds_bucket{broker="dev",error_type="",outcome="success",tool="test-tool",le="5"} 1
mcp_tool_invocation_duration_seconds_bucket{broker="dev",error_type="",outcome="success",tool="test-tool",le="10"} 1
mcp_tool_invocation_duration_seconds_bucket{broker="dev",error_type="",outcome="success",tool="test-tool",le="+Inf"} 1
mcp_tool_invocation_duration_seconds_sum{broker="dev",error_type="",outcome="success",tool="test-tool"} 0.042
mcp_tool_invocation_duration_seconds_count{broker="dev",error_type="",outcome="success",tool="test-tool"} 1
`
	if err := testutil.GatherAndCompare(p.registry, strings.NewReader(want), "mcp_tool_invocation_duration_seconds"); err != nil {
		t.Error(err)
	}
}

// gaugeValue reads the current value of an unlabelled gauge series from the
// provider's registry. Fails the test if the series is missing or not unique.
func gaugeValue(t *testing.T, p *Provider, name string) float64 {
	t.Helper()
	mfs, err := p.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		series := mf.GetMetric()
		if len(series) != 1 {
			t.Fatalf("%s: want 1 series, got %d", name, len(series))
		}
		return series[0].GetGauge().GetValue()
	}
	t.Fatalf("%s not found in gathered metrics", name)
	return 0
}

// TestToolMetrics_ActiveRequestsIncDec verifies the in-flight gauge rises to the
// number of concurrent requests and returns to zero once they finish. A barrier
// holds every request "in flight" until all have incremented, so the mid-point
// read sees the full count and cannot race an early decrement.
func TestToolMetrics_ActiveRequestsIncDec(t *testing.T) {
	p, err := New(testVersion, sdkresource.Default())
	if err != nil {
		t.Fatal(err)
	}
	tm, err := p.ToolMetrics()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	const n = 5
	incremented := make(chan struct{}, n) // each goroutine signals after Inc
	release := make(chan struct{})        // closed to let all goroutines Dec
	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tm.IncActive(ctx)
			incremented <- struct{}{}
			<-release
			tm.DecActive(ctx)
		}()
	}

	// Barrier: wait until all n increments have happened.
	for i := 0; i < n; i++ {
		<-incremented
	}
	if got := gaugeValue(t, p, "mcp_http_active_requests"); got != n {
		t.Errorf("gauge at peak = %v, want %d", got, n)
	}

	// Release all goroutines to decrement, then wait for them to finish.
	close(release)
	wg.Wait()
	if got := gaugeValue(t, p, "mcp_http_active_requests"); got != 0 {
		t.Errorf("gauge after drain = %v, want 0", got)
	}
}
