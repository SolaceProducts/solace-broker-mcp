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
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/oteldiag"
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
// otel.SetTracerProvider, which runs later.
//
// Delegates to internal/observability/oteldiag (SOL-152418, Story 46
// extracted the implementation so metrics' own otlpmetricgrpc.New — the
// identical leak surface — can install the same suppression independently
// of whether tracing is even enabled). This wrapper, and this package's own
// tests against it, stay in place unchanged.
func installOTelDiagnostics() {
	oteldiag.Install()
}
