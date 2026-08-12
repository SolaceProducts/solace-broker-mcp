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
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv1"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tools"
)

// stubV1Client implements sempv1.Client for unit tests. It returns the
// configured result/err on every Execute call regardless of the XML
// argument. The xml input is not validated — handler tests focus on
// response-handling, not request shape (which is a static literal in
// Handle anyway).
type stubV1Client struct {
	result *sempv1.Result
	err    error
}

func (s *stubV1Client) Execute(ctx context.Context, xml string) (*sempv1.Result, error) {
	return s.result, s.err
}

// extractInnerRPC pulls the inner <rpc>...</rpc> bytes out of a full
// <rpc-reply> envelope, mimicking what parseReply (T2) returns at runtime.
// Used by the success test to feed Handle a realistic InnerXML.
//
// Matching is by string search, not by parsing, so a fixture's leading comment
// must not contain a literal <rpc> tag — this would slice from the comment and
// silently hand Handle the wrong bytes. Describe the request in prose instead.
func extractInnerRPC(t *testing.T, envelope []byte) []byte {
	t.Helper()
	s := string(envelope)
	open := strings.Index(s, "<rpc>")
	close := strings.LastIndex(s, "</rpc>")
	if open < 0 || close < 0 || close < open {
		t.Fatalf("could not locate <rpc>...</rpc> in fixture")
	}
	return []byte(s[open+len("<rpc>") : close])
}

func TestHandler_Metadata(t *testing.T) {
	h := NewHandler()
	meta := h.Metadata()

	if meta.Name != "get-redundancy-status" {
		t.Errorf("Name = %q, want %q", meta.Name, "get-redundancy-status")
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

// TestHandle_Success runs the happy path: the stub client returns the
// inner-of-<rpc> bytes from the live-broker fixture, and Handle should
// produce a step-keyed envelope with a "redundancy" key whose value is
// a map carrying the camelCase fields decoded from the wire response.
func TestHandle_Success(t *testing.T) {
	fullEnvelope, err := os.ReadFile("testdata/show_redundancy_standalone.xml")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	innerBytes := extractInnerRPC(t, fullEnvelope)

	stub := &stubV1Client{
		result: &sempv1.Result{InnerXML: innerBytes},
	}

	h := NewHandler()
	tc := &tools.ToolContext{SEMPv1Client: stub}

	result, err := h.Handle(context.Background(), tc, map[string]any{})
	if err != nil {
		t.Fatalf("Handle() error: %v", err)
	}
	if result == nil {
		t.Fatal("Handle() returned nil result")
	}

	redundancy, ok := result.StructuredContent["redundancy"].(map[string]any)
	if !ok {
		t.Fatalf(`StructuredContent["redundancy"] is not a map: %T`,
			result.StructuredContent["redundancy"])
	}

	// Spot-check Story 8 fields appear with their wire-decoded values.
	if redundancy["configStatus"] != "Shutdown" {
		t.Errorf(`configStatus = %v, want "Shutdown"`, redundancy["configStatus"])
	}
	if redundancy["activeStandbyRole"] != "Primary" {
		t.Errorf(`activeStandbyRole = %v, want "Primary"`, redundancy["activeStandbyRole"])
	}
	if redundancy["redundancyStatus"] != "Down" {
		t.Errorf(`redundancyStatus = %v, want "Down"`, redundancy["redundancyStatus"])
	}

	// operStatus should be a nested map (proves nested-struct → nested-map round-trip works).
	operStatus, ok := redundancy["operStatus"].(map[string]any)
	if !ok {
		t.Fatalf(`redundancy["operStatus"] is not a map: %T`, redundancy["operStatus"])
	}
	if operStatus["adbLinkUp"] != false {
		t.Errorf(`operStatus.adbLinkUp = %v, want false`, operStatus["adbLinkUp"])
	}
}

// TestHandle_ActiveStandby_Success runs the happy path against the HA-enabled
// fixture and asserts the step-keyed envelope carries the HA state fields the
// tool description promises: config/operational status, active-standby role,
// mate router name, mate link state, and per-virtual-router activity.
//
// This is the handler-level counterpart to
// TestResponse_ActiveStandby_RoundTrip: it proves the values survive the
// struct → JSON → map[string]any conversion in Handle, not just the XML decode.
func TestHandle_ActiveStandby_Success(t *testing.T) {
	fullEnvelope, err := os.ReadFile("testdata/show_redundancy_active_standby.xml")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	stub := &stubV1Client{
		result: &sempv1.Result{InnerXML: extractInnerRPC(t, fullEnvelope)},
	}

	h := NewHandler()
	tc := &tools.ToolContext{SEMPv1Client: stub}

	result, err := h.Handle(context.Background(), tc, map[string]any{})
	if err != nil {
		t.Fatalf("Handle() error: %v", err)
	}
	if result == nil {
		t.Fatal("Handle() returned nil result")
	}

	redundancy, ok := result.StructuredContent["redundancy"].(map[string]any)
	if !ok {
		t.Fatalf(`StructuredContent["redundancy"] is not a map: %T`,
			result.StructuredContent["redundancy"])
	}

	for _, f := range []struct {
		key  string
		want any
	}{
		{"configStatus", "Enabled"},
		{"redundancyStatus", "Up"},
		{"redundancyMode", "Active/Standby"},
		{"activeStandbyRole", "Primary"},
		{"mateRouterName", "testha01backup"},
		{"failoverCriteria", "any-fail"},
	} {
		if redundancy[f.key] != f.want {
			t.Errorf("%s = %v, want %v", f.key, redundancy[f.key], f.want)
		}
	}

	// Mate link state: both flags true, unlike the standalone fixture.
	operStatus, ok := redundancy["operStatus"].(map[string]any)
	if !ok {
		t.Fatalf(`redundancy["operStatus"] is not a map: %T`, redundancy["operStatus"])
	}
	if operStatus["adbLinkUp"] != true {
		t.Errorf("operStatus.adbLinkUp = %v, want true", operStatus["adbLinkUp"])
	}
	if operStatus["adbHelloUp"] != true {
		t.Errorf("operStatus.adbHelloUp = %v, want true", operStatus["adbHelloUp"])
	}

	// Per-virtual-router activity, and the nested priority-reported-by-mate
	// subtree — the deepest path in the response, so reaching it proves the
	// whole nested conversion holds.
	virtualRouters, ok := redundancy["virtualRouters"].(map[string]any)
	if !ok {
		t.Fatalf(`redundancy["virtualRouters"] is not a map: %T`, redundancy["virtualRouters"])
	}
	primaryStatus := nestedMap(t, virtualRouters, "primary", "status")
	if primaryStatus["activity"] != "Local Active" {
		t.Errorf(`primary.status.activity = %v, want "Local Active"`, primaryStatus["activity"])
	}
	mate, ok := primaryStatus["priorityReportedByMate"].(map[string]any)
	if !ok {
		t.Fatalf("primary.status.priorityReportedByMate is not a map: %T",
			primaryStatus["priorityReportedByMate"])
	}
	if mate["summary"] != "Standby" {
		t.Errorf(`primary.status.priorityReportedByMate.summary = %v, want "Standby"`, mate["summary"])
	}

	// Backup role: only activity survives, because that is all the broker sent.
	backupStatus := nestedMap(t, virtualRouters, "backup", "status")
	if backupStatus["activity"] != "Shutdown" {
		t.Errorf(`backup.status.activity = %v, want "Shutdown"`, backupStatus["activity"])
	}
	if len(backupStatus) != 1 {
		t.Errorf("backup.status has %d keys, want 1 (only activity); got %v",
			len(backupStatus), backupStatus)
	}
}

// TestHandle_Released_Success is the handler-level counterpart to
// TestResponse_Released_RoundTrip: it proves the degraded-state values survive
// the struct → JSON → map[string]any conversion in Handle, not just the XML
// decode.
//
// The assertion that matters most is the key count on primary.status. Handle
// builds its map from the marshalled struct, so a regression that made the
// optional pointer fields materialize as zero values would show up here as
// extra keys — a client would then see a primary virtual router reporting
// vrrpPriority 0 and an empty routing-interface that the broker never sent.
func TestHandle_Released_Success(t *testing.T) {
	fullEnvelope, err := os.ReadFile("testdata/show_redundancy_released.xml")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	stub := &stubV1Client{
		result: &sempv1.Result{InnerXML: extractInnerRPC(t, fullEnvelope)},
	}

	h := NewHandler()
	tc := &tools.ToolContext{SEMPv1Client: stub}

	result, err := h.Handle(context.Background(), tc, map[string]any{})
	if err != nil {
		t.Fatalf("Handle() error: %v", err)
	}
	if result == nil {
		t.Fatal("Handle() returned nil result")
	}

	redundancy, ok := result.StructuredContent["redundancy"].(map[string]any)
	if !ok {
		t.Fatalf(`StructuredContent["redundancy"] is not a map: %T`,
			result.StructuredContent["redundancy"])
	}

	for _, f := range []struct {
		key  string
		want any
	}{
		{"configStatus", "Enabled"},
		{"redundancyStatus", "Down"},
		{"redundancyMode", "Active/Standby"},
		{"activeStandbyRole", "Backup"},
		{"mateRouterName", "testha01primary"},
	} {
		if redundancy[f.key] != f.want {
			t.Errorf("%s = %v, want %v", f.key, redundancy[f.key], f.want)
		}
	}

	// Degraded group, healthy mate link — the combination this fixture exists
	// for, asserted here at the boundary a client actually reads.
	operStatus, ok := redundancy["operStatus"].(map[string]any)
	if !ok {
		t.Fatalf(`redundancy["operStatus"] is not a map: %T`, redundancy["operStatus"])
	}
	if operStatus["adbLinkUp"] != true {
		t.Errorf("operStatus.adbLinkUp = %v, want true", operStatus["adbLinkUp"])
	}
	if operStatus["adbHelloUp"] != true {
		t.Errorf("operStatus.adbHelloUp = %v, want true", operStatus["adbHelloUp"])
	}

	virtualRouters, ok := redundancy["virtualRouters"].(map[string]any)
	if !ok {
		t.Fatalf(`redundancy["virtualRouters"] is not a map: %T`, redundancy["virtualRouters"])
	}

	// Backup is the populated, active role here.
	backupStatus := nestedMap(t, virtualRouters, "backup", "status")
	if backupStatus["activity"] != "Local Active" {
		t.Errorf(`backup.status.activity = %v, want "Local Active"`, backupStatus["activity"])
	}
	mate, ok := backupStatus["priorityReportedByMate"].(map[string]any)
	if !ok {
		t.Fatalf("backup.status.priorityReportedByMate is not a map: %T",
			backupStatus["priorityReportedByMate"])
	}
	if mate["summary"] != "Release" {
		t.Errorf(`backup.status.priorityReportedByMate.summary = %v, want "Release"`, mate["summary"])
	}

	// Primary is the sparse role: only activity, mirroring the backup-side
	// assertion in TestHandle_ActiveStandby_Success.
	primaryStatus := nestedMap(t, virtualRouters, "primary", "status")
	if primaryStatus["activity"] != "Shutdown" {
		t.Errorf(`primary.status.activity = %v, want "Shutdown"`, primaryStatus["activity"])
	}
	if len(primaryStatus) != 1 {
		t.Errorf("primary.status has %d keys, want 1 (only activity); got %v",
			len(primaryStatus), primaryStatus)
	}
}

// nestedMap walks a chain of map[string]any keys, failing the test with the
// path that broke rather than a bare type assertion panic.
func nestedMap(t *testing.T, m map[string]any, keys ...string) map[string]any {
	t.Helper()
	for i, k := range keys {
		next, ok := m[k].(map[string]any)
		if !ok {
			t.Fatalf("%s is not a map[string]any: %T", strings.Join(keys[:i+1], "."), m[k])
		}
		m = next
	}
	return m
}

// TestHandle_ClientError_Passthrough verifies that errors from the
// SEMPv1 client surface unwrapped, so the manager's logToolResult (W3)
// can still extract structured fields via errors.As(err, &*sempv1.Error).
// If Handle ever wrapped these errors with anything other than %w (or
// suppressed them entirely), this test would catch it.
func TestHandle_ClientError_Passthrough(t *testing.T) {
	sempErr := &sempv1.Error{
		Kind:       sempv1.ErrorKindHTTP,
		StatusCode: 401,
	}
	stub := &stubV1Client{err: sempErr}

	h := NewHandler()
	tc := &tools.ToolContext{SEMPv1Client: stub}

	_, err := h.Handle(context.Background(), tc, map[string]any{})
	if err == nil {
		t.Fatal("Handle() returned nil error, expected sempv1 error")
	}

	var v1Err *sempv1.Error
	if !errors.As(err, &v1Err) {
		t.Errorf("returned error %T not unwrappable to *sempv1.Error", err)
	}
	if v1Err.StatusCode != 401 {
		t.Errorf("StatusCode = %d, want 401", v1Err.StatusCode)
	}
}

// TestHandle_ParseError_WrapsError verifies that XML parse failures
// inside Handle are wrapped with the get-redundancy-status: prefix, so
// the resulting log line clearly attributes the failure to this tool's
// processing rather than the broker.
func TestHandle_ParseError_WrapsError(t *testing.T) {
	stub := &stubV1Client{
		result: &sempv1.Result{
			InnerXML: []byte("<not-valid-xml<<>"),
		},
	}

	h := NewHandler()
	tc := &tools.ToolContext{SEMPv1Client: stub}

	_, err := h.Handle(context.Background(), tc, map[string]any{})
	if err == nil {
		t.Fatal("Handle() returned nil error, expected parse failure")
	}
	if !strings.Contains(err.Error(), "redundancy response") {
		t.Errorf("error %q should mention 'redundancy response' for context", err)
	}
}
