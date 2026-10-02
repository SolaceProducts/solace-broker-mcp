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

package tools

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// See internal/composite/executor_span_test.go for why the provider is
// installed exactly once per test binary and reset per test.
var (
	sharedSpanRecorder *tracetest.SpanRecorder
	installSpanTracer  sync.Once
)

// Callers of this MUST NOT call t.Parallel. The recorder is shared and reset
// per test (only the first otel.SetTracerProvider is honored, so the provider
// cannot be swapped per test), so two tests recording spans concurrently would
// race on Reset and on Ended.
//
// Unlike internal/semp/sempv2, this package DOES have t.Parallel tests
// (describe_semp_schema_test.go, audit_error_type_drift_test.go). They are safe
// today only because none of them records spans, and because Go resumes a
// package's parallel tests only after every sequential top-level test has
// finished. Adding t.Parallel to a span-recording test here is what would break
// it — flakily, and only under some orderings.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	installSpanTracer.Do(func() {
		sharedSpanRecorder = tracetest.NewSpanRecorder()
		otel.SetTracerProvider(sdktrace.NewTracerProvider(
			sdktrace.WithSpanProcessor(sharedSpanRecorder),
			sdktrace.WithSampler(sdktrace.AlwaysSample()),
		))
	})
	sharedSpanRecorder.Reset()
	return sharedSpanRecorder
}

// TestDescribeSempSchema_EmitsDispatchSpan formerly covered describe-semp-
// schema as a handler registered straight against the MCP server, never
// reaching ToolManager.CallTool, that had to produce its own dispatch span
// the same way it produced its own audit line and metric — and separately
// pinned that its "unknown operation" case surfaced as a protocol error with
// error_type=not_found.
//
// SOL-153693 retired all of that: the tool is now an ordinary CallTool-routed
// tool (Metadata.NoBroker, not a bypass), so its span comes from CallTool's
// own instrumentation — the same plumbing every other tool uses, already
// covered generically elsewhere in this package — and "unknown operation" is
// now an isError tool result with error_type=execution_error, not a protocol
// error with error_type=not_found (see describe_semp_schema_test.go's
// TestDescribeSempSchema_UnknownOperation_ErrorTypeReachesAuditAndMetric,
// which is where that behavior is pinned now). The dedicated span test was
// deleted as redundant rather than updated to re-describe a bypass that no
// longer exists.

// spanIDCapturingHandler records the span that was in context each time the
// "tool invoked" audit line was emitted. slog passes the caller's context
// through to the handler, which makes it the one place a test can observe which
// span a dispatch site actually had in context at emission time.
type spanIDCapturingHandler struct {
	slog.Handler
	mu      sync.Mutex
	spanIDs []string
}

func (h *spanIDCapturingHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "tool invoked" {
		h.mu.Lock()
		h.spanIDs = append(h.spanIDs, trace.SpanContextFromContext(ctx).SpanID().String())
		h.mu.Unlock()
	}
	return nil
}

func (h *spanIDCapturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *spanIDCapturingHandler) captured() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.spanIDs...)
}

// TestDispatch_AuditAndMetricSeeTheDispatchSpanInContext pins the ordering half
// of dispatch instrumentation, which attribute assertions cannot reach.
//
// Every dispatch site emits its audit line, its metric and its span from one
// context, and that context has to be the span-carrying one — otherwise the
// audit line and the metric are recorded while only the caller's context is in
// scope, and a Story 47 exemplar attaches to the wrong span (or, off an HTTP
// request, to no span at all). Every attribute VALUE is identical either way,
// which is exactly why this went unnoticed: the earlier version of the
// argument-parse branch recorded both signals before starting its span, and the
// cross-signal test passed regardless because it only compared values.
//
// Driven through the argument-parse failure because that is the branch that had
// it wrong — a straight-line return that could not express the deferred
// ordering ToolManager.CallTool, the other dispatch site, used.
func TestDispatch_AuditAndMetricSeeTheDispatchSpanInContext(t *testing.T) {
	sr := recordSpans(t)

	capture := &spanIDCapturingHandler{}
	old := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(old) })

	pool := newRegTestPool(t)
	mgr := NewToolManager(pool)
	mgr.Register(newStubHandler("ordering-probe"))

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	RegisterWithServer(mgr, server, pool, true, nil, "")

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverTransport) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	// Valid JSON that is not an object: the instrumented closure's unmarshal
	// into map[string]any fails and it returns before reaching CallTool.
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ordering-probe",
		Arguments: []any{"not", "an", "object"},
	}); err != nil {
		t.Fatalf("CallTool returned a protocol error: %v", err)
	}

	var dispatchSpan sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == dispatchSpanName {
			dispatchSpan = s
		}
	}
	if dispatchSpan == nil {
		t.Fatalf("no %q span for the argument-parse failure", dispatchSpanName)
	}

	ids := capture.captured()
	if len(ids) != 1 {
		t.Fatalf("captured %d \"tool invoked\" audit lines, want 1: %v", len(ids), ids)
	}
	want := dispatchSpan.SpanContext().SpanID().String()
	if ids[0] != want {
		t.Errorf("the audit record was emitted with span %s in context, but the dispatch span describing it is %s — the span must be started and its context threaded BEFORE the audit and metric calls, or their exemplars link to the wrong span",
			ids[0], want)
	}
}
