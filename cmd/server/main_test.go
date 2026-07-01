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
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/observability/health"
	"github.com/SolaceDev/solace-broker-mcp/internal/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// testMux creates a mux using the shared buildMux function with a minimal
// MCP server. This ensures tests use the same route definitions as main().
func testMux() *http.ServeMux {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "solace-broker-mcp",
		Version: version.Version(),
	}, nil)
	readiness := health.NewReadinessState()
	readiness.SetInitialized()
	mux := buildMux(func() []string { return nil }, func(_ context.Context, _ string) error { return nil }, readiness)

	// Register /mcp endpoint for testing (without auth middleware)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		return server
	}, nil)
	mux.Handle("/mcp", mcpHandler)

	return mux
}

// TestLivez_GET_ReturnsOK pins the canonical liveness endpoint: 200 with the
// exact process-alive body and JSON content type.
func TestLivez_GET_ReturnsOK(t *testing.T) {
	mux := testMux()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/livez", nil)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /livez status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.String() != `{"status":"alive"}` {
		t.Errorf("GET /livez body = %q, want %q", rec.Body.String(), `{"status":"alive"}`)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("GET /livez Content-Type = %q, want %q", ct, "application/json")
	}
}

func TestLivez_POST_ReturnsMethodNotAllowed(t *testing.T) {
	mux := testMux()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/livez", nil)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /livez status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

// TestHealth_GET_ReturnsOK pins that /health preserves its ORIGINAL shipped body
// {"status":"healthy"} — it is NOT a body-identical alias of /livez. /health is
// retained for backward compatibility so external consumers that parse
// .status == "healthy" keep working; /livez is the canonical liveness endpoint
// and returns {"status":"alive"}.
func TestHealth_GET_ReturnsOK(t *testing.T) {
	mux := testMux()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /health status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.String() != `{"status":"healthy"}` {
		t.Errorf("GET /health body = %q, want %q", rec.Body.String(), `{"status":"healthy"}`)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("GET /health Content-Type = %q, want %q", ct, "application/json")
	}
}

func TestHealth_POST_ReturnsMethodNotAllowed(t *testing.T) {
	mux := testMux()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/health", nil)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /health status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

// TestLivezAndHealthShareStatusButNotBody proves the back-compat contract:
// /livez and /health share status (200 GET, 405 non-GET) and Content-Type, but
// their bodies DIFFER deliberately — /livez returns {"status":"alive"} (canonical
// liveness) and /health returns {"status":"healthy"} (original shipped body
// retained for external consumers). They are NOT body-identical aliases.
func TestLivezAndHealthShareStatusButNotBody(t *testing.T) {
	mux := testMux()

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		livezRec := httptest.NewRecorder()
		mux.ServeHTTP(livezRec, httptest.NewRequestWithContext(context.Background(), method, "/livez", nil))

		healthRec := httptest.NewRecorder()
		mux.ServeHTTP(healthRec, httptest.NewRequestWithContext(context.Background(), method, "/health", nil))

		if livezRec.Code != healthRec.Code {
			t.Errorf("%s: status /livez=%d /health=%d, want equal", method, livezRec.Code, healthRec.Code)
		}
		if livezRec.Header().Get("Content-Type") != healthRec.Header().Get("Content-Type") {
			t.Errorf("%s: Content-Type /livez=%q /health=%q, want equal",
				method, livezRec.Header().Get("Content-Type"), healthRec.Header().Get("Content-Type"))
		}
	}

	// On GET the bodies must differ: /livez is the canonical alive signal,
	// /health preserves its original healthy body for back-compat.
	livezGet := httptest.NewRecorder()
	mux.ServeHTTP(livezGet, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/livez", nil))
	healthGet := httptest.NewRecorder()
	mux.ServeHTTP(healthGet, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil))

	if got, want := livezGet.Body.String(), `{"status":"alive"}`; got != want {
		t.Errorf("GET /livez body = %q, want %q", got, want)
	}
	if got, want := healthGet.Body.String(), `{"status":"healthy"}`; got != want {
		t.Errorf("GET /health body = %q, want %q", got, want)
	}
	if livezGet.Body.String() == healthGet.Body.String() {
		t.Errorf("/livez and /health bodies must differ, both = %q", livezGet.Body.String())
	}
}

func TestMCPEndpoint_POST_ReachesMCPHandler(t *testing.T) {
	mux := testMux()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	// The MCP handler should respond — not 404. The exact status depends on
	// the request body (we sent no body), but anything other than 404 proves
	// the route is wired correctly.
	if rec.Code == http.StatusNotFound {
		t.Error("POST /mcp returned 404 — route not registered")
	}
}

func TestNewHTTPServer_TimeoutsAreSet(t *testing.T) {
	// Asserts the production timeout posture: ReadHeaderTimeout / ReadTimeout
	// / IdleTimeout are all non-zero (close Slowloris-class and idle
	// keep-alive exhaustion vectors), and WriteTimeout is deliberately zero
	// (preserves long-lived MCP streamable HTTP / SSE responses).
	srv := newHTTPServer(":0", http.NewServeMux())

	if want := 10 * time.Second; srv.ReadHeaderTimeout != want {
		t.Errorf("ReadHeaderTimeout = %s, want %s", srv.ReadHeaderTimeout, want)
	}
	if want := 30 * time.Second; srv.ReadTimeout != want {
		t.Errorf("ReadTimeout = %s, want %s", srv.ReadTimeout, want)
	}
	if want := 120 * time.Second; srv.IdleTimeout != want {
		t.Errorf("IdleTimeout = %s, want %s", srv.IdleTimeout, want)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %s, want 0 (intentional — preserves SSE streams)", srv.WriteTimeout)
	}
}

func TestUnknownRoute_Returns404(t *testing.T) {
	mux := testMux()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/unknown", nil)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /unknown status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestStartServer_PortConflict_SendsToChannel(t *testing.T) {
	// Occupy a port to force an "address already in use" startup error.
	lc := &net.ListenConfig{}
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	srv := &http.Server{
		Addr:    l.Addr().String(),
		Handler: http.NewServeMux(),
	}

	errCh := startServer(srv, "", "")

	select {
	case startErr := <-errCh:
		if startErr == nil {
			t.Fatal("expected error for port conflict, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: expected startup error to be sent to channel, not os.Exit")
	}
}

func TestStartServer_NormalShutdown_NoErrorSent(t *testing.T) {
	// Verify that ErrServerClosed (the result of a clean Shutdown) is filtered
	// out and not sent to the error channel.
	//
	// We call Shutdown before startServer so http.Server.ListenAndServe sees
	// shuttingDown=true on entry and returns ErrServerClosed without binding
	// any port. This exercises the same filter path without the port-reuse
	// race of binding-then-rebinding to the same address.
	srv := &http.Server{
		Addr:    "127.0.0.1:0",
		Handler: http.NewServeMux(),
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	errCh := startServer(srv, "", "")

	select {
	case startErr := <-errCh:
		t.Errorf("expected no error for ErrServerClosed, got: %v", startErr)
	case <-time.After(200 * time.Millisecond):
		// expected: ListenAndServe returned ErrServerClosed and was filtered out
	}
}

func TestHealthConfigFromFile_ReadsPortFromYAML(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("port: 8080\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_FILE", cfgPath)

	got := healthConfigFromFile()
	if got.Port != 8080 {
		t.Errorf("Port = %d, want 8080", got.Port)
	}
	if got.Scheme != "http" {
		t.Errorf("Scheme = %q, want http", got.Scheme)
	}
}

func TestHealthConfigFromFile_ReturnsZeroPortWhenNoPortField(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("log_level: info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_FILE", cfgPath)

	got := healthConfigFromFile()
	if got.Port != 0 {
		t.Errorf("Port = %d, want 0", got.Port)
	}
}

func TestHealthConfigFromFile_ReturnsDefaultsWhenNoConfigFound(t *testing.T) {
	t.Setenv("CONFIG_FILE", "")
	origDir, _ := os.Getwd()
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(origDir) }) //nolint:errcheck

	got := healthConfigFromFile()
	if got.Port != 0 {
		t.Errorf("Port = %d, want 0", got.Port)
	}
	if got.Scheme != "http" {
		t.Errorf("Scheme = %q, want http", got.Scheme)
	}
}

func TestHealthConfigFromFile_ReturnsDefaultsOnInvalidYAML(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("{{invalid yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_FILE", cfgPath)

	got := healthConfigFromFile()
	if got.Port != 0 {
		t.Errorf("Port = %d, want 0", got.Port)
	}
	if got.Scheme != "http" {
		t.Errorf("Scheme = %q, want http", got.Scheme)
	}
}

func TestHealthConfigFromFile_LocalPathFallback(t *testing.T) {
	t.Setenv("CONFIG_FILE", "")
	origDir, _ := os.Getwd()
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(origDir) }) //nolint:errcheck

	if err := os.WriteFile("broker-config.yaml", []byte("port: 7070\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := healthConfigFromFile()
	if got.Port != 7070 {
		t.Errorf("Port = %d, want 7070", got.Port)
	}
}

func TestHealthConfigFromFile_DetectsTLS(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := "port: 9090\ntls_cert_file: /etc/certs/server.pem\ntls_key_file: /etc/certs/server-key.pem\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_FILE", cfgPath)

	got := healthConfigFromFile()
	if got.Scheme != "https" {
		t.Errorf("Scheme = %q, want https", got.Scheme)
	}
	if got.Port != 9090 {
		t.Errorf("Port = %d, want 9090", got.Port)
	}
}

func TestHealthConfigFromFile_PartialTLSIsHTTP(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := "port: 9090\ntls_cert_file: /etc/certs/server.pem\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_FILE", cfgPath)

	got := healthConfigFromFile()
	if got.Scheme != "http" {
		t.Errorf("Scheme = %q, want http (only cert set, no key)", got.Scheme)
	}
}

func TestReady_POST_ReturnsMethodNotAllowed(t *testing.T) {
	mux := buildMux(func() []string { return nil }, func(_ context.Context, _ string) error { return nil }, health.NewReadinessState())
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/ready", nil)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /ready status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestReady_GET_AllBrokersReachable(t *testing.T) {
	brokers := []string{"prod-1", "prod-2"}
	mux := buildMux(
		func() []string { return brokers },
		func(_ context.Context, _ string) error { return nil },
		health.NewReadinessState(),
	)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/ready", nil)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /ready status = %d, want %d", rec.Code, http.StatusOK)
	}
	if contentType := rec.Header().Get("Content-Type"); contentType != "application/json" {
		t.Errorf("GET /ready Content-Type = %q, want %q", contentType, "application/json")
	}
	var body struct {
		Ready   bool     `json:"ready"`
		Brokers []string `json:"brokers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if !body.Ready {
		t.Errorf("ready = %v, want true", body.Ready)
	}
	if len(body.Brokers) != len(brokers) {
		t.Errorf("brokers len = %d, want %d", len(body.Brokers), len(brokers))
	}
	for i, expectedBroker := range brokers {
		if body.Brokers[i] != expectedBroker {
			t.Errorf("brokers[%d] = %q, want %q", i, body.Brokers[i], expectedBroker)
		}
	}
}

func TestReady_GET_OneBrokerUnreachable(t *testing.T) {
	brokers := []string{"prod-1", "prod-2"}
	mux := buildMux(
		func() []string { return brokers },
		func(_ context.Context, broker string) error {
			if broker == "prod-2" {
				return errors.New("connection refused")
			}
			return nil
		},
		health.NewReadinessState(),
	)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/ready", nil)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /ready status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var body struct {
		Ready  bool     `json:"ready"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if body.Ready {
		t.Errorf("ready = %v, want false", body.Ready)
	}
	if len(body.Errors) == 0 {
		t.Fatal("errors array is empty, want at least one entry")
	}
	want := "prod-2: unreachable"
	if body.Errors[0] != want {
		t.Errorf("errors[0] = %q, want %q", body.Errors[0], want)
	}
}

// TestReadyz_BeforeInit_Returns503 pins that /readyz wired through the real
// buildMux returns 503 {"status":"starting"} before SetInitialized.
func TestReadyz_BeforeInit_Returns503(t *testing.T) {
	readiness := health.NewReadinessState() // not initialized
	mux := buildMux(func() []string { return nil }, func(_ context.Context, _ string) error { return nil }, readiness)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if rec.Body.String() != `{"status":"starting"}` {
		t.Errorf("GET /readyz body = %q, want %q", rec.Body.String(), `{"status":"starting"}`)
	}
}

// TestReadyz_AfterInit_Returns200 pins that /readyz wired through the real
// buildMux returns 200 {"status":"ready"} once SetInitialized has been called.
func TestReadyz_AfterInit_Returns200(t *testing.T) {
	readiness := health.NewReadinessState()
	readiness.SetInitialized()
	mux := buildMux(func() []string { return nil }, func(_ context.Context, _ string) error { return nil }, readiness)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /readyz status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.String() != `{"status":"ready"}` {
		t.Errorf("GET /readyz body = %q, want %q", rec.Body.String(), `{"status":"ready"}`)
	}
}

// TestReadyz_BrokerUnreachable_StillReady proves AC #4 through the real mux:
// even when the broker probe always errors (every broker unreachable), /readyz
// returns 200 once initialized and not draining, because readiness is decoupled
// from broker reachability. The same mux's /ready endpoint returns 503 for the
// same unreachable broker, demonstrating the two endpoints are independent.
func TestReadyz_BrokerUnreachable_StillReady(t *testing.T) {
	readiness := health.NewReadinessState()
	readiness.SetInitialized()
	alwaysFail := func(_ context.Context, broker string) error {
		return errors.New("connection refused")
	}
	mux := buildMux(func() []string { return []string{"prod-1"} }, alwaysFail, readiness)

	// /readyz ignores the broker entirely → 200 ready.
	readyzReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil)
	readyzRec := httptest.NewRecorder()
	mux.ServeHTTP(readyzRec, readyzReq)
	if readyzRec.Code != http.StatusOK {
		t.Errorf("GET /readyz status = %d, want %d (readiness must not depend on broker reachability)", readyzRec.Code, http.StatusOK)
	}
	if readyzRec.Body.String() != `{"status":"ready"}` {
		t.Errorf("GET /readyz body = %q, want %q", readyzRec.Body.String(), `{"status":"ready"}`)
	}

	// /ready (legacy, broker-coupled) reflects the unreachable broker → 503.
	readyReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/ready", nil)
	readyRec := httptest.NewRecorder()
	mux.ServeHTTP(readyRec, readyReq)
	if readyRec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /ready status = %d, want %d (legacy probe is broker-coupled)", readyRec.Code, http.StatusServiceUnavailable)
	}
}

// probeTestConfig writes a minimal YAML config with the given brokers and
// returns the loaded *config.ServerConfig. Each broker entry is an
// (alias, url) pair.
// probeTestConfig writes a minimal YAML config with the given brokers and
// returns the loaded *config.ServerConfig. Each entry is an [alias, url] pair.
func probeTestConfig(t *testing.T, brokers ...[2]string) *config.ServerConfig {
	t.Helper()
	var yamlContent string
	yamlContent += "mcp_client_auth:\n  mode: disabled\nbrokers:\n"
	for _, broker := range brokers {
		alias, brokerURL := broker[0], broker[1]
		yamlContent += fmt.Sprintf("  %s:\n    url: %s\n    auth:\n      mode: basic\n      username: admin\n      password: secret\n", alias, brokerURL)
	}
	path := filepath.Join(t.TempDir(), "broker-config.yaml")
	if err := os.WriteFile(path, []byte(yamlContent), os.FileMode(0o600)); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

func TestNewBrokerReachabilityProbe_UnknownBroker(t *testing.T) {
	cfg := probeTestConfig(t, [2]string{"existing", "http://127.0.0.1:8080"})
	probe := newBrokerReachabilityProbe(cfg)

	err := probe(context.Background(), "no-such-broker")
	if err == nil {
		t.Fatal("expected error for unknown broker, got nil")
	}
	if want := `unknown broker "no-such-broker"`; err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestNewBrokerReachabilityProbe_ExplicitPort(t *testing.T) {
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	addr := l.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	cfg := probeTestConfig(t, [2]string{"broker-a", "http://127.0.0.1:" + port})
	probe := newBrokerReachabilityProbe(cfg)

	if err := probe(context.Background(), "broker-a"); err != nil {
		t.Errorf("expected success for reachable broker, got: %v", err)
	}
}

func TestNewBrokerReachabilityProbe_HTTPSDefaultsTo443(t *testing.T) {
	cfg := probeTestConfig(t, [2]string{"broker-a", "https://192.0.2.1"})
	probe := newBrokerReachabilityProbe(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := probe(ctx, "broker-a")
	if err == nil {
		t.Fatal("expected dial error to 192.0.2.1:443, got nil")
	}
}

func TestNewBrokerReachabilityProbe_HTTPDefaultsTo80(t *testing.T) {
	cfg := probeTestConfig(t, [2]string{"broker-a", "http://192.0.2.1"})
	probe := newBrokerReachabilityProbe(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := probe(ctx, "broker-a")
	if err == nil {
		t.Fatal("expected dial error to 192.0.2.1:80, got nil")
	}
}
