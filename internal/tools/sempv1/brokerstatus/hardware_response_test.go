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

import (
	"encoding/json"
	"encoding/xml"
	"testing"
)

// TestHardwareResponse_RoundTrip decodes the live 3560 fixture, runs it
// through Curated(), and asserts the curated MVP fields land at their
// expected camelCase JSON keys with the wire values from the fixture.
// Also verifies the deferred-field set (mac-addresses, sfps, fpga,
// fibre-channel ports, ADB internals, dynamic telemetry) does NOT leak
// into the curated output — the curated payload must stay small and
// identity-only.
func TestHardwareResponse_RoundTrip(t *testing.T) {
	inner := extractInnerRPC(t, "testdata/show_hardware_details_appliance.xml")

	var w struct {
		XMLName xml.Name         `xml:"show"`
		Inner   hardwareResponse `xml:"hardware"`
	}
	if err := xml.Unmarshal(inner, &w); err != nil {
		t.Fatalf("unmarshal hardware: %v", err)
	}

	curated := w.Inner.Curated()
	if curated == nil {
		t.Fatal("Curated() returned nil on a populated appliance fixture")
	}

	asJSON, err := json.Marshal(curated)
	if err != nil {
		t.Fatalf("marshal curated hardware: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(asJSON, &out); err != nil {
		t.Fatalf("re-unmarshal hardware JSON: %v", err)
	}

	// MVP identity fields — every one of these must round-trip from the
	// curated struct to the JSON output. Loosening any single assertion
	// would silently drop a contracted field from the get-broker-status
	// envelope.
	wantStr := map[string]string{
		"platform":      "Solace Event Broker 3560",
		"chassisSerial": "S009001344",
		"biosVersion":   "SE5C600.86B.02.05.0004.051120151007",
	}
	for k, want := range wantStr {
		if got := out[k]; got != want {
			t.Errorf("hardwareDetails.%s = %v, want %q", k, got, want)
		}
	}

	if got := out["cpuCount"]; got != float64(2) {
		t.Errorf("hardwareDetails.cpuCount = %v, want 2", got)
	}
	if got := out["cpuModel"]; got != "Intel(R) Xeon(R) CPU E5-2630 v2 @ 2.60GHz" {
		t.Errorf("hardwareDetails.cpuModel = %v, want first-socket model from fixture", got)
	}
	if got := out["systemMemoryGiB"]; got != float64(32) {
		t.Errorf("hardwareDetails.systemMemoryGiB = %v, want 32 (parsed from \"32.0 GiB\")", got)
	}

	power, ok := out["power"].(map[string]any)
	if !ok {
		t.Fatalf("hardwareDetails.power missing or wrong type: %T", out["power"])
	}
	if got := power["redundancyConfiguration"]; got != "1+1" {
		t.Errorf("power.redundancyConfiguration = %v, want \"1+1\"", got)
	}
	if got := power["operationalCount"]; got != float64(2) {
		t.Errorf("power.operationalCount = %v, want 2", got)
	}

	disks, ok := out["disks"].([]any)
	if !ok || len(disks) != 2 {
		t.Fatalf("hardwareDetails.disks should have 2 entries, got %v", out["disks"])
	}
	for i, d := range disks {
		m := d.(map[string]any)
		for _, k := range []string{"id", "deviceModel", "serial"} {
			if _, ok := m[k]; !ok {
				t.Errorf("disks[%d] missing key %q", i, k)
			}
		}
	}

	// Slots: live fixture has 8 slot-numbers (1/1..1/8). Empty/in-use-by
	// placeholders are 1/1, 1/2, 1/5, 1/8 (4 entries). Populated slots
	// are 1/3 (ADB), 1/4 (TRB), 1/6 (NAB), 1/7 (HBA) — 4 entries.
	slots, ok := out["slots"].([]any)
	if !ok || len(slots) != 4 {
		t.Fatalf("hardwareDetails.slots should have 4 populated entries (empty + in-use-by filtered), got %v", out["slots"])
	}
	for i, s := range slots {
		m := s.(map[string]any)
		for _, k := range []string{"slot", "blade"} {
			if _, ok := m[k]; !ok {
				t.Errorf("slots[%d] missing required key %q", i, k)
			}
		}
	}

	// Deferred fields must not leak into the curated output. These come
	// from the live broker reply but are deliberately excluded per the
	// story spec — keep the curated payload identity-only.
	for _, dropped := range []string{
		// Extended chassis fields deferred:
		"chassisProductNumber", "chassisRevision", "boardSerial",
		"boardPartnumber", "systemType", "supportedBladeConfiguration",
		// Per-PSU detail deferred:
		"powerModules",
		// Per-blade-type extensions deferred:
		"macAddresses", "sfps", "fpga", "fibreChannel", "mateLink",
		"flashCardState", "fwVer", "aclTopicMatchingMode",
		// Dynamic telemetry — belongs in a different tool:
		"txPower", "rxPower", "chargeLevel", "errors", "fatalErrors",
		"crcErrCountP1", "invalidCrcCount",
	} {
		if _, present := out[dropped]; present {
			t.Errorf("deferred field %q should not appear in curated hardwareDetails (kept identity-only)", dropped)
		}
	}
}

// TestIsApplianceFromDescription exercises the platform-detection heuristic.
// The strings here mirror real broker output from the story spec plus the
// VMR/PubSub+ Standard line we hit in the existing version fixture; the
// last few cover defensive paths (empty, malformed, no-spaces "Software").
func TestIsApplianceFromDescription(t *testing.T) {
	cases := []struct {
		name        string
		description string
		want        bool
	}{
		// 3560 is the only SKU confirmed real (matches the live capture at
		// testdata/show_hardware_details_appliance.xml). The XXXX case pins
		// the heuristic's generation-agnostic property — any non-software
		// description should classify as appliance regardless of the model
		// number, so a future SKU works without code changes.
		{"appliance 3560", "Solace Event Broker 3560 Version 100.0main.0.7305", true},
		{"appliance generic placeholder SKU", "Solace Event Broker XXXX Version 100.0main.0.7305", true},
		{"software enterprise", "Solace Event Broker Software Enterprise Version 100.0main.0.8344", false},
		{"pubsub software standard", "Solace PubSub+ Software Standard Version 10.25.0.217", false},
		// Defensive: an empty/whitespace-only description must skip the
		// hardware call rather than guess.
		{"empty", "", false},
		{"whitespace only", "   ", false},
		// Defensive: "Software" without the surrounding spaces (e.g. as
		// a substring in a model name) does not count as the keyword.
		{"software substring no boundary", "SolaceSoftware3560 Version X", true},
		// Case-insensitive on the keyword side.
		{"uppercase Software", "Solace Event Broker SOFTWARE Enterprise Version X", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isApplianceFromDescription(tc.description); got != tc.want {
				t.Errorf("isApplianceFromDescription(%q) = %v, want %v", tc.description, got, tc.want)
			}
		})
	}
}

// TestParseMemoryGiB pins the broker-string → numeric conversion. The
// broker emits memory as "<float> GiB"; anything else must yield 0 so the
// curated payload's omitempty drops the field cleanly.
func TestParseMemoryGiB(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"32.0 GiB", 32},
		{"64 GiB", 64},
		{"  128.5 GiB  ", 128.5},
		{"32.0 gib", 32}, // case-insensitive unit
		// Failure modes: omit (return 0).
		{"", 0},
		{"32 MiB", 0},
		{"32.0", 0},
		{"thirty-two GiB", 0},
		{"32 GiB extra", 0},
	}
	for _, tc := range cases {
		if got := parseMemoryGiB(tc.in); got != tc.want {
			t.Errorf("parseMemoryGiB(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
