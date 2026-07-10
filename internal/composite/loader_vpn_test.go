package composite

import (
	"testing"
	"testing/fstest"
)

func TestLoadTools_GetVPNHealth(t *testing.T) {
	yaml := `
tools:
  - name: get-vpn-health
    description: >
      Get health and connection statistics for a Message VPN.
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The name of the Message VPN"
    steps:
      - id: vpnHealth
        operation: monitor/getMsgVpn
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
	if tool.Name != "get-vpn-health" {
		t.Errorf("expected name %q, got %q", "get-vpn-health", tool.Name)
	}
	if len(tool.Parameters) != 1 {
		t.Fatalf("expected 1 parameter, got %d", len(tool.Parameters))
	}
	if tool.Parameters[0].Name != "msgVpnName" || !tool.Parameters[0].Required {
		t.Errorf("expected msgVpnName required parameter, got %+v", tool.Parameters[0])
	}
	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if tool.Steps[0].ID != "vpnHealth" {
		t.Errorf("expected step ID %q, got %q", "vpnHealth", tool.Steps[0].ID)
	}
	if tool.Steps[0].Operation != "monitor/getMsgVpn" {
		t.Errorf("expected operation %q, got %q", "monitor/getMsgVpn", tool.Steps[0].Operation)
	}
	if tool.Steps[0].FollowPages {
		t.Error("expected FollowPages=false for get-vpn-health step")
	}
	if tool.Result.Strategy != "collect" {
		t.Errorf("expected strategy %q, got %q", "collect", tool.Result.Strategy)
	}
}

func TestLoadTools_ListVPNs(t *testing.T) {
	yaml := `
tools:
  - name: list-vpns
    description: >
      List all Message VPNs on the broker.
    parameters:
      - name: maxResults
        type: integer
        required: false
        description: "Maximum number of VPNs to return (default 100, max 500)"
    steps:
      - id: vpns
        operation: monitor/getMsgVpns
        followPages: true
        args:
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
	if tool.Name != "list-vpns" {
		t.Errorf("expected name %q, got %q", "list-vpns", tool.Name)
	}
	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if !tool.Steps[0].FollowPages {
		t.Error("expected FollowPages=true for list-vpns step")
	}
	if tool.Steps[0].Operation != "monitor/getMsgVpns" {
		t.Errorf("expected operation %q, got %q", "monitor/getMsgVpns", tool.Steps[0].Operation)
	}
	if tool.Result.Strategy != "collect" {
		t.Errorf("expected strategy %q, got %q", "collect", tool.Result.Strategy)
	}
}

func TestLoadTools_GetMessageRates(t *testing.T) {
	yaml := `
tools:
  - name: get-message-rates
    description: >
      Get current and average message and byte rates for a Message VPN.
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The name of the Message VPN"
    steps:
      - id: rates
        operation: monitor/getMsgVpn
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
	if tool.Name != "get-message-rates" {
		t.Errorf("expected name %q, got %q", "get-message-rates", tool.Name)
	}
	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if tool.Steps[0].Operation != "monitor/getMsgVpn" {
		t.Errorf("expected operation %q, got %q", "monitor/getMsgVpn", tool.Steps[0].Operation)
	}
	if tool.Steps[0].FollowPages {
		t.Error("expected FollowPages=false for get-message-rates step")
	}
	if tool.Steps[0].Parallel {
		t.Error("expected Parallel=false for get-message-rates step")
	}
	if len(tool.Parameters) != 1 {
		t.Fatalf("expected 1 parameter, got %d", len(tool.Parameters))
	}
	if tool.Parameters[0].Name != "msgVpnName" || !tool.Parameters[0].Required {
		t.Error("expected required msgVpnName parameter")
	}
}

func TestLoadTools_GetReplicationStatus(t *testing.T) {
	yaml := `
tools:
  - name: get-replication-status
    description: >
      Get replication state for a Message VPN.
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The name of the Message VPN"
    steps:
      - id: replication
        operation: monitor/getMsgVpn
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
	if tool.Name != "get-replication-status" {
		t.Errorf("expected name %q, got %q", "get-replication-status", tool.Name)
	}
	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if tool.Steps[0].ID != "replication" {
		t.Errorf("expected step ID %q, got %q", "replication", tool.Steps[0].ID)
	}
	if tool.Steps[0].Operation != "monitor/getMsgVpn" {
		t.Errorf("expected operation %q, got %q", "monitor/getMsgVpn", tool.Steps[0].Operation)
	}
	if tool.Steps[0].FollowPages {
		t.Error("expected FollowPages=false for get-replication-status step")
	}
	if tool.Steps[0].Parallel {
		t.Error("expected Parallel=false for get-replication-status step")
	}
	if len(tool.Parameters) != 1 {
		t.Fatalf("expected 1 parameter, got %d", len(tool.Parameters))
	}
	if tool.Parameters[0].Name != "msgVpnName" || !tool.Parameters[0].Required {
		t.Error("expected required msgVpnName parameter")
	}
}

func TestLoadTools_CreateMessageVPN(t *testing.T) {
	yaml := `
tools:
  - name: create-message-vpn
    description: Create a Message VPN.
    annotations:
      readOnly: false
      destructive: false
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The name of the Message VPN to create"
      - name: msgVpnConfig
        type: object
        required: false
        description: "Optional MsgVpn attributes"
    steps:
      - id: createVpn
        operation: config/createMsgVpn
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
	if tool.Name != "create-message-vpn" {
		t.Errorf("expected name %q, got %q", "create-message-vpn", tool.Name)
	}

	// Write tool: not read-only, not destructive.
	if tool.Annotations.ReadOnly == nil || *tool.Annotations.ReadOnly {
		t.Error("expected ReadOnly = false")
	}
	if tool.Annotations.Destructive == nil || *tool.Annotations.Destructive {
		t.Error("expected Destructive = false")
	}

	if len(tool.Parameters) != 2 {
		t.Fatalf("expected 2 parameters, got %d", len(tool.Parameters))
	}
	if tool.Parameters[0].Name != "msgVpnName" || tool.Parameters[0].Type != "string" || !tool.Parameters[0].Required {
		t.Errorf("expected required string msgVpnName parameter, got %+v", tool.Parameters[0])
	}
	// msgVpnConfig is an optional object: omitting it lets the broker apply defaults.
	if tool.Parameters[1].Name != "msgVpnConfig" || tool.Parameters[1].Type != "object" || tool.Parameters[1].Required {
		t.Errorf("expected optional object msgVpnConfig parameter, got %+v", tool.Parameters[1])
	}

	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if tool.Steps[0].ID != "createVpn" {
		t.Errorf("expected step ID %q, got %q", "createVpn", tool.Steps[0].ID)
	}
	if tool.Steps[0].Operation != "config/createMsgVpn" {
		t.Errorf("expected operation %q, got %q", "config/createMsgVpn", tool.Steps[0].Operation)
	}
	// Create takes no path arg — the whole body is assembled from params by the executor.
	if len(tool.Steps[0].Args) != 0 {
		t.Errorf("expected no step args, got %v", tool.Steps[0].Args)
	}
	if tool.Result.Strategy != "collect" {
		t.Errorf("expected strategy %q, got %q", "collect", tool.Result.Strategy)
	}
}

func TestLoadTools_UpdateMessageVPN(t *testing.T) {
	yaml := `
tools:
  - name: update-message-vpn
    description: Update an existing Message VPN.
    annotations:
      readOnly: false
      destructive: true
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The name of the Message VPN to modify"
      - name: msgVpnConfig
        type: object
        required: true
        description: "MsgVpn attributes to update"
    steps:
      - id: updateVpn
        operation: config/updateMsgVpn
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
	if tool.Name != "update-message-vpn" {
		t.Errorf("expected name %q, got %q", "update-message-vpn", tool.Name)
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
	if tool.Parameters[0].Name != "msgVpnName" || !tool.Parameters[0].Required {
		t.Errorf("expected required msgVpnName parameter, got %+v", tool.Parameters[0])
	}
	// On update msgVpnConfig is required — there is nothing to change without it.
	if tool.Parameters[1].Name != "msgVpnConfig" || tool.Parameters[1].Type != "object" || !tool.Parameters[1].Required {
		t.Errorf("expected required object msgVpnConfig parameter, got %+v", tool.Parameters[1])
	}

	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if tool.Steps[0].ID != "updateVpn" {
		t.Errorf("expected step ID %q, got %q", "updateVpn", tool.Steps[0].ID)
	}
	if tool.Steps[0].Operation != "config/updateMsgVpn" {
		t.Errorf("expected operation %q, got %q", "config/updateMsgVpn", tool.Steps[0].Operation)
	}
	// msgVpnName is wired as a step arg so it fills the {msgVpnName} path slot.
	if tool.Steps[0].Args["msgVpnName"] != "{{.Params.msgVpnName}}" {
		t.Errorf("expected msgVpnName path arg, got %q", tool.Steps[0].Args["msgVpnName"])
	}
	if tool.Result.Strategy != "collect" {
		t.Errorf("expected strategy %q, got %q", "collect", tool.Result.Strategy)
	}
}

func TestLoadTools_DeleteMessageVPN(t *testing.T) {
	yaml := `
tools:
  - name: delete-message-vpn
    description: Delete a Message VPN.
    annotations:
      readOnly: false
      destructive: true
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The name of the Message VPN to delete"
    steps:
      - id: deleteVpn
        operation: config/deleteMsgVpn
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
	if tool.Name != "delete-message-vpn" {
		t.Errorf("expected name %q, got %q", "delete-message-vpn", tool.Name)
	}

	if tool.Annotations.ReadOnly == nil || *tool.Annotations.ReadOnly {
		t.Error("expected ReadOnly = false")
	}
	// Delete is the one VPN write tool flagged destructive.
	if tool.Annotations.Destructive == nil || !*tool.Annotations.Destructive {
		t.Error("expected Destructive = true")
	}

	if len(tool.Parameters) != 1 {
		t.Fatalf("expected 1 parameter, got %d", len(tool.Parameters))
	}
	if tool.Parameters[0].Name != "msgVpnName" || !tool.Parameters[0].Required {
		t.Errorf("expected required msgVpnName parameter, got %+v", tool.Parameters[0])
	}

	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if tool.Steps[0].ID != "deleteVpn" {
		t.Errorf("expected step ID %q, got %q", "deleteVpn", tool.Steps[0].ID)
	}
	if tool.Steps[0].Operation != "config/deleteMsgVpn" {
		t.Errorf("expected operation %q, got %q", "config/deleteMsgVpn", tool.Steps[0].Operation)
	}
	if tool.Steps[0].Args["msgVpnName"] != "{{.Params.msgVpnName}}" {
		t.Errorf("expected msgVpnName path arg, got %q", tool.Steps[0].Args["msgVpnName"])
	}
	if tool.Result.Strategy != "collect" {
		t.Errorf("expected strategy %q, got %q", "collect", tool.Result.Strategy)
	}
}
