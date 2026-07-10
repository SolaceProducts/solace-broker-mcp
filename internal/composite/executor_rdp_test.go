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

// listRDPsTool returns the list-rdps tool definition for tests.
func listRDPsTool() CompositeTool {
	return CompositeTool{
		Name:        "list-rdps",
		Description: "List all REST Delivery Points in a Message VPN",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "maxResults", Type: "integer", Required: false},
		},
		Steps: []Step{
			{
				ID:          "rdps",
				Operation:   "monitor/getMsgVpnRestDeliveryPoints",
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

// createRDPTool returns the create-rdp tool definition for tests. Like
// create-queue, msgVpnName is the only path param; restDeliveryPointName and
// rdpConfig are assembled into the body by constructRequestBody.
func createRDPTool() CompositeTool {
	return CompositeTool{
		Name:        "create-rdp",
		Description: "Create a REST Delivery Point in a Message VPN",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "restDeliveryPointName", Type: "string", Required: true},
			{Name: "rdpConfig", Type: "object", Required: false},
		},
		Steps: []Step{
			{
				ID:        "createRdp",
				Operation: "config/createMsgVpnRestDeliveryPoint",
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// updateRDPTool returns the update-rdp tool definition for tests. Both
// msgVpnName and restDeliveryPointName are path params (passed as step args);
// only rdpConfig is spread into the PATCH body.
func updateRDPTool() CompositeTool {
	return CompositeTool{
		Name:        "update-rdp",
		Description: "Update an existing REST Delivery Point",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "restDeliveryPointName", Type: "string", Required: true},
			{Name: "rdpConfig", Type: "object", Required: true},
		},
		Steps: []Step{
			{
				ID:        "updateRdp",
				Operation: "config/updateMsgVpnRestDeliveryPoint",
				Args: map[string]string{
					"msgVpnName":            "{{.Params.msgVpnName}}",
					"restDeliveryPointName": "{{.Params.restDeliveryPointName}}",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// deleteRDPTool returns the delete-rdp tool definition for tests. Both names are
// path args and the operation carries no request body.
func deleteRDPTool() CompositeTool {
	return CompositeTool{
		Name:        "delete-rdp",
		Description: "Delete a REST Delivery Point from a Message VPN",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "restDeliveryPointName", Type: "string", Required: true},
		},
		Steps: []Step{
			{
				ID:        "deleteRdp",
				Operation: "config/deleteMsgVpnRestDeliveryPoint",
				Args: map[string]string{
					"msgVpnName":            "{{.Params.msgVpnName}}",
					"restDeliveryPointName": "{{.Params.restDeliveryPointName}}",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// makeRDPItems builds a slice of n mock RDP objects for use in paginated responses.
func makeRDPItems(n int) []any {
	items := make([]any, n)
	for i := range items {
		items[i] = map[string]any{"restDeliveryPointName": fmt.Sprintf("rdp-%d", i), "enabled": true}
	}
	return items
}

func TestExecute_ListRDPs_SinglePage(t *testing.T) {
	rdpItems := []any{
		map[string]any{"restDeliveryPointName": "rdp-1", "enabled": true, "up": true},
		map[string]any{"restDeliveryPointName": "rdp-2", "enabled": true, "up": false},
	}
	client := newSeqMockClient()
	client.addResponses("getMsgVpnRestDeliveryPoints", pageResult(rdpItems, ""))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listRDPsTool(), client, map[string]any{
		"msgVpnName": "default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rdps, ok := result["rdps"].(map[string]any)
	if !ok {
		t.Fatal("expected rdps key containing a map")
	}
	items, ok := rdps["data"].([]any)
	if !ok {
		t.Fatal("expected rdps.data to be a slice")
	}
	if len(items) != 2 {
		t.Errorf("len(items) = %d, want 2", len(items))
	}
	if rdps["truncated"] != false {
		t.Errorf("truncated = %v, want false", rdps["truncated"])
	}
	if len(client.calls) != 1 {
		t.Errorf("expected 1 SEMP call, got %d", len(client.calls))
	}
}

func TestExecute_ListRDPs_TruncatesAtMaxResults(t *testing.T) {
	// Page has 100 RDPs but maxResults=50, paginator should stop and set truncated.
	client := newSeqMockClient()
	client.addResponses("getMsgVpnRestDeliveryPoints", pageResult(makeRDPItems(100), "cursor-next"))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listRDPsTool(), client, map[string]any{
		"msgVpnName": "default",
		"maxResults": float64(50),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rdps := result["rdps"].(map[string]any)
	items := rdps["data"].([]any)

	if len(items) != 50 {
		t.Errorf("len(items) = %d, want 50", len(items))
	}
	if rdps["truncated"] != true {
		t.Errorf("truncated = %v, want true", rdps["truncated"])
	}
	if len(client.calls) != 1 {
		t.Errorf("expected 1 SEMP call, got %d", len(client.calls))
	}
}

func TestExecute_CreateRDP_ConstructsBody(t *testing.T) {
	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: newMockClient(), recorded: &recorded, mu: &mu}

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), createRDPTool(), capture, map[string]any{
		"msgVpnName":            "vpn-a",
		"restDeliveryPointName": "rdp-1",
		"rdpConfig": map[string]any{
			"enabled":           true,
			"clientProfileName": "default",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if recorded[0].opID != "createMsgVpnRestDeliveryPoint" {
		t.Errorf("opID = %q, want createMsgVpnRestDeliveryPoint", recorded[0].opID)
	}
	// msgVpnName identifies the VPN via the URL path, so it stays in args.
	if recorded[0].args["msgVpnName"] != "vpn-a" {
		t.Errorf("args[msgVpnName] = %v, want vpn-a", recorded[0].args["msgVpnName"])
	}

	body, ok := recorded[0].args["body"].(map[string]any)
	if !ok {
		t.Fatalf("expected body to be a map, got %T", recorded[0].args["body"])
	}
	// createMsgVpnRestDeliveryPoint declares no restDeliveryPointName path param,
	// so the RDP name rides in the body as a scalar.
	if body["restDeliveryPointName"] != "rdp-1" {
		t.Errorf("body[restDeliveryPointName] = %v, want rdp-1", body["restDeliveryPointName"])
	}
	// rdpConfig fields are spread into the body as siblings of restDeliveryPointName.
	if body["enabled"] != true {
		t.Errorf("body[enabled] = %v, want true", body["enabled"])
	}
	if body["clientProfileName"] != "default" {
		t.Errorf("body[clientProfileName] = %v, want default", body["clientProfileName"])
	}
	// msgVpnName is a path param and must not leak into the create body.
	if _, leaked := body["msgVpnName"]; leaked {
		t.Error("msgVpnName is a path param and must not appear in the create body")
	}
	if _, nested := body["rdpConfig"]; nested {
		t.Error("rdpConfig should be spread into the body, not nested under rdpConfig")
	}

	if result["createRdp"] == nil {
		t.Error("expected createRdp result to be collected")
	}
}

func TestExecute_CreateRDP_AlreadyExists(t *testing.T) {
	client := newMockClient()
	client.errors["createMsgVpnRestDeliveryPoint"] = &sempv2.SEMPError{
		Operation:   "createMsgVpnRestDeliveryPoint",
		StatusCode:  400,
		SEMPCode:    10,
		SEMPStatus:  "ALREADY_EXISTS",
		Description: "REST Delivery Point already exists",
		Body:        `{"meta":{"error":{"code":10,"description":"REST Delivery Point already exists","status":"ALREADY_EXISTS"}}}`,
	}

	executor := NewCompositeExecutor(testOperations())

	_, err := executor.Execute(context.Background(), createRDPTool(), client, map[string]any{
		"msgVpnName":            "vpn-a",
		"restDeliveryPointName": "rdp-1",
	})
	if err == nil {
		t.Fatal("expected error when RDP already exists, got nil")
	}

	var sempErr *sempv2.SEMPError
	if !errors.As(err, &sempErr) {
		t.Errorf("expected SEMPError in error chain, got: %v", err)
	} else if sempErr.SEMPCode != 10 {
		t.Errorf("SEMPCode = %d, want 10 (already exists)", sempErr.SEMPCode)
	}
}

func TestExecute_UpdateRDP_BodyExcludesPathParams(t *testing.T) {
	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: newMockClient(), recorded: &recorded, mu: &mu}

	executor := NewCompositeExecutor(testOperations())

	_, err := executor.Execute(context.Background(), updateRDPTool(), capture, map[string]any{
		"msgVpnName":            "vpn-a",
		"restDeliveryPointName": "rdp-1",
		"rdpConfig": map[string]any{
			"enabled": false,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if recorded[0].opID != "updateMsgVpnRestDeliveryPoint" {
		t.Errorf("opID = %q, want updateMsgVpnRestDeliveryPoint", recorded[0].opID)
	}
	if recorded[0].args["msgVpnName"] != "vpn-a" {
		t.Errorf("args[msgVpnName] = %v, want vpn-a", recorded[0].args["msgVpnName"])
	}
	if recorded[0].args["restDeliveryPointName"] != "rdp-1" {
		t.Errorf("args[restDeliveryPointName] = %v, want rdp-1", recorded[0].args["restDeliveryPointName"])
	}

	body, ok := recorded[0].args["body"].(map[string]any)
	if !ok {
		t.Fatalf("expected body to be a map, got %T", recorded[0].args["body"])
	}
	// Both names are path params; neither may be duplicated into the PATCH body.
	if _, leaked := body["msgVpnName"]; leaked {
		t.Error("msgVpnName is a path param and must not appear in the PATCH body")
	}
	if _, leaked := body["restDeliveryPointName"]; leaked {
		t.Error("restDeliveryPointName is a path param and must not appear in the PATCH body")
	}
	if body["enabled"] != false {
		t.Errorf("body[enabled] = %v, want false", body["enabled"])
	}
}

func TestExecute_DeleteRDP_NoBodyConstructed(t *testing.T) {
	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: newMockClient(), recorded: &recorded, mu: &mu}

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), deleteRDPTool(), capture, map[string]any{
		"msgVpnName":            "vpn-a",
		"restDeliveryPointName": "rdp-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if recorded[0].opID != "deleteMsgVpnRestDeliveryPoint" {
		t.Errorf("opID = %q, want deleteMsgVpnRestDeliveryPoint", recorded[0].opID)
	}
	if recorded[0].args["restDeliveryPointName"] != "rdp-1" {
		t.Errorf("args[restDeliveryPointName] = %v, want rdp-1", recorded[0].args["restDeliveryPointName"])
	}
	// deleteMsgVpnRestDeliveryPoint declares no body parameter, so no body should be built.
	if _, hasBody := recorded[0].args["body"]; hasBody {
		t.Error("delete operation has no body param; no body should be constructed")
	}

	if result["deleteRdp"] == nil {
		t.Error("expected deleteRdp result to be collected")
	}
}
