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

func TestLoadTools_CreateQueueSubscription(t *testing.T) {
	yaml := `
tools:
  - name: create-queue-subscription
    description: Add a topic subscription to a queue.
    annotations:
      readOnly: false
      destructive: false
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The Message VPN containing the queue"
      - name: queueName
        type: string
        required: true
        description: "The name of the queue to add the subscription to"
      - name: subscriptionTopic
        type: string
        required: true
        description: "The topic to subscribe the queue to"
    steps:
      - id: createQueueSubscription
        operation: config/createMsgVpnQueueSubscription
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
	if tool.Name != "create-queue-subscription" {
		t.Errorf("expected name %q, got %q", "create-queue-subscription", tool.Name)
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
	if tool.Parameters[1].Name != "queueName" || !tool.Parameters[1].Required {
		t.Errorf("expected required queueName parameter, got %+v", tool.Parameters[1])
	}
	if tool.Parameters[2].Name != "subscriptionTopic" || tool.Parameters[2].Type != "string" || !tool.Parameters[2].Required {
		t.Errorf("expected required string subscriptionTopic parameter, got %+v", tool.Parameters[2])
	}

	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if tool.Steps[0].ID != "createQueueSubscription" {
		t.Errorf("expected step ID %q, got %q", "createQueueSubscription", tool.Steps[0].ID)
	}
	if tool.Steps[0].Operation != "config/createMsgVpnQueueSubscription" {
		t.Errorf("expected operation %q, got %q", "config/createMsgVpnQueueSubscription", tool.Steps[0].Operation)
	}
	// msgVpnName and queueName are path params of POST .../queues/{queueName}/subscriptions.
	// subscriptionTopic is deliberately left out of args so the executor spreads
	// it into the create body, where SEMP expects it.
	if tool.Steps[0].Args["msgVpnName"] != "{{.Params.msgVpnName}}" {
		t.Errorf("expected msgVpnName path arg, got %q", tool.Steps[0].Args["msgVpnName"])
	}
	if tool.Steps[0].Args["queueName"] != "{{.Params.queueName}}" {
		t.Errorf("expected queueName path arg, got %q", tool.Steps[0].Args["queueName"])
	}
	if _, ok := tool.Steps[0].Args["subscriptionTopic"]; ok {
		t.Error("subscriptionTopic must not be a step arg on create — it belongs in the body")
	}
	if tool.Result.Strategy != "collect" {
		t.Errorf("expected strategy %q, got %q", "collect", tool.Result.Strategy)
	}
}

func TestLoadTools_DeleteQueueSubscription(t *testing.T) {
	yaml := `
tools:
  - name: delete-queue-subscription
    description: Remove a topic subscription from a queue.
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
        description: "The name of the queue to remove the subscription from"
      - name: subscriptionTopic
        type: string
        required: true
        description: "The topic of the subscription to remove"
    steps:
      - id: deleteQueueSubscription
        operation: config/deleteMsgVpnQueueSubscription
        args:
          msgVpnName: "{{.Params.msgVpnName}}"
          queueName: "{{.Params.queueName}}"
          subscriptionTopic: "{{.Params.subscriptionTopic}}"
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
	if tool.Name != "delete-queue-subscription" {
		t.Errorf("expected name %q, got %q", "delete-queue-subscription", tool.Name)
	}

	if tool.Annotations.ReadOnly == nil || *tool.Annotations.ReadOnly {
		t.Error("expected ReadOnly = false")
	}
	// Delete is destructive: removal is silent and stops matching traffic from
	// arriving at the queue with no further signal.
	if tool.Annotations.Destructive == nil || !*tool.Annotations.Destructive {
		t.Error("expected Destructive = true")
	}

	if len(tool.Parameters) != 3 {
		t.Fatalf("expected 3 parameters, got %d", len(tool.Parameters))
	}

	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if tool.Steps[0].Operation != "config/deleteMsgVpnQueueSubscription" {
		t.Errorf("expected operation %q, got %q", "config/deleteMsgVpnQueueSubscription", tool.Steps[0].Operation)
	}
	// Unlike create, subscriptionTopic IS a path param here — DELETE
	// .../queues/{queueName}/subscriptions/{subscriptionTopic} identifies the
	// subscription entirely by its position in the URL, with no body.
	if tool.Steps[0].Args["msgVpnName"] != "{{.Params.msgVpnName}}" {
		t.Errorf("expected msgVpnName path arg, got %q", tool.Steps[0].Args["msgVpnName"])
	}
	if tool.Steps[0].Args["queueName"] != "{{.Params.queueName}}" {
		t.Errorf("expected queueName path arg, got %q", tool.Steps[0].Args["queueName"])
	}
	if tool.Steps[0].Args["subscriptionTopic"] != "{{.Params.subscriptionTopic}}" {
		t.Errorf("expected subscriptionTopic path arg, got %q", tool.Steps[0].Args["subscriptionTopic"])
	}
	if tool.Result.Strategy != "collect" {
		t.Errorf("expected strategy %q, got %q", "collect", tool.Result.Strategy)
	}
}

func TestLoadTools_ListQueueSubscriptions(t *testing.T) {
	yaml := `
tools:
  - name: list-queue-subscriptions
    description: List topic subscriptions attached to a queue.
    annotations:
      readOnly: true
    parameters:
      - name: msgVpnName
        type: string
        required: true
        description: "The Message VPN containing the queue"
      - name: queueName
        type: string
        required: true
        description: "The name of the queue to list subscriptions for"
      - name: maxResults
        type: integer
        required: false
        description: "Maximum number of subscriptions to return (default 100, max 500)"
    steps:
      - id: queue
        operation: monitor/getMsgVpnQueue
        args:
          msgVpnName: "{{.Params.msgVpnName}}"
          queueName: "{{.Params.queueName}}"
        select:
          - queueName
      - id: subscriptions
        operation: monitor/getMsgVpnQueueSubscriptions
        followPages: true
        args:
          msgVpnName: "{{.Params.msgVpnName}}"
          queueName: "{{.Params.queueName}}"
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
	if tool.Name != "list-queue-subscriptions" {
		t.Errorf("expected name %q, got %q", "list-queue-subscriptions", tool.Name)
	}
	if tool.Annotations.ReadOnly == nil || !*tool.Annotations.ReadOnly {
		t.Error("expected ReadOnly = true")
	}

	// Two steps: a preflight existence check on the queue itself, then the
	// paginated subscription list. The preflight is what turns a nonexistent
	// queue into an error instead of an empty list (SEMP's collection GET
	// returns 200 with data: [] for a missing parent).
	if len(tool.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(tool.Steps))
	}

	preflight := tool.Steps[0]
	if preflight.ID != "queue" {
		t.Errorf("expected first step ID %q, got %q", "queue", preflight.ID)
	}
	if preflight.Operation != "monitor/getMsgVpnQueue" {
		t.Errorf("expected first step operation %q, got %q", "monitor/getMsgVpnQueue", preflight.Operation)
	}
	if preflight.FollowPages {
		t.Error("expected the preflight step to not paginate")
	}

	list := tool.Steps[1]
	if list.ID != "subscriptions" {
		t.Errorf("expected second step ID %q, got %q", "subscriptions", list.ID)
	}
	if list.Operation != "monitor/getMsgVpnQueueSubscriptions" {
		t.Errorf("expected second step operation %q, got %q", "monitor/getMsgVpnQueueSubscriptions", list.Operation)
	}
	if !list.FollowPages {
		t.Error("expected FollowPages=true for the subscriptions step")
	}

	if tool.Result.Strategy != "collect" {
		t.Errorf("expected strategy %q, got %q", "collect", tool.Result.Strategy)
	}

	// Verify optional maxResults parameter, same convention as every other
	// paginated list tool.
	maxResults := tool.Parameters[2]
	if maxResults.Name != "maxResults" {
		t.Errorf("expected parameter name %q, got %q", "maxResults", maxResults.Name)
	}
	if maxResults.Required {
		t.Error("expected maxResults to be optional (required=false)")
	}
}
