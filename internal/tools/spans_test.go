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
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

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
