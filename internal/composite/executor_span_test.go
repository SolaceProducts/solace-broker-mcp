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

package composite

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

// spanRecorderOnce installs one always-sampling provider for this test binary
// and hands back the single recorder every caller shares.
//
// Installed exactly once because this package holds its tracer in a
// package-scoped var resolved at init, and only the FIRST
// otel.SetTracerProvider call is honored for handles obtained before it — a
// per-test provider would capture spans for whichever test ran first and
// silently nothing afterwards (internal/tokenexchange/span_test.go documents
// the same trap). Callers therefore filter the recorder by span name rather
// than assuming it holds only their own spans.
var (
	sharedSpanRecorder *tracetest.SpanRecorder
	installSpanTracer  sync.Once
)

func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	installSpanTracer.Do(func() {
		sharedSpanRecorder = tracetest.NewSpanRecorder()
		otel.SetTracerProvider(sdktrace.NewTracerProvider(
			sdktrace.WithSpanProcessor(sharedSpanRecorder),
			sdktrace.WithSampler(sdktrace.AlwaysSample()),
		))
	})
	// Reset per test: the recorder is shared (only the first
	// SetTracerProvider is honored, so it cannot be swapped per test), and
	// without this it accumulates across tests and across `go test -count`
	// iterations — a count assertion then passes on the first run and fails on
	// the second. Safe because nothing in this package calls t.Parallel.
	sharedSpanRecorder.Reset()
	return sharedSpanRecorder
}

// endedSpan returns the most recently ended span with the given name.
func endedSpan(t *testing.T, sr *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	var found sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == name {
			found = s
		}
	}
	if found == nil {
		t.Fatalf("no ended span named %q", name)
	}
	return found
}

// panickingClient panics instead of answering, standing in for a programmer
// error anywhere below Execute.
type panickingClient struct{}

func (panickingClient) Execute(context.Context, *sempv2.Operation, map[string]any) (*sempv2.Result, error) {
	panic("deliberate panic below composite.Execute")
}

// A panic below Execute must not leave the span claiming success. Go does not
// populate named returns on an unwound panic, so `err` is still nil when the
// deferred span-closing function runs: without the recover in Execute the span
// would be exported as `outcome: success` moments before the panic takes the
// process down, pointing an investigation in exactly the wrong direction.
//
// Also asserts the panic still propagates — the recover exists for span
// fidelity, not to swallow the failure, and the layers above (withRecovery in
// internal/tools) depend on it reaching them.
func TestExecute_PanicMarksSpanAsErrorAndRepanics(t *testing.T) {
	sr := recordSpans(t)
	executor := NewCompositeExecutor(testOperations())
	tool := CompositeTool{
		Name:   "panic-probe",
		Steps:  []Step{{ID: "step1", Operation: "monitor/getMsgVpnQueue"}},
		Result: ResultStrategy{Strategy: "collect"},
	}

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("panic did not propagate out of Execute; the recover must re-panic, not swallow")
			}
		}()
		_, _ = executor.Execute(context.Background(), tool, panickingClient{}, map[string]any{
			"msgVpnName": "default",
			"queueName":  "q1",
		})
	}()

	span := endedSpan(t, sr, "composite.Execute")
	var outcome string
	for _, kv := range span.Attributes() {
		if kv.Key == "outcome" {
			outcome = kv.Value.AsString()
		}
	}
	if outcome != "error" {
		t.Errorf("span outcome = %q, want %q: a panicked call must not be exported as a success", outcome, "error")
	}
	if got := span.Status().Code; got != codes.Error {
		t.Errorf("span status = %v, want Error", got)
	}
	if desc := span.Status().Description; desc != "" {
		t.Errorf("span status description = %q, want empty (the panic value is unvouched text and must not be exported)", desc)
	}
}

// spanningClient starts a span from whatever context each step hands it, so a
// test can read back which span that context actually pointed at.
type spanningClient struct{}

func (spanningClient) Execute(ctx context.Context, op *sempv2.Operation, _ map[string]any) (*sempv2.Result, error) {
	_, span := otel.Tracer("test/probe").Start(ctx, "probe.step")
	defer span.End()
	return &sempv2.Result{Data: map[string]any{"op": op.ID}, StatusCode: 200}, nil
}

// Parallel steps run in errgroup goroutines, which is the one place in this
// story where the request context is easiest to lose: `errgroup.WithContext`
// takes a parent, and a step handed context.Background() (or a context derived
// before tracer.Start) produces a span that looks perfectly healthy while
// sitting in its own detached trace. Nothing about the resulting spans is
// malformed, so only reading back the parent edge catches it — and it is the
// same defect that would silently yield zero exemplars for Story 47
// (SOL-152419), which the ticket calls out explicitly.
func TestExecute_ParallelStepsInheritTheExecutorSpan(t *testing.T) {
	sr := recordSpans(t)
	executor := NewCompositeExecutor(testOperations())
	tool := CompositeTool{
		Name: "parallel-span-probe",
		Steps: []Step{
			{ID: "a", Operation: "monitor/getMsgVpnQueue", Parallel: true},
			{ID: "b", Operation: "monitor/getMsgVpnQueue", Parallel: true},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}

	if _, err := executor.Execute(context.Background(), tool, spanningClient{}, map[string]any{
		"msgVpnName": "default",
		"queueName":  "q1",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	parent := endedSpan(t, sr, "composite.Execute")
	var probes []sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == "probe.step" {
			probes = append(probes, s)
		}
	}
	if len(probes) != 2 {
		t.Fatalf("probe spans = %d, want 2 (one per parallel step)", len(probes))
	}
	for i, p := range probes {
		if p.Parent().SpanID() != parent.SpanContext().SpanID() {
			t.Errorf("parallel step %d: span parent = %v, want composite.Execute (%v) — the step's context does not carry the executor span, so its subtree is detached from the trace",
				i, p.Parent().SpanID(), parent.SpanContext().SpanID())
		}
		if p.SpanContext().TraceID() != parent.SpanContext().TraceID() {
			t.Errorf("parallel step %d: trace ID = %s, want %s", i, p.SpanContext().TraceID(), parent.SpanContext().TraceID())
		}
	}
}
