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

package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer wraps bytes.Buffer with a mutex: the background emitter
// goroutine writes to it (via the slog handler) while the test goroutine
// polls it, and bytes.Buffer alone is not safe for that concurrent access.
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

// captureLogs swaps slog's default to a JSON handler writing into the
// returned buffer for the duration of the test, then restores the prior
// default (mirrors internal/middleware/recovery's helper of the same name,
// adapted for concurrent access from a background goroutine).
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// countOccurrences returns how many lines in buf contain substr.
func countOccurrences(buf *syncBuffer, substr string) int {
	n := 0
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

// TestSafeRun_RecoversPanic pins the core guarantee: a panicking fn must not
// crash the caller, and the recovered log must never leak the panic value's
// text — same secure-logging rule safego.Go enforces (its call sites don't
// fit this ticker-driven goroutine, see selfstats.go's doc comment).
func TestSafeRun_RecoversPanic(t *testing.T) {
	buf := captureLogs(t)

	const secret = "super-secret-token-value"
	safeRun(func() { panic(secret) })

	log := buf.String()
	if strings.Contains(log, secret) {
		t.Fatalf("panic value text leaked into log: %s", log)
	}
	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace([]byte(buf.String())), &rec); err != nil {
		t.Fatalf("log line is not valid JSON: %v (%s)", err, log)
	}
	if rec["event"] != "panic_recovered" {
		t.Errorf("event = %v, want panic_recovered", rec["event"])
	}
	if rec["panic_type"] != "string" {
		t.Errorf("panic_type = %v, want string (the panic value's Go type)", rec["panic_type"])
	}
}

// TestSafeRun_PassesThroughOnSuccess confirms the non-panic path is
// unaffected: fn runs and nothing is logged.
func TestSafeRun_PassesThroughOnSuccess(t *testing.T) {
	buf := captureLogs(t)

	ran := false
	safeRun(func() { ran = true })

	if !ran {
		t.Fatal("fn was never executed")
	}
	if buf.String() != "" {
		t.Fatalf("expected no log output on the success path, got: %s", buf.String())
	}
}

// TestStartSelfStatsEmitter_EmitsImmediatelyThenOnInterval pins that an
// operator flipping tracing on mid-incident sees the first reading without
// waiting a full interval, and that subsequent ticks keep reporting.
func TestStartSelfStatsEmitter_EmitsImmediatelyThenOnInterval(t *testing.T) {
	buf := captureLogs(t)

	stats := newExportStats()
	stats.recordExported(context.Background(), 42)

	stop := startSelfStatsEmitter(10*time.Millisecond, stats)
	defer stop()

	// The immediate report should land well before the first tick fires.
	deadline := time.After(2 * time.Second)
	for countOccurrences(buf, "otel_self_stats") < 1 {
		select {
		case <-deadline:
			t.Fatalf("no otel_self_stats line within the deadline; log=%s", buf.String())
		case <-time.After(time.Millisecond):
		}
	}
	if !strings.Contains(buf.String(), `"spans_exported_total":42`) {
		t.Fatalf("emitted line missing spans_exported_total=42: %s", buf.String())
	}

	// Wait for at least one more tick to confirm the ticker loop, not just
	// the immediate report, is alive.
	deadline = time.After(2 * time.Second)
	for countOccurrences(buf, "otel_self_stats") < 2 {
		select {
		case <-deadline:
			t.Fatalf("only one otel_self_stats line after waiting for a tick; log=%s", buf.String())
		case <-time.After(time.Millisecond):
		}
	}
}

// TestStartSelfStatsEmitter_StopIsIdempotentAndHalts confirms stop halts the
// goroutine (no further lines after stop returns) and can be called more than
// once without hanging or panicking.
func TestStartSelfStatsEmitter_StopIsIdempotentAndHalts(t *testing.T) {
	buf := captureLogs(t)

	stats := newExportStats()
	stop := startSelfStatsEmitter(5*time.Millisecond, stats)
	stop()
	countAtStop := countOccurrences(buf, "otel_self_stats")

	// A second call must not hang or panic (sync.Once guards the close).
	stop()

	time.Sleep(50 * time.Millisecond)
	if got := countOccurrences(buf, "otel_self_stats"); got != countAtStop {
		t.Fatalf("emitter kept reporting after stop: %d lines before, %d after", countAtStop, got)
	}
}
