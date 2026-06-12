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
	"encoding/json"
	"encoding/xml"
	"os"
	"strings"
	"testing"
)

// extractInnerRPC strips the outer <rpc-reply>...<rpc>...</rpc>...</rpc-reply>
// envelope and returns the inner <show>...</show> bytes — the shape of
// result.InnerXML at runtime per the SEMPv1 parseReply contract.
func extractInnerRPC(t *testing.T, fixturePath string) []byte {
	t.Helper()
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", fixturePath, err)
	}
	s := string(raw)
	open := strings.Index(s, "<rpc>")
	close := strings.LastIndex(s, "</rpc>")
	if open < 0 || close < 0 || close < open {
		t.Fatalf("could not locate <rpc>...</rpc> in fixture %s", fixturePath)
	}
	return []byte(s[open+len("<rpc>") : close])
}

// decodeAndMarshal runs the same XML → struct → method → JSON → map pipeline
// the handler uses. For client/vpn responses, the method is
// IngressEgressDiscards() (returns {ingress, egress}); for spool it is
// Discards() (returns the flat counter payload). Marshalling the method
// output — not the raw response struct — exercises the production code path
// the user actually sees.
func decodeAndMarshal(t *testing.T, inner []byte, innerTag string, target any) map[string]any {
	t.Helper()

	var marshalled any
	switch tgt := target.(type) {
	case *clientStatsResponse:
		var w struct {
			XMLName xml.Name            `xml:"show"`
			Inner   clientStatsResponse `xml:"stats"`
		}
		if err := xml.Unmarshal(inner, &w); err != nil {
			t.Fatalf("unmarshal %s: %v", innerTag, err)
		}
		*tgt = w.Inner
		marshalled = tgt.IngressEgressDiscards()
	case *spoolStatsResponse:
		var w struct {
			XMLName xml.Name           `xml:"show"`
			Inner   spoolStatsResponse `xml:"message-spool"`
		}
		if err := xml.Unmarshal(inner, &w); err != nil {
			t.Fatalf("unmarshal %s: %v", innerTag, err)
		}
		*tgt = w.Inner
		marshalled = tgt.Discards()
	case *vpnStatsResponse:
		var w struct {
			XMLName xml.Name         `xml:"show"`
			Inner   vpnStatsResponse `xml:"message-vpn"`
		}
		if err := xml.Unmarshal(inner, &w); err != nil {
			t.Fatalf("unmarshal %s: %v", innerTag, err)
		}
		*tgt = w.Inner
		marshalled = tgt.IngressEgressDiscards()
	default:
		t.Fatalf("decodeAndMarshal: unsupported target type %T", target)
	}

	asJSON, err := json.Marshal(marshalled)
	if err != nil {
		t.Fatalf("marshal %s: %v", innerTag, err)
	}
	var out map[string]any
	if err := json.Unmarshal(asJSON, &out); err != nil {
		t.Fatalf("re-unmarshal %s JSON: %v", innerTag, err)
	}
	return out
}

// TestClientStatsResponse_RoundTrip decodes the live show-stats-client
// fixture, runs it through IngressEgressDiscards(), and asserts (1) the
// top-level shape is exactly {ingress, egress}, (2) all 11 ingress and 10
// egress curated categories are present, (3) uncurated sibling fields from
// <stats><client><global><stats>... (connection counts, byte rates,
// certificate-revocation stats, subscription operations) do not leak into
// the output.
func TestClientStatsResponse_RoundTrip(t *testing.T) {
	inner := extractInnerRPC(t, "testdata/show_stats_client.xml")

	var resp clientStatsResponse
	out := decodeAndMarshal(t, inner, "stats", &resp)

	if len(out) != 2 {
		t.Errorf("top-level should be {ingress, egress}; got %d keys", len(out))
	}

	ingress, ok := out["ingress"].(map[string]any)
	if !ok {
		t.Fatalf("ingress not a map: %T", out["ingress"])
	}
	egress, ok := out["egress"].(map[string]any)
	if !ok {
		t.Fatalf("egress not a map: %T", out["egress"])
	}

	wantIngress := []string{
		"totalIngressDiscards", "noSubscriptionMatch", "topicParseError",
		"parseError", "msgTooBig", "ttlExceeded", "webParseError",
		"publishTopicAcl", "msgSpoolDiscards", "messagePromotionCongestion",
		"messageSpoolCongestion",
	}
	for _, k := range wantIngress {
		if _, present := ingress[k]; !present {
			t.Errorf("ingress.%s not present (all 11 curated fields must round-trip)", k)
		}
	}
	if len(ingress) != len(wantIngress) {
		t.Errorf("ingress should have %d fields; got %d", len(wantIngress), len(ingress))
	}

	wantEgress := []string{
		"totalEgressDiscards", "transmitCongestion", "compressionCongestion",
		"messageElided", "ttlExceeded", "payloadCouldNotBeFormatted",
		"messagePromotionCongestion", "messageSpoolCongestion",
		"clientNotConnected", "msgSpoolEgressDiscards",
	}
	for _, k := range wantEgress {
		if _, present := egress[k]; !present {
			t.Errorf("egress.%s not present (all 10 curated fields must round-trip)", k)
		}
	}
	if len(egress) != len(wantEgress) {
		t.Errorf("egress should have %d fields; got %d", len(wantEgress), len(egress))
	}

	// Uncurated sibling fields from <stats><client><global><stats>... must
	// not leak. These belong in get-broker-status / get-client-info / future
	// tools — not in a discard-counter view.
	for _, dropped := range []string{
		"totalClients", "totalClientsConnected", "totalClientMessagesReceived",
		"currentIngressRatePerSecond", "totalClientBytesReceived",
		"certificateRevocationCheckStats", "keepaliveMsgsReceived",
		"deniedAuthorizationFailed", "addSubscriptionMsgsReceived",
		"clientControlMessagesReceived",
	} {
		if _, present := out[dropped]; present {
			t.Errorf("uncurated field %q should not appear in JSON output", dropped)
		}
	}
}
