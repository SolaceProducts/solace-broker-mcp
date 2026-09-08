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
	"log/slog"
	"sync/atomic"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
)

// installOTelDiagnostics routes the OTel SDK's own internal error handler
// and logr logger into slog, in place of their defaults — which print raw
// text straight to stderr, outside this project's structured logging and
// its Rule 3 ReplaceAttr redaction net.
//
// This exists because main.go's decision to suppress otlptracegrpc.New's
// returned error (it "can echo back OTEL_EXPORTER_OTLP_HEADERS") only closes
// one leak: the SDK's env-var parsing logs through this global channel
// *inside* that constructor, on the line before it returns, so the same
// header value can already be on stderr by the time the caller's error
// value even exists (flagged by review). A malformed
// OTEL_EXPORTER_OTLP_HEADERS entry — a token pasted without percent-encoding
// is the realistic trigger — reaches it verbatim.
//
// Must be called before the first SDK call that can trigger either channel,
// which is otlptracegrpc.New itself — not merely before
// otel.SetTracerProvider, which runs later. Deliberately reports no error
// text, attribute values, or keys-and-values: the whole point is that this
// channel cannot be trusted to carry only safe material, so nothing it
// carries is logged, only the fact that it fired. One line per call to New
// is enough to make the condition visible to an operator without echoing
// whatever novel case triggered it next time.
func installOTelDiagnostics() {
	sink := &otelDiagnosticSink{}
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) { sink.warn() }))
	otel.SetLogger(logr.New(sink))
}

// otelDiagnosticSink is a logr.LogSink that discards everything it is given
// and emits a single fixed slog.Warn the first time it is called. See
// installOTelDiagnostics for why no argument it receives is ever logged.
type otelDiagnosticSink struct {
	reported atomic.Bool
}

func (s *otelDiagnosticSink) warn() {
	if s.reported.CompareAndSwap(false, true) {
		slog.Warn("otel sdk emitted an internal diagnostic on its own error/log channel; suppressed here because that channel is not audited for OTLP headers or endpoint credentials — see docs/observability.md")
	}
}

func (s *otelDiagnosticSink) Init(logr.RuntimeInfo)      {}
func (s *otelDiagnosticSink) Enabled(int) bool           { return true }
func (s *otelDiagnosticSink) Info(int, string, ...any)   { s.warn() }
func (s *otelDiagnosticSink) Error(error, string, ...any) { s.warn() }

// WithValues and WithName return the same sink rather than a derived one:
// there are no per-call values to carry since nothing this sink receives is
// ever logged.
func (s *otelDiagnosticSink) WithValues(...any) logr.LogSink { return s }
func (s *otelDiagnosticSink) WithName(string) logr.LogSink   { return s }
