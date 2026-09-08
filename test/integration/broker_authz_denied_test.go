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

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkresource "go.opentelemetry.io/otel/sdk/resource"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite"
	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tools"
)

// TestBrokerAuthzDenial_RealHTTPStack_SEMPv2Code72 proves SOL-153332's
// classification survives the FULL real stack a production destructive call
// actually runs through: a real HTTP response, the real sempv2.HTTPClient and
// resilience.Sender, the real composite executor (which wraps every step
// error in fmt.Errorf("tool step %s: %w", ...) before returning it), and
// ToolManager.CallTool itself.
//
// This matters because every unit test in internal/tools/broker_authz_denied_test.go
// injects the raw *sempv2.SEMPError directly from a stub handler — none of
// them exercise the wrapping a real composite (YAML-defined) tool call
// produces on the way up. errors.As is documented to unwrap a %w chain, so
// this should work, but "should" is exactly the gap an integration test at
// this tier exists to close (see README.md) — most of the server's shipped
// destructive tools are composite tools, not native Go handlers, so this is
// the code path that actually matters in production.
func TestBrokerAuthzDenial_RealHTTPStack_SEMPv2Code72(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"meta":{"error":{"code":72,"status":"FORBIDDEN","description":"Insufficient permission"}}}`))
	}))
	defer server.Close()

	cfgYAML := fmt.Sprintf("mcp_client_auth:\n  mode: disabled\nbrokers:\n  dev:\n    url: %s\n    auth:\n      mode: basic\n      username: admin\n      password: admin\n", server.URL)
	cfgPath := filepath.Join(t.TempDir(), "broker-config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	pool := semp.NewBrokerPool(cfg, nil)
	defer pool.Close()

	// A private test operation, mirroring the shape of a real destructive
	// composite tool (config-namespace DELETE, no path parameters — the
	// fixture stays focused on error propagation, not URL construction).
	operations := map[string]*sempv2.Operation{
		"config/deleteTestQueue": {
			ID:     "deleteTestQueue",
			Method: http.MethodDelete,
			Path:   "/SEMP/v2/config/__private_test__/testQueue",
		},
	}
	yes := true
	tool := composite.CompositeTool{
		Name:        "delete-test-queue",
		Description: "fixture: a destructive composite tool",
		Steps: []composite.Step{{
			ID:        "del",
			Operation: "config/deleteTestQueue",
		}},
		Result:      composite.ResultStrategy{Strategy: "collect"},
		Annotations: composite.ToolAnnotations{Destructive: &yes},
	}
	executor := composite.NewCompositeExecutor(operations)

	mp, err := metrics.New("v1.2.3-test", sdkresource.Default())
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}
	tm, err := mp.ToolMetrics()
	if err != nil {
		t.Fatalf("ToolMetrics: %v", err)
	}

	mgr := tools.NewToolManagerFromComposite(pool, []composite.CompositeTool{tool}, executor,
		tools.WithAuditLog(true), tools.WithToolMetrics(tm))

	var logBuf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(oldLogger)

	result, callErr := mgr.CallTool(context.Background(), "delete-test-queue", map[string]any{"broker": "dev"}, tools.Identity{})
	if callErr != nil {
		t.Fatalf("CallTool: %v", callErr)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true for a denied destructive call")
	}
	sc, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent = %#v, want map[string]any", result.StructuredContent)
	}
	if sc["retryable"] != false {
		t.Errorf("retryable = %v, want false", sc["retryable"])
	}

	// Parse every audit line the call produced, exactly as the destructive
	// audit test suite does in internal/tools.
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimRight(logBuf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("undecodable log line %q: %v", line, err)
		}
		if rec["event"] == "audit" {
			records = append(records, rec)
		}
	}

	var ops, denials []map[string]any
	for _, rec := range records {
		switch rec["audit_event_type"] {
		case "operation":
			ops = append(ops, rec)
		case "broker_authz_denied":
			denials = append(denials, rec)
		}
	}

	if len(ops) != 1 {
		t.Fatalf("emitted %d operation record(s) through the real HTTP+composite stack, want exactly 1:\n%v", len(ops), records)
	}
	if got := ops[0]["error_type"]; got != "broker_permission_denied" {
		t.Errorf("operation record error_type = %v, want broker_permission_denied — the real-stack "+
			"error (wrapped in composite's \"tool step %%s: %%w\" and the executor's own return chain) "+
			"was not recognized by isBrokerAuthzDenial's errors.As checks", got)
	}
	if len(denials) != 1 {
		t.Fatalf("emitted %d broker_authz_denied record(s) through the real HTTP+composite stack, want exactly 1:\n%v", len(denials), records)
	}
	if got := denials[0]["broker"]; got != "dev" {
		t.Errorf("broker_authz_denied broker = %v, want dev", got)
	}
	if got := denials[0]["reason"]; got != "permission_denied" {
		t.Errorf("broker_authz_denied reason = %v, want permission_denied", got)
	}

	// Scrape the real Prometheus handler, exactly as a NOC dashboard would,
	// rather than reaching into the provider's internals.
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	mp.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	const wantSeries = `mcp_broker_authz_denied_total{broker="dev",reason="permission_denied",tool="delete-test-queue"} 1`
	if !strings.Contains(body, wantSeries) {
		t.Errorf("mcp_broker_authz_denied_total series not found on a real /metrics scrape.\nwant substring: %s\ngot body:\n%s", wantSeries, body)
	}
}
