package composite

import (
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/SolaceDev/solace-broker-mcp/internal/composite/postprocess"
	"github.com/SolaceDev/solace-broker-mcp/internal/composite/postprocess/postprocesstest"
)

func TestLoadTools_Valid(t *testing.T) {
	yaml := `
tools:
  - name: test-tool
    description: A test tool
    parameters:
      - name: vpnName
        type: string
        required: true
        description: "The VPN name"
    steps:
      - id: step1
        operation: monitor/getVpn
        args:
          vpnName: "{{.Params.vpnName}}"
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
	if tool.Name != "test-tool" {
		t.Errorf("expected name %q, got %q", "test-tool", tool.Name)
	}
	if tool.Description != "A test tool" {
		t.Errorf("expected description %q, got %q", "A test tool", tool.Description)
	}
	if len(tool.Parameters) != 1 {
		t.Fatalf("expected 1 parameter, got %d", len(tool.Parameters))
	}
	if tool.Parameters[0].Name != "vpnName" {
		t.Errorf("expected parameter name %q, got %q", "vpnName", tool.Parameters[0].Name)
	}
	if !tool.Parameters[0].Required {
		t.Error("expected parameter to be required")
	}
	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if tool.Steps[0].ID != "step1" {
		t.Errorf("expected step ID %q, got %q", "step1", tool.Steps[0].ID)
	}
	if tool.Steps[0].Operation != "monitor/getVpn" {
		t.Errorf("expected operation %q, got %q", "monitor/getVpn", tool.Steps[0].Operation)
	}
	if tool.Result.Strategy != "collect" {
		t.Errorf("expected strategy %q, got %q", "collect", tool.Result.Strategy)
	}
}

func TestLoadTools_MissingName(t *testing.T) {
	yaml := `
tools:
  - description: No name tool
    steps:
      - id: s1
        operation: monitor/getVpn
`
	fsys := fstest.MapFS{
		"tools.yaml": &fstest.MapFile{Data: []byte(yaml)},
	}

	_, err := LoadTools(fsys, "tools.yaml")
	if err == nil {
		t.Fatal("expected error for missing name, got nil")
	}
}

func TestLoadTools_MissingSteps(t *testing.T) {
	yaml := `
tools:
  - name: no-steps
    description: Tool without steps
    steps: []
`
	fsys := fstest.MapFS{
		"tools.yaml": &fstest.MapFile{Data: []byte(yaml)},
	}

	_, err := LoadTools(fsys, "tools.yaml")
	if err == nil {
		t.Fatal("expected error for missing steps, got nil")
	}
}

func TestLoadTools_DuplicateStepIDs(t *testing.T) {
	yaml := `
tools:
  - name: dup-steps
    description: Tool with duplicate step IDs
    steps:
      - id: same
        operation: monitor/getVpn
      - id: same
        operation: action/doSomething
`
	fsys := fstest.MapFS{
		"tools.yaml": &fstest.MapFile{Data: []byte(yaml)},
	}

	_, err := LoadTools(fsys, "tools.yaml")
	if err == nil {
		t.Fatal("expected error for duplicate step IDs, got nil")
	}
}

func TestLoadTools_EmptyOperation(t *testing.T) {
	yaml := `
tools:
  - name: empty-op
    description: Tool with empty operation
    steps:
      - id: s1
        operation: ""
`
	fsys := fstest.MapFS{
		"tools.yaml": &fstest.MapFile{Data: []byte(yaml)},
	}

	_, err := LoadTools(fsys, "tools.yaml")
	if err == nil {
		t.Fatal("expected error for empty operation, got nil")
	}
}

func TestLoadTools_UnsupportedStrategy(t *testing.T) {
	strategies := []string{"merge", "unwrap", "invalid"}
	for _, strategy := range strategies {
		t.Run(strategy, func(t *testing.T) {
			yaml := fmt.Sprintf(`
tools:
  - name: bad-strategy
    description: Tool with unsupported strategy
    steps:
      - id: s1
        operation: monitor/getVpn
    result:
      strategy: %s
`, strategy)
			fsys := fstest.MapFS{
				"tools.yaml": &fstest.MapFile{Data: []byte(yaml)},
			}

			_, err := LoadTools(fsys, "tools.yaml")
			if err == nil {
				t.Fatalf("expected error for strategy %q, got nil", strategy)
			}
		})
	}
}

func TestLoadTools_MissingStrategy(t *testing.T) {
	yaml := `
tools:
  - name: no-strategy
    description: Tool with no strategy specified
    steps:
      - id: s1
        operation: monitor/getVpn
`
	fsys := fstest.MapFS{
		"tools.yaml": &fstest.MapFile{Data: []byte(yaml)},
	}

	_, err := LoadTools(fsys, "tools.yaml")
	if err == nil {
		t.Fatal("expected error for missing strategy, got nil")
	}
}

func TestLoadTools_MissingDescription(t *testing.T) {
	yaml := `
tools:
  - name: no-desc
    steps:
      - id: s1
        operation: monitor/getVpn
`
	fsys := fstest.MapFS{
		"tools.yaml": &fstest.MapFile{Data: []byte(yaml)},
	}

	_, err := LoadTools(fsys, "tools.yaml")
	if err == nil {
		t.Fatal("expected error for missing description, got nil")
	}
}

func TestLoadTools_MissingStepID(t *testing.T) {
	yaml := `
tools:
  - name: no-step-id
    description: Tool with missing step ID
    steps:
      - operation: monitor/getVpn
`
	fsys := fstest.MapFS{
		"tools.yaml": &fstest.MapFile{Data: []byte(yaml)},
	}

	_, err := LoadTools(fsys, "tools.yaml")
	if err == nil {
		t.Fatal("expected error for missing step ID, got nil")
	}
}

func TestLoadTools_Annotations(t *testing.T) {
	yaml := `
tools:
  - name: monitor-tool
    description: A monitoring tool
    annotations:
      readOnly: true
    steps:
      - id: s1
        operation: monitor/getVpn
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

	if tools[0].Annotations.ReadOnly == nil || !*tools[0].Annotations.ReadOnly {
		t.Error("expected ReadOnly = true")
	}
	if tools[0].Annotations.Destructive != nil {
		t.Error("expected Destructive = nil (omitted)")
	}
}

func TestLoadTools_AnnotationsDefault(t *testing.T) {
	yaml := `
tools:
  - name: no-annotations
    description: A tool without annotations
    steps:
      - id: s1
        operation: monitor/getVpn
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

	// All annotation fields should default to nil (omitted).
	ann := tools[0].Annotations
	if ann.ReadOnly != nil || ann.Destructive != nil || ann.Idempotent != nil || ann.OpenWorld != nil {
		t.Errorf("expected all annotations to default to nil, got %+v", ann)
	}
}

func TestLoadTools_GetQueueMetrics(t *testing.T) {
	yaml := `
tools:
  - name: get-queue-metrics
    description: >
      Get detailed metrics for a specific queue including message depth,
      throughput rates, spool usage, and configuration.
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The Message VPN containing the queue"
      - name: queueName
        type: string
        required: true
        description: "The name of the queue"
    steps:
      - id: queueMetrics
        operation: monitor/getMsgVpnQueue
        args:
          msgVpnName: "{{.Params.msgVpnName}}"
          queueName: "{{.Params.queueName}}"
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
	if tool.Name != "get-queue-metrics" {
		t.Errorf("expected name %q, got %q", "get-queue-metrics", tool.Name)
	}
	if len(tool.Parameters) != 2 {
		t.Fatalf("expected 2 parameters, got %d", len(tool.Parameters))
	}
	if tool.Parameters[0].Name != "msgVpnName" || !tool.Parameters[0].Required {
		t.Errorf("expected msgVpnName required parameter, got %+v", tool.Parameters[0])
	}
	if tool.Parameters[1].Name != "queueName" || !tool.Parameters[1].Required {
		t.Errorf("expected queueName required parameter, got %+v", tool.Parameters[1])
	}
	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if tool.Steps[0].ID != "queueMetrics" {
		t.Errorf("expected step ID %q, got %q", "queueMetrics", tool.Steps[0].ID)
	}
	if tool.Steps[0].Operation != "monitor/getMsgVpnQueue" {
		t.Errorf("expected operation %q, got %q", "monitor/getMsgVpnQueue", tool.Steps[0].Operation)
	}
}

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

func TestLoadTools_ListQueues(t *testing.T) {
	yaml := `
tools:
  - name: list-queues
    description: >
      List all queues in a Message VPN.
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The Message VPN to list queues for"
      - name: maxResults
        type: integer
        required: false
        description: "Maximum number of queues to return (default 100, max 500)"
    steps:
      - id: queues
        operation: monitor/getMsgVpnQueues
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
	if tool.Name != "list-queues" {
		t.Errorf("expected name %q, got %q", "list-queues", tool.Name)
	}
	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if !tool.Steps[0].FollowPages {
		t.Error("expected FollowPages=true for list-queues step")
	}
	if tool.Steps[0].Operation != "monitor/getMsgVpnQueues" {
		t.Errorf("expected operation %q, got %q", "monitor/getMsgVpnQueues", tool.Steps[0].Operation)
	}
	if tool.Result.Strategy != "collect" {
		t.Errorf("expected strategy %q, got %q", "collect", tool.Result.Strategy)
	}

	// Verify optional maxResults parameter.
	maxResults := tool.Parameters[1]
	if maxResults.Name != "maxResults" {
		t.Errorf("expected parameter name %q, got %q", "maxResults", maxResults.Name)
	}
	if maxResults.Required {
		t.Error("expected maxResults to be optional (required=false)")
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

func TestLoadTools_UnknownFieldRejected(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{
			name: "unknown top-level tool field",
			yaml: `
tools:
  - name: test-tool
    description: A test tool
    bogusField: oops
    steps:
      - id: s1
        operation: monitor/getVpn
    result:
      strategy: collect
`,
		},
		{
			name: "unknown step field",
			yaml: `
tools:
  - name: test-tool
    description: A test tool
    steps:
      - id: s1
        operation: monitor/getVpn
        paralel: true
    result:
      strategy: collect
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsys := fstest.MapFS{
				"tools.yaml": &fstest.MapFile{Data: []byte(tc.yaml)},
			}
			_, err := LoadTools(fsys, "tools.yaml")
			if err == nil {
				t.Fatal("expected error for unknown field, got nil")
			}
		})
	}
}

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

func TestLoadTools_ListQueueDiscards(t *testing.T) {
	yaml := `
tools:
  - name: list-queue-discards
    description: >
      List per-queue message discard counts for a Message VPN.
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The Message VPN to summarize discards for"
      - name: maxResults
        type: integer
        required: false
        description: "Maximum number of queues to return (default 100, max 500)"
    steps:
      - id: queueDiscards
        operation: monitor/getMsgVpnQueues
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
	if tool.Name != "list-queue-discards" {
		t.Errorf("expected name %q, got %q", "list-queue-discards", tool.Name)
	}
	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if tool.Steps[0].ID != "queueDiscards" {
		t.Errorf("expected step ID %q, got %q", "queueDiscards", tool.Steps[0].ID)
	}
	if tool.Steps[0].Operation != "monitor/getMsgVpnQueues" {
		t.Errorf("expected operation %q, got %q", "monitor/getMsgVpnQueues", tool.Steps[0].Operation)
	}
	if !tool.Steps[0].FollowPages {
		t.Error("expected FollowPages=true for list-queue-discards step")
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

// TestLoadTools_StrategyConfig covers the cross-field rules between
// result.strategy and result.postProcess plus the reserved "summary" step ID
// and the args.select / select coexistence guard.
func TestLoadTools_StrategyConfig(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{
			name: "postProcess without name",
			yaml: `
tools:
  - name: bad
    description: missing handler name
    steps:
      - id: s1
        operation: monitor/getVpn
    result:
      strategy: postProcess
`,
			wantSub: `postProcess is required when strategy is "postProcess"`,
		},
		{
			name: "collect with postProcess set",
			yaml: `
tools:
  - name: bad
    description: postProcess on collect strategy
    steps:
      - id: s1
        operation: monitor/getVpn
    result:
      strategy: collect
      postProcess: someHandler
`,
			wantSub: `postProcess must be empty when strategy is "collect"`,
		},
		{
			name: "summary step ID reserved under postProcess",
			yaml: `
tools:
  - name: bad
    description: reserved step ID
    steps:
      - id: summary
        operation: monitor/getVpn
    result:
      strategy: postProcess
      postProcess: someHandler
`,
			wantSub: `step ID "summary" is reserved`,
		},
		{
			name: "both args.select and select set",
			yaml: `
tools:
  - name: bad
    description: select set twice
    steps:
      - id: s1
        operation: monitor/getVpn
        args:
          select: "a, b"
        select:
          - a
          - b
    result:
      strategy: collect
`,
			wantSub: "cannot set both args.select and select",
		},
		{
			// args.select under postProcess would silently bypass the
			// RequiredFields cross-check (it only reads structured select),
			// so the message must point at the real cause — not the field.
			name: "args.select under postProcess is rejected",
			yaml: `
tools:
  - name: bad
    description: args.select under postProcess
    steps:
      - id: s1
        operation: monitor/getVpn
        args:
          select: "a, b"
    result:
      strategy: postProcess
      postProcess: someHandler
`,
			wantSub: `args.select is not allowed when strategy is "postProcess"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsys := fstest.MapFS{"tools.yaml": &fstest.MapFile{Data: []byte(tc.yaml)}}
			_, err := LoadTools(fsys, "tools.yaml")
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err, tc.wantSub)
			}
		})
	}
}

// TestValidatePostProcess covers the boot-time cross-check between a
// postProcess tool's handler RequiredFields and the union of its steps'
// `select:` arrays. Builds CompositeTool values directly rather than going
// through YAML so the test isolates the validation logic.
func TestValidatePostProcess(t *testing.T) {
	postprocesstest.Register(t, "__test_pp_handler", postprocess.Handler{
		Fn:             func(map[string]map[string]any) (map[string]any, error) { return nil, nil },
		RequiredFields: []string{"a", "b"},
	})

	t.Run("collect tool is ignored", func(t *testing.T) {
		tools := []CompositeTool{{
			Name:   "collect-tool",
			Steps:  []Step{{ID: "s1", Operation: "x"}},
			Result: ResultStrategy{Strategy: "collect"},
		}}
		if err := ValidatePostProcess(tools); err != nil {
			t.Fatalf("collect tool should be skipped, got %v", err)
		}
	})

	t.Run("happy path with required fields covered", func(t *testing.T) {
		tools := []CompositeTool{{
			Name: "ok-tool",
			Steps: []Step{
				{ID: "s1", Operation: "x", Select: []string{"a"}},
				{ID: "s2", Operation: "y", Select: []string{"b", "c"}},
			},
			Result: ResultStrategy{Strategy: "postProcess", PostProcess: "__test_pp_handler"},
		}}
		if err := ValidatePostProcess(tools); err != nil {
			t.Fatalf("expected ok, got %v", err)
		}
	})

	t.Run("missing required field surfaces templated error", func(t *testing.T) {
		tools := []CompositeTool{{
			Name:   "bad-tool",
			Steps:  []Step{{ID: "s1", Operation: "x", Select: []string{"a"}}}, // b missing
			Result: ResultStrategy{Strategy: "postProcess", PostProcess: "__test_pp_handler"},
		}}
		err := ValidatePostProcess(tools)
		want := `tool "bad-tool": postprocessor "__test_pp_handler" reads "b" but it is not in select`
		if err == nil || err.Error() != want {
			t.Fatalf("\nwant: %s\ngot:  %v", want, err)
		}
	})

	t.Run("unregistered handler", func(t *testing.T) {
		tools := []CompositeTool{{
			Name:   "no-handler",
			Steps:  []Step{{ID: "s1", Operation: "x"}},
			Result: ResultStrategy{Strategy: "postProcess", PostProcess: "nope"},
		}}
		err := ValidatePostProcess(tools)
		if err == nil || !strings.Contains(err.Error(), `postprocessor "nope" not registered`) {
			t.Fatalf("got %v", err)
		}
	})
}

// TestValidatePostProcess_MultiStep covers the multi-step form: a handler
// declaring RequiredFieldsPerStep must be validated against each step's own
// `select:`, not the union across steps. This guards the property that added
// with the first multi-step handler (getRdpStatus) — the old union check
// would silently accept a config where step A reads field X but only step B
// selects it.
func TestValidatePostProcess_MultiStep(t *testing.T) {
	postprocesstest.Register(t, "__test_pp_multi", postprocess.Handler{
		Fn:            func(map[string]map[string]any) (map[string]any, error) { return nil, nil },
		RequiredSteps: []string{"a", "b"},
		RequiredFieldsPerStep: map[string][]string{
			"a": {"x"},
			"b": {"y"},
		},
	})

	t.Run("each step covers its own required fields", func(t *testing.T) {
		tools := []CompositeTool{{
			Name: "ok-multi",
			Steps: []Step{
				{ID: "a", Operation: "op-a", Select: []string{"x"}},
				{ID: "b", Operation: "op-b", Select: []string{"y"}},
			},
			Result: ResultStrategy{Strategy: "postProcess", PostProcess: "__test_pp_multi"},
		}}
		if err := ValidatePostProcess(tools); err != nil {
			t.Fatalf("expected ok, got %v", err)
		}
	})

	t.Run("field only in sibling select is rejected", func(t *testing.T) {
		// The union across steps includes both x and y, so the old
		// (pre-multi-step) check would have passed. The per-step check must
		// fail because step "a" reads x but step "a"'s select does not.
		tools := []CompositeTool{{
			Name: "bad-multi",
			Steps: []Step{
				{ID: "a", Operation: "op-a", Select: []string{"y"}},
				{ID: "b", Operation: "op-b", Select: []string{"x", "y"}},
			},
			Result: ResultStrategy{Strategy: "postProcess", PostProcess: "__test_pp_multi"},
		}}
		err := ValidatePostProcess(tools)
		want := `tool "bad-multi": postprocessor "__test_pp_multi" reads "x" on step "a" but it is not in that step's select`
		if err == nil || err.Error() != want {
			t.Fatalf("\nwant: %s\ngot:  %v", want, err)
		}
	})
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

func TestLoadTools_CreateQueue(t *testing.T) {
	yaml := `
tools:
  - name: create-queue
    description: Create a queue in a Message VPN.
    annotations:
      readOnly: false
      destructive: false
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The Message VPN to create the queue in"
      - name: queueName
        type: string
        required: true
        description: "The name of the queue to create"
      - name: queueConfig
        type: object
        required: false
        description: "Optional Queue attributes"
    steps:
      - id: createQueue
        operation: config/createMsgVpnQueue
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
	if tool.Name != "create-queue" {
		t.Errorf("expected name %q, got %q", "create-queue", tool.Name)
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
	if tool.Parameters[0].Name != "msgVpnName" || !tool.Parameters[0].Required {
		t.Errorf("expected required msgVpnName parameter, got %+v", tool.Parameters[0])
	}
	if tool.Parameters[1].Name != "queueName" || tool.Parameters[1].Type != "string" || !tool.Parameters[1].Required {
		t.Errorf("expected required string queueName parameter, got %+v", tool.Parameters[1])
	}
	if tool.Parameters[2].Name != "queueConfig" || tool.Parameters[2].Type != "object" || tool.Parameters[2].Required {
		t.Errorf("expected optional object queueConfig parameter, got %+v", tool.Parameters[2])
	}

	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if tool.Steps[0].ID != "createQueue" {
		t.Errorf("expected step ID %q, got %q", "createQueue", tool.Steps[0].ID)
	}
	if tool.Steps[0].Operation != "config/createMsgVpnQueue" {
		t.Errorf("expected operation %q, got %q", "config/createMsgVpnQueue", tool.Steps[0].Operation)
	}
	// Only msgVpnName is wired as a path arg; queueName is deliberately left out
	// of args so the executor spreads it into the create body.
	if tool.Steps[0].Args["msgVpnName"] != "{{.Params.msgVpnName}}" {
		t.Errorf("expected msgVpnName path arg, got %q", tool.Steps[0].Args["msgVpnName"])
	}
	if _, ok := tool.Steps[0].Args["queueName"]; ok {
		t.Error("queueName must not be a step arg on create — it belongs in the body")
	}
	if tool.Result.Strategy != "collect" {
		t.Errorf("expected strategy %q, got %q", "collect", tool.Result.Strategy)
	}
}

func TestLoadTools_UpdateQueue(t *testing.T) {
	yaml := `
tools:
  - name: update-queue
    description: Update an existing queue.
    annotations:
      readOnly: false
      destructive: true
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The Message VPN containing the queue"
      - name: queueName
        type: string
        required: true
        description: "The name of the queue to modify"
      - name: queueConfig
        type: object
        required: true
        description: "Queue attributes to update"
    steps:
      - id: updateQueue
        operation: config/updateMsgVpnQueue
        args:
          msgVpnName: "{{.Params.msgVpnName}}"
          queueName: "{{.Params.queueName}}"
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
	if tool.Name != "update-queue" {
		t.Errorf("expected name %q, got %q", "update-queue", tool.Name)
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
	// On update queueConfig is required — there is nothing to change without it.
	if tool.Parameters[2].Name != "queueConfig" || tool.Parameters[2].Type != "object" || !tool.Parameters[2].Required {
		t.Errorf("expected required object queueConfig parameter, got %+v", tool.Parameters[2])
	}

	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if tool.Steps[0].Operation != "config/updateMsgVpnQueue" {
		t.Errorf("expected operation %q, got %q", "config/updateMsgVpnQueue", tool.Steps[0].Operation)
	}
	// Both names are path params, so both are wired as step args.
	if tool.Steps[0].Args["msgVpnName"] != "{{.Params.msgVpnName}}" {
		t.Errorf("expected msgVpnName path arg, got %q", tool.Steps[0].Args["msgVpnName"])
	}
	if tool.Steps[0].Args["queueName"] != "{{.Params.queueName}}" {
		t.Errorf("expected queueName path arg, got %q", tool.Steps[0].Args["queueName"])
	}
	if tool.Result.Strategy != "collect" {
		t.Errorf("expected strategy %q, got %q", "collect", tool.Result.Strategy)
	}
}

func TestLoadTools_DeleteQueue(t *testing.T) {
	yaml := `
tools:
  - name: delete-queue
    description: Delete a queue from a Message VPN.
    annotations:
      readOnly: false
      destructive: true
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The Message VPN containing the queue"
      - name: queueName
        type: string
        required: true
        description: "The name of the queue to delete"
    steps:
      - id: deleteQueue
        operation: config/deleteMsgVpnQueue
        args:
          msgVpnName: "{{.Params.msgVpnName}}"
          queueName: "{{.Params.queueName}}"
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
	if tool.Name != "delete-queue" {
		t.Errorf("expected name %q, got %q", "delete-queue", tool.Name)
	}

	if tool.Annotations.ReadOnly == nil || *tool.Annotations.ReadOnly {
		t.Error("expected ReadOnly = false")
	}
	// Delete is destructive: it discards any messages still spooled on the queue.
	if tool.Annotations.Destructive == nil || !*tool.Annotations.Destructive {
		t.Error("expected Destructive = true")
	}

	if len(tool.Parameters) != 2 {
		t.Fatalf("expected 2 parameters, got %d", len(tool.Parameters))
	}

	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if tool.Steps[0].Operation != "config/deleteMsgVpnQueue" {
		t.Errorf("expected operation %q, got %q", "config/deleteMsgVpnQueue", tool.Steps[0].Operation)
	}
	if tool.Steps[0].Args["msgVpnName"] != "{{.Params.msgVpnName}}" {
		t.Errorf("expected msgVpnName path arg, got %q", tool.Steps[0].Args["msgVpnName"])
	}
	if tool.Steps[0].Args["queueName"] != "{{.Params.queueName}}" {
		t.Errorf("expected queueName path arg, got %q", tool.Steps[0].Args["queueName"])
	}
	if tool.Result.Strategy != "collect" {
		t.Errorf("expected strategy %q, got %q", "collect", tool.Result.Strategy)
	}
}

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
