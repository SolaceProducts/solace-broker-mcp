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

import "testing"

// TestVPNStatsResponse_RoundTrip decodes the live per-VPN
// show-message-vpn-stats fixture, runs it through IngressEgressDiscards(),
// and asserts (1) the top-level shape is exactly {ingress, egress}, (2) all
// 10 ingress and 9 egress per-VPN curated categories are present, (3) the
// two broker-global-only fields (webParseError on ingress,
// payloadCouldNotBeFormatted on egress) are absent — the broker does not
// emit them at the per-VPN layer, and the struct must not invent them, and
// (4) uncurated sibling fields from <message-vpn><vpn>... (connection
// counts, authentication, subscription totals, byte rates) do not leak.
func TestVPNStatsResponse_RoundTrip(t *testing.T) {
	inner := extractInnerRPC(t, "testdata/show_message_vpn_default_stats.xml")

	var resp vpnStatsResponse
	out := decodeAndMarshal(t, inner, "message-vpn", &resp)

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

	// Per-VPN ingress: 10 curated fields, no webParseError (broker-global).
	wantIngress := []string{
		"totalIngressDiscards", "noSubscriptionMatch", "topicParseError",
		"parseError", "msgTooBig", "ttlExceeded", "publishTopicAcl",
		"msgSpoolDiscards", "messagePromotionCongestion",
		"messageSpoolCongestion",
	}
	for _, k := range wantIngress {
		if _, present := ingress[k]; !present {
			t.Errorf("ingress.%s not present (all 10 per-VPN curated fields must round-trip)", k)
		}
	}
	if len(ingress) != len(wantIngress) {
		t.Errorf("per-VPN ingress should have %d fields; got %d", len(wantIngress), len(ingress))
	}
	if _, present := ingress["webParseError"]; present {
		t.Error("ingress.webParseError must not appear in per-VPN response (broker-global only)")
	}

	// Per-VPN egress: 9 curated fields, no payloadCouldNotBeFormatted.
	wantEgress := []string{
		"totalEgressDiscards", "transmitCongestion", "compressionCongestion",
		"messageElided", "ttlExceeded", "messagePromotionCongestion",
		"messageSpoolCongestion", "clientNotConnected",
		"msgSpoolEgressDiscards",
	}
	for _, k := range wantEgress {
		if _, present := egress[k]; !present {
			t.Errorf("egress.%s not present (all 9 per-VPN curated fields must round-trip)", k)
		}
	}
	if len(egress) != len(wantEgress) {
		t.Errorf("per-VPN egress should have %d fields; got %d", len(wantEgress), len(egress))
	}
	if _, present := egress["payloadCouldNotBeFormatted"]; present {
		t.Error("egress.payloadCouldNotBeFormatted must not appear in per-VPN response (broker-global only)")
	}

	// Uncurated sibling fields from <message-vpn><vpn>... must not leak.
	// These are VPN metadata / capacity / throughput, not discard counters.
	for _, dropped := range []string{
		"name", "alias", "enabled", "operational", "connections",
		"maxConnections", "authentication", "basicAuth",
		"totalClientMessagesReceived", "totalClientBytesReceived",
		"uniqueSubscriptions", "certificateRevocationCheckStats",
		"currentIngressRatePerSecond", "stats",
	} {
		if _, present := out[dropped]; present {
			t.Errorf("uncurated field %q should not appear in JSON output", dropped)
		}
	}
}
