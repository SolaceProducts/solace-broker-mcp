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

package tools

import (
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newRegTestPool() *semp.BrokerPool {
	cfg := &config.ServerConfig{
		Brokers: map[string]*config.BrokerConfig{
			"dev": {
				URL:  "http://localhost:8081",
				Auth: config.AuthConfig{Mode: "basic", Username: "admin", Password: "admin"},
			},
			"prod": {
				URL:  "http://localhost:8082",
				Auth: config.AuthConfig{Mode: "basic", Username: "admin", Password: "admin"},
			},
		},
		SEMP: testSEMPCfg(),
	}
	return semp.NewBrokerPool(cfg)
}

func TestInjectBrokerParam(t *testing.T) {
	pool := newRegTestPool()
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"msgVpnName": map[string]any{"type": "string"},
		},
		"required": []string{"msgVpnName"},
	}

	result := injectBrokerParam(schema, pool)

	props, ok := result["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties map")
	}
	if _, ok := props["broker"]; !ok {
		t.Fatal("broker property not injected")
	}
	if _, ok := props["msgVpnName"]; !ok {
		t.Fatal("original property lost after injection")
	}

	required, ok := result["required"].([]string)
	if !ok {
		t.Fatal("expected required list")
	}
	if required[0] != "broker" {
		t.Errorf("expected broker first in required, got %v", required)
	}
	if len(required) != 2 {
		t.Errorf("expected 2 required fields, got %d", len(required))
	}
}

func TestInjectBrokerParam_BrokerDescriptionListsAliases(t *testing.T) {
	pool := newRegTestPool()
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}

	result := injectBrokerParam(schema, pool)
	props := result["properties"].(map[string]any)
	brokerProp := props["broker"].(map[string]any)
	desc := brokerProp["description"].(string)

	for _, alias := range []string{"dev", "prod"} {
		if !containsStr(desc, alias) {
			t.Errorf("broker description %q missing alias %q", desc, alias)
		}
	}
}

func TestInjectBrokerParam_NoRequired(t *testing.T) {
	pool := newRegTestPool()
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}

	result := injectBrokerParam(schema, pool)
	required := result["required"].([]string)
	if len(required) != 1 || required[0] != "broker" {
		t.Errorf("expected [broker], got %v", required)
	}
}

func TestRegisterWithServer(t *testing.T) {
	pool := newRegTestPool()
	mgr := NewToolManager(pool)
	mgr.Register(newStubHandler("test-tool"))

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	RegisterWithServer(mgr, server, pool)

	// No panic = tools registered successfully. The MCP SDK doesn't expose
	// a way to list registered tools directly, so we verify by checking
	// that AddTool was called without error.
}

func TestRegisterListBrokers(t *testing.T) {
	pool := newRegTestPool()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)

	RegisterListBrokers(server, pool)
	// No panic = registered successfully.
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
