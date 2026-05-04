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

// versionResponse decodes the curated subset of the <version> payload from
// <rpc><show><version/></show></rpc>. The full XSD-aligned shape was reviewed
// during planning (see docs/semp/get-broker-health-curated-fields.md); only
// the operator-cited health signals remain. Optional fields use pointer +
// omitempty so older brokers that omit a field produce an absent JSON key
// rather than a zero value.
type versionResponse struct {
	Description *string  `xml:"description" json:"description,omitempty"`
	Uptime      *uptimeT `xml:"uptime" json:"uptime,omitempty"`
}

// uptimeT keeps only total-secs — the numeric uptime value operators alarm on.
// The broker also emits days/hours/mins/secs in the same envelope but those
// are derivable from total-secs and add noise to LLM context.
type uptimeT struct {
	TotalSecs *int `xml:"total-secs" json:"totalSecs,omitempty"`
}
