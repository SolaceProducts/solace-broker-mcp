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

	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2/specs"
)

// See internal/composite/executor_span_test.go for why the provider is
// installed exactly once per test binary and reset per test.
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
	sharedSpanRecorder.Reset()
	return sharedSpanRecorder
}

// The span counterpart of TestDescribeSempSchema_EmitsAuditLog: this handler is
// registered straight against the MCP server and never reaches
// ToolManager.CallTool, so it has to produce its own dispatch span the same way
// it already produces its own audit line and metric. Without one, a tool that
// appears in every dashboard appears in no trace, and `not_found` — the
// error_type only this handler raises — is a metric-only value of a vocabulary
// documented as shared by all three signals.
func TestDescribeSempSchema_EmitsDispatchSpan(t *testing.T) {
	for _, tt := range []struct {
		name          string
		operation     string
		wantOutcome   string
		wantErrorType string
		wantProtoErr  bool
	}{
		{name: "success", operation: "config/createMsgVpnQueue", wantOutcome: "success"},
		// An unknown operation is returned as a protocol error rather than a
		// structured error result, so the call itself fails — the span still
		// has to be closed and classified on the way out.
		{name: "unknown operation", operation: "config/noSuchOperation", wantOutcome: "error", wantErrorType: "not_found", wantProtoErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sr := recordSpans(t)

			server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
			if err := RegisterDescribeSempSchema(server, specs.FS, nil); err != nil {
				t.Fatalf("RegisterDescribeSempSchema: %v", err)
			}

			ctx := context.Background()
			serverTransport, clientTransport := mcp.NewInMemoryTransports()
			go func() { _ = server.Run(ctx, serverTransport) }()

			client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.1.0"}, nil)
			session, err := client.Connect(ctx, clientTransport, nil)
			if err != nil {
				t.Fatalf("client connect: %v", err)
			}
			defer func() { _ = session.Close() }()

			_, callErr := session.CallTool(ctx, &mcp.CallToolParams{
				Name:      describeSempSchemaToolName,
				Arguments: map[string]any{"operation": tt.operation},
			})
			if gotErr := callErr != nil; gotErr != tt.wantProtoErr {
				t.Fatalf("CallTool error = %v, want error: %v", callErr, tt.wantProtoErr)
			}

			var span sdktrace.ReadOnlySpan
			for _, s := range sr.Ended() {
				if s.Name() == dispatchSpanName {
					span = s
				}
			}
			if span == nil {
				t.Fatalf("no %q span: this handler bypasses CallTool, so it must start its own", dispatchSpanName)
			}

			attrs := map[string]string{}
			for _, kv := range span.Attributes() {
				attrs[string(kv.Key)] = kv.Value.AsString()
			}
			if attrs["tool"] != describeSempSchemaToolName {
				t.Errorf("span tool = %q, want %q", attrs["tool"], describeSempSchemaToolName)
			}
			if attrs["outcome"] != tt.wantOutcome {
				t.Errorf("span outcome = %q, want %q", attrs["outcome"], tt.wantOutcome)
			}
			if attrs["error_type"] != tt.wantErrorType {
				t.Errorf("span error_type = %q, want %q", attrs["error_type"], tt.wantErrorType)
			}
			// The `none` sentinel, not an absent attribute: this tool resolves
			// no broker, and the audit line and the metric label both carry
			// `none` for it, so the span has to as well or the join breaks.
			if attrs["broker"] != brokerLabelNone {
				t.Errorf("span broker = %q, want %q", attrs["broker"], brokerLabelNone)
			}
		})
	}
}

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
// of the dispatch seam, which attribute assertions cannot reach.
//
// dispatch.finish emits the audit record, the metric, and the span from one
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
// ordering the other three sites used.
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
