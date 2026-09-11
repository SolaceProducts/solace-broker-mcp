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

package oteldiag

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
)

// syncBuffer is a concurrency-safe io.Writer for capturing slog output —
// otel.Handle and the logr sink can both fire from background goroutines.
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

func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func countOccurrences(buf *syncBuffer, substr string) int {
	n := 0
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

// restoreGlobalOTelDiagnostics saves and restores the process-wide OTel error
// handler, since Install mutates global state every other test in this
// package (and in tracing/metrics, which call it independently) shares.
// otel has no getter for the current global logger (only SetLogger), so its
// cleanup resets to logr.Discard() — a known-safe default — rather than a
// captured previous value.
func restoreGlobalOTelDiagnostics(t *testing.T) {
	t.Helper()
	prevHandler := otel.GetErrorHandler()
	t.Cleanup(func() {
		otel.SetErrorHandler(prevHandler)
		otel.SetLogger(logr.Discard())
	})
}

// TestInstall_SuppressesErrorHandlerText proves the core guarantee: after
// Install, an error routed through otel.Handle (the path otlptracegrpc.New
// and otlpmetricgrpc.New's own env-var parsing both use) never reaches the
// log verbatim — only the fixed, content-free warning does.
func TestInstall_SuppressesErrorHandlerText(t *testing.T) {
	restoreGlobalOTelDiagnostics(t)
	buf := captureLogs(t)

	Install()
	const secret = "Basic%hunter2secret" // %hu is not a valid hex escape
	otel.Handle(errors.New("bad header value: " + secret))

	got := buf.String()
	if strings.Contains(got, secret) {
		t.Fatalf("captured logs contain the raw error text %q, want it suppressed:\n%s", secret, got)
	}
	if n := countOccurrences(buf, "otel sdk emitted an internal diagnostic"); n != 1 {
		t.Fatalf("suppressed-diagnostic warning logged %d times, want exactly 1", n)
	}
}

// TestInstall_SuppressesLoggerText covers the second channel Install routes:
// the logr.Logger the SDK's own components log through directly (distinct
// from otel.Handle above). otel exposes no getter for the global logger it
// was just given (only SetLogger), so this drives the sink directly — the
// same one Install hands to otel.SetLogger — rather than round-tripping
// through global state that cannot be read back.
func TestInstall_SuppressesLoggerText(t *testing.T) {
	buf := captureLogs(t)

	sink := &diagnosticSink{}
	log := logr.New(sink)
	const secret = "authorization=Bearer hunter2secret"
	log.Info("connecting", "target", secret)

	got := buf.String()
	if strings.Contains(got, secret) {
		t.Fatalf("captured logs contain the raw logger text %q, want it suppressed:\n%s", secret, got)
	}
	if n := countOccurrences(buf, "otel sdk emitted an internal diagnostic"); n != 1 {
		t.Fatalf("suppressed-diagnostic warning logged %d times, want exactly 1", n)
	}
}

// TestInstall_RateLimitsAcrossManyCalls proves the one-warning-per-process
// (well, per Install call) behavior holds under repeated firing, not just
// two calls — a component that logs once per retry attempt must not turn
// this into log spam.
func TestInstall_RateLimitsAcrossManyCalls(t *testing.T) {
	restoreGlobalOTelDiagnostics(t)
	buf := captureLogs(t)

	Install()
	for range 10 {
		otel.Handle(errors.New("boom"))
	}

	if n := countOccurrences(buf, "otel sdk emitted an internal diagnostic"); n != 1 {
		t.Fatalf("suppressed-diagnostic warning logged %d times across 10 firings, want exactly 1", n)
	}
}

// TestInstall_IdempotentAcrossCallers pins the property both tracing and
// metrics depend on: calling Install a second time (modelling "both
// tracing and OTLP metrics are enabled, each installing independently")
// does not break suppression or double-log.
func TestInstall_IdempotentAcrossCallers(t *testing.T) {
	restoreGlobalOTelDiagnostics(t)
	buf := captureLogs(t)

	Install()
	Install()
	const secret = "hunter2secret"
	otel.Handle(errors.New(secret))

	got := buf.String()
	if strings.Contains(got, secret) {
		t.Fatalf("captured logs contain the raw error text %q after a second Install, want it suppressed:\n%s", secret, got)
	}
	if n := countOccurrences(buf, "otel sdk emitted an internal diagnostic"); n != 1 {
		t.Fatalf("suppressed-diagnostic warning logged %d times after a second Install, want exactly 1", n)
	}
}
