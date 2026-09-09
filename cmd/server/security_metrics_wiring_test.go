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
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/auth"
	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/audit"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/schema"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
)

// buildSecurityMetrics returns a recorder iff the resolved flag is true and a
// provider exists. The flag's derived default is pinned in
// internal/config/observability_test.go; these tests set the resolved bool.

func securityCfg(counterEnabled, metricsEnabled bool) *config.ServerConfig {
	return &config.ServerConfig{Observability: config.ObservabilityConfig{
		AuthFailureCounterEnabled: counterEnabled,
		MetricsEnabled:            metricsEnabled,
	}}
}

func newTestMetricsProvider(t *testing.T) *metrics.Provider {
	t.Helper()
	p, err := metrics.New("v-test", sdkresource.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p
}

func TestBuildSecurityMetrics_FlagOff_NilEvenWithProvider(t *testing.T) {
	if sm := buildSecurityMetrics(securityCfg(false, true), newTestMetricsProvider(t)); sm != nil {
		t.Errorf("buildSecurityMetrics with flag off = %v, want nil", sm)
	}
}

func TestBuildSecurityMetrics_FlagOnMetricsOff_NilAndWarns(t *testing.T) {
	buf, restore := captureStartupLog(t)
	defer restore()

	if sm := buildSecurityMetrics(securityCfg(true, false), nil); sm != nil {
		t.Fatalf("buildSecurityMetrics with metrics off = %v, want nil", sm)
	}
	if !strings.Contains(buf.String(), "OBS_AUTH_FAILURE_COUNTER_ENABLED is true but OBS_METRICS_ENABLED is false") {
		t.Errorf("no WARN naming the flag was logged:\n%s", buf.String())
	}
}

// Metrics on but no provider means the provider build failed, which main has
// already reported; this must not blame the flag on top of it.
func TestBuildSecurityMetrics_FlagOnProviderBuildFailed_NilAndSilent(t *testing.T) {
	buf, restore := captureStartupLog(t)
	defer restore()

	if sm := buildSecurityMetrics(securityCfg(true, true), nil); sm != nil {
		t.Fatalf("buildSecurityMetrics with no provider = %v, want nil", sm)
	}
	if strings.Contains(buf.String(), "OBS_AUTH_FAILURE_COUNTER_ENABLED") {
		t.Errorf("flag blamed for a provider build failure:\n%s", buf.String())
	}
}

// Flag on with a provider: the recorder is live, every value of the real
// auth-failure vocabulary is seeded, and samples reach /metrics.
func TestBuildSecurityMetrics_FlagOnWithProvider_RecordsOnScrape(t *testing.T) {
	p := newTestMetricsProvider(t)
	sm := buildSecurityMetrics(securityCfg(true, true), p)
	if sm == nil {
		t.Fatal("buildSecurityMetrics with flag on and a provider = nil, want a recorder")
	}

	sm.RecordAuthFailure(context.Background(), schema.AuthFailureReasonSignatureInvalid)
	sm.RecordAuthzDenied(context.Background(), "delete-queue", "not_permitted")

	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, reason := range schema.AuthFailureReasons() {
		n := 0
		if reason == schema.AuthFailureReasonSignatureInvalid {
			n = 1
		}
		if want := fmt.Sprintf("mcp_auth_failure_total{reason=%q} %d", reason, n); !strings.Contains(body, want) {
			t.Errorf("scrape missing %q", want)
		}
	}
	if want := `mcp_authz_denied_total{reason="not_permitted",tool="delete-queue"} 1`; !strings.Contains(body, want) {
		t.Errorf("scrape missing %q", want)
	}
}

// The whole authentication composition, as main wires it: NewAuthMiddleware
// reports rejections to CountingAuthHook, which counts them. Static mode keeps
// the test IdP-free; the no-header case is the one the SDK never hands to a
// verifier at all, so it proves the missing-bearer peek is inside the chain.
func TestCountingAuthHook_WiredThroughAuthMiddleware(t *testing.T) {
	p := newTestMetricsProvider(t)
	sm := buildSecurityMetrics(securityCfg(true, true), p)
	cfg := &config.ServerConfig{
		Port:          9090,
		Observability: config.ObservabilityConfig{AuthFailureCounterEnabled: true, MetricsEnabled: true},
		MCPClientAuth: config.MCPClientAuthConfig{Mode: config.AuthModeStatic, DevToken: "s3cr3t", ResourceURL: "http://localhost:9090/mcp"},
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	handler, err := auth.NewAuthMiddleware(cfg, nil, ok, metrics.CountingAuthHook(sm, audit.NewAuthHook(false)))
	if err != nil {
		t.Fatalf("NewAuthMiddleware: %v", err)
	}
	send := func(authorization string) int {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := send(""); code != http.StatusUnauthorized {
		t.Errorf("no header: status = %d, want 401", code)
	}
	if code := send("Bearer wrong"); code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", code)
	}
	if code := send("Bearer s3cr3t"); code != http.StatusOK {
		t.Errorf("right token: status = %d, want 200", code)
	}

	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`mcp_auth_failure_total{reason="missing"} 1`,
		`mcp_auth_failure_total{reason="invalid_token"} 1`,
		`mcp_auth_failure_total{reason="expired"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape missing %q", want)
		}
	}
}
