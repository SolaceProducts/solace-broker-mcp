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

package sempv2

import "strings"

// ScrubEnvelope removes the broker's self-referential "links" object and any
// uri-bearing field inside "meta" from a parsed SEMP v2 response envelope
// ({"data":..., "meta":{...}, "links":{...}}), in place. Left unscrubbed,
// these fields leak the broker's management hostname:port, SEMP API variant
// (e.g. __private_config__), VPN name, and resource path through ordinary
// successful tool use — observed live on both a plain read (meta.request.uri,
// including the full select= query string) and a write (links.uri,
// links.subscriptionsUri) (SOL-155432).
//
// "links" is deleted unconditionally rather than type-asserted first: on a
// single-resource response it is an object, but on a collection response
// (e.g. getMsgVpnQueues) the swagger specs define it as an array, one entry
// per returned item — delete(m, "links") removes the key either way, so
// callers don't need to special-case the shape.
//
// "data" is deliberately untouched: a broker resource's own configured
// attributes live there, and nothing observed ever names a real attribute
// "uri" or "*Uri" the way the self-referential envelope fields do — this
// package has no business guessing that it's safe to strip fields there too.
//
// Shared by every caller that returns a SEMP v2 envelope to an MCP client:
// internal/composite/executor.go's scrubBrokerURIs (composite tool results)
// and internal/tools/queuemetrics's handler (the native get-queue-metrics
// tool). A caller with its own nested envelopes (composite's fan-out "byKey"
// rows) recurses into those itself and calls this once per envelope.
func ScrubEnvelope(m map[string]any) {
	if m == nil {
		return
	}
	delete(m, "links")
	if meta, ok := m["meta"].(map[string]any); ok {
		scrubURIFields(meta)
	}
}

// scrubURIFields deletes any key named "uri" or ending in "Uri" from m,
// recursing into nested maps — meta.paging.nextPageUri sits one level deeper
// than meta.request.uri, and a future SEMP response shape may nest another
// uri-bearing field differently again.
func scrubURIFields(m map[string]any) {
	for k, v := range m {
		if k == "uri" || strings.HasSuffix(k, "Uri") {
			delete(m, k)
			continue
		}
		if nested, ok := v.(map[string]any); ok {
			scrubURIFields(nested)
		}
	}
}
