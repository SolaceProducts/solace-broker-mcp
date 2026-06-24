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

package clientactions

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/defaults"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/auth"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/resilience"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools"
)

// TestIntegration_ClearClientStats runs the non-destructive clear-client-stats
// tool against a real broker. Only the non-destructive tool is tested live;
// disconnect-client is never executed by automated tests to avoid service
// disruption.
//
// Run with:
//
//	go test -tags=integration -count=1 -run TestIntegration_ClearClientStats \
//	  ./internal/tools/sempv2/clientactions/
//
// Required env vars (all must be set or the test skips):
//
//	MCP_INT_BROKER_URL    e.g., http://lab-129-78:80
//	MCP_INT_BROKER_USER   SEMP basic-auth username
//	MCP_INT_BROKER_PASS   SEMP basic-auth password
//	MCP_INT_VPN           Message VPN name (e.g., default)
//	MCP_INT_CLIENT        Existing connected client name on the VPN
func TestIntegration_ClearClientStats(t *testing.T) {
	url := os.Getenv("MCP_INT_BROKER_URL")
	user := os.Getenv("MCP_INT_BROKER_USER")
	pass := os.Getenv("MCP_INT_BROKER_PASS")
	vpn := os.Getenv("MCP_INT_VPN")
	client := os.Getenv("MCP_INT_CLIENT")
	if url == "" || user == "" || pass == "" || vpn == "" || client == "" {
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
		URL:  url,
		Auth: config.AuthConfig{Mode: config.AuthModeBasic}, // credentials live on the Authenticator
	}

	sempClient, err := sempv2.NewHTTPClient(brokerCfg, sempCfg,
		resilience.NewSemaphore(defaults.DefaultMaxConcurrentPerBroker),
		auth.NewBasicAuthenticator(user, pass))
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	defer sempClient.Close()

	tc := &tools.ToolContext{SEMPv2Client: sempClient}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := NewClearStatsHandler().Handle(ctx, tc, map[string]any{
		"msgVpnName": vpn,
		"clientName": client,
	})
	if err != nil {
		t.Fatalf("Handle clear-client-stats: %v", err)
	}

	for k, want := range map[string]any{
		"status":     "ok",
		"msgVpnName": vpn,
		"clientName": client,
	} {
		if got := result.StructuredContent[k]; got != want {
			t.Errorf("StructuredContent[%q] = %v, want %v", k, got, want)
		}
	}
}
