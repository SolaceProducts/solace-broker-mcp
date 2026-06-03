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

// vpnStatsResponse decodes the curated discard subset from
// <rpc><show><message-vpn><vpn-name>X</vpn-name><stats/></message-vpn></show></rpc>.
//
// The full per-VPN response carries ~120 fields. This struct keeps only the
// <ingress-discards/> and <egress-discards/> sub-trees — 10 ingress + 9
// egress categories. These are a near-complete subset of the broker-wide
// client-stats view, missing only web-parse-error (broker-global only) and
// payload-could-not-be-formatted (protocol-bridge transforms are not
// VPN-scoped). Spool-level discards are not VPN-scoped at the SEMPv1 layer
// at all and are omitted from this response (the broker-wide call returns
// them).
//
// See docs/internal/semp/get-discard-stats-curated-fields.md for the gap
// rationale.
type vpnStatsResponse struct {
	VPN *vpnStatsVPNT `xml:"vpn"`
}

type vpnStatsVPNT struct {
	Stats *vpnStatsT `xml:"stats"`
}

type vpnStatsT struct {
	IngressDiscards *vpnIngressDiscardsT `xml:"ingress-discards"`
	EgressDiscards  *vpnEgressDiscardsT  `xml:"egress-discards"`
}

type vpnIngressDiscardsT struct {
	Total                      *int `xml:"total-ingress-discards" json:"totalIngressDiscards,omitempty"`
	NoSubscriptionMatch        *int `xml:"no-subscription-match" json:"noSubscriptionMatch,omitempty"`
	TopicParseError            *int `xml:"topic-parse-error" json:"topicParseError,omitempty"`
	ParseError                 *int `xml:"parse-error" json:"parseError,omitempty"`
	MsgTooBig                  *int `xml:"msg-too-big" json:"msgTooBig,omitempty"`
	TTLExceeded                *int `xml:"ttl-exceeded" json:"ttlExceeded,omitempty"`
	PublishTopicACL            *int `xml:"publish-topic-acl" json:"publishTopicAcl,omitempty"`
	MsgSpoolDiscards           *int `xml:"msg-spool-discards" json:"msgSpoolDiscards,omitempty"`
	MessagePromotionCongestion *int `xml:"message-promotion-congestion" json:"messagePromotionCongestion,omitempty"`
	MessageSpoolCongestion     *int `xml:"message-spool-congestion" json:"messageSpoolCongestion,omitempty"`
}

type vpnEgressDiscardsT struct {
	Total                      *int `xml:"total-egress-discards" json:"totalEgressDiscards,omitempty"`
	TransmitCongestion         *int `xml:"transmit-congestion" json:"transmitCongestion,omitempty"`
	CompressionCongestion      *int `xml:"compression-congestion" json:"compressionCongestion,omitempty"`
	MessageElided              *int `xml:"message-elided" json:"messageElided,omitempty"`
	TTLExceeded                *int `xml:"ttl-exceeded" json:"ttlExceeded,omitempty"`
	MessagePromotionCongestion *int `xml:"message-promotion-congestion" json:"messagePromotionCongestion,omitempty"`
	MessageSpoolCongestion     *int `xml:"message-spool-congestion" json:"messageSpoolCongestion,omitempty"`
	ClientNotConnected         *int `xml:"client-not-connected" json:"clientNotConnected,omitempty"`
	MsgSpoolEgressDiscards     *int `xml:"msg-spool-egress-discards" json:"msgSpoolEgressDiscards,omitempty"`
}

// IngressEgressDiscards returns the flattened {ingress, egress} structure.
func (r vpnStatsResponse) IngressEgressDiscards() any {
	var ingress *vpnIngressDiscardsT
	var egress *vpnEgressDiscardsT
	if r.VPN != nil && r.VPN.Stats != nil {
		ingress = r.VPN.Stats.IngressDiscards
		egress = r.VPN.Stats.EgressDiscards
	}
	return struct {
		Ingress *vpnIngressDiscardsT `json:"ingress,omitempty"`
		Egress  *vpnEgressDiscardsT  `json:"egress,omitempty"`
	}{
		Ingress: ingress,
		Egress:  egress,
	}
}
