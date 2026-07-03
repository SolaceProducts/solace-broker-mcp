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

package queuemetrics

import "encoding/xml"

// queueDetailResponse decodes the fields we need from a SEMPv1
// <rpc><show><queue><name>..</name><vpn-name>..</vpn-name><detail/></queue></show></rpc>
// response. The client strips the <rpc>...</rpc> envelope, so the InnerXML we
// unmarshal starts at <show>; the Go xml path tag walks down to the single
// <info> block.
//
// Unlike SEMPv2 MsgVpnQueue (whose *MsgCount fields are all cumulative), this
// SEMPv1 block carries the AUTHORITATIVE live state: num-messages-spooled is
// the current number of messages sitting in the queue, and it decreases as
// messages are consumed (SOL-150260). Only the live-state fields are decoded;
// everything else (config, redelivery policy, event thresholds) is already
// covered by the SEMPv2 half of get-queue-metrics.
type queueDetailResponse struct {
	XMLName xml.Name   `xml:"show"`
	Info    *queueInfo `xml:"queue>queues>queue>info"`
}

// queueInfo holds the curated live-state fields from the SEMPv1 queue <info>
// block. Pointer fields with json ",omitempty" so a field the broker omits
// (e.g. oldest/newest msg id on an empty queue) drops out of the output after
// the JSON round-trip rather than surfacing as a misleading zero.
type queueInfo struct {
	// CurrentMsgCount is the authoritative current queue depth — the number of
	// messages currently spooled in the queue right now. This is the field the
	// tool exists to surface; contrast SEMPv2 spooledMsgCount (cumulative).
	CurrentMsgCount *int64 `xml:"num-messages-spooled" json:"currentMsgCount,omitempty"`
	// CurrentSpoolUsageBytes is the live spool usage in bytes.
	CurrentSpoolUsageBytes *int64 `xml:"current-spool-usage-in-bytes" json:"currentSpoolUsageBytes,omitempty"`
	// DeliveredUnackedMsgCount is the SEMPv1 analogue of SEMPv2 txUnackedMsgCount.
	DeliveredUnackedMsgCount *int64 `xml:"total-delivered-unacked-msgs" json:"deliveredUnackedMsgCount,omitempty"`
	// HighWaterMarkBytes is the peak spool usage in bytes.
	HighWaterMarkBytes *int64 `xml:"high-water-mark-in-bytes" json:"highWaterMarkBytes,omitempty"`
	// OldestMsgID / NewestMsgID are present only when the queue holds messages.
	OldestMsgID *int64 `xml:"oldest-msg-id" json:"oldestMsgId,omitempty"`
	NewestMsgID *int64 `xml:"newest-msg-id" json:"newestMsgId,omitempty"`
}
