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

// decodeFixture reads a full <rpc-reply> fixture and decodes the
// <redundancy> subtree into a redundancyResponse.
//
// The fixtures wrap <redundancy> inside
// <rpc-reply><rpc><show>...</show></rpc></rpc-reply>, so we decode through a
// small outer struct matching the envelope path. At runtime the handler uses
// parseReply to strip the envelope before decoding, so the inner-only path is
// exercised end-to-end in the handler tests instead.
func decodeFixture(t *testing.T, path string) redundancyResponse {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", path, err)
	}

	var wrapper struct {
		XMLName    xml.Name           `xml:"rpc-reply"`
		Redundancy redundancyResponse `xml:"rpc>show>redundancy"`
	}
	if err := xml.Unmarshal(body, &wrapper); err != nil {
		t.Fatalf("XML unmarshal of %s failed: %v", path, err)
	}
	return wrapper.Redundancy
}

// TestRedundancyResponse_RoundTrip uses a captured live-broker SEMPv1
// response (testdata/show_redundancy_standalone.xml) to verify the
// redundancyResponse struct decodes every documented field. The fixture
// is from a standalone broker (redundancy disabled), so values are in
// their disabled state — what matters is field presence, not values.
// TestResponse_ActiveStandby_RoundTrip covers the HA-enabled values.
func TestResponse_RoundTrip(t *testing.T) {
	parsed := decodeFixture(t, "testdata/show_redundancy_standalone.xml")

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

// TestResponse_ActiveStandby_RoundTrip covers the HA-enabled parse paths that
// the standalone fixture leaves in their disabled state: redundancy up in
// Active/Standby mode, both mate-link flags true, a populated mate-router-name,
// and a priority-reported-by-mate subtree reporting the mate as Standby.
//
// Fixture is a scrubbed capture from the active member of a real HA pair; see
// the provenance comment in testdata/show_redundancy_active_standby.xml.
func TestResponse_ActiveStandby_RoundTrip(t *testing.T) {
	parsed := decodeFixture(t, "testdata/show_redundancy_active_standby.xml")

	// Top-level HA state. Every value here differs from the standalone
	// fixture's, so a struct-tag regression on any of them fails this test
	// even though the other fixture still decodes.
	scalars := []struct {
		field string
		got   string
		want  string
	}{
		{"ConfigStatus", parsed.ConfigStatus, "Enabled"},
		{"RedundancyStatus", parsed.RedundancyStatus, "Up"},
		{"RedundancyMode", parsed.RedundancyMode, "Active/Standby"},
		{"ActiveStandbyRole", parsed.ActiveStandbyRole, "Primary"},
		{"OperatingMode", parsed.OperatingMode, "Message Routing Node"},
		{"SwitchoverMechanism", parsed.SwitchoverMechanism, "Hostlist"},
		{"FailoverCriteria", parsed.FailoverCriteria, "any-fail"},
		{"GroupManagementServerIdentity", parsed.GroupManagementServerIdentity, "testha01.messaging.example.com"},
		{"MateRouterName", parsed.MateRouterName, "testha01backup"},
	}
	for _, s := range scalars {
		if s.got != s.want {
			t.Errorf("%s = %q, want %q", s.field, s.got, s.want)
		}
	}
	if parsed.AutoRevert {
		t.Error("AutoRevert = true, want false")
	}

	// Mate-link flags: both true here, both false in the standalone fixture,
	// so this pins the bool decode in the true direction too.
	if !parsed.OperStatus.ADBLinkUp {
		t.Error("OperStatus.ADBLinkUp = false, want true")
	}
	if !parsed.OperStatus.ADBHelloUp {
		t.Error("OperStatus.ADBHelloUp = false, want true")
	}

	// Top-level VRRP interface list.
	if len(parsed.VRRPInterfaces) != 1 {
		t.Fatalf("VRRPInterfaces count = %d, want 1", len(parsed.VRRPInterfaces))
	}
	if got, want := parsed.VRRPInterfaces[0].Name, "intf0"; got != want {
		t.Errorf("VRRPInterfaces[0].Name = %q, want %q", got, want)
	}
	if got, want := parsed.VRRPInterfaces[0].StaticAddress, "198.51.100.10"; got != want {
		t.Errorf("VRRPInterfaces[0].StaticAddress = %q, want %q", got, want)
	}
	if got, want := parsed.VRRPInterfaces[0].StaticStatus, "Up"; got != want {
		t.Errorf("VRRPInterfaces[0].StaticStatus = %q, want %q", got, want)
	}

	// Primary virtual router: config, then the populated status subtree.
	primary := parsed.VirtualRouters.Primary
	if got, want := primary.Config.RoutingInterface, "intf0:1"; got != want {
		t.Errorf("Primary.Config.RoutingInterface = %q, want %q", got, want)
	}
	if got, want := primary.Config.VRRPVRID, "-1"; got != want {
		t.Errorf("Primary.Config.VRRPVRID = %q, want %q", got, want)
	}
	if got, want := primary.Status.Activity, "Local Active"; got != want {
		t.Errorf("Primary.Status.Activity = %q, want %q", got, want)
	}

	// Optional (pointer) status fields: present on the primary role, so each
	// must be non-nil and carry the wire value.
	if primary.Status.VRRP == nil {
		t.Error("Primary.Status.VRRP is nil, want \"Initialize\"")
	} else if got, want := *primary.Status.VRRP, "Initialize"; got != want {
		t.Errorf("Primary.Status.VRRP = %q, want %q", got, want)
	}
	if primary.Status.RoutingInterface == nil {
		t.Error("Primary.Status.RoutingInterface is nil, want \"Up\"")
	} else if got, want := *primary.Status.RoutingInterface, "Up"; got != want {
		t.Errorf("Primary.Status.RoutingInterface = %q, want %q", got, want)
	}
	if primary.Status.VRRPPriority == nil {
		t.Error("Primary.Status.VRRPPriority is nil, want 250")
	} else if got, want := *primary.Status.VRRPPriority, 250; got != want {
		t.Errorf("Primary.Status.VRRPPriority = %d, want %d", got, want)
	}
	if len(primary.Status.VRRPInterfaces) != 1 {
		t.Errorf("Primary.Status.VRRPInterfaces count = %d, want 1", len(primary.Status.VRRPInterfaces))
	} else {
		if got, want := primary.Status.VRRPInterfaces[0].Name, "intf0"; got != want {
			t.Errorf("Primary.Status.VRRPInterfaces[0].Name = %q, want %q", got, want)
		}
		if got, want := primary.Status.VRRPInterfaces[0].Status, "Initialize"; got != want {
			t.Errorf("Primary.Status.VRRPInterfaces[0].Status = %q, want %q", got, want)
		}
	}

	// priority-reported-by-mate: the mate reports itself Standby. In the
	// standalone fixture every one of these is "None", so these assertions
	// only pass against a real HA capture.
	mate := primary.Status.PriorityReportedByMate
	if mate == nil {
		t.Fatal("Primary.Status.PriorityReportedByMate is nil, want a populated subtree")
	}
	mateFields := []struct {
		field string
		got   *string
		want  string
	}{
		{"Summary", mate.Summary, "Standby"},
		{"CSPF", mate.CSPF, "None"},
		{"ADBHello", mate.ADBHello, "Standby"},
		{"VRRP", mate.VRRP, "Standby (100)"},
	}
	for _, f := range mateFields {
		if f.got == nil {
			t.Errorf("PriorityReportedByMate.%s is nil, want %q", f.field, f.want)
			continue
		}
		if *f.got != f.want {
			t.Errorf("PriorityReportedByMate.%s = %q, want %q", f.field, *f.got, f.want)
		}
	}
	if len(mate.VRRPInterfaces) != 1 {
		t.Errorf("PriorityReportedByMate.VRRPInterfaces count = %d, want 1", len(mate.VRRPInterfaces))
	} else {
		if got, want := mate.VRRPInterfaces[0].Name, "intf0"; got != want {
			t.Errorf("PriorityReportedByMate.VRRPInterfaces[0].Name = %q, want %q", got, want)
		}
		if got, want := mate.VRRPInterfaces[0].Priority, "Standby (100)"; got != want {
			t.Errorf("PriorityReportedByMate.VRRPInterfaces[0].Priority = %q, want %q", got, want)
		}
	}

	// Backup virtual router: captured from the active member, so the broker
	// emits only <activity> for it. Config still decodes, and the optional
	// status fields must stay nil rather than materializing as zero values.
	backup := parsed.VirtualRouters.Backup
	if got, want := backup.Config.RoutingInterface, "intf0:1"; got != want {
		t.Errorf("Backup.Config.RoutingInterface = %q, want %q", got, want)
	}
	if got, want := backup.Status.Activity, "Shutdown"; got != want {
		t.Errorf("Backup.Status.Activity = %q, want %q", got, want)
	}
	if backup.Status.VRRP != nil {
		t.Errorf("Backup.Status.VRRP = %q, want nil (broker did not emit it)", *backup.Status.VRRP)
	}
	if backup.Status.RoutingInterface != nil {
		t.Errorf("Backup.Status.RoutingInterface = %q, want nil (broker did not emit it)",
			*backup.Status.RoutingInterface)
	}
	if backup.Status.VRRPPriority != nil {
		t.Errorf("Backup.Status.VRRPPriority = %d, want nil (broker did not emit it)",
			*backup.Status.VRRPPriority)
	}
	if backup.Status.PriorityReportedByMate != nil {
		t.Error("Backup.Status.PriorityReportedByMate is non-nil, want nil (broker did not emit it)")
	}
	if backup.Status.VRRPInterfaces != nil {
		t.Errorf("Backup.Status.VRRPInterfaces = %v, want nil (broker did not emit it)",
			backup.Status.VRRPInterfaces)
	}

	// JSON round-trip: the HA values must survive marshalling, and the
	// backup's omitted fields must stay absent from the output.
	asJSON, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("JSON marshal failed: %v", err)
	}
	jsonStr := string(asJSON)
	for _, want := range []string{
		`"redundancyStatus":"Up"`,
		`"redundancyMode":"Active/Standby"`,
		`"mateRouterName":"testha01backup"`,
		`"adbLinkUp":true`,
		`"adbHelloUp":true`,
		`"priorityReportedByMate"`,
		`"summary":"Standby"`,
	} {
		if !strings.Contains(jsonStr, want) {
			t.Errorf("JSON output missing %s; got: %s", want, jsonStr)
		}
	}

	backupJSON, err := json.Marshal(backup.Status)
	if err != nil {
		t.Fatalf("marshalling backup status: %v", err)
	}
	backupStr := string(backupJSON)
	for _, key := range []string{
		`"vrrp"`, `"vrrpInterfaces"`, `"routingInterface"`,
		`"vrrpPriority"`, `"priorityReportedByMate"`,
	} {
		if strings.Contains(backupStr, key) {
			t.Errorf("backup status JSON should not contain %s (broker did not emit it); got: %s",
				key, backupStr)
		}
	}
	if got, want := backupStr, `{"activity":"Shutdown"}`; got != want {
		t.Errorf("backup status JSON = %s, want %s", got, want)
	}
}

// TestResponse_Released_RoundTrip covers the degraded HA state: a pair whose
// Primary has released activity, captured from the Backup member that took
// over. Three parse properties are unique to this fixture:
//
//   - redundancy-status Down while both mate-link flags are still true. The
//     standalone fixture is Down with both false, so only this fixture proves
//     group health and mate-link health decode independently.
//   - active-standby-role Backup, and with it a populated <backup> status
//     subtree against a bare-<activity> <primary> — the exact mirror of
//     show_redundancy_active_standby.xml. Together the two fixtures pin
//     omitempty on both roles instead of only ever on the backup.
//   - priority-reported-by-mate reporting "Release", an enum value no other
//     fixture carries.
//
// See the provenance comment in testdata/show_redundancy_released.xml for how
// the state was induced and reverted.
func TestResponse_Released_RoundTrip(t *testing.T) {
	parsed := decodeFixture(t, "testdata/show_redundancy_released.xml")

	scalars := []struct {
		field string
		got   string
		want  string
	}{
		{"ConfigStatus", parsed.ConfigStatus, "Enabled"},
		{"RedundancyStatus", parsed.RedundancyStatus, "Down"},
		{"RedundancyMode", parsed.RedundancyMode, "Active/Standby"},
		{"ActiveStandbyRole", parsed.ActiveStandbyRole, "Backup"},
		{"GroupManagementServerIdentity", parsed.GroupManagementServerIdentity, "testha01.messaging.example.com"},
		{"MateRouterName", parsed.MateRouterName, "testha01primary"},
	}
	for _, s := range scalars {
		if s.got != s.want {
			t.Errorf("%s = %q, want %q", s.field, s.got, s.want)
		}
	}

	// The point of this fixture: redundancy is Down, yet the mate link is
	// healthy. A reader that derives either from the other is wrong, and only
	// this combination catches it.
	if parsed.RedundancyStatus != "Down" {
		t.Errorf("RedundancyStatus = %q, want %q", parsed.RedundancyStatus, "Down")
	}
	if !parsed.OperStatus.ADBLinkUp {
		t.Error("OperStatus.ADBLinkUp = false, want true (mate link is up even though redundancy is Down)")
	}
	if !parsed.OperStatus.ADBHelloUp {
		t.Error("OperStatus.ADBHelloUp = false, want true (mate link is up even though redundancy is Down)")
	}

	// Backup virtual router is the populated one here. Every optional pointer
	// field must be non-nil — the inverse of what the active-standby fixture
	// asserts about this same role.
	backup := parsed.VirtualRouters.Backup
	if got, want := backup.Status.Activity, "Local Active"; got != want {
		t.Errorf("Backup.Status.Activity = %q, want %q", got, want)
	}
	if backup.Status.VRRP == nil {
		t.Error("Backup.Status.VRRP is nil, want \"Initialize\"")
	} else if got, want := *backup.Status.VRRP, "Initialize"; got != want {
		t.Errorf("Backup.Status.VRRP = %q, want %q", got, want)
	}
	if backup.Status.RoutingInterface == nil {
		t.Error("Backup.Status.RoutingInterface is nil, want \"Up\"")
	} else if got, want := *backup.Status.RoutingInterface, "Up"; got != want {
		t.Errorf("Backup.Status.RoutingInterface = %q, want %q", got, want)
	}
	if backup.Status.VRRPPriority == nil {
		t.Error("Backup.Status.VRRPPriority is nil, want 240")
	} else if got, want := *backup.Status.VRRPPriority, 240; got != want {
		t.Errorf("Backup.Status.VRRPPriority = %d, want %d", got, want)
	}
	if len(backup.Status.VRRPInterfaces) != 1 {
		t.Errorf("Backup.Status.VRRPInterfaces count = %d, want 1", len(backup.Status.VRRPInterfaces))
	} else if got, want := backup.Status.VRRPInterfaces[0].Status, "Initialize"; got != want {
		t.Errorf("Backup.Status.VRRPInterfaces[0].Status = %q, want %q", got, want)
	}

	// The "Release" enum family, carried only by this fixture.
	mate := backup.Status.PriorityReportedByMate
	if mate == nil {
		t.Fatal("Backup.Status.PriorityReportedByMate is nil, want a populated subtree")
	}
	mateFields := []struct {
		field string
		got   *string
		want  string
	}{
		{"Summary", mate.Summary, "Release"},
		{"CSPF", mate.CSPF, "None"},
		{"ADBHello", mate.ADBHello, "Release"},
		{"VRRP", mate.VRRP, "Release (0)"},
	}
	for _, f := range mateFields {
		if f.got == nil {
			t.Errorf("PriorityReportedByMate.%s is nil, want %q", f.field, f.want)
			continue
		}
		if *f.got != f.want {
			t.Errorf("PriorityReportedByMate.%s = %q, want %q", f.field, *f.got, f.want)
		}
	}
	if len(mate.VRRPInterfaces) != 1 {
		t.Errorf("PriorityReportedByMate.VRRPInterfaces count = %d, want 1", len(mate.VRRPInterfaces))
	} else if got, want := mate.VRRPInterfaces[0].Priority, "Release (0)"; got != want {
		t.Errorf("PriorityReportedByMate.VRRPInterfaces[0].Priority = %q, want %q", got, want)
	}

	// Primary is now the sparse role: the broker sent only <activity>, so the
	// optional fields must stay nil. This is the mirror of the omitempty
	// assertion in TestResponse_ActiveStandby_RoundTrip, which checks the same
	// thing on the backup role.
	primary := parsed.VirtualRouters.Primary
	if got, want := primary.Status.Activity, "Shutdown"; got != want {
		t.Errorf("Primary.Status.Activity = %q, want %q", got, want)
	}
	if got, want := primary.Config.RoutingInterface, "intf0:1"; got != want {
		t.Errorf("Primary.Config.RoutingInterface = %q, want %q", got, want)
	}
	if primary.Status.VRRP != nil {
		t.Errorf("Primary.Status.VRRP = %q, want nil (broker did not emit it)", *primary.Status.VRRP)
	}
	if primary.Status.RoutingInterface != nil {
		t.Errorf("Primary.Status.RoutingInterface = %q, want nil (broker did not emit it)",
			*primary.Status.RoutingInterface)
	}
	if primary.Status.VRRPPriority != nil {
		t.Errorf("Primary.Status.VRRPPriority = %d, want nil (broker did not emit it)",
			*primary.Status.VRRPPriority)
	}
	if primary.Status.PriorityReportedByMate != nil {
		t.Error("Primary.Status.PriorityReportedByMate is non-nil, want nil (broker did not emit it)")
	}
	if primary.Status.VRRPInterfaces != nil {
		t.Errorf("Primary.Status.VRRPInterfaces = %v, want nil (broker did not emit it)",
			primary.Status.VRRPInterfaces)
	}

	// JSON round-trip, and the mirrored omitempty check: it is the PRIMARY
	// status that must marshal to activity alone here.
	primaryJSON, err := json.Marshal(primary.Status)
	if err != nil {
		t.Fatalf("marshalling primary status: %v", err)
	}
	if got, want := string(primaryJSON), `{"activity":"Shutdown"}`; got != want {
		t.Errorf("primary status JSON = %s, want %s", got, want)
	}

	asJSON, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("JSON marshal failed: %v", err)
	}
	jsonStr := string(asJSON)
	for _, want := range []string{
		`"redundancyStatus":"Down"`,
		`"activeStandbyRole":"Backup"`,
		`"mateRouterName":"testha01primary"`,
		`"adbLinkUp":true`,
		`"adbHelloUp":true`,
		`"summary":"Release"`,
	} {
		if !strings.Contains(jsonStr, want) {
			t.Errorf("JSON output missing %s; got: %s", want, jsonStr)
		}
	}
}

// TestResponse_BackupActive_RoundTrip pins the one thing
// show_redundancy_released.xml cannot: that a populated <backup> subtree is
// independent of group health. Here the Backup member is active and the pair is
// fully healthy (redundancy-status Up, both mate-link flags true), which is the
// normal steady state after a release-and-revert with auto-revert false.
//
// Read alongside TestResponse_Released_RoundTrip: the two fixtures differ on
// redundancy-status and on the priority-reported-by-mate enum, and agree on
// everything else. Anything treating "activity on the backup role" as
// inherently degraded passes one and fails the other.
func TestResponse_BackupActive_RoundTrip(t *testing.T) {
	parsed := decodeFixture(t, "testdata/show_redundancy_backup_active.xml")

	if got, want := parsed.ActiveStandbyRole, "Backup"; got != want {
		t.Errorf("ActiveStandbyRole = %q, want %q", got, want)
	}
	if got, want := parsed.RedundancyStatus, "Up"; got != want {
		t.Errorf("RedundancyStatus = %q, want %q", got, want)
	}
	if !parsed.OperStatus.ADBLinkUp || !parsed.OperStatus.ADBHelloUp {
		t.Errorf("mate-link flags = (%v, %v), want both true",
			parsed.OperStatus.ADBLinkUp, parsed.OperStatus.ADBHelloUp)
	}
	if parsed.AutoRevert {
		t.Error("AutoRevert = true, want false (activity stays on the backup precisely because it is false)")
	}

	// Healthy, yet the backup is the active and populated role.
	backup := parsed.VirtualRouters.Backup
	if got, want := backup.Status.Activity, "Local Active"; got != want {
		t.Errorf("Backup.Status.Activity = %q, want %q", got, want)
	}
	if backup.Status.PriorityReportedByMate == nil {
		t.Fatal("Backup.Status.PriorityReportedByMate is nil, want a populated subtree")
	}

	// The returned mate reports Standby here, not the Release of the degraded
	// fixture. This is the assertion that keeps the two fixtures distinct.
	mate := backup.Status.PriorityReportedByMate
	if mate.Summary == nil {
		t.Error("PriorityReportedByMate.Summary is nil, want \"Standby\"")
	} else if got, want := *mate.Summary, "Standby"; got != want {
		t.Errorf("PriorityReportedByMate.Summary = %q, want %q", got, want)
	}
	if mate.VRRP == nil {
		t.Error("PriorityReportedByMate.VRRP is nil, want \"Standby (150)\"")
	} else if got, want := *mate.VRRP, "Standby (150)"; got != want {
		t.Errorf("PriorityReportedByMate.VRRP = %q, want %q", got, want)
	}

	// Primary remains the sparse role.
	primaryJSON, err := json.Marshal(parsed.VirtualRouters.Primary.Status)
	if err != nil {
		t.Fatalf("marshalling primary status: %v", err)
	}
	if got, want := string(primaryJSON), `{"activity":"Shutdown"}`; got != want {
		t.Errorf("primary status JSON = %s, want %s", got, want)
	}
}
