package composite

import (
	"testing"
	"testing/fstest"
)

func TestLoadTools_GetClientDetails(t *testing.T) {
	yaml := `
tools:
  - name: get-client-details
    description: >
      Get performance metrics for a specific connected client including message
      rates, slow subscriber status, and egress discard counts.
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The Message VPN the client is connected to"
      - name: clientName
        type: string
        required: true
        description: "The name of the client connection"
    steps:
      - id: clientDetails
        operation: monitor/getMsgVpnClient
        args:
          msgVpnName: "{{.Params.msgVpnName}}"
          clientName: "{{.Params.clientName}}"
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
	if tool.Name != "get-client-details" {
		t.Errorf("expected name %q, got %q", "get-client-details", tool.Name)
	}
	if len(tool.Parameters) != 2 {
		t.Fatalf("expected 2 parameters, got %d", len(tool.Parameters))
	}
	if tool.Parameters[0].Name != "msgVpnName" || !tool.Parameters[0].Required {
		t.Errorf("expected msgVpnName required parameter, got %+v", tool.Parameters[0])
	}
	if tool.Parameters[1].Name != "clientName" || !tool.Parameters[1].Required {
		t.Errorf("expected clientName required parameter, got %+v", tool.Parameters[1])
	}
	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if tool.Steps[0].ID != "clientDetails" {
		t.Errorf("expected step ID %q, got %q", "clientDetails", tool.Steps[0].ID)
	}
	if tool.Steps[0].Operation != "monitor/getMsgVpnClient" {
		t.Errorf("expected operation %q, got %q", "monitor/getMsgVpnClient", tool.Steps[0].Operation)
	}
}

func TestLoadTools_ListClientSubscriptions(t *testing.T) {
	yaml := `
tools:
  - name: list-client-subscriptions
    description: >
      List topic subscriptions for a specific client.
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The Message VPN the client is connected to"
      - name: clientName
        type: string
        required: true
        description: "The name of the client connection"
      - name: maxResults
        type: integer
        required: false
        description: "Maximum number of subscriptions to return (default 100, max 500)"
    steps:
      - id: subscriptions
        operation: monitor/getMsgVpnClientSubscriptions
        followPages: true
        args:
          msgVpnName: "{{.Params.msgVpnName}}"
          clientName: "{{.Params.clientName}}"
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
	if tool.Name != "list-client-subscriptions" {
		t.Errorf("expected name %q, got %q", "list-client-subscriptions", tool.Name)
	}
	if len(tool.Parameters) != 3 {
		t.Fatalf("expected 3 parameters, got %d", len(tool.Parameters))
	}

	// Verify maxResults is optional.
	maxResults := tool.Parameters[2]
	if maxResults.Name != "maxResults" {
		t.Errorf("expected parameter name %q, got %q", "maxResults", maxResults.Name)
	}
	if maxResults.Required {
		t.Error("expected maxResults to be optional (required=false)")
	}
	if maxResults.Type != "integer" {
		t.Errorf("expected maxResults type %q, got %q", "integer", maxResults.Type)
	}

	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if tool.Steps[0].Operation != "monitor/getMsgVpnClientSubscriptions" {
		t.Errorf("expected operation %q, got %q", "monitor/getMsgVpnClientSubscriptions", tool.Steps[0].Operation)
	}
	if !tool.Steps[0].FollowPages {
		t.Error("expected FollowPages=true for list-client-subscriptions step")
	}
	if tool.Steps[0].Args["count"] != "100" {
		t.Errorf("expected count arg %q, got %q", "100", tool.Steps[0].Args["count"])
	}
}

func TestLoadTools_ListClients(t *testing.T) {
	yaml := `
tools:
  - name: list-clients
    description: >
      List all active client connections in a Message VPN.
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The Message VPN to list clients for"
      - name: maxResults
        type: integer
        required: false
        description: "Maximum number of clients to return (default 100, max 500)"
    steps:
      - id: clients
        operation: monitor/getMsgVpnClients
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
	if tool.Name != "list-clients" {
		t.Errorf("expected name %q, got %q", "list-clients", tool.Name)
	}
	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if !tool.Steps[0].FollowPages {
		t.Error("expected FollowPages=true for list-clients step")
	}
	if tool.Steps[0].Operation != "monitor/getMsgVpnClients" {
		t.Errorf("expected operation %q, got %q", "monitor/getMsgVpnClients", tool.Steps[0].Operation)
	}
	if tool.Result.Strategy != "collect" {
		t.Errorf("expected strategy %q, got %q", "collect", tool.Result.Strategy)
	}
}

func TestLoadTools_ListSlowSubscribers(t *testing.T) {
	yaml := `
tools:
  - name: list-slow-subscribers
    description: >
      List clients flagged as slow subscribers.
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The Message VPN to search for slow subscribers"
      - name: maxResults
        type: integer
        required: false
        description: "Maximum number of slow subscribers to return (default 100, max 500)"
    steps:
      - id: slowSubscribers
        operation: monitor/getMsgVpnClients
        followPages: true
        args:
          msgVpnName: "{{.Params.msgVpnName}}"
          count: "100"
          where: "slowSubscriber==true"
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
	if tool.Name != "list-slow-subscribers" {
		t.Errorf("expected name %q, got %q", "list-slow-subscribers", tool.Name)
	}
	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if tool.Steps[0].ID != "slowSubscribers" {
		t.Errorf("expected step ID %q, got %q", "slowSubscribers", tool.Steps[0].ID)
	}
	if tool.Steps[0].Operation != "monitor/getMsgVpnClients" {
		t.Errorf("expected operation %q, got %q", "monitor/getMsgVpnClients", tool.Steps[0].Operation)
	}
	if !tool.Steps[0].FollowPages {
		t.Error("expected FollowPages=true for list-slow-subscribers step")
	}
	if tool.Result.Strategy != "collect" {
		t.Errorf("expected strategy %q, got %q", "collect", tool.Result.Strategy)
	}

	if len(tool.Parameters) != 2 {
		t.Fatalf("expected 2 parameters, got %d", len(tool.Parameters))
	}
	if tool.Parameters[0].Name != "msgVpnName" || !tool.Parameters[0].Required {
		t.Error("expected required msgVpnName parameter")
	}
	if tool.Parameters[1].Name != "maxResults" || tool.Parameters[1].Required {
		t.Error("expected optional maxResults parameter")
	}
}

func TestLoadTools_DisconnectClient(t *testing.T) {
	yaml := `
tools:
  - name: disconnect-client
    description: Forcibly disconnect a connected client.
    annotations:
      readOnly: false
      destructive: true
      idempotent: false
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The Message VPN the client is connected to."
      - name: clientName
        type: string
        required: true
        description: "The name of the client connection to act on."
    steps:
      - id: disconnect
        operation: action/doMsgVpnClientDisconnect
        args:
          msgVpnName: "{{.Params.msgVpnName}}"
          clientName: "{{.Params.clientName}}"
    result:
      strategy: collect
`
	fsys := fstest.MapFS{"tools.yaml": &fstest.MapFile{Data: []byte(yaml)}}

	tools, err := LoadTools(fsys, "tools.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	tool := tools[0]
	if tool.Annotations.Destructive == nil || !*tool.Annotations.Destructive {
		t.Error("expected Destructive = true")
	}
	if tool.Steps[0].Operation != "action/doMsgVpnClientDisconnect" {
		t.Errorf("expected operation %q, got %q", "action/doMsgVpnClientDisconnect", tool.Steps[0].Operation)
	}
}

func TestLoadTools_ClearClientStats(t *testing.T) {
	yaml := `
tools:
  - name: clear-client-stats
    description: Reset the per-connection statistics counters for a client.
    annotations:
      readOnly: false
      destructive: false
      idempotent: true
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The Message VPN the client is connected to."
      - name: clientName
        type: string
        required: true
        description: "The name of the client connection to act on."
    steps:
      - id: clearStats
        operation: action/doMsgVpnClientClearStats
        args:
          msgVpnName: "{{.Params.msgVpnName}}"
          clientName: "{{.Params.clientName}}"
    result:
      strategy: collect
`
	fsys := fstest.MapFS{"tools.yaml": &fstest.MapFile{Data: []byte(yaml)}}

	tools, err := LoadTools(fsys, "tools.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	tool := tools[0]
	if tool.Annotations.Destructive == nil || *tool.Annotations.Destructive {
		t.Error("expected Destructive = false")
	}
	if tool.Annotations.Idempotent == nil || !*tool.Annotations.Idempotent {
		t.Error("expected Idempotent = true")
	}
	if tool.Steps[0].Operation != "action/doMsgVpnClientClearStats" {
		t.Errorf("expected operation %q, got %q", "action/doMsgVpnClientClearStats", tool.Steps[0].Operation)
	}
}