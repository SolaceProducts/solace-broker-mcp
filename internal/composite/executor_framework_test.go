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
	"sync"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

// threeStepTool returns the queue-replay-recovery tool definition for tests.
func threeStepTool() CompositeTool {
	return CompositeTool{
		Name:        "queue-replay-recovery",
		Description: "Replay recovery for a queue",
		Steps: []Step{
			{
				ID:        "queue",
				Operation: "monitor/getMsgVpnQueue",
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
					"queueName":  "{{.Params.queueName}}",
				},
			},
			{
				ID:        "cancelReplay",
				Operation: "action/doMsgVpnQueueCancelReplay",
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
					"queueName":  "{{.Params.queueName}}",
					"body":       "{}",
				},
			},
			{
				ID:        "startReplay",
				Operation: "action/doMsgVpnQueueStartReplay",
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
					"queueName":  "{{.Params.queueName}}",
					"body":       `{"replayLogName": "{{.Params.replayLogName}}"}`,
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

func TestExecute_SequentialSteps(t *testing.T) {
	client := newMockClient()
	executor := NewCompositeExecutor(testOperations())

	params := map[string]any{
		"msgVpnName":    "default",
		"queueName":     "Orders",
		"replayLogName": "replay-log-1",
	}

	_, err := executor.Execute(context.Background(), threeStepTool(), client, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{"getMsgVpnQueue", "doMsgVpnQueueCancelReplay", "doMsgVpnQueueStartReplay"}
	if len(client.calls) != len(want) {
		t.Fatalf("expected %d calls, got %d: %v", len(want), len(client.calls), client.calls)
	}
	for i, got := range client.calls {
		if got != want[i] {
			t.Errorf("call %d: expected %q, got %q", i, want[i], got)
		}
	}
}

func TestExecute_StepFailure_StopsExecution(t *testing.T) {
	client := newMockClient()
	client.errors["doMsgVpnQueueCancelReplay"] = fmt.Errorf("cancel failed: 500")

	executor := NewCompositeExecutor(testOperations())

	params := map[string]any{
		"msgVpnName":    "default",
		"queueName":     "Orders",
		"replayLogName": "replay-log-1",
	}

	_, err := executor.Execute(context.Background(), threeStepTool(), client, params)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// Step 3 should never have been called.
	for _, call := range client.calls {
		if call == "doMsgVpnQueueStartReplay" {
			t.Error("step 3 should not have been called after step 2 failure")
		}
	}
}

func TestExecute_TemplateResolution_Params(t *testing.T) {
	client := newMockClient()
	executor := NewCompositeExecutor(testOperations())

	tool := CompositeTool{
		Name:        "test",
		Description: "test",
		Steps: []Step{
			{
				ID:        "s1",
				Operation: "monitor/getMsgVpnQueue",
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
					"queueName":  "{{.Params.queueName}}",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}

	params := map[string]any{
		"msgVpnName": "prod-vpn",
		"queueName":  "Orders",
	}

	var recorded []callRecord
	var mu sync.Mutex

	argCapture := &argCapturingClient{
		inner:    client,
		recorded: &recorded,
		mu:       &mu,
	}

	_, err := executor.Execute(context.Background(), tool, argCapture, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(recorded) != 1 {
		t.Fatalf("expected 1 call, got %d", len(recorded))
	}

	if recorded[0].args["msgVpnName"] != "prod-vpn" {
		t.Errorf("expected msgVpnName %q, got %q", "prod-vpn", recorded[0].args["msgVpnName"])
	}
	if recorded[0].args["queueName"] != "Orders" {
		t.Errorf("expected queueName %q, got %q", "Orders", recorded[0].args["queueName"])
	}
}

func TestExecute_TemplateResolution_StepRefs(t *testing.T) {
	ops := map[string]*sempv2.Operation{
		"monitor/getVpn": {ID: "getVpn", Method: "GET", Path: "/vpn"},
		"monitor/getQ":   {ID: "getQ", Method: "GET", Path: "/q"},
	}

	client := newMockClient()
	client.responses["getVpn"] = &sempv2.Result{
		Data:       map[string]any{"vpnName": "resolved-vpn"},
		StatusCode: 200,
	}

	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: client, recorded: &recorded, mu: &mu}

	executor := NewCompositeExecutor(ops)

	tool := CompositeTool{
		Name:        "test",
		Description: "test",
		Steps: []Step{
			{ID: "vpn", Operation: "monitor/getVpn", Args: map[string]string{"name": "default"}},
			{ID: "q", Operation: "monitor/getQ", Args: map[string]string{"vpnName": "{{.StepResults.vpn.vpnName}}"}},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}

	_, err := executor.Execute(context.Background(), tool, capture, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(recorded))
	}

	if recorded[1].args["vpnName"] != "resolved-vpn" {
		t.Errorf("expected step ref to resolve to %q, got %q", "resolved-vpn", recorded[1].args["vpnName"])
	}
}

func TestExecute_TemplateResolution_InvalidSyntax(t *testing.T) {
	executor := NewCompositeExecutor(testOperations())

	tool := CompositeTool{
		Name:        "test",
		Description: "test",
		Steps: []Step{
			{ID: "s1", Operation: "monitor/getMsgVpnQueue", Args: map[string]string{
				"bad": "{{.Params.missing",
			}},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}

	_, err := executor.Execute(context.Background(), tool, newMockClient(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for invalid template syntax, got nil")
	}
}

func TestExecute_TemplateResolution_NilPath(t *testing.T) {
	executor := NewCompositeExecutor(testOperations())

	tool := CompositeTool{
		Name:        "test",
		Description: "test",
		Steps: []Step{
			{ID: "s1", Operation: "monitor/getMsgVpnQueue", Args: map[string]string{
				"val": "{{.StepResults.missing.data}}",
			}},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}

	_, err := executor.Execute(context.Background(), tool, newMockClient(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for nil path traversal, got nil")
	}
}

func TestExecute_OperationNotFound(t *testing.T) {
	executor := NewCompositeExecutor(testOperations())

	tool := CompositeTool{
		Name:        "test",
		Description: "test",
		Steps: []Step{
			{ID: "s1", Operation: "monitor/nonExistentOp", Args: map[string]string{}},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}

	_, err := executor.Execute(context.Background(), tool, newMockClient(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing operation, got nil")
	}

	// Error should mention both step ID and operation name.
	errMsg := err.Error()
	if !contains(errMsg, "s1") || !contains(errMsg, "monitor/nonExistentOp") {
		t.Errorf("error should mention step ID and operation name, got: %s", errMsg)
	}
}

func TestExecute_ParallelSteps(t *testing.T) {
	ops := map[string]*sempv2.Operation{
		"monitor/getA": {ID: "getA", Method: "GET", Path: "/a"},
		"monitor/getB": {ID: "getB", Method: "GET", Path: "/b"},
	}

	client := newMockClient()
	client.responses["getA"] = &sempv2.Result{Data: map[string]any{"a": "valueA"}, StatusCode: 200}
	client.responses["getB"] = &sempv2.Result{Data: map[string]any{"b": "valueB"}, StatusCode: 200}

	executor := NewCompositeExecutor(ops)

	tool := CompositeTool{
		Name:        "test",
		Description: "test",
		Steps: []Step{
			{ID: "stepA", Operation: "monitor/getA", Args: map[string]string{}, Parallel: true},
			{ID: "stepB", Operation: "monitor/getB", Args: map[string]string{}, Parallel: true},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}

	result, err := executor.Execute(context.Background(), tool, client, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Both results should be collected.
	if result["stepA"] == nil {
		t.Error("expected stepA result, got nil")
	}
	if result["stepB"] == nil {
		t.Error("expected stepB result, got nil")
	}
}

// TestExecute_ParallelSteps_AppliesSelect pins applySelect inside executeBatch.
// Regression test: an earlier revision of the parallel path skipped applySelect,
// so structured Select fields on parallel steps never reached the SEMP wire as
// args["select"] — silently dropping the field filter. Without this assertion
// the next refactor of executeBatch can regress the same way.
func TestExecute_ParallelSteps_AppliesSelect(t *testing.T) {
	ops := map[string]*sempv2.Operation{
		"monitor/getA": {ID: "getA", Method: "GET", Path: "/a"},
		"monitor/getB": {ID: "getB", Method: "GET", Path: "/b"},
	}

	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: newMockClient(), recorded: &recorded, mu: &mu}

	tool := CompositeTool{
		Name:        "test",
		Description: "test",
		Steps: []Step{
			{ID: "stepA", Operation: "monitor/getA", Args: map[string]string{}, Select: []string{"a1", "a2"}, Parallel: true},
			{ID: "stepB", Operation: "monitor/getB", Args: map[string]string{}, Select: []string{"b1"}, Parallel: true},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}

	_, err := NewCompositeExecutor(ops).Execute(context.Background(), tool, capture, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(recorded))
	}
	wantByOp := map[string]string{
		"getA": "a1, a2",
		"getB": "b1",
	}
	for _, call := range recorded {
		got, ok := call.args["select"].(string)
		if !ok {
			t.Errorf("op %q: args[\"select\"] missing or not string: %v", call.opID, call.args["select"])
			continue
		}
		if want := wantByOp[call.opID]; got != want {
			t.Errorf("op %q: args[\"select\"] = %q, want %q", call.opID, got, want)
		}
	}
}

// TestExecute_ParallelSteps_FollowsPages pins the invariant that a step with
// followPages: true actually follows pages even when it also runs in a parallel
// batch. Regression test: an earlier revision routed parallel steps through
// executeBatch which never inspected step.FollowPages, so multi-page results
// were silently truncated to page 1 and no truncated flag was set — an RDP
// with more than one page of bindings could ship a confident but wrong
// health summary.
func TestExecute_ParallelSteps_FollowsPages(t *testing.T) {
	ops := map[string]*sempv2.Operation{
		"monitor/getA": {ID: "getA", Method: "GET", Path: "/a"},
		"monitor/getB": {ID: "getB", Method: "GET", Path: "/b"},
	}

	client := newSeqMockClient()
	// Step A: two pages (100 + 30). Step B: one page (5).
	client.addResponses("getA",
		pageResult(makeSubscriptionItems(100), "cursor-a2"),
		pageResult(makeSubscriptionItems(30), ""),
	)
	client.addResponses("getB", pageResult(makeSubscriptionItems(5), ""))

	tool := CompositeTool{
		Name:        "test",
		Description: "test",
		Steps: []Step{
			{ID: "stepA", Operation: "monitor/getA", Args: map[string]string{}, Parallel: true, FollowPages: true},
			{ID: "stepB", Operation: "monitor/getB", Args: map[string]string{}, Parallel: true, FollowPages: true},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}

	result, err := NewCompositeExecutor(ops).Execute(context.Background(), tool, client, map[string]any{
		"maxResults": float64(200),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	stepA := result["stepA"].(map[string]any)
	if items := stepA["data"].([]any); len(items) != 130 {
		t.Errorf("stepA len(items) = %d, want 130 (paginator did not follow nextPageUri on parallel step)", len(items))
	}
	if stepA["truncated"] != false {
		t.Errorf("stepA truncated = %v, want false", stepA["truncated"])
	}

	stepB := result["stepB"].(map[string]any)
	if items := stepB["data"].([]any); len(items) != 5 {
		t.Errorf("stepB len(items) = %d, want 5", len(items))
	}
}

// TestExecute_ParallelSteps_FollowsPages_Truncates pins that the truncated
// flag fires on a parallel + followPages step when maxResults is reached.
// Regression: the earlier executeBatch path never produced a truncated key
// at all, so the caller could not distinguish complete from incomplete
// results.
func TestExecute_ParallelSteps_FollowsPages_Truncates(t *testing.T) {
	ops := map[string]*sempv2.Operation{
		"monitor/getA": {ID: "getA", Method: "GET", Path: "/a"},
		"monitor/getB": {ID: "getB", Method: "GET", Path: "/b"},
	}

	client := newSeqMockClient()
	client.addResponses("getA", pageResult(makeSubscriptionItems(100), "cursor-a2"))
	client.addResponses("getB", pageResult(makeSubscriptionItems(1), ""))

	tool := CompositeTool{
		Name:        "test",
		Description: "test",
		Steps: []Step{
			{ID: "stepA", Operation: "monitor/getA", Args: map[string]string{}, Parallel: true, FollowPages: true},
			{ID: "stepB", Operation: "monitor/getB", Args: map[string]string{}, Parallel: true, FollowPages: true},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}

	result, err := NewCompositeExecutor(ops).Execute(context.Background(), tool, client, map[string]any{
		"maxResults": float64(50),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	stepA := result["stepA"].(map[string]any)
	if stepA["truncated"] != true {
		t.Errorf("stepA truncated = %v, want true", stepA["truncated"])
	}
	if items := stepA["data"].([]any); len(items) != 50 {
		t.Errorf("stepA len(items) = %d, want 50", len(items))
	}
}

func TestExecute_ResultStrategy_Collect(t *testing.T) {
	client := newMockClient()
	client.responses["getMsgVpnQueue"] = &sempv2.Result{Data: map[string]any{"queue": "data"}, StatusCode: 200}
	client.responses["doMsgVpnQueueCancelReplay"] = &sempv2.Result{Data: map[string]any{"cancel": "done"}, StatusCode: 200}
	client.responses["doMsgVpnQueueStartReplay"] = &sempv2.Result{Data: map[string]any{"start": "done"}, StatusCode: 200}

	executor := NewCompositeExecutor(testOperations())

	params := map[string]any{
		"msgVpnName":    "default",
		"queueName":     "Orders",
		"replayLogName": "log-1",
	}

	result, err := executor.Execute(context.Background(), threeStepTool(), client, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result["queue"] == nil {
		t.Error("expected queue result")
	}
	if result["cancelReplay"] == nil {
		t.Error("expected cancelReplay result")
	}
	if result["startReplay"] == nil {
		t.Error("expected startReplay result")
	}
}

func TestExecute_ResultStrategy_UnsupportedStrategy(t *testing.T) {
	ops := map[string]*sempv2.Operation{
		"monitor/getVpn": {ID: "getVpn", Method: "GET", Path: "/vpn"},
	}

	executor := NewCompositeExecutor(ops)

	tool := CompositeTool{
		Name:        "test",
		Description: "test",
		Steps: []Step{
			{ID: "vpn", Operation: "monitor/getVpn", Args: map[string]string{}},
		},
		Result: ResultStrategy{Strategy: "merge"},
	}

	_, err := executor.Execute(context.Background(), tool, newMockClient(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for unsupported strategy, got nil")
	}
}

func TestExecute_BrokerParamStripped(t *testing.T) {
	ops := map[string]*sempv2.Operation{
		"monitor/getVpn": {ID: "getVpn", Method: "GET", Path: "/vpn"},
	}

	// Use a tool that tries to reference {{.Params.broker}} — it should
	// resolve to empty because broker is stripped.
	tool := CompositeTool{
		Name:        "test",
		Description: "test",
		Steps: []Step{
			{ID: "s1", Operation: "monitor/getVpn", Args: map[string]string{
				"name": "static",
			}},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}

	var recorded []callRecord
	var mu sync.Mutex
	client := newMockClient()
	capture := &argCapturingClient{inner: client, recorded: &recorded, mu: &mu}

	executor := NewCompositeExecutor(ops)

	params := map[string]any{
		"broker": "prod-us",
		"vpn":    "default",
	}

	_, err := executor.Execute(context.Background(), tool, capture, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify broker was not passed through to the step args.
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call, got %d", len(recorded))
	}
	if _, hasBroker := recorded[0].args["broker"]; hasBroker {
		t.Error("broker param should not be passed to step args")
	}
}
