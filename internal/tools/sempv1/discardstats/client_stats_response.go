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

// clientStatsResponse decodes the curated discard subset from
// <rpc><show><stats><client/></stats></show></rpc>.
//
// The full broker response carries ~80 fields covering connection counts,
// message totals, byte counts, control-plane traffic, certificate revocation
// stats, and rate metrics. None of those are part of "are we dropping
// messages?". This struct keeps only the two <ingress-discards/> and
// <egress-discards/> sub-trees, which together carry the 21 client-level
// discard categories the broker tracks.
//
// See docs/internal/semp/get-discard-stats-curated-fields.md for rationale.
type clientStatsResponse struct {
	Client *clientGlobalT `xml:"client"`
}

type clientGlobalT struct {
	Global *clientGlobalStatsT `xml:"global"`
}

type clientGlobalStatsT struct {
	Stats *clientStatsT `xml:"stats"`
}

type clientStatsT struct {
	IngressDiscards *ingressDiscardsT `xml:"ingress-discards"`
	EgressDiscards  *egressDiscardsT  `xml:"egress-discards"`
}

// ingressDiscardsT mirrors <ingress-discards/> in show stats client.
type ingressDiscardsT struct {
	Total                      *int `xml:"total-ingress-discards" json:"totalIngressDiscards,omitempty"`
	NoSubscriptionMatch        *int `xml:"no-subscription-match" json:"noSubscriptionMatch,omitempty"`
	TopicParseError            *int `xml:"topic-parse-error" json:"topicParseError,omitempty"`
	ParseError                 *int `xml:"parse-error" json:"parseError,omitempty"`
	MsgTooBig                  *int `xml:"msg-too-big" json:"msgTooBig,omitempty"`
	TTLExceeded                *int `xml:"ttl-exceeded" json:"ttlExceeded,omitempty"`
	WebParseError              *int `xml:"web-parse-error" json:"webParseError,omitempty"`
	PublishTopicACL            *int `xml:"publish-topic-acl" json:"publishTopicAcl,omitempty"`
	MsgSpoolDiscards           *int `xml:"msg-spool-discards" json:"msgSpoolDiscards,omitempty"`
	MessagePromotionCongestion *int `xml:"message-promotion-congestion" json:"messagePromotionCongestion,omitempty"`
	MessageSpoolCongestion     *int `xml:"message-spool-congestion" json:"messageSpoolCongestion,omitempty"`
}

// egressDiscardsT mirrors <egress-discards/> in show stats client.
type egressDiscardsT struct {
	Total                       *int `xml:"total-egress-discards" json:"totalEgressDiscards,omitempty"`
	TransmitCongestion          *int `xml:"transmit-congestion" json:"transmitCongestion,omitempty"`
	CompressionCongestion       *int `xml:"compression-congestion" json:"compressionCongestion,omitempty"`
	MessageElided               *int `xml:"message-elided" json:"messageElided,omitempty"`
	TTLExceeded                 *int `xml:"ttl-exceeded" json:"ttlExceeded,omitempty"`
	PayloadCouldNotBeFormatted  *int `xml:"payload-could-not-be-formatted" json:"payloadCouldNotBeFormatted,omitempty"`
	MessagePromotionCongestion  *int `xml:"message-promotion-congestion" json:"messagePromotionCongestion,omitempty"`
	MessageSpoolCongestion      *int `xml:"message-spool-congestion" json:"messageSpoolCongestion,omitempty"`
	ClientNotConnected          *int `xml:"client-not-connected" json:"clientNotConnected,omitempty"`
	MsgSpoolEgressDiscards      *int `xml:"msg-spool-egress-discards" json:"msgSpoolEgressDiscards,omitempty"`
}

// IngressEgressDiscards returns the flattened {ingress, egress} structure
// suitable for JSON marshalling. Returns nil pointers (which marshal to
// omitempty=absent) when the broker omits the corresponding sub-tree.
func (r clientStatsResponse) IngressEgressDiscards() any {
	var ingress *ingressDiscardsT
	var egress *egressDiscardsT
	if r.Client != nil && r.Client.Global != nil && r.Client.Global.Stats != nil {
		ingress = r.Client.Global.Stats.IngressDiscards
		egress = r.Client.Global.Stats.EgressDiscards
	}
	return struct {
		Ingress *ingressDiscardsT `json:"ingress,omitempty"`
		Egress  *egressDiscardsT  `json:"egress,omitempty"`
	}{
		Ingress: ingress,
		Egress:  egress,
	}
}
