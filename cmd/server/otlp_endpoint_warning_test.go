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

package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
)

func captureWarnings(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}

func TestWarnIfOTLPEndpointUnset_FlagOff_NoWarning(t *testing.T) {
	out := captureWarnings(t, func() {
		warnIfOTLPEndpointUnset(config.ObservabilityConfig{MetricsOTLPEnabled: false})
	})
	if strings.Contains(out, "OTLP metrics push") {
		t.Errorf("expected no warning with the flag off, got:\n%s", out)
	}
}

func TestWarnIfOTLPEndpointUnset_FlagOn_EndpointSet_NoWarning(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector:4317")
	out := captureWarnings(t, func() {
		warnIfOTLPEndpointUnset(config.ObservabilityConfig{MetricsOTLPEnabled: true})
	})
	if strings.Contains(out, "OTLP metrics push") {
		t.Errorf("expected no warning with OTEL_EXPORTER_OTLP_ENDPOINT set, got:\n%s", out)
	}
}

func TestWarnIfOTLPEndpointUnset_FlagOn_MetricsSpecificEndpointSet_NoWarning(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "https://collector:4317")
	out := captureWarnings(t, func() {
		warnIfOTLPEndpointUnset(config.ObservabilityConfig{MetricsOTLPEnabled: true})
	})
	if strings.Contains(out, "OTLP metrics push") {
		t.Errorf("expected no warning with OTEL_EXPORTER_OTLP_METRICS_ENDPOINT set, got:\n%s", out)
	}
}

func TestWarnIfOTLPEndpointUnset_FlagOn_NeitherEndpointSet_Warns(t *testing.T) {
	out := captureWarnings(t, func() {
		warnIfOTLPEndpointUnset(config.ObservabilityConfig{MetricsOTLPEnabled: true})
	})
	if !strings.Contains(out, "OTLP metrics push") || !strings.Contains(out, "localhost:4317") {
		t.Errorf("expected a warning naming the SDK's default endpoint, got:\n%s", out)
	}
}
