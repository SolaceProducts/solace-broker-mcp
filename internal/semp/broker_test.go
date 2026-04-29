package semp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
	sempCfg := &config.SEMPConfig{RequestTimeoutDuration: 5 * time.Second}

	bc, err := semp.NewBrokerClient("test-broker", brokerCfg, sempCfg)
	if err != nil {
		t.Fatalf("NewBrokerClient() error: %v", err)
	}
	client := bc.SEMPv2()

	if client == nil {
		t.Fatal("SEMPv2() returned nil")
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
	sempCfg := &config.SEMPConfig{RequestTimeoutDuration: 5 * time.Second}

	bc, err := semp.NewBrokerClient("test-broker", brokerCfg, sempCfg)
	if err != nil {
		t.Fatalf("NewBrokerClient() error: %v", err)
	}
	client := bc.SEMPv2()

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

// TestBrokerClient_V1_ReturnsClient verifies that NewBrokerClient populates
// the SEMPv1 peer field and SEMPv1() returns a non-nil client. Parallels the
// existing V2 test to confirm both protocols are initialized at construction.
func TestBrokerClient_V1_ReturnsClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<rpc-reply><rpc><show/></rpc><execute-result code="ok"/></rpc-reply>`))
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
	sempCfg := &config.SEMPConfig{RequestTimeoutDuration: 5 * time.Second}

	bc, err := semp.NewBrokerClient("test-broker", brokerCfg, sempCfg)
	if err != nil {
		t.Fatalf("NewBrokerClient() error: %v", err)
	}
	client := bc.SEMPv1()

	if client == nil {
		t.Fatal("SEMPv1() returned nil")
	}
}

// TestBrokerClient_V1_ExecutePassesThrough verifies that calling Execute on
// the SEMPv1() client reaches the broker's HTTP endpoint. Parallels the V2
// pass-through test to prove the v1 accessor is wired to a real HTTPClient,
// not a nil-or-stub value.
func TestBrokerClient_V1_ExecutePassesThrough(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<rpc-reply><rpc><show/></rpc><execute-result code="ok"/></rpc-reply>`))
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
	sempCfg := &config.SEMPConfig{RequestTimeoutDuration: 5 * time.Second}

	bc, err := semp.NewBrokerClient("test-broker", brokerCfg, sempCfg)
	if err != nil {
		t.Fatalf("NewBrokerClient() error: %v", err)
	}
	client := bc.SEMPv1()

	_, execErr := client.Execute(context.Background(), `<rpc><show><version/></show></rpc>`)
	if execErr != nil {
		t.Fatalf("Execute() error: %v", execErr)
	}

	if !called {
		t.Error("HTTP server was not called — Execute did not pass through to HTTPClient")
	}
}
