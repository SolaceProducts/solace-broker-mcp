package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

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
	mux := buildMux()

	// Register /mcp endpoint for testing (without auth middleware)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		return server
	}, nil)
	mux.Handle("/mcp", mcpHandler)

	return mux
}

func TestHealth_GET_ReturnsOK(t *testing.T) {
	mux := testMux()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /health status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.String() != `{"status": "ok"}` {
		t.Errorf("GET /health body = %q, want %q", rec.Body.String(), `{"status": "ok"}`)
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

func TestUnknownRoute_Returns404(t *testing.T) {
	mux := testMux()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/unknown", nil)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /unknown status = %d, want %d", rec.Code, http.StatusNotFound)
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
