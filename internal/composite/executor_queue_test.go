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

// createQueueTool returns the create-queue tool definition for tests. It mirrors
// the YAML: createMsgVpnQueue takes msgVpnName from the path (passed as a step
// arg), while queueName and queueConfig are assembled into the body by
// constructRequestBody.
func createQueueTool() CompositeTool {
	return CompositeTool{
		Name:        "create-queue",
		Description: "Create a queue in a Message VPN",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "queueName", Type: "string", Required: true},
			{Name: "queueConfig", Type: "object", Required: false},
		},
		Steps: []Step{
			{
				ID:        "createQueue",
				Operation: "config/createMsgVpnQueue",
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// updateQueueTool returns the update-queue tool definition for tests. Both
// msgVpnName and queueName are path params (passed as step args); only
// queueConfig is spread into the PATCH body.
func updateQueueTool() CompositeTool {
	return CompositeTool{
		Name:        "update-queue",
		Description: "Update an existing queue",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "queueName", Type: "string", Required: true},
			{Name: "queueConfig", Type: "object", Required: true},
		},
		Steps: []Step{
			{
				ID:        "updateQueue",
				Operation: "config/updateMsgVpnQueue",
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
					"queueName":  "{{.Params.queueName}}",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// deleteQueueTool returns the delete-queue tool definition for tests. Both names
// are path args and the operation carries no request body.
func deleteQueueTool() CompositeTool {
	return CompositeTool{
		Name:        "delete-queue",
		Description: "Delete a queue from a Message VPN",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "queueName", Type: "string", Required: true},
		},
		Steps: []Step{
			{
				ID:        "deleteQueue",
				Operation: "config/deleteMsgVpnQueue",
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
					"queueName":  "{{.Params.queueName}}",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// makeQueueItems builds a slice of n mock queue objects for use in paginated responses.
func makeQueueItems(n int) []any {
	items := make([]any, n)
	for i := range items {
		items[i] = map[string]any{"queueName": fmt.Sprintf("queue-%d", i), "bindCount": float64(0)}
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

func TestExecute_CreateQueue_ConstructsBody(t *testing.T) {
	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: newMockClient(), recorded: &recorded, mu: &mu}

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), createQueueTool(), capture, map[string]any{
		"msgVpnName": "vpn-a",
		"queueName":  "orders",
		"queueConfig": map[string]any{
			"accessType":       "exclusive",
			"maxMsgSpoolUsage": float64(2000),
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if recorded[0].opID != "createMsgVpnQueue" {
		t.Errorf("opID = %q, want createMsgVpnQueue", recorded[0].opID)
	}
	// msgVpnName identifies the VPN via the URL path, so it stays in args.
	if recorded[0].args["msgVpnName"] != "vpn-a" {
		t.Errorf("args[msgVpnName] = %v, want vpn-a", recorded[0].args["msgVpnName"])
	}

	body, ok := recorded[0].args["body"].(map[string]any)
	if !ok {
		t.Fatalf("expected body to be a map, got %T", recorded[0].args["body"])
	}
	// createMsgVpnQueue declares no queueName path param, so the queue name rides
	// in the body as a scalar.
	if body["queueName"] != "orders" {
		t.Errorf("body[queueName] = %v, want orders", body["queueName"])
	}
	// queueConfig fields are spread into the body as siblings of queueName.
	if body["accessType"] != "exclusive" {
		t.Errorf("body[accessType] = %v, want exclusive", body["accessType"])
	}
	if body["maxMsgSpoolUsage"] != float64(2000) {
		t.Errorf("body[maxMsgSpoolUsage] = %v, want 2000", body["maxMsgSpoolUsage"])
	}
	// msgVpnName is a path param and must not leak into the create body.
	if _, leaked := body["msgVpnName"]; leaked {
		t.Error("msgVpnName is a path param and must not appear in the create body")
	}
	if _, nested := body["queueConfig"]; nested {
		t.Error("queueConfig should be spread into the body, not nested under queueConfig")
	}

	if result["createQueue"] == nil {
		t.Error("expected createQueue result to be collected")
	}
}

func TestExecute_CreateQueue_AlreadyExists(t *testing.T) {
	client := newMockClient()
	client.errors["createMsgVpnQueue"] = &sempv2.SEMPError{
		Operation:   "createMsgVpnQueue",
		StatusCode:  400,
		SEMPCode:    10,
		SEMPStatus:  "ALREADY_EXISTS",
		Description: "Queue already exists",
		Body:        `{"meta":{"error":{"code":10,"description":"Queue already exists","status":"ALREADY_EXISTS"}}}`,
	}

	executor := NewCompositeExecutor(testOperations())

	_, err := executor.Execute(context.Background(), createQueueTool(), client, map[string]any{
		"msgVpnName": "vpn-a",
		"queueName":  "orders",
	})
	if err == nil {
		t.Fatal("expected error when queue already exists, got nil")
	}

	var sempErr *sempv2.SEMPError
	if !errors.As(err, &sempErr) {
		t.Errorf("expected SEMPError in error chain, got: %v", err)
	} else if sempErr.SEMPCode != 10 {
		t.Errorf("SEMPCode = %d, want 10 (already exists)", sempErr.SEMPCode)
	}
}

func TestExecute_UpdateQueue_BodyExcludesPathParams(t *testing.T) {
	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: newMockClient(), recorded: &recorded, mu: &mu}

	executor := NewCompositeExecutor(testOperations())

	_, err := executor.Execute(context.Background(), updateQueueTool(), capture, map[string]any{
		"msgVpnName": "vpn-a",
		"queueName":  "orders",
		"queueConfig": map[string]any{
			"egressEnabled":      false,
			"maxRedeliveryCount": float64(3),
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if recorded[0].opID != "updateMsgVpnQueue" {
		t.Errorf("opID = %q, want updateMsgVpnQueue", recorded[0].opID)
	}
	if recorded[0].args["msgVpnName"] != "vpn-a" {
		t.Errorf("args[msgVpnName] = %v, want vpn-a", recorded[0].args["msgVpnName"])
	}
	if recorded[0].args["queueName"] != "orders" {
		t.Errorf("args[queueName] = %v, want orders", recorded[0].args["queueName"])
	}

	body, ok := recorded[0].args["body"].(map[string]any)
	if !ok {
		t.Fatalf("expected body to be a map, got %T", recorded[0].args["body"])
	}
	// Both names are path params; neither may be duplicated into the PATCH body.
	if _, leaked := body["msgVpnName"]; leaked {
		t.Error("msgVpnName is a path param and must not appear in the PATCH body")
	}
	if _, leaked := body["queueName"]; leaked {
		t.Error("queueName is a path param and must not appear in the PATCH body")
	}
	if body["egressEnabled"] != false {
		t.Errorf("body[egressEnabled] = %v, want false", body["egressEnabled"])
	}
	if body["maxRedeliveryCount"] != float64(3) {
		t.Errorf("body[maxRedeliveryCount] = %v, want 3", body["maxRedeliveryCount"])
	}
}

func TestExecute_DeleteQueue_NoBodyConstructed(t *testing.T) {
	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: newMockClient(), recorded: &recorded, mu: &mu}

	executor := NewCompositeExecutor(testOperations())

	result, err := executor.Execute(context.Background(), deleteQueueTool(), capture, map[string]any{
		"msgVpnName": "vpn-a",
		"queueName":  "orders",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if recorded[0].opID != "deleteMsgVpnQueue" {
		t.Errorf("opID = %q, want deleteMsgVpnQueue", recorded[0].opID)
	}
	if recorded[0].args["queueName"] != "orders" {
		t.Errorf("args[queueName] = %v, want orders", recorded[0].args["queueName"])
	}
	// deleteMsgVpnQueue declares no body parameter, so no body should be built.
	if _, hasBody := recorded[0].args["body"]; hasBody {
		t.Error("delete operation has no body param; no body should be constructed")
	}

	if result["deleteQueue"] == nil {
		t.Error("expected deleteQueue result to be collected")
	}
}
