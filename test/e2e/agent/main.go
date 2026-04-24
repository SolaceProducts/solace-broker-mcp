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

// Package main implements a standalone Go MCP SDK client agent for E2E testing.
// It connects to the MCP server via StreamableClientTransport and exercises
// the registered tools with hardcoded calls against both configured brokers.
//
// Usage: agent <server-url>
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <server-url>\n", os.Args[0])
		os.Exit(1)
	}
	serverURL := os.Args[1]

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := run(ctx, serverURL); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("PASS: all agent checks passed")
}

func run(ctx context.Context, serverURL string) error {
	// Connect to the MCP server
	fmt.Printf("Connecting to %s ...\n", serverURL)
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "e2e-agent",
		Version: "1.0.0",
	}, nil)

	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: serverURL + "/mcp",
	}, nil)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer session.Close()
	fmt.Println("Connected.")

	// 1. ListTools — verify expected tools are present
	fmt.Println("\n--- ListTools ---")
	toolsResult, err := session.ListTools(ctx, nil)
	if err != nil {
		return fmt.Errorf("ListTools: %w", err)
	}

	toolNames := make(map[string]bool)
	for _, t := range toolsResult.Tools {
		toolNames[t.Name] = true
		fmt.Printf("  tool: %s\n", t.Name)
	}

	expected := []string{"list-brokers", "get-rdp-status", "get-queue-metrics", "get-client-details", "list-client-subscriptions"}
	for _, name := range expected {
		if !toolNames[name] {
			return fmt.Errorf("expected tool %q not found in ListTools result", name)
		}
	}
	fmt.Printf("All %d expected tools found.\n", len(expected))

	// 2. Call list-brokers — verify both aliases
	fmt.Println("\n--- list-brokers ---")
	brokersResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list-brokers",
		Arguments: map[string]any{},
	})
	if err != nil {
		return fmt.Errorf("CallTool(list-brokers): %w", err)
	}
	if brokersResult.IsError {
		return fmt.Errorf("list-brokers returned error: %v", contentText(brokersResult))
	}
	text := contentText(brokersResult)
	fmt.Printf("  response: %s\n", text)
	for _, alias := range []string{"broker-a", "broker-b"} {
		if !strings.Contains(text, alias) {
			return fmt.Errorf("list-brokers response missing '%s' alias: %s", alias, text)
		}
	}
	fmt.Println("Both broker aliases found (broker-a, broker-b).")

	// 3. Test tools against both brokers
	for _, broker := range []string{"broker-a", "broker-b"} {
		if err := testBroker(ctx, session, broker); err != nil {
			return err
		}
	}

	return nil
}

func testBroker(ctx context.Context, session *mcp.ClientSession, broker string) error {
	fmt.Printf("\n=== Testing %s ===\n", broker)

	// get-rdp-status
	fmt.Printf("\n--- get-rdp-status (%s) ---\n", broker)
	rdpResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "get-rdp-status",
		Arguments: map[string]any{
			"broker":                broker,
			"msgVpnName":            "default",
			"restDeliveryPointName": "test-rdp",
		},
	})
	if err != nil {
		return fmt.Errorf("CallTool(get-rdp-status, %s): %w", broker, err)
	}
	if rdpResult.IsError {
		return fmt.Errorf("get-rdp-status (%s) returned error: %v", broker, contentText(rdpResult))
	}
	rdpText := contentText(rdpResult)
	fmt.Printf("  response length: %d bytes\n", len(rdpText))
	for _, key := range []string{"rdpStatus", "queueBindings", "restConsumers"} {
		if !strings.Contains(rdpText, key) {
			return fmt.Errorf("get-rdp-status (%s) response missing %q", broker, key)
		}
	}
	fmt.Printf("All 3 response sections present (%s).\n", broker)

	// get-queue-metrics
	fmt.Printf("\n--- get-queue-metrics (%s) ---\n", broker)
	queueResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "get-queue-metrics",
		Arguments: map[string]any{
			"broker":     broker,
			"msgVpnName": "default",
			"queueName":  "test-queue",
		},
	})
	if err != nil {
		return fmt.Errorf("CallTool(get-queue-metrics, %s): %w", broker, err)
	}
	if queueResult.IsError {
		return fmt.Errorf("get-queue-metrics (%s) returned error: %v", broker, contentText(queueResult))
	}
	queueText := contentText(queueResult)
	if !strings.Contains(queueText, "queueMetrics") {
		return fmt.Errorf("get-queue-metrics (%s) response missing 'queueMetrics'", broker)
	}
	if !strings.Contains(queueText, "test-queue") {
		return fmt.Errorf("get-queue-metrics (%s) response missing 'test-queue'", broker)
	}
	fmt.Printf("Queue metrics response valid (%s).\n", broker)

	return nil
}

func contentText(result *mcp.CallToolResult) string {
	var parts []string
	for _, c := range result.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			parts = append(parts, v.Text)
		default:
			b, _ := json.Marshal(v)
			parts = append(parts, string(b))
		}
	}
	return strings.Join(parts, "\n")
}
