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

// Package oteldiag routes the OTel SDK's own internal error handler and logr
// logger into slog, in place of their defaults — which print raw text
// straight to stderr, outside this project's structured logging and its
// Rule 3 ReplaceAttr redaction net.
//
// Extracted from internal/observability/tracing (SOL-152420, Story 25) to
// SOL-152418 (Story 46): otlpmetricgrpc.New has the identical leak surface
// otlptracegrpc.New does (both parse OTEL_EXPORTER_OTLP_* env vars through
// the same global channel before either constructor can return an error a
// caller could suppress), and the two providers construct independently in
// cmd/server/main.go — metrics before tracing. A customer running
// OBS_METRICS_OTLP_ENABLED=true with OBS_TRACING_ENABLED=false (a real,
// plausible combination) would otherwise never reach tracing.New's call to
// this installer at all, leaving the leak open for exactly the deployment
// shape this story targets. One shared installer, called by both providers'
// constructors, closes it regardless of which capability is on.
package oteldiag

import (
	"log/slog"
	"sync/atomic"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
)

// Install routes the OTel SDK's global error handler and logr logger into
// slog. Idempotent and safe to call from more than one caller (metrics and
// tracing both do, each before its own OTLP exporter construction): the
// underlying otel.SetErrorHandler/otel.SetLogger calls are simple global
// setters, so a second call just installs an equivalent sink, and whichever
// caller runs last is a harmless implementation detail — the security
// property (nothing this channel carries is ever logged) holds regardless of
// which sink instance is currently active.
//
// Must be called before the first SDK call that can trigger either channel
// — otlptracegrpc.New or otlpmetricgrpc.New — not merely before
// otel.SetTracerProvider/a meter provider existing, which runs later. A
// malformed OTEL_EXPORTER_OTLP_HEADERS entry (a token pasted without
// percent-encoding is the realistic trigger) reaches it verbatim if this
// runs even one line late.
//
// Deliberately reports no error text, attribute values, or keys-and-values:
// the whole point is that this channel cannot be trusted to carry only safe
// material, so nothing it carries is logged, only the fact that it fired.
// One line per call to New is enough to make the condition visible to an
// operator without echoing whatever novel case triggered it next time.
func Install() {
	sink := &diagnosticSink{}
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) { sink.warn() }))
	otel.SetLogger(logr.New(sink))
}

// diagnosticSink is a logr.LogSink that discards everything it is given and
// emits a single fixed slog.Warn the first time it is called. See Install
// for why no argument it receives is ever logged.
type diagnosticSink struct {
	reported atomic.Bool
}

func (s *diagnosticSink) warn() {
	if s.reported.CompareAndSwap(false, true) {
		slog.Warn("otel sdk emitted an internal diagnostic on its own error/log channel; suppressed here because that channel is not audited for OTLP headers or endpoint credentials — see docs/observability.md")
	}
}

func (s *diagnosticSink) Init(logr.RuntimeInfo)       {}
func (s *diagnosticSink) Enabled(int) bool            { return true }
func (s *diagnosticSink) Info(int, string, ...any)    { s.warn() }
func (s *diagnosticSink) Error(error, string, ...any) { s.warn() }

// WithValues and WithName return the same sink rather than a derived one:
// there are no per-call values to carry since nothing this sink receives is
// ever logged.
func (s *diagnosticSink) WithValues(...any) logr.LogSink { return s }
func (s *diagnosticSink) WithName(string) logr.LogSink   { return s }
