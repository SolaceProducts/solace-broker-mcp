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
	"strconv"
	"strings"
)

// hardwareResponse decodes <rpc><show><hardware>...</hardware></show></rpc>
// from appliance brokers. Software brokers do not implement the underlying
// CLI and either return an error or an empty payload; callers must gate the
// RPC on isApplianceFromDescription and treat a hardware-step error as
// best-effort (omit the section, do not fail the whole tool).
//
// The full broker reply is wide (~30 elements per slot, including dynamic
// telemetry like SFP Tx/Rx power, ADB charge level, and FC error counters).
// Curated() reduces it to the small identity-only subset surfaced through
// get-broker-status: chassis identity, CPU/memory, power summary, disks,
// and a flat slot inventory. Telemetry is deliberately excluded — it
// belongs in a separate observation tool, not in a status snapshot.
//
// See docs/internal/semp/get-broker-status-curated-fields.md (Hardware
// section) for the per-field rationale and the explicitly deferred list.
type hardwareResponse struct {
	Platform        string            `xml:"platform"`
	Mainboard       *mainboardT       `xml:"mainboard"`
	PowerRedundancy *powerRedundancyT `xml:"power-redundancy"`
	Disks           *disksT           `xml:"disks"`
	// The schema declares <fabric> as maxOccurs="unbounded" — multi-fabric
	// chassis are possible, so accept a slice. The 3560 has one.
	Fabrics []fabricT `xml:"fabric"`
}

// mainboardT carries chassis-level identity plus CPU and memory facts. The
// broker also emits chassis-revision, board-serial, board-partnumber, and
// per-socket cpu1-version/cpu2-version siblings; those are deliberately
// dropped — chassis-serial + bios-version are the operator-cited identity
// pair and the cpus/cpu repeating element supersedes the per-socket fields.
type mainboardT struct {
	ChassisSerial string `xml:"chassis-serial"`
	BIOSVersion   string `xml:"bios-version"`
	CPUs          *cpusT `xml:"cpus"`
	// Memory is a unit-qualified string from the broker (e.g. "32.0 GiB").
	// parseMemoryGiB converts it to a numeric for the curated payload.
	Memory string `xml:"memory"`
}

type cpusT struct {
	CPU []string `xml:"cpu"`
}

// powerRedundancyT carries the redundancy-summary fields. Per-PSU module
// status (power-modules/power-module/{power-module-number, power-module-status})
// is intentionally not surfaced — operators want the summary, not the list,
// for a status snapshot. Add later if a per-PSU view is requested.
//
// operational-power-supplies is xs:string in the schema (despite being a
// count) to match the broker's wire encoding, so it's parsed via Atoi when
// flattening into the curated payload.
type powerRedundancyT struct {
	RedundancyConfig    string `xml:"power-redundancy-config"`
	OperationalSupplies string `xml:"operational-power-supplies"`
}

type disksT struct {
	Disk []diskT `xml:"disk"`
}

type diskT struct {
	DiskNumber  int    `xml:"disk-number"`
	DeviceModel string `xml:"device-model"`
	Serial      string `xml:"serial"`
}

// fabricT and slotT mirror the <fabric>/<slot> repeat. We keep only the
// identity columns (slot-number, card-type, product, serial,
// operational-state-up); blade-type-specific fields (mac-addresses, sfps,
// fpga, fibre-channel ports, ADB internals) are deferred — they would
// require per-blade-type switch logic without adding identity signal.
type fabricT struct {
	FabricNumber int     `xml:"fabric-number"`
	Slot         []slotT `xml:"slot"`
}

type slotT struct {
	SlotNumber         string `xml:"slot-number"`
	CardType           string `xml:"card-type"`
	Product            string `xml:"product"`
	Serial             string `xml:"serial"`
	OperationalStateUp *bool  `xml:"operational-state-up"`
}

// hardwareCurated is the JSON shape emitted under "hardwareDetails" in the
// envelope. Names mirror the get-broker-status curated-fields doc. Top-level
// chassis fields are omitempty so missing-on-this-chassis attributes drop
// out of the payload rather than appearing as zero values; inside disks[]
// and slots[] the identity columns (id, slot) are always emitted because
// they label the row and a zero value there means "broker decoded oddly",
// not "absent".
type hardwareCurated struct {
	Platform        string         `json:"platform,omitempty"`
	ChassisSerial   string         `json:"chassisSerial,omitempty"`
	BIOSVersion     string         `json:"biosVersion,omitempty"`
	CPUCount        int            `json:"cpuCount,omitempty"`
	CPUModel        string         `json:"cpuModel,omitempty"`
	SystemMemoryGiB float64        `json:"systemMemoryGiB,omitempty"`
	Power           *powerCurated  `json:"power,omitempty"`
	Disks           []diskCurated  `json:"disks,omitempty"`
	Slots           []slotCurated  `json:"slots,omitempty"`
}

type powerCurated struct {
	RedundancyConfiguration string `json:"redundancyConfiguration,omitempty"`
	OperationalCount        int    `json:"operationalCount,omitempty"`
}

type diskCurated struct {
	ID          int    `json:"id"`
	DeviceModel string `json:"deviceModel,omitempty"`
	Serial      string `json:"serial,omitempty"`
}

type slotCurated struct {
	Slot             string `json:"slot"`
	Blade            string `json:"blade,omitempty"`
	ProductNumber    string `json:"productNumber,omitempty"`
	Serial           string `json:"serial,omitempty"`
	OperationalState string `json:"operationalState,omitempty"`
}

// Curated returns the small identity-only subset of the hardware response
// suitable for inclusion in the get-broker-status envelope. The hardware
// step is gated on isApplianceFromDescription upstream, so a software
// broker can never reach this code — no defensive empty-payload sentinel
// is needed here. On a malformed appliance reply the returned struct may
// carry only the fields that decoded; omitempty on the top-level chassis
// fields handles the partial case cleanly.
func (r hardwareResponse) Curated() *hardwareCurated {
	c := &hardwareCurated{Platform: strings.TrimSpace(r.Platform)}

	if r.Mainboard != nil {
		c.ChassisSerial = strings.TrimSpace(r.Mainboard.ChassisSerial)
		c.BIOSVersion = strings.TrimSpace(r.Mainboard.BIOSVersion)
		c.SystemMemoryGiB = parseMemoryGiB(r.Mainboard.Memory)
		if r.Mainboard.CPUs != nil && len(r.Mainboard.CPUs.CPU) > 0 {
			c.CPUCount = len(r.Mainboard.CPUs.CPU)
			c.CPUModel = strings.TrimSpace(r.Mainboard.CPUs.CPU[0])
		}
	}

	if r.PowerRedundancy != nil {
		p := &powerCurated{
			RedundancyConfiguration: strings.TrimSpace(r.PowerRedundancy.RedundancyConfig),
		}
		if n, err := strconv.Atoi(strings.TrimSpace(r.PowerRedundancy.OperationalSupplies)); err == nil {
			p.OperationalCount = n
		}
		// Drop the whole section if neither field decoded — avoids an empty
		// {"power": {}} object in the JSON output on chassis that don't emit
		// power-redundancy at all.
		if p.RedundancyConfiguration != "" || p.OperationalCount != 0 {
			c.Power = p
		}
	}

	if r.Disks != nil {
		for _, d := range r.Disks.Disk {
			c.Disks = append(c.Disks, diskCurated{
				ID:          d.DiskNumber,
				DeviceModel: strings.TrimSpace(d.DeviceModel),
				Serial:      strings.TrimSpace(d.Serial),
			})
		}
	}

	for _, fab := range r.Fabrics {
		for _, sl := range fab.Slot {
			// Skip empty slots — both literal "empty" and the sibling
			// placeholder "in use by slot N/M" the broker emits for the
			// half of a paired card slot. Operators want the populated
			// inventory, not gaps.
			cardType := strings.TrimSpace(sl.CardType)
			lower := strings.ToLower(cardType)
			if cardType == "" || lower == "empty" || strings.HasPrefix(lower, "in use by") {
				continue
			}
			entry := slotCurated{
				Slot:          strings.TrimSpace(sl.SlotNumber),
				Blade:         cardType,
				ProductNumber: strings.TrimSpace(sl.Product),
				Serial:        strings.TrimSpace(sl.Serial),
			}
			// operational-state-up is ADB-only on the 3560; other blades
			// omit it. Map true/false to "up"/"down" so the JSON value is
			// self-describing rather than a bare boolean.
			if sl.OperationalStateUp != nil {
				if *sl.OperationalStateUp {
					entry.OperationalState = "up"
				} else {
					entry.OperationalState = "down"
				}
			}
			c.Slots = append(c.Slots, entry)
		}
	}

	return c
}

// parseMemoryGiB extracts the GiB value from broker memory strings of the
// form "32.0 GiB". Returns 0 if the value is empty, malformed, or carries
// an unexpected unit — callers treat 0 as "absent" via omitempty.
func parseMemoryGiB(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	parts := strings.Fields(s)
	if len(parts) != 2 || !strings.EqualFold(parts[1], "GiB") {
		return 0
	}
	v, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0
	}
	return v
}

// isApplianceFromDescription inspects the description string returned by
// show version (the existing step-1 response) to decide whether the broker
// is a hardware appliance and the conditional show-hardware-details step
// should fire.
//
// Heuristic per the SOL-150708 spec: descriptions containing " Software "
// (case-insensitive, with surrounding spaces to avoid matching "softwarey"
// or similar) are software brokers. Any other non-empty description is
// treated as appliance. The check is intentionally generation-agnostic —
// hardware descriptions follow the pattern "Solace Event Broker NNNN
// Version ..." (3560, 3260, 3660, future SKUs), and the model number is
// never inspected. A new appliance generation works without code changes.
//
// Defensive default: an empty/missing description returns false (skip the
// hardware call) rather than firing a wasted RPC against a broker we
// couldn't identify. False positives on the appliance side are harmless
// because the hardware step is failure-isolated — a bogus call against
// a software broker produces an error that gets logged and dropped, with
// the rest of the envelope still returned.
func isApplianceFromDescription(description string) bool {
	description = strings.TrimSpace(description)
	if description == "" {
		return false
	}
	return !strings.Contains(strings.ToLower(description), " software ")
}
