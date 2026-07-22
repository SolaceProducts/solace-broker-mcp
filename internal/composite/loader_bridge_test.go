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

func TestLoadTools_ListBridges(t *testing.T) {
	yaml := `
tools:
  - name: list-bridges
    description: >
      List Bridges in a Message VPN.
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The Message VPN to list Bridges for"
      - name: maxResults
        type: integer
        required: false
        description: "Maximum number of bridges to return (default 100, max 500)"
    steps:
      - id: bridges
        operation: monitor/getMsgVpnBridges
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
	if tool.Name != "list-bridges" {
		t.Errorf("expected name %q, got %q", "list-bridges", tool.Name)
	}
	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if !tool.Steps[0].FollowPages {
		t.Error("expected FollowPages=true for list-bridges step")
	}
	if tool.Steps[0].Operation != "monitor/getMsgVpnBridges" {
		t.Errorf("expected operation %q, got %q", "monitor/getMsgVpnBridges", tool.Steps[0].Operation)
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

// TestLoadTools_GetBridgeStatus pins that both of a bridge's identifying path
// params — bridgeName and bridgeVirtualRouter — parse as independent
// parameters and step args. Bridges are the only object in this server
// identified by two names rather than one (the SEMP path segment is a
// literal "{bridgeName},{bridgeVirtualRouter}"), so a YAML authoring mistake
// dropping one of the two would silently produce a tool that always 404s.
func TestLoadTools_GetBridgeStatus(t *testing.T) {
	yaml := `
tools:
  - name: get-bridge-status
    description: >
      Get the operational status of a Bridge.
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The Message VPN containing the Bridge"
      - name: bridgeName
        type: string
        required: true
        description: "The name of the Bridge"
      - name: bridgeVirtualRouter
        type: string
        required: true
        description: "The virtual router of the Bridge"
    steps:
      - id: bridgeStatus
        operation: monitor/getMsgVpnBridge
        args:
          msgVpnName: "{{.Params.msgVpnName}}"
          bridgeName: "{{.Params.bridgeName}}"
          bridgeVirtualRouter: "{{.Params.bridgeVirtualRouter}}"
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
	if tool.Name != "get-bridge-status" {
		t.Errorf("expected name %q, got %q", "get-bridge-status", tool.Name)
	}

	if len(tool.Parameters) != 3 {
		t.Fatalf("expected 3 parameters, got %d", len(tool.Parameters))
	}
	for i, name := range []string{"msgVpnName", "bridgeName", "bridgeVirtualRouter"} {
		if tool.Parameters[i].Name != name || tool.Parameters[i].Type != "string" || !tool.Parameters[i].Required {
			t.Errorf("parameter %d: expected required string %q, got %+v", i, name, tool.Parameters[i])
		}
	}

	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	step := tool.Steps[0]
	if step.Operation != "monitor/getMsgVpnBridge" {
		t.Errorf("expected operation %q, got %q", "monitor/getMsgVpnBridge", step.Operation)
	}
	if step.Args["msgVpnName"] != "{{.Params.msgVpnName}}" {
		t.Errorf("expected msgVpnName path arg, got %q", step.Args["msgVpnName"])
	}
	if step.Args["bridgeName"] != "{{.Params.bridgeName}}" {
		t.Errorf("expected bridgeName path arg, got %q", step.Args["bridgeName"])
	}
	if step.Args["bridgeVirtualRouter"] != "{{.Params.bridgeVirtualRouter}}" {
		t.Errorf("expected bridgeVirtualRouter path arg, got %q", step.Args["bridgeVirtualRouter"])
	}
	if tool.Result.Strategy != "collect" {
		t.Errorf("expected strategy %q, got %q", "collect", tool.Result.Strategy)
	}
}
