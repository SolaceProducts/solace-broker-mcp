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

// getVPNStatusTool returns the get-vpn-status tool definition for tests.
func getVPNStatusTool() CompositeTool {
	return CompositeTool{
		Name:        "get-vpn-status",
		Description: "Get operational status and connection statistics for a Message VPN",
		Parameters: []ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
		},
		Steps: []Step{
			{
				ID:        "vpnStatus",
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

// makeVPNItems builds a slice of n mock VPN objects for use in paginated responses.
func makeVPNItems(n int) []any {
	items := make([]any, n)
	for i := range items {
		items[i] = map[string]any{"msgVpnName": fmt.Sprintf("vpn-%d", i), "enabled": true}
	}
	return items
}

func TestExecute_GetVPNStatus_ReturnsData(t *testing.T) {
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

	result, err := executor.Execute(context.Background(), getVPNStatusTool(), client, map[string]any{
		"msgVpnName": "default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	vpnData, ok := result["vpnStatus"].(map[string]any)
	if !ok {
		t.Fatal("expected vpnStatus key containing a map")
	}
	if vpnData["msgVpnName"] != "default" {
		t.Errorf("msgVpnName = %v, want default", vpnData["msgVpnName"])
	}
	if vpnData["enabled"] != true {
		t.Errorf("enabled = %v, want true", vpnData["enabled"])
	}
}

func TestExecute_GetVPNStatus_SEMPError(t *testing.T) {
	client := newMockClient()
	client.errors["getMsgVpn"] = &sempv2.SEMPError{
		Operation:  "getMsgVpn",
		StatusCode: 400,
		Body:       `{"meta":{"error":{"code":400,"description":"VPN not found","status":"NOT_FOUND"}}}`,
	}

	executor := NewCompositeExecutor(testOperations())

	_, err := executor.Execute(context.Background(), getVPNStatusTool(), client, map[string]any{
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

func TestExecute_CreateMessageVPN_RejectsFieldCollision(t *testing.T) {
	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: newMockClient(), recorded: &recorded, mu: &mu}

	executor := NewCompositeExecutor(testOperations())

	// msgVpnName is supplied both as the dedicated param and inside msgVpnConfig.
	// The two sources could disagree, and with no defined precedence the winner
	// would depend on map-iteration order, so the request must be rejected.
	_, err := executor.Execute(context.Background(), createMessageVPNTool(), capture, map[string]any{
		"msgVpnName": "new-vpn",
		"msgVpnConfig": map[string]any{
			"msgVpnName": "other-vpn",
			"enabled":    true,
		},
	})
	if err == nil {
		t.Fatal("expected error for duplicate body field, got nil")
	}
	if !strings.Contains(err.Error(), "msgVpnName") {
		t.Errorf("error should name the colliding field, got: %v", err)
	}
	// The collision is caught while constructing the body, before any SEMP call.
	if len(recorded) != 0 {
		t.Errorf("expected no SEMP calls on rejected body, got %d", len(recorded))
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

func TestExecute_UpdateMessageVPN_RejectsPathParamInConfig(t *testing.T) {
	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: newMockClient(), recorded: &recorded, mu: &mu}

	executor := NewCompositeExecutor(testOperations())

	// msgVpnName is a path param of updateMsgVpn, so its scalar form is taken from
	// the URL. Placing it inside msgVpnConfig would otherwise leak it into the
	// PATCH body; it must be rejected before any SEMP call.
	_, err := executor.Execute(context.Background(), updateMessageVPNTool(), capture, map[string]any{
		"msgVpnName": "prod-vpn",
		"msgVpnConfig": map[string]any{
			"msgVpnName": "prod-vpn",
			"enabled":    false,
		},
	})
	if err == nil {
		t.Fatal("expected error for path param inside config object, got nil")
	}
	if !strings.Contains(err.Error(), "msgVpnName") {
		t.Errorf("error should name the offending field, got: %v", err)
	}
	if len(recorded) != 0 {
		t.Errorf("expected no SEMP calls on rejected body, got %d", len(recorded))
	}
}

func TestExecute_UpdateMessageVPN_RejectsUnknownBodyField(t *testing.T) {
	var recorded []callRecord
	var mu sync.Mutex
	capture := &argCapturingClient{inner: newMockClient(), recorded: &recorded, mu: &mu}

	// updateMsgVpn whose body schema declares only these attributes. A field the
	// schema doesn't know about must be rejected here rather than sent on and bounced
	// by the broker as an unknown attribute.
	ops := map[string]*sempv2.Operation{
		"config/updateMsgVpn": {
			ID:     "updateMsgVpn",
			Method: "PATCH",
			Path:   "/SEMP/v2/__private_config__/msgVpns/{msgVpnName}",
			Parameters: []sempv2.Parameter{
				{Name: "msgVpnName", In: "path", Type: "string", Required: true},
				{Name: "body", In: "body", Type: "object", Required: true},
			},
			BodyFields: map[string]bool{"msgVpnName": true, "enabled": true},
		},
	}
	executor := NewCompositeExecutor(ops)

	_, err := executor.Execute(context.Background(), updateMessageVPNTool(), capture, map[string]any{
		"msgVpnName": "prod-vpn",
		"msgVpnConfig": map[string]any{
			"enabled": false,
			"dryRun":  true,
		},
	})
	if err == nil {
		t.Fatal("expected error for unknown body field, got nil")
	}
	if !strings.Contains(err.Error(), "dryRun") {
		t.Errorf("error should name the unknown field, got: %v", err)
	}
	if len(recorded) != 0 {
		t.Errorf("expected no SEMP calls on rejected body, got %d", len(recorded))
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
