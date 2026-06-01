package composite

import (
	"fmt"
	"testing"
	"testing/fstest"
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
        args:
          msgVpnName: "{{.Params.msgVpnName}}"
          clientName: "{{.Params.clientName}}"
          count: '{{with index .Params "maxResults"}}{{.}}{{else}}100{{end}}'
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

	// Verify the count template arg is present.
	if _, ok := tool.Steps[0].Args["count"]; !ok {
		t.Error("expected count arg in step, not found")
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
