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
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
)

// getClientDetailsTool returns the get-client-details tool definition for tests.
func getClientDetailsTool() CompositeTool {
	return CompositeTool{
		Name:        "get-client-details",
		Description: "Get performance metrics for a specific connected client",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "clientName", Type: "string", Required: true},
		},
		Steps: []Step{
			{
				ID:        "clientDetails",
				Operation: "monitor/getMsgVpnClient",
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
					"clientName": "{{.Params.clientName}}",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// listClientSubscriptionsTool returns the list-client-subscriptions tool definition for tests.
func listClientSubscriptionsTool() CompositeTool {
	return CompositeTool{
		Name:        "list-client-subscriptions",
		Description: "List topic subscriptions for a specific client",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "clientName", Type: "string", Required: true},
			{Name: "maxResults", Type: "integer", Required: false},
		},
		Steps: []Step{
			{
				ID:          "subscriptions",
				Operation:   "monitor/getMsgVpnClientSubscriptions",
				FollowPages: true,
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
					"clientName": "{{.Params.clientName}}",
					"count":      "100",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// listClientsTool returns the list-clients tool definition for tests.
func listClientsTool() CompositeTool {
	return CompositeTool{
		Name:        "list-clients",
		Description: "List all active client connections in a Message VPN",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "maxResults", Type: "integer", Required: false},
		},
		Steps: []Step{
			{
				ID:          "clients",
				Operation:   "monitor/getMsgVpnClients",
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

// listSlowSubscribersTool returns the list-slow-subscribers tool definition for tests.
func listSlowSubscribersTool() CompositeTool {
	return CompositeTool{
		Name:        "list-slow-subscribers",
		Description: "List clients flagged as slow subscribers",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "maxResults", Type: "integer", Required: false},
		},
		Steps: []Step{
			{
				ID:          "slowSubscribers",
				Operation:   "monitor/getMsgVpnClients",
				FollowPages: true,
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
					"count":      "100",
					"where":      "slowSubscriber==true",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// makeSubscriptionItems builds a slice of n mock subscription objects for use in paginated responses.
func makeSubscriptionItems(n int) []any {
	items := make([]any, n)
	for i := range items {
		items[i] = map[string]any{
			"subscriptionTopic": fmt.Sprintf("topic/%d/>", i),
			"clientName":        "myapp/1",
		}
	}
	return items
}

// makeClientItems builds a slice of n mock client objects for use in paginated responses.
func makeClientItems(n int) []any {
	items := make([]any, n)
	for i := range items {
		items[i] = map[string]any{"clientName": fmt.Sprintf("client-%d", i), "slowSubscriber": false}
	}
	return items
}

// makeSlowSubscriberItems builds a slice of n mock slow-subscriber client objects
// for use in paginated responses (slowSubscriber forced true to mirror the SEMP
// where filter that the tool applies).
func makeSlowSubscriberItems(n int) []any {
	items := make([]any, n)
	for i := range items {
		items[i] = map[string]any{
			"clientName":          fmt.Sprintf("slow-client-%d", i),
			"slowSubscriber":      true,
			"txDiscardedMsgCount": float64(0),
		}
	}
	return items
}

func TestExecute_GetClientDetails_ReturnsData(t *testing.T) {
	client := newMockClient()
	client.responses["getMsgVpnClient"] = &sempv2.Result{
		Data: map[string]any{
			"clientName":          "myapp/1",
			"clientUsername":      "app-user",
			"slowSubscriber":      false,
			"txDiscardedMsgCount": float64(0),
		},
		StatusCode: 200,
	}

	executor := NewCompositeExecutor(testOperations())

	params := map[string]any{
		"msgVpnName": "default",
		"clientName": "myapp/1",
	}

	result, err := executor.Execute(context.Background(), getClientDetailsTool(), client, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result["clientDetails"] == nil {
		t.Fatal("expected clientDetails key in result, got nil")
	}

	data, ok := result["clientDetails"].(map[string]any)
	if !ok {
		t.Fatal("expected clientDetails to be a map")
	}

	if data["clientName"] != "myapp/1" {
		t.Errorf("expected clientName %q, got %v", "myapp/1", data["clientName"])
	}
}

func TestExecute_GetClientDetails_NotFound(t *testing.T) {
	client := newMockClient()
	client.errors["getMsgVpnClient"] = &sempv2.SEMPError{
		Operation:  "getMsgVpnClient",
		StatusCode: 400,
		Body:       `{"meta":{"error":{"code":400,"description":"Client not found","status":"NOT_FOUND"}}}`,
	}

	executor := NewCompositeExecutor(testOperations())

	params := map[string]any{
		"msgVpnName": "default",
		"clientName": "nonexistent-client",
	}

	_, err := executor.Execute(context.Background(), getClientDetailsTool(), client, params)
	if err == nil {
		t.Fatal("expected error for 404 response, got nil")
	}

	var sempErr *sempv2.SEMPError
	if !errors.As(err, &sempErr) {
		t.Errorf("expected SEMPError in error chain, got: %v", err)
	} else if sempErr.StatusCode != 400 {
		t.Errorf("expected status code 400, got %d", sempErr.StatusCode)
	}
}

func TestExecute_ListClientSubscriptions_ReturnsData(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClientSubscriptions", pageResult(makeSubscriptionItems(2), ""))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listClientSubscriptionsTool(), client, map[string]any{
		"msgVpnName": "default",
		"clientName": "myapp/1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	subs := result["subscriptions"].(map[string]any)
	items := subs["data"].([]any)

	if len(items) != 2 {
		t.Errorf("len(items) = %d, want 2", len(items))
	}
	if subs["truncated"] != false {
		t.Errorf("truncated = %v, want false", subs["truncated"])
	}
}

func TestExecute_ListClientSubscriptions_DefaultMaxResults(t *testing.T) {
	// 80 subscriptions on a single page, fits within the default 100 limit.
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClientSubscriptions", pageResult(makeSubscriptionItems(80), ""))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listClientSubscriptionsTool(), client, map[string]any{
		"msgVpnName": "default",
		"clientName": "myapp/1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	subs := result["subscriptions"].(map[string]any)
	items := subs["data"].([]any)

	if len(items) != 80 {
		t.Errorf("len(items) = %d, want 80", len(items))
	}
	if subs["truncated"] != false {
		t.Errorf("truncated = %v, want false", subs["truncated"])
	}
}

func TestExecute_ListClientSubscriptions_MultiPage(t *testing.T) {
	// Page 1: 100 subscriptions + cursor; page 2: 30 subscriptions, no cursor. Total: 130.
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClientSubscriptions",
		pageResult(makeSubscriptionItems(100), "cursor-s2"),
		pageResult(makeSubscriptionItems(30), ""),
	)

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listClientSubscriptionsTool(), client, map[string]any{
		"msgVpnName": "default",
		"clientName": "myapp/1",
		"maxResults": float64(200),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	subs := result["subscriptions"].(map[string]any)
	items := subs["data"].([]any)

	if len(items) != 130 {
		t.Errorf("len(items) = %d, want 130", len(items))
	}
	if subs["truncated"] != false {
		t.Errorf("truncated = %v, want false", subs["truncated"])
	}
	if len(client.calls) != 2 {
		t.Errorf("expected 2 SEMP calls, got %d", len(client.calls))
	}
}

func TestExecute_ListClientSubscriptions_TruncatesAtMaxResults(t *testing.T) {
	// Page has 100 items but maxResults=50 — paginator should stop and set truncated.
	// Regression test: the previous tool definition silently capped at one SEMP page
	// without signalling truncation, hiding subscriptions from the agent.
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClientSubscriptions", pageResult(makeSubscriptionItems(100), "cursor-next"))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listClientSubscriptionsTool(), client, map[string]any{
		"msgVpnName": "default",
		"clientName": "myapp/1",
		"maxResults": float64(50),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	subs := result["subscriptions"].(map[string]any)
	items := subs["data"].([]any)

	if len(items) != 50 {
		t.Errorf("len(items) = %d, want 50", len(items))
	}
	if subs["truncated"] != true {
		t.Errorf("truncated = %v, want true", subs["truncated"])
	}
	wantMsg := "Results limited to 50. Use maxResults (up to 500) to retrieve more."
	if subs["truncatedMessage"] != wantMsg {
		t.Errorf("truncatedMessage = %v, want %q", subs["truncatedMessage"], wantMsg)
	}
}

func TestExecute_ListClientSubscriptions_FixedCountOnWire(t *testing.T) {
	// Regression test: count must always be "100" on the SEMP wire regardless of
	// maxResults. The earlier tool definition piped maxResults straight into count,
	// which exceeds SEMPv2's per-page cap of 100 and produced 400 errors at higher
	// values.
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClientSubscriptions", pageResult(makeSubscriptionItems(10), ""))

	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: client, recorded: &recorded, mu: &mu}

	executor := NewCompositeExecutor(testOperations())
	_, err := executor.Execute(context.Background(), listClientSubscriptionsTool(), capture, map[string]any{
		"msgVpnName": "default",
		"clientName": "myapp/1",
		"maxResults": float64(500),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(recorded) < 1 {
		t.Fatalf("expected at least 1 call, got %d", len(recorded))
	}
	if recorded[0].args["count"] != "100" {
		t.Errorf("expected count %q on the wire, got %v", "100", recorded[0].args["count"])
	}
}

func TestExecute_ListClients_DefaultMaxResults(t *testing.T) {
	// 80 clients on a single page, fits within the default 100 limit.
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClients", pageResult(makeClientItems(80), ""))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listClientsTool(), client, map[string]any{
		"msgVpnName": "default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	clients := result["clients"].(map[string]any)
	items := clients["data"].([]any)

	if len(items) != 80 {
		t.Errorf("len(items) = %d, want 80", len(items))
	}
	if clients["truncated"] != false {
		t.Errorf("truncated = %v, want false", clients["truncated"])
	}
}

func TestExecute_ListClients_MultiPage(t *testing.T) {
	// Page 1: 100 clients + cursor; page 2: 20 clients, no cursor. Total: 120.
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClients",
		pageResult(makeClientItems(100), "cursor-c2"),
		pageResult(makeClientItems(20), ""),
	)

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listClientsTool(), client, map[string]any{
		"msgVpnName": "default",
		"maxResults": float64(200), // larger than 120 total so paginator follows all pages
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	clients := result["clients"].(map[string]any)
	items := clients["data"].([]any)

	if len(items) != 120 {
		t.Errorf("len(items) = %d, want 120", len(items))
	}
	if clients["truncated"] != false {
		t.Errorf("truncated = %v, want false", clients["truncated"])
	}
	if len(client.calls) != 2 {
		t.Errorf("expected 2 SEMP calls, got %d", len(client.calls))
	}
}

func TestExecute_ListSlowSubscribers_ReturnsFiltered(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClients", pageResult(makeSlowSubscriberItems(3), ""))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listSlowSubscribersTool(), client, map[string]any{
		"msgVpnName": "default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	subs, ok := result["slowSubscribers"].(map[string]any)
	if !ok {
		t.Fatal("expected slowSubscribers key containing a map")
	}
	items, ok := subs["data"].([]any)
	if !ok {
		t.Fatal("expected slowSubscribers.data to be a slice")
	}
	if len(items) != 3 {
		t.Errorf("len(items) = %d, want 3", len(items))
	}
	if subs["truncated"] != false {
		t.Errorf("truncated = %v, want false", subs["truncated"])
	}

	// Verify the SEMP `where=slowSubscriber==true` predicate was passed through to
	// the broker — server-side filtering is the entire point of this tool.
	if len(client.calls) != 1 {
		t.Fatalf("expected 1 SEMP call, got %d", len(client.calls))
	}
	if client.calls[0].args["where"] != "slowSubscriber==true" {
		t.Errorf("where = %v, want slowSubscriber==true", client.calls[0].args["where"])
	}
}

func TestExecute_ListSlowSubscribers_Empty(t *testing.T) {
	// Empty is the steady-state return value: most VPNs don't have slow
	// subscribers most of the time. Pins the len(items) == 0 → break path
	// through the paginator.
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClients", pageResult([]any{}, ""))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listSlowSubscribersTool(), client, map[string]any{
		"msgVpnName": "default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	subs, ok := result["slowSubscribers"].(map[string]any)
	if !ok {
		t.Fatal("expected slowSubscribers key containing a map")
	}
	items, ok := subs["data"].([]any)
	if !ok {
		t.Fatal("expected slowSubscribers.data to be a slice")
	}
	if len(items) != 0 {
		t.Errorf("len(items) = %d, want 0", len(items))
	}
	if subs["truncated"] != false {
		t.Errorf("truncated = %v, want false", subs["truncated"])
	}
	if len(client.calls) != 1 {
		t.Errorf("expected 1 SEMP call, got %d", len(client.calls))
	}
}

func TestExecute_ListSlowSubscribers_TruncatesAtMaxResults(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClients", pageResult(makeSlowSubscriberItems(100), "cursor-next"))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listSlowSubscribersTool(), client, map[string]any{
		"msgVpnName": "default",
		"maxResults": float64(40),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	subs := result["slowSubscribers"].(map[string]any)
	items := subs["data"].([]any)

	if len(items) != 40 {
		t.Errorf("len(items) = %d, want 40", len(items))
	}
	if subs["truncated"] != true {
		t.Errorf("truncated = %v, want true", subs["truncated"])
	}
	if len(client.calls) != 1 {
		t.Errorf("expected 1 SEMP call, got %d", len(client.calls))
	}
}

// disconnectClientTool mirrors the disconnect-client YAML definition.
func disconnectClientTool() CompositeTool {
	return CompositeTool{
		Name: "disconnect-client",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "clientName", Type: "string", Required: true},
		},
		Steps: []Step{{
			ID:        "disconnect",
			Operation: "action/doMsgVpnClientDisconnect",
			Args: map[string]string{
				"msgVpnName": "{{.Params.msgVpnName}}",
				"clientName": "{{.Params.clientName}}",
			},
		}},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// clearClientStatsTool mirrors the clear-client-stats YAML definition.
func clearClientStatsTool() CompositeTool {
	return CompositeTool{
		Name: "clear-client-stats",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "clientName", Type: "string", Required: true},
		},
		Steps: []Step{{
			ID:        "clearStats",
			Operation: "action/doMsgVpnClientClearStats",
			Args: map[string]string{
				"msgVpnName": "{{.Params.msgVpnName}}",
				"clientName": "{{.Params.clientName}}",
			},
		}},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// TestExecute_DisconnectClient verifies disconnect-client issues the PUT with
// both path params filled in, synthesizes an empty request body, and collects
// under the "disconnect" step ID.
func TestExecute_DisconnectClient(t *testing.T) {
	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: newMockClient(), recorded: &recorded, mu: &mu}

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), disconnectClientTool(), capture, map[string]any{
		"msgVpnName": "vpn-a",
		"clientName": "consumer-7",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if recorded[0].opID != "doMsgVpnClientDisconnect" {
		t.Errorf("opID = %q, want doMsgVpnClientDisconnect", recorded[0].opID)
	}
	if recorded[0].args["clientName"] != "consumer-7" {
		t.Errorf("args[clientName] = %v, want consumer-7", recorded[0].args["clientName"])
	}
	body, ok := recorded[0].args["body"].(map[string]any)
	if !ok {
		t.Fatalf("args[body] = %v (%T), want empty map[string]any", recorded[0].args["body"], recorded[0].args["body"])
	}
	if len(body) != 0 {
		t.Errorf("body = %v, want empty map", body)
	}
	if result["disconnect"] == nil {
		t.Error("expected disconnect result to be collected")
	}
}

// TestExecute_ClearClientStats verifies clear-client-stats issues the clearStats
// action on the correct client path and collects under the step ID.
func TestExecute_ClearClientStats(t *testing.T) {
	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: newMockClient(), recorded: &recorded, mu: &mu}

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), clearClientStatsTool(), capture, map[string]any{
		"msgVpnName": "vpn-a",
		"clientName": "consumer-7",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if recorded[0].opID != "doMsgVpnClientClearStats" {
		t.Errorf("opID = %q, want doMsgVpnClientClearStats", recorded[0].opID)
	}
	if result["clearStats"] == nil {
		t.Error("expected clearStats result to be collected")
	}
}
