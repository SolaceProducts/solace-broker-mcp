package semp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
)

func TestBrokerClient_V2_ReturnsClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer server.Close()

	brokerCfg := &config.BrokerConfig{
		URL: server.URL,
		Auth: config.AuthConfig{
			Mode:     "basic",
			Username: "admin",
			Password: "secret",
		},
	}
	sempCfg := testSEMPConfig()

	bc, err := semp.NewBrokerClient("test-broker", brokerCfg, sempCfg)
	if err != nil {
		t.Fatalf("NewBrokerClient() error: %v", err)
	}
	client := bc.SempV2()

	if client == nil {
		t.Fatal("SempV2() returned nil")
	}
}

func TestBrokerClient_V2_ExecutePassesThrough(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	}))
	defer server.Close()

	brokerCfg := &config.BrokerConfig{
		URL: server.URL,
		Auth: config.AuthConfig{
			Mode:     "basic",
			Username: "admin",
			Password: "secret",
		},
	}
	sempCfg := testSEMPConfig()

	bc, err := semp.NewBrokerClient("test-broker", brokerCfg, sempCfg)
	if err != nil {
		t.Fatalf("NewBrokerClient() error: %v", err)
	}
	client := bc.SempV2()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
	}

	_, execErr := client.Execute(context.Background(), op, map[string]any{})
	if execErr != nil {
		t.Fatalf("Execute() error: %v", execErr)
	}

	if !called {
		t.Error("HTTP server was not called — Execute did not pass through to HTTPClient")
	}
}
