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

package discardstats

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

// fixtureClient routes XML requests to testdata files. Mirrors brokerhealth's
// fixtureClient pattern. errorFor and replaceXML let individual tests inject
// failure modes for one specific RPC.
type fixtureClient struct {
	errorFor   map[string]error
	replaceXML map[string][]byte
	calls      int32
}

func (s *fixtureClient) Execute(_ context.Context, xmlReq string) (*sempv1.Result, error) {
	atomic.AddInt32(&s.calls, 1)

	if err, ok := s.errorFor[xmlReq]; ok {
		return nil, err
	}

	var path string
	switch {
	case xmlReq == statsClientXML:
		path = "testdata/show_stats_client.xml"
	case xmlReq == spoolStatsXML:
		path = "testdata/show_message_spool_stats.xml"
	case xmlReq == vpnStatsXML("default"):
		path = "testdata/show_message_vpn_default_stats.xml"
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
	body := string(raw)
	open := strings.Index(body, "<rpc>")
	closeIdx := strings.LastIndex(body, "</rpc>")
	if open < 0 || closeIdx < 0 {
		return nil, fmt.Errorf("fixtureClient: no <rpc>...</rpc> in %s", path)
	}
	return &sempv1.Result{InnerXML: []byte(body[open+len("<rpc>") : closeIdx])}, nil
}

// TestHandler_Metadata pins the MCP-facing surface: name, optional vpnName
// param shape, output schema, and read-only annotations.
func TestHandler_Metadata(t *testing.T) {
	h := NewHandler()
	meta := h.Metadata()

	if meta.Name != "get-discard-stats" {
		t.Errorf("Name = %q, want %q", meta.Name, "get-discard-stats")
	}
	if meta.Description == "" {
		t.Error("Description is empty")
	}

	if meta.InputSchema["type"] != "object" {
		t.Errorf(`InputSchema["type"] = %v, want "object"`, meta.InputSchema["type"])
	}
	props, ok := meta.InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf(`InputSchema["properties"] is not a map[string]any: %T`,
			meta.InputSchema["properties"])
	}
	vpnProp, ok := props["vpnName"].(map[string]any)
	if !ok {
		t.Fatalf(`InputSchema.properties["vpnName"] missing or wrong type: %T`, props["vpnName"])
	}
	if vpnProp["type"] != "string" {
		t.Errorf(`vpnName.type = %v, want "string"`, vpnProp["type"])
	}
	if _, hasRequired := meta.InputSchema["required"]; hasRequired {
		t.Error("vpnName must be optional — required should not be set")
	}
	// minLength: 1 makes empty strings fail upstream validation so the tool
	// can't silently downgrade to broker-wide when the caller intended a
	// specific VPN. Pin it so a future schema refactor can't relax this.
	if vpnProp["minLength"] != 1 {
		t.Errorf(`vpnName.minLength = %v, want 1 — empty strings must be rejected upstream`, vpnProp["minLength"])
	}

	if meta.OutputSchema["type"] != "object" {
		t.Errorf(`OutputSchema["type"] = %v, want "object"`, meta.OutputSchema["type"])
	}
	outProps, ok := meta.OutputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("OutputSchema.properties missing or wrong type: %T", meta.OutputSchema["properties"])
	}
	for _, key := range []string{"vpnName", "clientDiscards", "spoolDiscards"} {
		if _, ok := outProps[key]; !ok {
			t.Errorf("OutputSchema.properties missing %q — schema must cover both response shapes", key)
		}
	}

	// additionalProperties: false rejects any debug/leaked field at the
	// envelope boundary. required: [clientDiscards] pins the cross-mode
	// invariant. Both are load-bearing — pin them so a refactor can't
	// silently relax the schema.
	if got := meta.OutputSchema["additionalProperties"]; got != false {
		t.Errorf(`OutputSchema["additionalProperties"] = %v, want false`, got)
	}
	required, ok := meta.OutputSchema["required"].([]string)
	if !ok || len(required) != 1 || required[0] != "clientDiscards" {
		t.Errorf(`OutputSchema["required"] = %v, want ["clientDiscards"]`, meta.OutputSchema["required"])
	}

	if !meta.Annotations.ReadOnly {
		t.Error("Annotations.ReadOnly = false, want true")
	}
	if meta.Annotations.Destructive == nil || *meta.Annotations.Destructive {
		t.Errorf("Annotations.Destructive = %v, want explicit false", meta.Annotations.Destructive)
	}
}

// TestHandle_BrokerWide_Success verifies the no-vpnName path: both broker-wide
// fixtures decode and the envelope carries clientDiscards + spoolDiscards.
func TestHandle_BrokerWide_Success(t *testing.T) {
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

	if atomic.LoadInt32(&stub.calls) != 2 {
		t.Errorf("broker-wide path expected 2 RPC calls, got %d", stub.calls)
	}

	for _, key := range []string{"clientDiscards", "spoolDiscards"} {
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

	// Spot-check: clientDiscards must contain the ingress sub-tree, which
	// must in turn contain total-ingress-discards mapped to camelCase.
	cd := result.StructuredContent["clientDiscards"].(map[string]any)
	ingress, ok := cd["ingress"].(map[string]any)
	if !ok {
		t.Fatalf("clientDiscards.ingress missing or wrong type: %T", cd["ingress"])
	}
	if _, ok := ingress["totalIngressDiscards"]; !ok {
		t.Error("clientDiscards.ingress.totalIngressDiscards not present (camelCase rename failed)")
	}

	// Spot-check: spoolDiscards must include the DMQ-split TTL and
	// max-redelivery categories — the entire reason this tool uses SEMPv1.
	sd := result.StructuredContent["spoolDiscards"].(map[string]any)
	for _, key := range []string{
		"totalTtlExpiredToDmqMessages",
		"maxRedeliveryExceededToDmqMessages",
		"discardSpoolOverQuota",
	} {
		if _, ok := sd[key]; !ok {
			t.Errorf("spoolDiscards missing curated field %q", key)
		}
	}
}

// TestHandle_PerVPN_Success verifies the vpnName path: one RPC call, envelope
// carries vpnName + a reduced clientDiscards block; spoolDiscards is absent.
func TestHandle_PerVPN_Success(t *testing.T) {
	h := NewHandler()
	stub := &fixtureClient{}
	tc := &tools.ToolContext{SEMPv1Client: stub}

	result, err := h.Handle(context.Background(), tc, map[string]any{"vpnName": "default"})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if result == nil || result.StructuredContent == nil {
		t.Fatal("Handle returned nil result")
	}

	if atomic.LoadInt32(&stub.calls) != 1 {
		t.Errorf("per-VPN path expected 1 RPC call, got %d", stub.calls)
	}

	if got := result.StructuredContent["vpnName"]; got != "default" {
		t.Errorf("vpnName = %v, want \"default\"", got)
	}
	if _, ok := result.StructuredContent["spoolDiscards"]; ok {
		t.Error("per-VPN response should not include spoolDiscards (SEMPv1 doesn't expose per-VPN spool stats)")
	}
	cd, ok := result.StructuredContent["clientDiscards"].(map[string]any)
	if !ok {
		t.Fatalf("clientDiscards missing or wrong type: %T", result.StructuredContent["clientDiscards"])
	}
	ingress, ok := cd["ingress"].(map[string]any)
	if !ok {
		t.Fatalf("clientDiscards.ingress missing: %T", cd["ingress"])
	}
	for _, key := range []string{"totalIngressDiscards", "noSubscriptionMatch", "publishTopicAcl", "msgTooBig"} {
		if _, ok := ingress[key]; !ok {
			t.Errorf("per-VPN clientDiscards.ingress.%s not present — per-VPN must expose the full broker-returned set", key)
		}
	}
	egress, ok := cd["egress"].(map[string]any)
	if !ok {
		t.Fatalf("clientDiscards.egress missing: %T", cd["egress"])
	}
	for _, key := range []string{"totalEgressDiscards", "transmitCongestion", "clientNotConnected", "messageElided"} {
		if _, ok := egress[key]; !ok {
			t.Errorf("per-VPN clientDiscards.egress.%s not present — per-VPN must expose the full broker-returned set", key)
		}
	}
}

// TestHandle_EmptyVpnName_FallsBackToBrokerWide is the defense-in-depth
// behavior: the InputSchema's minLength:1 rejects empty vpnName upstream so
// production callers can never reach this branch, but if validation is
// bypassed (e.g. a direct Go caller, or a buggy validator), Handle still
// does the safe thing by falling through to broker-wide rather than
// issuing a per-VPN RPC with an empty name.
func TestHandle_EmptyVpnName_FallsBackToBrokerWide(t *testing.T) {
	h := NewHandler()
	stub := &fixtureClient{}
	tc := &tools.ToolContext{SEMPv1Client: stub}

	_, err := h.Handle(context.Background(), tc, map[string]any{"vpnName": ""})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if atomic.LoadInt32(&stub.calls) != 2 {
		t.Errorf("empty vpnName should fall back to broker-wide (2 calls), got %d", stub.calls)
	}
}

// TestHandle_BrokerWide_ClientError_Passthrough verifies that errors from the
// SEMPv1 client are returned unwrapped so the manager can extract structured
// fields via errors.As(err, &*sempv1.Error). Same convention as brokerhealth.
func TestHandle_BrokerWide_ClientError_Passthrough(t *testing.T) {
	sempErr := &sempv1.Error{
		Kind:       sempv1.ErrorKindHTTP,
		StatusCode: 401,
	}
	stub := &fixtureClient{
		errorFor: map[string]error{
			spoolStatsXML: sempErr,
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

// TestHandle_PerVPN_ParseError_WrapsError verifies that XML parse failures
// inside Handle are wrapped with the get-discard-stats: prefix so log lines
// attribute the failure to this tool.
func TestHandle_PerVPN_ParseError_WrapsError(t *testing.T) {
	stub := &fixtureClient{
		replaceXML: map[string][]byte{
			vpnStatsXML("default"): []byte("<not-valid-xml<<>"),
		},
	}
	h := NewHandler()
	tc := &tools.ToolContext{SEMPv1Client: stub}

	_, err := h.Handle(context.Background(), tc, map[string]any{"vpnName": "default"})
	if err == nil {
		t.Fatal("Handle returned nil error, expected parse failure")
	}
	if !strings.Contains(err.Error(), "get-discard-stats:") {
		t.Errorf("error %q should contain 'get-discard-stats:' prefix", err)
	}
}

// TestHandle_BrokerWide_PartialFailure verifies hard-failure semantics: if
// one of the two parallel broker-wide calls fails, the whole tool fails.
func TestHandle_BrokerWide_PartialFailure(t *testing.T) {
	stub := &fixtureClient{
		errorFor: map[string]error{
			spoolStatsXML: &sempv1.Error{
				Kind:    sempv1.ErrorKindExecuteFail,
				Message: "synthetic failure",
			},
		},
	}
	h := NewHandler()
	tc := &tools.ToolContext{SEMPv1Client: stub}

	result, err := h.Handle(context.Background(), tc, map[string]any{})
	if err == nil {
		t.Fatal("expected partial failure to surface as error")
	}
	if result != nil {
		t.Errorf("result should be nil on partial failure, got %v", result)
	}
}

// TestVPNStatsXML_EscapesInput guards against the broker receiving an
// unescaped XML payload if a VPN name happens to contain markup-like
// characters. The broker would reject most of these anyway, but escaping at
// the boundary is the defensive posture.
func TestVPNStatsXML_EscapesInput(t *testing.T) {
	got := vpnStatsXML("ev&il<vpn>")
	if strings.Contains(got, "ev&il<vpn>") {
		t.Errorf("vpnStatsXML did not escape input: %s", got)
	}
	for _, want := range []string{"&amp;", "&lt;", "&gt;"} {
		if !strings.Contains(got, want) {
			t.Errorf("vpnStatsXML output missing %q escape: %s", want, got)
		}
	}
}
