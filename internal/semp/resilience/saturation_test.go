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

package resilience

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
)

// slowAdmissionThreshold is the trip point every test here configures. Short
// enough to keep the suite fast, long enough that a loaded runner cannot cross
// it by accident on the "admitted promptly" cases.
const slowAdmissionThreshold = 50 * time.Millisecond

// syncBuffer collects log output written from the goroutine under test while
// the test goroutine reads it. Both happen concurrently by construction here —
// the sender is parked inside admit when the assertion runs — so the mutex is
// load-bearing, not decoration; without it -race fails the suite.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// records decodes everything logged so far into one map per line.
func (b *syncBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	raw := b.buf.String()
	b.mu.Unlock()

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// matching returns the decoded log lines whose msg equals want.
func (b *syncBuffer) matching(t *testing.T, want string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range b.records(t) {
		if rec["msg"] == want {
			out = append(out, rec)
		}
	}
	return out
}

// captureLogs redirects the default logger into a buffer for the duration of
// the test. Tests using it must not call t.Parallel: slog.SetDefault is
// process-wide.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// newAdmissionSenderWithOpts mirrors newAdmissionSender (admission_test.go) but
// threads construction options through, which is what the saturation signal is
// configured by.
func newAdmissionSenderWithOpts(t *testing.T, maxQueueWait *time.Duration, limiter *RateLimiter, sem Semaphore, opts ...Option) *Sender {
	t.Helper()
	retries := 0
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		Retries:                &retries,
		RequestMinInterval:     &minInterval,
		MaxQueueWait:           maxQueueWait,
		RequestTimeoutDuration: 30 * time.Second,
		RetryMinInterval:       1 * time.Millisecond,
		RetryMaxInterval:       10 * time.Millisecond,
	}
	return New(&http.Client{}, sempCfg, bearerAuth(t), "https://broker.example.com", sem, limiter, opts...)
}

// newSaturationSender builds a Sender with the interim saturation signal on.
func newSaturationSender(t *testing.T, maxQueueWait *time.Duration, limiter *RateLimiter, sem Semaphore) *Sender {
	t.Helper()
	return newAdmissionSenderWithOpts(t, maxQueueWait, limiter, sem,
		WithSaturationEvents(slowAdmissionThreshold))
}

const slowAdmissionMsg = "broker admission slow: request still waiting to be admitted"

// A request held at the rate-limiter gate past the threshold must be reported
// WHILE it waits, not after it resolves. This is the whole point of the signal:
// an operator fielding "MCP feels slow" needs the evidence during the episode.
//
// The assertion runs while the sender is still parked in admit, so a
// resolution-time implementation cannot pass it.
func TestAdmit_SlowAtRateLimitGate_WarnsDuringTheWait(t *testing.T) {
	logs := captureLogs(t)
	sem := NewSemaphore(1)
	sender := newSaturationSender(t, nil, NewRateLimiter(blockedInterval), sem)

	done := make(chan error, 1)
	go func() { done <- sender.admit(context.Background()) }()

	// Wait for the warning to appear, polling rather than sleeping a fixed
	// span so the test is not tuned to a particular machine's speed.
	waitForLogLine(t, logs, slowAdmissionMsg, 1)

	select {
	case err := <-done:
		t.Fatalf("admit returned %v while the limiter was still blocked; the warning must fire during the wait", err)
	default:
	}

	rec := logs.matching(t, slowAdmissionMsg)[0]
	if got := rec["level"]; got != "WARN" {
		t.Errorf("level = %v, want WARN", got)
	}
	if got := rec["stage"]; got != AdmissionStageRateLimit {
		t.Errorf("stage = %v, want %q", got, AdmissionStageRateLimit)
	}
	if _, ok := rec["waited"]; !ok {
		t.Error("log line has no waited field")
	}
}

// The same signal must fire at the concurrency gate, reporting that stage
// rather than the one before it. Without a per-gate stage an operator cannot
// tell "raise max_concurrent_per_broker" from "lower request_min_interval",
// which is the decision this log exists to inform.
func TestAdmit_SlowAtConcurrencyGate_WarnsWithConcurrencyStage(t *testing.T) {
	logs := captureLogs(t)
	// Interval 0 leaves the limiter open, so the request clears the first gate
	// immediately and can only be delayed by the full semaphore.
	sender := newSaturationSender(t, nil, NewRateLimiter(0), fullSemaphore(t))

	done := make(chan error, 1)
	go func() { done <- sender.admit(context.Background()) }()

	waitForLogLine(t, logs, slowAdmissionMsg, 1)

	rec := logs.matching(t, slowAdmissionMsg)[0]
	if got := rec["stage"]; got != AdmissionStageConcurrency {
		t.Errorf("stage = %v, want %q", got, AdmissionStageConcurrency)
	}
}

// One warning per request, however long the wait runs. A signal that re-fires
// on a timer turns a saturated broker into a log flood, which is the failure
// mode that gets an operator to switch the signal back off.
func TestAdmit_SlowAdmission_WarnsOnlyOnce(t *testing.T) {
	logs := captureLogs(t)
	sender := newSaturationSender(t, nil, NewRateLimiter(blockedInterval), NewSemaphore(1))

	go func() { _ = sender.admit(context.Background()) }()

	waitForLogLine(t, logs, slowAdmissionMsg, 1)
	// Stay parked well past several more threshold periods.
	time.Sleep(6 * slowAdmissionThreshold)

	if n := len(logs.matching(t, slowAdmissionMsg)); n != 1 {
		t.Errorf("got %d slow-admission warnings, want exactly 1", n)
	}
}

// The signal must not depend on the outcome. A request that trips the
// threshold and is then shed by the SOL-153442 bound produces both lines: the
// slow warning during the wait, and the shed warning when the budget expires.
func TestAdmit_SlowThenShed_LogsBothSignals(t *testing.T) {
	logs := captureLogs(t)
	budget := 4 * slowAdmissionThreshold
	sender := newSaturationSender(t, durPtr(budget), NewRateLimiter(blockedInterval), NewSemaphore(1))

	err := sender.admit(context.Background())

	var busy *BrokerBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("admit returned %v, want *BrokerBusyError", err)
	}
	if n := len(logs.matching(t, slowAdmissionMsg)); n != 1 {
		t.Errorf("got %d slow-admission warnings, want 1 (the signal must fire even when the request is later shed)", n)
	}
	if n := len(logs.matching(t, "request shed: broker admission bound exceeded")); n != 1 {
		t.Errorf("got %d shed warnings, want 1", n)
	}
}

// With the capability off, nothing is emitted however long the wait runs. The
// signal is opt-in (OBS_SATURATION_EVENTS_ENABLED), so a Sender built without
// the option must stay silent.
func TestAdmit_SaturationEventsDisabled_NeverWarns(t *testing.T) {
	logs := captureLogs(t)
	sender := newAdmissionSenderWithOpts(t, nil, NewRateLimiter(blockedInterval), NewSemaphore(1))

	go func() { _ = sender.admit(context.Background()) }()
	time.Sleep(6 * slowAdmissionThreshold)

	if n := len(logs.matching(t, slowAdmissionMsg)); n != 0 {
		t.Errorf("got %d slow-admission warnings with the signal disabled, want 0", n)
	}
}

// A request admitted promptly must not warn. Guards against a threshold that
// is ignored, which would make every request look like saturation.
func TestAdmit_FastAdmission_DoesNotWarn(t *testing.T) {
	logs := captureLogs(t)
	sender := newSaturationSender(t, nil, NewRateLimiter(0), NewSemaphore(1))

	if err := sender.admit(context.Background()); err != nil {
		t.Fatalf("admit() error = %v, want nil", err)
	}

	if n := len(logs.matching(t, slowAdmissionMsg)); n != 0 {
		t.Errorf("got %d slow-admission warnings for a request admitted immediately, want 0", n)
	}
}

// waitForLogLine blocks until at least n lines with the given msg have been
// logged, or the test times out.
func waitForLogLine(t *testing.T, logs *syncBuffer, msg string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(logs.matching(t, msg)) >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d %q log line(s); got %d", n, msg, len(logs.matching(t, msg)))
}
