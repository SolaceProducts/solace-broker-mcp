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
	"sync"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
)

// mockClient implements sempv2.Client for testing. It records which operations
// were called (in order) and returns preconfigured responses.
type mockClient struct {
	mu        sync.Mutex
	calls     []string
	responses map[string]*sempv2.Result
	errors    map[string]error
}

func newMockClient() *mockClient {
	return &mockClient{
		responses: make(map[string]*sempv2.Result),
		errors:    make(map[string]error),
	}
}

func (m *mockClient) Execute(_ context.Context, op *sempv2.Operation, _ map[string]any) (*sempv2.Result, error) {
	m.mu.Lock()
	m.calls = append(m.calls, op.ID)
	m.mu.Unlock()

	if err, ok := m.errors[op.ID]; ok {
		return nil, err
	}

	if resp, ok := m.responses[op.ID]; ok {
		return resp, nil
	}

	return &sempv2.Result{
		Data:       map[string]any{"status": "ok"},
		StatusCode: 200,
	}, nil
}

// testOperations returns a minimal operation catalog for tests.
func testOperations() map[string]*sempv2.Operation {
	return map[string]*sempv2.Operation{
		"monitor/getMsgVpnQueue": {
			ID:     "getMsgVpnQueue",
			Method: "GET",
			Path:   "/SEMP/v2/monitor/msgVpns/{msgVpnName}/queues/{queueName}",
		},
		"monitor/getMsgVpnClient": {
			ID:     "getMsgVpnClient",
			Method: "GET",
			Path:   "/SEMP/v2/monitor/msgVpns/{msgVpnName}/clients/{clientName}",
		},
		"monitor/getMsgVpnClientSubscriptions": {
			ID:     "getMsgVpnClientSubscriptions",
			Method: "GET",
			Path:   "/SEMP/v2/monitor/msgVpns/{msgVpnName}/clients/{clientName}/subscriptions",
		},
		"action/doMsgVpnQueueCancelReplay": {
			ID:     "doMsgVpnQueueCancelReplay",
			Method: "PUT",
			Path:   "/SEMP/v2/action/msgVpns/{msgVpnName}/queues/{queueName}/cancelReplay",
		},
		"action/doMsgVpnQueueStartReplay": {
			ID:     "doMsgVpnQueueStartReplay",
			Method: "PUT",
			Path:   "/SEMP/v2/action/msgVpns/{msgVpnName}/queues/{queueName}/startReplay",
		},
		"monitor/getMsgVpn": {
			ID:     "getMsgVpn",
			Method: "GET",
			Path:   "/SEMP/v2/__private_monitor__/msgVpns/{msgVpnName}",
		},
		"monitor/getMsgVpns": {
			ID:     "getMsgVpns",
			Method: "GET",
			Path:   "/SEMP/v2/__private_monitor__/msgVpns",
		},
		"monitor/getMsgVpnQueues": {
			ID:     "getMsgVpnQueues",
			Method: "GET",
			Path:   "/SEMP/v2/__private_monitor__/msgVpns/{msgVpnName}/queues",
		},
		"monitor/getMsgVpnClients": {
			ID:     "getMsgVpnClients",
			Method: "GET",
			Path:   "/SEMP/v2/__private_monitor__/msgVpns/{msgVpnName}/clients",
		},
		"monitor/getMsgVpnRestDeliveryPoints": {
			ID:     "getMsgVpnRestDeliveryPoints",
			Method: "GET",
			Path:   "/SEMP/v2/__private_monitor__/msgVpns/{msgVpnName}/restDeliveryPoints",
		},
		"config/createMsgVpn": {
			ID:     "createMsgVpn",
			Method: "POST",
			Path:   "/SEMP/v2/__private_config__/msgVpns",
			Parameters: []sempv2.Parameter{
				{Name: "body", In: "body", Type: "object", Required: true},
			},
		},
		"config/updateMsgVpn": {
			ID:     "updateMsgVpn",
			Method: "PATCH",
			Path:   "/SEMP/v2/__private_config__/msgVpns/{msgVpnName}",
			Parameters: []sempv2.Parameter{
				{Name: "msgVpnName", In: "path", Type: "string", Required: true},
				{Name: "body", In: "body", Type: "object", Required: true},
			},
		},
		"config/deleteMsgVpn": {
			ID:     "deleteMsgVpn",
			Method: "DELETE",
			Path:   "/SEMP/v2/__private_config__/msgVpns/{msgVpnName}",
		},
	}
}

// seqMockClient returns pre-configured responses for operations in sequence.
// Each Execute call for an operation advances to the next response in its sequence.
// When the sequence is exhausted the last response is repeated.
type seqMockClient struct {
	mu     sync.Mutex
	calls  []callRecord
	seqs   map[string][]*sempv2.Result
	errors map[string]error
	idx    map[string]int
}

func newSeqMockClient() *seqMockClient {
	return &seqMockClient{
		seqs:   make(map[string][]*sempv2.Result),
		errors: make(map[string]error),
		idx:    make(map[string]int),
	}
}

func (m *seqMockClient) addResponses(opID string, results ...*sempv2.Result) {
	m.seqs[opID] = append(m.seqs[opID], results...)
}

func (m *seqMockClient) Execute(_ context.Context, op *sempv2.Operation, args map[string]any) (*sempv2.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls = append(m.calls, callRecord{opID: op.ID, args: args})

	if err, ok := m.errors[op.ID]; ok {
		return nil, err
	}

	seq, ok := m.seqs[op.ID]
	if !ok || len(seq) == 0 {
		return &sempv2.Result{Data: map[string]any{}, StatusCode: 200}, nil
	}

	idx := m.idx[op.ID]
	if idx >= len(seq) {
		idx = len(seq) - 1
	}
	m.idx[op.ID] = idx + 1
	return seq[idx], nil
}

// getQueueMetricsTool returns the get-queue-metrics tool definition for tests.
func getQueueMetricsTool() CompositeTool {
	return CompositeTool{
		Name:        "get-queue-metrics",
		Description: "Get detailed metrics for a specific queue",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "queueName", Type: "string", Required: true},
		},
		Steps: []Step{
			{
				ID:        "queueMetrics",
				Operation: "monitor/getMsgVpnQueue",
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
					"queueName":  "{{.Params.queueName}}",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// getClientDetailsTool returns the get-client-details tool definition for tests.
func getClientDetailsTool() CompositeTool {
	return CompositeTool{
		Name:        "get-client-details",
		Description: "Get performance metrics for a specific connected client",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "clientName", Type: "string", Required: true},
		},
		Steps: []Step{
			{
				ID:        "clientDetails",
				Operation: "monitor/getMsgVpnClient",
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
					"clientName": "{{.Params.clientName}}",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// listClientSubscriptionsTool returns the list-client-subscriptions tool definition for tests.
func listClientSubscriptionsTool() CompositeTool {
	return CompositeTool{
		Name:        "list-client-subscriptions",
		Description: "List topic subscriptions for a specific client",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "clientName", Type: "string", Required: true},
			{Name: "maxResults", Type: "integer", Required: false},
		},
		Steps: []Step{
			{
				ID:          "subscriptions",
				Operation:   "monitor/getMsgVpnClientSubscriptions",
				FollowPages: true,
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
					"clientName": "{{.Params.clientName}}",
					"count":      "100",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

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

// argCapturingClient wraps a client and records the args passed to each Execute call.
type argCapturingClient struct {
	inner    sempv2.Client
	recorded *[]callRecord
	mu       *sync.Mutex
}

type callRecord struct {
	opID string
	args map[string]any
}

func (c *argCapturingClient) Execute(ctx context.Context, op *sempv2.Operation, args map[string]any) (*sempv2.Result, error) {
	c.mu.Lock()
	*c.recorded = append(*c.recorded, callRecord{opID: op.ID, args: args})
	c.mu.Unlock()
	return c.inner.Execute(ctx, op, args)
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

func TestExecute_GetQueueMetrics_ReturnsData(t *testing.T) {
	client := newMockClient()
	client.responses["getMsgVpnQueue"] = &sempv2.Result{
		Data: map[string]any{
			"queueName":       "Orders",
			"spooledMsgCount": float64(1234),
			"rxMsgRate":       float64(50),
			"txMsgRate":       float64(48),
		},
		StatusCode: 200,
	}

	executor := NewCompositeExecutor(testOperations())

	params := map[string]any{
		"msgVpnName": "default",
		"queueName":  "Orders",
	}

	result, err := executor.Execute(context.Background(), getQueueMetricsTool(), client, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result["queueMetrics"] == nil {
		t.Fatal("expected queueMetrics key in result, got nil")
	}

	data, ok := result["queueMetrics"].(map[string]any)
	if !ok {
		t.Fatal("expected queueMetrics to be a map")
	}

	if data["queueName"] != "Orders" {
		t.Errorf("expected queueName %q, got %v", "Orders", data["queueName"])
	}
}

func TestExecute_GetQueueMetrics_NotFound(t *testing.T) {
	client := newMockClient()
	client.errors["getMsgVpnQueue"] = &sempv2.SEMPError{
		Operation:  "getMsgVpnQueue",
		StatusCode: 400,
		Body:       `{"meta":{"error":{"code":400,"description":"Queue not found","status":"NOT_FOUND"}}}`,
	}

	executor := NewCompositeExecutor(testOperations())

	params := map[string]any{
		"msgVpnName": "default",
		"queueName":  "nonexistent-queue",
	}

	_, err := executor.Execute(context.Background(), getQueueMetricsTool(), client, params)
	if err == nil {
		t.Fatal("expected error for 404 response, got nil")
	}

	var sempErr *sempv2.SEMPError
	if !errors.As(err, &sempErr) {
		t.Errorf("expected SEMPError in error chain, got: %v", err)
	} else if sempErr.StatusCode != 400 {
		t.Errorf("expected status code 400, got %d", sempErr.StatusCode)
	}
}

func TestExecute_GetClientDetails_ReturnsData(t *testing.T) {
	client := newMockClient()
	client.responses["getMsgVpnClient"] = &sempv2.Result{
		Data: map[string]any{
			"clientName":          "myapp/1",
			"clientUsername":      "app-user",
			"slowSubscriber":      false,
			"txDiscardedMsgCount": float64(0),
		},
		StatusCode: 200,
	}

	executor := NewCompositeExecutor(testOperations())

	params := map[string]any{
		"msgVpnName": "default",
		"clientName": "myapp/1",
	}

	result, err := executor.Execute(context.Background(), getClientDetailsTool(), client, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result["clientDetails"] == nil {
		t.Fatal("expected clientDetails key in result, got nil")
	}

	data, ok := result["clientDetails"].(map[string]any)
	if !ok {
		t.Fatal("expected clientDetails to be a map")
	}

	if data["clientName"] != "myapp/1" {
		t.Errorf("expected clientName %q, got %v", "myapp/1", data["clientName"])
	}
}

func TestExecute_GetClientDetails_NotFound(t *testing.T) {
	client := newMockClient()
	client.errors["getMsgVpnClient"] = &sempv2.SEMPError{
		Operation:  "getMsgVpnClient",
		StatusCode: 400,
		Body:       `{"meta":{"error":{"code":400,"description":"Client not found","status":"NOT_FOUND"}}}`,
	}

	executor := NewCompositeExecutor(testOperations())

	params := map[string]any{
		"msgVpnName": "default",
		"clientName": "nonexistent-client",
	}

	_, err := executor.Execute(context.Background(), getClientDetailsTool(), client, params)
	if err == nil {
		t.Fatal("expected error for 404 response, got nil")
	}

	var sempErr *sempv2.SEMPError
	if !errors.As(err, &sempErr) {
		t.Errorf("expected SEMPError in error chain, got: %v", err)
	} else if sempErr.StatusCode != 400 {
		t.Errorf("expected status code 400, got %d", sempErr.StatusCode)
	}
}

// makeSubscriptionItems builds a slice of n mock subscription objects for use in paginated responses.
func makeSubscriptionItems(n int) []any {
	items := make([]any, n)
	for i := range items {
		items[i] = map[string]any{
			"subscriptionTopic": fmt.Sprintf("topic/%d/>", i),
			"clientName":        "myapp/1",
		}
	}
	return items
}

func TestExecute_ListClientSubscriptions_ReturnsData(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClientSubscriptions", pageResult(makeSubscriptionItems(2), ""))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listClientSubscriptionsTool(), client, map[string]any{
		"msgVpnName": "default",
		"clientName": "myapp/1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	subs := result["subscriptions"].(map[string]any)
	items := subs["data"].([]any)

	if len(items) != 2 {
		t.Errorf("len(items) = %d, want 2", len(items))
	}
	if subs["truncated"] != false {
		t.Errorf("truncated = %v, want false", subs["truncated"])
	}
}

func TestExecute_ListClientSubscriptions_DefaultMaxResults(t *testing.T) {
	// 80 subscriptions on a single page, fits within the default 100 limit.
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClientSubscriptions", pageResult(makeSubscriptionItems(80), ""))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listClientSubscriptionsTool(), client, map[string]any{
		"msgVpnName": "default",
		"clientName": "myapp/1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	subs := result["subscriptions"].(map[string]any)
	items := subs["data"].([]any)

	if len(items) != 80 {
		t.Errorf("len(items) = %d, want 80", len(items))
	}
	if subs["truncated"] != false {
		t.Errorf("truncated = %v, want false", subs["truncated"])
	}
}

func TestExecute_ListClientSubscriptions_MultiPage(t *testing.T) {
	// Page 1: 100 subscriptions + cursor; page 2: 30 subscriptions, no cursor. Total: 130.
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClientSubscriptions",
		pageResult(makeSubscriptionItems(100), "cursor-s2"),
		pageResult(makeSubscriptionItems(30), ""),
	)

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listClientSubscriptionsTool(), client, map[string]any{
		"msgVpnName": "default",
		"clientName": "myapp/1",
		"maxResults": float64(200),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	subs := result["subscriptions"].(map[string]any)
	items := subs["data"].([]any)

	if len(items) != 130 {
		t.Errorf("len(items) = %d, want 130", len(items))
	}
	if subs["truncated"] != false {
		t.Errorf("truncated = %v, want false", subs["truncated"])
	}
	if len(client.calls) != 2 {
		t.Errorf("expected 2 SEMP calls, got %d", len(client.calls))
	}
}

func TestExecute_ListClientSubscriptions_TruncatesAtMaxResults(t *testing.T) {
	// Page has 100 items but maxResults=50 — paginator should stop and set truncated.
	// Regression test: the previous tool definition silently capped at one SEMP page
	// without signalling truncation, hiding subscriptions from the agent.
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClientSubscriptions", pageResult(makeSubscriptionItems(100), "cursor-next"))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listClientSubscriptionsTool(), client, map[string]any{
		"msgVpnName": "default",
		"clientName": "myapp/1",
		"maxResults": float64(50),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	subs := result["subscriptions"].(map[string]any)
	items := subs["data"].([]any)

	if len(items) != 50 {
		t.Errorf("len(items) = %d, want 50", len(items))
	}
	if subs["truncated"] != true {
		t.Errorf("truncated = %v, want true", subs["truncated"])
	}
	wantMsg := "Results limited to 50. Use maxResults (up to 500) to retrieve more."
	if subs["truncatedMessage"] != wantMsg {
		t.Errorf("truncatedMessage = %v, want %q", subs["truncatedMessage"], wantMsg)
	}
}

func TestExecute_ListClientSubscriptions_FixedCountOnWire(t *testing.T) {
	// Regression test: count must always be "100" on the SEMP wire regardless of
	// maxResults. The earlier tool definition piped maxResults straight into count,
	// which exceeds SEMPv2's per-page cap of 100 and produced 400 errors at higher
	// values.
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClientSubscriptions", pageResult(makeSubscriptionItems(10), ""))

	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: client, recorded: &recorded, mu: &mu}

	executor := NewCompositeExecutor(testOperations())
	_, err := executor.Execute(context.Background(), listClientSubscriptionsTool(), capture, map[string]any{
		"msgVpnName": "default",
		"clientName": "myapp/1",
		"maxResults": float64(500),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(recorded) < 1 {
		t.Fatalf("expected at least 1 call, got %d", len(recorded))
	}
	if recorded[0].args["count"] != "100" {
		t.Errorf("expected count %q on the wire, got %v", "100", recorded[0].args["count"])
	}
}

// getVPNHealthTool returns the get-vpn-health tool definition for tests.
func getVPNHealthTool() CompositeTool {
	return CompositeTool{
		Name:        "get-vpn-health",
		Description: "Get health and connection statistics for a Message VPN",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
		},
		Steps: []Step{
			{
				ID:        "vpnHealth",
				Operation: "monitor/getMsgVpn",
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// listVPNsTool returns the list-vpns tool definition for tests.
func listVPNsTool() CompositeTool {
	return CompositeTool{
		Name:        "list-vpns",
		Description: "List all Message VPNs on the broker",
		Parameters: []ParameterDef{
			{Name: "maxResults", Type: "integer", Required: false},
		},
		Steps: []Step{
			{
				ID:          "vpns",
				Operation:   "monitor/getMsgVpns",
				FollowPages: true,
				Args: map[string]string{
					"count": "100",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// listQueuesTool returns the list-queues tool definition for tests.
func listQueuesTool() CompositeTool {
	return CompositeTool{
		Name:        "list-queues",
		Description: "List all queues in a Message VPN",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "maxResults", Type: "integer", Required: false},
		},
		Steps: []Step{
			{
				ID:          "queues",
				Operation:   "monitor/getMsgVpnQueues",
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

// listClientsTool returns the list-clients tool definition for tests.
func listClientsTool() CompositeTool {
	return CompositeTool{
		Name:        "list-clients",
		Description: "List all active client connections in a Message VPN",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "maxResults", Type: "integer", Required: false},
		},
		Steps: []Step{
			{
				ID:          "clients",
				Operation:   "monitor/getMsgVpnClients",
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

// makeVPNItems builds a slice of n mock VPN objects for use in paginated responses.
func makeVPNItems(n int) []any {
	items := make([]any, n)
	for i := range items {
		items[i] = map[string]any{"msgVpnName": fmt.Sprintf("vpn-%d", i), "enabled": true}
	}
	return items
}

// makeQueueItems builds a slice of n mock queue objects for use in paginated responses.
func makeQueueItems(n int) []any {
	items := make([]any, n)
	for i := range items {
		items[i] = map[string]any{"queueName": fmt.Sprintf("queue-%d", i), "bindCount": float64(0)}
	}
	return items
}

// makeClientItems builds a slice of n mock client objects for use in paginated responses.
func makeClientItems(n int) []any {
	items := make([]any, n)
	for i := range items {
		items[i] = map[string]any{"clientName": fmt.Sprintf("client-%d", i), "slowSubscriber": false}
	}
	return items
}

// makeRDPItems builds a slice of n mock RDP objects for use in paginated responses.
func makeRDPItems(n int) []any {
	items := make([]any, n)
	for i := range items {
		items[i] = map[string]any{"restDeliveryPointName": fmt.Sprintf("rdp-%d", i), "enabled": true}
	}
	return items
}

// pageResult builds a SEMP list response with optional pagination cursor.
func pageResult(items []any, nextCursor string) *sempv2.Result {
	data := map[string]any{"data": items}
	if nextCursor != "" {
		data["meta"] = map[string]any{
			"paging": map[string]any{
				"nextPageUri": "/SEMP/v2/__private_monitor__/resource?cursor=" + nextCursor + "&count=100",
			},
		}
	}
	return &sempv2.Result{Data: data, StatusCode: 200}
}

func TestExecute_GetVPNHealth_ReturnsData(t *testing.T) {
	client := newMockClient()
	client.responses["getMsgVpn"] = &sempv2.Result{
		Data: map[string]any{
			"msgVpnName":                     "default",
			"enabled":                        true,
			"msgVpnConnections":              float64(5),
			"msgVpnTotalUniqueSubscriptions": float64(42),
			"state":                          "up",
		},
		StatusCode: 200,
	}

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), getVPNHealthTool(), client, map[string]any{
		"msgVpnName": "default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	vpnData, ok := result["vpnHealth"].(map[string]any)
	if !ok {
		t.Fatal("expected vpnHealth key containing a map")
	}
	if vpnData["msgVpnName"] != "default" {
		t.Errorf("msgVpnName = %v, want default", vpnData["msgVpnName"])
	}
	if vpnData["enabled"] != true {
		t.Errorf("enabled = %v, want true", vpnData["enabled"])
	}
}

func TestExecute_ListVPNs_SinglePage(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpns", pageResult(makeVPNItems(3), ""))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listVPNsTool(), client, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	vpns, ok := result["vpns"].(map[string]any)
	if !ok {
		t.Fatal("expected vpns key containing a map")
	}

	items, ok := vpns["data"].([]any)
	if !ok {
		t.Fatal("expected vpns.data to be a slice")
	}
	if len(items) != 3 {
		t.Errorf("len(items) = %d, want 3", len(items))
	}
	if vpns["truncated"] != false {
		t.Errorf("truncated = %v, want false", vpns["truncated"])
	}
	if len(client.calls) != 1 {
		t.Errorf("expected 1 SEMP call, got %d", len(client.calls))
	}
}

func TestExecute_ListVPNs_MultiPage(t *testing.T) {
	// Page 1: 10 VPNs with nextPageUri; page 2: 5 VPNs, no nextPageUri. Total: 15.
	client := newSeqMockClient()
	client.addResponses("getMsgVpns",
		pageResult(makeVPNItems(10), "cursor-page2"),
		pageResult(makeVPNItems(5), ""),
	)

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listVPNsTool(), client, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	vpns := result["vpns"].(map[string]any)
	items := vpns["data"].([]any)

	if len(items) != 15 {
		t.Errorf("len(items) = %d, want 15", len(items))
	}
	if vpns["truncated"] != false {
		t.Errorf("truncated = %v, want false", vpns["truncated"])
	}
	if len(client.calls) != 2 {
		t.Errorf("expected 2 SEMP calls, got %d", len(client.calls))
	}
	// Second call must include the cursor from the first page's nextPageUri.
	if client.calls[1].args["cursor"] != "cursor-page2" {
		t.Errorf("second call cursor = %v, want cursor-page2", client.calls[1].args["cursor"])
	}
}

func TestExecute_ListQueues_MultiPage(t *testing.T) {
	// Page 1: 100 queues + cursor; page 2: 50 queues, no cursor. Total: 150.
	client := newSeqMockClient()
	client.addResponses("getMsgVpnQueues",
		pageResult(makeQueueItems(100), "cursor-q2"),
		pageResult(makeQueueItems(50), ""),
	)

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listQueuesTool(), client, map[string]any{
		"msgVpnName": "default",
		"maxResults": float64(200), // larger than 150 total so paginator follows all pages
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	queues := result["queues"].(map[string]any)
	items := queues["data"].([]any)

	if len(items) != 150 {
		t.Errorf("len(items) = %d, want 150", len(items))
	}
	if queues["truncated"] != false {
		t.Errorf("truncated = %v, want false", queues["truncated"])
	}
	if len(client.calls) != 2 {
		t.Errorf("expected 2 SEMP calls, got %d", len(client.calls))
	}
}

func TestExecute_ListQueues_TruncatesAtMaxResults(t *testing.T) {
	// Page has 100 items but maxResults=50, paginator should stop and set truncated.
	client := newSeqMockClient()
	client.addResponses("getMsgVpnQueues", pageResult(makeQueueItems(100), "cursor-next"))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listQueuesTool(), client, map[string]any{
		"msgVpnName": "default",
		"maxResults": float64(50),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	queues := result["queues"].(map[string]any)
	items := queues["data"].([]any)

	if len(items) != 50 {
		t.Errorf("len(items) = %d, want 50", len(items))
	}
	if queues["truncated"] != true {
		t.Errorf("truncated = %v, want true", queues["truncated"])
	}
	wantMsg := "Results limited to 50. Use maxResults (up to 500) to retrieve more."
	if queues["truncatedMessage"] != wantMsg {
		t.Errorf("truncatedMessage = %v, want %q", queues["truncatedMessage"], wantMsg)
	}
	// Paginator stopped after the first page, no second call.
	if len(client.calls) != 1 {
		t.Errorf("expected 1 SEMP call, got %d", len(client.calls))
	}
}

func TestExecute_ListClients_DefaultMaxResults(t *testing.T) {
	// 80 clients on a single page, fits within the default 100 limit.
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClients", pageResult(makeClientItems(80), ""))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listClientsTool(), client, map[string]any{
		"msgVpnName": "default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	clients := result["clients"].(map[string]any)
	items := clients["data"].([]any)

	if len(items) != 80 {
		t.Errorf("len(items) = %d, want 80", len(items))
	}
	if clients["truncated"] != false {
		t.Errorf("truncated = %v, want false", clients["truncated"])
	}
}

func TestExecute_ListClients_MultiPage(t *testing.T) {
	// Page 1: 100 clients + cursor; page 2: 20 clients, no cursor. Total: 120.
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClients",
		pageResult(makeClientItems(100), "cursor-c2"),
		pageResult(makeClientItems(20), ""),
	)

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listClientsTool(), client, map[string]any{
		"msgVpnName": "default",
		"maxResults": float64(200), // larger than 120 total so paginator follows all pages
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	clients := result["clients"].(map[string]any)
	items := clients["data"].([]any)

	if len(items) != 120 {
		t.Errorf("len(items) = %d, want 120", len(items))
	}
	if clients["truncated"] != false {
		t.Errorf("truncated = %v, want false", clients["truncated"])
	}
	if len(client.calls) != 2 {
		t.Errorf("expected 2 SEMP calls, got %d", len(client.calls))
	}
}

func TestExecute_ListQueues_MaxResultsCappedAt500(t *testing.T) {
	// maxResults=1000 exceeds the 500 cap, paginator should stop at 500.
	page1 := pageResult(makeQueueItems(100), "c2")
	page2 := pageResult(makeQueueItems(100), "c3")
	page3 := pageResult(makeQueueItems(100), "c4")
	page4 := pageResult(makeQueueItems(100), "c5")
	page5 := pageResult(makeQueueItems(100), "c6")
	// page6 would push beyond cap, should not be fetched.
	page6 := pageResult(makeQueueItems(100), "")

	client := newSeqMockClient()
	client.addResponses("getMsgVpnQueues", page1, page2, page3, page4, page5, page6)

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listQueuesTool(), client, map[string]any{
		"msgVpnName": "default",
		"maxResults": float64(1000),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	queues := result["queues"].(map[string]any)
	items := queues["data"].([]any)

	if len(items) != 500 {
		t.Errorf("len(items) = %d, want 500 (cap)", len(items))
	}
	if queues["truncated"] != true {
		t.Errorf("truncated = %v, want true", queues["truncated"])
	}
	wantMsg := "More results exist but the maximum limit of 500 has been reached. Not all results are shown."
	if queues["truncatedMessage"] != wantMsg {
		t.Errorf("truncatedMessage = %v, want %q", queues["truncatedMessage"], wantMsg)
	}
	// Cap stops after 5 pages (500 items), page6 must not be fetched.
	if len(client.calls) != 5 {
		t.Errorf("expected 5 SEMP calls, got %d", len(client.calls))
	}
}

func TestExecute_ListQueues_MissingVPNName(t *testing.T) {
	client := newSeqMockClient()
	executor := NewCompositeExecutor(testOperations())

	// msgVpnName is required by the template, omitting it should return an error.
	_, err := executor.Execute(context.Background(), listQueuesTool(), client, map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing msgVpnName, got nil")
	}
}

func TestExecute_GetVPNHealth_SEMPError(t *testing.T) {
	client := newMockClient()
	client.errors["getMsgVpn"] = &sempv2.SEMPError{
		Operation:  "getMsgVpn",
		StatusCode: 400,
		Body:       `{"meta":{"error":{"code":400,"description":"VPN not found","status":"NOT_FOUND"}}}`,
	}

	executor := NewCompositeExecutor(testOperations())

	_, err := executor.Execute(context.Background(), getVPNHealthTool(), client, map[string]any{
		"msgVpnName": "nonexistent",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var sempErr *sempv2.SEMPError
	if !errors.As(err, &sempErr) {
		t.Errorf("expected SEMPError in error chain, got: %v", err)
	} else if sempErr.StatusCode != 400 {
		t.Errorf("StatusCode = %d, want 400", sempErr.StatusCode)
	}
}

func TestExecute_ListQueues_EmptyFirstPage(t *testing.T) {
	// First page returns an empty data array — result should be [] not null, truncated=false.
	client := newSeqMockClient()
	client.addResponses("getMsgVpnQueues", pageResult([]any{}, ""))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listQueuesTool(), client, map[string]any{
		"msgVpnName": "default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	queues := result["queues"].(map[string]any)
	items, ok := queues["data"].([]any)
	if !ok {
		t.Fatal("expected queues.data to be a slice, got nil")
	}
	if len(items) != 0 {
		t.Errorf("len(items) = %d, want 0", len(items))
	}
	if queues["truncated"] != false {
		t.Errorf("truncated = %v, want false", queues["truncated"])
	}
}

func TestExecute_ListQueues_EmptyCursorInNextPageURI(t *testing.T) {
	// nextPageUri is present but has no cursor query param, executor must return an error.
	response := &sempv2.Result{
		Data: map[string]any{
			"data": makeQueueItems(10),
			"meta": map[string]any{
				"paging": map[string]any{
					"nextPageUri": "/SEMP/v2/monitor/msgVpns/default/queues?count=100",
				},
			},
		},
		StatusCode: 200,
	}

	client := newSeqMockClient()
	client.addResponses("getMsgVpnQueues", response)

	executor := NewCompositeExecutor(testOperations())

	_, err := executor.Execute(context.Background(), listQueuesTool(), client, map[string]any{
		"msgVpnName": "default",
	})
	if err == nil {
		t.Fatal("expected error for empty cursor in nextPageUri, got nil")
	}
	if !contains(err.Error(), "empty pagination cursor") {
		t.Errorf("expected error mentioning 'empty pagination cursor', got: %v", err)
	}
}

func TestExecute_ListQueues_UnparsableNextPageURI(t *testing.T) {
	// nextPageUri cannot be parsed by url.Parse — executor must return an error.
	response := &sempv2.Result{
		Data: map[string]any{
			"data": makeQueueItems(10),
			"meta": map[string]any{
				"paging": map[string]any{
					"nextPageUri": ":not-a-valid-uri",
				},
			},
		},
		StatusCode: 200,
	}

	client := newSeqMockClient()
	client.addResponses("getMsgVpnQueues", response)

	executor := NewCompositeExecutor(testOperations())

	_, err := executor.Execute(context.Background(), listQueuesTool(), client, map[string]any{
		"msgVpnName": "default",
	})
	if err == nil {
		t.Fatal("expected error for unparsable nextPageUri, got nil")
	}
	if !contains(err.Error(), "failed to parse pagination cursor") {
		t.Errorf("expected error mentioning 'failed to parse pagination cursor', got: %v", err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// getMessageRatesTool returns the get-message-rates tool definition for tests.
func getMessageRatesTool() CompositeTool {
	return CompositeTool{
		Name:        "get-message-rates",
		Description: "Get current and average message and byte rates for a Message VPN",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
		},
		Steps: []Step{
			{
				ID:        "rates",
				Operation: "monitor/getMsgVpn",
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

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

func TestExecute_GetMessageRates_ReturnsData(t *testing.T) {
	client := newMockClient()
	client.responses["getMsgVpn"] = &sempv2.Result{
		Data: map[string]any{
			"rxMsgRate":         float64(150),
			"txMsgRate":         float64(200),
			"rxByteRate":        float64(15000),
			"txByteRate":        float64(20000),
			"averageRxMsgRate":  float64(140),
			"averageTxMsgRate":  float64(190),
			"averageRxByteRate": float64(14000),
			"averageTxByteRate": float64(19000),
		},
		StatusCode: 200,
	}

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), getMessageRatesTool(), client, map[string]any{
		"msgVpnName": "default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rates, ok := result["rates"].(map[string]any)
	if !ok {
		t.Fatal("expected rates key containing a map")
	}
	if rates["rxMsgRate"] != float64(150) {
		t.Errorf("rxMsgRate = %v, want 150", rates["rxMsgRate"])
	}
	if rates["txMsgRate"] != float64(200) {
		t.Errorf("txMsgRate = %v, want 200", rates["txMsgRate"])
	}
}

func TestExecute_ListRDPs_SinglePage(t *testing.T) {
	rdpItems := []any{
		map[string]any{"restDeliveryPointName": "rdp-1", "enabled": true, "up": true},
		map[string]any{"restDeliveryPointName": "rdp-2", "enabled": true, "up": false},
	}
	client := newSeqMockClient()
	client.addResponses("getMsgVpnRestDeliveryPoints", pageResult(rdpItems, ""))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listRDPsTool(), client, map[string]any{
		"msgVpnName": "default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rdps, ok := result["rdps"].(map[string]any)
	if !ok {
		t.Fatal("expected rdps key containing a map")
	}
	items, ok := rdps["data"].([]any)
	if !ok {
		t.Fatal("expected rdps.data to be a slice")
	}
	if len(items) != 2 {
		t.Errorf("len(items) = %d, want 2", len(items))
	}
	if rdps["truncated"] != false {
		t.Errorf("truncated = %v, want false", rdps["truncated"])
	}
	if len(client.calls) != 1 {
		t.Errorf("expected 1 SEMP call, got %d", len(client.calls))
	}
}

func TestExecute_ListQueues_PageCapTerminatesLoop(t *testing.T) {
	// seqMockClient repeats the last response forever — one page with 1 item
	// and a cursor, so the paginator would loop indefinitely without the cap.
	client := newSeqMockClient()
	client.addResponses("getMsgVpnQueues", pageResult(makeQueueItems(1), "cursor-perpetual"))

	executor := NewCompositeExecutor(testOperations())
	// Lower the page cap on this instance so the test completes quickly without
	// needing capMax pages. Using a per-instance field avoids the race risk that
	// came with a package-level var when tests run with t.Parallel().
	executor.maxPages = 3

	result, err := executor.Execute(context.Background(), listQueuesTool(), client, map[string]any{
		"msgVpnName": "default",
		"maxResults": float64(capMax + 1), // above capMax so item cap doesn't fire first
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	queues := result["queues"].(map[string]any)

	// Page cap fires after maxPages=3 iterations; 1 item per page → 3 items collected.
	items := queues["data"].([]any)
	if len(items) != 3 {
		t.Errorf("len(items) = %d, want 3 (one per page before cap)", len(items))
	}
	if queues["truncated"] != true {
		t.Errorf("truncated = %v, want true", queues["truncated"])
	}
	got, _ := queues["truncatedMessage"].(string)
	if !strings.Contains(got, "Pagination stopped after 3 pages") {
		t.Errorf("truncatedMessage = %q, want it to start with cap-hit phrasing including page count", got)
	}
	if !strings.Contains(got, "narrow the query") {
		t.Errorf("truncatedMessage = %q, want it to include operator remediation hint", got)
	}
	// Exactly maxPages=3 SEMP calls made.
	if len(client.calls) != 3 {
		t.Errorf("expected 3 SEMP calls (one per page before cap), got %d", len(client.calls))
	}
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

// getReplicationStatusTool returns the get-replication-status tool definition for tests.
func getReplicationStatusTool() CompositeTool {
	return CompositeTool{
		Name:        "get-replication-status",
		Description: "Get replication state for a Message VPN",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
		},
		Steps: []Step{
			{
				ID:        "replication",
				Operation: "monitor/getMsgVpn",
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// listSlowSubscribersTool returns the list-slow-subscribers tool definition for tests.
func listSlowSubscribersTool() CompositeTool {
	return CompositeTool{
		Name:        "list-slow-subscribers",
		Description: "List clients flagged as slow subscribers",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "maxResults", Type: "integer", Required: false},
		},
		Steps: []Step{
			{
				ID:          "slowSubscribers",
				Operation:   "monitor/getMsgVpnClients",
				FollowPages: true,
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
					"count":      "100",
					"where":      "slowSubscriber==true",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// listQueueDiscardsTool returns the list-queue-discards tool definition for tests.
func listQueueDiscardsTool() CompositeTool {
	return CompositeTool{
		Name:        "list-queue-discards",
		Description: "List per-queue message discard counts for a Message VPN",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "maxResults", Type: "integer", Required: false},
		},
		Steps: []Step{
			{
				ID:          "queueDiscards",
				Operation:   "monitor/getMsgVpnQueues",
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

// makeSlowSubscriberItems builds a slice of n mock slow-subscriber client objects
// for use in paginated responses (slowSubscriber forced true to mirror the SEMP
// where filter that the tool applies).
func makeSlowSubscriberItems(n int) []any {
	items := make([]any, n)
	for i := range items {
		items[i] = map[string]any{
			"clientName":          fmt.Sprintf("slow-client-%d", i),
			"slowSubscriber":      true,
			"txDiscardedMsgCount": float64(0),
		}
	}
	return items
}

// makeQueueDiscardItems builds a slice of n mock queue objects with discard
// counters for use in list-queue-discards paginated responses.
func makeQueueDiscardItems(n int) []any {
	items := make([]any, n)
	for i := range items {
		items[i] = map[string]any{
			"queueName":                                 fmt.Sprintf("queue-%d", i),
			"maxTtlExceededDiscardedMsgCount":           float64(0),
			"maxRedeliveryExceededDiscardedMsgCount":    float64(0),
			"maxMsgSpoolUsageExceededDiscardedMsgCount": float64(0),
		}
	}
	return items
}

func TestExecute_GetReplicationStatus_ReturnsData(t *testing.T) {
	client := newMockClient()
	client.responses["getMsgVpn"] = &sempv2.Result{
		Data: map[string]any{
			"msgVpnName":                              "default",
			"replicationEnabled":                      true,
			"replicationRole":                         "active",
			"replicationTransactionMode":              "sync",
			"replicationActiveAsyncQueuedMsgCount":    float64(0),
			"replicationActiveSyncQueuedMsgCount":     float64(0),
			"replicationActiveSyncIneligiblePeakTime": float64(0),
			"replicationBridgeUp":                     true,
			"replicationRemoteBridgeUp":               true,
			"replicationSyncEligible":                 true,
		},
		StatusCode: 200,
	}

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), getReplicationStatusTool(), client, map[string]any{
		"msgVpnName": "default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	repl, ok := result["replication"].(map[string]any)
	if !ok {
		t.Fatal("expected replication key containing a map")
	}
	if repl["replicationEnabled"] != true {
		t.Errorf("replicationEnabled = %v, want true", repl["replicationEnabled"])
	}
	if repl["replicationRole"] != "active" {
		t.Errorf("replicationRole = %v, want active", repl["replicationRole"])
	}
	if repl["replicationSyncEligible"] != true {
		t.Errorf("replicationSyncEligible = %v, want true", repl["replicationSyncEligible"])
	}
}

func TestExecute_GetReplicationStatus_SEMPError(t *testing.T) {
	client := newMockClient()
	client.errors["getMsgVpn"] = &sempv2.SEMPError{
		Operation:  "getMsgVpn",
		StatusCode: 404,
	}

	executor := NewCompositeExecutor(testOperations())

	_, err := executor.Execute(context.Background(), getReplicationStatusTool(), client, map[string]any{
		"msgVpnName": "nonexistent",
	})
	if err == nil {
		t.Fatal("expected error when replication step fails, got nil")
	}

	var sempErr *sempv2.SEMPError
	if !errors.As(err, &sempErr) {
		t.Fatalf("expected SEMPError in error chain, got %T: %v", err, err)
	}
	if sempErr.StatusCode != 404 {
		t.Errorf("StatusCode = %d, want 404", sempErr.StatusCode)
	}
	if sempErr.Operation != "getMsgVpn" {
		t.Errorf("Operation = %q, want %q", sempErr.Operation, "getMsgVpn")
	}
}

func TestExecute_ListSlowSubscribers_ReturnsFiltered(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClients", pageResult(makeSlowSubscriberItems(3), ""))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listSlowSubscribersTool(), client, map[string]any{
		"msgVpnName": "default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	subs, ok := result["slowSubscribers"].(map[string]any)
	if !ok {
		t.Fatal("expected slowSubscribers key containing a map")
	}
	items, ok := subs["data"].([]any)
	if !ok {
		t.Fatal("expected slowSubscribers.data to be a slice")
	}
	if len(items) != 3 {
		t.Errorf("len(items) = %d, want 3", len(items))
	}
	if subs["truncated"] != false {
		t.Errorf("truncated = %v, want false", subs["truncated"])
	}

	// Verify the SEMP `where=slowSubscriber==true` predicate was passed through to
	// the broker — server-side filtering is the entire point of this tool.
	if len(client.calls) != 1 {
		t.Fatalf("expected 1 SEMP call, got %d", len(client.calls))
	}
	if client.calls[0].args["where"] != "slowSubscriber==true" {
		t.Errorf("where = %v, want slowSubscriber==true", client.calls[0].args["where"])
	}
}

func TestExecute_ListSlowSubscribers_Empty(t *testing.T) {
	// Empty is the steady-state return value: most VPNs don't have slow
	// subscribers most of the time. Pins the len(items) == 0 → break path
	// through the paginator.
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClients", pageResult([]any{}, ""))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listSlowSubscribersTool(), client, map[string]any{
		"msgVpnName": "default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	subs, ok := result["slowSubscribers"].(map[string]any)
	if !ok {
		t.Fatal("expected slowSubscribers key containing a map")
	}
	items, ok := subs["data"].([]any)
	if !ok {
		t.Fatal("expected slowSubscribers.data to be a slice")
	}
	if len(items) != 0 {
		t.Errorf("len(items) = %d, want 0", len(items))
	}
	if subs["truncated"] != false {
		t.Errorf("truncated = %v, want false", subs["truncated"])
	}
	if len(client.calls) != 1 {
		t.Errorf("expected 1 SEMP call, got %d", len(client.calls))
	}
}

func TestExecute_ListSlowSubscribers_TruncatesAtMaxResults(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpnClients", pageResult(makeSlowSubscriberItems(100), "cursor-next"))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listSlowSubscribersTool(), client, map[string]any{
		"msgVpnName": "default",
		"maxResults": float64(40),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	subs := result["slowSubscribers"].(map[string]any)
	items := subs["data"].([]any)

	if len(items) != 40 {
		t.Errorf("len(items) = %d, want 40", len(items))
	}
	if subs["truncated"] != true {
		t.Errorf("truncated = %v, want true", subs["truncated"])
	}
	if len(client.calls) != 1 {
		t.Errorf("expected 1 SEMP call, got %d", len(client.calls))
	}
}

func TestExecute_ListQueueDiscards_SinglePage(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpnQueues", pageResult(makeQueueDiscardItems(5), ""))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listQueueDiscardsTool(), client, map[string]any{
		"msgVpnName": "default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	stats, ok := result["queueDiscards"].(map[string]any)
	if !ok {
		t.Fatal("expected queueDiscards key containing a map")
	}
	items, ok := stats["data"].([]any)
	if !ok {
		t.Fatal("expected queueDiscards.data to be a slice")
	}
	if len(items) != 5 {
		t.Errorf("len(items) = %d, want 5", len(items))
	}
	if stats["truncated"] != false {
		t.Errorf("truncated = %v, want false", stats["truncated"])
	}

	// Spot-check that discard counter fields survived the round-trip.
	first := items[0].(map[string]any)
	if _, ok := first["maxTtlExceededDiscardedMsgCount"]; !ok {
		t.Error("expected maxTtlExceededDiscardedMsgCount on per-queue result")
	}
}

func TestExecute_ListQueueDiscards_TruncatesAtMaxResults(t *testing.T) {
	client := newSeqMockClient()
	client.addResponses("getMsgVpnQueues", pageResult(makeQueueDiscardItems(100), "cursor-next"))

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), listQueueDiscardsTool(), client, map[string]any{
		"msgVpnName": "default",
		"maxResults": float64(50),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	stats := result["queueDiscards"].(map[string]any)
	items := stats["data"].([]any)

	if len(items) != 50 {
		t.Errorf("len(items) = %d, want 50", len(items))
	}
	if stats["truncated"] != true {
		t.Errorf("truncated = %v, want true", stats["truncated"])
	}
	if len(client.calls) != 1 {
		t.Errorf("expected 1 SEMP call, got %d", len(client.calls))
	}
}

// createMessageVPNTool returns the create-message-vpn tool definition for tests.
// It mirrors the YAML: a single config/createMsgVpn step with no Args, so the
// entire request body is assembled by constructRequestBody from the input params.
func createMessageVPNTool() CompositeTool {
	return CompositeTool{
		Name:        "create-message-vpn",
		Description: "Create a Message VPN",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "msgVpnConfig", Type: "object", Required: false},
		},
		Steps: []Step{
			{
				ID:        "createVpn",
				Operation: "config/createMsgVpn",
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// updateMessageVPNTool returns the update-message-vpn tool definition for tests.
// It mirrors the YAML: msgVpnName is passed as a step arg (it identifies the VPN
// via the URL path), while msgVpnConfig is spread into the PATCH body by
// constructRequestBody.
func updateMessageVPNTool() CompositeTool {
	return CompositeTool{
		Name:        "update-message-vpn",
		Description: "Update an existing Message VPN",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "msgVpnConfig", Type: "object", Required: true},
		},
		Steps: []Step{
			{
				ID:        "updateVpn",
				Operation: "config/updateMsgVpn",
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// deleteMessageVPNTool returns the delete-message-vpn tool definition for tests.
// It mirrors the YAML: a single config/deleteMsgVpn step that takes only the
// msgVpnName path arg and carries no request body.
func deleteMessageVPNTool() CompositeTool {
	return CompositeTool{
		Name:        "delete-message-vpn",
		Description: "Delete a Message VPN",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
		},
		Steps: []Step{
			{
				ID:        "deleteVpn",
				Operation: "config/deleteMsgVpn",
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

func TestExecute_CreateMessageVPN_ConstructsBody(t *testing.T) {
	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: newMockClient(), recorded: &recorded, mu: &mu}

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), createMessageVPNTool(), capture, map[string]any{
		"msgVpnName": "new-vpn",
		"msgVpnConfig": map[string]any{
			"enabled":            true,
			"maxConnectionCount": float64(100),
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(recorded) != 1 {
		t.Fatalf("expected 1 call, got %d", len(recorded))
	}
	if recorded[0].opID != "createMsgVpn" {
		t.Errorf("opID = %q, want createMsgVpn", recorded[0].opID)
	}

	body, ok := recorded[0].args["body"].(map[string]any)
	if !ok {
		t.Fatalf("expected body to be a map, got %T", recorded[0].args["body"])
	}
	// createMsgVpn declares no path param, so msgVpnName rides in the body as a scalar.
	if body["msgVpnName"] != "new-vpn" {
		t.Errorf("body[msgVpnName] = %v, want new-vpn", body["msgVpnName"])
	}
	// msgVpnConfig fields are spread into the body as siblings of msgVpnName.
	if body["enabled"] != true {
		t.Errorf("body[enabled] = %v, want true", body["enabled"])
	}
	if body["maxConnectionCount"] != float64(100) {
		t.Errorf("body[maxConnectionCount] = %v, want 100", body["maxConnectionCount"])
	}
	// The object param must be spread, never nested back under its own key.
	if _, nested := body["msgVpnConfig"]; nested {
		t.Error("msgVpnConfig should be spread into the body, not nested under msgVpnConfig")
	}

	if result["createVpn"] == nil {
		t.Error("expected createVpn result to be collected")
	}
}

func TestExecute_CreateMessageVPN_AlreadyExists(t *testing.T) {
	client := newMockClient()
	client.errors["createMsgVpn"] = &sempv2.SEMPError{
		Operation:   "createMsgVpn",
		StatusCode:  400,
		SEMPCode:    10,
		SEMPStatus:  "ALREADY_EXISTS",
		Description: "Message VPN already exists",
		Body:        `{"meta":{"error":{"code":10,"description":"Message VPN already exists","status":"ALREADY_EXISTS"}}}`,
	}

	executor := NewCompositeExecutor(testOperations())

	_, err := executor.Execute(context.Background(), createMessageVPNTool(), client, map[string]any{
		"msgVpnName": "existing-vpn",
	})
	if err == nil {
		t.Fatal("expected error when VPN already exists, got nil")
	}

	var sempErr *sempv2.SEMPError
	if !errors.As(err, &sempErr) {
		t.Errorf("expected SEMPError in error chain, got: %v", err)
	} else if sempErr.SEMPCode != 10 {
		t.Errorf("SEMPCode = %d, want 10 (already exists)", sempErr.SEMPCode)
	}
}

func TestExecute_UpdateMessageVPN_BodyExcludesPathParam(t *testing.T) {
	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: newMockClient(), recorded: &recorded, mu: &mu}

	executor := NewCompositeExecutor(testOperations())

	_, err := executor.Execute(context.Background(), updateMessageVPNTool(), capture, map[string]any{
		"msgVpnName": "edit-vpn",
		"msgVpnConfig": map[string]any{
			"enabled":          false,
			"maxMsgSpoolUsage": float64(5000),
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if recorded[0].opID != "updateMsgVpn" {
		t.Errorf("opID = %q, want updateMsgVpn", recorded[0].opID)
	}
	// msgVpnName identifies the VPN via the URL path, so it stays in args.
	if recorded[0].args["msgVpnName"] != "edit-vpn" {
		t.Errorf("args[msgVpnName] = %v, want edit-vpn", recorded[0].args["msgVpnName"])
	}

	body, ok := recorded[0].args["body"].(map[string]any)
	if !ok {
		t.Fatalf("expected body to be a map, got %T", recorded[0].args["body"])
	}
	// The path param must not be duplicated into the PATCH body.
	if _, leaked := body["msgVpnName"]; leaked {
		t.Error("msgVpnName is a path param and must not appear in the PATCH body")
	}
	// Only the requested attribute changes ride in the body.
	if body["enabled"] != false {
		t.Errorf("body[enabled] = %v, want false", body["enabled"])
	}
	if body["maxMsgSpoolUsage"] != float64(5000) {
		t.Errorf("body[maxMsgSpoolUsage] = %v, want 5000", body["maxMsgSpoolUsage"])
	}
}

func TestExecute_UpdateMessageVPN_NotFound(t *testing.T) {
	client := newMockClient()
	client.errors["updateMsgVpn"] = &sempv2.SEMPError{
		Operation:   "updateMsgVpn",
		StatusCode:  400,
		SEMPCode:    6,
		SEMPStatus:  "NOT_FOUND",
		Description: "Message VPN not found",
		Body:        `{"meta":{"error":{"code":6,"description":"Message VPN not found","status":"NOT_FOUND"}}}`,
	}

	executor := NewCompositeExecutor(testOperations())

	_, err := executor.Execute(context.Background(), updateMessageVPNTool(), client, map[string]any{
		"msgVpnName":   "ghost-vpn",
		"msgVpnConfig": map[string]any{"enabled": true},
	})
	if err == nil {
		t.Fatal("expected error when VPN not found, got nil")
	}

	var sempErr *sempv2.SEMPError
	if !errors.As(err, &sempErr) {
		t.Errorf("expected SEMPError in error chain, got: %v", err)
	} else if sempErr.SEMPCode != 6 {
		t.Errorf("SEMPCode = %d, want 6 (not found)", sempErr.SEMPCode)
	}
}

func TestExecute_DeleteMessageVPN_NoBodyConstructed(t *testing.T) {
	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: newMockClient(), recorded: &recorded, mu: &mu}

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), deleteMessageVPNTool(), capture, map[string]any{
		"msgVpnName": "doomed-vpn",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if recorded[0].opID != "deleteMsgVpn" {
		t.Errorf("opID = %q, want deleteMsgVpn", recorded[0].opID)
	}
	if recorded[0].args["msgVpnName"] != "doomed-vpn" {
		t.Errorf("args[msgVpnName] = %v, want doomed-vpn", recorded[0].args["msgVpnName"])
	}
	// deleteMsgVpn declares no body parameter, so constructRequestBody must not
	// synthesize a body key.
	if _, hasBody := recorded[0].args["body"]; hasBody {
		t.Error("delete operation has no body param; no body should be constructed")
	}

	if result["deleteVpn"] == nil {
		t.Error("expected deleteVpn result to be collected")
	}
}

func TestExecute_DeleteMessageVPN_SEMPError(t *testing.T) {
	client := newMockClient()
	client.errors["deleteMsgVpn"] = &sempv2.SEMPError{
		Operation:   "deleteMsgVpn",
		StatusCode:  400,
		SEMPCode:    6,
		SEMPStatus:  "NOT_FOUND",
		Description: "Message VPN not found",
		Body:        `{"meta":{"error":{"code":6,"description":"Message VPN not found","status":"NOT_FOUND"}}}`,
	}

	executor := NewCompositeExecutor(testOperations())

	_, err := executor.Execute(context.Background(), deleteMessageVPNTool(), client, map[string]any{
		"msgVpnName": "ghost-vpn",
	})
	if err == nil {
		t.Fatal("expected error when deleting nonexistent VPN, got nil")
	}

	var sempErr *sempv2.SEMPError
	if !errors.As(err, &sempErr) {
		t.Errorf("expected SEMPError in error chain, got: %v", err)
	} else if sempErr.SEMPCode != 6 {
		t.Errorf("SEMPCode = %d, want 6 (not found)", sempErr.SEMPCode)
	}
}
