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

package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// failingHandler is a slog.Handler whose Handle always fails, standing in for
// the sink backpressure the drop notice exists to report. Concurrency-safe
// because Emit's drop path calls back into it.
type failingHandler struct {
	mu    sync.Mutex
	calls int
}

func (h *failingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *failingHandler) Handle(context.Context, slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	return errors.New("sink refused the record")
}
func (h *failingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *failingHandler) WithGroup(string) slog.Handler      { return h }

// withLogger swaps the default logger for the duration of fn.
func withLogger(t *testing.T, h slog.Handler, fn func()) {
	t.Helper()
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(old)
	fn()
}

// captureRecords runs fn against a JSON handler at level and decodes what it
// wrote.
func captureRecords(t *testing.T, level slog.Level, fn func()) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	withLogger(t, slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level}), fn)

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("undecodable line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// TestEmit_writesTheRecord is the happy path: the record reaches the log with
// its attributes and its message intact.
func TestEmit_writesTheRecord(t *testing.T) {
	e, err := NewEvent(context.Background(), validOperation())
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	records := captureRecords(t, slog.LevelDebug, func() { Emit(context.Background(), e) })

	if len(records) != 1 {
		t.Fatalf("wrote %d record(s), want 1: %v", len(records), records)
	}
	if records[0]["msg"] != Message || records[0]["event"] != EventValue {
		t.Errorf("record = %#v", records[0])
	}
	if records[0]["audit_event_type"] != string(EventOperation) {
		t.Errorf("audit_event_type = %v", records[0]["audit_event_type"])
	}
}

// TestEmit_levelFilteredRecordBecomesADrop is the failure mode most likely to
// reach an operator: enabling the audit log on a server running above INFO.
// The operation record cannot be written, so a drop record is — at WARN, which
// survives the same filter.
func TestEmit_levelFilteredRecordBecomesADrop(t *testing.T) {
	e, err := NewEvent(context.Background(), validOperation())
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	records := captureRecords(t, slog.LevelWarn, func() { Emit(context.Background(), e) })

	if len(records) != 1 {
		t.Fatalf("wrote %d record(s), want exactly the drop: %v", len(records), records)
	}
	if records[0]["audit_event_type"] != string(EventAuditDrop) {
		t.Errorf("audit_event_type = %v, want %q", records[0]["audit_event_type"], EventAuditDrop)
	}
	// The drop must stay inside the customer's event="audit" filter: a notice
	// that fell outside it would go unseen in exactly the situation it reports.
	if records[0]["event"] != EventValue {
		t.Errorf("drop record event = %v, want %q", records[0]["event"], EventValue)
	}
	if records[0]["level"] != "WARN" {
		t.Errorf("drop record level = %v, want WARN so it outlives the filter that suppressed the record it reports", records[0]["level"])
	}
}

// TestEmit_handlerFailureBecomesADrop covers the other half: the handler
// accepted the level but refused the write.
func TestEmit_handlerFailureBecomesADrop(t *testing.T) {
	e, err := NewEvent(context.Background(), validOperation())
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	h := &failingHandler{}
	withLogger(t, h, func() { Emit(context.Background(), e) })

	// Two attempts: the operation record, then the drop notice reporting it.
	if h.calls != 2 {
		t.Errorf("handler saw %d write(s), want 2 (the record, then the drop notice)", h.calls)
	}
}

// TestEmit_dropNoticeDoesNotRecurse pins the termination condition. With a
// handler that fails every write, a drop notice that reported its own failure
// would recurse until the stack ran out.
func TestEmit_dropNoticeDoesNotRecurse(t *testing.T) {
	drop, err := NewEvent(context.Background(), Fields{Type: EventAuditDrop})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	h := &failingHandler{}
	withLogger(t, h, func() { Emit(context.Background(), drop) })

	if h.calls != 1 {
		t.Errorf("handler saw %d write(s) for a failing drop notice, want exactly 1", h.calls)
	}
}

// TestEmitDrop_writesTheNotice covers the path callers take when they cannot
// build a valid record at all — a failed canonicalization, or a record the
// constructor rejected.
func TestEmitDrop_writesTheNotice(t *testing.T) {
	records := captureRecords(t, slog.LevelDebug, func() { EmitDrop(context.Background()) })

	if len(records) != 1 {
		t.Fatalf("wrote %d record(s), want 1: %v", len(records), records)
	}
	if records[0]["event"] != EventValue || records[0]["audit_event_type"] != string(EventAuditDrop) {
		t.Errorf("record = %#v", records[0])
	}
	// Only the common fields; there is no surviving record to describe.
	for _, key := range []string{"tool", "broker", "outcome", "error_type", "principal", "arguments_hash"} {
		if _, present := records[0][key]; present {
			t.Errorf("drop notice carries %q = %#v", key, records[0][key])
		}
	}
}

// TestEmit_honoursReplaceAttr pins that audit records go through the server's
// configured handler, not around it. The handler is where this repo's
// redaction safety net lives, and a record written past it would be the one
// log line in the process with no redaction applied.
func TestEmit_honoursReplaceAttr(t *testing.T) {
	e, err := NewEvent(context.Background(), validOperation())
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}

	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == "broker" {
				return slog.String("broker", "REDACTED")
			}
			return a
		},
	})
	withLogger(t, h, func() { Emit(context.Background(), e) })

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("undecodable record %q: %v", buf.String(), err)
	}
	if rec["broker"] != "REDACTED" {
		t.Errorf("broker = %v; the handler's ReplaceAttr was not applied to the audit record", rec["broker"])
	}
}

// TestEmit_respectsTheConfiguredLevel pins that Emit does not smuggle records
// past the operator's log level. It reads the handler directly to see the
// write error, which would otherwise be an easy place to skip the level check
// slog.Logger normally applies.
func TestEmit_respectsTheConfiguredLevel(t *testing.T) {
	// An audit_drop is WARN, so an ERROR-only handler must suppress it, and
	// the suppression must not itself produce output.
	records := captureRecords(t, slog.LevelError, func() { EmitDrop(context.Background()) })
	if len(records) != 0 {
		t.Errorf("wrote %d record(s) below the configured level: %v", len(records), records)
	}
}
