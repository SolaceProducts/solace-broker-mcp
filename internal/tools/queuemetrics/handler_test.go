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

package queuemetrics

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv1"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tools"
)

// mockV1 is a stub sempv1.Client. It returns the configured InnerXML (or err)
// and records the request XML so tests can assert the built command.
type mockV1 struct {
	inner   []byte
	err     error
	lastXML string
}

func (m *mockV1) Execute(_ context.Context, x string) (*sempv1.Result, error) {
	m.lastXML = x
	if m.err != nil {
		return nil, m.err
	}
	return &sempv1.Result{InnerXML: m.inner}, nil
}

// mockV2 is a stub sempv2.Client. It returns the configured Data (or err) and
// records the operation and args so tests can assert path/select.
type mockV2 struct {
	data     map[string]any
	err      error
	lastOp   *sempv2.Operation
	lastArgs map[string]any
}

func (m *mockV2) Execute(_ context.Context, op *sempv2.Operation, args map[string]any) (*sempv2.Result, error) {
	m.lastOp = op
	m.lastArgs = args
	if m.err != nil {
		return nil, m.err
	}
	return &sempv2.Result{Data: m.data, StatusCode: 200}, nil
}

// sempv2Envelope is a representative SEMPv2 monitor response body: the queue
// fields nested under "data", plus a "meta" block — the shape get-queue-metrics
// preserves under the "queueMetrics" key.
func sempv2Envelope() map[string]any {
	return map[string]any{
		"data": map[string]any{
			"queueName":         "sol150260-test",
			"msgVpnName":        "vpn_1",
			"spooledMsgCount":   float64(10), // cumulative lifetime, not current depth
			"bindCount":         float64(1),
			"txUnackedMsgCount": float64(0),
			"rxMsgRate":         float64(5),
			"txMsgRate":         float64(5),
		},
		"meta": map[string]any{"responseCode": float64(200)},
	}
}

func fixtureXML(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/show_queue_detail.xml")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return b
}

func callHandle(t *testing.T, v1 *mockV1, v2 *mockV2, params map[string]any) (*tools.ToolResult, error) {
	t.Helper()
	tc := &tools.ToolContext{SEMPv1Client: v1, SEMPv2Client: v2}
	return NewHandler().Handle(context.Background(), tc, params)
}

func defaultParams() map[string]any {
	return map[string]any{"msgVpnName": "vpn_1", "queueName": "sol150260-test"}
}

func TestHandle_MergesBothProtocols(t *testing.T) {
	v1 := &mockV1{inner: fixtureXML(t)}
	v2 := &mockV2{data: sempv2Envelope()}

	res, err := callHandle(t, v1, v2, defaultParams())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// queueMetrics is the SEMPv2 envelope verbatim.
	qm, ok := res.StructuredContent["queueMetrics"].(map[string]any)
	if !ok {
		t.Fatalf("queueMetrics missing/wrong type: %T", res.StructuredContent["queueMetrics"])
	}
	data, ok := qm["data"].(map[string]any)
	if !ok {
		t.Fatalf("queueMetrics.data missing/wrong type: %T", qm["data"])
	}
	if data["spooledMsgCount"] != float64(10) {
		t.Errorf("queueMetrics.data.spooledMsgCount = %v, want 10 (cumulative preserved)", data["spooledMsgCount"])
	}

	// liveDepth is the authoritative current depth from SEMPv1.
	ld, ok := res.StructuredContent["liveDepth"].(map[string]any)
	if !ok {
		t.Fatalf("liveDepth missing/wrong type: %T", res.StructuredContent["liveDepth"])
	}
	if ld["currentMsgCount"] != float64(10) {
		t.Errorf("liveDepth.currentMsgCount = %v, want 10", ld["currentMsgCount"])
	}
	if ld["currentSpoolUsageBytes"] != float64(340) {
		t.Errorf("liveDepth.currentSpoolUsageBytes = %v, want 340", ld["currentSpoolUsageBytes"])
	}
	if ld["oldestMsgId"] != float64(12794319) || ld["newestMsgId"] != float64(12794328) {
		t.Errorf("oldest/newest msg id = %v/%v, want 12794319/12794328", ld["oldestMsgId"], ld["newestMsgId"])
	}

	// SEMPv2 call used the private monitor path and passed the select.
	if !strings.Contains(v2.lastOp.Path, "__private_monitor__") {
		t.Errorf("SEMPv2 path = %q, want private monitor endpoint", v2.lastOp.Path)
	}
	if sel, _ := v2.lastArgs["select"].(string); !strings.Contains(sel, "spooledMsgCount") || !strings.Contains(sel, "bindCount") {
		t.Errorf("select missing expected fields: %q", sel)
	}
	// SEMPv1 request targets the right queue/vpn with detail.
	if !strings.Contains(v1.lastXML, "<name>sol150260-test</name>") ||
		!strings.Contains(v1.lastXML, "<vpn-name>vpn_1</vpn-name>") ||
		!strings.Contains(v1.lastXML, "<detail/>") {
		t.Errorf("SEMPv1 request malformed: %q", v1.lastXML)
	}
}

func TestHandle_EmptyQueueReturnsZeroDepth(t *testing.T) {
	// Empty queue: num-messages-spooled 0, no oldest/newest msg id.
	inner := []byte(`<show><queue><queues><queue><info>` +
		`<message-vpn>vpn_1</message-vpn>` +
		`<num-messages-spooled>0</num-messages-spooled>` +
		`<current-spool-usage-in-bytes>0</current-spool-usage-in-bytes>` +
		`<total-delivered-unacked-msgs>0</total-delivered-unacked-msgs>` +
		`</info></queue></queues></queue></show>`)
	v1 := &mockV1{inner: inner}
	v2 := &mockV2{data: sempv2Envelope()}

	res, err := callHandle(t, v1, v2, defaultParams())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	ld := res.StructuredContent["liveDepth"].(map[string]any)
	// currentMsgCount is present and zero (explicit 0, not omitted).
	if ld["currentMsgCount"] != float64(0) {
		t.Errorf("currentMsgCount = %v, want 0", ld["currentMsgCount"])
	}
	// oldest/newest are omitted when the broker does not report them.
	if _, present := ld["oldestMsgId"]; present {
		t.Errorf("oldestMsgId should be omitted on an empty queue, got %v", ld["oldestMsgId"])
	}
}

func TestHandle_SEMPv1ErrorFailsWhole(t *testing.T) {
	v1 := &mockV1{err: &sempv1.Error{Kind: sempv1.ErrorKindHTTP, StatusCode: 401}}
	v2 := &mockV2{data: sempv2Envelope()}
	if _, err := callHandle(t, v1, v2, defaultParams()); err == nil {
		t.Fatal("expected error when SEMPv1 fails")
	}
}

func TestHandle_SEMPv2ErrorFailsWhole(t *testing.T) {
	v1 := &mockV1{inner: fixtureXML(t)}
	v2 := &mockV2{err: errors.New("boom")}
	if _, err := callHandle(t, v1, v2, defaultParams()); err == nil {
		t.Fatal("expected error when SEMPv2 fails")
	}
}

func TestHandle_MissingParams(t *testing.T) {
	v1 := &mockV1{inner: fixtureXML(t)}
	v2 := &mockV2{data: sempv2Envelope()}
	for _, p := range []map[string]any{
		{"queueName": "q"},  // no msgVpnName
		{"msgVpnName": "v"}, // no queueName
		{"msgVpnName": "", "queueName": ""},
	} {
		if _, err := callHandle(t, v1, v2, p); err == nil {
			t.Errorf("expected error for params %v", p)
		}
	}
}

func TestMetadata(t *testing.T) {
	m := NewHandler().Metadata()
	if m.Name != toolName {
		t.Errorf("Name = %q, want %q", m.Name, toolName)
	}
	if !m.Annotations.ReadOnly {
		t.Error("expected ReadOnly annotation")
	}
	if !strings.Contains(m.Description, "currentMsgCount") {
		t.Error("description should mention currentMsgCount as the authoritative depth")
	}
}
