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

package tokenexchange

// FailureClass distinguishes the transport causes that all share the
// ErrExchangeTransport sentinel. The sentinel must stay coarse (AgentMessage
// and the retryable classification treat these causes identically), but the
// circuit breaker needs them apart: a 429 is excluded from the failure rate
// while a 5xx or network error counts. HTTPStatus can't carry this — it is
// zero for network, body-read, and deadline alike.
type FailureClass int

const (
	// FailureClassNone is the zero value: no transport sub-cause applies.
	FailureClassNone FailureClass = iota

	FailureClassNetwork
	FailureClassUpstream5xx

	// FailureClassRateLimited exists so the breaker can exclude a 429: a
	// rate limit is not an availability failure.
	FailureClassRateLimited

	FailureClassBodyRead
)

// String is the log-safe label emitted in LogAttrs.
func (f FailureClass) String() string {
	switch f {
	case FailureClassNetwork:
		return "network"
	case FailureClassUpstream5xx:
		return "upstream_5xx"
	case FailureClassRateLimited:
		return "rate_limited"
	case FailureClassBodyRead:
		return "body_read"
	default:
		return "none"
	}
}
