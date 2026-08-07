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

package redundancy

import (
	"encoding/json"
	"encoding/xml"
	"os"
	"strings"
	"testing"
)

// TestRedundancyResponse_RoundTrip uses a captured live-broker SEMPv1
// response (testdata/show_redundancy_standalone.xml) to verify the
// redundancyResponse struct decodes every documented field. The fixture
// is from a standalone broker (redundancy disabled), so values are in
// their disabled state — what matters is field presence, not values.
func TestResponse_RoundTrip(t *testing.T) {
	body, err := os.ReadFile("testdata/show_redundancy_standalone.xml")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	// The fixture wraps <redundancy> inside
	// <rpc-reply><rpc><show>...</show></rpc></rpc-reply>. For W4 we focus on
	// decoding the redundancyResponse struct itself, so we wrap it in a small
	// outer struct that matches the envelope path. W6's handler will use
	// parseReply to strip the envelope before decoding, so the inner-only
	// path is exercised end-to-end there.
	var wrapper struct {
		XMLName    xml.Name           `xml:"rpc-reply"`
		Redundancy redundancyResponse `xml:"rpc>show>redundancy"`
	}
	if err := xml.Unmarshal(body, &wrapper); err != nil {
		t.Fatalf("XML unmarshal failed: %v", err)
	}

	parsed := wrapper.Redundancy

	// Spot-check several fields including each Story 8-required field, to
	// prove the struct tags map correctly across all nesting depths.
	if parsed.ConfigStatus != "Shutdown" {
		t.Errorf("ConfigStatus = %q, want %q", parsed.ConfigStatus, "Shutdown")
	}
	if parsed.RedundancyStatus != "Down" {
		t.Errorf("RedundancyStatus = %q, want %q", parsed.RedundancyStatus, "Down")
	}
	if parsed.RedundancyMode != "N/A" {
		t.Errorf("RedundancyMode = %q, want %q", parsed.RedundancyMode, "N/A")
	}
	if parsed.ActiveStandbyRole != "Primary" {
		t.Errorf("ActiveStandbyRole = %q, want %q", parsed.ActiveStandbyRole, "Primary")
	}
	if parsed.SwitchoverMechanism != "Hostlist" {
		t.Errorf("SwitchoverMechanism = %q, want %q", parsed.SwitchoverMechanism, "Hostlist")
	}
	if parsed.FailoverCriteria != "any-fail" {
		t.Errorf("FailoverCriteria = %q, want %q", parsed.FailoverCriteria, "any-fail")
	}
	if parsed.OperStatus.ADBLinkUp != false {
		t.Errorf("OperStatus.ADBLinkUp = %v, want false", parsed.OperStatus.ADBLinkUp)
	}
	if parsed.OperStatus.ADBHelloUp != false {
		t.Errorf("OperStatus.ADBHelloUp = %v, want false", parsed.OperStatus.ADBHelloUp)
	}
	if parsed.VirtualRouters.Primary.Status.Activity != "Local Active" {
		t.Errorf("VirtualRouters.Primary.Status.Activity = %q, want %q",
			parsed.VirtualRouters.Primary.Status.Activity, "Local Active")
	}
	if parsed.VirtualRouters.Backup.Status.Activity != "Shutdown" {
		t.Errorf("VirtualRouters.Backup.Status.Activity = %q, want %q",
			parsed.VirtualRouters.Backup.Status.Activity, "Shutdown")
	}
	if len(parsed.VRRPInterfaces) != 1 {
		t.Errorf("VRRPInterfaces count = %d, want 1", len(parsed.VRRPInterfaces))
	} else if parsed.VRRPInterfaces[0].Name != "intf0" {
		t.Errorf("VRRPInterfaces[0].Name = %q, want %q", parsed.VRRPInterfaces[0].Name, "intf0")
	}

	// Round-trip via JSON to verify json: tags produce the expected camelCase
	// keys and the marshal step doesn't lose data.
	asJSON, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("JSON marshal failed: %v", err)
	}
	jsonStr := string(asJSON)

	requiredKeys := []string{
		`"configStatus"`, `"redundancyStatus"`, `"operatingMode"`,
		`"switchoverMechanism"`, `"autoRevert"`, `"redundancyMode"`,
		`"activeStandbyRole"`, `"mateRouterName"`, `"operStatus"`,
		`"adbLinkUp"`, `"adbHelloUp"`, `"failoverCriteria"`,
		`"vrrpInterfaces"`, `"virtualRouters"`, `"primary"`, `"backup"`,
		`"activity"`,
	}
	for _, key := range requiredKeys {
		if !strings.Contains(jsonStr, key) {
			t.Errorf("JSON output missing key %s", key)
		}
	}

	// Verify the omitempty behavior on backup-role status: the wire
	// response includes only <activity> for the backup, so the JSON should
	// NOT contain the other status fields under backup. If any of these
	// appear, the struct is inventing data the broker never sent (the bug
	// we fixed by switching to pointer + omitempty).
	backupJSON, err := json.Marshal(parsed.VirtualRouters.Backup.Status)
	if err != nil {
		t.Fatalf("marshalling backup status: %v", err)
	}
	backupStr := string(backupJSON)
	mustNotContain := []string{
		`"vrrp"`,
		`"vrrpInterfaces"`,
		`"routingInterface"`,
		`"vrrpPriority"`,
		`"priorityReportedByMate"`,
	}
	for _, key := range mustNotContain {
		if strings.Contains(backupStr, key) {
			t.Errorf("backup status JSON should not contain %s (broker did not emit it); got: %s",
				key, backupStr)
		}
	}
	if !strings.Contains(backupStr, `"activity"`) {
		t.Errorf("backup status JSON should still contain activity; got: %s", backupStr)
	}
}
