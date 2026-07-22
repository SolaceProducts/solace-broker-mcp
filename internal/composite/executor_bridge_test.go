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

package composite

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// listBridgesTool returns the list-bridges tool definition for tests.
func listBridgesTool() CompositeTool {
	return CompositeTool{
		Name:        "list-bridges",
		Description: "List Bridges in a Message VPN",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "maxResults", Type: "integer", Required: false},
		},
		Steps: []Step{
			{
				ID:          "bridges",
				Operation:   "monitor/getMsgVpnBridges",
				FollowPages: true,
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
					"count":      "100",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// getBridgeStatusTool returns the get-bridge-status tool definition for
// tests. Bridges are identified by two path params (bridgeName,
// bridgeVirtualRouter) rather than one — the SEMP path segment is a literal
// "{bridgeName},{bridgeVirtualRouter}" — so this pins that both template
// substitutions land in the step args independently.
func getBridgeStatusTool() CompositeTool {
	return CompositeTool{
		Name:        "get-bridge-status",
		Description: "Get the operational status of a Bridge",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "bridgeName", Type: "string", Required: true},
			{Name: "bridgeVirtualRouter", Type: "string", Required: true},
		},
		Steps: []Step{
			{
				ID:        "bridgeStatus",
				Operation: "monitor/getMsgVpnBridge",
				Args: map[string]string{
					"msgVpnName":          "{{.Params.msgVpnName}}",
					"bridgeName":          "{{.Params.bridgeName}}",
					"bridgeVirtualRouter": "{{.Params.bridgeVirtualRouter}}",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// makeBridgeItems builds a slice of n mock bridge objects for use in
// paginated responses.
func makeBridgeItems(n int) []any {
	items := make([]any, n)
	for i := range items {
		items[i] = map[string]any{"bridgeName": fmt.Sprintf("bridge-%d", i), "enabled": true}
	}
	return items
}

func TestExecute_ListBridges_SinglePage(t *testing.T) {
	bridgeItems := []any{
		map[string]any{"bridgeName": "bridge-1", "bridgeVirtualRouter": "auto", "enabled": true, "inboundState": "ready-in-sync", "outboundState": "ready"},
		map[string]any{"bridgeName": "bridge-2", "bridgeVirtualRouter": "auto", "enabled": true, "inboundState": "stalled", "outboundState": "ready"},
	}
	client := newSeqMockClient()
	client.addResponses("getMsgVpnBridges", pageResult(bridgeItems, ""))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listBridgesTool(), client, map[string]any{
		"msgVpnName": "default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	bridges, ok := result["bridges"].(map[string]any)
	if !ok {
		t.Fatal("expected bridges key containing a map")
	}
	items, ok := bridges["data"].([]any)
	if !ok {
		t.Fatal("expected bridges.data to be a slice")
	}
	if len(items) != 2 {
		t.Errorf("len(items) = %d, want 2", len(items))
	}
	if bridges["truncated"] != false {
		t.Errorf("truncated = %v, want false", bridges["truncated"])
	}
	if len(client.calls) != 1 {
		t.Errorf("expected 1 SEMP call, got %d", len(client.calls))
	}
}

func TestExecute_ListBridges_TruncatesAtMaxResults(t *testing.T) {
	// Page has 100 bridges but maxResults=50, paginator should stop and set truncated.
	client := newSeqMockClient()
	client.addResponses("getMsgVpnBridges", pageResult(makeBridgeItems(100), "cursor-next"))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listBridgesTool(), client, map[string]any{
		"msgVpnName": "default",
		"maxResults": float64(50),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	bridges := result["bridges"].(map[string]any)
	items := bridges["data"].([]any)

	if len(items) != 50 {
		t.Errorf("len(items) = %d, want 50", len(items))
	}
	if bridges["truncated"] != true {
		t.Errorf("truncated = %v, want true", bridges["truncated"])
	}
	if len(client.calls) != 1 {
		t.Errorf("expected 1 SEMP call, got %d", len(client.calls))
	}
}

// TestExecute_GetBridgeStatus_CompoundPathParams pins that both bridgeName
// and bridgeVirtualRouter — the two-part identifier SEMP uses for bridges,
// unlike every other object this server exposes — are independently
// substituted into the step args (and therefore the URL template, which
// contains both as literal "{bridgeName},{bridgeVirtualRouter}").
func TestExecute_GetBridgeStatus_CompoundPathParams(t *testing.T) {
	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: newMockClient(), recorded: &recorded, mu: &mu}

	executor := NewCompositeExecutor(testOperations())

	res, err := executor.Execute(context.Background(), getBridgeStatusTool(), capture, map[string]any{
		"msgVpnName":          "vpn-a",
		"bridgeName":          "bridge-to-dr",
		"bridgeVirtualRouter": "auto",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if recorded[0].opID != "getMsgVpnBridge" {
		t.Errorf("opID = %q, want getMsgVpnBridge", recorded[0].opID)
	}
	if recorded[0].args["msgVpnName"] != "vpn-a" {
		t.Errorf("args[msgVpnName] = %v, want vpn-a", recorded[0].args["msgVpnName"])
	}
	if recorded[0].args["bridgeName"] != "bridge-to-dr" {
		t.Errorf("args[bridgeName] = %v, want bridge-to-dr", recorded[0].args["bridgeName"])
	}
	if recorded[0].args["bridgeVirtualRouter"] != "auto" {
		t.Errorf("args[bridgeVirtualRouter] = %v, want auto", recorded[0].args["bridgeVirtualRouter"])
	}
	if res["bridgeStatus"] == nil {
		t.Error("expected bridgeStatus result to be collected")
	}
}
