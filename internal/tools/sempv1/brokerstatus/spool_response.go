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

// messageSpoolResponse decodes the curated subset of the <message-spool>
// payload from <rpc><show><message-spool><detail/></message-spool></show></rpc>.
//
// The full SEMP response includes ~300 fields covering individual messages,
// per-VPN spool stats, rate metrics, transaction operation counters, and
// detailed spool-files / spool-sync sub-trees. None of those belong in an
// operational status snapshot — operators inspect them via dedicated stats
// / VPN tools. This struct keeps only the operator-cited status signals
// (HA state, utilization percentages, recent failures, fragmentation).
//
// See docs/internal/semp/get-broker-status-curated-fields.md for the full
// rationale.
type messageSpoolResponse struct {
	MessageSpoolInfo *messageSpoolInfoT `xml:"message-spool-info" json:"messageSpoolInfo,omitempty"`
}

type messageSpoolInfoT struct {
	// HA / operational state
	ConfigStatus          *string `xml:"config-status" json:"configStatus,omitempty"`
	OperationalStatus     *string `xml:"operational-status" json:"operationalStatus,omitempty"`
	DatapathUp            *string `xml:"datapath-up" json:"datapathUp,omitempty"`
	SynchronizationStatus *string `xml:"synchronization-status" json:"synchronizationStatus,omitempty"`
	SpoolSyncStatus       *string `xml:"spool-sync-status" json:"spoolSyncStatus,omitempty"`

	// Capacity / utilization
	ActiveDiskPartitionUsage                    *string `xml:"active-disk-partition-usage" json:"activeDiskPartitionUsage,omitempty"`
	MessageCountUtilizationPercentage           *string `xml:"message-count-utilization-percentage" json:"messageCountUtilizationPercentage,omitempty"`
	SpoolFilesUtilizationPercentage             *string `xml:"spool-files-utilization-percentage" json:"spoolFilesUtilizationPercentage,omitempty"`
	TransactionResourceUtilizationPercentage    *string `xml:"transaction-resource-utilization-percentage" json:"transactionResourceUtilizationPercentage,omitempty"`
	TransactedSessionResourceUtilizationPercent *string `xml:"transacted-session-resource-utilization-percentage" json:"transactedSessionResourceUtilizationPercentage,omitempty"`
	DeliveredUnackedMsgsUtilizationPercentage   *string `xml:"delivered-unacked-msgs-utilization-percentage" json:"deliveredUnackedMsgsUtilizationPercentage,omitempty"`

	// Spool counts / quotas
	TotalMessagesCurrentlySpooled *int    `xml:"total-messages-currently-spooled" json:"totalMessagesCurrentlySpooled,omitempty"`
	MaxMessageCount               *string `xml:"max-message-count" json:"maxMessageCount,omitempty"`

	// Sparse-spool / fragmentation
	DefragEstFragmentationPercentage *int `xml:"defrag-est-fragmentation-percentage" json:"defragEstFragmentationPercentage,omitempty"`

	// Recent failure context
	LastFailureReason *string `xml:"last-failure-reason" json:"lastFailureReason,omitempty"`
	LastFailureTime   *string `xml:"last-failure-time" json:"lastFailureTime,omitempty"`
}
