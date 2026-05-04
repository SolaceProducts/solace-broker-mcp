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

package brokerhealth

// systemResponse decodes the curated subset of the <system> payload from
// <rpc><show><system/></show></rpc>. See docs/semp/get-broker-health-curated-fields.md
// for the rationale behind each kept field.
//
// Three groups are surfaced:
//   - uptime/restart context (operator first-pass triage)
//   - scaling tier limits (broker capacity tier)
//   - system resources, available vs required (under-scaling detection)
//
// Pointer + omitempty everywhere so older brokers that omit a field produce
// an absent JSON key rather than a zero value.
type systemResponse struct {
	// Uptime / restart
	SystemUptimeSeconds *int    `xml:"system-uptime-seconds" json:"systemUptimeSeconds,omitempty"`
	LastRestartReason   *string `xml:"last-restart-reason" json:"lastRestartReason,omitempty"`

	// Scaling — broker-tier limits
	MaxBridges                *int `xml:"max-bridges" json:"maxBridges,omitempty"`
	MaxConnections            *int `xml:"max-connections" json:"maxConnections,omitempty"`
	MaxQueueMessages          *int `xml:"max-queue-messages" json:"maxQueueMessages,omitempty"`
	MaxKafkaBridges           *int `xml:"max-kafka-bridges" json:"maxKafkaBridges,omitempty"`
	MaxKafkaBrokerConnections *int `xml:"max-kafka-broker-connections" json:"maxKafkaBrokerConnections,omitempty"`
	MaxSubscriptions          *int `xml:"max-subscriptions" json:"maxSubscriptions,omitempty"`
	MaxGuaranteedMessageSize  *int `xml:"max-guaranteed-message-size" json:"maxGuaranteedMessageSize,omitempty"`

	// System resources — available vs required pairs
	CPUCores                  *int     `xml:"cpu-cores" json:"cpuCores,omitempty"`
	CPUCoresRequired          *int     `xml:"cpu-cores-required" json:"cpuCoresRequired,omitempty"`
	HostVirtualMemory         *float64 `xml:"host-virtual-memory" json:"hostVirtualMemory,omitempty"`
	HostVirtualMemoryRequired *float64 `xml:"host-virtual-memory-required" json:"hostVirtualMemoryRequired,omitempty"`
	MemoryCgroupLimit         *float64 `xml:"memory-cgroup-limit" json:"memoryCgroupLimit,omitempty"`
	MemoryCgroupLimitRequired *float64 `xml:"memory-cgroup-limit-required" json:"memoryCgroupLimitRequired,omitempty"`
	SharedMemory              *float64 `xml:"shared-memory" json:"sharedMemory,omitempty"`
	SharedMemoryRequired      *float64 `xml:"shared-memory-required" json:"sharedMemoryRequired,omitempty"`
}
