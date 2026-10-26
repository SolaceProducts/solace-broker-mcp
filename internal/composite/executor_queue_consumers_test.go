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
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

// SOL-156294: list-queue-consumers runs the real tool definition against the
// real embedded spec, with a fake SEMP client. The spec and catalog loader is
// shared with executor_kafka_status_test.go.

func runListQueueConsumers(t *testing.T, client sempv2.Client, params map[string]any) (map[string]any, error) {
	t.Helper()
	fx := loadKafkaStatusFixture(t)
	tool, ok := fx.tools["list-queue-consumers"]
	if !ok {
		t.Fatal("list-queue-consumers not found in the embedded catalog")
	}
	return NewCompositeExecutor(fx.ops).Execute(context.Background(), tool, client, params)
}

func makeTxFlows(start, n int) []any {
	items := make([]any, n)
	for i := range items {
		items[i] = map[string]any{
			"clientName":      fmt.Sprintf("consumer-%d", start+i),
			"flowId":          float64(start + i),
			"unackedMsgCount": float64(255),
			"windowSize":      float64(255),
			"deliveryState":   "deliver-from-spool",
		}
	}
	return items
}

func queuePreflightOK() *sempv2.Result {
	return &sempv2.Result{StatusCode: 200, Data: map[string]any{"data": map[string]any{
		"queueName":                      "q1",
		"maxDeliveredUnackedMsgsPerFlow": float64(10000),
	}}}
}

func TestListQueueConsumers_ReturnsBoundFlows(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpnQueue", queuePreflightOK())
	client.addResponses("getMsgVpnQueueTxFlows", pageResult(makeTxFlows(0, 2), ""))

	result, err := runListQueueConsumers(t, client, map[string]any{"msgVpnName": "vpn1", "queueName": "q1"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(client.calls) != 2 || client.calls[0].opID != "getMsgVpnQueue" || client.calls[1].opID != "getMsgVpnQueueTxFlows" {
		t.Fatalf("calls = %v, want the queue GET then the tx-flows GET", client.calls)
	}
	preArgs := client.calls[0].args
	if preArgs["msgVpnName"] != "vpn1" || preArgs["queueName"] != "q1" || preArgs["select"] != strings.Join(queuePreflightFields, ", ") {
		t.Errorf("preflight args = %v", preArgs)
	}
	args := client.calls[1].args
	if args["msgVpnName"] != "vpn1" || args["queueName"] != "q1" || args["count"] != "100" {
		t.Errorf("tx-flows args = %v", args)
	}
	if args["select"] != strings.Join(queueConsumerFields, ", ") {
		t.Errorf("tx-flows select = %q", args["select"])
	}

	// The per-flow ACK limit comes back in the same response as the flows.
	queue, _ := result["queue"].(map[string]any)
	if q, _ := queue["data"].(map[string]any); q["maxDeliveredUnackedMsgsPerFlow"] != float64(10000) {
		t.Errorf("queue = %v, want maxDeliveredUnackedMsgsPerFlow 10000", result["queue"])
	}

	consumers, _ := result["consumers"].(map[string]any)
	items, _ := consumers["data"].([]any)
	if len(items) != 2 {
		t.Fatalf("consumers.data = %v, want 2 flows", consumers["data"])
	}
	if first, _ := items[0].(map[string]any); first["clientName"] != "consumer-0" {
		t.Errorf("first flow = %v", first)
	}
	if consumers["truncated"] == true {
		t.Error("2 flows under the default limit must not be truncated")
	}
}

// An existing queue with nothing bound is an empty list, not an error.
func TestListQueueConsumers_NoConsumersIsAnEmptyList(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpnQueue", queuePreflightOK())
	client.addResponses("getMsgVpnQueueTxFlows", pageResult([]any{}, ""))

	result, err := runListQueueConsumers(t, client, map[string]any{"msgVpnName": "vpn1", "queueName": "q1"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	consumers, _ := result["consumers"].(map[string]any)
	items, ok := consumers["data"].([]any)
	if !ok || len(items) != 0 {
		t.Errorf("consumers.data = %#v, want an empty list", consumers["data"])
	}
}

// A missing queue fails on the preflight, before the tx-flows GET (which
// would answer 200 with an empty list and read as "nobody is bound").
func TestListQueueConsumers_MissingQueueFailsBeforeTheFlowsStep(t *testing.T) {
	client := newSeqMockClient()
	client.errors["getMsgVpnQueue"] = &sempv2.SEMPError{Operation: "getMsgVpnQueue", StatusCode: 400, SEMPCode: 6, SEMPStatus: "NOT_FOUND"}

	_, err := runListQueueConsumers(t, client, map[string]any{"msgVpnName": "vpn1", "queueName": "missing"})
	if err == nil {
		t.Fatal("expected the missing queue to be an error")
	}
	var sempErr *sempv2.SEMPError
	if !errors.As(err, &sempErr) || sempErr.SEMPStatus != "NOT_FOUND" {
		t.Errorf("err = %v, want the broker's NOT_FOUND passed through", err)
	}
	if len(client.calls) != 1 || client.calls[0].opID != "getMsgVpnQueue" {
		t.Errorf("calls = %v, want only the queue GET (the tx-flows step must not run)", client.calls)
	}
}

// maxResults follows the shared list-tool rules in resolveMaxResults: at or
// below 0 means the default 100, above 500 is clamped to 500.
func TestListQueueConsumers_MaxResults(t *testing.T) {
	for _, tc := range []struct {
		name       string
		maxResults any
		wantItems  int
	}{
		{"one", float64(1), 1},
		{"default", nil, 100},
		{"zero falls back to the default", float64(0), 100},
		{"five hundred", float64(500), 500},
		{"501 is clamped to 500", float64(501), 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newSeqMockClient()
			client.addResponses("getMsgVpnQueue", queuePreflightOK())
			// Six full pages with a cursor each, so every limit is hit before the broker runs out.
			for p := range 6 {
				client.addResponses("getMsgVpnQueueTxFlows", pageResult(makeTxFlows(p*100, 100), fmt.Sprintf("c%d", p+1)))
			}
			params := map[string]any{"msgVpnName": "vpn1", "queueName": "q1"}
			if tc.maxResults != nil {
				params["maxResults"] = tc.maxResults
			}

			result, err := runListQueueConsumers(t, client, params)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			consumers, _ := result["consumers"].(map[string]any)
			items, _ := consumers["data"].([]any)
			if len(items) != tc.wantItems {
				t.Errorf("len(consumers.data) = %d, want %d", len(items), tc.wantItems)
			}
			if consumers["truncated"] != true {
				t.Errorf("truncated = %v, want true (more flows remain)", consumers["truncated"])
			}
		})
	}
}
