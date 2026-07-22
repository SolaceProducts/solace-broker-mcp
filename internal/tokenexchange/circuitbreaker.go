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

import (
	"context"
	"errors"
)

// The circuit breaker protects one shared IdP, so its counters must reflect
// IdP availability and nothing else. These three functions translate a Layer 2
// exchange outcome into one of gobreaker's three verdicts — excluded, failure,
// or success — and own the policy for which outcomes mean "the IdP is
// unhealthy". They must stay fast and side-effect-free: gobreaker invokes them
// while holding the lock that guards its internal counters, so no logging, no
// State()/Counts() calls, no blocking work.

// isBreakerExcluded reports outcomes that must count as neither success nor
// failure. Excluding them (rather than counting a success) keeps the failure
// rate honest: a caller cancelling, or the server rejecting a bad token, says
// nothing about whether the IdP is up. gobreaker consults this first — an
// excluded outcome never reaches isBreakerSuccess.
func isBreakerExcluded(err error) bool {
	if err == nil {
		return false // a real success is not an exclusion
	}

	// Not an availability signal at all: the caller went away, or the failure
	// originates inside this process rather than at the IdP.
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrExchangeRequestBuild) ||
		errors.Is(err, ErrExchangeMissingSubject) {
		return true
	}

	// A rate limit means the IdP is reachable and deliberately throttling;
	// treating it as an outage would let throttling trip the breaker for
	// everyone. Excluded regardless of whether retries were exhausted, which
	// is why the class must survive the exhaustion rewrap.
	if failureClassOf(err) == FailureClassRateLimited {
		return true
	}

	return false
}

// isBreakerSuccess reports whether an outcome counts as a success for breaker
// purposes. gobreaker calls this only for outcomes isBreakerExcluded did not
// exclude, so "not a counted failure" is the same as success here — including
// a rejection (errno at the IdP proves it is up).
func isBreakerSuccess(err error) bool {
	return !isBreakerFailure(err)
}

// isBreakerFailure reports the outcomes that indicate the IdP itself is
// unhealthy: no usable response (network), a server-side 5xx, or a transport
// interruption while reading the response body. A rejection is deliberately
// absent — the IdP answered, so it is available.
func isBreakerFailure(err error) bool {
	if err == nil {
		return false
	}
	switch failureClassOf(err) {
	case FailureClassNetwork, FailureClassUpstream5xx, FailureClassBodyRead:
		return true
	default:
		return false
	}
}

// failureClassOf extracts the FailureClass from an error's ExchangeError, or
// FailureClassNone if the error is not (or does not wrap) an *ExchangeError.
// Reaching through the wrapping chain is what lets classification survive the
// ErrExchangeRetriesExhausted rewrap.
func failureClassOf(err error) FailureClass {
	var exchErr *ExchangeError
	if errors.As(err, &exchErr) {
		return exchErr.FailureClass
	}
	return FailureClassNone
}
