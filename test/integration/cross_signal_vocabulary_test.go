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

// The span/metric/audit vocabulary join (SOL-154036).
//
// ADR-009's single-join-key property was asserted only pairwise before this:
// Story 26 joined the span to the metric, SOL-152090's AST guard joined this
// package's error_type set to the audit constructor's vocabulary, and nothing
// joined the audit RECORD's values to the other two on one real call.
//
// Drift is the risk. Signals that agree today can diverge with nothing failing,
// because each signal's own tests only see that signal — and the operator-
// visible failure is a filter that quietly matches nothing.
//
// The helpers here are shared with request_path_spans_test.go, which joins the
// span, the metric and the `tool invoked` log line.
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/audit"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// syncBuffer is a bytes.Buffer guarded by a mutex.
//
// slog's handler serialises writes to its own writer, but nothing serialises
// those writes against a reader. The dispatch that produces these records runs
// on an SDK-spawned goroutine (jsonrpc2.handleAsync), not the test goroutine —
// the HTTP response to CallTool is what currently makes the records visible to
// a subsequent read. That edge is a property of the transport, not of this
// fixture, so the lock is what actually makes the pattern safe: adding
// t.Parallel(), or any background goroutine that logs, would otherwise turn a
// read into an intermittent data race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogRecords redirects the default logger into a buffer for the test.
// Both surfaces read here — logToolResult's line and audit.Emit's record —
// write through slog.Default(), so one buffer proves the same process wrote
// them. Debug level so nothing is filtered before it can be asserted on.
func captureLogRecords(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// crossSignalCall drives exactly one `span-probe-tool` call against h with
// every signal captured, and returns the three an assertion needs: the
// tools.CallTool span, the labels of the single metric series the call
// produced, and the log buffer holding both the `tool invoked` line and any
// audit record.
//
// One fixture, not two: the arrange block is identical for both cross-signal
// tests apart from the handler and the manager options, and two copies of "one
// call, all signals captured" would drift — which is the exact failure class
// these tests exist to catch.
func crossSignalCall(t *testing.T, h tools.ToolHandler, brokerAlias string,
	extraOpts ...tools.ManagerOption) (sdktrace.ReadOnlySpan, map[string]string, *syncBuffer) {
	t.Helper()

	sr := recordRequestPathSpans(t)
	p, err := metrics.New("v-test", sdkresource.Default(), config.ObservabilityConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	tm, err := p.ToolMetrics()
	if err != nil {
		t.Fatal(err)
	}

	logged := captureLogRecords(t)

	broker := fakeBroker(t)
	session := tracedSessionWith(t, h, broker.URL, true, tm, extraOpts...)

	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "span-probe-tool",
		Arguments: map[string]any{"broker": brokerAlias, "msgVpnName": "default"},
	}); err != nil {
		t.Fatalf("CallTool returned a protocol error: %v", err)
	}

	series := toolMetricSeries(t, p)
	if len(series) != 1 {
		t.Fatalf("mcp_tool_invocation_total series = %d, want exactly 1 for one call: %v",
			len(series), series)
	}
	return oneSpan(t, sr, "tools.CallTool"), series[0], logged
}

// jsonLogRecords parses the JSON lines out of buf, skipping interleaved
// non-JSON output rather than failing on it.
func jsonLogRecords(buf *syncBuffer) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// oneRecordWithMsg returns the single record with that msg. Exactly one, not
// the first: a doubled emit for one call would otherwise pass unnoticed.
func oneRecordWithMsg(t *testing.T, buf *syncBuffer, msg string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, rec := range jsonLogRecords(buf) {
		if rec["msg"] == msg {
			found = append(found, rec)
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d log records with msg=%q, want exactly 1 for one call:\n%s",
			len(found), msg, buf.String())
	}
	return found[0]
}

// oneOperationAuditRecord returns the single `operation` audit record.
// Filtered on event + audit_event_type, not msg: every audit record shares one
// msg, and a hop-2 denial (SOL-153332) legitimately adds a second record for
// the same call.
func oneOperationAuditRecord(t *testing.T, buf *syncBuffer) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, rec := range jsonLogRecords(buf) {
		if rec["event"] == audit.EventValue && rec["audit_event_type"] == string(audit.EventOperation) {
			found = append(found, rec)
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d audit_event_type=operation records, want exactly 1 for one call:\n%s",
			len(found), buf.String())
	}
	return found[0]
}

// stringField reads a string field, reporting an absent one as "".
//
// Load-bearing, not convenience: each surface encodes "no value" differently —
// a Prometheus label cannot be absent, an unset span attribute is simply
// missing, and slog omits an attr the emit site never added (what both
// emitters do with error_type on a success). All three mean the same thing, so
// they must compare equal or every successful call reads as a disagreement.
func stringField(rec map[string]any, key string) string {
	s, _ := rec[key].(string)
	return s
}

// destructiveStubHandler is compositeStubHandler with the annotations that put
// a call on the audit surface. Embedded so the traced path stays byte-for-byte
// the one the other tests exercise.
type destructiveStubHandler struct {
	compositeStubHandler
}

func (h *destructiveStubHandler) Metadata() tools.Metadata {
	md := h.compositeStubHandler.Metadata()
	yes := true
	// TWO fields change, not one: types.go documents Destructive as meaningful
	// only when ReadOnly is false, so the embedded handler's ReadOnly: true has
	// to be cleared alongside it. Assigned field by field rather than replacing
	// the whole Annotations struct, so an Idempotent or OpenWorld annotation
	// added to the embedded handler later is carried here instead of being
	// silently dropped.
	md.Annotations.ReadOnly = false
	md.Annotations.Destructive = &yes
	return md
}

// TestRequestPathSpans_OperationAuditRecordAgreesWithSpanAndMetric closes
// ADR-009's third side: the record a compliance reviewer reads must spell the
// call the way the dashboard and the trace backend do.
//
// A different path from the `tool invoked` line, not a second look at it: this
// record comes from emitOperationAudit through audit.NewEvent, whose Outcome is
// an independent typed vocabulary that merely happens to be spelled like the
// metric's — and the error_type crosses the package boundary as a bare string.
//
// The panic case is why this exists rather than being a third table row. It is
// classified by a route no other case takes (the emission defer infers it from
// toolErr and result both being nil, then rewrites errorType), and the record
// derives panic_recovered by string-comparing that value. A destructive handler
// that panicked may already have changed the broker, so this is the record
// whose mislabelling would actually mislead an incident review.
//
// No unknown-broker row: resolution fails before the destructive gate, so no
// hash is taken and no operation record is emitted. That is correct, and
// asserting one would pin behaviour the design does not have.
func TestRequestPathSpans_OperationAuditRecordAgreesWithSpanAndMetric(t *testing.T) {
	for _, tt := range []struct {
		name          string
		failStep      bool
		panics        bool
		broker        string
		wantBroker    string
		wantOutcome   string
		wantErrorType string
	}{
		{name: "success", broker: "dev", wantBroker: "dev", wantOutcome: "success"},
		{name: "error", failStep: true, broker: "dev", wantBroker: "dev",
			wantOutcome: "error", wantErrorType: "execution_error"},
		{name: "panic", panics: true, broker: "dev", wantBroker: "dev",
			wantOutcome: "error", wantErrorType: "panic"},
		// The case that gives the broker key something to prove. Unlike the
		// `tool invoked` line, this record is written after CallTool has
		// rewritten the alias to its configured form, so it is canonical by
		// construction rather than by an explicit canonicalBrokerLabel call —
		// and with a fixture whose alias and display name are spelled alike,
		// a broker comparison cannot tell the two apart. The caller types
		// "dev-eu"; every canonicalized surface must say "Dev-EU".
		{name: "non-canonical caller alias", broker: "dev-eu", wantBroker: "Dev-EU",
			wantOutcome: "success"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dispatch, labels, logged := crossSignalCall(t,
				&destructiveStubHandler{compositeStubHandler{failStep: tt.failStep, panics: tt.panics}},
				tt.broker, tools.WithAuditLog(true))

			record := oneOperationAuditRecord(t, logged)

			// Pinned first, so the cross-comparison below cannot pass by all
			// three surfaces being wrong the same way — or, for tool and
			// broker, by all three being empty, which would compare equal
			// while proving nothing.
			if got := stringField(record, "outcome"); got != tt.wantOutcome {
				t.Errorf("audit outcome = %q, want %q", got, tt.wantOutcome)
			}
			if got := stringField(record, "error_type"); got != tt.wantErrorType {
				t.Errorf("audit error_type = %q, want %q", got, tt.wantErrorType)
			}
			if got := stringField(record, "tool"); got != "span-probe-tool" {
				t.Errorf("audit tool = %q, want %q", got, "span-probe-tool")
			}
			if got := stringField(record, "broker"); got != tt.wantBroker {
				t.Errorf("audit broker = %q, want the canonical %q", got, tt.wantBroker)
			}

			for _, key := range []string{"tool", "broker", "outcome", "error_type"} {
				spanValue, _ := spanAttr(dispatch, key)
				auditValue := stringField(record, key)
				if auditValue != labels[key] {
					t.Errorf("%s: audit record = %q, metric label = %q — a compliance query and a dashboard filter disagree about the same call",
						key, auditValue, labels[key])
				}
				if auditValue != spanValue {
					t.Errorf("%s: audit record = %q, span attribute = %q — a reviewer cannot carry the value into the trace backend",
						key, auditValue, spanValue)
				}
			}

			// Audit-only, so nothing to join against. Derived by string-
			// comparing the error_type the loop above pins, so a rename that
			// broke the join would flip this and turn a crash into an ordinary
			// failure in the audit trail.
			wantPanic := tt.wantErrorType == "panic"
			if got, _ := record["panic_recovered"].(bool); got != wantPanic {
				t.Errorf("audit panic_recovered = %v, want %v (error_type = %q)",
					got, wantPanic, stringField(record, "error_type"))
			}
		})
	}
}
