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
	"strings"
	"sync"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/definitions"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2/specs"
)

// SOL-156211: get-kafka-receiver-status / get-kafka-sender-status name the failed
// binding and its reason. These run the real tool definitions against the real
// embedded spec, with a mock SEMP client.

type kafkaStatusFixture struct {
	ops   map[string]*sempv2.Operation
	tools map[string]CompositeTool
}

var (
	kafkaStatusOnce sync.Once
	kafkaStatusFx   kafkaStatusFixture
	kafkaStatusErr  error
)

func loadKafkaStatusFixture(t *testing.T) kafkaStatusFixture {
	t.Helper()
	kafkaStatusOnce.Do(func() {
		ops, err := sempv2.ParseSpecs(specs.FS)
		if err != nil {
			kafkaStatusErr = err
			return
		}
		loaded, err := LoadTools(definitions.FS, "tools.yaml")
		if err != nil {
			kafkaStatusErr = err
			return
		}
		fx := kafkaStatusFixture{ops: ops, tools: map[string]CompositeTool{}}
		for _, tl := range loaded {
			fx.tools[tl.Name] = tl
		}
		kafkaStatusFx = fx
	})
	if kafkaStatusErr != nil {
		t.Fatalf("loading the embedded spec and catalog: %v", kafkaStatusErr)
	}
	return kafkaStatusFx
}

func runKafkaStatus(t *testing.T, tool string, client sempv2.Client, params map[string]any) (map[string]any, error) {
	t.Helper()
	fx := loadKafkaStatusFixture(t)
	return NewCompositeExecutor(fx.ops).Execute(context.Background(), fx.tools[tool], client, params)
}

func TestGetKafkaSenderStatus_ReportsTheDownBindingTheBridgeHides(t *testing.T) {
	// The lab case: the sender reads up with no failureReason, but its only
	// binding is down because the Kafka topic does not exist.
	var recorded []callRecord
	var mu sync.Mutex
	mock := newMockClient()
	mock.responses["getMsgVpnKafkaSender"] = &sempv2.Result{StatusCode: 200, Data: map[string]any{"data": map[string]any{
		"kafkaSenderName": "tx", "up": true, "enabled": true, "failureReason": "",
		"queueBindingCount": float64(1), "queueBindingUpCount": float64(0),
	}}}
	mock.responses["getMsgVpnKafkaSenderQueueBindings"] = pageResult([]any{
		map[string]any{"queueName": "q-out", "remoteTopic": "out-topic", "enabled": true, "up": false,
			"failureReason": "Remote topic has no leader on remote-broker", "lastNotice": "Enabled"},
	}, "")
	client := &argCapturingClient{inner: mock, recorded: &recorded, mu: &mu}

	result, err := runKafkaStatus(t, "get-kafka-sender-status", client, map[string]any{"msgVpnName": "vpn1", "kafkaSenderName": "tx"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The bridge status is preflight: it runs first, then the bindings.
	if len(recorded) != 2 || recorded[0].opID != "getMsgVpnKafkaSender" || recorded[1].opID != "getMsgVpnKafkaSenderQueueBindings" {
		t.Fatalf("calls = %v, want the sender GET then the queue-bindings GET", recorded)
	}
	bArgs := recorded[1].args
	if bArgs["count"] != "100" || bArgs["kafkaSenderName"] != "tx" || bArgs["msgVpnName"] != "vpn1" {
		t.Errorf("bindings args = %v", bArgs)
	}
	if sel, _ := bArgs["select"].(string); !strings.Contains(sel, "failureReason") || !strings.Contains(sel, "queueName") {
		t.Errorf("bindings select = %q", sel)
	}

	summary, _ := result["summary"].(map[string]any)
	if summary["bindingDownCount"] != 1 {
		t.Fatalf("summary = %v", summary)
	}
	failed, _ := summary["failedBindings"].([]map[string]any)
	if len(failed) != 1 || failed[0]["name"] != "q-out" || failed[0]["failureReason"] != "Remote topic has no leader on remote-broker" {
		t.Errorf("failedBindings = %v", failed)
	}
	// The raw bridge status is still there and unchanged: existing consumers keep working.
	raw, _ := result["kafkaSenderStatus"].(map[string]any)
	if data, _ := raw["data"].(map[string]any); data["queueBindingUpCount"] != float64(0) || data["up"] != true {
		t.Errorf("raw kafkaSenderStatus = %v", raw)
	}
	if _, ok := result["queueBindings"]; !ok {
		t.Error("raw queueBindings step should still be in the result")
	}
}

func TestGetKafkaReceiverStatus_ReportsTheDownBinding(t *testing.T) {
	mock := newMockClient()
	mock.responses["getMsgVpnKafkaReceiver"] = &sempv2.Result{StatusCode: 200, Data: map[string]any{"data": map[string]any{
		"kafkaReceiverName": "rx", "up": true, "enabled": true, "topicBindingCount": float64(2), "topicBindingUpCount": float64(1),
	}}}
	mock.responses["getMsgVpnKafkaReceiverTopicBindings"] = pageResult([]any{
		map[string]any{"topicName": "orders", "enabled": true, "up": true, "failureReason": ""},
		map[string]any{"topicName": "payments", "enabled": true, "up": false, "failureReason": "Local failure: spool over quota", "rejectedMsgCount": float64(4)},
	}, "")
	result, err := runKafkaStatus(t, "get-kafka-receiver-status", mock, map[string]any{"msgVpnName": "vpn1", "kafkaReceiverName": "rx"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	summary, _ := result["summary"].(map[string]any)
	failed, _ := summary["failedBindings"].([]map[string]any)
	if len(failed) != 1 || failed[0]["name"] != "payments" || failed[0]["failureReason"] != "Local failure: spool over quota" {
		t.Errorf("failedBindings = %v", failed)
	}
	if summary["bindingUpCount"] != 1 || summary["bindingScannedCount"] != 2 {
		t.Errorf("summary = %v", summary)
	}
}

// A missing bridge must fail on the status step, before the bindings collection GET
// (which would answer 200 with an empty list and read as "nothing is down").
func TestGetKafkaStatusTools_MissingBridgeFailsBeforeTheBindingsStep(t *testing.T) {
	for _, tc := range []struct{ tool, statusOp, bindingsOp string }{
		{"get-kafka-receiver-status", "getMsgVpnKafkaReceiver", "getMsgVpnKafkaReceiverTopicBindings"},
		{"get-kafka-sender-status", "getMsgVpnKafkaSender", "getMsgVpnKafkaSenderQueueBindings"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			mock := newMockClient()
			mock.errors[tc.statusOp] = &sempv2.SEMPError{Operation: tc.statusOp, StatusCode: 400, SEMPCode: 6, SEMPStatus: "NOT_FOUND"}
			_, err := runKafkaStatus(t, tc.tool, mock, map[string]any{"msgVpnName": "vpn1", "kafkaReceiverName": "x", "kafkaSenderName": "x"})
			if err == nil {
				t.Fatal("expected the missing bridge to be an error")
			}
			if len(mock.calls) != 1 || mock.calls[0] != tc.statusOp {
				t.Errorf("calls = %v, want only the bridge GET (the bindings step must not run)", mock.calls)
			}
		})
	}
}
