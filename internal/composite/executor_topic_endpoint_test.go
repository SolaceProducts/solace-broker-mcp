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
	"sync"
	"testing"
)

// These three tests are deliberately kept even though constructRequestBody's
// path-param-exclusion mechanism is already exhaustively tested via VPN's
// canonical tests (executor_vpn_test.go) — that mechanism is generic, but it
// stays correct regardless of resource name. What isn't generic is whether
// THIS tool's own YAML-mirroring field names (topicEndpointName,
// topicEndpointConfig) are wired to the right path/body split. A rename typo
// in tools.yaml's create/update/delete-topic-endpoint definitions would pass
// every other test in this package and only show up here, fast, without a
// Docker broker — test/e2e-management proves the same thing but in minutes,
// not milliseconds, and isn't part of `make check`.

// createTopicEndpointTool returns the create-topic-endpoint tool definition for
// tests. Like create-queue, msgVpnName is the only path param; topicEndpointName
// and topicEndpointConfig are assembled into the body.
func createTopicEndpointTool() CompositeTool {
	return CompositeTool{
		Name:        "create-topic-endpoint",
		Description: "Create a topic endpoint in a Message VPN",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "topicEndpointName", Type: "string", Required: true},
			{Name: "topicEndpointConfig", Type: "object", Required: false},
		},
		Steps: []Step{
			{
				ID:        "createTopicEndpoint",
				Operation: "config/createMsgVpnTopicEndpoint",
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// updateTopicEndpointTool returns the update-topic-endpoint tool definition for
// tests. Both msgVpnName and topicEndpointName are path params (passed as step
// args); only topicEndpointConfig is spread into the PATCH body.
func updateTopicEndpointTool() CompositeTool {
	return CompositeTool{
		Name:        "update-topic-endpoint",
		Description: "Update an existing topic endpoint",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "topicEndpointName", Type: "string", Required: true},
			{Name: "topicEndpointConfig", Type: "object", Required: true},
		},
		Steps: []Step{
			{
				ID:        "updateTopicEndpoint",
				Operation: "config/updateMsgVpnTopicEndpoint",
				Args: map[string]string{
					"msgVpnName":        "{{.Params.msgVpnName}}",
					"topicEndpointName": "{{.Params.topicEndpointName}}",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// deleteTopicEndpointTool returns the delete-topic-endpoint tool definition for
// tests. Both names are path args and the operation carries no request body.
func deleteTopicEndpointTool() CompositeTool {
	return CompositeTool{
		Name:        "delete-topic-endpoint",
		Description: "Delete a topic endpoint from a Message VPN",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "topicEndpointName", Type: "string", Required: true},
		},
		Steps: []Step{
			{
				ID:        "deleteTopicEndpoint",
				Operation: "config/deleteMsgVpnTopicEndpoint",
				Args: map[string]string{
					"msgVpnName":        "{{.Params.msgVpnName}}",
					"topicEndpointName": "{{.Params.topicEndpointName}}",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

func TestExecute_CreateTopicEndpoint_ConstructsBody(t *testing.T) {
	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: newMockClient(), recorded: &recorded, mu: &mu}

	executor := NewCompositeExecutor(testOperations())

	_, err := executor.Execute(context.Background(), createTopicEndpointTool(), capture, map[string]any{
		"msgVpnName":        "vpn-a",
		"topicEndpointName": "te-1",
		"topicEndpointConfig": map[string]any{
			"accessType": "exclusive",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if recorded[0].opID != "createMsgVpnTopicEndpoint" {
		t.Errorf("opID = %q, want createMsgVpnTopicEndpoint", recorded[0].opID)
	}
	if recorded[0].args["msgVpnName"] != "vpn-a" {
		t.Errorf("args[msgVpnName] = %v, want vpn-a", recorded[0].args["msgVpnName"])
	}

	body, ok := recorded[0].args["body"].(map[string]any)
	if !ok {
		t.Fatalf("expected body to be a map, got %T", recorded[0].args["body"])
	}
	if body["topicEndpointName"] != "te-1" {
		t.Errorf("body[topicEndpointName] = %v, want te-1", body["topicEndpointName"])
	}
	if body["accessType"] != "exclusive" {
		t.Errorf("body[accessType] = %v, want exclusive", body["accessType"])
	}
	if _, leaked := body["msgVpnName"]; leaked {
		t.Error("msgVpnName is a path param and must not appear in the create body")
	}
}

func TestExecute_UpdateTopicEndpoint_BodyExcludesPathParams(t *testing.T) {
	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: newMockClient(), recorded: &recorded, mu: &mu}

	executor := NewCompositeExecutor(testOperations())

	_, err := executor.Execute(context.Background(), updateTopicEndpointTool(), capture, map[string]any{
		"msgVpnName":        "vpn-a",
		"topicEndpointName": "te-1",
		"topicEndpointConfig": map[string]any{
			"egressEnabled": false,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if recorded[0].opID != "updateMsgVpnTopicEndpoint" {
		t.Errorf("opID = %q, want updateMsgVpnTopicEndpoint", recorded[0].opID)
	}
	if recorded[0].args["msgVpnName"] != "vpn-a" {
		t.Errorf("args[msgVpnName] = %v, want vpn-a", recorded[0].args["msgVpnName"])
	}
	if recorded[0].args["topicEndpointName"] != "te-1" {
		t.Errorf("args[topicEndpointName] = %v, want te-1", recorded[0].args["topicEndpointName"])
	}

	body, ok := recorded[0].args["body"].(map[string]any)
	if !ok {
		t.Fatalf("expected body to be a map, got %T", recorded[0].args["body"])
	}
	if _, leaked := body["msgVpnName"]; leaked {
		t.Error("msgVpnName is a path param and must not appear in the PATCH body")
	}
	if _, leaked := body["topicEndpointName"]; leaked {
		t.Error("topicEndpointName is a path param and must not appear in the PATCH body")
	}
	if body["egressEnabled"] != false {
		t.Errorf("body[egressEnabled] = %v, want false", body["egressEnabled"])
	}
}

func TestExecute_DeleteTopicEndpoint_NoBodyConstructed(t *testing.T) {
	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: newMockClient(), recorded: &recorded, mu: &mu}

	executor := NewCompositeExecutor(testOperations())

	_, err := executor.Execute(context.Background(), deleteTopicEndpointTool(), capture, map[string]any{
		"msgVpnName":        "vpn-a",
		"topicEndpointName": "te-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if recorded[0].opID != "deleteMsgVpnTopicEndpoint" {
		t.Errorf("opID = %q, want deleteMsgVpnTopicEndpoint", recorded[0].opID)
	}
	if _, hasBody := recorded[0].args["body"]; hasBody {
		t.Error("delete operation has no body param; no body should be constructed")
	}
}
