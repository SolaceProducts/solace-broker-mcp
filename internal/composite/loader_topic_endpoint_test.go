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
	"testing"
	"testing/fstest"
)

func TestLoadTools_CreateTopicEndpoint(t *testing.T) {
	yaml := `
tools:
  - name: create-topic-endpoint
    description: Create a topic endpoint in a Message VPN.
    annotations:
      readOnly: false
      destructive: false
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The Message VPN to create the topic endpoint in"
      - name: topicEndpointName
        type: string
        required: true
        description: "The name of the topic endpoint to create"
      - name: topicEndpointConfig
        type: object
        required: false
        description: "Optional TopicEndpoint attributes"
    steps:
      - id: createTopicEndpoint
        operation: config/createMsgVpnTopicEndpoint
        args:
          msgVpnName: "{{.Params.msgVpnName}}"
    result:
      strategy: collect
`
	fsys := fstest.MapFS{
		"tools.yaml": &fstest.MapFile{Data: []byte(yaml)},
	}

	tools, err := LoadTools(fsys, "tools.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	tool := tools[0]
	if tool.Name != "create-topic-endpoint" {
		t.Errorf("expected name %q, got %q", "create-topic-endpoint", tool.Name)
	}

	if tool.Annotations.ReadOnly == nil || *tool.Annotations.ReadOnly {
		t.Error("expected ReadOnly = false")
	}
	if tool.Annotations.Destructive == nil || *tool.Annotations.Destructive {
		t.Error("expected Destructive = false")
	}

	if len(tool.Parameters) != 3 {
		t.Fatalf("expected 3 parameters, got %d", len(tool.Parameters))
	}
	if tool.Parameters[1].Name != "topicEndpointName" || tool.Parameters[1].Type != "string" || !tool.Parameters[1].Required {
		t.Errorf("expected required string topicEndpointName parameter, got %+v", tool.Parameters[1])
	}
	if tool.Parameters[2].Name != "topicEndpointConfig" || tool.Parameters[2].Type != "object" || tool.Parameters[2].Required {
		t.Errorf("expected optional object topicEndpointConfig parameter, got %+v", tool.Parameters[2])
	}

	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if tool.Steps[0].Operation != "config/createMsgVpnTopicEndpoint" {
		t.Errorf("expected operation %q, got %q", "config/createMsgVpnTopicEndpoint", tool.Steps[0].Operation)
	}
	// Only msgVpnName is a path arg; topicEndpointName belongs in the body.
	if tool.Steps[0].Args["msgVpnName"] != "{{.Params.msgVpnName}}" {
		t.Errorf("expected msgVpnName path arg, got %q", tool.Steps[0].Args["msgVpnName"])
	}
	if _, ok := tool.Steps[0].Args["topicEndpointName"]; ok {
		t.Error("topicEndpointName must not be a step arg on create — it belongs in the body")
	}
	if tool.Result.Strategy != "collect" {
		t.Errorf("expected strategy %q, got %q", "collect", tool.Result.Strategy)
	}
}

func TestLoadTools_UpdateTopicEndpoint(t *testing.T) {
	yaml := `
tools:
  - name: update-topic-endpoint
    description: Update an existing topic endpoint.
    annotations:
      readOnly: false
      destructive: true
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The Message VPN containing the topic endpoint"
      - name: topicEndpointName
        type: string
        required: true
        description: "The name of the topic endpoint to modify"
      - name: topicEndpointConfig
        type: object
        required: true
        description: "TopicEndpoint attributes to update"
    steps:
      - id: updateTopicEndpoint
        operation: config/updateMsgVpnTopicEndpoint
        args:
          msgVpnName: "{{.Params.msgVpnName}}"
          topicEndpointName: "{{.Params.topicEndpointName}}"
    result:
      strategy: collect
`
	fsys := fstest.MapFS{
		"tools.yaml": &fstest.MapFile{Data: []byte(yaml)},
	}

	tools, err := LoadTools(fsys, "tools.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	tool := tools[0]
	if tool.Name != "update-topic-endpoint" {
		t.Errorf("expected name %q, got %q", "update-topic-endpoint", tool.Name)
	}

	if tool.Annotations.ReadOnly == nil || *tool.Annotations.ReadOnly {
		t.Error("expected ReadOnly = false")
	}
	if tool.Annotations.Destructive == nil || !*tool.Annotations.Destructive {
		t.Error("expected Destructive = true")
	}

	if len(tool.Parameters) != 3 {
		t.Fatalf("expected 3 parameters, got %d", len(tool.Parameters))
	}
	if tool.Parameters[2].Name != "topicEndpointConfig" || tool.Parameters[2].Type != "object" || !tool.Parameters[2].Required {
		t.Errorf("expected required object topicEndpointConfig parameter, got %+v", tool.Parameters[2])
	}

	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if tool.Steps[0].Operation != "config/updateMsgVpnTopicEndpoint" {
		t.Errorf("expected operation %q, got %q", "config/updateMsgVpnTopicEndpoint", tool.Steps[0].Operation)
	}
	if tool.Steps[0].Args["msgVpnName"] != "{{.Params.msgVpnName}}" {
		t.Errorf("expected msgVpnName path arg, got %q", tool.Steps[0].Args["msgVpnName"])
	}
	if tool.Steps[0].Args["topicEndpointName"] != "{{.Params.topicEndpointName}}" {
		t.Errorf("expected topicEndpointName path arg, got %q", tool.Steps[0].Args["topicEndpointName"])
	}
	if tool.Result.Strategy != "collect" {
		t.Errorf("expected strategy %q, got %q", "collect", tool.Result.Strategy)
	}
}

func TestLoadTools_DeleteTopicEndpoint(t *testing.T) {
	yaml := `
tools:
  - name: delete-topic-endpoint
    description: Delete a topic endpoint from a Message VPN.
    annotations:
      readOnly: false
      destructive: true
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The Message VPN containing the topic endpoint"
      - name: topicEndpointName
        type: string
        required: true
        description: "The name of the topic endpoint to delete"
    steps:
      - id: deleteTopicEndpoint
        operation: config/deleteMsgVpnTopicEndpoint
        args:
          msgVpnName: "{{.Params.msgVpnName}}"
          topicEndpointName: "{{.Params.topicEndpointName}}"
    result:
      strategy: collect
`
	fsys := fstest.MapFS{
		"tools.yaml": &fstest.MapFile{Data: []byte(yaml)},
	}

	tools, err := LoadTools(fsys, "tools.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	tool := tools[0]
	if tool.Name != "delete-topic-endpoint" {
		t.Errorf("expected name %q, got %q", "delete-topic-endpoint", tool.Name)
	}

	if tool.Annotations.ReadOnly == nil || *tool.Annotations.ReadOnly {
		t.Error("expected ReadOnly = false")
	}
	if tool.Annotations.Destructive == nil || !*tool.Annotations.Destructive {
		t.Error("expected Destructive = true")
	}

	if len(tool.Parameters) != 2 {
		t.Fatalf("expected 2 parameters, got %d", len(tool.Parameters))
	}

	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if tool.Steps[0].Operation != "config/deleteMsgVpnTopicEndpoint" {
		t.Errorf("expected operation %q, got %q", "config/deleteMsgVpnTopicEndpoint", tool.Steps[0].Operation)
	}
	if tool.Steps[0].Args["msgVpnName"] != "{{.Params.msgVpnName}}" {
		t.Errorf("expected msgVpnName path arg, got %q", tool.Steps[0].Args["msgVpnName"])
	}
	if tool.Steps[0].Args["topicEndpointName"] != "{{.Params.topicEndpointName}}" {
		t.Errorf("expected topicEndpointName path arg, got %q", tool.Steps[0].Args["topicEndpointName"])
	}
	if tool.Result.Strategy != "collect" {
		t.Errorf("expected strategy %q, got %q", "collect", tool.Result.Strategy)
	}
}