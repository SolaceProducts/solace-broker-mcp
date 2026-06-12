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

package brokerstatus

import "testing"

// TestSpoolResponse_RoundTrip decodes the live show-message-spool-detail
// fixture into messageSpoolResponse, marshals to JSON, and asserts the
// curated HA / utilization / failure fields carry through. Most spool
// utilization values arrive as strings (e.g. "0.00") because the broker
// emits trailing-zero decimals as strings — the curated set keeps them as
// pointer-to-string for fidelity.
func TestSpoolResponse_RoundTrip(t *testing.T) {
	inner := extractInnerRPC(t, "testdata/show_message_spool_detail.xml")

	var resp messageSpoolResponse
	out := decodeAndMarshal(t, inner, "message-spool", &resp)

	info, ok := out["messageSpoolInfo"].(map[string]any)
	if !ok {
		t.Fatalf("messageSpoolInfo not a map: %T", out["messageSpoolInfo"])
	}

	// Spot-check HA / operational state.
	for k, want := range map[string]any{
		"configStatus":          "Enabled (Primary)",
		"operationalStatus":     "AD-Active",
		"datapathUp":            "true",
		"synchronizationStatus": "Synced",
		"spoolSyncStatus":       "Synced",
	} {
		if got := info[k]; got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}

	// Utilization percentages and quotas.
	if got := info["activeDiskPartitionUsage"]; got != "42.22" {
		t.Errorf("activeDiskPartitionUsage = %v, want %q", got, "42.22")
	}
	if got := info["maxMessageCount"]; got != "100M" {
		t.Errorf("maxMessageCount = %v, want %q (the broker emits unit-bearing strings)", got, "100M")
	}
	if got := info["totalMessagesCurrentlySpooled"]; got != float64(0) {
		t.Errorf("totalMessagesCurrentlySpooled = %v, want 0", got)
	}

	// Recent failure context — broker emitted last-failure-reason but not
	// last-failure-time on the standalone VMR. omitempty must drop the
	// missing field.
	if got, present := info["lastFailureReason"]; !present {
		t.Error("lastFailureReason should be present (broker emitted it)")
	} else if got != "N/A" {
		t.Errorf("lastFailureReason = %v, want %q", got, "N/A")
	}
	if _, present := info["lastFailureTime"]; present {
		t.Error("lastFailureTime should be absent (broker did not emit it)")
	}

	// Defragmentation field.
	if got := info["defragEstFragmentationPercentage"]; got != float64(0) {
		t.Errorf("defragEstFragmentationPercentage = %v, want 0", got)
	}

	// Uncurated branches must not appear.
	for _, dropped := range []string{"messages", "messageSpoolStats", "messageSpoolRates", "messageVpn"} {
		if _, present := out[dropped]; present {
			t.Errorf("uncurated field %q should not appear in JSON output", dropped)
		}
	}
}
