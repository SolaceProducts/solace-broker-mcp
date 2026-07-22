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
	"testing"
	"time"
)

// TestComputeChainDeadline_FormulaAcrossInputs pins the formula:
// (maxRetries+1)*perAttempt + maxRetries*retryWaitMax. Rows chosen to
// vary each input independently so a regression in the formula shape
// (e.g. wrong multiplier, integer overflow, missed +1 on attempts)
// surfaces on at least one row.
func TestComputeChainDeadline_FormulaAcrossInputs(t *testing.T) {
	cases := []struct {
		name         string
		perAttempt   time.Duration
		retryWaitMax time.Duration
		maxRetries   int
		want         time.Duration
	}{
		{
			// The shipped defaults; canonical row worth pinning explicitly.
			name:         "package defaults → 19s",
			perAttempt:   DefaultPerAttemptTimeout,
			retryWaitMax: DefaultRetryWaitMax,
			maxRetries:   DefaultMaxRetries,
			want:         19 * time.Second,
		},
		{
			// Zero retries — a single attempt, no backoff term.
			name:         "no retries → per-attempt only",
			perAttempt:   5 * time.Second,
			retryWaitMax: 2 * time.Second,
			maxRetries:   0,
			want:         5 * time.Second,
		},
		{
			// One retry: 2 attempts * per + 1 * waitMax.
			name:         "one retry",
			perAttempt:   3 * time.Second,
			retryWaitMax: 1 * time.Second,
			maxRetries:   1,
			want:         7 * time.Second, // 2*3 + 1*1
		},
		{
			// Multiplies out cleanly to demonstrate the additive shape.
			name:         "row 3 from PR notes → 14s",
			perAttempt:   4 * time.Second,
			retryWaitMax: 1 * time.Second,
			maxRetries:   2,
			want:         14 * time.Second, // 3*4 + 2*1
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeChainDeadline(0, tc.perAttempt, tc.retryWaitMax, tc.maxRetries)
			if got != tc.want {
				t.Errorf("ComputeChainDeadline(override=0, %v, %v, %d) = %v, want %v",
					tc.perAttempt, tc.retryWaitMax, tc.maxRetries, got, tc.want)
			}
		})
	}
}

// TestComputeChainDeadline_OverrideWins covers the extensibility hook:
// a positive override returns as-is, regardless of what the formula would
// compute. Guards against a future refactor that "helpfully" clamps the
// override to the formula's ceiling or floor.
func TestComputeChainDeadline_OverrideWins(t *testing.T) {
	cases := []struct {
		name     string
		override time.Duration
		want     time.Duration
	}{
		{"override much smaller than formula", 3 * time.Second, 3 * time.Second},
		{"override slightly larger than formula", 25 * time.Second, 25 * time.Second},
		{"override equal to formula", 19 * time.Second, 19 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeChainDeadline(
				tc.override,
				DefaultPerAttemptTimeout,
				DefaultRetryWaitMax,
				DefaultMaxRetries,
			)
			if got != tc.want {
				t.Errorf("override %v: got %v, want %v", tc.override, got, tc.want)
			}
		})
	}
}

// TestComputeChainDeadline_ZeroOrNegativeOverrideUsesFormula documents
// the "override <= 0 → formula" boundary. Two distinct rows because Go
// zero-values (an unset field on a Params struct, e.g.) and explicit
// negative values are semantically the same here.
func TestComputeChainDeadline_ZeroOrNegativeOverrideUsesFormula(t *testing.T) {
	want := 19 * time.Second // matches package defaults

	got := ComputeChainDeadline(0, DefaultPerAttemptTimeout, DefaultRetryWaitMax, DefaultMaxRetries)
	if got != want {
		t.Errorf("zero override: got %v, want %v", got, want)
	}
	got = ComputeChainDeadline(-1*time.Second, DefaultPerAttemptTimeout, DefaultRetryWaitMax, DefaultMaxRetries)
	if got != want {
		t.Errorf("negative override: got %v, want %v", got, want)
	}
}

// TestDefaults_PinShippedValues freezes the shipped constants so that a
// change to any of them requires updating this test explicitly. Prevents
// a silent tighten/loosen of retry policy through a one-line edit.
func TestDefaults_PinShippedValues(t *testing.T) {
	if DefaultPerAttemptTimeout != 5*time.Second {
		t.Errorf("DefaultPerAttemptTimeout = %v, want 5s", DefaultPerAttemptTimeout)
	}
	if DefaultMaxRetries != 2 {
		t.Errorf("DefaultMaxRetries = %d, want 2", DefaultMaxRetries)
	}
	if DefaultRetryWaitMin != 1*time.Second {
		t.Errorf("DefaultRetryWaitMin = %v, want 1s", DefaultRetryWaitMin)
	}
	if DefaultRetryWaitMax != 2*time.Second {
		t.Errorf("DefaultRetryWaitMax = %v, want 2s", DefaultRetryWaitMax)
	}
}
