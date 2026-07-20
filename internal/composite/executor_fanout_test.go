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
	"sync/atomic"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
)

// vpnRows builds a parent step result with the given (name, enabled) rows,
// matching the shape a `getMsgVpns` step produces after `applySelect`.
func vpnRows(rows ...map[string]any) map[string]any {
	data := make([]any, 0, len(rows))
	for _, r := range rows {
		data = append(data, r)
	}
	return map[string]any{"data": data}
}

func fanOutTool() CompositeTool {
	return CompositeTool{
		Name:        "fan-out-test",
		Description: "fan-out over vpns",
		Steps: []Step{
			{ID: "vpns", Operation: "monitor/getMsgVpns"},
			{
				ID:         "clients",
				Operation:  "monitor/getMsgVpnClients",
				ForEach:    "vpns",
				ForEachKey: "msgVpnName",
				Args: map[string]string{
					"msgVpnName": "{{.Item.msgVpnName}}",
				},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

// TestFanOut_Basic: parent has 3 rows → executor issues 3 per-row SEMP calls
// and returns a result keyed by ForEachKey. Fan-out shape is stable so the
// list-vpns handler can walk `byKey[vpnName]`.
func TestFanOut_Basic(t *testing.T) {
	client := newMockClient()
	client.responses["getMsgVpns"] = &sempv2.Result{
		Data: vpnRows(
			map[string]any{"msgVpnName": "a"},
			map[string]any{"msgVpnName": "b"},
			map[string]any{"msgVpnName": "c"},
		),
	}
	client.responses["getMsgVpnClients"] = &sempv2.Result{
		Data: map[string]any{"data": []any{map[string]any{"clientName": "x"}}},
	}

	ce := NewCompositeExecutor(testOperations())
	out, err := ce.Execute(context.Background(), fanOutTool(), client, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	clients, ok := out["clients"].(map[string]any)
	if !ok {
		t.Fatalf("clients step result missing or wrong type: %T", out["clients"])
	}
	byKey, ok := clients["byKey"].(map[string]any)
	if !ok {
		t.Fatalf("byKey missing or wrong type: %T", clients["byKey"])
	}
	for _, k := range []string{"a", "b", "c"} {
		if _, present := byKey[k]; !present {
			t.Errorf("byKey missing key %q; got keys %v", k, byKey)
		}
	}
	// One call to getMsgVpns + one per row.
	got := 0
	for _, c := range client.calls {
		if c == "getMsgVpnClients" {
			got++
		}
	}
	if got != 3 {
		t.Errorf("getMsgVpnClients call count: got %d, want 3", got)
	}
}

// TestFanOut_ForEachIf_SkipsFalsyRows: an enabled=false row must be skipped
// and tallied under "skipped"; no SEMP call is issued for it. This is what
// list-vpns will lean on to avoid calling getMsgVpnClients on disabled VPNs.
func TestFanOut_ForEachIf_SkipsFalsyRows(t *testing.T) {
	client := newMockClient()
	client.responses["getMsgVpns"] = &sempv2.Result{
		Data: vpnRows(
			map[string]any{"msgVpnName": "on1", "enabled": true},
			map[string]any{"msgVpnName": "off1", "enabled": false},
			map[string]any{"msgVpnName": "on2", "enabled": true},
		),
	}
	tool := fanOutTool()
	tool.Steps[1].ForEachIf = "{{.Item.enabled}}"

	ce := NewCompositeExecutor(testOperations())
	out, err := ce.Execute(context.Background(), tool, client, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	clients := out["clients"].(map[string]any)
	byKey := clients["byKey"].(map[string]any)
	if len(byKey) != 2 {
		t.Errorf("byKey size: got %d, want 2 (only enabled rows)", len(byKey))
	}
	if _, present := byKey["off1"]; present {
		t.Errorf("off1 should not have been called")
	}
	if clients["skipped"] != 1 {
		t.Errorf("skipped: got %v, want 1", clients["skipped"])
	}
	got := 0
	for _, c := range client.calls {
		if c == "getMsgVpnClients" {
			got++
		}
	}
	if got != 2 {
		t.Errorf("getMsgVpnClients call count: got %d, want 2 (skipped row must not call)", got)
	}
}

// TestFanOut_ForEachIf_NonBoolIsError: a template that resolves to anything
// ParseBool won't accept must fail loud — silently treating "yes"/"no" or "1.0"
// as boolean would let a bad predicate ship undetected.
func TestFanOut_ForEachIf_NonBoolIsError(t *testing.T) {
	client := newMockClient()
	client.responses["getMsgVpns"] = &sempv2.Result{
		Data: vpnRows(map[string]any{"msgVpnName": "a", "enabled": "sometimes"}),
	}
	tool := fanOutTool()
	tool.Steps[1].ForEachIf = "{{.Item.enabled}}"

	ce := NewCompositeExecutor(testOperations())
	_, err := ce.Execute(context.Background(), tool, client, nil)
	if err == nil {
		t.Fatal("expected error on non-bool forEachIf, got nil")
	}
	if !contains(err.Error(), "forEachIf must resolve to a bool literal") {
		t.Errorf("expected bool-literal error, got: %v", err)
	}
}

// TestFanOut_EmptyParent: no rows → no SEMP calls, empty byKey, no error.
// Legal case: a broker with zero VPNs matching some filter shouldn't crash.
func TestFanOut_EmptyParent(t *testing.T) {
	client := newMockClient()
	client.responses["getMsgVpns"] = &sempv2.Result{Data: map[string]any{"data": []any{}}}

	ce := NewCompositeExecutor(testOperations())
	out, err := ce.Execute(context.Background(), fanOutTool(), client, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	clients := out["clients"].(map[string]any)
	byKey := clients["byKey"].(map[string]any)
	if len(byKey) != 0 {
		t.Errorf("byKey should be empty, got %v", byKey)
	}
	for _, c := range client.calls {
		if c == "getMsgVpnClients" {
			t.Errorf("no getMsgVpnClients call should fire on empty parent")
		}
	}
}

// TestFanOut_DuplicateKeyIsError: two parent rows sharing the same ForEachKey
// value would silently overwrite in byKey under last-writer-wins. Surface it
// as a hard error so a broken tool definition (or an unexpected broker
// response) fails loud rather than dropping a per-row result.
func TestFanOut_DuplicateKeyIsError(t *testing.T) {
	client := newMockClient()
	client.responses["getMsgVpns"] = &sempv2.Result{
		Data: vpnRows(
			map[string]any{"msgVpnName": "a"},
			map[string]any{"msgVpnName": "a"},
		),
	}
	ce := NewCompositeExecutor(testOperations())
	_, err := ce.Execute(context.Background(), fanOutTool(), client, nil)
	if err == nil {
		t.Fatal("expected duplicate-key error, got nil")
	}
	if !contains(err.Error(), `duplicate forEachKey "msgVpnName"="a"`) {
		t.Errorf("expected duplicate-key error, got: %v", err)
	}
}

// TestFanOut_ParentDataWrongType: parent step returning a non-list `data`
// field is a broken tool definition. Previously this silently produced an
// empty byKey; now it must error so the misconfiguration surfaces.
func TestFanOut_ParentDataWrongType(t *testing.T) {
	client := newMockClient()
	client.responses["getMsgVpns"] = &sempv2.Result{
		Data: map[string]any{"data": "not-a-list"},
	}
	ce := NewCompositeExecutor(testOperations())
	_, err := ce.Execute(context.Background(), fanOutTool(), client, nil)
	if err == nil {
		t.Fatal("expected error on non-list parent data, got nil")
	}
	if !contains(err.Error(), `forEach parent "vpns" data: want []any`) {
		t.Errorf("expected data-type error, got: %v", err)
	}
}

// TestFanOut_FailFast: one per-row error must abort the whole fan-out. The
// story locks fail-fast semantics; if this test starts passing without an
// error, we've silently drifted to best-effort and lost the guarantee.
func TestFanOut_FailFast(t *testing.T) {
	client := newMockClient()
	client.responses["getMsgVpns"] = &sempv2.Result{
		Data: vpnRows(
			map[string]any{"msgVpnName": "ok1"},
			map[string]any{"msgVpnName": "boom"},
			map[string]any{"msgVpnName": "ok2"},
		),
	}
	client.errors["getMsgVpnClients"] = fmt.Errorf("simulated broker error")

	ce := NewCompositeExecutor(testOperations())
	_, err := ce.Execute(context.Background(), fanOutTool(), client, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !contains(err.Error(), "simulated broker error") {
		t.Errorf("expected wrapped simulated error, got: %v", err)
	}
}

// TestFanOut_ConcurrencyCap: with Concurrency=2 and 5 rows, at most 2 in-flight
// per-row calls must exist at any moment. A barrier client parks calls until
// released so the peak inflight count is observable. Ordering isn't asserted;
// only the cap.
func TestFanOut_ConcurrencyCap(t *testing.T) {
	client := newBarrierClient()
	for i := range 5 {
		client.rows = append(client.rows, map[string]any{"msgVpnName": fmt.Sprintf("v%d", i)})
	}

	tool := fanOutTool()
	tool.Steps[1].Concurrency = 2

	ce := NewCompositeExecutor(testOperations())
	done := make(chan error, 1)
	go func() {
		_, err := ce.Execute(context.Background(), tool, client, nil)
		done <- err
	}()

	// Release calls one at a time, letting the executor drain and refill the
	// semaphore. If the cap were broken we'd see peak > 2.
	for range 5 {
		client.releaseOne()
	}

	if err := <-done; err != nil {
		t.Fatalf("execute: %v", err)
	}
	if peak := client.peakInflight(); peak > 2 {
		t.Errorf("concurrency cap breached: peak inflight was %d, want <=2", peak)
	}
}

// TestFanOut_ItemAvailableInTemplate: templates referencing .Item.<field> get
// the parent row's value substituted. If .Item isn't wired we'd see the
// literal "<no value>" or a template error.
func TestFanOut_ItemAvailableInTemplate(t *testing.T) {
	recorded := []callRecord{}
	var mu sync.Mutex
	inner := newMockClient()
	inner.responses["getMsgVpns"] = &sempv2.Result{
		Data: vpnRows(
			map[string]any{"msgVpnName": "aaa"},
			map[string]any{"msgVpnName": "bbb"},
		),
	}
	cap := &argCapturingClient{inner: inner, recorded: &recorded, mu: &mu}

	ce := NewCompositeExecutor(testOperations())
	_, err := ce.Execute(context.Background(), fanOutTool(), cap, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := map[string]bool{}
	for _, r := range recorded {
		if r.opID != "getMsgVpnClients" {
			continue
		}
		name, _ := r.args["msgVpnName"].(string)
		got[name] = true
	}
	for _, want := range []string{"aaa", "bbb"} {
		if !got[want] {
			t.Errorf("no getMsgVpnClients call saw msgVpnName=%q; recorded: %+v", want, got)
		}
	}
}

// TestFanOut_MissingParentStep: fan-out step referencing a parent that never
// ran must error with a clear message. Loader should have prevented this;
// this defends the runtime as a last line of defence.
func TestFanOut_MissingParentStep(t *testing.T) {
	tool := CompositeTool{
		Name: "orphan", Description: "d",
		Steps: []Step{
			{
				ID: "clients", Operation: "monitor/getMsgVpnClients",
				ForEach: "missing", ForEachKey: "msgVpnName",
				Args: map[string]string{"msgVpnName": "{{.Item.msgVpnName}}"},
			},
		},
		Result: ResultStrategy{Strategy: "collect"},
	}
	ce := NewCompositeExecutor(testOperations())
	_, err := ce.Execute(context.Background(), tool, newMockClient(), nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !contains(err.Error(), `forEach step "missing"`) {
		t.Errorf("expected error naming missing parent, got: %v", err)
	}
}

// TestFanOut_MissingKeyOnRow: parent row lacks the ForEachKey field. The
// executor cannot key the result and must fail rather than drop the row.
func TestFanOut_MissingKeyOnRow(t *testing.T) {
	client := newMockClient()
	client.responses["getMsgVpns"] = &sempv2.Result{
		Data: vpnRows(map[string]any{"other": "x"}),
	}
	ce := NewCompositeExecutor(testOperations())
	_, err := ce.Execute(context.Background(), fanOutTool(), client, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !contains(err.Error(), `forEachKey "msgVpnName" missing`) {
		t.Errorf("expected error on missing key, got: %v", err)
	}
}

// barrierClient parks each getMsgVpnClients call on a channel until released.
// Tracks concurrent inflight count so tests can assert the fan-out concurrency
// cap. getMsgVpns returns its configured rows directly; only per-row calls block.
type barrierClient struct {
	inflight int32
	peak     int32
	rows     []map[string]any
	release  chan struct{}
}

func newBarrierClient() *barrierClient {
	return &barrierClient{release: make(chan struct{}, 128)}
}

func (b *barrierClient) releaseOne() { b.release <- struct{}{} }

func (b *barrierClient) peakInflight() int32 { return atomic.LoadInt32(&b.peak) }

func (b *barrierClient) Execute(ctx context.Context, op *sempv2.Operation, _ map[string]any) (*sempv2.Result, error) {
	if op.ID == "getMsgVpns" {
		items := make([]any, 0, len(b.rows))
		for _, r := range b.rows {
			items = append(items, r)
		}
		return &sempv2.Result{Data: map[string]any{"data": items}}, nil
	}
	n := atomic.AddInt32(&b.inflight, 1)
	for {
		p := atomic.LoadInt32(&b.peak)
		if n <= p || atomic.CompareAndSwapInt32(&b.peak, p, n) {
			break
		}
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		atomic.AddInt32(&b.inflight, -1)
		return nil, ctx.Err()
	}
	atomic.AddInt32(&b.inflight, -1)
	return &sempv2.Result{Data: map[string]any{"data": []any{}}}, nil
}
