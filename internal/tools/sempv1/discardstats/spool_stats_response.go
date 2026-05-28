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

// spoolStatsResponse decodes the curated discard subset from
// <rpc><show><message-spool><stats/></message-spool></show></rpc>.
//
// The full broker response carries ~50 fields including current spool
// throughput rates, transaction operation counters, redelivery counts, and
// total spooled messages. The curated subset below keeps only the discard
// counters operators inspect when investigating "where did my messages go?":
// quota-exceeded, max-redelivery (with DMQ split), TTL-expired (with DMQ
// split), low-priority congestion, replication-standby drops, and a small
// set of structural-failure counters (spool-to-disk fail, sequence-number
// errors). Throughput and capacity belong in get-broker-health.
//
// See docs/internal/semp/get-discard-stats-curated-fields.md for rationale.
type spoolStatsResponse struct {
	Stats *messageSpoolStatsT `xml:"message-spool-stats"`
}

// messageSpoolStatsT mirrors the curated subset of <message-spool-stats/> in
// show message-spool stats. Field tags include both XML and JSON tags so the
// JSON round-trip in handler.go produces camelCase keys with omitempty.
type messageSpoolStatsT struct {
	// Quota / capacity
	DiscardSpoolOverQuota         *int `xml:"discard-spool-over-quota" json:"discardSpoolOverQuota,omitempty"`
	DiscardQEndptOverQuota        *int `xml:"discard-qendpt-over-quota" json:"discardQueueEndpointOverQuota,omitempty"`
	DiscardReplayLogOverQuota     *int `xml:"discard-replay-log-over-quota" json:"discardReplayLogOverQuota,omitempty"`
	DiscardMaxMsgUsageExceeded    *int `xml:"discard-max-msg-usage-exceeded" json:"discardMaxMsgUsageExceeded,omitempty"`
	DiscardMaxMsgSizeExceeded     *int `xml:"discard-max-msg-size-exceeded" json:"discardMaxMsgSizeExceeded,omitempty"`
	DiscardSpoolFileLimitExceeded *int `xml:"discard-spool-file-limit-exceeded" json:"discardSpoolFileLimitExceeded,omitempty"`

	// Message-spool path failures
	DiscardSpoolToADBFail  *int `xml:"discard-spool-to-adb-fail" json:"discardSpoolToAdbFail,omitempty"`
	DiscardSpoolToDiskFail *int `xml:"discard-spool-to-disk-fail" json:"discardSpoolToDiskFail,omitempty"`
	SpoolShutdownDiscard   *int `xml:"spool-shutdown-discard" json:"spoolShutdownDiscard,omitempty"`

	// Routing / delivery failures
	DiscardNoDest         *int `xml:"discard-nodest" json:"discardNoDest,omitempty"`
	DiscardQueueNotFound  *int `xml:"discard-queue-not-found" json:"discardQueueNotFound,omitempty"`
	NoLocalDeliveryDiscard *int `xml:"no-local-delivery-discard" json:"noLocalDeliveryDiscard,omitempty"`
	DiscardOther          *int `xml:"discard-other" json:"discardOther,omitempty"`

	// Congestion / scheduling
	LowPriorityMsgCongestionDiscard *int `xml:"low-priority-msg-congestion-discard" json:"lowPriorityMsgCongestionDiscard,omitempty"`

	// Replication
	ReplicationIsStandbyDiscard      *int `xml:"replication-is-standby-discard" json:"replicationIsStandbyDiscard,omitempty"`
	SyncReplicationIneligibleDiscard *int `xml:"sync-replication-ineligible-discard" json:"syncReplicationIneligibleDiscard,omitempty"`

	// TTL — the broker emits both "discard" (no DMQ route) and "to-dmq"
	// (routed to DMQ) counts; keep both so operators can tell whether
	// messages were lost or just rerouted.
	TotalTTLExpiredDiscardMessages *int `xml:"total-ttl-expired-discard-messages" json:"totalTtlExpiredDiscardMessages,omitempty"`
	TotalTTLExpiredToDMQMessages   *int `xml:"total-ttl-expired-to-dmq-messages" json:"totalTtlExpiredToDmqMessages,omitempty"`
	TotalTTLExpiredToDMQFailures   *int `xml:"total-ttl-expired-to-dmq-failures" json:"totalTtlExpiredToDmqFailures,omitempty"`
	TotalTTLExceededDiscardMessages *int `xml:"total-ttl-exceeded-discard-messages" json:"totalTtlExceededDiscardMessages,omitempty"`

	// Max-redelivery — same DMQ split as TTL.
	MaxRedeliveryExceededDiscardMessages *int `xml:"max-redelivery-exceeded-discard-messages" json:"maxRedeliveryExceededDiscardMessages,omitempty"`
	MaxRedeliveryExceededToDMQMessages   *int `xml:"max-redelivery-exceeded-to-dmq-messages" json:"maxRedeliveryExceededToDmqMessages,omitempty"`
	MaxRedeliveryExceededToDMQFailures   *int `xml:"max-redelivery-exceeded-to-dmq-failures" json:"maxRedeliveryExceededToDmqFailures,omitempty"`

	// Sequence-number / ordering
	SeqNumMessagesDiscarded *int `xml:"seq-num-messages-discarded" json:"seqNumMessagesDiscarded,omitempty"`

	// Aggregate roll-ups
	TotalDiscardedMessages       *int `xml:"total-discarded-messages" json:"totalDiscardedMessages,omitempty"`
	TotalDiscardedEgressMessages *int `xml:"total-discarded-egress-messages" json:"totalDiscardedEgressMessages,omitempty"`
}

// Discards returns the curated stats payload, or an empty struct when the
// broker omits <message-spool-stats>.
func (r spoolStatsResponse) Discards() any {
	if r.Stats == nil {
		return struct{}{}
	}
	return r.Stats
}
