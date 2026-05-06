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

// memoryResponse decodes the curated subset of the <memory> payload from
// <rpc><show><memory/></show></rpc>. Operators monitor the percentage
// signals (Datadog alarms on these); raw memory pool sizes, IPC buffer
// pools, slot infos, and event thresholds are decoded by other tools when
// needed and are not surfaced for broker-health triage. See
// docs/semp/get-broker-health-curated-fields.md for the full rationale.
type memoryResponse struct {
	PhysicalMemoryUsagePercent     *float64 `xml:"physical-memory-usage-percent" json:"physicalMemoryUsagePercent,omitempty"`
	SubscriptionMemoryUsagePercent *float64 `xml:"subscription-memory-usage-percent" json:"subscriptionMemoryUsagePercent,omitempty"`
}
