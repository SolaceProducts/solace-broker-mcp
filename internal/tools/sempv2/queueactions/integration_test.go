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

package queueactions

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

// TestIntegration_ClearQueueStats runs the non-destructive clear-queue-stats
// tool against a real broker. Only the non-destructive tool is tested live;
// delete-queue-messages is never executed by automated tests to avoid data loss.
//
// Run with:
//
//	go test -tags=integration -count=1 -run TestIntegration_ClearQueueStats \
//	  ./internal/tools/sempv2/queueactions/
//
// Required env vars (all must be set or the test skips):
//
//	MCP_INT_BROKER_URL    e.g., http://lab-129-78:80
//	MCP_INT_BROKER_USER   SEMP basic-auth username
//	MCP_INT_BROKER_PASS   SEMP basic-auth password
//	MCP_INT_VPN           Message VPN name (e.g., default)
//	MCP_INT_QUEUE         Existing queue on the VPN (e.g., awtest)
func TestIntegration_ClearQueueStats(t *testing.T) {
	url := os.Getenv("MCP_INT_BROKER_URL")
	user := os.Getenv("MCP_INT_BROKER_USER")
	pass := os.Getenv("MCP_INT_BROKER_PASS")
	vpn := os.Getenv("MCP_INT_VPN")
	queue := os.Getenv("MCP_INT_QUEUE")
	if url == "" || user == "" || pass == "" || vpn == "" || queue == "" {
		t.Skip("Set MCP_INT_BROKER_URL/USER/PASS/VPN/QUEUE to run this integration test.")
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

	client, err := sempv2.NewHTTPClient(brokerCfg, sempCfg,
		resilience.NewSemaphore(defaults.DefaultMaxConcurrentPerBroker),
		auth.NewBasicAuthenticator(user, pass))
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	defer client.Close()

	tc := &tools.ToolContext{SEMPv2Client: client}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := NewClearStatsHandler().Handle(ctx, tc, map[string]any{
		"msgVpnName": vpn,
		"queueName":  queue,
	})
	if err != nil {
		t.Fatalf("Handle clear-queue-stats: %v", err)
	}

	for k, want := range map[string]any{
		"status":     "ok",
		"msgVpnName": vpn,
		"queueName":  queue,
	} {
		if got := result.StructuredContent[k]; got != want {
			t.Errorf("StructuredContent[%q] = %v, want %v", k, got, want)
		}
	}
}
