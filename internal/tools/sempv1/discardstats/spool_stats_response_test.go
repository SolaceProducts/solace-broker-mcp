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

// TestSpoolStatsResponse_RoundTrip decodes the live show-message-spool-stats
// fixture, runs it through Discards(), and asserts (1) all 26 curated
// discard counters appear with their expected JSON keys, (2) discard-like
// fields the broker also emits but we deliberately excluded (discard-ooo,
// discard-duplicate, smf-ttl-exceeded, publish-acl-denied, user-profile-
// deny-guaranteed, etc.) do not leak, and (3) throughput / capacity /
// transaction counters (ingress-messages, spooled-to-adb, open-session,
// transactions, current-bind-rate, etc.) do not leak. The last two
// categories together catch any accidental field added to messageSpoolStatsT.
func TestSpoolStatsResponse_RoundTrip(t *testing.T) {
	inner := extractInnerRPC(t, "testdata/show_message_spool_stats.xml")

	var resp spoolStatsResponse
	out := decodeAndMarshal(t, inner, "message-spool", &resp)

	wantCurated := []string{
		// Quota / capacity
		"discardSpoolOverQuota", "discardQueueEndpointOverQuota",
		"discardReplayLogOverQuota", "discardMaxMsgUsageExceeded",
		"discardMaxMsgSizeExceeded", "discardSpoolFileLimitExceeded",
		// Storage path failures
		"discardSpoolToAdbFail", "discardSpoolToDiskFail",
		"spoolShutdownDiscard",
		// Routing / delivery failures
		"discardNoDest", "discardQueueNotFound", "noLocalDeliveryDiscard",
		"discardOther",
		// Congestion
		"lowPriorityMsgCongestionDiscard",
		// Replication
		"replicationIsStandbyDiscard", "syncReplicationIneligibleDiscard",
		// TTL (with DMQ split)
		"totalTtlExpiredDiscardMessages", "totalTtlExpiredToDmqMessages",
		"totalTtlExpiredToDmqFailures", "totalTtlExceededDiscardMessages",
		// Max-redelivery (with DMQ split)
		"maxRedeliveryExceededDiscardMessages",
		"maxRedeliveryExceededToDmqMessages",
		"maxRedeliveryExceededToDmqFailures",
		// Sequence-number
		"seqNumMessagesDiscarded",
		// Aggregate roll-ups
		"totalDiscardedMessages", "totalDiscardedEgressMessages",
	}
	for _, k := range wantCurated {
		if _, present := out[k]; !present {
			t.Errorf("curated %q not present (all 26 curated fields must round-trip)", k)
		}
	}
	if len(out) != len(wantCurated) {
		t.Errorf("top-level should have exactly %d curated keys; got %d", len(wantCurated), len(out))
	}

	// Uncurated discard-like fields the broker emits but we deliberately
	// dropped because they are not actionable for "where did messages go?".
	// A leak here means the curated struct accidentally grew a sibling field.
	for _, dropped := range []string{
		"discardSpoolingNotReady", "discardOoo", "discardDuplicate",
		"discardRemoteRouterSpoolingNotSupported", "discardErroredMessage",
		"userProfileDenyGuaranteed", "discardPublisherNotFound",
		"smfTtlExceeded", "publishAclDenied", "destinationGroupError",
		"notCompatibleWithForwardingMode", "xaTransactionNotSupported",
	} {
		if _, present := out[dropped]; present {
			t.Errorf("uncurated discard-like field %q should not appear (deliberately excluded)", dropped)
		}
	}

	// Throughput / capacity / transaction counters are out of scope — they
	// belong in get-broker-health or future tools.
	for _, dropped := range []string{
		"vpnName", "ingressMessages", "egressMessages", "spooledToAdb",
		"spooledToDisk", "retrieveFromAdb", "confirmedDelivered",
		"openSession", "transactions", "xaTransactions",
		"xaTransactionsSuccessOperations", "currentBindRatePerSecond",
		"totalDeletedMessages", "requestForRedelivery", "replaysInitiated",
	} {
		if _, present := out[dropped]; present {
			t.Errorf("non-discard field %q should not appear (belongs in a different tool)", dropped)
		}
	}
}
