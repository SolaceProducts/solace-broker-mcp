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
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

// These tests reuse getReplicationStatusTool from executor_vpn_test.go (same
// package), which already mirrors the YAML definition.
//
// They differ from TestExecute_GetReplicationStatus_ReturnsData there in two
// deliberate ways. That test flattens the attributes directly into
// sempv2.Result.Data and reads them off result["replication"]; a real SEMPv2
// body nests them under a "data" key alongside "links" and "meta", which is the
// shape used below. And that test's payload is a fully healthy sync-mode VPN
// with every flag true — including replicationBridgeUp, which a broker only
// sends when the local replication bridge is operational.

// degradedReplicationPayload is a scrubbed capture (SOL-151996) from the active
// member of a real HA pair whose Message VPN has DR replication configured and
// currently broken. Names are synthetic; every numeric and boolean value is as
// the broker sent it.
//
// The important property is what is NOT here. The tool's select requests 18
// attributes; the broker returned these 16. It omitted replicationBridgeUp and
// replicationBridgeBoundToQueue — the two describing the LOCAL replication
// bridge — because that bridge was not operational at capture time.
//
// This is not version skew and not a select typo. SEMPv2's select is strict: an
// unknown attribute name is rejected with HTTP 400 "not a valid attribute", and
// both of these were accepted with 200. The broker simply omits replication
// attributes that do not apply to the VPN's current state — on a
// non-replicated VPN even replicationSyncEligible and replicationRemoteBridgeUp
// disappear, while replicationRole still reports.
func degradedReplicationPayload() map[string]any {
	return map[string]any{
		"data": map[string]any{
			"msgVpnName": "test-dr-active-vpn",
			"replicationAckPropagationIntervalMsgCount":        20,
			"replicationActiveAsyncQueuedMsgCount":             337791,
			"replicationActiveSyncEligiblePeakTime":            0,
			"replicationActiveSyncIneligiblePeakTime":          691,
			"replicationActiveSyncQueuedAsAsyncMsgCount":       0,
			"replicationActiveSyncQueuedMsgCount":              0,
			"replicationActiveTransitionToSyncIneligibleCount": 1,
			"replicationEnabled":                               true,
			"replicationQueueBound":                            false,
			"replicationQueueMaxMsgSpoolUsage":                 60000,
			"replicationRejectMsgWhenSyncIneligibleEnabled":    false,
			"replicationRemoteBridgeUp":                        false,
			"replicationRole":                                  "active",
			"replicationSyncEligible":                          false,
			"replicationTransactionMode":                       "async",
		},
	}
}

// TestExecute_GetReplicationStatus_DegradedActiveRole covers the state the tool
// actually exists to diagnose, which no fixture in this repo previously reached.
// The e2e-llm scenario (test/e2e-llm/scenarios/read-get-replication-status.json)
// documents its own fixture as two standalone brokers with no replication
// configured, and asserts the model reports replication is OFF — so the
// enabled-but-broken path had no coverage anywhere.
//
// Every failure signal here is non-default: role active, replication enabled,
// sync ineligible, remote bridge down, replication queue unbound, and a
// non-zero replicationActiveTransitionToSyncIneligibleCount — the field the tool
// description singles out as the best "has replication been flaky?" indicator,
// since SEMP exposes no current lagSeconds.
func TestExecute_GetReplicationStatus_DegradedActiveRole(t *testing.T) {
	client := newMockClient()
	client.responses["getMsgVpn"] = &sempv2.Result{
		Data:       degradedReplicationPayload(),
		StatusCode: 200,
	}

	executor := NewCompositeExecutor(testOperations())

	res, err := executor.Execute(context.Background(), getReplicationStatusTool(), client,
		map[string]any{"msgVpnName": "test-dr-active-vpn"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	step, ok := res["replication"].(map[string]any)
	if !ok {
		t.Fatalf(`res["replication"] is not a map: %T`, res["replication"])
	}
	data, ok := step["data"].(map[string]any)
	if !ok {
		t.Fatalf(`replication.data is not a map: %T`, step["data"])
	}

	for _, f := range []struct {
		key  string
		want any
	}{
		{"replicationRole", "active"},
		{"replicationEnabled", true},
		{"replicationSyncEligible", false},
		{"replicationRemoteBridgeUp", false},
		{"replicationQueueBound", false},
		{"replicationTransactionMode", "async"},
		{"replicationActiveTransitionToSyncIneligibleCount", 1},
	} {
		if data[f.key] != f.want {
			t.Errorf("%s = %v (%T), want %v", f.key, data[f.key], data[f.key], f.want)
		}
	}
}

// TestExecute_GetReplicationStatus_OmittedFieldsStayAbsent pins that attributes
// the broker did not send do not materialize in the tool output.
//
// This matters because of what the tool promises. Its description offers "bridge
// status (local + remote)", but when the local replication bridge is not
// operational the broker sends only the remote half. Under the collect strategy
// the step body is passed through, so absent must stay absent: a zero-valued
// replicationBridgeUp:false would be indistinguishable from the broker actively
// reporting the local bridge as down — the single most misleading thing this
// tool could say, since "no data" and "confirmed down" call for different
// operator responses.
//
// If a future change starts decoding this payload into a struct with non-pointer
// bools (the same bug already fixed once in the SEMPv1 redundancy response), the
// omitted fields would appear as false and this test fails.
func TestExecute_GetReplicationStatus_OmittedFieldsStayAbsent(t *testing.T) {
	client := newMockClient()
	client.responses["getMsgVpn"] = &sempv2.Result{
		Data:       degradedReplicationPayload(),
		StatusCode: 200,
	}

	executor := NewCompositeExecutor(testOperations())

	res, err := executor.Execute(context.Background(), getReplicationStatusTool(), client,
		map[string]any{"msgVpnName": "test-dr-active-vpn"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data := res["replication"].(map[string]any)["data"].(map[string]any)

	for _, key := range []string{"replicationBridgeUp", "replicationBridgeBoundToQueue"} {
		if v, present := data[key]; present {
			t.Errorf("%s is present with value %v, want absent — the broker did not send it, "+
				"and a false here reads as a confirmed-down local bridge", key, v)
		}
	}

	// The remote half IS reported, so its presence must be preserved rather
	// than dropped alongside the absent local fields.
	if v, present := data["replicationRemoteBridgeUp"]; !present {
		t.Error("replicationRemoteBridgeUp is absent, want present with value false")
	} else if v != false {
		t.Errorf("replicationRemoteBridgeUp = %v, want false", v)
	}

	// Guard the count too: exactly the 16 attributes the broker sent.
	if len(data) != 16 {
		t.Errorf("replication.data has %d fields, want 16 (the attributes the broker actually returned); got keys %v",
			len(data), keysOf(data))
	}
}

// keysOf returns a map's keys, for failure messages that need to show what was
// present rather than only the count.
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
