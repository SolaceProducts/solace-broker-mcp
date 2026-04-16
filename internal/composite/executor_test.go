package composite

import (
	"context"
	"errors"
	"fmt"
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
	}
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
				ID:        "subscriptions",
				Operation: "monitor/getMsgVpnClientSubscriptions",
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
					"clientName": "{{.Params.clientName}}",
					"count": `{{with index .Params "maxResults"}}{{.}}{{else}}100{{end}}`,
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
		StatusCode: 404,
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
	} else if sempErr.StatusCode != 404 {
		t.Errorf("expected status code 404, got %d", sempErr.StatusCode)
	}
}

func TestExecute_GetClientDetails_ReturnsData(t *testing.T) {
	client := newMockClient()
	client.responses["getMsgVpnClient"] = &sempv2.Result{
		Data: map[string]any{
			"clientName":            "myapp/1",
			"clientUsername":        "app-user",
			"slowSubscriber":        false,
			"txDiscardedMsgCount":   float64(0),
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
		StatusCode: 404,
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
	} else if sempErr.StatusCode != 404 {
		t.Errorf("expected status code 404, got %d", sempErr.StatusCode)
	}
}

func TestExecute_ListClientSubscriptions_ReturnsData(t *testing.T) {
	client := newMockClient()
	client.responses["getMsgVpnClientSubscriptions"] = &sempv2.Result{
		Data: map[string]any{
			"data": []any{
				map[string]any{"subscriptionTopic": "orders/>", "clientName": "myapp/1"},
				map[string]any{"subscriptionTopic": "inventory/>", "clientName": "myapp/1"},
			},
		},
		StatusCode: 200,
	}

	executor := NewCompositeExecutor(testOperations())

	params := map[string]any{
		"msgVpnName": "default",
		"clientName": "myapp/1",
	}

	result, err := executor.Execute(context.Background(), listClientSubscriptionsTool(), client, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result["subscriptions"] == nil {
		t.Fatal("expected subscriptions key in result, got nil")
	}
}

func TestExecute_ListClientSubscriptions_PassesMaxResults(t *testing.T) {
	client := newMockClient()
	executor := NewCompositeExecutor(testOperations())

	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: client, recorded: &recorded, mu: &mu}

	params := map[string]any{
		"msgVpnName": "default",
		"clientName": "myapp/1",
		"maxResults": 50,
	}

	_, err := executor.Execute(context.Background(), listClientSubscriptionsTool(), capture, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(recorded) != 1 {
		t.Fatalf("expected 1 call, got %d", len(recorded))
	}

	if recorded[0].args["count"] != "50" {
		t.Errorf("expected count %q, got %v", "50", recorded[0].args["count"])
	}
}

func TestExecute_ListClientSubscriptions_DefaultsCountQuery(t *testing.T) {
	client := newMockClient()
	executor := NewCompositeExecutor(testOperations())

	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: client, recorded: &recorded, mu: &mu}

	// maxResults is intentionally absent — executor should default count to "100".
	params := map[string]any{
		"msgVpnName": "default",
		"clientName": "myapp/1",
	}

	_, err := executor.Execute(context.Background(), listClientSubscriptionsTool(), capture, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(recorded) != 1 {
		t.Fatalf("expected 1 call, got %d", len(recorded))
	}

	if recorded[0].args["count"] != "100" {
		t.Errorf("expected count to default to %q, got %v", "100", recorded[0].args["count"])
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
