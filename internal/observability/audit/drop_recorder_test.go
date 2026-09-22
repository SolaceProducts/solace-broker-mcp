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
	"context"
	"log/slog"
	"sync"
	"testing"
)

// countingDropRecorder is the DropRecorder these tests install: it counts
// calls and nothing else, so an assertion on it is an assertion on how many
// times EmitDrop decided a record was lost. Mutex-guarded because the drop
// path runs under a handler the test may also be counting on.
type countingDropRecorder struct {
	mu    sync.Mutex
	calls int
}

func (r *countingDropRecorder) RecordAuditDrop(context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
}

func (r *countingDropRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// withDropRecorder installs r for the duration of fn and restores whatever
// was installed before, so no test leaks a recorder into the next.
func withDropRecorder(t *testing.T, r DropRecorder, fn func()) {
	t.Helper()
	prev := dropRecorder.Load()
	SetDropRecorder(r)
	defer dropRecorder.Store(prev)
	fn()
}

// TestEmitDrop_countsOneDropOnTheRecorder is the direct path: a caller that
// could not build a record reports the gap, and the counter moves by one.
func TestEmitDrop_countsOneDropOnTheRecorder(t *testing.T) {
	r := &countingDropRecorder{}
	withDropRecorder(t, r, func() {
		captureRecords(t, slog.LevelDebug, func() {
			EmitDrop(context.Background(), DropContext{DroppedEventType: EventOperation, Tool: "delete-queue"})
		})
	})
	if got := r.count(); got != 1 {
		t.Errorf("recorder saw %d drop(s) for one EmitDrop, want 1", got)
	}
}

// TestEmit_deliveredRecordDoesNotCount pins the negative: a record that
// reached the handler is not a drop, so the counter must not move.
func TestEmit_deliveredRecordDoesNotCount(t *testing.T) {
	e, err := NewEvent(context.Background(), validOperation())
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	r := &countingDropRecorder{}
	withDropRecorder(t, r, func() {
		captureRecords(t, slog.LevelDebug, func() { Emit(context.Background(), e) })
	})
	if got := r.count(); got != 0 {
		t.Errorf("recorder saw %d drop(s) for a delivered record, want 0", got)
	}
}

// TestEmit_levelFilteredDropCounts covers the drop mode most likely to reach
// an operator — audit on, log level above INFO — and pins that the counter
// moves for it, not only for a refusing handler. The drop notice itself is
// still written (it is ERROR), so this is the "both signals" case
// docs/observability.md describes.
func TestEmit_levelFilteredDropCounts(t *testing.T) {
	e, err := NewEvent(context.Background(), validOperation())
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	r := &countingDropRecorder{}
	var records []map[string]any
	withDropRecorder(t, r, func() {
		records = captureRecords(t, slog.LevelError, func() { Emit(context.Background(), e) })
	})
	if got := r.count(); got != 1 {
		t.Errorf("recorder saw %d drop(s) for one level-filtered record, want 1", got)
	}
	if len(records) != 1 || records[0]["audit_event_type"] != string(EventAuditDrop) {
		t.Errorf("expected the drop notice alongside the count, got %v", records)
	}
}

// TestEmit_handlerFailureCountsOnceNotTwice pins the counter's semantics
// under a dead sink. The handler refuses the operation record AND the drop
// notice that reports it, so it sees two failed writes — but exactly one
// audit record was lost, and the counter must say one. Counting the notice's
// own failure would double every drop precisely when the log stream is down,
// the situation the counter exists to report accurately.
func TestEmit_handlerFailureCountsOnceNotTwice(t *testing.T) {
	e, err := NewEvent(context.Background(), validOperation())
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	h := &failingHandler{}
	r := &countingDropRecorder{}
	withDropRecorder(t, r, func() {
		withLogger(t, h, func() { Emit(context.Background(), e) })
	})
	if h.calls != 2 {
		t.Fatalf("handler saw %d write(s), want 2 (the record, then the drop notice) — the test's premise does not hold", h.calls)
	}
	if got := r.count(); got != 1 {
		t.Errorf("recorder saw %d drop(s) when one record was lost under a failing sink, want exactly 1", got)
	}
}

// TestEmitDrop_countsEvenWhenTheNoticeCannotBeWritten is the whole reason the
// counter exists: with the log stream failing every write, the audit_drop
// record never lands anywhere, and the counter is the only signal left. The
// increment must therefore not depend on the notice being deliverable.
func TestEmitDrop_countsEvenWhenTheNoticeCannotBeWritten(t *testing.T) {
	h := &failingHandler{}
	r := &countingDropRecorder{}
	withDropRecorder(t, r, func() {
		withLogger(t, h, func() { EmitDrop(context.Background(), DropContext{}) })
	})
	if h.calls != 1 {
		t.Fatalf("handler saw %d write(s), want 1 (the refused notice)", h.calls)
	}
	if got := r.count(); got != 1 {
		t.Errorf("recorder saw %d drop(s) with a dead sink, want 1 — the counter must not wait on the notice being written", got)
	}
}

// TestEmit_panickingHandlerStillCounts: a handler that panics is a drop, not
// a failed operation (see write), and it is counted like any other drop.
func TestEmit_panickingHandlerStillCounts(t *testing.T) {
	e, err := NewEvent(context.Background(), validOperation())
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	r := &countingDropRecorder{}
	withDropRecorder(t, r, func() {
		withLogger(t, panickingHandler{}, func() { Emit(context.Background(), e) })
	})
	if got := r.count(); got != 1 {
		t.Errorf("recorder saw %d drop(s) for a record lost to a panicking handler, want 1", got)
	}
}

// TestEmitDrop_noRecorderIsANoOp pins the default: before SetDropRecorder
// runs, and permanently with metrics off, EmitDrop still writes the notice
// and does not touch a recorder. This is the state every other test in the
// package runs in.
func TestEmitDrop_noRecorderIsANoOp(t *testing.T) {
	var records []map[string]any
	withDropRecorder(t, nil, func() {
		if dropRecorder.Load() != nil {
			t.Fatal("SetDropRecorder(nil) left a recorder installed")
		}
		records = captureRecords(t, slog.LevelDebug, func() { EmitDrop(context.Background(), DropContext{}) })
	})
	if len(records) != 1 {
		t.Errorf("wrote %d record(s) with no recorder installed, want the notice alone", len(records))
	}
}

// TestSetDropRecorder_replacesThePrevious: a second install wins, so a test
// or a restart cannot end up with two recorders each seeing every drop.
func TestSetDropRecorder_replacesThePrevious(t *testing.T) {
	first, second := &countingDropRecorder{}, &countingDropRecorder{}
	withDropRecorder(t, first, func() {
		SetDropRecorder(second)
		captureRecords(t, slog.LevelDebug, func() { EmitDrop(context.Background(), DropContext{}) })
	})
	if first.count() != 0 || second.count() != 1 {
		t.Errorf("first saw %d, second saw %d; want 0 and 1", first.count(), second.count())
	}
}
