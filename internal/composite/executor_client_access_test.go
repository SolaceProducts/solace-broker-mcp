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

	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

// listClientUsernamesTool mirrors the list-client-usernames YAML definition.
func listClientUsernamesTool() CompositeTool {
	return CompositeTool{
		Name: "list-client-usernames",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "maxResults", Type: "integer", Required: false},
		},
		Steps: []Step{{
			ID:          "clientUsernames",
			Operation:   "monitor/getMsgVpnClientUsernames",
			FollowPages: true,
			Args: map[string]string{
				"msgVpnName": "{{.Params.msgVpnName}}",
				"count":      "100",
			},
			Select: []string{"aclProfileName", "clientProfileName", "clientUsername", "enabled", "guaranteedEndpointPermissionOverrideEnabled", "msgVpnName", "subscriptionManagerEnabled"},
		}},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// getClientUsernameTool mirrors the get-client-username YAML definition.
func getClientUsernameTool() CompositeTool {
	return CompositeTool{
		Name: "get-client-username",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "clientUsername", Type: "string", Required: true},
		},
		Steps: []Step{{
			ID:        "clientUsername",
			Operation: "monitor/getMsgVpnClientUsername",
			Args: map[string]string{
				"msgVpnName":     "{{.Params.msgVpnName}}",
				"clientUsername": "{{.Params.clientUsername}}",
				"select":         "aclProfileName, clientProfileName, clientUsername, dynamic, enabled, guaranteedEndpointPermissionOverrideEnabled, msgVpnName, subscriptionManagerEnabled",
			},
		}},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// listClientProfilesTool mirrors the list-client-profiles YAML definition.
func listClientProfilesTool() CompositeTool {
	return CompositeTool{
		Name: "list-client-profiles",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "maxResults", Type: "integer", Required: false},
		},
		Steps: []Step{{
			ID:          "clientProfiles",
			Operation:   "monitor/getMsgVpnClientProfiles",
			FollowPages: true,
			Args: map[string]string{
				"msgVpnName": "{{.Params.msgVpnName}}",
				"count":      "100",
			},
			Select: []string{"allowGuaranteedEndpointCreateEnabled", "allowGuaranteedMsgReceiveEnabled", "allowGuaranteedMsgSendEnabled", "clientProfileName", "maxConnectionCountPerClientUsername", "maxEndpointCountPerClientUsername", "maxSubscriptionCount", "msgVpnName"},
		}},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// getClientProfileTool mirrors the get-client-profile YAML definition.
func getClientProfileTool() CompositeTool {
	return CompositeTool{
		Name: "get-client-profile",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "clientProfileName", Type: "string", Required: true},
		},
		Steps: []Step{{
			ID:        "clientProfile",
			Operation: "monitor/getMsgVpnClientProfile",
			Args: map[string]string{
				"msgVpnName":        "{{.Params.msgVpnName}}",
				"clientProfileName": "{{.Params.clientProfileName}}",
				"select":            "allowBridgeConnectionsEnabled, allowGuaranteedEndpointCreateDurability, allowGuaranteedEndpointCreateEnabled, allowGuaranteedMsgReceiveEnabled, allowGuaranteedMsgSendEnabled, allowSharedSubscriptionsEnabled, allowTransactedSessionsEnabled, clientProfileName, compressionEnabled, elidingEnabled, maxConnectionCountPerClientUsername, maxEffectiveEndpointCount, maxEffectiveRxFlowCount, maxEffectiveSubscriptionCount, maxEffectiveTransactedSessionCount, maxEffectiveTransactionCount, maxEffectiveTxFlowCount, maxEgressFlowCount, maxEndpointCountPerClientUsername, maxIngressFlowCount, maxSubscriptionCount, maxTransactedSessionCount, maxTransactionCount, msgVpnName",
			},
		}},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

func makeClientUsernameItems(n int) []any {
	items := make([]any, n)
	for i := range items {
		items[i] = map[string]any{
			"clientUsername":    fmt.Sprintf("app-user-%d", i),
			"enabled":           true,
			"clientProfileName": "default",
			"aclProfileName":    "default",
		}
	}
	return items
}

func makeClientProfileItems(n int) []any {
	items := make([]any, n)
	for i := range items {
		items[i] = map[string]any{
			"clientProfileName":             fmt.Sprintf("profile-%d", i),
			"allowGuaranteedMsgSendEnabled": false,
		}
	}
	return items
}

func TestExecute_ListClientUsernames_ReturnsData(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClientUsernames", pageResult(makeClientUsernameItems(3), ""))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listClientUsernamesTool(), client, map[string]any{
		"msgVpnName": "default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	usernames := result["clientUsernames"].(map[string]any)
	items := usernames["data"].([]any)
	if len(items) != 3 {
		t.Errorf("len(items) = %d, want 3", len(items))
	}
	if usernames["truncated"] != false {
		t.Errorf("truncated = %v, want false", usernames["truncated"])
	}
}

func TestExecute_ListClientUsernames_TruncatesAtMaxResults(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClientUsernames", pageResult(makeClientUsernameItems(100), "cursor-next"))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listClientUsernamesTool(), client, map[string]any{
		"msgVpnName": "default",
		"maxResults": float64(40),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	usernames := result["clientUsernames"].(map[string]any)
	items := usernames["data"].([]any)
	if len(items) != 40 {
		t.Errorf("len(items) = %d, want 40", len(items))
	}
	if usernames["truncated"] != true {
		t.Errorf("truncated = %v, want true", usernames["truncated"])
	}
	wantMsg := "Results limited to 40. Use maxResults (up to 500) to retrieve more."
	if usernames["truncatedMessage"] != wantMsg {
		t.Errorf("truncatedMessage = %v, want %q", usernames["truncatedMessage"], wantMsg)
	}
}

// TestExecute_ListClientUsernames_FixedCountOnWire pins count="100" on the wire
// regardless of maxResults, and that the select list reaches the broker as the
// comma-joined query arg (the tool's whole value is the trimmed projection).
func TestExecute_ListClientUsernames_FixedCountOnWire(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClientUsernames", pageResult(makeClientUsernameItems(10), ""))

	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: client, recorded: &recorded, mu: &mu}

	executor := NewCompositeExecutor(testOperations())
	_, err := executor.Execute(context.Background(), listClientUsernamesTool(), capture, map[string]any{
		"msgVpnName": "default",
		"maxResults": float64(500),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if recorded[0].args["count"] != "100" {
		t.Errorf("count on wire = %v, want 100", recorded[0].args["count"])
	}
	sel, _ := recorded[0].args["select"].(string)
	// Assert a field unique to the full projection (not one shared with a
	// trimmed fixture), so dropping it from the YAML actually fails this test.
	if !contains(sel, "guaranteedEndpointPermissionOverrideEnabled") {
		t.Errorf("select on wire = %q, want it to contain guaranteedEndpointPermissionOverrideEnabled", sel)
	}
}

func TestExecute_GetClientUsername_ReturnsData(t *testing.T) {
	client := newMockClient()
	client.responses["getMsgVpnClientUsername"] = &sempv2.Result{
		Data: map[string]any{
			"clientUsername":    "app-user",
			"enabled":           true,
			"clientProfileName": "default",
			"aclProfileName":    "default",
		},
		StatusCode: 200,
	}

	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: client, recorded: &recorded, mu: &mu}

	executor := NewCompositeExecutor(testOperations())
	result, err := executor.Execute(context.Background(), getClientUsernameTool(), capture, map[string]any{
		"msgVpnName":     "default",
		"clientUsername": "app-user",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data := result["clientUsername"].(map[string]any)
	if data["clientUsername"] != "app-user" {
		t.Errorf("clientUsername = %v, want app-user", data["clientUsername"])
	}
	// The username is a path segment, so it must reach the wire as an arg.
	if recorded[0].args["clientUsername"] != "app-user" {
		t.Errorf("clientUsername arg = %v, want app-user", recorded[0].args["clientUsername"])
	}
}

func TestExecute_ListClientProfiles_ReturnsData(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClientProfiles", pageResult(makeClientProfileItems(2), ""))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listClientProfilesTool(), client, map[string]any{
		"msgVpnName": "default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	profiles := result["clientProfiles"].(map[string]any)
	items := profiles["data"].([]any)
	if len(items) != 2 {
		t.Errorf("len(items) = %d, want 2", len(items))
	}
	if profiles["truncated"] != false {
		t.Errorf("truncated = %v, want false", profiles["truncated"])
	}
}

func TestExecute_ListClientProfiles_TruncatesAtMaxResults(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClientProfiles", pageResult(makeClientProfileItems(100), "cursor-next"))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listClientProfilesTool(), client, map[string]any{
		"msgVpnName": "default",
		"maxResults": float64(25),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	profiles := result["clientProfiles"].(map[string]any)
	items := profiles["data"].([]any)
	if len(items) != 25 {
		t.Errorf("len(items) = %d, want 25", len(items))
	}
	if profiles["truncated"] != true {
		t.Errorf("truncated = %v, want true", profiles["truncated"])
	}
	wantMsg := "Results limited to 25. Use maxResults (up to 500) to retrieve more."
	if profiles["truncatedMessage"] != wantMsg {
		t.Errorf("truncatedMessage = %v, want %q", profiles["truncatedMessage"], wantMsg)
	}
}

// TestExecute_ListClientProfiles_FixedCountOnWire pins count="100" on the wire
// regardless of maxResults, and that a full-projection field reaches the broker.
func TestExecute_ListClientProfiles_FixedCountOnWire(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClientProfiles", pageResult(makeClientProfileItems(10), ""))

	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: client, recorded: &recorded, mu: &mu}

	executor := NewCompositeExecutor(testOperations())
	_, err := executor.Execute(context.Background(), listClientProfilesTool(), capture, map[string]any{
		"msgVpnName": "default",
		"maxResults": float64(500),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if recorded[0].args["count"] != "100" {
		t.Errorf("count on wire = %v, want 100", recorded[0].args["count"])
	}
	sel, _ := recorded[0].args["select"].(string)
	if !contains(sel, "allowGuaranteedMsgReceiveEnabled") {
		t.Errorf("select on wire = %q, want it to contain allowGuaranteedMsgReceiveEnabled", sel)
	}
}

func TestExecute_GetClientProfile_ReturnsData(t *testing.T) {
	client := newMockClient()
	client.responses["getMsgVpnClientProfile"] = &sempv2.Result{
		Data: map[string]any{
			"clientProfileName":             "default",
			"allowGuaranteedMsgSendEnabled": true,
		},
		StatusCode: 200,
	}

	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: client, recorded: &recorded, mu: &mu}

	executor := NewCompositeExecutor(testOperations())
	result, err := executor.Execute(context.Background(), getClientProfileTool(), capture, map[string]any{
		"msgVpnName":        "default",
		"clientProfileName": "default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data := result["clientProfile"].(map[string]any)
	if data["clientProfileName"] != "default" {
		t.Errorf("clientProfileName = %v, want default", data["clientProfileName"])
	}
	if recorded[0].args["clientProfileName"] != "default" {
		t.Errorf("clientProfileName arg = %v, want default", recorded[0].args["clientProfileName"])
	}
}
