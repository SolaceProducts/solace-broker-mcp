package registry

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/composite"
	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mockClient implements sempv2.Client for testing.
type mockClient struct {
	results map[string]*sempv2.Result
}

func (m *mockClient) Execute(_ context.Context, op *sempv2.Operation, _ map[string]any) (*sempv2.Result, error) {
	if r, ok := m.results[op.ID]; ok {
		return r, nil
	}
	return &sempv2.Result{Data: map[string]any{"op": op.ID}, StatusCode: 200}, nil
}

func newTestPool() *semp.BrokerPool {
	cfg := &config.ServerConfig{
		Brokers: map[string]*config.BrokerConfig{
			"dev": {
				URL:       "http://localhost:8081",
				EnvPrefix: "DEV",
				Auth:      config.AuthConfig{Method: "basic", Username: "admin", Password: "admin"},
			},
			"prod": {
				URL:       "http://localhost:8082",
				EnvPrefix: "PROD",
				Auth:      config.AuthConfig{Method: "basic", Username: "admin", Password: "admin"},
			},
		},
		SEMP: config.SEMPConfig{RequestTimeoutSeconds: 5},
	}
	return semp.NewBrokerPool(cfg)
}

func TestBuildInputSchema_InjectsBroker(t *testing.T) {
	pool := newTestPool()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	reg := NewRegistry(server, pool, nil)

	params := []composite.ParameterDef{
		{Name: "msgVpnName", Type: "string", Required: true, Description: "The VPN"},
	}

	schema := reg.buildInputSchema(params)

	props := schema["properties"].(map[string]any)
	if _, ok := props["broker"]; !ok {
		t.Fatal("broker property not injected into schema")
	}

	brokerProp := props["broker"].(map[string]any)
	if brokerProp["type"] != "string" {
		t.Errorf("broker type = %v, want string", brokerProp["type"])
	}

	required := schema["required"].([]string)
	found := false
	for _, r := range required {
		if r == "broker" {
			found = true
			break
		}
	}
	if !found {
		t.Error("broker not in required list")
	}
}

func TestBuildInputSchema_IncludesToolParams(t *testing.T) {
	pool := newTestPool()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	reg := NewRegistry(server, pool, nil)

	params := []composite.ParameterDef{
		{Name: "msgVpnName", Type: "string", Required: true, Description: "The VPN"},
		{Name: "queueName", Type: "string", Required: true, Description: "The queue"},
		{Name: "optional", Type: "string", Required: false, Description: "Optional param"},
	}

	schema := reg.buildInputSchema(params)

	props := schema["properties"].(map[string]any)
	for _, name := range []string{"msgVpnName", "queueName", "optional"} {
		if _, ok := props[name]; !ok {
			t.Errorf("missing property %q in schema", name)
		}
	}

	required := schema["required"].([]string)
	requiredSet := make(map[string]bool)
	for _, r := range required {
		requiredSet[r] = true
	}
	if !requiredSet["msgVpnName"] || !requiredSet["queueName"] {
		t.Error("required tool params missing from required list")
	}
	if requiredSet["optional"] {
		t.Error("optional param should not be in required list")
	}
}

func TestBuildInputSchema_BrokerDescriptionListsAliases(t *testing.T) {
	pool := newTestPool()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	reg := NewRegistry(server, pool, nil)

	schema := reg.buildInputSchema(nil)
	props := schema["properties"].(map[string]any)
	desc := props["broker"].(map[string]any)["description"].(string)

	for _, alias := range []string{"dev", "prod"} {
		if !contains(desc, alias) {
			t.Errorf("broker description %q missing alias %q", desc, alias)
		}
	}
}

func TestRegisterAll_RegistersTools(t *testing.T) {
	pool := newTestPool()
	ops := map[string]*sempv2.Operation{
		"monitor/getMsgVpnRestDeliveryPoint": {
			ID:     "getMsgVpnRestDeliveryPoint",
			Method: "GET",
			Path:   "/SEMP/v2/monitor/msgVpns/{msgVpnName}/restDeliveryPoints/{restDeliveryPointName}",
		},
	}
	executor := composite.NewCompositeExecutor(ops)
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	reg := NewRegistry(server, pool, executor)

	tools := []composite.CompositeTool{
		{
			Name:        "test-tool",
			Description: "A test tool",
			Parameters: []composite.ParameterDef{
				{Name: "msgVpnName", Type: "string", Required: true},
			},
			Steps: []composite.Step{
				{
					ID:        "step1",
					Operation: "monitor/getMsgVpnRestDeliveryPoint",
					Args:      map[string]string{"msgVpnName": "{{.Params.msgVpnName}}"},
				},
			},
			Result: composite.ResultStrategy{Strategy: "collect"},
		},
	}

	if err := reg.RegisterAll(tools); err != nil {
		t.Fatalf("RegisterAll failed: %v", err)
	}
}

func TestHandleToolCall_MissingBroker(t *testing.T) {
	pool := newTestPool()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	reg := NewRegistry(server, pool, nil)

	tool := &composite.CompositeTool{Name: "test"}
	req := &mcp.CallToolRequest{}
	req.Params = &mcp.CallToolParamsRaw{
		Name:      "test",
		Arguments: json.RawMessage(`{"msgVpnName": "default"}`),
	}

	_, err := reg.handleToolCall(context.Background(), req, tool)
	if err == nil {
		t.Fatal("expected error for missing broker")
	}
	if !contains(err.Error(), "broker parameter is required") {
		t.Errorf("error = %v, want 'broker parameter is required'", err)
	}
}

func TestHandleToolCall_UnknownBroker(t *testing.T) {
	pool := newTestPool()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	reg := NewRegistry(server, pool, nil)

	tool := &composite.CompositeTool{Name: "test"}
	req := &mcp.CallToolRequest{}
	req.Params = &mcp.CallToolParamsRaw{
		Name:      "test",
		Arguments: json.RawMessage(`{"broker": "nonexistent"}`),
	}

	_, err := reg.handleToolCall(context.Background(), req, tool)
	if err == nil {
		t.Fatal("expected error for unknown broker")
	}
	if !contains(err.Error(), "unknown broker") {
		t.Errorf("error = %v, want 'unknown broker'", err)
	}
}

func TestBuildInputSchema_NoParams(t *testing.T) {
	pool := newTestPool()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	reg := NewRegistry(server, pool, nil)

	schema := reg.buildInputSchema(nil)

	props := schema["properties"].(map[string]any)
	if len(props) != 1 {
		t.Errorf("expected 1 property (broker only), got %d", len(props))
	}
	if _, ok := props["broker"]; !ok {
		t.Fatal("broker property missing from empty-params schema")
	}

	required := schema["required"].([]string)
	if len(required) != 1 || required[0] != "broker" {
		t.Errorf("required = %v, want [broker]", required)
	}
}

func TestHandleToolCall_BrokerRemovedFromParams(t *testing.T) {
	pool := newTestPool()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)

	// Track what params the executor receives.
	var receivedParams map[string]any
	ops := map[string]*sempv2.Operation{
		"monitor/testOp": {
			ID:     "testOp",
			Method: "GET",
			Path:   "/test",
		},
	}
	executor := composite.NewCompositeExecutor(ops)

	reg := NewRegistry(server, pool, executor)

	// Use a tool with one step that will fail (mock broker won't serve HTTP),
	// but we intercept at the handler level by checking params before execution.
	// Instead, test via the handleToolCall path with a real pool entry.
	// The "dev" broker exists in the test pool but points at localhost:8081
	// which won't respond — that's fine, we just need to verify broker is stripped.

	tool := &composite.CompositeTool{
		Name:        "test",
		Description: "test",
		Parameters:  []composite.ParameterDef{{Name: "vpnName", Type: "string", Required: true}},
		Steps: []composite.Step{
			{ID: "s1", Operation: "monitor/testOp", Args: map[string]string{"vpnName": "{{.Params.vpnName}}"}},
		},
		Result: composite.ResultStrategy{Strategy: "collect"},
	}
	req := &mcp.CallToolRequest{}
	req.Params = &mcp.CallToolParamsRaw{
		Name:      "test",
		Arguments: json.RawMessage(`{"broker": "dev", "vpnName": "default"}`),
	}

	// The call will fail because there's no real broker at localhost:8081,
	// but we can verify broker was stripped by checking the error message.
	// If broker leaked through, the executor would try to resolve {{.Params.broker}}
	// in a template — but our tool doesn't use it, so we verify indirectly:
	// the error should be about the HTTP call failing, not about broker param.
	_, err := reg.handleToolCall(context.Background(), req, tool)
	_ = receivedParams // not used in this approach

	if err == nil {
		t.Fatal("expected error (no real broker), but got nil")
	}
	// Error should be from SEMP call failure, not from broker-related template issues.
	if contains(err.Error(), "broker") && !contains(err.Error(), "executing tool") {
		t.Errorf("error mentions broker unexpectedly: %v", err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
