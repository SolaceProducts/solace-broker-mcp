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

package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
)

// TestLivezHandler_GET pins the liveness contract: GET returns 200 with the
// exact process-alive body and a JSON content type. The body string is asserted
// verbatim because /livez is the canonical liveness endpoint and container/k8s
// tooling reads the response.
func TestLivezHandler_GET(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/livez", nil)

	LivezHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got, want := rec.Body.String(), `{"status":"alive"}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}
}

// TestLivezHandler_NonGET pins that any non-GET method is rejected with 405.
func TestLivezHandler_NonGET(t *testing.T) {
	t.Parallel()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), method, "/livez", nil)

		LivezHandler().ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
		}
	}
}

// TestHealthHandler_GET pins the legacy /health contract: GET returns 200 with
// the ORIGINAL {"status":"healthy"} body and a JSON content type. The body is
// asserted verbatim because external consumers parse .status == "healthy";
// /health is NOT a body-identical alias of /livez.
func TestHealthHandler_GET(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)

	HealthHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got, want := rec.Body.String(), `{"status":"healthy"}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}
}

// TestHealthHandler_NonGET pins that any non-GET method is rejected with 405.
func TestHealthHandler_NonGET(t *testing.T) {
	t.Parallel()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), method, "/health", nil)

		HealthHandler().ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
		}
	}
}

// TestSaturationEventsEnabled pins that the accessor reflects the
// SaturationEventsEnabled flag rather than a hardcoded constant — both
// directions. There is deliberately no generic Enabled accessor: the
// liveness/readiness probes are unconditional and must never be gated.
func TestSaturationEventsEnabled(t *testing.T) {
	t.Parallel()
	for _, want := range []bool{true, false} {
		cfg := config.ObservabilityConfig{SaturationEventsEnabled: want}
		if got := SaturationEventsEnabled(cfg); got != want {
			t.Errorf("SaturationEventsEnabled() = %v, want %v", got, want)
		}
	}
}
