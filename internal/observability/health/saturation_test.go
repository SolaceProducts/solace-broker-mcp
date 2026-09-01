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

package health_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/health"
)

const occupancyMsg = "broker in-flight occupancy"

// reportInterval is short enough to keep the suite fast; every test either
// waits for a line to appear or asserts none appears over several intervals.
const reportInterval = 10 * time.Millisecond

// syncBuffer collects log output written by the reporter goroutine while the
// test goroutine reads it. The mutex is load-bearing: without it -race fails.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) matching(t *testing.T, want string) []map[string]any {
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
		if rec["msg"] == want {
			out = append(out, rec)
		}
	}
	return out
}

// captureLogs redirects the default logger for the duration of the test. Tests
// using it must not call t.Parallel: slog.SetDefault is process-wide.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func enabledCfg() config.ObservabilityConfig {
	return config.ObservabilityConfig{SaturationEventsEnabled: true}
}

func staticSnapshot(occ ...health.BrokerOccupancy) health.OccupancySnapshot {
	return func() []health.BrokerOccupancy { return occ }
}

func waitForLine(t *testing.T, logs *syncBuffer, msg string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := logs.matching(t, msg); len(got) > 0 {
			return got[0]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for a %q log line", msg)
	return nil
}

// A broker with requests in flight is reported, naming the broker by its
// configured alias and carrying both the current occupancy and the cap. The cap
// is what makes the number readable: "8 in flight" means nothing to an operator
// who does not also know the limit is 10.
func TestStartOccupancyReporter_ReportsBusyBroker(t *testing.T) {
	logs := captureLogs(t)
	stop := health.StartOccupancyReporter(enabledCfg(), reportInterval,
		staticSnapshot(health.BrokerOccupancy{Broker: "prod-east", InFlight: 3, Limit: 10}))
	defer stop()

	rec := waitForLine(t, logs, occupancyMsg)
	if got := rec["broker"]; got != "prod-east" {
		t.Errorf("broker = %v, want prod-east", got)
	}
	if got := rec["in_flight"]; got != float64(3) {
		t.Errorf("in_flight = %v, want 3", got)
	}
	if got := rec["limit"]; got != float64(10) {
		t.Errorf("limit = %v, want 10", got)
	}
	if got := rec["level"]; got != "INFO" {
		t.Errorf("level = %v, want INFO for a broker below its cap", got)
	}
}

// A full semaphore is the actionable state: every further request to that
// broker now queues at the concurrency gate. It must be distinguishable from
// ordinary load without parsing the numbers, so it logs at WARN.
func TestStartOccupancyReporter_WarnsWhenBrokerIsAtItsCap(t *testing.T) {
	logs := captureLogs(t)
	stop := health.StartOccupancyReporter(enabledCfg(), reportInterval,
		staticSnapshot(health.BrokerOccupancy{Broker: "prod-east", InFlight: 10, Limit: 10}))
	defer stop()

	rec := waitForLine(t, logs, occupancyMsg)
	if got := rec["level"]; got != "WARN" {
		t.Errorf("level = %v, want WARN for a broker at its cap", got)
	}
}

// An idle broker is not reported. Logging every configured broker every
// interval forever would bury the signal in its own noise; the presence of a
// line is itself the information that a broker is carrying load.
func TestStartOccupancyReporter_SkipsIdleBrokers(t *testing.T) {
	logs := captureLogs(t)
	stop := health.StartOccupancyReporter(enabledCfg(), reportInterval,
		staticSnapshot(
			health.BrokerOccupancy{Broker: "idle", InFlight: 0, Limit: 10},
			health.BrokerOccupancy{Broker: "busy", InFlight: 1, Limit: 10},
		))
	defer stop()

	waitForLine(t, logs, occupancyMsg)
	time.Sleep(3 * reportInterval)

	for _, rec := range logs.matching(t, occupancyMsg) {
		if rec["broker"] == "idle" {
			t.Fatal("an idle broker was reported; only brokers carrying load should be")
		}
	}
}

// The signal is opt-in. With OBS_SATURATION_EVENTS_ENABLED off nothing is
// emitted, and no goroutine is started to emit it.
func TestStartOccupancyReporter_Disabled_EmitsNothing(t *testing.T) {
	logs := captureLogs(t)
	var called atomic.Bool
	stop := health.StartOccupancyReporter(config.ObservabilityConfig{}, reportInterval,
		func() []health.BrokerOccupancy {
			called.Store(true)
			return []health.BrokerOccupancy{{Broker: "prod-east", InFlight: 5, Limit: 10}}
		})
	defer stop()

	time.Sleep(5 * reportInterval)

	if called.Load() {
		t.Error("the snapshot function was called with the capability disabled")
	}
	if n := len(logs.matching(t, occupancyMsg)); n != 0 {
		t.Errorf("got %d occupancy lines with the capability disabled, want 0", n)
	}
}

// stop must actually halt reporting, and must be safe to call more than once —
// it is wired to a defer in main alongside pool.Close.
func TestStartOccupancyReporter_StopHaltsReportingAndIsIdempotent(t *testing.T) {
	logs := captureLogs(t)
	stop := health.StartOccupancyReporter(enabledCfg(), reportInterval,
		staticSnapshot(health.BrokerOccupancy{Broker: "prod-east", InFlight: 1, Limit: 10}))

	waitForLine(t, logs, occupancyMsg)
	stop()
	stop() // must not panic or double-close.

	after := len(logs.matching(t, occupancyMsg))
	time.Sleep(5 * reportInterval)

	if got := len(logs.matching(t, occupancyMsg)); got != after {
		t.Errorf("reporter emitted %d more lines after stop(), want 0", got-after)
	}
}

// A nil snapshot is a wiring mistake, not a reason to panic on a background
// goroutine where the crash would surface far from its cause.
func TestStartOccupancyReporter_NilSnapshot_IsInert(t *testing.T) {
	logs := captureLogs(t)
	stop := health.StartOccupancyReporter(enabledCfg(), reportInterval, nil)
	defer stop()

	time.Sleep(5 * reportInterval)

	if n := len(logs.matching(t, occupancyMsg)); n != 0 {
		t.Errorf("got %d occupancy lines from a nil snapshot, want 0", n)
	}
}
