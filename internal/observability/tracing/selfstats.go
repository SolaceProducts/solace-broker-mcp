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
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// startSelfStatsEmitter starts the periodic INFO fallback (Decision #12):
// when there is no meter provider to register the export counters against,
// there is no /metrics surface to read span-export health from, so this
// goroutine logs the current totals on a timer instead. Reports once
// immediately so an operator who flips tracing on mid-incident does not wait
// a full interval for the first reading — same rationale as
// health.StartOccupancyReporter.
//
// Returns a stop function that halts the goroutine and waits for it to exit;
// stop is idempotent. Call only when the caller has already checked
// interval > 0 and meterProvider == nil (provider.go) — that condition
// covers metrics being off and metrics being configured but its provider
// failing to build, not just cfg.MetricsEnabled being false.
func startSelfStatsEmitter(interval time.Duration, stats *exportStats) (stop func()) {
	done := make(chan struct{})
	var exited sync.WaitGroup
	exited.Add(1)

	emit := func() { safeRun(func() { emitSelfStats(stats) }) }

	go func() {
		defer exited.Done()
		emit()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				emit()
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		exited.Wait()
	}
}

// safeRun recovers a panic in fn rather than letting it escape, so one bad
// call logs and the caller moves on instead of the whole goroutine dying —
// called once per tick here, not once around the whole goroutine, so a single
// bad tick does not silently end all future reporting. Mirrors safego.Go's
// logging shape (event, panic TYPE, stack) — safego.Go itself is
// errgroup-only and doesn't fit this long-running, ticker-driven goroutine.
func safeRun(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("recovered panic in otel self-stats emitter",
				slog.String("event", "panic_recovered"),
				slog.String("panic_type", fmt.Sprintf("%T", r)),
				slog.String("stack", string(debug.Stack())))
		}
	}()
	fn()
}

// emitSelfStats logs one event=otel_self_stats INFO line with the current
// totals, so an operator reading kubectl logs can diagnose OTLP export health
// without flipping metrics on.
func emitSelfStats(stats *exportStats) {
	exported, queueFull, exportTimeout, exportError, shutdown := stats.snapshot()
	slog.Info("otel self stats",
		slog.String("event", "otel_self_stats"),
		slog.Int64("spans_exported_total", exported),
		slog.Int64("spans_dropped_queue_full_total", queueFull),
		slog.Int64("spans_dropped_export_timeout_total", exportTimeout),
		slog.Int64("spans_dropped_export_error_total", exportError),
		slog.Int64("spans_dropped_shutdown_total", shutdown))
}
