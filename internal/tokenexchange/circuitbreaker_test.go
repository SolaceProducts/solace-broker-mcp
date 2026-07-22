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
	"testing"

	"github.com/sony/gobreaker/v2"
)

// exhaustedWith wraps a transport-class *ExchangeError the way
// classifyRetryOutcome does: it replaces the sentinel with
// ErrExchangeRetriesExhausted but copies FailureClass forward. The breaker
// classification must produce the same verdict for the exhausted form as for
// the raw form, so tests build both from one class.
func exhaustedWith(class FailureClass) *ExchangeError {
	return &ExchangeError{Sentinel: ErrExchangeRetriesExhausted, FailureClass: class}
}

func rawWith(class FailureClass) *ExchangeError {
	return &ExchangeError{Sentinel: ErrExchangeTransport, FailureClass: class}
}

func TestIsBreakerFailure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not a failure", nil, false},

		{"raw network", rawWith(FailureClassNetwork), true},
		{"raw 5xx", rawWith(FailureClassUpstream5xx), true},
		{"raw body-read", rawWith(FailureClassBodyRead), true},

		// Same verdicts must hold after the exhaustion rewrap — this is the
		// whole reason FailureClass is carried through it.
		{"exhausted network", exhaustedWith(FailureClassNetwork), true},
		{"exhausted 5xx", exhaustedWith(FailureClassUpstream5xx), true},
		{"exhausted body-read", exhaustedWith(FailureClassBodyRead), true},

		{"rate-limited is not a failure", rawWith(FailureClassRateLimited), false},
		{"exhausted rate-limited is not a failure", exhaustedWith(FailureClassRateLimited), false},

		{"config fault is not a failure", rawWith(FailureClassConfig), false},
		{"exhausted config fault is not a failure", exhaustedWith(FailureClassConfig), false},

		{"rejection is not a failure", &ExchangeError{Sentinel: ErrExchangeRejected}, false},
		{"invalid response is not a failure", &ExchangeError{Sentinel: ErrInvalidResponse}, false},
		{"request build is not a failure", &ExchangeError{Sentinel: ErrExchangeRequestBuild}, false},
		{"non-exchange error is not a failure", errors.New("something else"), false},
		{"context canceled is not a failure", context.Canceled, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isBreakerFailure(tc.err); got != tc.want {
				t.Errorf("isBreakerFailure(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsBreakerExcluded(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not excluded", nil, false},

		{"context canceled", context.Canceled, true},
		{"context deadline", context.DeadlineExceeded, true},
		{"request build", &ExchangeError{Sentinel: ErrExchangeRequestBuild}, true},
		{"missing subject", &ExchangeError{Sentinel: ErrExchangeMissingSubject}, true},

		{"rate-limited excluded", rawWith(FailureClassRateLimited), true},
		{"exhausted rate-limited excluded", exhaustedWith(FailureClassRateLimited), true},

		{"config fault excluded", rawWith(FailureClassConfig), true},
		{"exhausted config fault excluded", exhaustedWith(FailureClassConfig), true},

		{"network not excluded", rawWith(FailureClassNetwork), false},
		{"5xx not excluded", rawWith(FailureClassUpstream5xx), false},
		{"body-read not excluded", rawWith(FailureClassBodyRead), false},
		{"rejection not excluded", &ExchangeError{Sentinel: ErrExchangeRejected}, false},
		{"invalid response not excluded", &ExchangeError{Sentinel: ErrInvalidResponse}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isBreakerExcluded(tc.err); got != tc.want {
				t.Errorf("isBreakerExcluded(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsBreakerSuccess spot-checks that the success verdict is the negation of
// the failure verdict (gobreaker only consults it for non-excluded outcomes,
// so a rejection reaching it counts as success — the IdP answered).
func TestIsBreakerSuccess(t *testing.T) {
	t.Parallel()
	success := []error{
		nil,
		&ExchangeError{Sentinel: ErrExchangeRejected},
		&ExchangeError{Sentinel: ErrInvalidResponse},
	}
	for _, err := range success {
		if !isBreakerSuccess(err) {
			t.Errorf("isBreakerSuccess(%v) = false, want true", err)
		}
	}
	failure := []error{
		rawWith(FailureClassNetwork),
		rawWith(FailureClassUpstream5xx),
		exhaustedWith(FailureClassUpstream5xx),
	}
	for _, err := range failure {
		if isBreakerSuccess(err) {
			t.Errorf("isBreakerSuccess(%v) = true, want false", err)
		}
	}
}

// TestNewReadyToTrip pins newReadyToTrip's two independent rules (see its
// "Regime gap" doc comment): a consecutive-failure count, and a failure-rate
// rule gated by a minimum sample. Most cases isolate one rule by disabling or
// starving the other, since the two thresholds are otherwise unrelated.
func TestNewReadyToTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		cfg    func() CircuitBreakerConfig
		counts gobreaker.Counts
		want   bool
	}{
		// Rate rule: DefaultCircuitBreakerConfig (MinimumRequests=10,
		// FailureRateThresholdPercent=50) with the consecutive rule disabled
		// (threshold 0) so these cases isolate the rate rule.
		{
			name: "below minimum requests never trips, even at 100% failure",
			cfg: func() CircuitBreakerConfig {
				cfg := DefaultCircuitBreakerConfig()
				cfg.ConsecutiveFailureThreshold = 0
				return cfg
			},
			counts: gobreaker.Counts{Requests: 9, TotalFailures: 9},
			want:   false,
		},
		{
			name: "exactly at minimum requests and exactly at threshold rate trips",
			cfg: func() CircuitBreakerConfig {
				cfg := DefaultCircuitBreakerConfig()
				cfg.ConsecutiveFailureThreshold = 0
				return cfg
			},
			counts: gobreaker.Counts{Requests: 10, TotalSuccesses: 5, TotalFailures: 5},
			want:   true,
		},
		{
			name: "at minimum requests but below threshold rate does not trip",
			cfg: func() CircuitBreakerConfig {
				cfg := DefaultCircuitBreakerConfig()
				cfg.ConsecutiveFailureThreshold = 0
				return cfg
			},
			counts: gobreaker.Counts{Requests: 10, TotalSuccesses: 6, TotalFailures: 4},
			want:   false,
		},
		{
			name: "large sample just under threshold rate does not trip",
			cfg: func() CircuitBreakerConfig {
				cfg := DefaultCircuitBreakerConfig()
				cfg.ConsecutiveFailureThreshold = 0
				return cfg
			},
			counts: gobreaker.Counts{Requests: 1000, TotalSuccesses: 501, TotalFailures: 499},
			want:   false,
		},
		{
			name: "exclusions cannot fill the minimum-requests floor",
			cfg: func() CircuitBreakerConfig {
				cfg := DefaultCircuitBreakerConfig()
				cfg.ConsecutiveFailureThreshold = 0
				return cfg
			},
			counts: gobreaker.Counts{Requests: 1009, TotalFailures: 9, TotalExclusions: 1000},
			want:   false,
		},
		{
			name: "exclusions do not dilute an already-tripping rate",
			cfg: func() CircuitBreakerConfig {
				cfg := DefaultCircuitBreakerConfig()
				cfg.ConsecutiveFailureThreshold = 0
				return cfg
			},
			counts: gobreaker.Counts{Requests: 110, TotalSuccesses: 5, TotalFailures: 5, TotalExclusions: 100},
			want:   true,
		},
		{
			name: "zero evaluated does not trip and does not panic",
			cfg: func() CircuitBreakerConfig {
				cfg := DefaultCircuitBreakerConfig()
				cfg.ConsecutiveFailureThreshold = 0
				return cfg
			},
			counts: gobreaker.Counts{},
			want:   false,
		},

		// Consecutive rule: MinimumRequests set far above any test sample so
		// the rate rule can never fire, isolating the consecutive rule.
		{
			name: "consecutive failures at threshold trips",
			cfg: func() CircuitBreakerConfig {
				cfg := DefaultCircuitBreakerConfig()
				cfg.MinimumRequests = 1_000_000
				return cfg
			},
			counts: gobreaker.Counts{ConsecutiveFailures: 5},
			want:   true,
		},
		{
			name: "consecutive failures one below threshold does not trip",
			cfg: func() CircuitBreakerConfig {
				cfg := DefaultCircuitBreakerConfig()
				cfg.MinimumRequests = 1_000_000
				return cfg
			},
			counts: gobreaker.Counts{ConsecutiveFailures: 4},
			want:   false,
		},
		{
			name: "zero threshold disables the consecutive rule even at huge counts",
			cfg: func() CircuitBreakerConfig {
				cfg := DefaultCircuitBreakerConfig()
				cfg.MinimumRequests = 1_000_000
				cfg.ConsecutiveFailureThreshold = 0
				return cfg
			},
			counts: gobreaker.Counts{ConsecutiveFailures: 1000},
			want:   false,
		},

		// Both rules live, as shipped: pins that the consecutive rule is
		// checked before the sample floor, so it can trip even when the rate
		// rule's MinimumRequests sample is unmet.
		{
			name: "consecutive rule trips ahead of an unmet sample floor",
			cfg: func() CircuitBreakerConfig {
				return DefaultCircuitBreakerConfig()
			},
			counts: gobreaker.Counts{Requests: 5, TotalFailures: 5, ConsecutiveFailures: 5},
			want:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			readyToTrip := newReadyToTrip(tc.cfg())
			if got := readyToTrip(tc.counts); got != tc.want {
				t.Errorf("newReadyToTrip(...)(%+v) = %v, want %v", tc.counts, got, tc.want)
			}
		})
	}
}

// TestBreakerClassification_ExcludedNeverCounted pins the invariant gobreaker's
// callback order depends on: any outcome the breaker excludes must NOT also be
// classified as a failure, or a rate limit / cancellation would both dilute and
// trip the breaker depending on ordering. Excluded wins by contract, but the
// two predicates should not disagree in the first place.
func TestBreakerClassification_ExcludedNeverCounted(t *testing.T) {
	t.Parallel()
	excluded := []error{
		context.Canceled,
		context.DeadlineExceeded,
		&ExchangeError{Sentinel: ErrExchangeRequestBuild},
		&ExchangeError{Sentinel: ErrExchangeMissingSubject},
		rawWith(FailureClassRateLimited),
		exhaustedWith(FailureClassRateLimited),
		rawWith(FailureClassConfig),
		exhaustedWith(FailureClassConfig),
	}
	for _, err := range excluded {
		if !isBreakerExcluded(err) {
			t.Fatalf("test setup: %v should be excluded", err)
		}
		if isBreakerFailure(err) {
			t.Errorf("%v is both excluded AND a failure; the two must never overlap", err)
		}
	}
}
