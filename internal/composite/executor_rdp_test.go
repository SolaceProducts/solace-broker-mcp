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
	"context"
	"fmt"
	"testing"
)

// listRDPsTool returns the list-rdps tool definition for tests.
func listRDPsTool() CompositeTool {
	return CompositeTool{
		Name:        "list-rdps",
		Description: "List all REST Delivery Points in a Message VPN",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "maxResults", Type: "integer", Required: false},
		},
		Steps: []Step{
			{
				ID:          "rdps",
				Operation:   "monitor/getMsgVpnRestDeliveryPoints",
				FollowPages: true,
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
					"count":      "100",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// makeRDPItems builds a slice of n mock RDP objects for use in paginated responses.
func makeRDPItems(n int) []any {
	items := make([]any, n)
	for i := range items {
		items[i] = map[string]any{"restDeliveryPointName": fmt.Sprintf("rdp-%d", i), "enabled": true}
	}
	return items
}

func TestExecute_ListRDPs_TruncatesAtMaxResults(t *testing.T) {
	// Page has 100 RDPs but maxResults=50, paginator should stop and set truncated.
	client := newSeqMockClient()
	client.addResponses("getMsgVpnRestDeliveryPoints", pageResult(makeRDPItems(100), "cursor-next"))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listRDPsTool(), client, map[string]any{
		"msgVpnName": "default",
		"maxResults": float64(50),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rdps := result["rdps"].(map[string]any)
	items := rdps["data"].([]any)

	if len(items) != 50 {
		t.Errorf("len(items) = %d, want 50", len(items))
	}
	if rdps["truncated"] != true {
		t.Errorf("truncated = %v, want true", rdps["truncated"])
	}
	if len(client.calls) != 1 {
		t.Errorf("expected 1 SEMP call, got %d", len(client.calls))
	}
}

