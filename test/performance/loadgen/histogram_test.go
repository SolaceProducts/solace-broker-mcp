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

package main

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

// histTolerance is the bound on any reported percentile relative to the exact
// nearest-rank value: the ≤ 0.5% the bucket layout guarantees (sqrt(1.01)-1 ≈
// 0.499%), plus a hair for float rounding at bucket edges. A literal, not
// derived from histGrowth, and tighter than the ticket's 1%: a change that
// coarsens the buckets or shifts the midpoint must fail here, not move the
// bar with it.
const histTolerance = 0.0051

// exactQuantile is the sort-based pass loadgen used before the histogram:
// the reference every histogram percentile is checked against.
func exactQuantile(sorted []time.Duration, p float64) time.Duration {
	return sorted[pctIdx(len(sorted), p)]
}

func assertClose(t *testing.T, label string, got, want time.Duration) {
	t.Helper()
	if want == 0 {
		// A relative error against zero is NaN, which compares false and
		// would pass anything.
		if got != 0 {
			t.Errorf("%s = %v, exact 0", label, got)
		}
		return
	}
	if rel := math.Abs(float64(got-want)) / float64(want); rel > histTolerance {
		t.Errorf("%s = %v, exact %v (off by %.3f%%, limit %.3f%%)",
			label, got, want, rel*100, histTolerance*100)
	}
}

// TestLatencyHistMatchesExactPercentiles is the AC check: on a fixed
// seeded set spanning µs to seconds, every percentile the summary prints is
// within tolerance of the exact computation, and max is exact.
func TestLatencyHistMatchesExactPercentiles(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	lo, hi := math.Log(float64(10*time.Microsecond)), math.Log(float64(5*time.Second))

	h := newLatencyHist()
	samples := make([]time.Duration, 100_000)
	for i := range samples {
		// Log-uniform, so every decade of the bucket range gets exercised.
		d := time.Duration(math.Exp(lo + rng.Float64()*(hi-lo)))
		samples[i] = d
		h.record(d)
	}
	slices.Sort(samples)

	for _, p := range []float64{0.50, 0.95, 0.99, 0.999} {
		assertClose(t, fmt.Sprintf("p%g", p*100), h.quantile(p), exactQuantile(samples, p))
	}
	if h.max != samples[len(samples)-1] {
		t.Errorf("max = %v, exact %v", h.max, samples[len(samples)-1])
	}
	if h.n != len(samples) {
		t.Errorf("n = %d, want %d", h.n, len(samples))
	}
}

// TestLatencyHistEdgeCases covers the shapes a percentile pass is most
// likely to get wrong: one sample, a constant latency, values outside the
// bucket range, and an empty histogram.
func TestLatencyHistEdgeCases(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		h := newLatencyHist()
		if got := h.quantile(0.99); got != 0 {
			t.Errorf("quantile on empty = %v, want 0", got)
		}
	})

	t.Run("one sample", func(t *testing.T) {
		h := newLatencyHist()
		h.record(12 * time.Millisecond)
		for _, p := range []float64{0.50, 0.95, 0.99} {
			assertClose(t, "quantile", h.quantile(p), 12*time.Millisecond)
		}
	})

	t.Run("all equal", func(t *testing.T) {
		h := newLatencyHist()
		for range 1000 {
			h.record(3 * time.Millisecond)
		}
		for _, p := range []float64{0.50, 0.95, 0.99} {
			got := h.quantile(p)
			assertClose(t, "quantile", got, 3*time.Millisecond)
			if got > h.max {
				t.Errorf("quantile %v exceeds max %v", got, h.max)
			}
		}
	})

	t.Run("overflow reports exact max", func(t *testing.T) {
		h := newLatencyHist()
		for range 99 {
			h.record(time.Millisecond)
		}
		h.record(90 * time.Second) // past histMax
		if got := h.quantile(1.0); got != 90*time.Second {
			t.Errorf("p100 = %v, want the exact overflow value 90s", got)
		}
		assertClose(t, "p50", h.quantile(0.50), time.Millisecond)
	})

	t.Run("underflow", func(t *testing.T) {
		h := newLatencyHist()
		h.record(200 * time.Nanosecond) // below histMin
		if got := h.quantile(0.50); got != 200*time.Nanosecond {
			t.Errorf("p50 = %v, want 200ns (capped at max)", got)
		}
	})
}

// TestLatencyHistMergeEqualsSingle pins that merging per-client histograms
// gives the same answer as recording everything into one, which is what the
// old concatenate-then-sort pass effectively did.
func TestLatencyHistMergeEqualsSingle(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	single := newLatencyHist()
	parts := []*latencyHist{newLatencyHist(), newLatencyHist(), newLatencyHist()}
	for i := range 30_000 {
		d := time.Duration(rng.Int64N(int64(2*time.Second))) + time.Microsecond
		single.record(d)
		parts[i%len(parts)].record(d)
	}
	merged := newLatencyHist()
	for _, p := range parts {
		merged.merge(p)
	}
	if merged.n != single.n || merged.max != single.max || !slices.Equal(merged.counts, single.counts) {
		t.Fatalf("merged histogram differs from single: n %d/%d max %v/%v",
			merged.n, single.n, merged.max, single.max)
	}
}

// TestSummarizeKeepsErrorsOutOfLatency pins the summary's accounting: errors
// count toward total and the breakdown but never toward latency, so a fast
// failure cannot drag p50 down.
func TestSummarizeKeepsErrorsOutOfLatency(t *testing.T) {
	var parts [2]clientTally
	for i := range parts {
		parts[i] = newClientTally()
		for range 50 {
			parts[i].record(10*time.Millisecond, nil)
		}
		parts[i].record(time.Microsecond, errors.New("context deadline exceeded"))
	}
	all := newClientTally()
	for _, p := range parts {
		all.merge(p)
	}

	s := summarize(all, 10*time.Second)
	if s.total != 102 || s.errs != 2 || s.errBreak["timeout"] != 2 {
		t.Errorf("total/errs/timeout = %d/%d/%d, want 102/2/2", s.total, s.errs, s.errBreak["timeout"])
	}
	if s.rps != 10.2 {
		t.Errorf("rps = %v, want 10.2", s.rps)
	}
	assertClose(t, "p50", s.p50, 10*time.Millisecond)
	if s.pMax != 10*time.Millisecond {
		t.Errorf("max = %v, want 10ms: the 1µs failure must not count", s.pMax)
	}
}
