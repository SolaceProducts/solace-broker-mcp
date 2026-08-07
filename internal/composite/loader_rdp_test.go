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

func TestLoadTools_ListRDPs(t *testing.T) {
	yaml := `
tools:
  - name: list-rdps
    description: >
      List all REST Delivery Points in a Message VPN.
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The Message VPN to list REST Delivery Points for"
      - name: maxResults
        type: integer
        required: false
        description: "Maximum number of RDPs to return (default 100, max 500)"
    steps:
      - id: rdps
        operation: monitor/getMsgVpnRestDeliveryPoints
        followPages: true
        args:
          msgVpnName: "{{.Params.msgVpnName}}"
          count: "100"
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
	if tool.Name != "list-rdps" {
		t.Errorf("expected name %q, got %q", "list-rdps", tool.Name)
	}
	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if !tool.Steps[0].FollowPages {
		t.Error("expected FollowPages=true for list-rdps step")
	}
	if tool.Steps[0].Operation != "monitor/getMsgVpnRestDeliveryPoints" {
		t.Errorf("expected operation %q, got %q", "monitor/getMsgVpnRestDeliveryPoints", tool.Steps[0].Operation)
	}
	if tool.Result.Strategy != "collect" {
		t.Errorf("expected strategy %q, got %q", "collect", tool.Result.Strategy)
	}

	if len(tool.Parameters) != 2 {
		t.Fatalf("expected 2 parameters, got %d", len(tool.Parameters))
	}
	if tool.Parameters[0].Name != "msgVpnName" {
		t.Errorf("expected first parameter name %q, got %q", "msgVpnName", tool.Parameters[0].Name)
	}
	maxResults := tool.Parameters[1]
	if maxResults.Name != "maxResults" {
		t.Errorf("expected parameter name %q, got %q", "maxResults", maxResults.Name)
	}
	if maxResults.Required {
		t.Error("expected maxResults to be optional (required=false)")
	}
}

func TestLoadTools_CreateRDP(t *testing.T) {
	yaml := `
tools:
  - name: create-rdp
    description: Create a REST Delivery Point in a Message VPN.
    annotations:
      readOnly: false
      destructive: false
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The Message VPN to create the RDP in"
      - name: restDeliveryPointName
        type: string
        required: true
        description: "The name of the REST Delivery Point to create"
      - name: rdpConfig
        type: object
        required: false
        description: "Optional RestDeliveryPoint attributes"
    steps:
      - id: createRdp
        operation: config/createMsgVpnRestDeliveryPoint
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
	if tool.Name != "create-rdp" {
		t.Errorf("expected name %q, got %q", "create-rdp", tool.Name)
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
	if tool.Parameters[1].Name != "restDeliveryPointName" || tool.Parameters[1].Type != "string" || !tool.Parameters[1].Required {
		t.Errorf("expected required string restDeliveryPointName parameter, got %+v", tool.Parameters[1])
	}
	if tool.Parameters[2].Name != "rdpConfig" || tool.Parameters[2].Type != "object" || tool.Parameters[2].Required {
		t.Errorf("expected optional object rdpConfig parameter, got %+v", tool.Parameters[2])
	}

	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if tool.Steps[0].Operation != "config/createMsgVpnRestDeliveryPoint" {
		t.Errorf("expected operation %q, got %q", "config/createMsgVpnRestDeliveryPoint", tool.Steps[0].Operation)
	}
	// Only msgVpnName is a path arg; restDeliveryPointName belongs in the body.
	if tool.Steps[0].Args["msgVpnName"] != "{{.Params.msgVpnName}}" {
		t.Errorf("expected msgVpnName path arg, got %q", tool.Steps[0].Args["msgVpnName"])
	}
	if _, ok := tool.Steps[0].Args["restDeliveryPointName"]; ok {
		t.Error("restDeliveryPointName must not be a step arg on create — it belongs in the body")
	}
	if tool.Result.Strategy != "collect" {
		t.Errorf("expected strategy %q, got %q", "collect", tool.Result.Strategy)
	}
}

func TestLoadTools_UpdateRDP(t *testing.T) {
	yaml := `
tools:
  - name: update-rdp
    description: Update an existing REST Delivery Point.
    annotations:
      readOnly: false
      destructive: true
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The Message VPN containing the RDP"
      - name: restDeliveryPointName
        type: string
        required: true
        description: "The name of the REST Delivery Point to modify"
      - name: rdpConfig
        type: object
        required: true
        description: "RestDeliveryPoint attributes to update"
    steps:
      - id: updateRdp
        operation: config/updateMsgVpnRestDeliveryPoint
        args:
          msgVpnName: "{{.Params.msgVpnName}}"
          restDeliveryPointName: "{{.Params.restDeliveryPointName}}"
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
	if tool.Name != "update-rdp" {
		t.Errorf("expected name %q, got %q", "update-rdp", tool.Name)
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
	if tool.Parameters[2].Name != "rdpConfig" || tool.Parameters[2].Type != "object" || !tool.Parameters[2].Required {
		t.Errorf("expected required object rdpConfig parameter, got %+v", tool.Parameters[2])
	}

	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if tool.Steps[0].Operation != "config/updateMsgVpnRestDeliveryPoint" {
		t.Errorf("expected operation %q, got %q", "config/updateMsgVpnRestDeliveryPoint", tool.Steps[0].Operation)
	}
	// Both names are path params, so both are wired as step args.
	if tool.Steps[0].Args["msgVpnName"] != "{{.Params.msgVpnName}}" {
		t.Errorf("expected msgVpnName path arg, got %q", tool.Steps[0].Args["msgVpnName"])
	}
	if tool.Steps[0].Args["restDeliveryPointName"] != "{{.Params.restDeliveryPointName}}" {
		t.Errorf("expected restDeliveryPointName path arg, got %q", tool.Steps[0].Args["restDeliveryPointName"])
	}
	if tool.Result.Strategy != "collect" {
		t.Errorf("expected strategy %q, got %q", "collect", tool.Result.Strategy)
	}
}

func TestLoadTools_DeleteRDP(t *testing.T) {
	yaml := `
tools:
  - name: delete-rdp
    description: Delete a REST Delivery Point from a Message VPN.
    annotations:
      readOnly: false
      destructive: true
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The Message VPN containing the RDP"
      - name: restDeliveryPointName
        type: string
        required: true
        description: "The name of the REST Delivery Point to delete"
    steps:
      - id: deleteRdp
        operation: config/deleteMsgVpnRestDeliveryPoint
        args:
          msgVpnName: "{{.Params.msgVpnName}}"
          restDeliveryPointName: "{{.Params.restDeliveryPointName}}"
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
	if tool.Name != "delete-rdp" {
		t.Errorf("expected name %q, got %q", "delete-rdp", tool.Name)
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
	if tool.Steps[0].Operation != "config/deleteMsgVpnRestDeliveryPoint" {
		t.Errorf("expected operation %q, got %q", "config/deleteMsgVpnRestDeliveryPoint", tool.Steps[0].Operation)
	}
	if tool.Steps[0].Args["msgVpnName"] != "{{.Params.msgVpnName}}" {
		t.Errorf("expected msgVpnName path arg, got %q", tool.Steps[0].Args["msgVpnName"])
	}
	if tool.Steps[0].Args["restDeliveryPointName"] != "{{.Params.restDeliveryPointName}}" {
		t.Errorf("expected restDeliveryPointName path arg, got %q", tool.Steps[0].Args["restDeliveryPointName"])
	}
	if tool.Result.Strategy != "collect" {
		t.Errorf("expected strategy %q, got %q", "collect", tool.Result.Strategy)
	}
}