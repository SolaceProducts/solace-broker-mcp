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
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/audit"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"go.opentelemetry.io/otel/codes"
)

// SOL-156509: a call counts as cancelled only when the context CallTool was
// given has ended AND the handler's error is that ending. Both halves matter:
// our own SEMP retry budget is a child context, so its timeout leaves the
// caller's ctx live and stays an error; and a real failure that races a
// cancel keeps reporting the failure.
func TestClassifyCallOutcome(t *testing.T) {
	live := context.Background()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	semErr := &sempv2.SEMPError{StatusCode: 400, SEMPCode: 6, Operation: "deleteMsgVpnQueue"}

	canceledErr := fmt.Errorf("step: %w", context.Canceled)
	deadlineErr := fmt.Errorf("step: %w", context.DeadlineExceeded)

	for _, tt := range []struct {
		name          string
		ctx           context.Context
		err           error
		budgetExpired bool
		want          metrics.Outcome
	}{
		{"success", live, nil, false, metrics.OutcomeSuccess},
		{"broker success that a cancel followed", cancelled, nil, false, metrics.OutcomeSuccess},
		{"failure, caller still there", live, errors.New("connection refused"), false, metrics.OutcomeError},
		{"caller cancelled", cancelled, canceledErr, false, metrics.OutcomeCancelled},
		{"caller's own deadline", expired, deadlineErr, false, metrics.OutcomeCancelled},
		{"our retry budget timed out", live, deadlineErr, true, metrics.OutcomeError},
		// Both deadlines have passed by the time CallTool classifies; the
		// error is the budget's, so it is our timeout, not the caller's.
		{"our retry budget timed out, then the caller's deadline passed", expired, deadlineErr, true, metrics.OutcomeError},
		{"caller cancelled after a retry budget timed out", cancelled, canceledErr, true, metrics.OutcomeCancelled},
		{"broker error that raced a cancel", cancelled, semErr, false, metrics.OutcomeError},
		{"panic while cancelled", cancelled, panicError{}, false, metrics.OutcomeError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyCallOutcome(tt.ctx, tt.err, tt.budgetExpired); got != tt.want {
				t.Errorf("classifyCallOutcome = %q, want %q", got, tt.want)
			}
		})
	}
}

// cancellingDestructiveManager registers a destructive tool whose handler
// sees the caller leave and returns the ctx error, the way a SEMP call does
// when its context is cancelled.
func cancellingDestructiveManager(t *testing.T) *ToolManager {
	t.Helper()
	mgr := NewToolManager(newTestPool(t), WithAuditLog(true))
	h := newStubHandler("delete-queue")
	yes := true
	h.annotations = Annotations{Destructive: &yes}
	h.handleFn = func(ctx context.Context, _ *ToolContext, _ map[string]any) (*ToolResult, error) {
		<-ctx.Done()
		return nil, fmt.Errorf("step deleteQueue: %w", ctx.Err())
	}
	mgr.Register(h)
	return mgr
}

func callCancelled(t *testing.T, mgr *ToolManager) []map[string]any {
	t.Helper()
	ctx, cancel := context.WithCancel(auditCtx(t))
	cancel()
	return captureAudit(t, slog.LevelDebug, func() {
		if _, err := mgr.CallTool(ctx, "delete-queue",
			map[string]any{"broker": "dev", "msgVpnName": "default"}, idFixture()); err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	})
}

func TestCallTool_CancelledCall_ToolInvokedLineSaysCancelled(t *testing.T) {
	records := callCancelled(t, cancellingDestructiveManager(t))

	var line map[string]any
	for _, rec := range records {
		if rec["msg"] == "tool invoked" {
			line = rec
		}
	}
	if line == nil {
		t.Fatalf("no tool invoked line:\n%v", records)
	}
	if line["outcome"] != "cancelled" {
		t.Errorf("outcome = %v, want cancelled", line["outcome"])
	}
	if _, has := line["error_type"]; has {
		t.Errorf("error_type = %v on a cancelled call; the shared vocabulary has none for cancelled", line["error_type"])
	}
	// The caller leaving is not a server failure: ERROR would page on it,
	// and WARN is noise for something a client does routinely.
	if line["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", line["level"])
	}
	// The line says whether a write reached the broker on its own: the audit
	// record that also says so is off by default and covers destructive tools
	// only. The stub never reached a SEMP sender.
	if got, ok := line["request_sent"]; !ok || got != false {
		t.Errorf("request_sent = %v (present=%v), want false", got, ok)
	}
}

func TestCallTool_CancelledCall_OperationRecordSaysCancelled(t *testing.T) {
	ops := ofType(callCancelled(t, cancellingDestructiveManager(t)), audit.EventOperation)
	if len(ops) != 1 {
		t.Fatalf("operation records = %d, want 1", len(ops))
	}
	if ops[0]["outcome"] != string(audit.OutcomeCancelled) {
		t.Errorf("outcome = %v, want cancelled", ops[0]["outcome"])
	}
	if _, has := ops[0]["error_type"]; has {
		t.Errorf("error_type = %v; the audit schema forbids it on a cancelled record", ops[0]["error_type"])
	}
	// The stub never reached a SEMP sender, so nothing went to the broker.
	if got, ok := ops[0]["request_sent"]; !ok || got != false {
		t.Errorf("request_sent = %v (present=%v), want false", got, ok)
	}
}

// A handler failure before any SEMP write: error, request_sent=false.
func TestCallTool_FailedBeforeSend_RequestSentFalse(t *testing.T) {
	mgr := auditTestManager(t, true)
	records := captureAudit(t, slog.LevelDebug, func() {
		if _, err := mgr.CallTool(auditCtx(t), "delete-queue-failing",
			map[string]any{"broker": "dev", "msgVpnName": "default"}, idFixture()); err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	})
	ops := ofType(records, audit.EventOperation)
	if len(ops) != 1 {
		t.Fatalf("operation records = %d, want 1:\n%v", len(ops), records)
	}
	if ops[0]["outcome"] != string(audit.OutcomeError) {
		t.Errorf("outcome = %v, want error", ops[0]["outcome"])
	}
	if got, ok := ops[0]["request_sent"]; !ok || got != false {
		t.Errorf("request_sent = %v (present=%v), want false", got, ok)
	}
}

func TestEndDispatchSpan_Cancelled(t *testing.T) {
	sr := recordSpans(t)
	ctx, span := tracer.Start(context.Background(), dispatchSpanName)
	endDispatchSpan(ctx, span, "delete-queue", "dev", metrics.ErrorTypeExecutionError, metrics.OutcomeCancelled)

	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(ended))
	}
	s := ended[0]
	attrs := map[string]string{}
	for _, kv := range s.Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if attrs["outcome"] != "cancelled" {
		t.Errorf("span outcome = %q, want cancelled", attrs["outcome"])
	}
	if _, has := attrs["error_type"]; has {
		t.Errorf("span error_type = %q on a cancelled call", attrs["error_type"])
	}
	if s.Status().Code == codes.Error {
		t.Error("span status is Error for a call the caller cancelled")
	}
}

// A desired-state no-op (ALREADY_EXISTS on a create) whose result then fails
// output validation is an error on the metric, the span and the audit record.
// The `tool invoked` line must say so too: it used to key on the no-op before
// the outcome and log INFO outcome=success.
func TestCallTool_DesiredStateNoopFailingValidation_LogsError(t *testing.T) {
	mgr := NewToolManager(newTestPool(t))
	h := newStubHandler("create-queue-subscription")
	// Rejects every field the no-op result carries.
	h.outputSch = map[string]any{"type": "object", "additionalProperties": false}
	h.handleFn = func(context.Context, *ToolContext, map[string]any) (*ToolResult, error) {
		return nil, &sempv2.SEMPError{
			Operation: "createMsgVpnQueueSubscription", StatusCode: 400,
			Description: "already exists", SEMPCode: 10, SEMPStatus: "ALREADY_EXISTS",
		}
	}
	mgr.Register(h)

	records := captureAudit(t, slog.LevelDebug, func() {
		if _, err := mgr.CallTool(context.Background(), "create-queue-subscription",
			map[string]any{"broker": "dev", "msgVpnName": "default"}, Identity{}); err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	})
	var line map[string]any
	for _, rec := range records {
		if rec["msg"] == "tool invoked" {
			line = rec
		}
	}
	if line == nil {
		t.Fatalf("no tool invoked line:\n%v", records)
	}
	if line["outcome"] != "error" || line["error_type"] != string(metrics.ErrorTypeOutputValidationError) {
		t.Errorf("outcome = %v, error_type = %v; want error / %s",
			line["outcome"], line["error_type"], metrics.ErrorTypeOutputValidationError)
	}
	if line["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", line["level"])
	}
}
