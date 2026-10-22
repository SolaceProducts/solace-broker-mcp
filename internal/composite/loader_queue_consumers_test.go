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
	"slices"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/definitions"
)

// queueConsumerFields is the select list of list-queue-consumers' consumers
// step: who is bound, and the window/ACK counters that tell a slow consumer
// apart from an idle one (SOL-156294).
var queueConsumerFields = []string{
	"clientName", "sessionName", "flowId", "bindTime",
	"activityState", "deliveryState",
	"unackedMsgCount", "windowSize", "usedWindowSize", "windowClosedCount",
	"maxUnackedMsgsExceededMsgCount", "redeliveredMsgCount", "ackedMsgCount",
	"selector",
}

// TestLoadTools_ListQueueConsumers checks the shipped definition, not a
// hand-built copy: preflight on the queue, then the paginated tx-flow list.
func TestLoadTools_ListQueueConsumers(t *testing.T) {
	tools, err := LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools(definitions.FS): %v", err)
	}
	tool := findTool(tools, "list-queue-consumers")
	if tool == nil {
		t.Fatal("list-queue-consumers not found in the embedded catalog")
	}

	if tool.Annotations.ReadOnly == nil || !*tool.Annotations.ReadOnly {
		t.Error("expected ReadOnly = true")
	}
	if !tool.StrictParameters {
		t.Error("expected strictParameters: true (paginated list tool)")
	}

	var names []string
	for _, p := range tool.Parameters {
		names = append(names, p.Name)
	}
	if !slices.Equal(names, []string{"msgVpnName", "queueName", "maxResults"}) {
		t.Fatalf("parameters = %v, want [msgVpnName queueName maxResults]", names)
	}
	if !tool.Parameters[0].Required || !tool.Parameters[1].Required || tool.Parameters[2].Required {
		t.Errorf("want msgVpnName and queueName required, maxResults optional; got %+v", tool.Parameters)
	}
	if tool.Parameters[2].Type != "integer" {
		t.Errorf("maxResults type = %q, want integer", tool.Parameters[2].Type)
	}

	if len(tool.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(tool.Steps))
	}

	// The preflight turns a missing queue into NOT_FOUND; the tx-flow
	// collection GET alone answers 200 with data: [] for it.
	preflight := tool.Steps[0]
	if preflight.ID != "queue" || preflight.Operation != "monitor/getMsgVpnQueue" {
		t.Errorf("first step = %s %s, want queue monitor/getMsgVpnQueue", preflight.ID, preflight.Operation)
	}
	if preflight.FollowPages || preflight.Parallel {
		t.Error("the preflight step must be sequential and unpaginated")
	}
	if !slices.Equal(preflight.Select, []string{"queueName"}) {
		t.Errorf("preflight select = %v, want [queueName]", preflight.Select)
	}

	list := tool.Steps[1]
	if list.ID != "consumers" || list.Operation != "monitor/getMsgVpnQueueTxFlows" {
		t.Errorf("second step = %s %s, want consumers monitor/getMsgVpnQueueTxFlows", list.ID, list.Operation)
	}
	if !list.FollowPages {
		t.Error("expected FollowPages=true for the consumers step")
	}
	if list.Args["count"] != "100" {
		t.Errorf("consumers count = %q, want 100", list.Args["count"])
	}
	if list.Args["msgVpnName"] != "{{.Params.msgVpnName}}" || list.Args["queueName"] != "{{.Params.queueName}}" {
		t.Errorf("consumers path args = %v", list.Args)
	}
	if !slices.Equal(list.Select, queueConsumerFields) {
		t.Errorf("consumers select = %v, want %v", list.Select, queueConsumerFields)
	}

	if tool.Result.Strategy != "collect" {
		t.Errorf("expected strategy %q, got %q", "collect", tool.Result.Strategy)
	}
}
