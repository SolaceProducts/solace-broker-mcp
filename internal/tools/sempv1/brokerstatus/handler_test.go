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
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv1"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools"
)

// fixtureClient routes incoming SEMPv1 requests by their XML body to the
// matching testdata fixture. It satisfies sempv1.Client and is the standard
// stub for handler tests in this package.
//
// Optional knobs let individual tests override behavior per-step:
//   - errorFor[XML] forces a specific Execute call to return that error
//     instead of the fixture (used for client-error and partial-failure tests)
//   - replaceXML[XML] replaces the InnerXML returned for that call (used
//     for parse-error tests that need to inject malformed XML for one step)
//   - versionPath overrides the show-version fixture path so tests can
//     swap between software and appliance descriptions without rewriting
//     the entire fixture map
type fixtureClient struct {
	errorFor    map[string]error
	replaceXML  map[string][]byte
	versionPath string
	calls       int32
}

func (s *fixtureClient) Execute(_ context.Context, xmlReq string) (*sempv1.Result, error) {
	atomic.AddInt32(&s.calls, 1)

	if err, ok := s.errorFor[xmlReq]; ok {
		return nil, err
	}

	var path string
	switch xmlReq {
	case versionXML:
		path = s.versionPath
		if path == "" {
			path = "testdata/show_version.xml"
		}
	case systemXML:
		path = "testdata/show_system.xml"
	case memoryXML:
		path = "testdata/show_memory.xml"
	case spoolXML:
		path = "testdata/show_message_spool_detail.xml"
	case hardwareXML:
		path = "testdata/show_hardware_details_appliance.xml"
	default:
		return nil, fmt.Errorf("fixtureClient: unexpected request %s", xmlReq)
	}

	if override, ok := s.replaceXML[xmlReq]; ok {
		return &sempv1.Result{InnerXML: override}, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s2 := string(raw)
	open := strings.Index(s2, "<rpc>")
	close := strings.LastIndex(s2, "</rpc>")
	if open < 0 || close < 0 {
		return nil, fmt.Errorf("fixtureClient: no <rpc>...</rpc> in %s", path)
	}
	return &sempv1.Result{InnerXML: []byte(s2[open+len("<rpc>") : close])}, nil
}

// TestHandler_Metadata exercises the tool's MCP surface directly: name,
// description, input schema, output schema, and annotations. Without this
// coverage a typo in Metadata() would only be caught at server registration
// time, when the LLM-facing wire contract is already broken. Mirrors the
// equivalent TestHandler_Metadata in the redundancy tool.
func TestHandler_Metadata(t *testing.T) {
	h := NewHandler()
	meta := h.Metadata()

	if meta.Name != "get-broker-status" {
		t.Errorf("Name = %q, want %q", meta.Name, "get-broker-status")
	}
	if meta.Description == "" {
		t.Error("Description is empty")
	}

	// Input schema: empty object (broker is injected by ToolManager).
	if meta.InputSchema["type"] != "object" {
		t.Errorf(`InputSchema["type"] = %v, want "object"`, meta.InputSchema["type"])
	}
	props, ok := meta.InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf(`InputSchema["properties"] is not a map[string]any: %T`, meta.InputSchema["properties"])
	}
	if len(props) != 0 {
		t.Errorf("InputSchema has %d properties, want 0", len(props))
	}
	if _, hasRequired := meta.InputSchema["required"]; hasRequired {
		t.Error(`InputSchema["required"] should not be set when properties is empty`)
	}

	// Output schema: generic step-keyed envelope.
	if meta.OutputSchema["type"] != "object" {
		t.Errorf(`OutputSchema["type"] = %v, want "object"`, meta.OutputSchema["type"])
	}
	addProps, ok := meta.OutputSchema["additionalProperties"].(map[string]any)
	if !ok {
		t.Fatalf(`OutputSchema["additionalProperties"] is not a map[string]any: %T`,
			meta.OutputSchema["additionalProperties"])
	}
	if addProps["type"] != "object" {
		t.Errorf(`additionalProperties["type"] = %v, want "object"`, addProps["type"])
	}

	// Annotations: read-only, explicit non-destructive.
	if !meta.Annotations.ReadOnly {
		t.Error("Annotations.ReadOnly = false, want true")
	}
	if meta.Annotations.Destructive == nil || *meta.Annotations.Destructive {
		t.Errorf("Annotations.Destructive = %v, want explicit false", meta.Annotations.Destructive)
	}
}

// TestHandle_Success runs the software-broker happy path: all four fixtures
// decode, the envelope carries all four step keys with non-empty payloads,
// the hardwareDetails step is skipped (description contains " Software "),
// and exactly four RPC calls fire. The default fixture's description names
// "Software Enterprise" so the platform-detection helper classifies it as
// non-appliance — keeps this test's "skip" assertion pinned to the same
// fixture every other test reuses.
func TestHandle_Success(t *testing.T) {
	h := NewHandler()
	stub := &fixtureClient{}
	tc := &tools.ToolContext{SEMPv1Client: stub}

	result, err := h.Handle(context.Background(), tc, map[string]any{})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if result == nil || result.StructuredContent == nil {
		t.Fatal("Handle returned nil result")
	}

	for _, key := range []string{"version", "system", "memory", "spool"} {
		v, ok := result.StructuredContent[key]
		if !ok {
			t.Errorf("envelope missing key %q", key)
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			t.Errorf("envelope[%q] is not a map: %T", key, v)
			continue
		}
		if len(m) == 0 {
			t.Errorf("envelope[%q] is empty", key)
		}
	}

	if got := atomic.LoadInt32(&stub.calls); got != 4 {
		t.Errorf("software-broker path should issue exactly 4 RPC calls, got %d (hardware step must be skipped)", got)
	}
	if _, ok := result.StructuredContent["hardwareDetails"]; ok {
		t.Error("software-broker envelope must not include hardwareDetails key")
	}

	// Spot-check one curated value reaches the envelope unchanged.
	if version, ok := result.StructuredContent["version"].(map[string]any); ok {
		if got := version["description"]; got != "Solace PubSub+ Software Enterprise Version 10.25.0.217" {
			t.Errorf("version.description = %v, want fixture value", got)
		}
	}
}

// TestHandle_ClientError_Passthrough verifies that errors from the SEMPv1
// client surface UNWRAPPED, so the manager's logToolResult can extract
// structured fields via errors.As(err, &*sempv1.Error). Wrapping with
// fmt.Errorf %w would still let errors.As work, but the redundancy tool
// established the convention of pure pass-through for v1 Execute errors.
func TestHandle_ClientError_Passthrough(t *testing.T) {
	sempErr := &sempv1.Error{
		Kind:       sempv1.ErrorKindHTTP,
		StatusCode: 401,
	}
	stub := &fixtureClient{
		errorFor: map[string]error{
			memoryXML: sempErr,
		},
	}

	h := NewHandler()
	tc := &tools.ToolContext{SEMPv1Client: stub}

	_, err := h.Handle(context.Background(), tc, map[string]any{})
	if err == nil {
		t.Fatal("Handle returned nil error, expected client failure")
	}

	var v1Err *sempv1.Error
	if !errors.As(err, &v1Err) {
		t.Fatalf("returned error %T not unwrappable to *sempv1.Error: %v", err, err)
	}
	if v1Err.StatusCode != 401 {
		t.Errorf("StatusCode = %d, want 401", v1Err.StatusCode)
	}
}

// TestHandle_ParseError_WrapsError verifies that XML parse failures inside
// Handle are wrapped with the get-broker-status: prefix so the resulting
// log line clearly attributes the failure to this tool's processing rather
// than the broker itself.
func TestHandle_ParseError_WrapsError(t *testing.T) {
	stub := &fixtureClient{
		replaceXML: map[string][]byte{
			systemXML: []byte("<not-valid-xml<<>"),
		},
	}

	h := NewHandler()
	tc := &tools.ToolContext{SEMPv1Client: stub}

	_, err := h.Handle(context.Background(), tc, map[string]any{})
	if err == nil {
		t.Fatal("Handle returned nil error, expected parse failure")
	}
	if !strings.Contains(err.Error(), "get-broker-status:") {
		t.Errorf("error %q should contain 'get-broker-status:' prefix to attribute failure", err)
	}
	if !strings.Contains(err.Error(), "system") {
		t.Errorf("error %q should mention which step failed (expected 'system')", err)
	}
}

// TestHandle_PartialFailure verifies the hard-failure policy: if any one
// of the four parallel calls errors, the whole tool errors. The user-facing
// envelope is never returned partially populated, since broker status is
// a coherent picture and a partial response could mislead consumers.
func TestHandle_PartialFailure(t *testing.T) {
	stub := &fixtureClient{
		errorFor: map[string]error{
			spoolXML: &sempv1.Error{
				Kind:       sempv1.ErrorKindExecuteFail,
				StatusCode: 200,
				Message:    "synthetic failure for partial-failure test",
			},
		},
	}

	h := NewHandler()
	tc := &tools.ToolContext{SEMPv1Client: stub}

	result, err := h.Handle(context.Background(), tc, map[string]any{})
	if err == nil {
		t.Fatal("Handle returned nil error, expected partial-failure to surface")
	}
	if result != nil {
		t.Errorf("Handle returned non-nil result on partial failure: %v", result)
	}
}

// TestHandle_Appliance_Success verifies the 5-call appliance path: the
// platform-detection helper recognises the appliance description, the
// hardware step fires after the parallel fan-out, and the envelope carries
// a populated hardwareDetails section with the curated MVP fields.
func TestHandle_Appliance_Success(t *testing.T) {
	stub := &fixtureClient{
		versionPath: "testdata/show_version_appliance.xml",
	}
	h := NewHandler()
	tc := &tools.ToolContext{SEMPv1Client: stub}

	result, err := h.Handle(context.Background(), tc, map[string]any{})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if result == nil || result.StructuredContent == nil {
		t.Fatal("Handle returned nil result")
	}

	if got := atomic.LoadInt32(&stub.calls); got != 5 {
		t.Errorf("appliance path should issue exactly 5 RPC calls (4 parallel + 1 hardware), got %d", got)
	}

	hw, ok := result.StructuredContent["hardwareDetails"].(map[string]any)
	if !ok {
		t.Fatalf("hardwareDetails missing or wrong type: %T", result.StructuredContent["hardwareDetails"])
	}
	// Pin every MVP curated key from the story spec — chassis identity,
	// compute, memory, power summary, disks, slot inventory. Loosening any
	// of these would silently drop a contracted field.
	if got := hw["platform"]; got != "Solace Event Broker 3560" {
		t.Errorf("hardwareDetails.platform = %v, want \"Solace Event Broker 3560\"", got)
	}
	if got := hw["chassisSerial"]; got != "S009001344" {
		t.Errorf("hardwareDetails.chassisSerial = %v, want fixture value", got)
	}
	if got := hw["biosVersion"]; got == nil || got == "" {
		t.Error("hardwareDetails.biosVersion missing")
	}
	if got := hw["cpuCount"]; got != float64(2) {
		t.Errorf("hardwareDetails.cpuCount = %v, want 2", got)
	}
	if got, _ := hw["cpuModel"].(string); !strings.Contains(got, "Xeon") {
		t.Errorf("hardwareDetails.cpuModel = %q, want a Xeon model string", got)
	}
	if got := hw["systemMemoryGiB"]; got != float64(32) {
		t.Errorf("hardwareDetails.systemMemoryGiB = %v, want 32 (parsed from \"32.0 GiB\")", got)
	}

	power, ok := hw["power"].(map[string]any)
	if !ok {
		t.Fatalf("hardwareDetails.power missing or wrong type: %T", hw["power"])
	}
	if got := power["redundancyConfiguration"]; got != "1+1" {
		t.Errorf("hardwareDetails.power.redundancyConfiguration = %v, want \"1+1\"", got)
	}
	if got := power["operationalCount"]; got != float64(2) {
		t.Errorf("hardwareDetails.power.operationalCount = %v, want 2", got)
	}

	disks, ok := hw["disks"].([]any)
	if !ok || len(disks) != 2 {
		t.Fatalf("hardwareDetails.disks should have 2 entries, got %T (len %d)", hw["disks"], len(disks))
	}

	// Slots: empty slots and "in use by" placeholders must be filtered out.
	// Live 3560 fixture populates 1/3 (ADB), 1/4 (TRB), 1/6 (NAB), 1/7 (HBA)
	// — 4 populated slots out of 8 total.
	slots, ok := hw["slots"].([]any)
	if !ok || len(slots) != 4 {
		t.Fatalf("hardwareDetails.slots should have 4 populated entries (empty + 'in use by' filtered), got %T (len %d)", hw["slots"], len(slots))
	}

	// At least one slot in the fixture (1/3, the ADB) carries
	// operational-state-up; that mapping must reach the JSON.
	foundOpState := false
	for _, s := range slots {
		m, ok := s.(map[string]any)
		if !ok {
			continue
		}
		if got, _ := m["operationalState"].(string); got == "up" || got == "down" {
			foundOpState = true
			break
		}
	}
	if !foundOpState {
		t.Error("at least one slot should carry operationalState (the ADB at 1/3 has operational-state-up=true)")
	}
}

// TestHandle_Appliance_HardwareStepFails verifies failure isolation: when
// the hardware step errors on an appliance broker, the rest of the envelope
// still returns successfully and hardwareDetails is omitted (rather than
// failing the whole tool). This is the critical "best-effort" property
// — a broken hardware RPC must not break get-broker-status overall.
func TestHandle_Appliance_HardwareStepFails(t *testing.T) {
	stub := &fixtureClient{
		versionPath: "testdata/show_version_appliance.xml",
		errorFor: map[string]error{
			hardwareXML: &sempv1.Error{
				Kind:    sempv1.ErrorKindExecuteFail,
				Message: "synthetic hardware-step failure",
			},
		},
	}
	h := NewHandler()
	tc := &tools.ToolContext{SEMPv1Client: stub}

	result, err := h.Handle(context.Background(), tc, map[string]any{})
	if err != nil {
		t.Fatalf("hardware-step failure should be swallowed; got error: %v", err)
	}
	if result == nil || result.StructuredContent == nil {
		t.Fatal("Handle returned nil result")
	}
	for _, key := range []string{"version", "system", "memory", "spool"} {
		if _, ok := result.StructuredContent[key]; !ok {
			t.Errorf("envelope still missing %q after hardware-step failure", key)
		}
	}
	if _, ok := result.StructuredContent["hardwareDetails"]; ok {
		t.Error("hardwareDetails must be omitted when the hardware step fails (best-effort)")
	}
}

// TestHandle_Appliance_HardwareStepParseError verifies failure isolation on
// the second failure mode for the hardware step: the broker answers with
// bytes that don't match the expected <show><hardware>...</hardware></show>
// shape (truncated wire response, schema drift, or an unexpected payload),
// so xml.Unmarshal inside executeAndDecode returns a parse error rather than
// the SEMPv1 client surfacing a transport error.
//
// Same contract as TestHandle_Appliance_HardwareStepFails: the rest of the
// envelope still returns successfully and hardwareDetails is omitted. The
// two tests are siblings, not duplicates — they exercise different code
// paths inside Handle's failure-isolation block. Without this test, a
// future refactor that swallowed transport errors but stopped catching
// parse errors would silently regress to "one bad slot field blanks the
// whole tool" behavior.
func TestHandle_Appliance_HardwareStepParseError(t *testing.T) {
	stub := &fixtureClient{
		versionPath: "testdata/show_version_appliance.xml",
		replaceXML: map[string][]byte{
			hardwareXML: []byte("<not-valid-xml<<>"),
		},
	}
	h := NewHandler()
	tc := &tools.ToolContext{SEMPv1Client: stub}

	result, err := h.Handle(context.Background(), tc, map[string]any{})
	if err != nil {
		t.Fatalf("hardware-step parse error should be swallowed; got error: %v", err)
	}
	if result == nil || result.StructuredContent == nil {
		t.Fatal("Handle returned nil result")
	}
	for _, key := range []string{"version", "system", "memory", "spool"} {
		if _, ok := result.StructuredContent[key]; !ok {
			t.Errorf("envelope still missing %q after hardware-step parse error", key)
		}
	}
	if _, ok := result.StructuredContent["hardwareDetails"]; ok {
		t.Error("hardwareDetails must be omitted when the hardware step fails to parse (best-effort)")
	}
}

// TestHandle_MalformedVersionDescription_SkipsHardware verifies the defensive
// default in isApplianceFromDescription: when the version response carries
// an empty description, the tool treats the broker as non-appliance and
// skips the hardware call rather than firing a wasted RPC against a broker
// it couldn't identify.
func TestHandle_MalformedVersionDescription_SkipsHardware(t *testing.T) {
	emptyDescVersion := []byte(`<show><version><description></description><uptime><total-secs>100</total-secs></uptime></version></show>`)
	stub := &fixtureClient{
		replaceXML: map[string][]byte{
			versionXML: emptyDescVersion,
		},
	}
	h := NewHandler()
	tc := &tools.ToolContext{SEMPv1Client: stub}

	result, err := h.Handle(context.Background(), tc, map[string]any{})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := atomic.LoadInt32(&stub.calls); got != 4 {
		t.Errorf("malformed/empty description should skip hardware call (4 calls expected), got %d", got)
	}
	if _, ok := result.StructuredContent["hardwareDetails"]; ok {
		t.Error("malformed-description envelope must not include hardwareDetails")
	}
}
