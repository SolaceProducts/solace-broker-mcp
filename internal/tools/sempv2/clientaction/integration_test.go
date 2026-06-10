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

//go:build integration

package clientaction

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/defaults"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools"
)

// TestIntegration_ClientClearStats runs the non-destructive `clearStats`
// action against a real broker. `disconnect` is never run live to avoid
// service interruption.
//
// Run with:
//
//	go test -tags=integration -count=1 -run TestIntegration_ClientClearStats \
//	  ./internal/tools/sempv2/clientaction/
//
// Required env vars (all must be set or the test skips):
//
//	MCP_INT_BROKER_URL    e.g., http://lab-129-78:80
//	MCP_INT_BROKER_USER   SEMP basic-auth username
//	MCP_INT_BROKER_PASS   SEMP basic-auth password
//	MCP_INT_VPN           Message VPN name (e.g., default)
//	MCP_INT_CLIENT        Connected client name on the VPN (e.g., #client)
func TestIntegration_ClientClearStats(t *testing.T) {
	url := os.Getenv("MCP_INT_BROKER_URL")
	user := os.Getenv("MCP_INT_BROKER_USER")
	pass := os.Getenv("MCP_INT_BROKER_PASS")
	vpn := os.Getenv("MCP_INT_VPN")
	clientName := os.Getenv("MCP_INT_CLIENT")
	if url == "" || user == "" || pass == "" || vpn == "" || clientName == "" {
		t.Skip("Set MCP_INT_BROKER_URL/USER/PASS/VPN/CLIENT to run this integration test.")
	}

	retries := 0
	noThrottle := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		MaxConcurrentPerBroker: defaults.DefaultMaxConcurrentPerBroker,
		RequestTimeoutDuration: defaults.DefaultSEMPRequestTimeoutDuration,
		RequestMinInterval:     &noThrottle,
		Retries:                &retries,
		RetryMinInterval:       defaults.DefaultRetryMinInterval,
		RetryMaxInterval:       defaults.DefaultRetryMaxInterval,
	}
	brokerCfg := &config.BrokerConfig{
		URL: url,
		Auth: config.AuthConfig{
			Mode:     config.AuthModeBasic,
			Username: user,
			Password: pass,
		},
	}

	client, err := sempv2.NewHTTPClient(brokerCfg, sempCfg)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	defer client.Close()

	tc := &tools.ToolContext{SEMPv2Client: client}
	h := NewHandler()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := h.Handle(ctx, tc, map[string]any{
		"msgVpnName": vpn,
		"clientName": clientName,
		"action":     actionClearStats,
	})
	if err != nil {
		t.Fatalf("Handle clearStats: %v", err)
	}

	for k, want := range map[string]any{
		"status":     "ok",
		"action":     actionClearStats,
		"msgVpnName": vpn,
		"clientName": clientName,
	} {
		if got := result.StructuredContent[k]; got != want {
			t.Errorf("StructuredContent[%q] = %v, want %v", k, got, want)
		}
	}
}
