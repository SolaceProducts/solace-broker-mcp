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
	"math"
	"time"
)

// Bucket layout for latencyHist. Each bucket spans a factor of histGrowth,
// so any latency lies within sqrt(histGrowth) of its bucket's geometric
// midpoint: at 1.01 that is ≤ 0.5% error on any percentile that falls
// between histMin and histMax. That range covers every latency an MCP call
// over HTTP can take (the per-call context times out at 30s). Anything
// outside still counts, in the underflow or overflow bucket, but the bound
// does not hold there: underflow reports histMin and overflow the exact max.
const (
	histMin    = time.Microsecond
	histMax    = 60 * time.Second
	histGrowth = 1.01
)

var (
	histLogGrowth = math.Log(histGrowth)
	// histBuckets is the number of log-scale buckets between histMin and
	// histMax (~1,800). counts has two more: [0] for underflow, and the
	// last for overflow.
	histBuckets = int(math.Ceil(math.Log(float64(histMax)/float64(histMin)) / histLogGrowth))
)

// latencyHist is a fixed-size log-scale latency histogram. It replaces the
// one-sample-per-request slice loadgen used to keep for its end-of-run
// percentile pass: that grew ~458 MB/h at 2,500 calls/s (SOL-154158), which a
// multi-day soak cannot survive. This costs ~14 KB per client for the whole
// run, however long.
//
// Not safe for concurrent use; each client owns one and they are merged
// after the run.
type latencyHist struct {
	counts []int
	n      int
	max    time.Duration // exact, not bucketed: printed as "max (worst)"
}

func newLatencyHist() *latencyHist {
	return &latencyHist{counts: make([]int, histBuckets+2)}
}

func (h *latencyHist) record(d time.Duration) {
	h.counts[bucketOf(d)]++
	h.n++
	h.max = max(h.max, d)
}

func (h *latencyHist) merge(o *latencyHist) {
	for i, c := range o.counts {
		h.counts[i] += c
	}
	h.n += o.n
	h.max = max(h.max, o.max)
}

// quantile returns the latency at percentile p (0.0..1.0), using the same
// nearest-rank rule as pctIdx so it matches what the old sort-based pass
// reported, to within the bucket width.
func (h *latencyHist) quantile(p float64) time.Duration {
	if h.n == 0 {
		return 0
	}
	rank := pctIdx(h.n, p)
	cum := 0
	for i, c := range h.counts {
		cum += c
		if cum > rank {
			return h.bucketValue(i)
		}
	}
	return h.max // unreachable: cum reaches n > rank
}

// bucketOf maps a latency to its index in counts.
func bucketOf(d time.Duration) int {
	if d < histMin {
		return 0
	}
	i := 1 + int(math.Log(float64(d)/float64(histMin))/histLogGrowth)
	return min(i, histBuckets+1)
}

// bucketValue is the latency reported for bucket i: its geometric midpoint,
// capped at the exact max so no percentile can exceed "max (worst)". The
// underflow and overflow buckets have no midpoint and report their bound
// and the max respectively.
func (h *latencyHist) bucketValue(i int) time.Duration {
	var v time.Duration
	switch {
	case i == 0:
		v = histMin
	case i > histBuckets:
		v = h.max
	default:
		v = time.Duration(float64(histMin) * math.Exp((float64(i)-0.5)*histLogGrowth))
	}
	return min(v, h.max)
}
