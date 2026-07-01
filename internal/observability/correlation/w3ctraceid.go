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

package correlation

// Exported header names — the single definition of the inbound/outbound header
// contract. The inbound middleware reads these and the outbound correlationhdr
// package writes them, so an ID received on one hop is forwarded under the same
// names on the next. traceparent is the W3C Trace Context header
// (https://www.w3.org/TR/trace-context/); X-Correlation-ID carries the
// correlation identity for non-trace IDs and as a stable companion to traceparent.
const (
	HeaderTraceparent   = "traceparent"
	HeaderCorrelationID = "X-Correlation-ID"
)

// TraceIDLen is the length in characters of a W3C trace-id: 32 lowercase hex.
const TraceIDLen = 32

// zeroTraceID is the all-zero trace-id the W3C Trace Context spec defines as invalid.
const zeroTraceID = "00000000000000000000000000000000"

// ValidTraceID reports whether s is a valid W3C trace-id: exactly TraceIDLen (32)
// lowercase hex characters and not the all-zero value. Single shared definition used
// by the inbound middleware (parsing a received traceparent) and the outbound
// correlationhdr package (deciding whether to emit a traceparent), so both validate identically.
func ValidTraceID(s string) bool {
	return len(s) == TraceIDLen && s != zeroTraceID && isLowerHex(s)
}
