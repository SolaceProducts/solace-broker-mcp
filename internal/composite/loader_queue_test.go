package composite

import (
	"testing"
	"testing/fstest"
)

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