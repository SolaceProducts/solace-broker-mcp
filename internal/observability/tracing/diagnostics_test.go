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
	"context"
	"strings"
	"testing"
	"time"

	sdkresource "go.opentelemetry.io/otel/sdk/resource"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
)

// TestNew_MalformedOTLPHeaders_DoesNotLeakToStderr reproduces the exact
// finding from review: OTEL_EXPORTER_OTLP_HEADERS with a value that isn't
// valid percent-encoding makes otlptracegrpc.New's own env-var parsing log
// the raw value through the OTel SDK's global error/log channel — a path
// that bypasses installOTelDiagnostics's caller entirely if that channel
// isn't routed first. Two malformed headers exercise both the suppression
// and the rate limiting in one call.
func TestNew_MalformedOTLPHeaders_DoesNotLeakToStderr(t *testing.T) {
	withRestoredGlobalTracer(t)
	buf := captureLogs(t)

	const secret = "Basic%hunter2secret" // %hu is not a valid hex escape
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization="+secret+",second="+secret)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")

	cfg := config.ObservabilityConfig{TracingEnabled: true, MetricsEnabled: true, OTelSelfStatsIntervalS: 60}
	p, err := New(cfg, nil, sdkresource.Default())
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})

	got := buf.String()
	if strings.Contains(got, secret) {
		t.Fatalf("captured logs contain the raw malformed header value %q, want it suppressed:\n%s", secret, got)
	}
	if n := countOccurrences(buf, "otel sdk emitted an internal diagnostic"); n != 1 {
		t.Fatalf("suppressed-diagnostic warning logged %d times, want exactly 1 (rate-limited across both malformed headers):\n%s", n, got)
	}
}
